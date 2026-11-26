//go:build windows

package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type cardSessionSave struct {
	Customer  string `json:"customer_name"`
	Players   int    `json:"players"`
	Category  string `json:"category"`
	Mode      string `json:"timer_mode,omitempty"`
	Seconds   int    `json:"seconds,omitempty"`
	Manual    *int   `json:"manual_price"`
	SetManual bool   `json:"set_manual_price"`
	Version   string `json:"updated_at"`
}

func cardPrice(pc PC, category string, players, minutes int) (PriceEntry, []PriceEntry, error) {
	if minutes < 0 || minutes > 525600 {
		return PriceEntry{}, nil, errors.New("invalid session duration")
	}
	var rates []PriceEntry
	found := false
	for _, entry := range pricingCatalog().Entries {
		group := entry.Group
		if group == "" {
			group = entry.Category
		}
		if entry.Category != category || !categoryAllowed(pc, category, group) {
			continue
		}
		found = true
		if entry.Players == players {
			rates = append(rates, entry)
		}
	}
	if category != "" && !found {
		return PriceEntry{}, nil, errors.New("category does not belong to this PC pricing group")
	}
	if category == "" || players == 0 {
		return PriceEntry{}, nil, nil
	}
	price, err := priceAt(rates, category, players, max(1, minutes))
	if minutes == 0 {
		price.Amount = 0
		price.DurationMinutes = 0
	}
	return price, rates, err
}

func saveCardSession(current Session, pc PC, q cardSessionSave) (Session, error) {
	extensionMu.Lock()
	defer extensionMu.Unlock()
	billingCloseMu.Lock()
	defer billingCloseMu.Unlock()
	billingStore.mu.Lock()
	defer billingStore.mu.Unlock()
	if err := billingStore.require(); err != nil {
		return Session{}, err
	}
	f, err := billingStore.openLocked()
	if err != nil {
		return Session{}, err
	}
	for i, x := range f.sessions {
		if x.ID != current.ID {
			continue
		}
		if x.Status != "open" || x.UpdatedAt != q.Version {
			return Session{}, apiError(http.StatusConflict, errors.New("session changed; refresh and save again"))
		}
		if !sessionAtPC(x, pc) {
			return Session{}, errors.New("session does not belong to the selected timer")
		}
		name := strings.TrimSpace(q.Customer)
		if len(name) > 120 || q.Players < 0 || q.Players > 4 {
			return Session{}, errors.New("enter a valid customer name and count")
		}
		if q.Seconds < -86400 || q.Seconds > 86400 || (q.Mode == "set" && q.Seconds < 0) ||
			(q.Mode != "" && q.Mode != "tweak" && q.Mode != "set") {
			return Session{}, errors.New("invalid timer adjustment")
		}
		address := billingControlAddress(pc)
		var previous TimerStatus
		if q.Mode != "" {
			previous, err = getTimerStatus(address)
			if err != nil {
				return Session{}, err
			}
		}
		change := q.Seconds
		if q.Mode == "set" {
			change = q.Seconds - previous.RemainingSeconds
		}
		if q.Mode == "tweak" {
			change = max(-previous.RemainingSeconds, change)
		}
		start, err := sessionTimestamp(x.StartedAt)
		if err != nil {
			return Session{}, err
		}
		totalSeconds := (x.PurchasedMinutes + x.AdditionalMinutes) * 60
		if expected, parseErr := sessionTimestamp(x.ExpectedEndAt); parseErr == nil && expected.After(start) {
			totalSeconds = int(expected.Sub(start).Seconds())
		}
		if q.Mode != "" {
			totalSeconds = max(0, totalSeconds+change)
		}
		minutes := (totalSeconds + 59) / 60
		chargeMinutes := minutes
		if x.Continued != nil {
			chargeMinutes = max(0, minutes-x.Continued.PriorMinutes)
		}
		price, rates, err := cardPrice(pc, q.Category, q.Players, chargeMinutes)
		if err != nil {
			return Session{}, err
		}
		x.CustomerName, x.Players, x.Category = name, q.Players, q.Category
		if q.Mode != "" {
			x.PurchasedMinutes = max(0, minutes-x.AdditionalMinutes)
			x.ExpectedEndAt = start.Add(time.Duration(totalSeconds) * time.Second).Format(time.RFC3339Nano)
		}
		if q.SetManual {
			if q.Manual != nil && (*q.Manual < 0 || *q.Manual > 9999999) {
				return Session{}, errors.New("enter a valid manual amount")
			}
			x.ManualPrice = q.Manual
		}
		if x.Continued != nil {
			price.Amount += x.Continued.Amount
		}
		x.EstimatedAmount = price.Amount
		x.PricingSnapshot = ""
		if len(rates) > 0 {
			x.PricingSnapshot = pricingSnapshot(price, rates)
		}
		if x.ManualPrice == nil {
			x.FinalAmount = price.Amount
		} else {
			x.FinalAmount = *x.ManualPrice
		}
		x.FinalAmount += x.Snacks
		x.PaymentStatus = "unpaid"
		if x.AmountPaid > 0 {
			x.PaymentStatus = "partial"
			if x.AmountPaid >= x.FinalAmount {
				x.PaymentStatus = "paid"
			}
		}
		x.UpdatedAt = billingSyncTime()
		x.SyncedUpdatedAt = ""
		if q.Mode != "" {
			if err = timerAction(address, q.Mode, q.Seconds); err != nil {
				return Session{}, errors.Join(err, restoreTimerStatus(address, previous))
			}
		}
		if resumed, exists := lastResume(x); exists && resumed.IsZero() && q.Mode != "" && previous.RemainingSeconds+change > 0 {
			if err = timerAction(address, "play", 0); err != nil {
				return Session{}, errors.Join(err, restoreTimerStatus(address, previous))
			}
			x.payments = append(x.payments, paymentRecord{ID: "R-active-" + x.UpdatedAt, Timestamp: x.UpdatedAt, Status: "resumed"})
			x.ResumeMarker = &x.payments[len(x.payments)-1]
		}
		if err = f.put(i, x); err == nil {
			err = billingStore.saveLocked(f)
		}
		if err != nil {
			if q.Mode != "" {
				err = errors.Join(err, restoreTimerStatus(address, previous))
			}
			return Session{}, err
		}
		return x, nil
	}
	return Session{}, errors.New("session not found")
}

func saveCardSessionForDashboard(ctx context.Context, id string, q cardSessionSave) (Session, error) {
	session, err := billingStore.sessionByID(id)
	if err != nil {
		return Session{}, err
	}
	pc, ok := routeForSession(session)
	if !ok {
		return Session{}, errors.New("owning timer not found")
	}
	if localBillingPC(pc) {
		return saveCardSession(session, pc, q)
	}
	payload := struct {
		PCAddress string          `json:"pc_address"`
		DeviceID  string          `json:"device_id"`
		Save      cardSessionSave `json:"save"`
	}{session.PCAddress, session.DeviceID, q}
	var updated Session
	if err = callBillingPeerContext(ctx, pc, http.MethodPost, "/internal/api/local/sessions/"+url.PathEscape(id)+"/save", payload, &updated); err != nil {
		return Session{}, err
	}
	if err = billingStore.mergeSyncedSessions(updated.DeviceID, []Session{updated}); err != nil {
		return Session{}, err
	}
	return updated, nil
}

func lastResume(s Session) (time.Time, bool) {
	for i := len(s.payments) - 1; i >= 0; i-- {
		if s.payments[i].Status == "resume_pending" {
			return time.Time{}, true
		}
		if s.payments[i].Status == "resumed" {
			when, err := sessionTimestamp(s.payments[i].Timestamp)
			return when, err == nil
		}
	}
	return time.Time{}, false
}

func sessionOrder(s Session) time.Time {
	when, err := sessionTimestamp(s.CreatedAt)
	if err != nil {
		when, _ = sessionTimestamp(s.StartedAt)
	}
	return when
}

func continueOwnedSession(session Session, pc PC) (Session, error) {
	billingStartMu.Lock()
	defer billingStartMu.Unlock()
	extensionMu.Lock()
	defer extensionMu.Unlock()
	billingCloseMu.Lock()
	defer billingCloseMu.Unlock()
	billingStore.mu.Lock()
	defer billingStore.mu.Unlock()
	if err := billingStore.require(); err != nil {
		return Session{}, err
	}
	f, err := billingStore.openLocked()
	if err != nil {
		return Session{}, err
	}
	index := -1
	for i, row := range f.sessions {
		if row.ID == session.ID {
			index = i
			session = row
			break
		}
	}
	if index < 0 || session.Status != "closed" || !sessionAtPC(session, pc) {
		return Session{}, apiError(http.StatusConflict, errors.New("only a closed session on its original timer can be continued"))
	}
	if assigned, ok := store.find(session.PCID); ok && !localBillingPC(assigned) && normalizeSessionAddress(assigned.Address) != normalizeSessionAddress(pc.Address) {
		return Session{}, apiError(http.StatusConflict, errors.New("a bill reassigned to another PC cannot resume its original timer"))
	}
	order := sessionOrder(session)
	for _, row := range f.sessions {
		if row.Status == "removed" || row.ID == session.ID || !sessionAtPC(row, pc) {
			continue
		}
		when := sessionOrder(row)
		if row.Status == "open" || when.After(order) || (when.Equal(order) && row.ID > session.ID) {
			return Session{}, apiError(http.StatusConflict, errors.New("only the latest session for this system can be continued"))
		}
	}
	pc.Address = billingControlAddress(pc)
	previous, err := getTimerStatus(pc.Address)
	if err != nil {
		return Session{}, err
	}
	if previous.IsRunning {
		return Session{}, apiError(http.StatusConflict, errors.New("the timer is already running"))
	}
	if err = timerAction(pc.Address, "reset", 0); err != nil {
		return Session{}, errors.Join(err, restoreTimerStatus(pc.Address, previous))
	}
	rollback := func(cause error) error {
		return errors.Join(cause, updateBillingSessionLink(pc, session.ID, true), restoreTimerStatus(pc.Address, previous))
	}
	if err = updateBillingSessionLink(pc, session.ID, false); err != nil {
		return Session{}, rollback(err)
	}
	now := billingSyncTime()
	session.Continued = &continuedBill{Amount: session.FinalAmount - session.Snacks, SpentSeconds: session.ActualDurationSeconds, PriorMinutes: session.PurchasedMinutes + session.AdditionalMinutes}
	session.ManualPrice = nil
	session.Status = "open"
	session.EndedAt = ""
	session.UpdatedAt = now
	session.SyncedUpdatedAt = ""
	session.payments = append(session.payments, paymentRecord{ID: "R-" + now, Timestamp: now, Status: "resume_pending"})
	session.ResumeMarker = &session.payments[len(session.payments)-1]
	if err = f.put(index, session); err == nil {
		err = billingStore.saveLocked(f)
	}
	if err != nil {
		return Session{}, rollback(err)
	}
	return session, nil
}
