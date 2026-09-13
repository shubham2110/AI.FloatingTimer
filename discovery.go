//go:build windows

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

type NodeIdentity struct {
	Service   string          `json:"service"`
	Name      string          `json:"name"`
	TimerPort int             `json:"timer_port"`
	UIPort    int             `json:"ui_port"`
	Timers    []TimerIdentity `json:"timers,omitempty"`
}

type TimerIdentity struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

func localNodeIdentity() NodeIdentity {
	identity := NodeIdentity{Service: "overlay-timer", Name: configuredFriendlyName(), TimerPort: appConfig.TimerPort, UIPort: appConfig.UIDiscoveryPort}
	identity.Timers = []TimerIdentity{{ID: "", Name: identity.Name, Path: ""}}
	for _, timer := range virtualTimerList() {
		identity.Timers = append(identity.Timers, TimerIdentity{ID: timer.Config.ID, Name: timer.Config.Name, Path: "/" + timer.Config.ID})
	}
	return identity
}

// The complete timer URL is the registry key, including a virtual timer's ID.
// Legacy identities without a manifest still describe the host's own timer.
func identityPCs(address string, identity NodeIdentity) []PC {
	pcs := []PC{{ID: stablePCID(address), Name: identity.Name, Address: address, UIPort: identity.UIPort}}
	seen := map[string]bool{"": true}
	for _, timer := range identity.Timers {
		if timer.ID == "" && timer.Path == "" && timer.Name != "" {
			pcs[0].Name = timer.Name
		}
		if seen[timer.ID] || !isNumericID(timer.ID) || timer.Path != "/"+timer.ID {
			continue
		}
		seen[timer.ID] = true
		name := timer.Name
		if strings.TrimSpace(name) == "" {
			name = "Agent " + timer.ID
		}
		timerAddress := address + timer.Path
		pcs = append(pcs, PC{ID: stablePCID(timerAddress), Name: name, Address: timerAddress, UIPort: identity.UIPort})
	}
	return pcs
}

type DiscoveryResult struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	LeaderIP   string    `json:"leader_ip"`
	Scanned    int       `json:"scanned"`
	Found      int       `json:"found"`
	HostsFound int       `json:"hosts_found"`
	Error      string    `json:"error,omitempty"`
}

var discoveryState = struct {
	sync.Mutex
	running bool
	last    DiscoveryResult
}{}

func stablePCID(address string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimRight(address, "/"))))
	return hex.EncodeToString(sum[:8])
}

func localIPv4s() []net.IP {
	addresses, _ := net.InterfaceAddrs()
	var result []net.IP
	for _, address := range addresses {
		ip, _, err := net.ParseCIDR(address.String())
		if err == nil && ip.To4() != nil && !ip.IsLoopback() {
			result = append(result, ip.To4())
		}
	}
	sort.Slice(result, func(i, j int) bool { return bytesToUint32(result[i]) < bytesToUint32(result[j]) })
	return result
}

func bytesToUint32(ip net.IP) uint32 {
	v := ip.To4()
	if v == nil {
		return ^uint32(0)
	}
	return uint32(v[0])<<24 | uint32(v[1])<<16 | uint32(v[2])<<8 | uint32(v[3])
}

func localRegistryPC() PC {
	host := "127.0.0.1"
	if ips := localIPv4s(); len(ips) > 0 {
		host = ips[0].String()
	}
	address := fmt.Sprintf("http://%s:%d", host, appConfig.TimerPort)
	return PC{ID: stablePCID(address), Name: configuredFriendlyName(), Address: address, UIPort: appConfig.UIDiscoveryPort}
}

func observeLocalTimers() {
	for _, pc := range identityPCs(localRegistryPC().Address, localNodeIdentity()) {
		observePC(pc)
	}
}

// A direct probe supplies liveness, never a new metadata revision.
func observePC(pc PC) {
	pc.LastSeen = time.Now().UTC()
	if _, err := store.merge([]PC{pc}); err != nil {
		logf("registry", "could not save observation: %v", err)
	}
}

func registerDiscoveryHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/node", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, 405, map[string]string{"error": "GET required"})
			return
		}
		writeJSON(w, 200, localNodeIdentity())
	})
	mux.HandleFunc("/api/discovery/status", func(w http.ResponseWriter, r *http.Request) {
		discoveryState.Lock()
		result, running := discoveryState.last, discoveryState.running
		discoveryState.Unlock()
		writeJSON(w, 200, map[string]interface{}{"running": running, "last": result, "leader_ip": result.LeaderIP})
	})
	mux.HandleFunc("/api/discovery/run", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, 405, map[string]string{"error": "POST required"})
			return
		}
		if !beginDiscovery() {
			writeJSON(w, 409, map[string]string{"error": "discovery already running"})
			return
		}
		go performDiscovery(true)
		writeJSON(w, 202, map[string]string{"status": "started"})
	})
	mux.HandleFunc("/api/discovery/peers", discoveryPeersHandler)
	mux.HandleFunc("/api/discovery/distribute", distributeHandler)
}

func discoveryPeersHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, store.active())
	case http.MethodPost:
		var peers []PC
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&peers); err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid peer list"})
			return
		}
		changed, err := store.merge(peers)
		if err != nil {
			logf("registry", "peer merge failed: %v", err)
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]interface{}{"status": "success", "changed": changed})
	default:
		writeJSON(w, 405, map[string]string{"error": "GET or POST required"})
	}
}

func startDiscoveryScheduler() {
	if !appConfig.Discovery.Enabled {
		return
	}
	go func() {
		time.Sleep(3 * time.Second)
		periodicDiscovery()
		ticker := time.NewTicker(time.Duration(appConfig.Discovery.IntervalSeconds) * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			periodicDiscovery()
		}
	}()
}

func periodicDiscovery() {
	observeLocalTimers()
	leader := discoveryLeaderIP()
	own := localIPv4s()
	if len(own) > 0 && leader == own[0].String() {
		if beginDiscovery() {
			go performDiscovery(false)
		}
		return
	}
	if leader != "" {
		pullPeers(leader)
	}
}

func beginDiscovery() bool {
	discoveryState.Lock()
	defer discoveryState.Unlock()
	if discoveryState.running {
		return false
	}
	discoveryState.running = true
	return true
}

func discoveryLeaderIP() string {
	candidates := localIPv4s()
	blocked := blockedDiscoveryHosts()
	for _, peer := range store.active() {
		u, err := url.Parse(peer.Address)
		if err != nil {
			continue
		}
		ip := net.ParseIP(u.Hostname())
		if ip != nil && ip.To4() != nil {
			candidates = append(candidates, ip.To4())
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	sort.Slice(candidates, func(i, j int) bool { return bytesToUint32(candidates[i]) < bytesToUint32(candidates[j]) })
	// Prefer the lowest candidate whose UI identity responds; own addresses are always eligible.
	own := map[string]bool{}
	for _, ip := range localIPv4s() {
		own[ip.String()] = true
	}
	for _, ip := range candidates {
		if blocked[ip.String()] {
			continue
		}
		if own[ip.String()] || uiNodeReachable(ip.String(), uiPortForHost(ip.String())) {
			return ip.String()
		}
	}
	return ""
}

func uiPortForHost(host string) int {
	for _, peer := range store.active() {
		u, err := url.Parse(peer.Address)
		if err == nil && u.Hostname() == host && peer.UIPort > 0 {
			return peer.UIPort
		}
	}
	return appConfig.UIDiscoveryPort
}

func uiNodeReachable(host string, port int) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(appConfig.Discovery.ConnectTimeoutMilliseconds)*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s:%d/api/node", host, port), nil)
	response, err := timerHTTP.Do(req)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return response.StatusCode == http.StatusOK
}

func configuredNetworks() []*net.IPNet {
	var networks []*net.IPNet
	for _, raw := range appConfig.Discovery.CIDRRanges {
		ip, network, err := net.ParseCIDR(strings.TrimSpace(raw))
		if err == nil && ip.To4() != nil {
			networks = append(networks, network)
		}
	}
	if len(networks) == 0 {
		for _, ip := range localIPv4s() {
			_, network, _ := net.ParseCIDR(ip.String() + "/24")
			networks = append(networks, network)
		}
	}
	return networks
}

func hostsForNetworks() []string {
	blocked := blockedDiscoveryHosts()
	seen := map[string]bool{}
	var hosts []string
	for _, network := range configuredNetworks() {
		ones, bits := network.Mask.Size()
		if bits != 32 || ones < 20 {
			continue
		}
		base := bytesToUint32(network.IP)
		count := uint32(1) << uint32(32-ones)
		if count > 4096 {
			count = 4096
		}
		for i := uint32(1); i+1 < count; i++ {
			value := base + i
			host := fmt.Sprintf("%d.%d.%d.%d", byte(value>>24), byte(value>>16), byte(value>>8), byte(value))
			if !seen[host] && !blocked[host] {
				seen[host] = true
				hosts = append(hosts, host)
			}
		}
	}
	return hosts
}

func performDiscovery(manual bool) {
	result := DiscoveryResult{StartedAt: time.Now()}
	if !manual {
		result.LeaderIP = discoveryLeaderIP()
	}
	defer func() {
		result.FinishedAt = time.Now()
		discoveryState.Lock()
		discoveryState.running = false
		discoveryState.last = result
		discoveryState.Unlock()
	}()
	hosts := hostsForNetworks()
	result.Scanned = len(hosts)
	workers := appConfig.Discovery.MaximumConcurrency
	jobs := make(chan string)
	found := make(chan []PC, len(hosts))
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for host := range jobs {
				if pcs, ok := probeTimer(host); ok {
					found <- pcs
				}
			}
		}()
	}
	go func() {
		for _, host := range hosts {
			jobs <- host
		}
		close(jobs)
		wg.Wait()
		close(found)
	}()
	for pcs := range found {
		result.HostsFound++
		result.Found += len(pcs)
		for _, pc := range pcs {
			observePC(pc)
		}
		pullPeersFromPC(pcs[0])
	}
	logf("discovery", "complete manual=%v scanned=%d hosts=%d timers=%d", manual, result.Scanned, result.HostsFound, result.Found)
}

func probeTimer(host string) ([]PC, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(appConfig.Discovery.ConnectTimeoutMilliseconds)*time.Millisecond)
	defer cancel()
	address := fmt.Sprintf("http://%s:%d", host, appConfig.TimerPort)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, address+"/identity", nil)
	response, err := timerHTTP.Do(req)
	if err != nil {
		return nil, false
	}
	defer response.Body.Close()
	var identity NodeIdentity
	if response.StatusCode != 200 || json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&identity) != nil || identity.Service != "overlay-timer" || identity.TimerPort < 0 || identity.TimerPort > 65535 || identity.UIPort < 0 || identity.UIPort > 65535 {
		return nil, false
	}
	if identity.TimerPort > 0 {
		address = fmt.Sprintf("http://%s:%d", host, identity.TimerPort)
	}
	return identityPCs(address, identity), true
}

func pullPeers(host string) {
	pullPeersURL(fmt.Sprintf("http://%s:%d/api/discovery/peers", host, uiPortForHost(host)))
}
func peerEndpoint(pc PC) string {
	u, err := url.Parse(pc.Address)
	if err != nil {
		return ""
	}
	port := pc.UIPort
	if port < 1 {
		port = appConfig.UIDiscoveryPort
	}
	return "http://" + net.JoinHostPort(u.Hostname(), fmt.Sprint(port)) + "/api/discovery/peers"
}

func pullPeersFromPC(pc PC) { pullPeersURL(peerEndpoint(pc)) }

func pullPeersURL(endpoint string) {
	response, err := timerHTTP.Get(endpoint)
	if err != nil {
		return
	}
	defer response.Body.Close()
	var peers []PC
	if response.StatusCode == 200 && json.NewDecoder(response.Body).Decode(&peers) == nil {
		if _, err := store.merge(peers); err != nil {
			logf("registry", "peer merge failed: %v", err)
		}
	}
}

// A disabled virtual timer never blocks its host. A disabled root timer only
// blocks scanning when no active sibling needs that host. Resolve aliases once.
func blockedDiscoveryHosts() map[string]bool {
	blocked := map[string]bool{}
	active := map[string]bool{}
	resolved := map[string][]string{}
	for _, pc := range store.snapshot() {
		u, err := url.Parse(pc.Address)
		if err != nil {
			continue
		}
		host := u.Hostname()
		keys, ok := resolved[host]
		if !ok {
			keys = []string{host}
			if net.ParseIP(host) == nil {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				addresses, _ := net.DefaultResolver.LookupIPAddr(ctx, host)
				cancel()
				for _, address := range addresses {
					keys = append(keys, address.IP.String())
				}
			}
			resolved[host] = keys
		}
		for _, key := range keys {
			if !pc.Inactive && !pc.Deleted {
				active[key] = true
			} else if u.Path == "" || u.Path == "/" {
				blocked[key] = true
			}
		}
	}
	for host := range active {
		delete(blocked, host)
	}
	return blocked
}
