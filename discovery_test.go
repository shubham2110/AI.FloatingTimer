//go:build windows

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
)

func isolateDiscovery(t *testing.T) {
	t.Helper()
	previousConfig, previousPCs, previousPath := appConfig, store.snapshot(), store.path
	previousTimers := virtualTimers.items
	t.Cleanup(func() {
		appConfig = previousConfig
		store.PCs, store.path = previousPCs, previousPath
		virtualTimers.items = previousTimers
	})
	appConfig = defaultAppConfig()
	store.PCs, store.path = nil, testStore(t).path
	virtualTimers.items = map[string]*VirtualTimer{
		"1":  {Config: OnBehalfConfig{ID: "1", Name: "Stage display"}},
		"20": {Config: OnBehalfConfig{ID: "20", Name: "Lobby display"}},
	}
}

func TestIdentityManifestAndLocalRegistration(t *testing.T) {
	isolateDiscovery(t)
	identity := localNodeIdentity()
	if len(identity.Timers) != 3 || identity.Timers[0].ID != "" || identity.Timers[0].Path != "" || identity.Timers[1].Path != "/1" || identity.Timers[2].Path != "/20" {
		t.Fatalf("incorrect manifest: %+v", identity)
	}
	mux := http.NewServeMux()
	registerDiscoveryHandlers(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/node", nil))
	var advertised NodeIdentity
	if err := json.Unmarshal(w.Body.Bytes(), &advertised); err != nil || len(advertised.Timers) != 3 {
		t.Fatalf("UI identity omitted hosted timers: %s %v", w.Body.String(), err)
	}
	observeLocalTimers()
	observeLocalTimers()
	if len(store.list()) != 3 {
		t.Fatalf("startup must register each timer exactly once: %+v", store.list())
	}
	for _, pc := range store.list() {
		if pc.LastSeen.IsZero() {
			t.Fatal("local timer missing observation")
		}
	}
}

func TestDiscoveryRegistersManifestWithoutStatusProbes(t *testing.T) {
	isolateDiscovery(t)
	var identityCalls, peerCalls, statusCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/identity":
			identityCalls.Add(1)
			writeJSON(w, 200, localNodeIdentity())
		case "/api/discovery/peers":
			peerCalls.Add(1)
			writeJSON(w, 200, []PC{})
		default:
			statusCalls.Add(1)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(u.Port())
	appConfig.TimerPort, appConfig.UIDiscoveryPort = port, port
	appConfig.Discovery.CIDRRanges = []string{"127.0.0.0/30"}
	appConfig.Discovery.ConnectTimeoutMilliseconds = 500
	discoveryState.Lock()
	previousResult, previousRunning := discoveryState.last, discoveryState.running
	discoveryState.Unlock()
	t.Cleanup(func() {
		discoveryState.Lock()
		discoveryState.last, discoveryState.running = previousResult, previousRunning
		discoveryState.Unlock()
	})
	performDiscovery(true)
	if identityCalls.Load() != 1 || peerCalls.Load() != 1 || statusCalls.Load() != 0 {
		t.Fatalf("unexpected requests: identity=%d peers=%d status=%d", identityCalls.Load(), peerCalls.Load(), statusCalls.Load())
	}
	if discoveryState.last.HostsFound != 1 || discoveryState.last.Found != 3 || len(store.list()) != 3 {
		t.Fatalf("host/timer counts wrong: %+v, %+v", discoveryState.last, store.list())
	}
	for _, path := range []string{"", "/1", "/20"} {
		pc, ok := store.find(stablePCID(server.URL + path))
		if !ok || pc.Address != server.URL+path || pc.UIPort != port {
			t.Fatalf("missing or incorrect timer %q: %+v", path, pc)
		}
	}
}

func TestIdentityLegacyAndInvalidEntries(t *testing.T) {
	identity := NodeIdentity{Service: "overlay-timer", Name: "PC4", TimerPort: 18081, UIPort: 18082}
	const address = "http://192.168.1.15:18081"
	legacy := identityPCs(address, identity)
	if len(legacy) != 1 || legacy[0].ID != stablePCID(address) {
		t.Fatal("legacy identity changed the host's registry ID")
	}
	identity.Timers = []TimerIdentity{
		{ID: "", Name: "PC4", Path: ""},
		{ID: "1", Name: "Stage", Path: "/1"},
		{ID: "1", Name: "Duplicate", Path: "/1"},
		{ID: "5", Path: "/5"},
		{ID: "6", Path: "/7"},
		{ID: "7", Path: "http://other-host:18081/7"},
		{ID: "../8", Path: "/../8"},
	}
	pcs := identityPCs(address, identity)
	if len(pcs) != 3 || pcs[1].Name != "Stage" || pcs[2].Name != "Agent 5" {
		t.Fatalf("manifest validation failed: %+v", pcs)
	}
	other := identityPCs("http://192.168.1.16:18081", identity)
	if pcs[0].ID == pcs[1].ID || pcs[1].ID == other[1].ID {
		t.Fatal("different timer endpoints share a registry ID")
	}
}

func TestTimerAddressesSurviveRegistrySyncAndSelection(t *testing.T) {
	isolateDiscovery(t)
	for input, want := range map[string]string{
		"PC4:18081/1/": "http://pc4:18081/1",
		"PC4/20":       "http://pc4:18081/20",
		"PC4:18081/":   "http://pc4:18081",
	} {
		got, err := normalizeAddress(input)
		if err != nil || got != want {
			t.Fatalf("normalize %q = %q, %v", input, got, err)
		}
	}
	for _, input := range []string{"pc4/1/status", "pc4/name", "pc4//1", "pc4/../1", "pc4/%31", "pc4/1?x=1", "pc4/1#fragment", "http://user:password@pc4/1"} {
		if _, err := normalizeAddress(input); err == nil {
			t.Fatalf("accepted invalid address %q", input)
		}
	}
	pcs := identityPCs("http://pc4:18081", localNodeIdentity())
	mergeRecords(t, &store, pcs...)
	if _, err := store.setActive(pcs[1].ID, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.edit(pcs[2].ID, PC{}, true); err != nil {
		t.Fatal(err)
	}
	mergeRecords(t, &store, pcs...)
	restarted := &PCStore{path: store.path}
	if err := restarted.load(); err != nil {
		t.Fatal(err)
	}
	if len(restarted.list()) != 2 || len(restarted.active()) != 1 || restarted.active()[0].ID != pcs[0].ID {
		t.Fatalf("discovery/restart lost per-timer preferences: %+v", restarted.snapshot())
	}
	remote := testStore(t)
	mergeRecords(t, remote, restarted.snapshot()...)
	if len(remote.list()) != 2 || remote.list()[1].Address != "http://pc4:18081/1" {
		t.Fatalf("peer sync lost timer address: %+v", remote.snapshot())
	}
}

func TestDiscoveryExclusionsRespectSiblingTimers(t *testing.T) {
	isolateDiscovery(t)
	const host = "127.0.0.1"
	for _, tc := range []struct {
		name    string
		pcs     []PC
		blocked bool
	}{
		{"inactive child", []PC{{Address: "http://127.0.0.1:18081/1", Inactive: true}}, false},
		{"deleted child", []PC{{Address: "http://127.0.0.1:18081/1", Deleted: true}}, false},
		{"inactive root with active child", []PC{{Address: "http://127.0.0.1:18081", Inactive: true}, {Address: "http://127.0.0.1:18081/1"}}, false},
		{"deleted root with active child", []PC{{Address: "http://127.0.0.1:18081", Deleted: true}, {Address: "http://127.0.0.1:18081/1"}}, false},
		{"all inactive", []PC{{Address: "http://127.0.0.1:18081", Inactive: true}, {Address: "http://127.0.0.1:18081/1", Inactive: true}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store.PCs = tc.pcs
			if got := blockedDiscoveryHosts()[host]; got != tc.blocked {
				t.Fatalf("blocked=%v, want %v", got, tc.blocked)
			}
		})
	}
}

func TestDistributionSendsAllTimersOncePerHost(t *testing.T) {
	isolateDiscovery(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/discovery/peers" {
			t.Errorf("distribution used timer path: %s", r.URL.Path)
		}
		var peers []PC
		if err := json.NewDecoder(r.Body).Decode(&peers); err != nil || len(peers) != 3 {
			t.Errorf("missing timers in distribution: %+v %v", peers, err)
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(u.Port())
	identity := localNodeIdentity()
	identity.UIPort = port
	pcs := identityPCs(server.URL, identity)
	mergeRecords(t, &store, pcs...)
	results := distributePeers(context.Background(), store.active())
	if len(results) != 1 || results[0].Error != "" || calls.Load() != 1 {
		t.Fatalf("expected one host send: %+v (%d calls)", results, calls.Load())
	}
}
