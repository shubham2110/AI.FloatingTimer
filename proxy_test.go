//go:build windows

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProxyDestinationBoundaries(t *testing.T) {
	pc := PC{Address: "http://192.168.1.10:18081/2", UIPort: 18082}
	for _, test := range []struct{ service, action, method, want string }{
		{"host", "status", "GET", "http://192.168.1.10:18081/status"},
		{"timer", "pause", "POST", "http://192.168.1.10:18081/2/pause"},
		{"manager", "node", "GET", "http://192.168.1.10:18082/api/node"},
		{"manager", "config", "POST", ""}, {"timer", "pause", "GET", ""}, {"timer", "../status", "GET", ""},
	} {
		got, err := proxyDestination(pc, test.service, test.action, test.method)
		if got != test.want || (err == nil) != (test.want != "") {
			t.Fatalf("%+v: %s %v", test, got, err)
		}
	}
	for _, addr := range []string{"http://192.168.1.10:80", "http://example.org:18081", "http://8.8.8.8:18081", "http://169.254.1.1:18081"} {
		pc.Address = addr
		if _, err := proxyDestination(pc, "host", "status", "GET"); err == nil {
			t.Fatal(addr)
		}
	}
}
func TestNodeProxyCredentials(t *testing.T) {
	file := filepath.Join(t.TempDir(), "auth.json")
	p := newProxyService(file)
	call := func(path, body string, forwarded bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://localhost:18082/api/proxy/"+path, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("Origin", "http://localhost:18082")
		r.Header.Set("Content-Type", "application/json")
		if forwarded {
			r.Header.Set("X-Forwarded-For", "127.0.0.1")
			r.Header.Set("Origin", "https://localhost:18082")
		}
		w := httptest.NewRecorder()
		p.auth(w, r)
		return w
	}
	defaultBody := `{"Password":"Paradise` + configuredFriendlyName() + `","NewPassword":"test-only-password"}`
	login := call("login", defaultBody, false)
	if login.Code != 200 {
		t.Fatal(login.Body.String())
	}
	r := httptest.NewRequest("GET", "/api/proxy/relay/unknown/host/status", nil)
	w := httptest.NewRecorder()
	p.relay(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	r.AddCookie(login.Result().Cookies()[0])
	if !p.valid(r) {
		t.Fatal("login cookie invalid")
	}
	if call("password", defaultBody, true).Code != 403 {
		t.Fatal("forwarded password update accepted")
	}
	if call("password", defaultBody, false).Code != 200 || p.valid(r) {
		t.Fatal("password rotation failed")
	}
	if call("login", defaultBody, false).Code != 401 || !newProxyService(file).matches("test-only-password") {
		t.Fatal("saved password mismatch")
	}
	w = httptest.NewRecorder()
	p.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })).ServeHTTP(w, httptest.NewRequest("GET", "/api/pcs", nil))
	if w.Code != 401 {
		t.Fatal("unprotected remote management route")
	}
}

type proxyTestTransport func(*http.Request) (*http.Response, error)

func (f proxyTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestAuthenticatedRelayForwardsOnlyTimerRequest(t *testing.T) {
	configureBillingTest(t)
	setBillingTestPCs(PC{ID: "screen", Address: "http://192.168.1.10:18081/2"})
	p := newProxyService(filepath.Join(t.TempDir(), "auth.json"))
	p.sessions["verified"] = time.Now().Add(time.Minute)
	p.client.Transport = proxyTestTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "http://192.168.1.10:18081/2/pause" || req.Method != "POST" || req.Header.Get("Cookie") != "" || req.Header.Get("Authorization") != "" {
			t.Fatalf("unexpected upstream request %s", req.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"success"}`))}, nil
	})
	r := httptest.NewRequest("POST", "https://portal.example/api/proxy/relay/screen/timer/pause", strings.NewReader("{}"))
	r.Header.Set("Origin", "https://portal.example")
	r.Header.Set("Authorization", "test")
	r.AddCookie(&http.Cookie{Name: proxyCookie, Value: "verified"})
	w := httptest.NewRecorder()
	p.relay(w, r)
	if w.Code != 200 || w.Body.String() != `{"status":"success"}` {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestDirectLANProbeIsLocalAndInstanceSpecific(t *testing.T) {
	p := newProxyService(filepath.Join(t.TempDir(), "auth"))
	for _, test := range []struct {
		peer      string
		forwarded bool
		status    int
	}{{"127.0.0.1:9090", false, 200}, {"127.0.0.1:9090", true, 403}, {"203.0.113.8:9090", false, 403}} {
		r := httptest.NewRequest("GET", "http://localhost:18082/api/proxy/local-probe", nil)
		r.RemoteAddr = test.peer
		if test.forwarded {
			r.Header.Set("X-Forwarded-For", "192.168.1.10")
		}
		w := httptest.NewRecorder()
		p.networkProbe(w, r)
		if w.Code != test.status || (w.Code == 200 && !strings.Contains(w.Body.String(), p.instance)) {
			t.Fatalf("%+v: %d", test, w.Code)
		}
	}
}
