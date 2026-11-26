//go:build windows

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"overlay-timer/internal/httpcheck"
)

const (
	billingSyncStateName = "billing-sync-status.json"
	billingSyncInterval  = 30 * time.Second
	billingTimezoneName  = "Asia/Kolkata"
	noPCSnackPCID        = "__no_pc_snack__"
)

var billingTimezone = time.FixedZone(billingTimezoneName, 5*60*60+30*60)

type BillingConfig struct {
	Enabled   bool   `json:"enabled"`
	Directory string `json:"directory,omitempty"`
}
type Session struct {
	ID                    string         `json:"id"`
	DeviceID              string         `json:"device_id,omitempty"`
	SyncedUpdatedAt       string         `json:"sync_updated_at,omitempty"`
	PCID                  string         `json:"pc_id"`
	PCName                string         `json:"pc_name"`
	PCAddress             string         `json:"pc_address"`
	Category              string         `json:"category"`
	CustomerName          string         `json:"customer_name"`
	Players               int            `json:"players"`
	Status                string         `json:"status"`
	StartedAt             string         `json:"started_at"`
	ExpectedEndAt         string         `json:"expected_end_at"`
	EndedAt               string         `json:"ended_at,omitempty"`
	PurchasedMinutes      int            `json:"purchased_minutes"`
	AdditionalMinutes     int            `json:"additional_minutes"`
	TimeCorrected         bool           `json:"time_corrected,omitempty"`
	ActualDurationSeconds int            `json:"actual_duration_seconds"`
	PricingSnapshot       string         `json:"pricing_snapshot"`
	ManualPrice           *int           `json:"manual_price,omitempty"`
	EstimatedAmount       int            `json:"estimated_amount"`
	Snacks                int            `json:"snacks"`
	FinalAmount           int            `json:"final_amount"`
	AmountPaid            int            `json:"amount_paid"`
	PaymentStatus         string         `json:"payment_status"`
	PaymentAt             string         `json:"payment_at,omitempty"`
	CreatedAt             string         `json:"created_at"`
	UpdatedAt             string         `json:"updated_at"`
	Continued             *continuedBill `json:"continued,omitempty"`
	ResumeMarker          *paymentRecord `json:"resume_marker,omitempty"`
	payments              []paymentRecord
}

func isStandaloneSnackSale(session Session) bool {
	return strings.EqualFold(session.PCID, noPCSnackPCID) && session.Snacks > 0
}

type continuedBill struct {
	Amount       int  `json:"amount"`
	SpentSeconds int  `json:"spent_seconds"`
	PriorMinutes int  `json:"prior_minutes"`
	PriorSeconds *int `json:"prior_seconds,omitempty"`
}
type billingSessionLink struct {
	Owner           string `json:"owner,omitempty"`
	SessionID       string `json:"session_id"`
	PCAddress       string `json:"pc_address"`
	ManagerEndpoint string `json:"manager_endpoint"`
}
type paymentRecord struct {
	ID               string `json:"id"`
	Timestamp        string `json:"timestamp"`
	Amount           int    `json:"amount"`
	Status           string `json:"status"`
	ExtensionMinutes int    `json:"extension_minutes,omitempty"`
	RemainingSeconds int    `json:"remaining_seconds,omitempty"`
}

var billingSessionLinks = struct {
	sync.RWMutex
	items map[string]billingSessionLink
}{items: make(map[string]billingSessionLink)}

func billingSessionLinkForEndpoint(address string) (billingSessionLink, bool) {
	billingSessionLinks.RLock()
	defer billingSessionLinks.RUnlock()
	link, ok := billingSessionLinks.items[normalizeSessionAddress(address)]
	return link, ok
}

func billingManagerEndpoint() (string, error) {
	manager := localRegistryPC()
	endpoint, err := url.Parse(manager.Address)
	if err != nil {
		return "", err
	}
	if endpoint.Hostname() == "" || appConfig.UIDiscoveryPort < 1 {
		return "", errors.New("billing manager endpoint is not configured")
	}
	return "http://" + net.JoinHostPort(endpoint.Hostname(), strconv.Itoa(appConfig.UIDiscoveryPort)) + "/internal/api/sessions/extend", nil
}

func setBillingSessionLinkHandler(mux *http.ServeMux, path, pcAddress string) {
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodDelete {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST or DELETE required"})
			return
		}
		var link billingSessionLink
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&link); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid billing session link"})
			return
		}
		key := normalizeSessionAddress(pcAddress)
		sameTimer := key != "" && normalizeSessionAddress(link.PCAddress) == key
		if link.Owner != "" {
			sameTimer = strings.EqualFold(link.Owner, configuredFriendlyName()) && billingPath(link.PCAddress) == billingPath(pcAddress) && billingPath(pcAddress) != "!invalid"
		}
		if !sameTimer || link.SessionID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "billing session link does not match this timer endpoint"})
			return
		}
		endpoint, err := url.Parse(link.ManagerEndpoint)
		if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") ||
			endpoint.Host == "" || endpoint.User != nil || endpoint.Path != "/internal/api/sessions/extend" ||
			endpoint.RawQuery != "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid billing manager endpoint"})
			return
		}
		if r.Method == http.MethodPost {
			billingSessionLinks.Lock()
			billingSessionLinks.items[key] = link
			billingSessionLinks.Unlock()
		} else {
			billingSessionLinks.Lock()
			if current, exists := billingSessionLinks.items[key]; exists && current.SessionID == link.SessionID {
				delete(billingSessionLinks.items, key)
			}
			billingSessionLinks.Unlock()
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "success"})
	})
}

func updateBillingSessionLink(pc PC, sessionID string, remove bool) error {
	managerEndpoint, err := billingManagerEndpoint()
	if err != nil {
		return fmt.Errorf("could not resolve billing manager endpoint: %w", err)
	}
	link := billingSessionLink{Owner: pc.Owner, SessionID: sessionID, PCAddress: pc.Address, ManagerEndpoint: managerEndpoint}
	payload, err := json.Marshal(link)
	if err != nil {
		return err
	}
	method := http.MethodPost
	if remove {
		method = http.MethodDelete
	}
	request, err := http.NewRequest(method, timerEndpoint(pc.Address, "billing-session"), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := monitoredHTTPDo(request, "billing-session-link", pc.ID)
	if err != nil {
		return fmt.Errorf("could not configure billing synchronization on %s: %w", pc.Name, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("timer endpoint returned HTTP %d while configuring billing synchronization: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

type BillingSummary struct {
	Enabled             bool   `json:"enabled"`
	TodayCollected      int    `json:"today_collected"`
	OpenSessions        int    `json:"open_sessions"`
	ConfigurationStatus string `json:"configuration_status"`
}

var billingNodeInstance = rand.Text()

type BillingSyncExport struct {
	Instance string    `json:"instance,omitempty"`
	DeviceID string    `json:"device_id"`
	Name     string    `json:"name"`
	Sessions []Session `json:"sessions"`
}
type BillingSyncAck struct {
	DeviceID string                 `json:"device_id"`
	Sessions []BillingSyncAckRecord `json:"sessions"`
}
type BillingSyncAckRecord struct {
	ID        string `json:"id"`
	UpdatedAt string `json:"updated_at"`
}
type BillingSyncAckResult struct {
	Acknowledged int `json:"acknowledged"`
}
type BillingDeviceStatus struct {
	Instance         string   `json:"instance,omitempty"`
	Aliases          []string `json:"aliases,omitempty"`
	DeviceID         string   `json:"device_id"`
	Name             string   `json:"name"`
	Endpoint         string   `json:"endpoint"`
	Online           bool     `json:"online"`
	PendingRecords   int      `json:"pending_records"`
	LastAttemptAt    string   `json:"last_attempt_at,omitempty"`
	LastSuccessfulAt string   `json:"last_successful_sync_at,omitempty"`
	Error            string   `json:"error,omitempty"`
}
type BillingReport struct {
	Date                string                `json:"date"`
	StartDate           string                `json:"start_date"`
	EndDate             string                `json:"end_date"`
	Timezone            string                `json:"timezone"`
	Complete            bool                  `json:"complete"`
	SystemCount         int                   `json:"system_count"`
	SessionCount        int                   `json:"session_count"`
	UnassignedSessions  int                   `json:"unassigned_sessions"`
	UniqueCustomerCount int                   `json:"unique_customer_count"`
	CustomerCount       int                   `json:"customer_count"`
	BilledAmount        int                   `json:"billed_amount"`
	GameAmount          int                   `json:"game_amount"`
	SnackAmount         int                   `json:"snack_amount"`
	CollectedAmount     int                   `json:"collected_amount"`
	Revenue             int                   `json:"revenue"`
	Outstanding         int                   `json:"outstanding"`
	PCs                 []BillingReportPC     `json:"pcs"`
	Records             []Session             `json:"records"`
	Devices             []BillingDeviceStatus `json:"devices"`
}
type BillingReportPC struct {
	DeviceID        string `json:"device_id,omitempty"`
	PCID            string `json:"pc_id"`
	PCName          string `json:"pc_name"`
	SessionCount    int    `json:"session_count"`
	CustomerCount   int    `json:"customer_count"`
	BilledAmount    int    `json:"billed_amount"`
	GameAmount      int    `json:"game_amount"`
	SnackAmount     int    `json:"snack_amount"`
	CollectedAmount int    `json:"collected_amount"`
	Revenue         int    `json:"revenue"`
	Outstanding     int    `json:"outstanding"`
}
type StartSessionRequest struct {
	AllowIncomplete bool           `json:"allow_incomplete,omitempty"`
	DurationSeconds int            `json:"duration_seconds,omitempty"`
	Target          *billingTarget `json:"target,omitempty"`
	PCID            string         `json:"pc_id"`
	Category        string         `json:"category"`
	CustomerName    string         `json:"customer_name"`
	Players         int            `json:"players"`
	DurationMinutes int            `json:"duration_minutes"`
	StartedAt       string         `json:"started_at"`
	ManualPrice     *int           `json:"manual_price,omitempty"`
}
type BillingStore struct {
	mu       sync.Mutex
	enabled  bool
	path     string
	deviceID string

	syncStatuses []BillingDeviceStatus
}

var billingStore BillingStore
var csvSessionFields = []string{"Session ID", "System ID", "System", "Address", "Category", "Customer", "Players", "Status", "Start", "Expected End", "End", "Purchased Minutes", "Additional Minutes", "Pricing Snapshot", "Estimated Amount", "Final Amount", "Amount Paid", "Payment Status", "Payment At", "Created At", "Updated At"}

func (s *BillingStore) open(c BillingConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enabled = c.Enabled
	if !s.enabled {
		return nil
	}
	s.path = c.Directory
	if s.path == "" {
		s.path = appDataPath("billing")
	} else if !filepath.IsAbs(s.path) {
		s.path = appDataPath(s.path)
	}
	s.deviceID = configuredFriendlyName()
	if err := os.MkdirAll(s.path, 0755); err != nil {
		return err
	}
	return s.loadSyncStatusesLocked()
}
func sessionTimestamp(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	if parsed, err := time.ParseInLocation("2006-01-02 15:04:05", value, billingTimezone); err == nil {
		return parsed, nil
	}
	return time.Time{}, fmt.Errorf("invalid session timestamp %q", value)
}

func (s Session) plannedSeconds(start time.Time) int {
	if expected, err := sessionTimestamp(s.ExpectedEndAt); err == nil && !start.IsZero() && !expected.Before(start) {
		return int(expected.Sub(start).Seconds())
	}
	return max(0, (s.PurchasedMinutes+s.AdditionalMinutes)*60)
}

func (s Session) gameplayLimitSeconds(start time.Time) int {
	planned := s.plannedSeconds(start)
	if s.Continued == nil {
		return planned
	}
	prior := s.Continued.PriorMinutes * 60
	if s.Continued.PriorSeconds != nil {
		prior = *s.Continued.PriorSeconds
	}
	return s.Continued.SpentSeconds + max(0, planned-prior)
}

func parseSession(r []string) Session {
	get := func(i int) string {
		if i < len(r) {
			return r[i]
		}
		return ""
	}
	num := func(i int) int { v, _ := strconv.Atoi(get(i)); return v }
	return Session{ID: get(0), PCID: get(1), PCName: get(2), PCAddress: get(3), Category: get(4), CustomerName: get(5), Players: num(6), Status: get(7), StartedAt: get(8), ExpectedEndAt: get(9), EndedAt: get(10), PurchasedMinutes: num(11), AdditionalMinutes: num(12), PricingSnapshot: get(13), EstimatedAmount: num(14), FinalAmount: num(15), AmountPaid: num(16), PaymentStatus: get(17), PaymentAt: get(18), CreatedAt: get(19), UpdatedAt: get(20)}
}
func rowFor(s Session) []string {
	return []string{s.ID, s.PCID, s.PCName, s.PCAddress, s.Category, s.CustomerName, strconv.Itoa(s.Players), s.Status, s.StartedAt, s.ExpectedEndAt, s.EndedAt, strconv.Itoa(s.PurchasedMinutes), strconv.Itoa(s.AdditionalMinutes), s.PricingSnapshot, strconv.Itoa(s.EstimatedAmount), strconv.Itoa(s.FinalAmount), strconv.Itoa(s.AmountPaid), s.PaymentStatus, s.PaymentAt, s.CreatedAt, s.UpdatedAt}
}
func (s *BillingStore) require() error {
	if !s.enabled {
		return errors.New("billing is disabled on this machine")
	}
	return nil
}
func (s *BillingStore) sessions(search, payment string, statusFilter ...string) ([]Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.require(); err != nil {
		return nil, err
	}
	f, err := s.openLocked()
	if err != nil {
		return nil, err
	}

	rows := f.sessions
	var out []Session
	search = strings.ToLower(strings.TrimSpace(search))
	status := ""
	if len(statusFilter) > 0 {
		status = statusFilter[0]
	}
	for _, x := range rows {
		if x.Status == "removed" || status != "" && x.Status != status {
			continue
		}
		if payment != "" && x.PaymentStatus != payment {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(x.CustomerName+" "+x.PCName+" "+x.ID), search) {
			continue
		}
		out = append(out, x)
	}
	return out, nil
}

func (s *BillingStore) customerNames() ([]string, error) {
	sessions, err := s.sessions("", "")
	if err != nil {
		return nil, err
	}
	names := make(map[string]string)
	for _, session := range sessions {
		name := strings.TrimSpace(session.CustomerName)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, exists := names[key]; !exists {
			names[key] = name
		}
	}
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result, nil
}

func (s *BillingStore) prepareStart(q StartSessionRequest) (Session, PC, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.require(); err != nil {
		return Session{}, PC{}, err
	}
	pc, ok := store.find(q.PCID)
	if q.Target != nil {
		pc, ok = ownedBillingTimer(q.Target)
	}
	if !ok {
		return Session{}, PC{}, errors.New("gaming system not found")
	}
	if pc.Inactive {
		return Session{}, PC{}, errors.New("gaming system is inactive")
	}
	q.CustomerName = strings.TrimSpace(q.CustomerName)
	if q.CustomerName == "" && !q.AllowIncomplete {
		return Session{}, PC{}, errors.New("Please enter the customer name before starting the session.")
	}
	if len(q.CustomerName) > 120 {
		return Session{}, PC{}, errors.New("customer name is too long")
	}
	if q.Players < 0 || (!q.AllowIncomplete && q.Players == 0) || q.Players > 4 {
		return Session{}, PC{}, errors.New("players must be between 1 and 4")
	}
	if q.DurationSeconds != 0 && (!q.AllowIncomplete || q.DurationSeconds < 1 || q.DurationSeconds > 86400 || (q.DurationSeconds+59)/60 != q.DurationMinutes) {
		return Session{}, PC{}, errors.New("invalid timer duration")
	}
	var entry PriceEntry
	var rates []PriceEntry
	var err error
	if q.AllowIncomplete {
		entry, rates, err = cardPrice(pc, q.Category, q.Players, q.DurationMinutes)
	} else {
		rates = pricingCatalog().Entries
		entry, err = priceAt(rates, q.Category, q.Players, q.DurationMinutes)
		if err == nil && !categoryAllowed(pc, entry.Category, entry.Group) {
			err = errors.New("category does not belong to this PC pricing group")
		}
	}
	if err != nil {
		return Session{}, PC{}, err
	}
	if q.ManualPrice != nil && (*q.ManualPrice < 0 || *q.ManualPrice > 9999999) {
		return Session{}, PC{}, errors.New("manual price must be between 0 and 9999999 INR")
	}
	f, err := s.openLocked()
	if err != nil {
		return Session{}, PC{}, apiError(http.StatusInternalServerError, err)
	}

	rows := f.sessions
	for _, x := range rows {
		if sessionAtPC(x, pc) && x.Status == "open" {
			return Session{}, PC{}, apiError(http.StatusConflict, errors.New("system already has an open session"))
		}
	}
	sessionID, err := newSessionID()
	if err != nil {
		return Session{}, PC{}, fmt.Errorf("could not generate a session ID: %w", err)
	}
	finalAmount := entry.Amount
	priceSnapshot := pricingSnapshot(entry, rates)
	if q.ManualPrice != nil {
		finalAmount = *q.ManualPrice
		if !q.AllowIncomplete {
			priceSnapshot += fmt.Sprintf("; manual override ₹%d", *q.ManualPrice)
		}
	}
	x := Session{
		ID: sessionID, PCID: pc.ID,
		PCName: pc.Name, PCAddress: pc.Address, Category: q.Category,
		CustomerName: q.CustomerName, Players: q.Players, Status: "open",
		PurchasedMinutes: q.DurationMinutes, PricingSnapshot: priceSnapshot, ManualPrice: q.ManualPrice,
		EstimatedAmount: entry.Amount, FinalAmount: finalAmount, AmountPaid: finalAmount,
		PaymentStatus: "paid",
	}
	if q.AllowIncomplete {
		x.AmountPaid = 0
		x.PaymentStatus = "unpaid"
		if len(rates) == 0 {
			x.PricingSnapshot = ""
		}
	}
	return x, pc, nil
}

func newSessionID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return "S-" + hex.EncodeToString(id[:]), nil
}

func (s *BillingStore) commitStart(x Session, startedAt time.Time, durationSeconds ...int) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.require(); err != nil {
		return Session{}, err
	}
	f, err := s.openLocked()
	if err != nil {
		return Session{}, apiError(http.StatusInternalServerError, err)
	}

	sessions := f.sessions
	for _, current := range sessions {
		if current.Status == "open" && current.PCID == x.PCID && (current.DeviceID == "" || strings.EqualFold(current.DeviceID, s.deviceID)) {
			return Session{}, apiError(http.StatusConflict, errors.New("system already has an open session"))
		}
	}
	if err := s.ensureDeviceIDLocked(); err != nil {
		return Session{}, err
	}
	x.DeviceID = s.deviceID
	x.CreatedAt = time.Now().In(billingTimezone).Format(time.RFC3339Nano)
	x.UpdatedAt = x.CreatedAt
	x.StartedAt = startedAt.In(billingTimezone).Format(time.RFC3339Nano)
	if x.PaymentStatus == "paid" {
		x.PaymentAt = x.StartedAt
	}
	seconds := x.PurchasedMinutes * 60
	if len(durationSeconds) > 0 {
		seconds = durationSeconds[0]
	}
	x.ExpectedEndAt = startedAt.Add(time.Duration(seconds) * time.Second).Format(time.RFC3339Nano)

	row := len(f.sessions)
	if err := f.put(row, x); err != nil {
		return Session{}, apiError(http.StatusInternalServerError, err)
	}

	if err := s.saveLocked(f); err != nil {
		return Session{}, apiError(http.StatusInternalServerError, err)
	}
	return x, nil
}

type billingHTTPError struct {
	Status int
	Err    error
}

func (e *billingHTTPError) Error() string { return e.Err.Error() }
func (e *billingHTTPError) Unwrap() error { return e.Err }

func apiError(status int, err error) error {
	return &billingHTTPError{Status: status, Err: err}
}

var billingStartMu sync.Mutex
var billingCloseMu sync.Mutex

func startBillingSession(q StartSessionRequest) (Session, error) {
	billingStartMu.Lock()
	defer billingStartMu.Unlock()

	x, pc, err := billingStore.prepareStart(q)
	if err != nil {
		return Session{}, err
	}
	pc.Address = billingControlAddress(pc)
	previous, err := getTimerStatus(pc.Address)
	if err != nil {
		return Session{}, apiError(http.StatusBadGateway, fmt.Errorf("could not read timer status for %s: %w", pc.Name, err))
	}
	restoreAfterFailure := func(cause error, status int) error {
		if restoreErr := restoreTimerStatus(pc.Address, previous); restoreErr != nil {
			logf("billing", "session start failed for pc_id=%s; timer rollback failed: %v", pc.ID, restoreErr)
			return apiError(http.StatusBadGateway, fmt.Errorf("%v; restoring the previous timer state also failed: %w", cause, restoreErr))
		}
		return apiError(status, fmt.Errorf("%v; previous timer state was restored", cause))
	}

	seconds := x.PurchasedMinutes * 60
	if q.DurationSeconds > 0 {
		seconds = q.DurationSeconds
	}
	if err := timerAction(pc.Address, "set", seconds); err != nil {
		return Session{}, restoreAfterFailure(fmt.Errorf("could not set timer for %s: %w", pc.Name, err), http.StatusBadGateway)
	}
	startedAt := time.Now().In(billingTimezone)
	action := "play"
	if seconds == 0 {
		action = "pause"
		stamp := startedAt.Format(time.RFC3339Nano)
		x.payments = append(x.payments, paymentRecord{ID: "R-" + stamp, Timestamp: stamp, Status: "resume_pending"})
		x.ResumeMarker = &x.payments[len(x.payments)-1]
	}
	if err := timerAction(pc.Address, action, 0); err != nil {
		return Session{}, restoreAfterFailure(fmt.Errorf("could not start timer for %s: %w", pc.Name, err), http.StatusBadGateway)
	}
	if err := updateBillingSessionLink(pc, x.ID, false); err != nil {
		cause := fmt.Errorf("could not configure billing synchronization for %s: %w", pc.Name, err)
		if clearErr := updateBillingSessionLink(pc, x.ID, true); clearErr != nil {
			cause = fmt.Errorf("%w; clearing a partial timer billing link also failed: %v", cause, clearErr)
		}
		return Session{}, restoreAfterFailure(cause, http.StatusBadGateway)
	}
	x, err = billingStore.commitStart(x, startedAt, seconds)
	if err != nil {
		if linkErr := updateBillingSessionLink(pc, x.ID, true); linkErr != nil {
			logf("billing", "session start failed for pc_id=%s; could not clear timer billing link: %v", pc.ID, linkErr)
			err = fmt.Errorf("%w; clearing the timer billing link also failed: %v", err, linkErr)
		}
		var statusErr *billingHTTPError
		if errors.As(err, &statusErr) {
			return Session{}, restoreAfterFailure(statusErr.Err, statusErr.Status)
		}
		return Session{}, restoreAfterFailure(err, http.StatusInternalServerError)
	}
	return x, nil
}

func timerEndpoint(address, action string) string {
	return strings.TrimRight(address, "/") + "/" + action
}

func getTimerStatus(address string) (TimerStatus, error) {
	request, err := http.NewRequest(http.MethodGet, timerEndpoint(address, "status"), nil)
	if err != nil {
		return TimerStatus{}, err
	}
	deviceID := ""
	if pc, ok := registeredPCByAddress(address); ok {
		deviceID = pc.ID
	}
	response, err := monitoredHTTPDo(request, "timer-status", deviceID)
	if err != nil {
		return TimerStatus{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return TimerStatus{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return TimerStatus{}, fmt.Errorf("status endpoint returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var status TimerStatus
	if err := httpcheck.DecodeJSON(body, response.Header.Get("Content-Type"), &status); err != nil {
		return TimerStatus{}, fmt.Errorf("invalid timer status response: %w", err)
	}
	if status.TimerStatus != "running" && status.TimerStatus != "paused" {
		return TimerStatus{}, errors.New("timer status response is missing a valid timer state")
	}
	return status, nil
}

func timerActionWithRemaining(address, action string, seconds int) (int, error) {
	payload := []byte("{}")
	switch action {
	case "set":
		payload = fmt.Appendf(nil, `{"seconds":%d}`, seconds)
	case "tweak":
		payload = fmt.Appendf(nil, `{"amount":%d}`, seconds)
	}
	request, err := http.NewRequest(http.MethodPost, timerEndpoint(address, action), bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	deviceID := ""
	if pc, ok := registeredPCByAddress(address); ok {
		deviceID = pc.ID
	}
	response, err := monitoredHTTPDo(request, "timer-action", deviceID)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return 0, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return 0, fmt.Errorf("timer endpoint returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var result struct {
		Status           string `json:"status"`
		Message          string `json:"message"`
		RemainingSeconds *int   `json:"remaining_seconds"`
	}
	if err := httpcheck.DecodeJSON(body, response.Header.Get("Content-Type"), &result); err != nil {
		return 0, fmt.Errorf("invalid timer response: %w", err)
	}
	if result.Status != "success" {
		if result.Message == "" {
			result.Message = "timer command was not accepted"
		}
		return 0, errors.New(result.Message)
	}
	remaining := 0
	if result.RemainingSeconds != nil {
		remaining = *result.RemainingSeconds
	}
	return remaining, nil
}

func timerAction(address, action string, seconds int) error {
	_, err := timerActionWithRemaining(address, action, seconds)
	return err
}

func restoreTimerStatus(address string, previous TimerStatus) error {
	actions := []struct {
		action  string
		seconds int
	}{
		{"set", previous.RemainingSeconds},
		{"pause", 0},
		{"hide", 0},
	}
	if previous.IsRunning {
		actions[1].action = "play"
	}
	if previous.TimeUpVisible {
		actions[2].action = "show"
	}
	var failures []error
	for _, item := range actions {
		if err := timerAction(address, item.action, item.seconds); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", item.action, err))
		}
	}
	return errors.Join(failures...)
}

func (s *BillingStore) summary() BillingSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := BillingSummary{Enabled: s.enabled}
	if !s.enabled {
		out.ConfigurationStatus = "billing disabled"
		return out
	}
	f, err := s.openLocked()
	if err != nil {
		out.ConfigurationStatus = err.Error()
		return out
	}

	rows := f.sessions
	today := time.Now().In(billingTimezone).Format("2006-01-02")
	for _, x := range rows {
		if x.Status == "open" {
			out.OpenSessions++
		}
		if x.PaymentAt != "" {
			paidAt, err := sessionTimestamp(x.PaymentAt)
			if err != nil {
				out.ConfigurationStatus = fmt.Sprintf("invalid payment timestamp for session %s", x.ID)
				return out
			}
			if paidAt.In(billingTimezone).Format("2006-01-02") == today {
				out.TodayCollected += x.AmountPaid
			}
		}
	}
	return out
}

func (s *BillingStore) update(id, action string, minutes, amount int) (Session, error) {
	return s.updateAtStop(id, action, minutes, amount, time.Time{})
}
func (s *BillingStore) updateAtStop(id, action string, minutes, amount int, stopped time.Time) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.require(); err != nil {
		return Session{}, err
	}
	f, err := s.openLocked()
	if err != nil {
		return Session{}, err
	}

	rows := f.indexed()
	for _, stored := range rows {
		x := stored.Session
		if x.ID != id {
			continue
		}
		if x.DeviceID == "" {
			x.DeviceID = s.deviceID
		}
		now := time.Now().In(billingTimezone).Format(time.RFC3339Nano)
		if x.Status == "removed" {
			return Session{}, errors.New("bill removed")
		}
		switch action {
		case "extend":
			if x.Status != "open" || minutes <= 0 {
				return Session{}, errors.New("only positive extensions of open sessions are allowed")
			}
			entry, e := sessionQuote(x, x.PurchasedMinutes+minutes)
			if e != nil {
				return Session{}, e
			}
			entry.Amount = max(0, entry.Amount-x.EstimatedAmount)
			start, e := sessionTimestamp(x.StartedAt)
			if e == nil {
				x.ExpectedEndAt = start.Add(time.Duration(x.plannedSeconds(start)+minutes*60) * time.Second).Format(time.RFC3339Nano)
			}
			x.PurchasedMinutes += minutes
			x.FinalAmount += entry.Amount
			x.EstimatedAmount = x.FinalAmount - x.Snacks
			x.AmountPaid += entry.Amount
			x.PaymentStatus = "paid"
			x.PaymentAt = now
			x.payments = append(x.payments, paymentRecord{
				ID:        "P-" + time.Now().UTC().Format("20060102150405.000000000"),
				Timestamp: now, Amount: entry.Amount, Status: "paid",
			})
		case "correct":
			if minutes < 0 || minutes > 1440 || amount < 0 || amount > 9999999 {
				return Session{}, errors.New("invalid duration or amount")
			}
			x.TimeCorrected = true
			x.ActualDurationSeconds = minutes * 60
			x.PaymentAt = now
			x.payments = append(x.payments, paymentRecord{ID: "C-" + now, Timestamp: now, Amount: amount - x.AmountPaid, Status: "correction"})
			x.AmountPaid = amount
			x.PaymentStatus = "partial"
			if amount == 0 {
				x.PaymentStatus = "unpaid"
			}
			if amount >= x.FinalAmount {
				x.PaymentStatus = "paid"
			}
		case "snack":
			if x.Status != "open" || amount <= 0 || amount > 9999999-x.FinalAmount {
				return Session{}, errors.New("select an open session and enter a positive snack amount")
			}
			x.Snacks += amount
			x.FinalAmount += amount
			if x.AmountPaid >= x.FinalAmount {
				x.PaymentStatus = "paid"
			} else if x.AmountPaid > 0 {
				x.PaymentStatus = "partial"
			} else {
				x.PaymentStatus = "unpaid"
			}
		case "pay":
			if amount <= 0 {
				amount = x.FinalAmount - x.AmountPaid
			}
			if amount <= 0 {
				return Session{}, errors.New("no amount is due")
			}
			x.AmountPaid += amount
			x.PaymentAt = now
			if x.AmountPaid >= x.FinalAmount {
				x.PaymentStatus = "paid"
			} else {
				x.PaymentStatus = "partial"
			}
			x.payments = append(x.payments, paymentRecord{
				ID:        "P-" + time.Now().UTC().Format("20060102150405.000000000"),
				Timestamp: now, Amount: amount, Status: x.PaymentStatus,
			})
		case "close":
			if x.Status == "closed" {
				return x, nil
			}
			if x.Status != "open" {
				return Session{}, apiError(http.StatusConflict, errors.New("session is already closed"))
			}
			started, parseErr := sessionTimestamp(x.StartedAt)
			if parseErr != nil {
				return Session{}, fmt.Errorf("session %q has an invalid start time: %w", x.ID, parseErr)
			}
			ended, parseErr := sessionTimestamp(now)
			if parseErr != nil {
				return Session{}, parseErr
			}
			if !stopped.IsZero() && stopped.Before(ended) && !stopped.Before(started) {
				ended = stopped
			}
			x.Status = "closed"
			x.EndedAt = ended.In(billingTimezone).Format(time.RFC3339Nano)
			if resumed, ok := lastResume(x); ok {
				if !resumed.IsZero() {
					remaining := max(0, x.gameplayLimitSeconds(started)-x.ActualDurationSeconds)
					x.ActualDurationSeconds += min(remaining, max(0, int(ended.Sub(resumed).Seconds())))
				}
			} else if !x.TimeCorrected {
				x.ActualDurationSeconds = min(x.gameplayLimitSeconds(started), max(0, int(ended.Sub(started).Seconds())))
			}
		default:
			return Session{}, errors.New("unknown session action")
		}
		if (action == "close" || action == "correct") && x.ManualPrice == nil && x.PricingSnapshot != "" {
			elapsed := (x.ActualDurationSeconds + 59) / 60
			x.FinalAmount = x.Snacks
			if x.Continued != nil && action == "close" {
				elapsed = (max(0, x.ActualDurationSeconds-x.Continued.SpentSeconds) + 59) / 60
				x.FinalAmount = x.Continued.Amount + x.Snacks
			}
			if elapsed > 0 {
				price, err := sessionQuote(x, elapsed)
				if err != nil {
					return Session{}, err
				}
				x.FinalAmount += price.Amount
			}
			x.PaymentStatus = "paid"
			if x.AmountPaid < x.FinalAmount {
				x.PaymentStatus = "partial"
				if x.AmountPaid == 0 {
					x.PaymentStatus = "unpaid"
				}
			}
		}
		if action == "close" && !pricingCatalog().ManualCollection {
			difference := x.FinalAmount - x.AmountPaid
			if difference != 0 {
				x.PaymentAt = now
				x.payments = append(x.payments, paymentRecord{ID: "P-close-" + now, Timestamp: now, Amount: difference, Status: "paid"})
			}
			x.AmountPaid = x.FinalAmount
			x.PaymentStatus = "paid"
		}
		x.UpdatedAt = now
		x.SyncedUpdatedAt = ""
		if err := f.put(stored.Row, x); err != nil {
			return Session{}, err
		}

		if err := s.saveLocked(f); err != nil {
			return Session{}, err
		}
		return x, nil
	}
	return Session{}, errors.New("session not found")
}

func normalizeSessionAddress(address string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(address)), "/")
}

func registeredPCByAddress(address string) (PC, bool) {
	normalized := normalizeSessionAddress(address)
	if normalized == "" {
		return PC{}, false
	}
	for _, pc := range store.list() {
		if normalizeSessionAddress(pc.Address) == normalized {
			return pc, true
		}
	}
	return PC{}, false
}

func extensionPaymentID(requestID string) string {
	sum := sha256.Sum256([]byte(requestID))
	return "E-" + hex.EncodeToString(sum[:])
}

func (s *BillingStore) extendSessionRecord(id, pcID, pcAddress string, minutes, amount, remaining int, requestID string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.require(); err != nil {
		return Session{}, err
	}
	f, err := s.openLocked()
	if err != nil {
		return Session{}, err
	}

	rows := f.indexed()
	requestKey := extensionPaymentID(requestID)
	for _, stored := range rows {
		x := stored.Session
		if x.ID != id {
			continue
		}
		if x.DeviceID == "" {
			x.DeviceID = s.deviceID
		}
		if x.PCID != pcID || normalizeSessionAddress(x.PCAddress) != normalizeSessionAddress(pcAddress) {
			return Session{}, errors.New("session does not belong to the requested system endpoint")
		}
		if x.Status != "open" {
			return Session{}, errors.New("only open sessions can be extended")
		}
		for _, payment := range x.payments {
			if payment.ID != requestKey {
				continue
			}
			if payment.ExtensionMinutes != minutes {
				return Session{}, errors.New("extension request ID was already used with a different duration")
			}
			return x, nil
		}

		now := time.Now().In(billingTimezone).Format(time.RFC3339Nano)
		started, err := sessionTimestamp(x.StartedAt)
		if err != nil {
			return Session{}, fmt.Errorf("session %q has no valid start time: %w", x.ID, err)
		}
		x.ExpectedEndAt = started.Add(time.Duration(x.plannedSeconds(started)+minutes*60) * time.Second).Format(time.RFC3339Nano)
		x.PurchasedMinutes += minutes
		x.FinalAmount += amount
		x.EstimatedAmount = x.FinalAmount - x.Snacks
		x.AmountPaid += amount
		if amount > 0 {
			x.PaymentAt = now
		}
		if x.AmountPaid == 0 {
			x.PaymentStatus = "unpaid"
		} else if x.AmountPaid >= x.FinalAmount {
			x.PaymentStatus = "paid"
		} else {
			x.PaymentStatus = "partial"
		}
		x.UpdatedAt = now
		x.SyncedUpdatedAt = ""
		x.payments = append(x.payments, paymentRecord{
			ID: requestKey, Timestamp: now, Amount: amount, Status: x.PaymentStatus,
			ExtensionMinutes: minutes, RemainingSeconds: remaining,
		})
		if err := f.put(stored.Row, x); err != nil {
			return Session{}, err
		}

		if err := s.saveLocked(f); err != nil {
			return Session{}, err
		}
		return x, nil
	}
	return Session{}, errors.New("session not found")
}

func (s *BillingStore) openSessionForSystem(sessionID, pcID, pcAddress string, target *billingTarget) (Session, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.require(); err != nil {
		return Session{}, false, err
	}
	f, err := s.openLocked()
	if err != nil {
		return Session{}, false, err
	}

	stored := f.indexed()
	targetAddr := normalizeSessionAddress(pcAddress)
	if sessionID == "" && pcID == "" && targetAddr == "" {
		return Session{}, false, errors.New("session ID or system endpoint is required")
	}
	matches := func(x Session) bool {
		if target != nil {
			return (strings.EqualFold(x.DeviceID, target.Owner) || previousDeviceName(x.DeviceID)) && billingPath(x.PCAddress) == target.Path
		}
		return (pcID == "" || pcID == x.PCID) && (targetAddr == "" || normalizeSessionAddress(x.PCAddress) == targetAddr)
	}
	var foundSession Session
	found := false
	for _, item := range stored {
		x := item.Session
		if x.Status != "open" || (sessionID != "" && x.ID != sessionID) {
			continue
		}
		if !matches(x) {
			continue
		}
		if found {
			return Session{}, false, errors.New("multiple open sessions match the requested system endpoint")
		}
		foundSession, found = x, true
	}
	if !found && sessionID != "" {
		for _, item := range stored {
			x := item.Session
			if x.Status == "open" &&
				matches(x) {
				return Session{}, false, errors.New("session ID no longer matches the active session for this system")
			}
		}
	}
	return foundSession, found, nil
}

type ExtendSessionRequest struct {
	Target     *billingTarget `json:"target,omitempty"`
	SessionID  string         `json:"session_id,omitempty"`
	PCID       string         `json:"pc_id,omitempty"`
	PCAddress  string         `json:"pc_address,omitempty"`
	Minutes    int            `json:"minutes,omitempty"`
	Seconds    int            `json:"seconds,omitempty"`
	RequestID  string         `json:"request_id,omitempty"`
	TweakTimer *bool          `json:"tweak_timer,omitempty"`
}

type ExtendSessionResponse struct {
	SessionFound     bool     `json:"session_found"`
	Session          *Session `json:"session,omitempty"`
	RemainingSeconds int      `json:"remaining_seconds,omitempty"`
	Message          string   `json:"message,omitempty"`
}

var (
	extensionMu sync.Mutex
)

func extendSessionForSystem(req ExtendSessionRequest, tweakRemoteTimer bool) (ExtendSessionResponse, error) {
	extensionMu.Lock()
	defer extensionMu.Unlock()

	if req.TweakTimer != nil {
		tweakRemoteTimer = *req.TweakTimer
	}

	if strings.TrimSpace(req.RequestID) == "" {
		return ExtendSessionResponse{}, errors.New("extension request ID is required")
	}
	if len(req.RequestID) > 200 {
		return ExtendSessionResponse{}, errors.New("extension request ID is too long")
	}

	var targetAddress string
	var targetPC PC
	if req.Target != nil {
		pc, ok := ownedBillingTimer(req.Target)
		if !ok {
			return ExtendSessionResponse{}, errors.New("timer is not hosted on this device")
		}
		targetPC = pc
		targetAddress = pc.Address
	} else if req.PCID != "" {
		pc, ok := store.find(req.PCID)
		if !ok {
			return ExtendSessionResponse{}, errors.New("gaming system not found")
		}
		targetPC = pc
		targetAddress = pc.Address
		if req.PCAddress != "" && normalizeSessionAddress(req.PCAddress) != normalizeSessionAddress(pc.Address) {
			return ExtendSessionResponse{}, errors.New("system ID and endpoint do not match")
		}
	} else if req.PCAddress != "" {
		for _, pc := range store.list() {
			if normalizeSessionAddress(pc.Address) == normalizeSessionAddress(req.PCAddress) {
				targetPC = pc
				targetAddress = pc.Address
				break
			}
		}
		if targetAddress == "" {
			return ExtendSessionResponse{}, errors.New("system endpoint is not registered")
		}
	}

	activeSession, found, err := billingStore.openSessionForSystem(req.SessionID, targetPC.ID, targetAddress, targetForPC(targetPC))
	if err != nil {
		return ExtendSessionResponse{}, err
	}
	if !found {
		return ExtendSessionResponse{
			SessionFound: false,
			Message:      "no open billing session found for this system",
		}, nil
	}

	if targetAddress == "" {
		targetAddress = activeSession.PCAddress
		targetPC.ID = activeSession.PCID
	} else {
		targetAddress = billingControlAddress(targetPC)
	}

	minutes := req.Minutes
	if req.Seconds != 0 {
		if minutes != 0 {
			return ExtendSessionResponse{}, errors.New("specify either minutes or seconds, not both")
		}
		if req.Seconds <= 0 || req.Seconds%60 != 0 {
			return ExtendSessionResponse{}, errors.New("a billed extension must add a whole number of minutes")
		}
		minutes = req.Seconds / 60
	}
	if minutes <= 0 {
		return ExtendSessionResponse{}, errors.New("extension duration must be greater than zero")
	}
	var entry PriceEntry
	if activeSession.Players > 0 && activeSession.Category != "" {
		entry, err = sessionQuote(activeSession, activeSession.PurchasedMinutes+minutes)
	}
	if err != nil {
		return ExtendSessionResponse{}, fmt.Errorf("pricing rule lookup failed: %w", err)
	}

	requestKey := extensionPaymentID(req.RequestID)
	for _, payment := range activeSession.payments {
		if payment.ID != requestKey {
			continue
		}
		if payment.ExtensionMinutes != minutes {
			return ExtendSessionResponse{}, errors.New("extension request ID was already used with a different duration")
		}
		remaining := payment.RemainingSeconds
		if tweakRemoteTimer {
			status, statusErr := getTimerStatus(targetAddress)
			if statusErr != nil {
				return ExtendSessionResponse{}, fmt.Errorf("extension was already applied to session %s, but current timer status could not be read: %w", activeSession.ID, statusErr)
			}
			remaining = status.RemainingSeconds
		}
		return ExtendSessionResponse{SessionFound: true, Session: &activeSession, RemainingSeconds: remaining}, nil
	}

	remainingSeconds := 0
	if tweakRemoteTimer && targetAddress != "" {
		rem, err := timerActionWithRemaining(targetAddress, "tweak", minutes*60)
		if err != nil {
			return ExtendSessionResponse{}, fmt.Errorf("could not extend overlay timer on %s: %w", activeSession.PCName, err)
		}
		remainingSeconds = rem
	}

	updated, err := billingStore.extendSessionRecord(activeSession.ID, activeSession.PCID, activeSession.PCAddress, minutes, max(0, entry.Amount-activeSession.EstimatedAmount), remainingSeconds, req.RequestID)
	if err != nil {
		if tweakRemoteTimer && targetAddress != "" {
			if _, rollbackErr := timerActionWithRemaining(targetAddress, "tweak", -minutes*60); rollbackErr != nil {
				return ExtendSessionResponse{}, apiError(http.StatusInternalServerError, fmt.Errorf("could not update CSV billing record for session %s: %v; timer rollback also failed: %w", activeSession.ID, err, rollbackErr))
			}
		}
		return ExtendSessionResponse{}, apiError(http.StatusInternalServerError, fmt.Errorf("could not update CSV billing record: %w", err))
	}

	resp := ExtendSessionResponse{
		SessionFound:     true,
		Session:          &updated,
		RemainingSeconds: remainingSeconds,
	}
	return resp, nil
}

func newExtensionRequestID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

// A billing target survives address changes and registry-local ID differences.
type billingTarget struct {
	Owner string `json:"owner"`
	Path  string `json:"path"`
}

func billingPath(address string) string {
	u, err := url.Parse(address)
	if err != nil || u.Host == "" {
		return "!invalid"
	}
	return strings.TrimSuffix(u.EscapedPath(), "/")
}

func targetForPC(pc PC) *billingTarget {
	owner := pc.Owner
	if owner == "" {
		// Older registries have the machine name on the host record. Virtual
		// timer display names are independent of the machine that hosts them.
		if root, ok := registeredPCByAddress(strings.TrimSuffix(strings.TrimRight(pc.Address, "/"), billingPath(pc.Address))); ok && strings.EqualFold(root.Name, configuredFriendlyName()) {
			owner = configuredFriendlyName()
		}
	}
	if owner == "" {
		return nil
	}
	return &billingTarget{Owner: owner, Path: billingPath(pc.Address)}
}

// Resolve only timers this installation actually serves. A peer never supplies
// the address used to control a local timer.
func ownedBillingTimer(t *billingTarget) (PC, bool) {
	if t == nil || !(strings.EqualFold(t.Owner, configuredFriendlyName()) || previousDeviceName(t.Owner)) {
		return PC{}, false
	}
	host := localRegistryPC()
	for _, pc := range identityPCs(host.Address, localNodeIdentity()) {
		if billingPath(pc.Address) != t.Path {
			continue
		}
		if saved, ok := registeredPCByAddress(pc.Address); ok {
			pc.ID = saved.ID
		}
		return pc, true
	}
	return PC{}, false
}

// The timer server listens on loopback as well as the LAN. Once ownership
// is resolved, keep the local command independent of adapter selection.
func billingControlAddress(pc PC) string {
	if pc.Owner != "" && strings.EqualFold(pc.Owner, configuredFriendlyName()) {
		return fmt.Sprintf("http://127.0.0.1:%d%s", appConfig.TimerPort, billingPath(pc.Address))
	}
	return pc.Address
}

func localBillingPC(pc PC) bool {
	if t := targetForPC(pc); t != nil {
		_, ok := ownedBillingTimer(t)
		return ok
	}
	// Old manually registered loopback timers predate owner metadata.
	u, err := url.Parse(pc.Address)
	if err != nil || (pc.UIPort != 0 && pc.UIPort != appConfig.UIDiscoveryPort) {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
}

func requestMatchesTimer(address string, pc PC, target *billingTarget) bool {
	if target != nil {
		return billingPath(address) == target.Path
	}
	return normalizeSessionAddress(address) == normalizeSessionAddress(pc.Address)
}

func sessionAtPC(s Session, pc PC) bool {
	if t := targetForPC(pc); t != nil && s.DeviceID != "" {
		return (strings.EqualFold(s.DeviceID, t.Owner) || (strings.EqualFold(t.Owner, configuredFriendlyName()) && previousDeviceName(s.DeviceID))) && billingPath(s.PCAddress) == t.Path
	}
	return s.PCID == pc.ID && normalizeSessionAddress(s.PCAddress) == normalizeSessionAddress(pc.Address)
}

func routeForSession(s Session) (PC, bool) {
	t := &billingTarget{Owner: s.DeviceID, Path: billingPath(s.PCAddress)}
	if pc, ok := ownedBillingTimer(t); ok {
		// Preserve the direct route for legacy/manual loopback timers.
		if old, found := store.find(s.PCID); found && targetForPC(old) == nil && localBillingPC(old) && sessionAtPC(s, old) {
			return old, true
		}
		return pc, true
	}
	if pc, ok := store.find(s.PCID); ok && !pc.Missing && sessionAtPC(s, pc) {
		return pc, true
	}
	for _, pc := range store.list() {
		if !pc.Missing && sessionAtPC(s, pc) {
			return pc, true
		}
	}
	return PC{}, false
}

var billingSyncRunMu sync.Mutex

func billingDeviceIdentity() (string, error) {
	billingStore.mu.Lock()
	defer billingStore.mu.Unlock()
	if err := billingStore.require(); err != nil {
		return "", err
	}
	if err := billingStore.ensureDeviceIDLocked(); err != nil {
		return "", err
	}
	return billingStore.deviceID, nil
}

func billingSyncTime() string {
	return time.Now().In(billingTimezone).Format(time.RFC3339Nano)
}

type billingPeerSnapshot struct {
	pc       PC
	status   BillingDeviceStatus
	exported BillingSyncExport
}

func readBillingPeer(pc PC, endpoint string) billingPeerSnapshot {
	peer := billingPeerSnapshot{pc: pc, status: BillingDeviceStatus{Name: pc.Name, Endpoint: endpoint, LastAttemptAt: billingSyncTime()}}
	status := &peer.status
	if err := callBillingPeer(pc, http.MethodGet, "/internal/api/billing/sync/pending", nil, &peer.exported); err != nil {
		status.DeviceID = "unavailable-" + stablePCID(endpoint)
		status.Error = err.Error()
		return peer
	}
	status.Online = true
	status.DeviceID = peer.exported.DeviceID
	if status.DeviceID == "" {
		status.DeviceID = "unavailable-" + stablePCID(endpoint)
		status.Error = "billing endpoint did not return a PC name"
		return peer
	}
	if peer.exported.Name != "" {
		status.Name = peer.exported.Name
	}
	status.PendingRecords = len(peer.exported.Sessions)
	status.Instance = peer.exported.Instance
	return peer
}

func mergeBillingPeer(peer billingPeerSnapshot) BillingDeviceStatus {
	pc, status, exported := peer.pc, peer.status, peer.exported
	for i := range exported.Sessions {
		if exported.Sessions[i].DeviceID != status.DeviceID {
			status.Error = "billing device exported a session with a mismatched device ID"
			return status
		}
	}
	if err := billingStore.mergeSyncedSessions(status.DeviceID, exported.Sessions); err != nil {
		status.Error = fmt.Sprintf("could not save synchronized sessions: %v", err)
		return status
	}
	if len(exported.Sessions) > 0 {
		ack := BillingSyncAck{DeviceID: status.DeviceID, Sessions: make([]BillingSyncAckRecord, 0, len(exported.Sessions))}
		for _, session := range exported.Sessions {
			ack.Sessions = append(ack.Sessions, BillingSyncAckRecord{ID: session.ID, UpdatedAt: session.UpdatedAt})
		}
		var ackResult BillingSyncAckResult
		if err := callBillingPeer(pc, http.MethodPost, "/internal/api/billing/sync/ack", ack, &ackResult); err != nil {
			status.Error = fmt.Sprintf("records were saved centrally but source acknowledgement failed: %v", err)
			return status
		}
		if ackResult.Acknowledged != len(exported.Sessions) {
			status.PendingRecords = len(exported.Sessions) - ackResult.Acknowledged
			status.Error = "one or more sessions changed during synchronization and remain pending"
			return status
		}
	}
	status.PendingRecords = 0
	status.LastSuccessfulAt = status.LastAttemptAt
	return status
}

func syncBillingDevices() ([]BillingDeviceStatus, error) {
	if !billingSyncRunMu.TryLock() {
		return billingStore.billingSyncStatuses(), errors.New("billing synchronization is already running")
	}
	defer billingSyncRunMu.Unlock()
	previousStatuses := billingStore.billingSyncStatuses()
	deviceID, err := billingDeviceIdentity()
	if err != nil {
		return nil, err
	}
	now := billingSyncTime()
	local := localRegistryPC()
	localEndpoint, err := billingPeerEndpoint(local, "")
	if err != nil {
		return nil, err
	}
	localStatus := BillingDeviceStatus{
		DeviceID: deviceID, Name: configuredFriendlyName(), Endpoint: localEndpoint,
		Online: true, LastAttemptAt: now, LastSuccessfulAt: now,
	}
	localStatus.Instance = billingNodeInstance
	statuses := []BillingDeviceStatus{localStatus}
	peers := map[string]PC{}
	for _, pc := range store.list() {
		if pc.Deleted {
			continue
		}
		endpoint, e := billingPeerEndpoint(pc, "")
		if e != nil || endpoint == localEndpoint || localConfigAddress(pc) {
			continue
		}
		peers[endpoint] = pc
	}
	results := make(chan billingPeerSnapshot, len(peers))
	for endpoint, pc := range peers {
		go func() { results <- readBillingPeer(pc, endpoint) }()
	}
	groups := map[string][]billingPeerSnapshot{}
	failed := map[string]BillingDeviceStatus{}
	for range peers {
		peer := <-results
		if peer.status.Error != "" {
			failed[peer.status.Endpoint] = peer.status
			continue
		}
		key := strings.ToLower(peer.status.DeviceID)
		groups[key] = append(groups[key], peer)
	}
	for key, group := range groups {
		isLocal := strings.EqualFold(key, deviceID)
		var prior BillingDeviceStatus
		for _, old := range previousStatuses {
			if strings.EqualFold(old.DeviceID, key) && (old.Error == "" || len(old.Aliases) > 0) {
				prior = old
				break
			}
		}
		sort.Slice(group, func(i, j int) bool {
			a, b := group[i].status.Endpoint, group[j].status.Endpoint
			if a == b {
				return false
			}
			if a == prior.Endpoint {
				return true
			}
			if b == prior.Endpoint {
				return false
			}
			return a < b
		})
		instance := group[0].status.Instance
		if len(group) > 1 || isLocal || len(prior.Aliases) > 0 {
			// Older nodes already expose this process marker on their LAN probe.
			for i := range group {
				if group[i].status.Instance == "" {
					var probe struct {
						Instance string `json:"instance"`
					}
					if callBillingPeer(group[i].pc, http.MethodGet, "/api/proxy/local-probe", nil, &probe) == nil {
						group[i].status.Instance = probe.Instance
					}
				}
			}
			instance = group[0].status.Instance
		}
		conflict := false
		if len(group) > 1 || isLocal {
			conflict = instance == "" || isLocal && instance != billingNodeInstance
			for _, peer := range group {
				if peer.status.Instance != instance {
					conflict = true
				}
			}
		}
		if conflict {
			for _, peer := range group {
				st := peer.status
				st.Error = "same PC name at multiple endpoints; could not verify one running timer, so records were not merged"
				statuses = append(statuses, st)
			}
			continue
		}
		var st BillingDeviceStatus
		if isLocal {
			st = statuses[0]
		} else {
			st = mergeBillingPeer(group[0])
		}
		aliases := map[string]bool{}
		for _, peer := range group {
			aliases[peer.status.Endpoint] = true
		}
		if instance != "" && prior.Instance == instance {
			for _, addr := range append([]string{prior.Endpoint}, prior.Aliases...) {
				if missing, ok := failed[addr]; ok && !missing.Online {
					aliases[addr] = true
					delete(failed, addr)
				}
			}
		}
		delete(aliases, st.Endpoint)
		for addr := range aliases {
			st.Aliases = append(st.Aliases, addr)
		}
		sort.Strings(st.Aliases)
		if isLocal {
			statuses[0] = st
		} else {
			statuses = append(statuses, st)
		}
	}
	// Keep previously verified aliases together if every registered route is offline.
	for _, old := range previousStatuses {
		if old.Instance == "" || len(old.Aliases) == 0 {
			continue
		}
		addresses := append([]string{old.Endpoint}, old.Aliases...)
		offline := []string{}
		allOffline := true
		for _, addr := range addresses {
			if _, registered := peers[addr]; !registered {
				continue
			}
			miss, ok := failed[addr]
			if !ok || miss.Online {
				allOffline = false
				break
			}
			offline = append(offline, addr)
		}
		if allOffline && len(offline) > 0 {
			st := failed[offline[0]]
			st.Name = old.Name
			st.DeviceID = old.DeviceID
			st.Instance = old.Instance
			st.Aliases = offline[1:]
			st.LastSuccessfulAt = old.LastSuccessfulAt
			statuses = append(statuses, st)
			for _, addr := range offline {
				delete(failed, addr)
			}
		}
	}
	for _, st := range failed {
		statuses = append(statuses, st)
	}
	for i := range statuses {
		if statuses[i].LastSuccessfulAt != "" {
			continue
		}
		for _, old := range previousStatuses {
			if old.Endpoint == statuses[i].Endpoint {
				statuses[i].LastSuccessfulAt = old.LastSuccessfulAt
				break
			}
		}
	}
	sort.Slice(statuses[1:], func(i, j int) bool { return statuses[i+1].Endpoint < statuses[j+1].Endpoint })
	if err := billingStore.replaceSyncStatuses(statuses); err != nil {
		return billingStore.billingSyncStatuses(), err
	}
	return billingStore.billingSyncStatuses(), nil
}

func startBillingSyncScheduler() {
	go func() {
		if _, err := syncBillingDevices(); err != nil {
			logf("billing-sync", "initial synchronization failed: %v", err)
		}
		ticker := time.NewTicker(billingSyncInterval)
		defer ticker.Stop()
		for range ticker.C {
			if _, err := syncBillingDevices(); err != nil {
				logf("billing-sync", "synchronization failed: %v", err)
			}
		}
	}()
}

func billingSyncHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	statuses, err := syncBillingDevices()
	if err != nil {
		writeBillingError(w, err)
		return
	}
	complete := len(statuses) > 0
	for _, status := range statuses {
		if !status.Online || status.PendingRecords > 0 || status.Error != "" {
			complete = false
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"complete": complete, "devices": statuses})
}

func billingReportHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET required"})
		return
	}
	start, end := r.URL.Query().Get("start"), r.URL.Query().Get("end")
	if start != "" || end != "" {
		if start == "" || end == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "start and end dates must be provided together"})
			return
		}
		report, err := billingStore.reportRange(start, end)
		if err != nil {
			writeBillingError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, report)
		return
	}
	date := r.URL.Query().Get("date")
	if date == "" {
		date = time.Now().In(billingTimezone).Format("2006-01-02")
	}
	report, err := billingStore.report(date)
	if err != nil {
		writeBillingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func noPCSnackSaleHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	var request struct {
		Amount int `json:"amount"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid snack sale"})
		return
	}
	if request.Amount < 1 || request.Amount > 9999999 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "snack amount must be between 1 and 9999999 INR"})
		return
	}
	id, err := newSessionID()
	if err != nil {
		writeBillingError(w, err)
		return
	}
	now := time.Now().In(billingTimezone)
	timestamp := now.Format(time.RFC3339Nano)
	sale := Session{
		ID: id, PCID: noPCSnackPCID, PCName: "No PC", Category: "Snacks",
		Status: "closed", StartedAt: timestamp, EndedAt: timestamp,
		PricingSnapshot: "Standalone snack sale", Snacks: request.Amount,
		FinalAmount: request.Amount, AmountPaid: request.Amount, PaymentStatus: "paid",
		PaymentAt: timestamp,
		payments:  []paymentRecord{{ID: "SNACK-" + id, Timestamp: timestamp, Amount: request.Amount, Status: "paid"}},
	}
	sale, err = billingStore.commitStart(sale, now)
	if err != nil {
		writeBillingError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sale)
}

func billingSyncPendingHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET required"})
		return
	}
	result, err := billingStore.pendingSync()
	if err != nil {
		writeBillingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func billingSyncAckHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	var ack BillingSyncAck
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&ack); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid billing synchronization acknowledgement"})
		return
	}
	acknowledged, err := billingStore.acknowledgeSync(ack)
	if err != nil {
		writeBillingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, BillingSyncAckResult{Acknowledged: acknowledged})
}

func billingPeerEndpoint(pc PC, path string) (string, error) {
	parsed, err := url.Parse(pc.Address)
	if err != nil || parsed.Hostname() == "" {
		return "", errors.New("registered system has an invalid timer endpoint")
	}
	uiPort := pc.UIPort
	if uiPort < 1 {
		uiPort = appConfig.UIDiscoveryPort
	}
	if uiPort < 1 || uiPort > 65535 {
		return "", errors.New("registered system has an invalid management port")
	}
	return "http://" + net.JoinHostPort(parsed.Hostname(), strconv.Itoa(uiPort)) + path, nil
}

func callBillingPeer(pc PC, method, path string, payload, result any) error {
	return callBillingPeerContext(context.Background(), pc, method, path, payload, result)
}

func callBillingPeerContext(ctx context.Context, pc PC, method, path string, payload, result any) error {
	endpoint, err := billingPeerEndpoint(pc, path)
	if err != nil {
		return err
	}
	operation := billingPeerOperation(method, path)
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		logBillingPeerFailure(operation, pc, method, endpoint, "", 0, "", err)
		return err
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if method == http.MethodGet && strings.HasSuffix(path, "/status") {
		request.Header.Set("Cache-Control", "no-cache")
	}
	response, err := monitoredHTTPDo(request, operation, pc.ID)
	if err != nil {
		logBillingPeerFailure(operation, pc, method, endpoint, request.Header.Get(httpcheck.RequestIDHeader), 0, "", err)
		return apiError(http.StatusBadGateway, fmt.Errorf("could not reach billing device %s at %s %s: %w", pc.Name, method, endpoint, err))
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		logBillingPeerFailure(operation, pc, method, endpoint, request.Header.Get(httpcheck.RequestIDHeader), response.StatusCode, "", err)
		return apiError(http.StatusBadGateway, fmt.Errorf("could not read response from billing device %s: %w", pc.Name, err))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		logBillingPeerFailure(operation, pc, method, endpoint, request.Header.Get(httpcheck.RequestIDHeader), response.StatusCode, string(data), nil)
		var problem struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &problem)
		if problem.Error == "" {
			problem.Error = strings.TrimSpace(string(data))
		}
		return apiError(http.StatusBadGateway, fmt.Errorf("billing device %s request %s %s returned HTTP %d: %s", pc.Name, method, endpoint, response.StatusCode, problem.Error))
	}
	if result != nil {
		if err := httpcheck.DecodeJSON(data, response.Header.Get("Content-Type"), result); err != nil {
			logBillingPeerFailure(operation, pc, method, endpoint, request.Header.Get(httpcheck.RequestIDHeader), response.StatusCode, "", err)
			return apiError(http.StatusBadGateway, fmt.Errorf("billing device %s returned an invalid response: %w", pc.Name, err))
		}
	}
	return nil
}

func billingPeerOperation(method, path string) string {
	switch {
	case method == http.MethodGet && strings.HasPrefix(path, "/internal/api/local/sessions/") &&
		strings.HasSuffix(path, "/status"):
		return "session-status"
	case method == http.MethodPost && path == "/internal/api/local/sessions":
		return "session-start"
	case method == http.MethodPost && strings.HasSuffix(path, "/close") &&
		strings.HasPrefix(path, "/internal/api/local/sessions/"):
		return "session-close"
	case strings.HasPrefix(path, "/internal/api/local/sessions/extend"):
		return "session-extension"
	case path == "/internal/api/billing/sync/pending":
		return "billing-sync-export"
	case path == "/internal/api/billing/sync/ack":
		return "billing-sync-acknowledgement"
	default:
		return "billing-peer-request"
	}
}

func safePeerErrorBody(body string) string {
	const maxLogBody = 160
	body = strings.TrimSpace(body)
	switch body {
	case "404 page not found", "Method Not Allowed", "Not Found":
		if len(body) > maxLogBody {
			body = body[:maxLogBody]
		}
		return body
	case "":
		return "<empty>"
	default:
		return "<response body omitted>"
	}
}

func logBillingPeerFailure(operation string, pc PC, method, endpoint, requestID string, status int, body string, err error) {
	handlerResult := "application-response"
	if status == 0 {
		handlerResult = "no-response"
	} else if safePeerErrorBody(body) == "404 page not found" {
		handlerResult = "route-not-matched"
	}
	logf("billing-peer",
		"operation=%s request_id=%q registered_pc_id=%q name=%q method=%s url=%s status=%d handler_result=%s response_body=%q error=%v",
		operation, requestID, pc.ID, pc.Name, method, endpoint, status, handlerResult, safePeerErrorBody(body), err)
}

func startBillingSessionForDashboard(q StartSessionRequest) (Session, error) {
	return startBillingSessionForDashboardContext(context.Background(), q)
}

func startBillingSessionForDashboardContext(ctx context.Context, q StartSessionRequest) (Session, error) {
	pc, ok := store.find(q.PCID)
	if !ok {
		return Session{}, errors.New("gaming system not found")
	}
	if pc.Inactive {
		return Session{}, errors.New("gaming system is inactive")
	}
	q.Target = targetForPC(pc)
	if localBillingPC(pc) {
		return startBillingSession(q)
	}
	var session Session
	if err := callBillingPeerContext(ctx, pc, http.MethodPost, "/internal/api/local/sessions", q, &session); err != nil {
		return Session{}, err
	}
	if session.DeviceID == "" || !sessionAtPC(session, pc) {
		return Session{}, errors.New("billing device returned a session for a different system endpoint")
	}
	if err := billingStore.mergeSyncedSessions(session.DeviceID, []Session{session}); err != nil {
		return Session{}, fmt.Errorf("session started on %s, but its billing record could not be saved to this dashboard: %w", pc.Name, err)
	}
	return session, nil
}

func extendSessionForDashboard(req ExtendSessionRequest) (ExtendSessionResponse, error) {
	return extendSessionForDashboardContext(context.Background(), req)
}

func extendSessionForDashboardContext(ctx context.Context, req ExtendSessionRequest) (ExtendSessionResponse, error) {
	pc, ok := store.find(req.PCID)
	if !ok && req.PCAddress != "" {
		pc, ok = registeredPCByAddress(req.PCAddress)
	}
	if !ok {
		return ExtendSessionResponse{}, errors.New("gaming system is not registered")
	}
	req.Target = targetForPC(pc)
	if req.PCAddress != "" && !requestMatchesTimer(req.PCAddress, pc, req.Target) {
		return ExtendSessionResponse{}, errors.New("system ID and endpoint do not match")
	}
	if localBillingPC(pc) {
		return extendSessionForSystem(req, true)
	}
	var response ExtendSessionResponse
	if err := callBillingPeerContext(ctx, pc, http.MethodPost, "/internal/api/local/sessions/extend", req, &response); err != nil {
		return ExtendSessionResponse{}, err
	}
	if response.SessionFound && response.Session != nil {
		if err := billingStore.mergeSyncedSessions(response.Session.DeviceID, []Session{*response.Session}); err != nil {
			return ExtendSessionResponse{}, fmt.Errorf("timer and device billing were updated, but this dashboard could not save the synchronized record: %w", err)
		}
	}
	return response, nil
}

func updateSessionForDashboard(id, action string, minutes, amount int) (Session, error) {
	return updateSessionForDashboardContext(context.Background(), id, action, minutes, amount)
}

func updateSessionForDashboardContext(ctx context.Context, id, action string, minutes, amount int) (Session, error) {
	session, err := billingStore.sessionByID(id)
	if err != nil {
		return Session{}, err
	}
	if action == "close" && session.Status == "closed" {
		return session, nil
	}
	pc, ok := routeForSession(session)
	if !ok {
		return Session{}, errors.New("the session's exact system endpoint is not registered; refusing to update another record")
	}
	if localBillingPC(pc) {
		if action == "continue" {
			return continueOwnedSession(session, pc)
		}
		if action == "close" {
			return closeOwnedSession(id, session, pc)
		}
		return billingStore.update(id, action, minutes, amount)
	}
	var updated Session
	payload := struct {
		PCID      string `json:"pc_id"`
		PCAddress string `json:"pc_address"`
		DeviceID  string `json:"device_id"`
		Minutes   int    `json:"minutes"`
		Amount    int    `json:"amount"`
	}{PCID: session.PCID, PCAddress: session.PCAddress, DeviceID: session.DeviceID, Minutes: minutes, Amount: amount}
	if err := callBillingPeerContext(ctx, pc, http.MethodPost, "/internal/api/local/sessions/"+url.PathEscape(id)+"/"+url.PathEscape(action), payload, &updated); err != nil {
		return Session{}, err
	}
	if err := billingStore.mergeSyncedSessions(updated.DeviceID, []Session{updated}); err != nil {
		return Session{}, fmt.Errorf("the owning PC updated the bill, but this dashboard could not save the synchronized record: %w", err)
	}
	return updated, nil
}

func sessionStatusForDashboardContext(ctx context.Context, id string) (Session, error) {
	session, err := billingStore.sessionByID(id)
	if err != nil {
		return Session{}, err
	}
	pc, ok := routeForSession(session)
	if !ok {
		return Session{}, errors.New("the session's exact system endpoint is not registered; refusing to read another record")
	}
	var current Session
	if localBillingPC(pc) {
		current, err = billingStore.sessionByID(id)
	} else {
		path := "/internal/api/local/sessions/" + url.PathEscape(id) + "/status"
		err = callBillingPeerContext(ctx, pc, http.MethodGet, path, nil, &current)
	}
	if err != nil {
		return Session{}, err
	}
	if current.ID != id || !sessionAtPC(current, pc) {
		return Session{}, apiError(http.StatusConflict, errors.New("session status response does not match the requested system"))
	}
	if !localBillingPC(pc) {
		if err := billingStore.mergeSyncedSessions(current.DeviceID, []Session{current}); err != nil {
			return Session{}, fmt.Errorf("the owning PC returned the session status, but this dashboard could not save the synchronized record: %w", err)
		}
	}
	return current, nil
}

// closeOwnedSession stops the owning timer before committing its closed record.
// If the workbook write fails, restore the timer snapshot so the open session remains usable.
func closeOwnedSession(id string, session Session, pc PC) (Session, error) {
	return closeOwnedSessionAt(id, session, pc, time.Time{})
}
func closeOwnedSessionAt(id string, session Session, pc PC, stopped time.Time) (Session, error) {
	billingCloseMu.Lock()
	defer billingCloseMu.Unlock()
	current, err := billingStore.sessionByID(id)
	if err != nil {
		return Session{}, err
	}
	if current.DeviceID != session.DeviceID || !sessionAtPC(current, pc) {
		return Session{}, apiError(http.StatusConflict, errors.New("session ID and owning PC do not match"))
	}
	if current.Status == "closed" {
		return current, nil
	}
	if current.Status != "open" {
		return Session{}, apiError(http.StatusConflict, fmt.Errorf("session cannot be closed with status %q", current.Status))
	}
	pc.Address = billingControlAddress(pc)
	previous, err := getTimerStatus(pc.Address)
	if err != nil {
		return Session{}, apiError(http.StatusBadGateway, fmt.Errorf("could not read timer status for %s before closing: %w", pc.Name, err))
	}
	if !stopped.IsZero() && (current.UpdatedAt != session.UpdatedAt || !shouldAutoCloseTimer(previous)) {
		return Session{}, errors.New("session changed or timer resumed")
	}
	if err := timerAction(pc.Address, "reset", 0); err != nil {
		if restoreErr := restoreTimerStatus(pc.Address, previous); restoreErr != nil {
			return Session{}, apiError(http.StatusBadGateway, fmt.Errorf("could not stop timer for %s: %v; restoring the prior timer state also failed: %w", pc.Name, err, restoreErr))
		}
		return Session{}, apiError(http.StatusBadGateway, fmt.Errorf("could not stop timer for %s; prior timer state was restored: %w", pc.Name, err))
	}
	closed, err := billingStore.updateAtStop(id, "close", 0, 0, stopped)
	if err != nil {
		if restoreErr := restoreTimerStatus(pc.Address, previous); restoreErr != nil {
			return Session{}, apiError(http.StatusInternalServerError, fmt.Errorf("could not save closed session: %v; timer restore also failed: %w", err, restoreErr))
		}
		return Session{}, apiError(http.StatusInternalServerError, fmt.Errorf("could not save closed session; timer was restored: %w", err))
	}
	if err := updateBillingSessionLink(pc, id, true); err != nil {
		logf("billing", "session %s closed on pc_id=%s but timer billing link cleanup failed: %v", id, pc.ID, err)
	}
	return closed, nil
}

func localSessionHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/internal/api/local/sessions" {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
			return
		}
		var request StartSessionRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid session data"})
			return
		}
		pc, ok := store.find(request.PCID)
		if request.Target != nil {
			pc, ok = ownedBillingTimer(request.Target)
		} else if ok {
			request.Target = targetForPC(pc)
		}
		if !ok || !localBillingPC(pc) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session endpoint is not owned by this billing device"})
			return
		}
		session, err := startBillingSession(request)
		if err != nil {
			writeBillingError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, session)
		return
	}
	if r.URL.Path == "/internal/api/local/sessions/extend" {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
			return
		}
		var request ExtendSessionRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid extension request"})
			return
		}
		pc, ok := store.find(request.PCID)
		if !ok && request.PCAddress != "" {
			pc, ok = registeredPCByAddress(request.PCAddress)
		}
		if request.Target != nil {
			pc, ok = ownedBillingTimer(request.Target)
		}
		if !ok || !localBillingPC(pc) || (request.Target == nil && request.PCAddress != "" && normalizeSessionAddress(request.PCAddress) != normalizeSessionAddress(pc.Address)) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session endpoint is not owned by this billing device"})
			return
		}
		response, err := extendSessionForSystem(request, true)
		if err != nil {
			writeBillingError(w, err)
			return
		}
		if response.Session != nil && previousDeviceName(response.Session.DeviceID) {
			response.Session.DeviceID = configuredFriendlyName()
		}
		writeJSON(w, http.StatusOK, response)
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/internal/api/local/sessions/"), "/"), "/")
	if len(parts) == 2 && parts[1] == "status" && r.Method == http.MethodGet {
		w.Header().Set("Cache-Control", "no-store")
		session, err := billingStore.sessionByID(parts[0])
		if err != nil {
			writeBillingError(w, err)
			return
		}
		pc, ok := routeForSession(session)
		if !ok || !localBillingPC(pc) || !sessionAtPC(session, pc) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "session endpoint is not owned by this billing device"})
			return
		}
		writeJSON(w, http.StatusOK, session)
		return
	}
	if len(parts) != 2 || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var body struct {
		PCID      string           `json:"pc_id"`
		PCAddress string           `json:"pc_address"`
		DeviceID  string           `json:"device_id"`
		Edit      *billEdit        `json:"edit"`
		Save      *cardSessionSave `json:"save"`
		Minutes   int              `json:"minutes"`
		Amount    int              `json:"amount"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid session update"})
		return
	}
	current, err := billingStore.sessionByID(parts[0])
	if err != nil {
		writeBillingError(w, err)
		return
	}
	if !(strings.EqualFold(current.DeviceID, configuredFriendlyName()) || previousDeviceName(current.DeviceID)) || !(strings.EqualFold(current.DeviceID, body.DeviceID) || (previousDeviceName(current.DeviceID) && strings.EqualFold(body.DeviceID, configuredFriendlyName()))) || billingPath(current.PCAddress) != billingPath(body.PCAddress) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "session ID, device ID, and system endpoint do not match"})
		return
	}
	var session Session
	if parts[1] == "save" && body.Save != nil {
		pc, ok := routeForSession(current)
		if !ok || !localBillingPC(pc) {
			writeBillingError(w, errors.New("owning timer not found"))
			return
		}
		session, err = saveCardSession(current, pc, *body.Save)
	} else if parts[1] == "edit" && body.Edit != nil {
		session, err = billingStore.editBill(parts[0], *body.Edit)
	} else if parts[1] == "close" || parts[1] == "continue" {
		pc, ok := routeForSession(current)
		if !ok || !localBillingPC(pc) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "session endpoint is not owned by this billing device"})
			return
		}
		if parts[1] == "continue" {
			session, err = continueOwnedSession(current, pc)
		} else {
			session, err = closeOwnedSession(parts[0], current, pc)
		}
	} else {
		session, err = billingStore.update(parts[0], parts[1], body.Minutes, body.Amount)
	}
	if err != nil {
		writeBillingError(w, err)
		return
	}
	if previousDeviceName(session.DeviceID) {
		session.DeviceID = configuredFriendlyName()
	}
	writeJSON(w, http.StatusOK, session)
}

func extendSessionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req ExtendSessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid extension request"})
		return
	}
	resp, err := extendSessionForDashboardContext(r.Context(), req)
	if err != nil {
		writeBillingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func internalExtensionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	var req ExtendSessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid extension request"})
		return
	}
	pc, ok := registeredPCByAddress(req.PCAddress)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "extension endpoint is not registered"})
		return
	}
	req.PCID = pc.ID
	resp, err := extendSessionForSystem(req, true)
	if err != nil {
		writeBillingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func registerBillingHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/pricing", pricingHandler)
	mux.HandleFunc("/api/pricing/distribute", distributePricing)
	mux.HandleFunc("/api/billing/summary", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, billingStore.summary()) })
	mux.HandleFunc("/api/billing/download", downloadBilling)
	mux.HandleFunc("/api/billing/sync", billingSyncHandler)
	mux.HandleFunc("/api/billing/report", billingReportHandler)
	mux.HandleFunc("/api/billing/analytics", billingAnalyticsHandler)
	mux.HandleFunc("/api/billing/snacks/no-pc", noPCSnackSaleHandler)
	mux.HandleFunc("/internal/api/billing/sync/pending", billingSyncPendingHandler)
	mux.HandleFunc("/internal/api/billing/sync/ack", billingSyncAckHandler)
	mux.HandleFunc("/internal/api/local/sessions", localSessionHandler)
	mux.HandleFunc("/internal/api/local/sessions/", localSessionHandler)
	mux.HandleFunc("/api/sessions/extend", extendSessionHandler)
	mux.HandleFunc("/internal/api/sessions/extend", internalExtensionHandler)
	mux.HandleFunc("/api/sessions", sessionsHandler)
	mux.HandleFunc("/api/customers", customerNamesHandler)
	mux.HandleFunc("/api/sessions/", sessionActionHandler)
}
func sessionsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		x, err := billingStore.sessions(r.URL.Query().Get("search"), r.URL.Query().Get("payment_status"), r.URL.Query().Get("status"))
		if err != nil {
			writeBillingError(w, err)
			return
		}
		writeJSON(w, 200, x)
	case http.MethodPost:
		var q StartSessionRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&q); err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid session data"})
			return
		}
		x, err := startBillingSessionForDashboardContext(r.Context(), q)
		if err != nil {
			writeBillingError(w, err)
			return
		}
		writeJSON(w, 201, x)
	default:
		http.Error(w, "GET or POST required", 405)
	}
}

func customerNamesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET required"})
		return
	}
	names, err := billingStore.customerNames()
	if err != nil {
		writeBillingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, names)
}

func sessionActionHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/sessions/extend" {
		extendSessionHandler(w, r)
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/sessions/"), "/"), "/")
	if len(parts) == 1 && parts[0] == "extend" {
		extendSessionHandler(w, r)
		return
	}
	if len(parts) == 2 && parts[1] == "status" && r.Method == http.MethodGet {
		w.Header().Set("Cache-Control", "no-store")
		session, err := sessionStatusForDashboardContext(r.Context(), parts[0])
		if err != nil {
			writeBillingError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"id": session.ID, "status": session.Status, "updated_at": session.UpdatedAt,
		})
		return
	}
	if len(parts) != 2 || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if parts[1] == "save" {
		var save cardSessionSave
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&save); err != nil {
			writeBillingError(w, err)
			return
		}
		updated, err := saveCardSessionForDashboard(r.Context(), parts[0], save)
		if err != nil {
			writeBillingError(w, err)
			return
		}
		writeJSON(w, 200, updated)
		return
	}
	if parts[1] == "edit" {
		var edit billEdit
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&edit); err != nil {
			writeBillingError(w, err)
			return
		}
		updated, err := editBillForDashboard(r.Context(), parts[0], edit)
		if err != nil {
			writeBillingError(w, err)
			return
		}
		writeJSON(w, 200, updated)
		return
	}
	var body struct {
		Minutes int `json:"minutes"`
		Amount  int `json:"amount"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid session update"})
		return
	}
	x, err := updateSessionForDashboardContext(r.Context(), parts[0], parts[1], body.Minutes, body.Amount)
	if err != nil {
		writeBillingError(w, err)
		return
	}
	writeJSON(w, 200, x)
}
func downloadBilling(w http.ResponseWriter, r *http.Request) {
	billingStore.mu.Lock()
	defer billingStore.mu.Unlock()
	if err := billingStore.require(); err != nil {
		writeBillingError(w, err)
		return
	}
	day := r.URL.Query().Get("date")
	if day == "" {
		day = time.Now().In(billingTimezone).Format(time.DateOnly)
	}
	if _, err := time.Parse(time.DateOnly, day); err != nil {
		http.Error(w, "invalid date", 400)
		return
	}
	ledger, err := billingStore.openLocked()
	if err != nil {
		writeBillingError(w, err)
		return
	}
	var csvData bytes.Buffer
	if err := writeDayCSV(&csvData, day, ledger.sessions); err != nil {
		writeBillingError(w, err)
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename="+day+".csv")
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	if _, err := w.Write(csvData.Bytes()); err != nil {
		logf("billing", "could not write CSV export for %s: %v", day, err)
	}
}
func writeBillingError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	var httpErr *billingHTTPError
	if errors.As(err, &httpErr) {
		status = httpErr.Status
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// Only the owning process closes a bill. A missing timer or failed status
// request breaks the observation period and cannot trigger a close.
func shouldAutoCloseTimer(status TimerStatus) bool {
	return status.RemainingSeconds == 0
}

func watchStoppedSessions() {
	type observation struct {
		since   time.Time
		version string
	}
	observed := map[string]observation{}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	checkRecovered := true
	for range ticker.C {
		billingSessionLinks.RLock()
		hasLinks := len(billingSessionLinks.items) > 0
		billingSessionLinks.RUnlock()
		if !hasLinks && !checkRecovered {
			continue
		}
		sessions, err := billingStore.sessions("", "", "open")
		if err != nil {
			clear(observed)
			continue
		}
		next := map[string]observation{}
		checkRecovered = false
		for _, session := range sessions {
			pc, ok := ownedBillingTimer(&billingTarget{Owner: session.DeviceID, Path: billingPath(session.PCAddress)})
			if !ok || !sessionAtPC(session, pc) {
				continue
			}
			checkRecovered = true
			status, err := getTimerStatus(billingControlAddress(pc))
			if err != nil || !shouldAutoCloseTimer(status) {
				continue
			}
			now := time.Now()
			sample, ok := observed[session.ID]
			if !ok || sample.version != session.UpdatedAt {
				sample = observation{now, session.UpdatedAt}
			}
			if now.Sub(sample.since) >= 30*time.Second {
				if _, err := closeOwnedSessionAt(session.ID, session, pc, sample.since); err == nil {
					continue
				}
			}
			next[session.ID] = sample
		}
		observed = next
	}
}
