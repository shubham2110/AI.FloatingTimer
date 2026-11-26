//go:build windows

package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"

	"strings"
	"sync"
	"time"
)

const proxyCookie = "floating_timer_proxy"

type proxyCredential struct {
	Salt string
	Hash string
}
type proxyService struct {
	instance string
	mu       sync.Mutex
	file     string
	sessions map[string]time.Time
	failures int
	window   time.Time
	client   *http.Client
}

func newProxyService(file string) *proxyService {

	return &proxyService{instance: billingNodeInstance, file: file, sessions: make(map[string]time.Time), client: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil, IdleConnTimeout: 30 * time.Second}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func proxyLAN(r *http.Request) bool {
	// Forwarding headers never grant LAN privileges.
	for k := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-forwarded-") || strings.EqualFold(k, "Forwarded") || strings.EqualFold(k, "X-Real-IP") {
			return false
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		if subnet, ok := addr.(*net.IPNet); ok && subnet.Contains(ip) {
			return true
		}
	}
	return false
}

// The instance marker distinguishes this node from a different timer using
// the same private address on a different LAN. It never authorizes a request.
func (p *proxyService) networkProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "GET required", 405)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/api/proxy/local-probe" {
		if !proxyLAN(r) || !localProxyHost(r.Host) {
			http.Error(w, "direct LAN connection required", 403)
			return
		}
		writeJSON(w, 200, map[string]string{"instance": p.instance})
		return
	}
	endpoints := []string{}
	for _, ip := range localIPv4s() {
		if ip.IsPrivate() {
			endpoints = append(endpoints, "http://"+net.JoinHostPort(ip.String(), strconv.Itoa(appConfig.UIDiscoveryPort)))
		}
	}
	writeJSON(w, 200, map[string]any{"instance": p.instance, "endpoints": endpoints, "timerPort": appConfig.TimerPort})
}

// Opt in only for a reverse proxy that appends the observed client IP to XFF.
// This exception is for dashboard access only, never proxy relay or credentials.
func forwardedLAN(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	peer := net.ParseIP(host)
	if peer == nil {
		return false
	}
	trusted := false
	for _, entry := range strings.Split(os.Getenv("FLOATING_TIMER_TRUSTED_PROXY_IPS"), ",") {
		if ip := net.ParseIP(strings.TrimSpace(entry)); ip != nil && ip.Equal(peer) {
			trusted = true
			break
		}
	}
	if !trusted {
		return false
	}
	chain := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	client := net.ParseIP(strings.TrimSpace(chain[len(chain)-1]))
	if client == nil || !client.IsPrivate() {
		return false
	}
	addrs, _ := net.InterfaceAddrs()
	for _, addr := range addrs {
		if network, ok := addr.(*net.IPNet); ok && network.Contains(client) {
			return true
		}
	}
	return false
}
func localProxyHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip != nil && ip.IsLoopback() {
		return true
	}
	name, _ := os.Hostname()
	if name != "" && strings.EqualFold(host, name) {
		return true
	}
	addrs, _ := net.InterfaceAddrs()
	for _, addr := range addrs {
		if subnet, ok := addr.(*net.IPNet); ok && subnet.IP.Equal(ip) {
			return true
		}
	}
	return false
}
func proxyOrigin(r *http.Request) bool {
	origin, err := url.Parse(r.Header.Get("Origin"))
	return err == nil && (origin.Scheme == "http" || origin.Scheme == "https") && strings.EqualFold(origin.Host, r.Host)
}
func (p *proxyService) valid(r *http.Request) bool {
	c, err := r.Cookie(proxyCookie)
	if err != nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return time.Now().Before(p.sessions[c.Value])
}
func proxyHash(password string, salt []byte) []byte {
	key, _ := pbkdf2.Key(sha256.New, password, salt, 600000, 32)
	return key
}
func (p *proxyService) matches(password string) bool {
	data, err := os.ReadFile(p.file)
	if errors.Is(err, os.ErrNotExist) {
		return hmac.Equal([]byte(password), []byte("Paradise"+configuredFriendlyName()))
	}
	var c proxyCredential
	if err != nil || json.Unmarshal(data, &c) != nil {
		return false
	}
	salt, err := hex.DecodeString(c.Salt)
	if err != nil || len(salt) != 16 {
		return false
	}
	return hmac.Equal([]byte(c.Hash), []byte(hex.EncodeToString(proxyHash(password, salt))))
}
func (p *proxyService) auth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != "POST" || !proxyOrigin(r) || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "same-origin JSON POST required", 403)
		return
	}
	if !proxyLAN(r) && !strings.HasPrefix(r.Header.Get("Origin"), "https://") {
		http.Error(w, "use HTTPS to sign in over the internet", 403)
		return
	}
	change := r.URL.Path == "/api/proxy/password"
	if change && (!proxyLAN(r) || !localProxyHost(r.Host)) {
		http.Error(w, "change the password from this node's LAN address or localhost", 403)
		return
	}
	if r.URL.Path == "/api/proxy/logout" {
		if cookie, err := r.Cookie(proxyCookie); err == nil {
			p.mu.Lock()
			delete(p.sessions, cookie.Value)
			p.mu.Unlock()
		}
		http.SetCookie(w, &http.Cookie{Name: proxyCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		writeJSON(w, 200, true)
		return
	}
	var q struct {
		Password    string
		NewPassword string
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&q) != nil || q.Password == "" {
		http.Error(w, "enter the node password", 400)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if now.Sub(p.window) >= time.Minute {
		p.window = now
		p.failures = 0
	}
	if p.failures >= 10 {
		http.Error(w, "too many attempts; retry in a minute", 429)
		return
	}
	if !p.matches(q.Password) {
		p.failures++
		http.Error(w, "incorrect node password", 401)
		return
	}
	if change {
		if strings.TrimSpace(q.NewPassword) == "" || len(q.NewPassword) > 256 {
			http.Error(w, "enter a new password (maximum 256 bytes)", 400)
			return
		}
		salt := make([]byte, 16)
		rand.Read(salt)
		data, _ := json.Marshal(proxyCredential{hex.EncodeToString(salt), hex.EncodeToString(proxyHash(q.NewPassword, salt))})
		if os.WriteFile(p.file+".tmp", data, 0600) != nil || os.Rename(p.file+".tmp", p.file) != nil {
			http.Error(w, "could not save password", 500)
			return
		}
		clear(p.sessions)
	}
	p.failures = 0
	expiry := now.Add(8 * time.Hour)
	for token, end := range p.sessions {
		if !now.Before(end) {
			delete(p.sessions, token)
		}
	}
	if old, err := r.Cookie(proxyCookie); err == nil {
		delete(p.sessions, old.Value)
	}
	token := rand.Text()
	p.sessions[token] = expiry
	http.SetCookie(w, &http.Cookie{Name: proxyCookie, Value: token, HttpOnly: true, Secure: strings.HasPrefix(r.Header.Get("Origin"), "https://"), SameSite: http.SameSiteStrictMode, Path: "/", Expires: expiry})
	writeJSON(w, 200, true)
}
func proxyDestination(pc PC, service, action, method string) (string, error) {
	invalid := errors.New("unsupported proxy destination or action")
	u, err := url.Parse(pc.Address)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return "", invalid
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil || ip.Zone() != "" || (!ip.IsPrivate() && !ip.IsLoopback()) {
		return "", invalid
	}
	prefix := strings.TrimSuffix(u.Path, "/")
	if prefix != "" && !isNumericID(strings.TrimPrefix(prefix, "/")) {
		return "", invalid
	}
	switch service {
	case "manager":
		if pc.UIPort != 18082 || method != "GET" || !slices.Contains([]string{"health", "node"}, action) {
			return "", invalid
		}
		u.Host = net.JoinHostPort(ip.String(), "18082")
		u.Path = "/api/" + action
	case "host", "timer":
		if u.Port() != "18081" {
			return "", invalid
		}
		if service == "host" {
			if action != "status" || method != "GET" {
				return "", invalid
			}
			prefix = ""
		}
		if !(method == "GET" && action == "status") && !(method == "POST" && (slices.Contains(timerActions, action) || slices.Contains([]string{"position", "drag", "special"}, action))) {
			return "", invalid
		}
		u.Path = prefix + "/" + action
	default:
		return "", invalid
	}
	return u.String(), nil
}
func (p *proxyService) relay(w http.ResponseWriter, r *http.Request) {
	if !p.valid(r) {
		w.Header().Set("X-Timer-Login", "required")
		http.Error(w, "proxy login required", 401)
		return
	}
	if (r.Method != "GET" && !proxyOrigin(r)) || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		http.Error(w, "same-origin request required", 403)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/proxy/relay/"), "/")
	if len(parts) != 3 || r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	pc, ok := store.find(parts[0])
	if !ok || pc.Missing || pc.Inactive {
		http.Error(w, "PC unavailable", 404)
		return
	}
	target, err := proxyDestination(pc, parts[1], parts[2], r.Method)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8192))
	if err != nil {
		http.Error(w, "request too large", 413)
		return
	}
	upstream, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(data))
	if err != nil {
		http.Error(w, "invalid timer request", 400)
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(upstream)
	if err != nil {
		http.Error(w, "timer unreachable", 502)
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1048577))
	if err != nil || len(body) > 1048576 || !json.Valid(body) {
		http.Error(w, "invalid timer response", 502)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}
func (p *proxyService) protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if !proxyLAN(r) && path != "/" && !strings.HasPrefix(path, "/api/proxy/") {
			trustedLAN := forwardedLAN(r)
			if strings.HasPrefix(path, "/internal/") || (!trustedLAN && !p.valid(r)) {
				w.Header().Set("X-Timer-Login", "required")
				http.Error(w, "proxy login required", 401)
				return
			}
			if !trustedLAN {
				w.Header().Set("X-Timer-Proxy", "required")
			}
			if r.Method != "GET" && !proxyOrigin(r) {
				http.Error(w, "same-origin request required", 403)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
