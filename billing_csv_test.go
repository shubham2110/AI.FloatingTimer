//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDailyCSVPartitionsAndPostPaymentCorrection(t *testing.T) {
	root := t.TempDir()
	original := appConfig.FriendlyName
	appConfig.FriendlyName = "Gaming PC"
	defer func() { appConfig.FriendlyName = original }()
	var store BillingStore
	if err := store.open(BillingConfig{Enabled: true, Directory: root}); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 10, 31, 23, 59, 0, 0, billingTimezone)
	rates := []PriceEntry{{Category: "Console", Players: 1, DurationMinutes: 15, Amount: 30}, {Category: "Console", Players: 1, DurationMinutes: 60, Amount: 90}}
	for i, id := range []string{"a", "b"} {
		session := Session{ID: id, PCID: id, PCName: "Console " + id, Status: "open", Category: "Console", Players: 1, CustomerName: "Guest, One", PurchasedMinutes: 15, PricingSnapshot: pricingSnapshot(rates[0], rates)}
		if _, err := store.commitStart(session, day.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	files, err := filepath.Glob(filepath.Join(root, "*", "*.csv"))
	if err != nil || len(files) != 2 {
		t.Fatalf("daily files %v %v", files, err)
	}
	other, err := os.ReadFile(files[1])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.update("a", "correct", 75, 100); err != nil {
		t.Fatal(err)
	}
	got, err := store.sessionByID("a")
	if err != nil || got.CustomerName != "Guest, One" || got.DeviceID != "Gaming PC" || got.ActualDurationSeconds != 4500 || got.AmountPaid != 100 || got.FinalAmount != 113 {
		t.Fatalf("reloaded %+v, %v", got, err)
	}
	after, err := os.ReadFile(files[1])
	if err != nil || string(after) != string(other) {
		t.Fatal("correction changed the other day")
	}
}
