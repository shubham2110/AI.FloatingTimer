//go:build windows

package main

import "testing"

func TestCardSessionStartsWithoutCustomerAndSavesSameTimer(t *testing.T) {
	configureBillingTest(t)
	server, mu, _, timer := newBillingTimerServer(t, false)
	pc := PC{ID: "card", Name: "Card", Address: server.URL + "/1", PricingGroup: "PC Pro"}
	setBillingTestPCs(pc)
	original, err := startBillingSession(StartSessionRequest{PCID: pc.ID, Category: "PC Pro", DurationMinutes: 2, DurationSeconds: 90, AllowIncomplete: true})
	if err != nil {
		t.Fatal(err)
	}
	if original.CustomerName != "" || original.Players != 0 || original.Status != "open" || original.AmountPaid != 0 {
		t.Fatalf("start %+v", original)
	}
	mu.Lock()
	seconds := timer.RemainingSeconds
	mu.Unlock()
	if seconds != 90 {
		t.Fatalf("timer started with %d seconds", seconds)
	}
	updated, err := saveCardSession(original, pc, cardSessionSave{Customer: "Alex", Players: 1, Category: "PC Pro", Mode: "tweak", Seconds: 300, Version: original.UpdatedAt})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := billingStore.sessions("", "")
	if err != nil || len(rows) != 1 || updated.ID != original.ID || rows[0].CustomerName != "Alex" || rows[0].AmountPaid != 0 || rows[0].PurchasedMinutes != 7 {
		t.Fatalf("saved %+v %v", rows, err)
	}
	mu.Lock()
	seconds = timer.RemainingSeconds
	mu.Unlock()
	if seconds != 390 {
		t.Fatalf("timer saved with %d seconds", seconds)
	}
}

func TestContinueOnlyLatestClosedSessionAndSaveRestartsCountdown(t *testing.T) {
	configureBillingTest(t)
	server, mu, _, timer := newBillingTimerServer(t, false)
	pc := PC{ID: "card", Name: "Card", Address: server.URL + "/1", PricingGroup: "PC Pro"}
	setBillingTestPCs(pc)
	start := func() Session {
		x, err := startBillingSession(StartSessionRequest{PCID: pc.ID, Category: "PC Pro", CustomerName: "Alex", Players: 1, DurationMinutes: 15, AllowIncomplete: true})
		if err != nil {
			t.Fatal(err)
		}
		closed, err := closeOwnedSession(x.ID, x, pc)
		if err != nil {
			t.Fatal(err)
		}
		return closed
	}
	old, latest := start(), start()

	if _, err := continueOwnedSession(old, pc); err == nil {
		t.Fatal("continued an older bill")
	}
	current, err := continueOwnedSession(latest, pc)
	if err != nil || current.ID != latest.ID || current.Status != "open" {
		t.Fatalf("continued %+v %v", current, err)
	}
	mu.Lock()
	remaining, running := timer.RemainingSeconds, timer.IsRunning
	mu.Unlock()
	if remaining != 0 || running {
		t.Fatal("continued session should wait for more time")
	}
	updated, err := saveCardSession(current, pc, cardSessionSave{Customer: "Alex", Players: 1, Category: "PC Pro", Mode: "tweak", Seconds: 1800, Version: current.UpdatedAt})
	quote, _, quoteErr := cardPrice(pc, "PC Pro", 1, 30)
	if err != nil || quoteErr != nil || updated.FinalAmount != latest.FinalAmount+quote.Amount || updated.Continued == nil {
		t.Fatalf("continued charge %+v %v", updated, err)
	}
	mu.Lock()
	remaining, running = timer.RemainingSeconds, timer.IsRunning
	mu.Unlock()
	if remaining != 1800 || !running {
		t.Fatalf("signed addition from zero got %d running=%t", remaining, running)
	}
	current = updated
	manual := 75
	updated, err = saveCardSession(current, pc, cardSessionSave{Customer: "Alex", Players: 1, Category: "PC Pro", Mode: "set", Seconds: 300, Version: current.UpdatedAt, SetManual: true, Manual: &manual})
	if err != nil || updated.FinalAmount != 75 {
		t.Fatalf("saved %+v %v", updated, err)
	}
	mu.Lock()
	remaining, running = timer.RemainingSeconds, timer.IsRunning
	mu.Unlock()
	if remaining != 300 || !running {
		t.Fatalf("timer got %d running=%t", remaining, running)
	}
}

func TestZeroDurationStartsPausedAndSaveAddsTime(t *testing.T) {
	configureBillingTest(t)
	server, mu, _, timer := newBillingTimerServer(t, false)
	pc := PC{ID: "zero-card", Name: "Zero", Address: server.URL + "/1", PricingGroup: "PC Pro"}
	setBillingTestPCs(pc)
	started, err := startBillingSession(StartSessionRequest{PCID: pc.ID, Category: "PC Pro", Players: 1, AllowIncomplete: true})
	if err != nil || started.FinalAmount != 0 || started.Status != "open" {
		t.Fatalf("start %+v %v", started, err)
	}
	mu.Lock()
	remaining, running := timer.RemainingSeconds, timer.IsRunning
	mu.Unlock()
	if remaining != 0 || running {
		t.Fatalf("timer got %d running=%t", remaining, running)
	}
	_, err = saveCardSession(started, pc, cardSessionSave{Customer: "Alex", Players: 1, Category: "PC Pro", Mode: "set", Seconds: 300, Version: started.UpdatedAt})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	remaining, running = timer.RemainingSeconds, timer.IsRunning
	mu.Unlock()
	if remaining != 300 || !running {
		t.Fatalf("timer got %d running=%t", remaining, running)
	}
}

func TestMinuteRateRoundsFinalChargeToNearestRupee(t *testing.T) {
	for _, test := range []struct{ rate, minutes, want int }{{167, 60, 100}, {167, 30, 50}, {150, 1, 2}, {101, 60, 61}} {
		entries := []PriceEntry{{Category: "Screen", Players: 1, DurationMinutes: 1, MinutePaise: &test.rate}}
		price, err := priceAt(entries, "Screen", 1, test.minutes)
		if err != nil || price.Amount != test.want {
			t.Errorf("%d paise x %d minutes: got %d, want %d (%v)", test.rate, test.minutes, price.Amount, test.want, err)
		}
	}
}
