//go:build windows

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestBillingOwnerAndTimerPathSurviveInterfaceAliases(t *testing.T) {
	isolateDiscovery(t)
	configureBillingTest(t)
	appConfig.FriendlyName = "Desk"
	server, _, _, _ := newBillingTimerServer(t, false)
	targetURL, _ := url.Parse(server.URL)
	timerHTTP = &http.Client{Transport: billingPeerRoundTripper(func(r *http.Request) (*http.Response, error) {
		forward := r.Clone(r.Context())
		forward.URL.Host = targetURL.Host
		return http.DefaultTransport.RoundTrip(forward)
	})}
	wifi := PC{ID: "dashboard-only", Name: "Console", Owner: "Desk", Address: "http://192.0.2.232:18081/1"}
	other := wifi
	other.ID = "second-interface"
	other.Address = "http://198.51.100.1:18081/1"
	setBillingTestPCs(wifi, other)
	q := StartSessionRequest{PCID: "unknown-on-owner", Category: "PC Pro", CustomerName: "Test", Players: 1, DurationMinutes: 15, Target: targetForPC(wifi)}
	post := func() *httptest.ResponseRecorder {
		data, _ := json.Marshal(q)
		w := httptest.NewRecorder()
		localSessionHandler(w, httptest.NewRequest(http.MethodPost, "/internal/api/local/sessions", bytes.NewReader(data)))
		return w
	}
	w := post()
	if w.Code != 201 {
		t.Fatalf("start by owner and path: %d %s", w.Code, w.Body.String())
	}
	var started Session
	if err := json.Unmarshal(w.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if !sessionAtPC(started, wifi) || !sessionAtPC(started, other) {
		t.Fatal("alias lost the session")
	}
	if _, err := startBillingSessionForDashboard(StartSessionRequest{PCID: other.ID, Category: "PC Pro", CustomerName: "Duplicate", Players: 1, DurationMinutes: 15}); err == nil {
		t.Fatal("alias created a second bill")
	}
	ext, err := extendSessionForDashboard(ExtendSessionRequest{PCID: other.ID, SessionID: started.ID, Seconds: 300, RequestID: "alias-test"})
	if err != nil || !ext.SessionFound {
		t.Fatalf("extend via alias: %+v %v", ext, err)
	}
	if _, err := updateSessionForDashboard(started.ID, "close", 0, 0); err != nil {
		t.Fatal(err)
	}
	q.Target.Owner = "Another PC"
	if w := post(); w.Code != 404 {
		t.Fatalf("accepted a foreign owner: %s", w.Body.String())
	}
	q.Target.Owner = "Desk"
	q.Target.Path = "/999"
	if w := post(); w.Code != 404 {
		t.Fatalf("accepted unknown virtual timer: %s", w.Body.String())
	}
}
