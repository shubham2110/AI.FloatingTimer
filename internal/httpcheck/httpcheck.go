package httpcheck

import (
	"context"
	"errors"
	"fmt"
	"encoding/json"
	"log"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

const RequestIDHeader = "X-Request-ID"
const ParentRequestIDHeader = "X-Parent-Request-ID"

var requestSequence atomic.Uint64
type requestIDContextKey struct{}

func NewRequestID() string {
	return fmt.Sprintf("%x-%x", time.Now().UTC().UnixNano(), requestSequence.Add(1))
}

func SafeRequestID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDContextKey{}, requestID)
}

func ParentRequestID(ctx context.Context) string {
	requestID, _ := ctx.Value(requestIDContextKey{}).(string)
	if SafeRequestID(requestID) {
		return requestID
	}
	return ""
}

func DecodeJSON(body []byte, contentType string, destination any) error {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || (mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json")) {
		return fmt.Errorf("unexpected JSON response content type")
	}
	if err := json.Unmarshal(body, destination); err != nil {
		return fmt.Errorf("invalid JSON response: %w", err)
	}
	return nil
}

func Do(client *http.Client, request *http.Request, feature, deviceID string, logger *log.Logger) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if request.Header.Get(RequestIDHeader) == "" {
		request.Header.Set(RequestIDHeader, NewRequestID())
	}
	parentID := ParentRequestID(request.Context())
	if parentID != "" && parentID != request.Header.Get(RequestIDHeader) {
		request.Header.Set(ParentRequestIDHeader, parentID)
	}
	if deviceID != "" && request.Header.Get("X-Device-ID") == "" {
		request.Header.Set("X-Device-ID", safeLogValue(deviceID, 128))
	}
	started := time.Now()
	response, err := client.Do(request)
	status := 0
	if response != nil {
		status = response.StatusCode
	}
	outcome := "no_response"
	errorKind := ""
	if err != nil {
		var netErr net.Error
		switch {
		case errors.Is(err, context.Canceled):
			outcome = "canceled"
			errorKind = "context_canceled"
		case errors.Is(err, context.DeadlineExceeded):
			outcome = "timeout"
			errorKind = "deadline_exceeded"
		case errors.As(err, &netErr) && netErr.Timeout():
			outcome = "timeout"
			errorKind = "network_timeout"
		default:
			outcome = "transport_error"
			errorKind = fmt.Sprintf("%T", err)
		}
	} else if response != nil && status >= 200 && status < 300 {
		outcome = "success"
	} else if response != nil {
		outcome = "http_error"
	}
	if logger != nil {
		targetHost := ""
		path := ""
		if request.URL != nil {
			targetHost = request.URL.Host
			path = request.URL.EscapedPath()
		}
		logger.Printf(
			"http-client timestamp=%q request_id=%q parent_request_id=%q method=%s path=%q target=%q feature=%q device_id=%q status=%d outcome=%s error_kind=%q duration_ms=%d",
			started.UTC().Format(time.RFC3339Nano), request.Header.Get(RequestIDHeader),
			request.Header.Get(ParentRequestIDHeader), request.Method, safeLogValue(path, 1024), safeLogValue(targetHost, 256),
			safeLogValue(feature, 64), safeLogValue(deviceID, 128), status, outcome, errorKind,
			time.Since(started).Milliseconds(),
		)
	}
	return response, err
}

func safeLogValue(value string, limit int) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
	if len(value) > limit {
		return value[:limit]
	}
	return value
}
