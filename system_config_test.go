//go:build windows

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSystemConfigScopeAndPreservation(t *testing.T) {
	defaults, _ := json.Marshal(defaultAppConfig())
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(defaults, &fields); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]json.RawMessage{"on_behalf_of": json.RawMessage(`[{"id":"1","name":"Screen","show_command":["display.exe"],"extension":true}]`), "extension": json.RawMessage(`true`)}
	changes := map[string]json.RawMessage{"on_behalf_of": json.RawMessage(`[{"id":"1","pricing_group":"Console"}]`)}
	if err := mergeSystemConfig(cfg, changes, fields, false); err != nil {
		t.Fatal(err)
	}
	var timers []map[string]json.RawMessage
	if err := json.Unmarshal(cfg["on_behalf_of"], &timers); err != nil {
		t.Fatal(err)
	}
	if string(timers[0]["show_command"]) != `["display.exe"]` || string(timers[0]["pricing_group"]) != `"Console"` || string(cfg["extension"]) != "true" {
		t.Fatalf("saved %+v", cfg)
	}
	for _, changes := range []map[string]json.RawMessage{
		{"special_commands": json.RawMessage(`[]`)},
		{"on_behalf_of": json.RawMessage(`[{"id":"1","show_command":["other.exe"]}]`)},
	} {
		if mergeSystemConfig(cfg, changes, fields, false) == nil {
			t.Fatal("LAN request altered a command field")
		}
	}
	req := httptest.NewRequest(http.MethodPut, "http://localhost:18082/api/pcs/local/config", nil)
	req.RemoteAddr = "127.0.0.1:30000"
	req.Header.Set("Origin", "http://localhost:18082")
	if !localConfigBrowser(req) {
		t.Fatal("owner dashboard should have full access")
	}
	req.Header.Set("Origin", "http://other.example")
	if localConfigBrowser(req) {
		t.Fatal("accepted cross-origin update")
	}
}

func TestSavedCategoriesAgreeWithDashboardAndBilling(t *testing.T) {
	configureBillingTest(t)
	prior := livePricing.Load()
	t.Cleanup(func() { livePricing.Store(prior) })
	config := filepath.Join(t.TempDir(), ConfigFileName)
	if err := os.WriteFile(config, []byte(`{"pricing_group":"PC Pro"}`), 0600); err != nil {
		t.Fatal(err)
	}
	pc := localRegistryPC()
	pc.PricingGroup = "PC Pro"
	setBillingTestPCs(pc)
	for _, category := range []string{"PS5", "Racing Pro"} {
		body, _ := json.Marshal(map[string][]string{"pricing_categories": {category}})
		save := httptest.NewRecorder()
		serveSystemConfigFile(save, httptest.NewRequest("PUT", "/config", strings.NewReader(string(body))), false, config)
		if save.Code != 200 {
			t.Fatal(save.Body.String())
		}
		list := httptest.NewRecorder()
		pcsHandler(list, httptest.NewRequest("GET", "/api/pcs", nil))
		var pcs []PC
		if err := json.Unmarshal(list.Body.Bytes(), &pcs); err != nil || len(pcs) != 1 || !slices.Equal(pcs[0].PricingCategories, []string{category}) {
			t.Fatalf("dashboard after selecting %s: %s", category, list.Body.String())
		}
		req := StartSessionRequest{PCID: pc.ID, Target: targetForPC(pc), Category: category, Players: 1, DurationMinutes: 30, AllowIncomplete: true}
		if _, _, err := billingStore.prepareStart(req); err != nil {
			t.Fatalf("billing rejected dashboard category %s: %v", category, err)
		}
		req.Category = "PC Pro"
		if _, _, err := billingStore.prepareStart(req); err == nil {
			t.Fatal("billing accepted an unassigned category")
		}
	}
}
