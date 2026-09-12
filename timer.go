//go:build windows

package main

import (
	"fmt"
	"net/http"
)

// Countdown holds the timer behavior shared by the overlay and virtual timers.
// Its owner holds the mutex; display and command hooks remain owner-specific.
type Countdown struct {
	Remaining int
	IsRunning bool
}

func (c *Countdown) tick() bool {
	if !c.IsRunning || c.Remaining == 0 {
		return false
	}
	c.Remaining--
	return c.Remaining == 0
}

func (c *Countdown) apply(action string, value int) {
	switch action {
	case "set":
		c.Remaining = value
	case "tweak":
		c.Remaining = max(0, c.Remaining+value)
	case "play", "pause":
		c.IsRunning = action == "play"
	case "toggle":
		c.IsRunning = !c.IsRunning
	case "reset":
		c.Remaining, c.IsRunning = 0, false
	}
}

type TimerStatus struct {
	ID                 string `json:"id,omitempty"`
	Name               string `json:"name,omitempty"`
	RemainingSeconds   int    `json:"remaining_seconds"`
	IsRunning          bool   `json:"is_running"`
	TimerStatus        string `json:"timer_status"`
	TimeUpVisible      bool   `json:"time_up_visible"`
	SecondsUntilTimeUp *int   `json:"seconds_until_time_up"`
}

func (c *Countdown) status(id, name string, visible bool) TimerStatus {
	status := TimerStatus{
		ID: id, Name: name, RemainingSeconds: c.Remaining,
		IsRunning: c.IsRunning, TimerStatus: "paused", TimeUpVisible: visible,
	}
	if c.IsRunning {
		status.TimerStatus = "running"
	}
	if c.IsRunning || c.Remaining == 0 {
		value := c.Remaining
		status.SecondsUntilTimeUp = &value
	}
	return status
}

func timerParameter(r *http.Request, action string) (int, error) {
	parameter := ""
	switch action {
	case "set":
		parameter = "seconds"
	case "tweak":
		parameter = "amount"
	}
	if parameter == "" {
		return 0, nil
	}
	value, err := readIntegerParameter(r, parameter)
	if err == nil && action == "set" && value < 0 {
		err = fmt.Errorf("seconds must be zero or greater")
	}
	return value, err
}

var timerActions = []string{"set", "tweak", "play", "pause", "toggle", "reset", "show", "hide", "dismiss"}

func registerTimerCommands(mux *http.ServeMux, prefix string, apply func(string, int) int) {
	for _, action := range timerActions {
		mux.HandleFunc(prefix+"/"+action, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				writeJSON(w, 405, map[string]string{"message": "POST required"})
				return
			}
			value, err := timerParameter(r, action)
			if err != nil {
				writeJSON(w, 400, map[string]string{"status": "error", "message": err.Error()})
				return
			}
			remaining := apply(action, value)
			result := map[string]interface{}{"status": "success"}
			if prefix != "" || action == "set" || action == "tweak" {
				result["remaining_seconds"] = remaining
			}
			writeJSON(w, 200, result)
		})
	}
}
