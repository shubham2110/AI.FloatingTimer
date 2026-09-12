//go:build windows

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCountdownTransitions(t *testing.T) {
	c := Countdown{}
	c.apply("set", 2)
	if c.tick() || c.Remaining != 2 {
		t.Fatal("paused timer ticked")
	}
	c.apply("play", 0)
	if c.tick() || !c.tick() || c.tick() {
		t.Fatal("expiry must fire once")
	}
	if c.Remaining != 0 || !c.IsRunning {
		t.Fatal("expiry changed running state")
	}
	c.apply("set", 10)
	c.apply("tweak", -20)
	if c.Remaining != 0 || !c.IsRunning {
		t.Fatal("tweak did not clamp/preserve state")
	}
	c.apply("pause", 0)
	c.apply("set", 10)
	if c.status("", "", true).SecondsUntilTimeUp != nil {
		t.Fatal("paused status has an expiry")
	}
	c.apply("reset", 0)
	status := c.status("", "", false)
	if status.IsRunning || status.RemainingSeconds != 0 || *status.SecondsUntilTimeUp != 0 {
		t.Fatal(status)
	}
}

func TestTimerRoutesShareValidationAndPreserveShowHideState(t *testing.T) {
	for _, prefix := range []string{"", "/1"} {
		t.Run(prefix, func(t *testing.T) {
			c := Countdown{Remaining: 30, IsRunning: true}
			mux := http.NewServeMux()
			registerTimerCommands(mux, prefix, func(action string, value int) int { c.apply(action, value); return c.Remaining })
			for _, tc := range []struct {
				method, path, body string
				code               int
			}{
				{"GET", "/play", "", 405}, {"POST", "/set", `{"seconds":-1}`, 400},
				{"POST", "/set", `{"seconds":1.5}`, 400}, {"POST", "/tweak", `{}`, 400},
				{"POST", "/show", "", 200}, {"POST", "/hide", "", 200}, {"POST", "/dismiss", "", 200},
			} {
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, httptest.NewRequest(tc.method, prefix+tc.path, strings.NewReader(tc.body)))
				if w.Code != tc.code {
					t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
				}
				if c.Remaining != 30 || !c.IsRunning {
					t.Fatal("invalid request or show/hide changed countdown")
				}
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("POST", prefix+"/set?seconds=42", nil))
			if w.Code != 200 || c.Remaining != 42 {
				t.Fatal("query parameter compatibility lost")
			}
		})
	}
}
