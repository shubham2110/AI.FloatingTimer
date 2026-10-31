//go:build windows

package main

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"overlay-timer/internal/httpcheck"
)

func TestRequestLogMiddlewareUsesActualResponseStatus(t *testing.T) {
	oldOutput := log.Writer()
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldOutput) })

	mux := http.NewServeMux()
	mux.HandleFunc("/known", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET required", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/failure", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	})
	handler := requestLogMiddleware(mux)

	tests := []struct {
		path, method, requestID string
		wantStatus              int
		wantRoute               string
	}{
		{path: "/known", method: http.MethodGet, requestID: "front-end-123", wantStatus: http.StatusOK, wantRoute: "/known"},
		{path: "/known", method: http.MethodPost, requestID: "method-check", wantStatus: http.StatusMethodNotAllowed, wantRoute: "/known"},
		{path: "/failure", method: http.MethodGet, requestID: "server-failure", wantStatus: http.StatusInternalServerError, wantRoute: "/failure"},
		{path: "/unknown", method: http.MethodGet, requestID: "missing-route", wantStatus: http.StatusNotFound, wantRoute: "<unmatched>"},
	}
	for _, test := range tests {
		t.Run(test.path+"-"+test.method, func(t *testing.T) {
			req := httptest.NewRequest(test.method, test.path, nil)
			req.Header.Set(httpcheck.RequestIDHeader, test.requestID)
			req.Header.Set("X-Device-ID", "PC-4\r\nforged=true")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, test.wantStatus)
			}
			if got := rec.Header().Get(httpcheck.RequestIDHeader); got != test.requestID {
				t.Fatalf("response request ID = %q, want %q", got, test.requestID)
			}
			logged := logs.String()
			if !strings.Contains(logged, "request_id=\""+test.requestID+"\"") ||
				!strings.Contains(logged, "route=\""+test.wantRoute+"\"") ||
				!strings.Contains(logged, "device_id=\"PC-4forged=true\"") ||
				!strings.Contains(logged, "status="+strconv.Itoa(test.wantStatus)) {
				t.Fatalf("request log omitted expected route, device, or actual status: %s", logged)
			}
			logs.Reset()
		})
	}
}

func TestHealthHandlerIsReadOnlyAndMethodChecked(t *testing.T) {
	get := httptest.NewRecorder()
	healthHandler(get, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), `"status":"ok"`) {
		t.Fatalf("GET health response = %d %s", get.Code, get.Body.String())
	}
	post := httptest.NewRecorder()
	healthHandler(post, httptest.NewRequest(http.MethodPost, "/api/health", nil))
	if post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST health status = %d, want 405", post.Code)
	}
}
