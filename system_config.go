//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
)

// Publish only pricing fields live. Other configuration retains its restart semantics.
type timerPricing struct {
	Group      string   `json:"pricing_group"`
	Categories []string `json:"pricing_categories"`
}
type savedPricing struct {
	timerPricing
	Timers []struct {
		ID string `json:"id"`
		timerPricing
	} `json:"on_behalf_of"`
}

var livePricing atomic.Pointer[savedPricing]

func pricingFromConfig(fields map[string]json.RawMessage) *savedPricing {
	cfg := &savedPricing{}
	data, _ := json.Marshal(fields)
	if json.Unmarshal(data, cfg) != nil {
		return nil
	}
	return cfg
}
func (cfg *savedPricing) forPath(path string) timerPricing {
	for _, t := range cfg.Timers {
		if path == "/"+t.ID && (t.Group != "" || len(t.Categories) > 0) {
			return t.timerPricing
		}
	}
	return cfg.timerPricing
}

type systemConfigView struct {
	Config     map[string]json.RawMessage `json:"config"`
	FullAccess bool                       `json:"full_access"`
}

func hostAddressForPricing(address string) string {
	u, err := url.Parse(address)
	if err != nil {
		return address
	}
	return u.Host
}
func loopbackHost(value string) bool {
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	}
	ip := net.ParseIP(strings.Trim(value, "[]"))
	return strings.EqualFold(value, "localhost") || (ip != nil && ip.IsLoopback())
}

func localConfigBrowser(r *http.Request) bool {
	origin, err := url.Parse(r.Header.Get("Origin"))
	sameOrigin := err == nil && origin.Host == r.Host && (origin.Scheme == "http" || origin.Scheme == "https")
	return loopbackHost(r.Host) && loopbackHost(r.RemoteAddr) &&
		(r.Method == http.MethodGet || sameOrigin)
}

func configurationEndpoint(w http.ResponseWriter, r *http.Request) {
	serveSystemConfig(w, r, false)
}

// Registry labels can lag a rename. A local interface can still open its
// owning configuration when both service ports match this installation.
func localConfigAddress(pc PC) bool {
	endpoint, err := url.Parse(pc.Address)
	if err != nil || endpoint.Port() != strconv.Itoa(appConfig.TimerPort) ||
		(pc.UIPort != 0 && pc.UIPort != appConfig.UIDiscoveryPort) {
		return false
	}
	host := net.ParseIP(endpoint.Hostname())
	if host == nil {
		return false
	}
	for _, ip := range localIPv4s() {
		if host.Equal(ip) {
			return true
		}
	}
	return host.IsLoopback()
}

func systemConfigForPC(w http.ResponseWriter, r *http.Request, pc PC) {
	if localBillingPC(pc) || localConfigAddress(pc) {
		serveSystemConfig(w, r, localConfigBrowser(r))
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		http.Error(w, "GET or PUT required", 405)
		return
	}
	var changes map[string]json.RawMessage
	if r.Method == http.MethodPut {
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&changes) != nil || changes == nil {
			writeJSON(w, 400, map[string]string{"error": "invalid timer configuration"})
			return
		}
	}
	var result systemConfigView
	if err := callBillingPeerContext(r.Context(), pc, r.Method, "/internal/api/node/config", changes, &result); err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	if r.Method == http.MethodPut {
		if cfg := pricingFromConfig(result.Config); cfg != nil {
			store.Lock()
			previous := append([]PC(nil), store.PCs...)
			for i, peer := range store.PCs {
				if hostAddressForPricing(peer.Address) != hostAddressForPricing(pc.Address) {
					continue
				}
				pricing := cfg.forPath(billingPath(peer.Address))
				store.PCs[i].PricingCategories = pricing.Categories
				store.PCs[i].PricingGroup = pricing.Group
			}
			err := store.commitLocked(previous)
			store.Unlock()
			if err != nil {
				writeBillingError(w, err)
				return
			}
		}
	}
	writeJSON(w, 200, result)
}

func serveSystemConfig(w http.ResponseWriter, r *http.Request, full bool) {
	serveSystemConfigFile(w, r, full, appDataPath(ConfigFileName))
}
func serveSystemConfigFile(w http.ResponseWriter, r *http.Request, full bool, filename string) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		http.Error(w, "GET or PUT required", 405)
		return
	}
	configuredNames.Lock()
	defer configuredNames.Unlock()
	raw, err := os.ReadFile(filename)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	var saved map[string]json.RawMessage
	if err = json.Unmarshal(raw, &saved); err != nil || saved == nil {
		writeJSON(w, 500, map[string]string{"error": "timer config is not a JSON object"})
		return
	}
	defaults, _ := json.Marshal(defaultAppConfig())
	fields := map[string]json.RawMessage{}
	_ = json.Unmarshal(defaults, &fields)
	if r.Method == http.MethodPut {
		var changes map[string]json.RawMessage
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&changes) != nil || changes == nil {
			writeJSON(w, 400, map[string]string{"error": "invalid timer configuration"})
			return
		}
		if err = mergeSystemConfig(saved, changes, fields, full); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		if err = writeTimerConfig(filename, saved); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		livePricing.Store(pricingFromConfig(saved))
	}
	for key, value := range saved {
		if _, ok := fields[key]; !ok {
			continue
		}
		if key == "billing" || key == "discovery" {
			var defaults, current map[string]json.RawMessage
			_ = json.Unmarshal(fields[key], &defaults)
			_ = json.Unmarshal(value, &current)
			for field, v := range current {
				defaults[field] = v
			}
			value, _ = json.Marshal(defaults)
		}
		fields[key] = value
	}
	if !full {
		// LAN callers may read ordinary settings. Command arguments and custom
		// filesystem locations are only available in the owning PC's local editor.
		delete(fields, "apps_to_close_on_time_up")
		delete(fields, "special_commands")
		var discovery DiscoveryConfig
		if json.Unmarshal(fields["discovery"], &discovery) == nil {
			fields["discovery"], _ = json.Marshal(discovery)
		}
		var b struct {
			Enabled bool `json:"enabled"`
		}
		if json.Unmarshal(fields["billing"], &b) == nil {
			fields["billing"], _ = json.Marshal(b)
		}
		var timers []struct {
			ID                    string   `json:"id"`
			Name                  string   `json:"name"`
			PricingCategories     []string `json:"pricing_categories"`
			PricingGroup          string   `json:"pricing_group,omitempty"`
			CommandTimeoutSeconds int      `json:"command_timeout_seconds"`
		}
		if json.Unmarshal(fields["on_behalf_of"], &timers) == nil {
			fields["on_behalf_of"], _ = json.Marshal(timers)
		}
	}
	writeJSON(w, 200, systemConfigView{Config: fields, FullAccess: full})
}

func mergeSystemConfig(saved, changes, fields map[string]json.RawMessage, full bool) error {
	for key, value := range changes {
		if _, ok := fields[key]; !ok {
			return fmt.Errorf("unknown timer field %q", key)
		}
		if !full && key != "pricing_categories" && key != "pricing_group" && key != "friendly_name" && key != "on_behalf_of" {
			return fmt.Errorf("%s can be edited from the owning PC's local dashboard", key)
		}
		if key == "on_behalf_of" && !full {
			var updates, entries []map[string]json.RawMessage
			if json.Unmarshal(value, &updates) != nil || json.Unmarshal(saved[key], &entries) != nil {
				return errors.New("invalid on-behalf configuration")
			}
			for _, update := range updates {
				found := false
				for _, entry := range entries {
					if string(entry["id"]) != string(update["id"]) {
						continue
					}
					found = true
					for field, v := range update {
						switch field {
						case "id":
						case "name", "pricing_group", "pricing_categories":
							entry[field] = v
						default:
							return errors.New("only the on-behalf name and pricing group can be edited from another PC")
						}
					}
					break
				}
				if !found {
					return errors.New("add on-behalf timers from the owning PC's local dashboard")
				}
			}
			value, _ = json.Marshal(entries)
		}
		if key == "billing" || key == "discovery" {
			var values, original map[string]json.RawMessage
			if json.Unmarshal(value, &values) != nil || values == nil {
				return fmt.Errorf("%s must be an object", key)
			}
			_ = json.Unmarshal(saved[key], &original)
			if original == nil {
				original = make(map[string]json.RawMessage)
			}
			for name, v := range values {
				original[name] = v
			}
			value, _ = json.Marshal(original)
		}
		saved[key] = value
	}
	data, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	cfg := defaultAppConfig()
	if err = json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	if cfg.TimerPort < 1 || cfg.TimerPort > 65535 || cfg.UIDiscoveryPort < 1 || cfg.UIDiscoveryPort > 65535 || cfg.TimerPort == cfg.UIDiscoveryPort {
		return errors.New("timer and dashboard ports must be distinct and between 1 and 65535")
	}
	if cfg.CommandTimeoutSeconds < 1 || cfg.ForceKillAfterSeconds < 0 || cfg.Discovery.IntervalSeconds < 10 || cfg.Discovery.ConnectTimeoutMilliseconds < 50 || cfg.Discovery.MaximumConcurrency < 1 {
		return errors.New("invalid timeout, discovery interval or concurrency")
	}
	for _, network := range cfg.Discovery.CIDRRanges {
		if _, _, err := net.ParseCIDR(network); err != nil {
			return fmt.Errorf("invalid CIDR range %q", network)
		}
	}
	if len(strings.TrimSpace(cfg.FriendlyName)) > 120 {
		return errors.New("system name is longer than 120 bytes")
	}
	if _, changed := changes["friendly_name"]; changed {
		var old string
		if previous := configuredNames.names[""]; previous != "" {
			old = previous
		} else {
			old = appConfig.FriendlyName
		}
		if old == "" {
			old, _ = os.Hostname()
		}
		if old != "" && !strings.EqualFold(old, cfg.FriendlyName) {
			var previous []string
			_ = json.Unmarshal(saved["previous_friendly_names"], &previous)
			previous = append(previous, old)
			saved["previous_friendly_names"], _ = json.Marshal(previous)
		}
	}
	seen := map[string]bool{}
	for _, timer := range cfg.OnBehalfOf {
		if !isNumericID(timer.ID) || seen[timer.ID] {
			return fmt.Errorf("on-behalf ID %q must be numeric and unique", timer.ID)
		}
		if len(timer.Name) > 120 {
			return fmt.Errorf("name for on-behalf timer %s is too long", strconv.Quote(timer.ID))
		}
		seen[timer.ID] = true
	}
	return nil
}
