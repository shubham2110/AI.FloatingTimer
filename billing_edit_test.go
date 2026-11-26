//go:build windows

package main

import (
	"testing"
	"time"
)

func TestBillEditorUpdatesFieldsAndMovesDailyRecord(t *testing.T) {
	configureBillingTest(t)
	start := time.Date(2026, 11, 26, 15, 0, 0, 0, billingTimezone)
	old, err := billingStore.commitStart(Session{ID: "bill", PCID: "pc1", PCName: "First", PCAddress: "http://desk:18081", Category: "PC Pro", CustomerName: "Before", Players: 1, Status: "closed", EndedAt: start.Add(time.Minute).Format(time.RFC3339), PurchasedMinutes: 30, FinalAmount: 80, AmountPaid: 60}, start)
	if err != nil {
		t.Fatal(err)
	}
	edit := billEdit{Customer: "After", PCID: "pc2", PCName: "Second", PCAddress: "http://other:18081", Category: "Console", Players: 2, Started: start.AddDate(0, 0, 1).Format(time.RFC3339), Ended: start.AddDate(0, 0, 1).Add(time.Hour).Format(time.RFC3339), Purchased: 45, Additional: 15, Spent: 60, Billed: 100, Paid: 90, Version: old.UpdatedAt}
	updated, err := billingStore.editBill(old.ID, edit)
	if err != nil {
		t.Fatal(err)
	}
	if updated.PCName != "Second" || updated.CustomerName != "After" || updated.Category != "Console" || updated.AmountPaid != 90 || updated.ActualDurationSeconds != 3600 || updated.PaymentStatus != "partial" {
		t.Fatalf("updated %+v", updated)
	}
	before, err := billingStore.report("2026-11-26")
	if err != nil || len(before.Records) != 0 {
		t.Fatalf("original day %+v %v", before, err)
	}
	after, err := billingStore.report("2026-11-27")
	if err != nil || len(after.Records) != 1 {
		t.Fatalf("destination %+v %v", after, err)
	}
	if _, err = billingStore.editBill(old.ID, edit); err == nil {
		t.Fatal("accepted stale edit")
	}
}
