package httpcheck

import (
	"bytes"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDoTracksGETPOSTStatusesAndRequestIDs(t *testing.T) {
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	methods := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods <- r.Method
		if r.Header.Get(RequestIDHeader) == "" || r.Header.Get(ParentRequestIDHeader) != "browser-id" ||
			r.Header.Get("X-Device-ID") != "PC-4" {
			t.Errorf("request headers omitted correlation metadata")
		}
		switch r.URL.Path {
		case "/missing":
			http.NotFound(w, r)
		case "/failure":
			http.Error(w, "internal error", http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()
	client := server.Client()

	for _, test := range []struct {
		method, path, wantOutcome string
		wantStatus                int
	}{
		{method: http.MethodGet, path: "/ok", wantStatus: http.StatusNoContent, wantOutcome: "success"},
		{method: http.MethodPost, path: "/ok", wantStatus: http.StatusNoContent, wantOutcome: "success"},
		{method: http.MethodPost, path: "/missing", wantStatus: http.StatusNotFound, wantOutcome: "http_error"},
		{method: http.MethodPost, path: "/failure", wantStatus: http.StatusInternalServerError, wantOutcome: "http_error"},
	} {
		req, err := http.NewRequest(test.method, server.URL+test.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req = req.WithContext(WithRequestID(req.Context(), "browser-id"))
		response, err := Do(client, req, "billing", "PC-4", logger)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != test.wantStatus {
			t.Fatalf("%s %s returned %d, want %d", test.method, test.path, response.StatusCode, test.wantStatus)
		}
		response.Body.Close()
		if method := <-methods; method != test.method {
			t.Fatalf("sent method %s, want %s", method, test.method)
		}
		if !strings.Contains(logs.String(), "status="+strconv.Itoa(test.wantStatus)) ||
			!strings.Contains(logs.String(), "outcome="+test.wantOutcome) ||
			!strings.Contains(logs.String(), `parent_request_id="browser-id"`) ||
			!strings.Contains(logs.String(), `device_id="PC-4"`) {
			t.Fatalf("request log lacks status or device attribution: %s", logs.String())
		}
		logs.Reset()
	}
}

func TestDoReportsConnectionFailureAndTimeout(t *testing.T) {
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := closed.Addr().String()
	closed.Close()
	req, _ := http.NewRequest(http.MethodGet, "http://"+address+"/refused", nil)
	if response, err := Do(&http.Client{Timeout: time.Second}, req, "probe", "", logger); err == nil || response != nil {
		t.Fatalf("closed listener returned response=%v err=%v", response, err)
	}
	if !strings.Contains(logs.String(), "outcome=transport_error") {
		t.Fatalf("connection failure was not logged: %s", logs.String())
	}
	logs.Reset()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	req, _ = http.NewRequest(http.MethodGet, server.URL+"/slow", nil)
	if response, err := Do(&http.Client{Timeout: 10 * time.Millisecond}, req, "probe", "", logger); err == nil || response != nil {
		if response != nil {
			response.Body.Close()
		}
		t.Fatalf("slow request returned response=%v err=%v", response, err)
	}
	if !strings.Contains(logs.String(), "outcome=timeout") {
		t.Fatalf("timeout was not logged: %s", logs.String())
	}
}

func TestRequestIDsAreValidAndUnique(t *testing.T) {
	first, second := NewRequestID(), NewRequestID()
	if first == second || !SafeRequestID(first) || !SafeRequestID(second) {
		t.Fatalf("generated request IDs are invalid or duplicate: %q %q", first, second)
	}
	if SafeRequestID("forged\r\nid") || SafeRequestID(strings.Repeat("a", 129)) {
		t.Fatal("unsafe request ID accepted")
	}
}

func TestDecodeJSONRejectsMalformedAndUnexpectedContentTypes(t *testing.T) {
	var value map[string]string
	if err := DecodeJSON([]byte(`{"status":"ok"}`), "application/json; charset=utf-8", &value); err != nil ||
		value["status"] != "ok" {
		t.Fatalf("valid JSON response rejected: value=%v err=%v", value, err)
	}
	if err := DecodeJSON([]byte(`<html/>`), "text/html", &value); err == nil {
		t.Fatal("unexpected content type accepted")
	}
	if err := DecodeJSON([]byte(`{"status":`), "application/json", &value); err == nil {
		t.Fatal("malformed JSON accepted")
	}
}
