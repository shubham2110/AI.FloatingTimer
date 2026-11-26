//go:build windows

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestRenameUpdatesConfigWithoutDroppingOtherSettings(t *testing.T) {
	configuredNames.Lock()
	previous, aliases := configuredNames.names, configuredNames.previous
	configuredNames.names = make(map[string]string)
	configuredNames.Unlock()
	t.Cleanup(func() {
		configuredNames.Lock()
		configuredNames.names = previous
		configuredNames.previous = aliases
		configuredNames.Unlock()
	})
	filename := filepath.Join(t.TempDir(), ConfigFileName)
	if err := os.WriteFile(filename, []byte(`{"friendly_name":"Old","pricing_group":"Console","on_behalf_of":[{"id":"1","name":"Before","show_command":["display"]}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := saveSystemName(filename, systemRename{Name: "Desk"}); err != nil {
		t.Fatal(err)
	}
	if err := saveSystemName(filename, systemRename{Name: "Stage", Path: "/1"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	var cfg AppConfig
	if err = json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.FriendlyName != "Desk" || cfg.PricingGroup != "Console" || len(cfg.OnBehalfOf) != 1 || cfg.OnBehalfOf[0].Name != "Stage" || len(cfg.OnBehalfOf[0].ShowCommand) != 1 {
		t.Fatalf("saved config: %+v", cfg)
	}
}

func TestCleanOfflineKeepsSystemWithReachableAlias(t *testing.T) {
	isolateDiscovery(t)
	online := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer online.Close()
	mergeRecords(t, &store, PC{Name: "Desk", Address: online.URL}, PC{Name: "Desk", Address: "http://127.0.0.1:1"}, PC{Name: "Offline", Address: "http://127.0.0.1:2"}, PC{Name: "Link", Address: "http://169.254.10.8:18081"})
	w := httptest.NewRecorder()
	cleanOfflineHandler(w, httptest.NewRequest(http.MethodPost, "/api/discovery/clean-offline", nil))
	if w.Code != 200 || len(store.list()) != 2 {
		t.Fatalf("cleanup got %d and %+v", w.Code, store.list())
	}
}
