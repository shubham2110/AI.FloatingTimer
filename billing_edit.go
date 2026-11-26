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

type billEdit struct {
	PaymentOnly bool   `json:"payment_only,omitempty"`
	Snacks      *int   `json:"snacks,omitempty"`
	Remove      bool   `json:"remove"`
	Customer    string `json:"customer_name"`
	PCID        string `json:"pc_id"`
	PCName      string `json:"pc_name"`
	PCAddress   string `json:"pc_address"`
	Category    string `json:"category"`
	Players     int    `json:"players"`
	Started     string `json:"started_at"`
	Ended       string `json:"ended_at"`
	Purchased   int    `json:"purchased_minutes"`
	Additional  int    `json:"additional_minutes"`
	Spent       int    `json:"spent_minutes"`
	Billed      int    `json:"final_amount"`
	Paid        int    `json:"amount_paid"`
	PaymentAt   string `json:"payment_at"`
	Version     string `json:"updated_at"`
}

func (s *BillingStore) editBill(id string, e billEdit) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.require(); err != nil {
		return Session{}, err
	}
	ledger, err := s.openLocked()
	if err != nil {
		return Session{}, err
	}
	for i, old := range ledger.sessions {
		if old.ID != id {
			continue
		}
		if old.UpdatedAt != e.Version {
			return Session{}, apiError(http.StatusConflict, errors.New("bill changed; reopen it to edit the latest version"))
		}
		if old.Status == "removed" {
			return Session{}, errors.New("bill already removed")
		}
		if e.PaymentOnly {
			if e.Paid < 0 || e.Paid > 9999999 {
				return Session{}, errors.New("enter a valid amount paid in whole rupees")
			}
			if old.AmountPaid == e.Paid {
				return old, nil
			}
			now := billingSyncTime()
			old.payments = append(old.payments, paymentRecord{ID: "C-" + now, Timestamp: now, Amount: e.Paid - old.AmountPaid, Status: "correction"})
			old.AmountPaid = e.Paid
			old.PaymentAt = now
			old.PaymentStatus = "unpaid"
			if e.Paid >= old.FinalAmount {
				old.PaymentStatus = "paid"
			} else if e.Paid > 0 {
				old.PaymentStatus = "partial"
			}
			old.UpdatedAt = now
			old.SyncedUpdatedAt = ""
			if err := ledger.put(i, old); err != nil {
				return Session{}, err
			}
			return old, s.saveLocked(ledger)
		}
		if e.Remove {
			if old.Status == "open" {
				return Session{}, errors.New("close the session before removing its bill")
			}
			old.Status = "removed"
			old.UpdatedAt = billingSyncTime()
			old.SyncedUpdatedAt = ""
			if err := ledger.put(i, old); err != nil {
				return Session{}, err
			}
			return old, s.saveLocked(ledger)
		}
		e.Customer = strings.TrimSpace(e.Customer)
		e.PCName = strings.TrimSpace(e.PCName)
		e.Category = strings.TrimSpace(e.Category)
		if len(e.Customer) > 120 || e.PCName == "" || len(e.PCName) > 120 || len(e.Category) > 120 || e.Players < 0 || e.Players > 4 {
			return Session{}, errors.New("enter a valid customer, PC, category and player count")
		}
		for _, minutes := range []int{e.Purchased, e.Additional, e.Spent} {
			if minutes < 0 || minutes > 525600 {
				return Session{}, errors.New("enter valid session durations")
			}
		}
		if e.Paid < 0 || e.Paid > 9999999 || e.Billed < 0 || e.Billed > 9999999 {
			return Session{}, errors.New("enter valid amounts in whole rupees")
		}
		start, err := sessionTimestamp(e.Started)
		if err != nil {
			return Session{}, err
		}
		end := ""
		if old.Status == "closed" {
			finish, err := sessionTimestamp(e.Ended)
			if err != nil || finish.Before(start) {
				return Session{}, errors.New("the end time must be at or after the start time")
			}
			end = finish.In(billingTimezone).Format(time.RFC3339Nano)
		}
		if old.Status == "open" && (e.PCID != old.PCID || e.PCAddress != old.PCAddress) {
			return Session{}, apiError(http.StatusConflict, errors.New("close the live session before reassigning it to another timer"))
		}
		x := old
		x.CustomerName, x.PCName, x.Category, x.Players = e.Customer, e.PCName, e.Category, e.Players
		x.PCID, x.PCAddress = e.PCID, e.PCAddress
		x.StartedAt, x.EndedAt = start.In(billingTimezone).Format(time.RFC3339Nano), end
		x.ExpectedEndAt = start.Add(time.Duration(e.Purchased+e.Additional) * time.Minute).In(billingTimezone).Format(time.RFC3339Nano)
		x.PurchasedMinutes, x.AdditionalMinutes, x.ActualDurationSeconds = e.Purchased, e.Additional, e.Spent*60
		if e.Spent == (old.ActualDurationSeconds+59)/60 {
			x.ActualDurationSeconds = old.ActualDurationSeconds
		}
		x.TimeCorrected = x.TimeCorrected || old.ActualDurationSeconds != x.ActualDurationSeconds
		x.FinalAmount, x.AmountPaid = e.Billed, e.Paid
		if e.Snacks != nil {
			x.Snacks = *e.Snacks
		}
		if x.Snacks < 0 || x.Snacks > e.Billed {
			return Session{}, errors.New("snacks must be between zero and the bill total")
		}
		gameAmount := e.Billed - x.Snacks
		if old.FinalAmount-old.Snacks != gameAmount {
			x.ManualPrice = &gameAmount
		}
		if (x.Category != old.Category || x.Players != old.Players) && x.Status == "open" && x.Players > 0 && x.Category != "" {
			rates := pricingCatalog().Entries
			price, err := priceAt(rates, x.Category, x.Players, max(1, e.Purchased))
			if err != nil {
				return Session{}, err
			}
			x.PricingSnapshot = pricingSnapshot(price, rates)
		}
		x.PaymentStatus = "unpaid"
		if x.AmountPaid >= x.FinalAmount {
			x.PaymentStatus = "paid"
		} else if x.AmountPaid > 0 {
			x.PaymentStatus = "partial"
		}
		x.UpdatedAt = billingSyncTime()
		x.SyncedUpdatedAt = ""
		if e.PaymentAt != "" {
			payment, err := sessionTimestamp(e.PaymentAt)
			if err != nil {
				return Session{}, err
			}
			x.PaymentAt = payment.In(billingTimezone).Format(time.RFC3339Nano)
		} else {
			x.PaymentAt = ""
		}
		if old.AmountPaid != x.AmountPaid {
			if x.PaymentAt == "" {
				x.PaymentAt = x.UpdatedAt
			}
			x.payments = append(x.payments, paymentRecord{ID: "C-" + x.UpdatedAt, Timestamp: x.PaymentAt, Amount: x.AmountPaid - old.AmountPaid, Status: "correction"})
		} else if old.PaymentAt != x.PaymentAt && len(x.payments) > 0 {
			timestamp := x.PaymentAt
			if timestamp == "" {
				timestamp = x.StartedAt
			}
			for i := len(x.payments) - 1; i >= 0; i-- {
				if x.payments[i].Amount != 0 {
					x.payments[i].Timestamp = timestamp
					break
				}
			}
		}
		if err := s.saveBillEdit(ledger, i, x); err != nil {
			return Session{}, err
		}
		return x, nil
	}
	return Session{}, errors.New("session not found")
}

func billingRecordOwner(s Session) (PC, bool) {
	if strings.EqualFold(s.DeviceID, configuredFriendlyName()) || previousDeviceName(s.DeviceID) {
		return localRegistryPC(), true
	}
	for _, pc := range store.list() {
		if !pc.Missing && billingPath(pc.Address) == "" && (strings.EqualFold(s.DeviceID, pc.Owner) || strings.EqualFold(s.DeviceID, pc.Name)) {
			return pc, true
		}
	}
	return routeForSession(s)
}

func editBillForDashboard(ctx context.Context, id string, e billEdit) (Session, error) {
	session, err := billingStore.sessionByID(id)
	if err != nil {
		return Session{}, err
	}
	if e.PCID == session.PCID {
		e.PCAddress = session.PCAddress
	} else {
		pc, ok := store.find(e.PCID)
		if !ok {
			return Session{}, errors.New("select a registered PC")
		}
		e.PCAddress = pc.Address

	}
	owner, ok := billingRecordOwner(session)
	if !ok {
		return Session{}, errors.New("billing owner is unavailable")
	}
	if localBillingPC(owner) {
		return billingStore.editBill(id, e)
	}
	var updated Session
	payload := struct {
		PCAddress string   `json:"pc_address"`
		DeviceID  string   `json:"device_id"`
		Edit      billEdit `json:"edit"`
	}{session.PCAddress, session.DeviceID, e}
	if err := callBillingPeerContext(ctx, owner, http.MethodPost, "/internal/api/local/sessions/"+url.PathEscape(id)+"/edit", payload, &updated); err != nil {
		return Session{}, err
	}
	if err := billingStore.mergeSyncedSessions(updated.DeviceID, []Session{updated}); err != nil {
		return Session{}, err
	}
	return updated, nil
}
