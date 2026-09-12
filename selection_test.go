//go:build windows

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestInactiveSelectionSurvivesRestartAndMerge(t *testing.T) {
	s := testStore(t)
	pc, _, err := s.edit("", PC{Name: "Desk", Address: "http://192.168.1.5:18081"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if pc.Inactive || len(s.active()) != 1 {
		t.Fatal("new PCs must be active")
	}
	if _, err := s.setActive(pc.ID, false); err != nil {
		t.Fatal(err)
	}
	pc.Name, pc.UpdatedAt = "New name", time.Now().Add(time.Hour)
	mergeRecords(t, s, pc)
	restarted := &PCStore{path: s.path}
	if err := restarted.load(); err != nil {
		t.Fatal(err)
	}
	if len(restarted.active()) != 0 || !restarted.list()[0].Inactive {
		t.Fatal("merge/restart reactivated PC")
	}
	pc = restarted.list()[0]
	if _, _, err := restarted.edit(pc.ID, pc, false); err != nil {
		t.Fatal(err)
	}
	if len(restarted.active()) != 0 {
		t.Fatal("edit reactivated PC")
	}
	if _, err := restarted.setActive(pc.ID, true); err != nil {
		t.Fatal(err)
	}
	if len(restarted.active()) != 1 {
		t.Fatal("could not reactivate PC")
	}
	remote := testStore(t)
	pc.Inactive = true
	mergeRecords(t, remote, pc)
	if len(remote.active()) != 1 {
		t.Fatal("imported another manager's inactive preference")
	}
}

func TestManualDiscoveryAndInactiveScanExclusion(t *testing.T) {
	if defaultAppConfig().Discovery.Enabled {
		t.Fatal("automatic discovery enabled by default")
	}
	previousConfig, previousPCs, previousPath := appConfig, store.snapshot(), store.path
	t.Cleanup(func() { appConfig = previousConfig; store.PCs, store.path = previousPCs, previousPath })
	appConfig.Discovery.CIDRRanges = []string{"127.0.0.0/30"}
	store.PCs = []PC{{ID: "inactive", Address: "http://127.0.0.1:18081", Inactive: true}}
	hosts := hostsForNetworks()
	if len(hosts) != 1 || hosts[0] != "127.0.0.2" {
		t.Fatalf("scan includes inactive host: %v", hosts)
	}
	appConfig.Discovery.Enabled = false
	startDiscoveryScheduler()
}

func TestDistributionExcludesInactivePayloadAndDestination(t *testing.T) {
	previousPCs, previousPath := store.snapshot(), store.path
	t.Cleanup(func() { store.PCs, store.path = previousPCs, previousPath })
	store.PCs, store.path = nil, testStore(t).path
	var activeCalls, inactiveCalls atomic.Int32
	active := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var peers []PC
		if err := json.NewDecoder(r.Body).Decode(&peers); err != nil {
			t.Error(err)
		}
		if len(peers) != 1 || peers[0].ID != "active" {
			t.Errorf("unexpected payload: %+v", peers)
		}
		activeCalls.Add(1)
		writeJSON(w, 200, map[string]string{"status": "success"})
	}))
	defer active.Close()
	inactive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { inactiveCalls.Add(1) }))
	defer inactive.Close()
	for id, server := range map[string]*httptest.Server{"active": active, "inactive": inactive} {
		u, _ := url.Parse(server.URL)
		port, _ := strconv.Atoi(u.Port())
		store.PCs = append(store.PCs, PC{ID: id, Name: id, Address: server.URL, UIPort: port, Inactive: id == "inactive"})
	}
	w := httptest.NewRecorder()
	distributeHandler(w, httptest.NewRequest("POST", "/api/discovery/distribute", strings.NewReader(`{"ids":["active","inactive"]}`)))
	if w.Code != 200 || activeCalls.Load() != 1 || inactiveCalls.Load() != 0 {
		t.Fatalf("distribution: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	pcHandler(w, httptest.NewRequest("POST", "/api/pcs/active/play", nil))
	if w.Code != 404 || activeCalls.Load() != 1 {
		t.Fatal("old backend proxy is still available")
	}
}

func TestCORSPreflightDoesNotExecuteCommands(t *testing.T) {
	called := false
	handler := cors(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(200) }))
	r := httptest.NewRequest("OPTIONS", "/1/set", nil)
	r.Header.Set("Origin", "http://other-pc:18082")
	r.Header.Set("Access-Control-Request-Method", "POST")
	r.Header.Set("Access-Control-Request-Headers", "content-type")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if called || w.Code != 204 || w.Header().Get("Access-Control-Allow-Origin") != "*" || w.Header().Get("Access-Control-Allow-Headers") != "*" {
		t.Fatal("preflight failed", w)
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/1/set", nil))
	if !called || w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("actual response missing CORS")
	}
}
