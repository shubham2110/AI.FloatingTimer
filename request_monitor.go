//go:build windows

package main

import (
	"bufio"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"overlay-timer/internal/httpcheck"
)

func requestLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := strings.TrimSpace(r.Header.Get(httpcheck.RequestIDHeader))
		if !httpcheck.SafeRequestID(requestID) {
			requestID = httpcheck.NewRequestID()
		}
		r.Header.Set(httpcheck.RequestIDHeader, requestID)
		r = r.WithContext(httpcheck.WithRequestID(r.Context(), requestID))
		w.Header().Set(httpcheck.RequestIDHeader, requestID)

		recorder := &requestStatusWriter{ResponseWriter: w}
		started := time.Now()
		defer func() {
			route := r.Pattern
			if route == "" {
				route = "<unmatched>"
			}
			log.Printf(
				"http-server timestamp=%q request_id=%q method=%s path=%q device_id=%q route=%q status=%d duration_ms=%d",
				started.UTC().Format(time.RFC3339Nano), requestID, r.Method,
				r.URL.EscapedPath(), requestDeviceID(r), route,
				recorder.statusCode(), time.Since(started).Milliseconds(),
			)
		}()

		next.ServeHTTP(recorder, r)
	})
}

func requestDeviceID(r *http.Request) string {
	value := strings.TrimSpace(r.Header.Get("X-Device-ID"))
	if value == "" {
		value = strings.TrimSpace(r.Header.Get("X-PC-ID"))
	}
	if len(value) > 128 {
		value = value[:128]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
}

type requestStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *requestStatusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *requestStatusWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *requestStatusWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *requestStatusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *requestStatusWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *requestStatusWriter) ReadFrom(reader io.Reader) (int64, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if readerFrom, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return readerFrom.ReadFrom(reader)
	}
	return io.Copy(w.ResponseWriter, reader)
}

func (w *requestStatusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("underlying response writer does not support hijacking")
	}
	return hijacker.Hijack()
}

func (w *requestStatusWriter) Push(target string, options *http.PushOptions) error {
	pusher, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, options)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET required"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "overlay-timer"})
}

func monitoredHTTPDo(request *http.Request, feature, deviceID string) (*http.Response, error) {
	return httpcheck.Do(timerHTTP, request, feature, deviceID, log.Default())
}
