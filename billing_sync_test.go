//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"overlay-timer/internal/httpcheck"
)

func TestBillingSyncRetriesAreIdempotentAcrossFourDevices(t *testing.T) {
	directory := configureBillingTest(t)
	const reportDate = "2026-02-02"
	var servers []*httptest.Server
	pcs := make([]PC, 0, 4)
	acknowledgements := make([]*atomic.Int32, 0, 4)
	var forceUnacknowledged atomic.Bool
	forceUnacknowledged.Store(true)
	for i := range 4 {
		deviceID := fmt.Sprintf("PC-%02d", i+1)
		session := Session{
			ID: fmt.Sprintf("S-device-%02d", i+1), DeviceID: deviceID,
			PCID: fmt.Sprintf("timer-%02d", i+1), PCName: fmt.Sprintf("PC %d", i+1),
			PCAddress:    fmt.Sprintf("http://192.0.2.%d:18081", i+1),
			CustomerName: fmt.Sprintf("Customer %d", i+1), Players: 1, Status: "closed",
			StartedAt: "2026-02-01T21:00:00Z", UpdatedAt: "2026-02-01T21:30:00Z",
			CreatedAt: "2026-02-01T21:00:00Z", EndedAt: "2026-02-01T21:30:00Z",
			PurchasedMinutes: 30, FinalAmount: (i + 1) * 10,
			AmountPaid: (i+1)*10 - (i + 1), PaymentStatus: "partial",
		}
		ackCount := &atomic.Int32{}
		acknowledgements = append(acknowledgements, ackCount)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/internal/api/billing/sync/pending":
				writeJSON(w, http.StatusOK, BillingSyncExport{
					DeviceID: deviceID, Name: fmt.Sprintf("PC %d", i+1),
					Sessions: []Session{session},
				})
			case r.Method == http.MethodPost && r.URL.Path == "/internal/api/billing/sync/ack":
				var ack BillingSyncAck
				if err := json.NewDecoder(r.Body).Decode(&ack); err != nil {
					t.Errorf("invalid acknowledgement: %v", err)
					http.Error(w, "invalid acknowledgement", http.StatusBadRequest)
					return
				}
				if ack.DeviceID != deviceID || len(ack.Sessions) != 1 || ack.Sessions[0].ID != session.ID {
					t.Errorf("acknowledgement was sent to the wrong device/session: %+v", ack)
					http.Error(w, "wrong acknowledgement", http.StatusBadRequest)
					return
				}
				ackCount.Add(1)
				if deviceID == "PC-01" && forceUnacknowledged.CompareAndSwap(true, false) {
					writeJSON(w, http.StatusOK, BillingSyncAckResult{Acknowledged: 0})
					return
				}
				writeJSON(w, http.StatusOK, BillingSyncAckResult{Acknowledged: len(ack.Sessions)})
			default:
				http.NotFound(w, r)
			}
		}))
		servers = append(servers, server)
		u, err := url.Parse(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		_, portText, err := net.SplitHostPort(u.Host)
		if err != nil {
			t.Fatal(err)
		}
		uiPort, err := strconv.Atoi(portText)
		if err != nil {
			t.Fatal(err)
		}
		pcs = append(pcs, PC{
			ID: fmt.Sprintf("registry-%02d", i+1), Name: fmt.Sprintf("PC %d", i+1),
			Address: server.URL, UIPort: uiPort,
		})
	}
	t.Cleanup(func() {
		for _, server := range servers {
			server.Close()
		}
	})
	setBillingTestPCs(pcs...)

	for attempt := 1; attempt <= 2; attempt++ {
		statuses, err := syncBillingDevices()
		if err != nil {
			t.Fatalf("synchronization attempt %d failed: %v", attempt, err)
		}
		if len(statuses) != 5 {
			t.Fatalf("expected one local and four remote device statuses, got %d", len(statuses))
		}
		if attempt == 1 {
			foundPending := false
			for _, status := range statuses {
				if status.DeviceID == "PC-01" {
					foundPending = status.PendingRecords == 1 && status.Error != ""
				}
			}
			if !foundPending {
				t.Fatalf("a session that was not acknowledged was not left pending: %+v", statuses)
			}
		}
	}
	sessions, err := billingStore.sessions("", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 4 {
		t.Fatalf("repeated synchronization created duplicate rows: got %d sessions", len(sessions))
	}
	for i, count := range acknowledgements {
		if count.Load() != 2 {
			t.Errorf("PC %d acknowledgement count = %d, want 2", i+1, count.Load())
		}
	}
	report, err := billingStore.report(reportDate)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete || report.SystemCount != 5 || report.SessionCount != 4 ||
		report.UniqueCustomerCount != 4 || report.CustomerCount != 4 ||
		report.BilledAmount != 100 || report.CollectedAmount != 90 ||
		report.Revenue != 90 || report.Outstanding != 10 {
		t.Fatalf("consolidated report has incorrect totals or completeness: %+v", report)
	}
	if len(report.PCs) != 4 || len(report.Records) != 4 {
		t.Fatalf("report did not retain individual PC/customer details: %+v", report)
	}
	var pcOne *BillingReportPC
	for i := range report.PCs {
		if report.PCs[i].DeviceID == "PC-01" {
			pcOne = &report.PCs[i]
			break
		}
	}
	if pcOne == nil || pcOne.SessionCount != 1 || pcOne.BilledAmount != 10 ||
		pcOne.CollectedAmount != 9 || pcOne.Outstanding != 1 {
		t.Fatalf("PC-wise totals do not separate billed, collected, and outstanding amounts: %+v", report.PCs)
	}

	servers[2].Close()
	statuses, err := syncBillingDevices()
	if err != nil {
		t.Fatalf("offline device should be reported rather than aborting all synchronization: %v", err)
	}
	report, err = billingStore.report(reportDate)
	if err != nil {
		t.Fatal(err)
	}
	if report.Complete {
		t.Fatal("report was marked complete while one registered PC was offline")
	}
	offlineFound := false
	for _, status := range statuses {
		if status.Endpoint == pcs[2].Address || status.Name == "PC 3" {
			offlineFound = !status.Online && status.Error != "" && status.LastSuccessfulAt != ""
		}
	}
	if !offlineFound {
		t.Fatalf("offline PC status did not retain its last successful sync: %+v", statuses)
	}
	if len(report.Records) != 4 || report.Revenue != 90 {
		t.Fatalf("offline status changed already synchronized historical totals: %+v", report)
	}
	if filepath.Dir(billingStore.path) != directory {
		t.Fatal("test workbook was not isolated in the test directory")
	}
}

func TestBillingPeerHTTPErrorIdentifiesExactMethodAndURL(t *testing.T) {
	configureBillingTest(t)
	requests := make(chan *http.Request, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Clone(r.Context())
		http.NotFound(w, r)
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	uiPort, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	pc := PC{ID: "pc-four", Name: "PC-4", Address: server.URL, UIPort: uiPort}
	tests := []struct {
		operation string
		method    string
		path      string
		payload   any
	}{
		{operation: "session-start", method: http.MethodPost, path: "/internal/api/local/sessions", payload: StartSessionRequest{}},
		{operation: "session-close", method: http.MethodPost, path: "/internal/api/local/sessions/S-diagnostic-only/close", payload: map[string]string{
			"pc_id": "pc-four", "pc_address": server.URL, "device_id": "device-diagnostic-only",
		}},
		{operation: "session-extension", method: http.MethodPost, path: "/internal/api/local/sessions/extend", payload: ExtendSessionRequest{}},
		{operation: "billing-sync-export", method: http.MethodGet, path: "/internal/api/billing/sync/pending"},
	}
	for _, test := range tests {
		t.Run(test.operation, func(t *testing.T) {
			err := callBillingPeer(pc, test.method, test.path, test.payload, nil)
			if err == nil {
				t.Fatal("expected a missing peer route to remain an error")
			}
			wantURL := server.URL + test.path
			if !strings.Contains(err.Error(), "request "+test.method+" "+wantURL+" returned HTTP 404") ||
				!strings.Contains(err.Error(), "404 page not found") {
				t.Fatalf("peer error did not identify the exact failed request: %v", err)
			}
			var httpErr *billingHTTPError
			if !errors.As(err, &httpErr) || httpErr.Status != http.StatusBadGateway {
				t.Fatalf("peer HTTP failure should be surfaced as HTTP 502, got %v", err)
			}
			request := <-requests
			if request.Method != test.method || request.URL.Path != test.path {
				t.Fatalf("received request %s %s, want %s %s", request.Method, request.URL.Path, test.method, test.path)
			}
		})
	}
}

func TestBillingPeerDisabledResponseRemainsAnExplicitUpstreamFailure(t *testing.T) {
	configureBillingTest(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/api/local/sessions" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "billing is disabled on this machine"})
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	uiPort, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	pc := PC{ID: "disabled-peer", Name: "Disabled PC", Address: "http://198.51.100.44:18081", UIPort: uiPort}
	backend, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	timerHTTP = &http.Client{Timeout: time.Second, Transport: billingPeerRoundTripper(func(request *http.Request) (*http.Response, error) {
		forwarded := request.Clone(request.Context())
		forwarded.URL.Scheme, forwarded.URL.Host, forwarded.Host = backend.Scheme, backend.Host, backend.Host
		return http.DefaultTransport.RoundTrip(forwarded)
	})}
	err = callBillingPeer(pc, http.MethodPost, "/internal/api/local/sessions", StartSessionRequest{PCID: pc.ID}, nil)
	if err == nil || !strings.Contains(err.Error(), "returned HTTP 503") || !strings.Contains(err.Error(), "billing is disabled") {
		t.Fatalf("disabled billing was not returned as a meaningful peer error: %v", err)
	}
	var httpErr *billingHTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusBadGateway {
		t.Fatalf("disabled peer response should surface as HTTP 502, got %v", err)
	}
}

func TestBillingPeerContextTimeoutIsDistinctFromHTTPNotFound(t *testing.T) {
	configureBillingTest(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	uiPort, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	pc := PC{ID: "slow-peer", Name: "Slow PC", Address: server.URL, UIPort: uiPort}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err = callBillingPeerContext(ctx, pc, http.MethodGet, "/internal/api/billing/sync/pending", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") || strings.Contains(err.Error(), "returned HTTP 404") {
		t.Fatalf("peer timeout was not distinguished from a route 404: %v", err)
	}
	var httpErr *billingHTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusBadGateway {
		t.Fatalf("peer timeout should surface as HTTP 502, got %v", err)
	}
}

func TestPeerErrorBodyLoggingOmitsUnrecognizedResponseContent(t *testing.T) {
	for _, test := range []struct {
		body, want string
	}{
		{body: "404 page not found", want: "404 page not found"},
		{body: "customer name or credential must not be logged", want: "<response body omitted>"},
		{body: "", want: "<empty>"},
	} {
		if got := safePeerErrorBody(test.body); got != test.want {
			t.Errorf("safePeerErrorBody(%q) = %q, want %q", test.body, got, test.want)
		}
	}
}

func TestLocalBillingSyncAcknowledgesOnlyTheExportedVersion(t *testing.T) {
	configureBillingTest(t)
	startedAt := time.Date(2026, time.February, 1, 21, 0, 0, 0, billingTimezone)
	session, err := billingStore.commitStart(Session{
		ID: "S-local-sync", PCID: "pc-local", PCName: "Local PC",
		PCAddress: "http://127.0.0.1:18081", CustomerName: "Local Customer",
		Players: 1, Status: "open", PurchasedMinutes: 30, FinalAmount: 30,
		AmountPaid: 30, PaymentStatus: "paid",
	}, startedAt)
	if err != nil {
		t.Fatal(err)
	}
	exported, err := billingStore.pendingSync()
	if err != nil {
		t.Fatal(err)
	}
	if exported.DeviceID == "" || len(exported.Sessions) != 1 || exported.Sessions[0].DeviceID != exported.DeviceID {
		t.Fatalf("local session was not assigned a stable device identity: %+v", exported)
	}
	wrongAck := BillingSyncAck{
		DeviceID: exported.DeviceID,
		Sessions: []BillingSyncAckRecord{{ID: session.ID, UpdatedAt: "2026-02-01T21:01:00+05:30"}},
	}
	if acknowledged, err := billingStore.acknowledgeSync(wrongAck); err != nil || acknowledged != 0 {
		t.Fatal(err)
	}
	stillPending, err := billingStore.pendingSync()
	if err != nil || len(stillPending.Sessions) != 1 {
		t.Fatalf("mismatched acknowledgement incorrectly cleared pending record: %+v, %v", stillPending, err)
	}
	ack := BillingSyncAck{
		DeviceID: exported.DeviceID,
		Sessions: []BillingSyncAckRecord{{ID: session.ID, UpdatedAt: exported.Sessions[0].UpdatedAt}},
	}
	if acknowledged, err := billingStore.acknowledgeSync(ack); err != nil || acknowledged != 1 {
		t.Fatal(err)
	}
	acknowledged, err := billingStore.pendingSync()
	if err != nil || len(acknowledged.Sessions) != 0 {
		t.Fatalf("matching acknowledgement did not clear pending state: %+v, %v", acknowledged, err)
	}
	localSessions, err := billingStore.sessions("", "")
	if err != nil || len(localSessions) != 1 || localSessions[0].ID != session.ID {
		t.Fatalf("sync acknowledgement removed the owning PC's local history: %+v, %v", localSessions, err)
	}
	if _, err := billingStore.update(session.ID, "close", 0, 0); err != nil {
		t.Fatal(err)
	}
	updated, err := billingStore.pendingSync()
	if err != nil || len(updated.Sessions) != 1 || updated.Sessions[0].Status != "closed" {
		t.Fatalf("session update was not marked pending for resynchronization: %+v, %v", updated, err)
	}
}

func TestDeviceBillingWorkbookAndIdentitySurviveStoreRestart(t *testing.T) {
	directory := t.TempDir()
	config := BillingConfig{Enabled: true, WorkbookPath: filepath.Join(directory, "billing.xlsx")}
	first := &BillingStore{}
	if err := first.open(config); err != nil {
		t.Fatal(err)
	}
	session, err := first.commitStart(Session{
		ID: "S-persistent", PCID: "pc-persistent", PCName: "Persistent PC",
		PCAddress: "http://192.0.2.5:18081", CustomerName: "Persisted Customer",
		Players: 1, Status: "open", PurchasedMinutes: 30, FinalAmount: 30,
		AmountPaid: 30, PaymentStatus: "paid",
	}, time.Date(2026, time.February, 1, 10, 0, 0, 0, billingTimezone))
	if err != nil {
		t.Fatal(err)
	}
	deviceID := first.deviceID
	first.close()

	restarted := &BillingStore{}
	if err := restarted.open(config); err != nil {
		t.Fatal(err)
	}
	defer restarted.close()
	sessions, err := restarted.sessions("", "")
	if err != nil {
		t.Fatal(err)
	}
	if restarted.deviceID != deviceID || len(sessions) != 1 ||
		sessions[0].ID != session.ID || sessions[0].CustomerName != "Persisted Customer" {
		t.Fatalf("local billing data or stable device ID did not survive restart: id=%q sessions=%+v", restarted.deviceID, sessions)
	}
}

func TestIndependentDeviceBillingStoresRetainHistoryOfflineAcrossRestarts(t *testing.T) {
	const deviceCount = 5
	deviceIDs := make(map[string]bool, deviceCount)
	for i := range deviceCount {
		directory := t.TempDir()
		config := BillingConfig{Enabled: true, WorkbookPath: filepath.Join(directory, billingWorkbookName)}
		store := &BillingStore{}
		if err := store.open(config); err != nil {
			t.Fatalf("open device %d billing store: %v", i+1, err)
		}
		startedAt := time.Date(2026, time.February, 1, 10+i, 15, 0, 0, billingTimezone)
		want := Session{
			ID: fmt.Sprintf("S-offline-%02d", i+1), PCID: fmt.Sprintf("timer-%02d", i+1),
			PCName: fmt.Sprintf("PC %d", i+1), PCAddress: fmt.Sprintf("http://192.0.2.%d:18081", i+1),
			Category: "PC Pro", CustomerName: fmt.Sprintf("Customer %d", i+1),
			Players: 2, Status: "open", PurchasedMinutes: 60, PricingSnapshot: "offline-test",
			EstimatedAmount: 120, FinalAmount: 120, AmountPaid: 120, PaymentStatus: "paid",
		}
		created, err := store.commitStart(want, startedAt)
		if err != nil {
			t.Fatalf("persist device %d session without a sync service: %v", i+1, err)
		}
		if created.DeviceID == "" || deviceIDs[created.DeviceID] {
			t.Fatalf("device %d has an empty or duplicate stable ID %q", i+1, created.DeviceID)
		}
		deviceIDs[created.DeviceID] = true
		store.close()

		restarted := &BillingStore{}
		if err := restarted.open(config); err != nil {
			t.Fatalf("reopen device %d billing store: %v", i+1, err)
		}
		sessions, err := restarted.sessions("", "")
		if err != nil {
			t.Fatalf("read device %d billing history after restart: %v", i+1, err)
		}
		if restarted.deviceID != created.DeviceID || len(sessions) != 1 {
			t.Fatalf("device %d lost its identity or local session after restart: device_id=%q sessions=%+v",
				i+1, restarted.deviceID, sessions)
		}
		got := sessions[0]
		if got.ID != want.ID || got.DeviceID != created.DeviceID || got.CustomerName != want.CustomerName ||
			got.PCID != want.PCID || got.Category != want.Category || got.Players != want.Players ||
			got.PurchasedMinutes != want.PurchasedMinutes || got.FinalAmount != want.FinalAmount ||
			got.AmountPaid != want.AmountPaid || got.PaymentStatus != want.PaymentStatus ||
			got.StartedAt != created.StartedAt || got.EndedAt != "" || got.CreatedAt != created.CreatedAt {
			t.Fatalf("device %d did not retain its complete local session record: %+v", i+1, got)
		}
		restarted.close()
	}
}

func TestDailyReportUsesAsiaKolkataSessionStartDateAndFlagsIncompleteSync(t *testing.T) {
	configureBillingTest(t)
	hub := BillingStore{
		enabled:   true,
		path:      filepath.Join(t.TempDir(), "hub.xlsx"),
		backupDir: filepath.Join(t.TempDir(), "hub-backups"),
	}
	session := Session{
		ID: "S-midnight", DeviceID: "PC-01", PCID: "pc-01", PCName: "PC 1",
		PCAddress: "http://192.0.2.1:18081", CustomerName: "Midnight Customer",
		Players: 1, Status: "closed", StartedAt: "2026-02-01T20:00:00Z",
		CreatedAt: "2026-02-01T20:00:00Z", UpdatedAt: "2026-02-01T20:30:00Z",
		PurchasedMinutes: 30, FinalAmount: 120, AmountPaid: 100,
	}
	if err := hub.mergeSyncedSessions(session.DeviceID, []Session{session}); err != nil {
		t.Fatal(err)
	}
	unpaidRepeatCustomer := Session{
		ID: "S-midnight-repeat", DeviceID: "PC-01", PCID: "pc-02", PCName: "PC 2",
		PCAddress: "http://192.0.2.1:18081", CustomerName: " midnight customer ",
		Players: 1, Status: "closed", StartedAt: "2026-02-02T10:00:00+05:30",
		CreatedAt: "2026-02-02T10:00:00+05:30", UpdatedAt: "2026-02-02T10:30:00+05:30",
		EndedAt: "2026-02-02T10:30:00+05:30", PurchasedMinutes: 30,
		FinalAmount: 100, AmountPaid: 0, PaymentStatus: "unpaid",
	}
	if err := hub.mergeSyncedSessions(unpaidRepeatCustomer.DeviceID, []Session{unpaidRepeatCustomer, unpaidRepeatCustomer}); err != nil {
		t.Fatal(err)
	}
	if err := hub.replaceSyncStatuses([]BillingDeviceStatus{
		{DeviceID: "hub", Name: "Hub", Endpoint: "http://192.0.2.10:18082", Online: true},
		{DeviceID: session.DeviceID, Name: "PC 1", Endpoint: "http://192.0.2.1:18082", Online: true},
	}); err != nil {
		t.Fatal(err)
	}
	report, err := hub.report("2026-02-02")
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete || report.SystemCount != 2 || report.SessionCount != 2 ||
		report.UniqueCustomerCount != 1 || report.CustomerCount != 2 ||
		report.BilledAmount != 220 || report.CollectedAmount != 100 ||
		report.Revenue != 100 || report.Outstanding != 120 || len(report.Records) != 2 {
		t.Fatalf("Asia/Kolkata report has incorrect totals: %+v", report)
	}
	if len(report.PCs) != 2 || report.PCs[1].SessionCount != 1 ||
		report.PCs[1].BilledAmount != 100 || report.PCs[1].CollectedAmount != 0 ||
		report.PCs[1].Outstanding != 100 {
		t.Fatalf("unpaid record was counted as collected or omitted from PC totals: %+v", report.PCs)
	}
	priorDate, err := hub.report("2026-02-01")
	if err != nil {
		t.Fatal(err)
	}
	if priorDate.SessionCount != 0 || priorDate.UniqueCustomerCount != 0 || priorDate.BilledAmount != 0 {
		t.Fatalf("UTC timestamp was assigned to the wrong local session date: %+v", priorDate)
	}
	if err := hub.replaceSyncStatuses([]BillingDeviceStatus{
		{DeviceID: "hub", Name: "Hub", Endpoint: "http://192.0.2.10:18082", Online: true},
		{DeviceID: session.DeviceID, Name: "PC 1", Endpoint: "http://192.0.2.1:18082", Online: false, PendingRecords: 1, Error: "offline"},
	}); err != nil {
		t.Fatal(err)
	}
	incomplete, err := hub.report("2026-02-02")
	if err != nil {
		t.Fatal(err)
	}
	if incomplete.Complete {
		t.Fatal("report should be incomplete while a registered device has pending/offline records")
	}
	if incomplete.SessionCount != 2 || incomplete.CollectedAmount != 100 {
		t.Fatalf("incomplete sync state should not discard previously synchronized history: %+v", incomplete)
	}
}

func TestDashboardRoutesRemoteBillingToTheOwningDevice(t *testing.T) {
	configureBillingTest(t)
	var startCount, extendCount, closeCount atomic.Int32
	const sessionID = "S-remote-owner"
	const deviceID = "PC-remote-owner"
	var pc PC
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(httpcheck.ParentRequestIDHeader) != "dashboard-request-42" ||
			r.Header.Get(httpcheck.RequestIDHeader) == "" {
			t.Errorf("remote request is not correlated with its dashboard request")
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/internal/api/local/sessions":
			var request StartSessionRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode remote start request: %v", err)
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			if request.PCID != pc.ID {
				t.Errorf("session was routed to the wrong PC: %+v", request)
			}
			startCount.Add(1)
			writeJSON(w, http.StatusCreated, Session{
				ID: sessionID, DeviceID: deviceID, PCID: pc.ID, PCName: pc.Name,
				PCAddress: pc.Address, CustomerName: request.CustomerName,
				Players: request.Players, Status: "open", StartedAt: "2026-02-01T10:00:00+05:30",
				CreatedAt: "2026-02-01T10:00:00+05:30", UpdatedAt: "2026-02-01T10:00:00+05:30",
				PurchasedMinutes: request.DurationMinutes, FinalAmount: 30, AmountPaid: 30,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/internal/api/local/sessions/extend":
			var request ExtendSessionRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode remote extension request: %v", err)
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			if request.SessionID != sessionID || request.PCID != pc.ID || request.PCAddress != pc.Address || request.Seconds != 900 {
				t.Errorf("extension was routed with the wrong session identity: %+v", request)
			}
			extendCount.Add(1)
			writeJSON(w, http.StatusOK, ExtendSessionResponse{
				SessionFound: true, RemainingSeconds: 1800,
				Session: &Session{
					ID: sessionID, DeviceID: deviceID, PCID: pc.ID, PCName: pc.Name,
					PCAddress: pc.Address, CustomerName: "Remote Customer",
					Status: "open", StartedAt: "2026-02-01T10:00:00+05:30",
					CreatedAt: "2026-02-01T10:00:00+05:30", UpdatedAt: "2026-02-01T10:15:00+05:30",
					PurchasedMinutes: 45, FinalAmount: 45, AmountPaid: 45,
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/internal/api/local/sessions/"+sessionID+"/close":
			var request struct {
				PCID      string `json:"pc_id"`
				PCAddress string `json:"pc_address"`
				DeviceID  string `json:"device_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode remote close request: %v", err)
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			if request.PCID != pc.ID || request.PCAddress != pc.Address || request.DeviceID != deviceID {
				t.Errorf("close was routed with the wrong session identity: %+v", request)
			}
			closeCount.Add(1)
			writeJSON(w, http.StatusOK, Session{
				ID: sessionID, DeviceID: deviceID, PCID: pc.ID, PCName: pc.Name,
				PCAddress: pc.Address, CustomerName: "Remote Customer",
				Status: "closed", StartedAt: "2026-02-01T10:00:00+05:30",
				CreatedAt: "2026-02-01T10:00:00+05:30", UpdatedAt: "2026-02-01T10:20:00+05:30",
				PurchasedMinutes: 45, FinalAmount: 45, AmountPaid: 45,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer remote.Close()
	remoteURL, err := url.Parse(remote.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(remoteURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	uiPort, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	pc = PC{ID: "remote-pc", Name: "Remote PC", Address: "http://127.0.0.1:18081", UIPort: uiPort}
	setBillingTestPCs(pc)
	ctx := httpcheck.WithRequestID(context.Background(), "dashboard-request-42")

	session, err := startBillingSessionForDashboardContext(ctx, StartSessionRequest{
		PCID: pc.ID, CustomerName: "Remote Customer", Players: 1,
		Category: "PC Lite", DurationMinutes: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if session.ID != sessionID || session.DeviceID != deviceID || startCount.Load() != 1 {
		t.Fatalf("remote session was not created by its owner: %+v", session)
	}
	extension, err := extendSessionForDashboardContext(ctx, ExtendSessionRequest{
		SessionID: sessionID, PCID: pc.ID, PCAddress: pc.Address,
		Seconds: 900, RequestID: "remote-extension",
	})
	if err != nil {
		t.Fatal(err)
	}
	if extension.Session == nil || extension.Session.PurchasedMinutes != 45 || extendCount.Load() != 1 {
		t.Fatalf("remote extension did not update the owning session: %+v", extension)
	}
	closed, err := updateSessionForDashboardContext(ctx, sessionID, "close", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if closed.Status != "closed" || closeCount.Load() != 1 {
		t.Fatalf("remote close was not routed to the owning PC: %+v", closed)
	}
	sessions, err := billingStore.sessions("", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Status != "closed" || sessions[0].PurchasedMinutes != 45 {
		t.Fatalf("hub did not retain exactly one latest synchronized session: %+v", sessions)
	}
}

type billingPeerRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip billingPeerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestDashboardRoutesFullBillingLifecycleToFourOwningDevices(t *testing.T) {
	configureBillingTest(t)
	const deviceCount = 4
	servers := make([]*httptest.Server, 0, deviceCount)
	pcs := make([]PC, 0, deviceCount)
	peerSessions := make([]map[string]Session, deviceCount)
	startCalls := make([]int, deviceCount)
	extendCalls := make([]int, deviceCount)
	payCalls := make([]int, deviceCount)
	closeCalls := make([]int, deviceCount)
	routes := make(map[string]string, deviceCount)
	for index := 0; index < deviceCount; index++ {
		peerIndex := index
		peerSessions[index] = make(map[string]Session)
		var pc PC
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/internal/api/local/sessions":
				var request StartSessionRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("PC %d start decode: %v", peerIndex+1, err)
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				if request.PCID != pc.ID {
					t.Errorf("PC %d received another system's start: %+v", peerIndex+1, request)
					http.Error(w, "wrong owner", http.StatusConflict)
					return
				}
				for _, current := range peerSessions[peerIndex] {
					if current.PCID == pc.ID && current.Status == "open" {
						writeJSON(w, http.StatusConflict, map[string]string{"error": "system already has an open session"})
						return
					}
				}
				startCalls[peerIndex]++
				now := time.Now().In(billingTimezone).Format(time.RFC3339Nano)
				session := Session{
					ID: fmt.Sprintf("S-four-pc-%d", peerIndex+1), DeviceID: fmt.Sprintf("device-four-pc-%d", peerIndex+1),
					PCID: pc.ID, PCName: pc.Name, PCAddress: pc.Address, Category: request.Category,
					CustomerName: request.CustomerName, Players: request.Players, Status: "open",
					StartedAt: now, ExpectedEndAt: now, CreatedAt: now, UpdatedAt: now,
					PurchasedMinutes: request.DurationMinutes, FinalAmount: 30, PaymentStatus: "unpaid",
				}
				peerSessions[peerIndex][session.ID] = session
				writeJSON(w, http.StatusCreated, session)
			case r.Method == http.MethodPost && r.URL.Path == "/internal/api/local/sessions/extend":
				var request ExtendSessionRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("PC %d extension decode: %v", peerIndex+1, err)
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				session, ok := peerSessions[peerIndex][request.SessionID]
				if !ok || request.PCID != pc.ID || request.PCAddress != pc.Address || request.Seconds != 900 {
					t.Errorf("PC %d received mismatched extension identity: %+v", peerIndex+1, request)
					http.Error(w, "wrong owner/session", http.StatusConflict)
					return
				}
				extendCalls[peerIndex]++
				session.PurchasedMinutes += 15
				session.FinalAmount += 15
				session.AmountPaid += 15
				session.PaymentStatus = "partial"
				session.UpdatedAt = time.Now().In(billingTimezone).Format(time.RFC3339Nano)
				peerSessions[peerIndex][session.ID] = session
				writeJSON(w, http.StatusOK, ExtendSessionResponse{SessionFound: true, RemainingSeconds: 1200, Session: &session})
			case strings.HasPrefix(r.URL.Path, "/internal/api/local/sessions/") && r.Method == http.MethodPost:
				parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/internal/api/local/sessions/"), "/"), "/")
				if len(parts) != 2 {
					http.NotFound(w, r)
					return
				}
				var request struct {
					PCID      string `json:"pc_id"`
					PCAddress string `json:"pc_address"`
					DeviceID  string `json:"device_id"`
					Minutes   int    `json:"minutes"`
					Amount    int    `json:"amount"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("PC %d update decode: %v", peerIndex+1, err)
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				session, ok := peerSessions[peerIndex][parts[0]]
				if !ok || request.PCID != pc.ID || request.PCAddress != pc.Address || request.DeviceID != session.DeviceID {
					t.Errorf("PC %d received mismatched update identity: %+v", peerIndex+1, request)
					http.Error(w, "wrong owner/session", http.StatusConflict)
					return
				}
				session.UpdatedAt = time.Now().In(billingTimezone).Format(time.RFC3339Nano)
				switch parts[1] {
				case "pay":
					payCalls[peerIndex]++
					amount := request.Amount
					if amount <= 0 {
						amount = session.FinalAmount - session.AmountPaid
					}
					if amount <= 0 {
						writeJSON(w, http.StatusConflict, map[string]string{"error": "no amount is due"})
						return
					}
					session.AmountPaid += amount
					if session.AmountPaid >= session.FinalAmount {
						session.PaymentStatus = "paid"
					} else {
						session.PaymentStatus = "partial"
					}
				case "close":
					if session.Status != "open" {
						writeJSON(w, http.StatusConflict, map[string]string{"error": "session is already closed"})
						return
					}
					closeCalls[peerIndex]++
					session.Status = "closed"
					session.EndedAt = session.UpdatedAt
				default:
					http.NotFound(w, r)
					return
				}
				peerSessions[peerIndex][session.ID] = session
				writeJSON(w, http.StatusOK, session)
			default:
				http.NotFound(w, r)
			}
		}))
		servers = append(servers, server)
		parsed, err := url.Parse(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		_, portText, err := net.SplitHostPort(parsed.Host)
		if err != nil {
			t.Fatal(err)
		}
		uiPort, err := strconv.Atoi(portText)
		if err != nil {
			t.Fatal(err)
		}
		pc = PC{
			ID: fmt.Sprintf("registry-four-pc-%d", index+1), Name: fmt.Sprintf("PC %d", index+1),
			Address: fmt.Sprintf("http://198.51.100.%d:18081", index+1), UIPort: uiPort,
		}
		pcs = append(pcs, pc)
		routes[fmt.Sprintf("198.51.100.%d", index+1)] = server.URL
	}
	t.Cleanup(func() {
		for _, server := range servers {
			server.Close()
		}
	})
	setBillingTestPCs(pcs...)
	if _, err := startBillingSessionForDashboardContext(context.Background(), StartSessionRequest{
		PCID: "unregistered-pc", Category: "PC Lite", CustomerName: "No Device", Players: 1, DurationMinutes: 30,
	}); err == nil {
		t.Fatal("an invalid PC ID unexpectedly reached a peer")
	}
	timerHTTP = &http.Client{Timeout: 4 * time.Second, Transport: billingPeerRoundTripper(func(request *http.Request) (*http.Response, error) {
		backend, ok := routes[request.URL.Hostname()]
		if !ok {
			return nil, fmt.Errorf("no mock registered for %s", request.URL.Hostname())
		}
		backendURL, err := url.Parse(backend)
		if err != nil {
			return nil, err
		}
		forwarded := request.Clone(request.Context())
		forwarded.URL.Scheme, forwarded.URL.Host = backendURL.Scheme, backendURL.Host
		forwarded.Host = backendURL.Host
		return http.DefaultTransport.RoundTrip(forwarded)
	})}
	mux := http.NewServeMux()
	registerBillingHandlers(mux)

	for index, pc := range pcs {
		startBody, _ := json.Marshal(StartSessionRequest{
			PCID: pc.ID, Category: "PC Lite", CustomerName: fmt.Sprintf("Customer %d", index+1),
			Players: 1, DurationMinutes: 30,
		})
		started := httptest.NewRecorder()
		mux.ServeHTTP(started, httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(startBody)))
		if started.Code != http.StatusCreated {
			t.Fatalf("PC %d start returned %d: %s", index+1, started.Code, started.Body.String())
		}
		var session Session
		if err := json.Unmarshal(started.Body.Bytes(), &session); err != nil {
			t.Fatal(err)
		}
		if session.PCID != pc.ID || session.PCAddress != pc.Address || session.DeviceID != fmt.Sprintf("device-four-pc-%d", index+1) {
			t.Fatalf("PC %d session identity was not preserved: %+v", index+1, session)
		}
		extensionBody, _ := json.Marshal(ExtendSessionRequest{
			SessionID: session.ID, PCID: pc.ID, PCAddress: pc.Address, Seconds: 900,
			RequestID: fmt.Sprintf("extension-four-pc-%d", index+1),
		})
		extended := httptest.NewRecorder()
		mux.ServeHTTP(extended, httptest.NewRequest(http.MethodPost, "/api/sessions/extend", bytes.NewReader(extensionBody)))
		if extended.Code != http.StatusOK {
			t.Fatalf("PC %d extension returned %d: %s", index+1, extended.Code, extended.Body.String())
		}
		payBody := []byte(`{"amount":0}`)
		paid := httptest.NewRecorder()
		mux.ServeHTTP(paid, httptest.NewRequest(http.MethodPost, "/api/sessions/"+session.ID+"/pay", bytes.NewReader(payBody)))
		if paid.Code != http.StatusOK {
			t.Fatalf("PC %d payment returned %d: %s", index+1, paid.Code, paid.Body.String())
		}
		closed := httptest.NewRecorder()
		mux.ServeHTTP(closed, httptest.NewRequest(http.MethodPost, "/api/sessions/"+session.ID+"/close", bytes.NewReader([]byte(`{}`))))
		if closed.Code != http.StatusOK {
			t.Fatalf("PC %d close returned %d: %s", index+1, closed.Code, closed.Body.String())
		}
		duplicate := httptest.NewRecorder()
		mux.ServeHTTP(duplicate, httptest.NewRequest(http.MethodPost, "/api/sessions/"+session.ID+"/close", bytes.NewReader([]byte(`{}`))))
		if duplicate.Code != http.StatusBadGateway {
			t.Fatalf("PC %d duplicate close returned %d; want upstream conflict surfaced as 502", index+1, duplicate.Code)
		}
	}
	stored, err := billingStore.sessions("", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != deviceCount {
		t.Fatalf("central workbook has %d sessions, want %d", len(stored), deviceCount)
	}
	for index, session := range stored {
		if session.Status != "closed" || session.PaymentStatus != "paid" || session.AmountPaid != session.FinalAmount ||
			session.PurchasedMinutes != 45 || session.CustomerName != fmt.Sprintf("Customer %d", index+1) {
			t.Errorf("PC %d lifecycle was not retained accurately: %+v", index+1, session)
		}
		if startCalls[index] != 1 || extendCalls[index] != 1 || payCalls[index] != 1 || closeCalls[index] != 1 {
			t.Errorf("PC %d received duplicate or missing operations: start=%d extend=%d pay=%d close=%d",
				index+1, startCalls[index], extendCalls[index], payCalls[index], closeCalls[index])
		}
	}
}
