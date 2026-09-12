//go:build windows

package main

import (
	"fmt"
	"net"
)

// Reserve both ports before any timers, UI, or discovery goroutines start.
func reserveListeners(timerPort, uiPort int) (net.Listener, net.Listener, error) {
	if timerPort == uiPort {
		return nil, nil, fmt.Errorf("timer_port and ui_discovery_port must differ (%d)", timerPort)
	}
	timer, err := net.Listen("tcp", fmt.Sprintf(":%d", timerPort))
	if err != nil {
		return nil, nil, fmt.Errorf("cannot reserve timer port %d: %w", timerPort, err)
	}
	ui, err := net.Listen("tcp", fmt.Sprintf(":%d", uiPort))
	if err != nil {
		timer.Close()
		return nil, nil, fmt.Errorf("cannot reserve UI port %d: %w", uiPort, err)
	}
	return timer, ui, nil
}
