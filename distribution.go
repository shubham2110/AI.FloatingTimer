//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type DistributionResult struct {
	ID      string `json:"id"`
	Address string `json:"address"`
	Error   string `json:"error,omitempty"`
}

func distributeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "POST required"})
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body) != nil || len(body.IDs) == 0 {
		writeJSON(w, 400, map[string]string{"error": "select at least one active PC"})
		return
	}
	selected := map[string]bool{}
	for _, id := range body.IDs {
		selected[id] = true
	}
	peers := []PC{}
	for _, pc := range store.active() {
		if selected[pc.ID] {
			peers = append(peers, pc)
		}
	}
	if len(peers) == 0 {
		writeJSON(w, 400, map[string]string{"error": "no selected PCs are active"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	results := distributePeers(ctx, peers)
	sent := 0
	for _, result := range results {
		if result.Error == "" {
			sent++
		}
	}
	writeJSON(w, 200, map[string]interface{}{"sent": sent, "results": results})
}

// Distribution is explicit, bounded, and never forwards recursively on receipt.
func distributePeers(ctx context.Context, peers []PC) []DistributionResult {
	destinations := []PC{}
	seen := map[string]bool{}
	for _, pc := range peers {
		endpoint := peerEndpoint(pc)
		if !isLocalPeer(pc) && !seen[endpoint] {
			destinations = append(destinations, pc)
			seen[endpoint] = true
		}
	}
	results := make([]DistributionResult, len(destinations))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(8, len(destinations)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range jobs {
				peer := destinations[i]
				result := DistributionResult{ID: peer.ID, Address: peer.Address}
				// Re-check the saved selection before each send.
				current, ok := store.find(peer.ID)
				if !ok || current.Inactive {
					result.Error = "PC is inactive or removed"
				} else {
					payload := []PC{}
					for _, entry := range peers {
						if pc, ok := store.find(entry.ID); ok && !pc.Inactive {
							payload = append(payload, pc)
						}
					}
					data, _ := json.Marshal(payload)
					req, err := http.NewRequestWithContext(ctx, http.MethodPost, peerEndpoint(current), bytes.NewReader(data))
					if err == nil {
						req.Header.Set("Content-Type", "application/json")
						var response *http.Response
						response, err = timerHTTP.Do(req)
						if err == nil {
							response.Body.Close()
							if response.StatusCode != http.StatusOK {
								err = fmt.Errorf("HTTP %d", response.StatusCode)
							}
						}
					}
					if err != nil {
						result.Error = err.Error()
					}
				}
				results[i] = result
			}
		}()
	}
	for i := range destinations {
		jobs <- i
	}
	close(jobs)
	workers.Wait()
	return results
}

func isLocalPeer(pc PC) bool {
	port := pc.UIPort
	if port == 0 {
		port = appConfig.UIDiscoveryPort
	}
	if port != appConfig.UIDiscoveryPort {
		return false
	}
	u, err := url.Parse(pc.Address)
	if err != nil {
		return false
	}
	hostname, _ := os.Hostname()
	if strings.EqualFold(u.Hostname(), hostname) || strings.EqualFold(u.Hostname(), "localhost") {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	if ip != nil && ip.IsLoopback() {
		return true
	}
	for _, own := range localIPv4s() {
		if own.Equal(ip) {
			return true
		}
	}
	return false
}
