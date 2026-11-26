//go:build windows

package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"
)

var dailyHeader = append(slices.Clone(csvSessionFields), "PC name (owner)", "Synced version", "Time spent seconds", "Manual price", "Payments", "Time corrected", "Continued bill", "Snacks")

type dailyLedger struct {
	sessions []Session
	changed  map[string]bool
}
type storedBillingSession struct {
	Row     int
	Session Session
}

func (f *dailyLedger) indexed() []storedBillingSession {
	rows := make([]storedBillingSession, len(f.sessions))
	for i, x := range f.sessions {
		rows[i] = storedBillingSession{i, x}
	}
	return rows
}
func billingDay(x Session) (string, error) {
	start, err := sessionTimestamp(x.StartedAt)
	return start.In(billingTimezone).Format(time.DateOnly), err
}
func (f *dailyLedger) put(i int, x Session) error {
	day, err := billingDay(x)
	if err != nil {
		return err
	}
	if i == len(f.sessions) {
		f.sessions = append(f.sessions, x)
	} else {
		old, _ := billingDay(f.sessions[i])
		if old != day {
			return errors.New("session start date cannot change")
		}
		f.sessions[i] = x
	}
	f.changed[day] = true
	return nil
}
func (s *BillingStore) openLocked() (*dailyLedger, error) {
	f := &dailyLedger{changed: map[string]bool{}}
	files, err := os.ReadDir(s.path)
	if err != nil {
		return nil, err
	}
	for _, entry := range files {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".csv" {
			continue
		}
		if _, err := time.Parse("2006-01-02.csv", name); err != nil {
			continue
		}
		if err := f.readDay(filepath.Join(s.path, name), name[:10]); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	return f, nil
}
func (f *dailyLedger) readDay(path, day string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	reader := csv.NewReader(file)
	header, err := reader.Read()
	if err != nil {
		return err
	}
	if len(header) < len(dailyHeader)-2 || len(header) > len(dailyHeader) || !slices.Equal(header, dailyHeader[:len(header)]) {
		return errors.New("invalid CSV header")
	}
	for {
		r, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, i := range []int{6, 11, 12, 14, 15, 16, 23} {
			if _, err := strconv.Atoi(r[i]); err != nil {
				return fmt.Errorf("invalid %s: %w", dailyHeader[i], err)
			}
		}
		x := parseSession(r)
		if len(r) > 28 {
			x.Snacks, err = strconv.Atoi(r[28])
			if err != nil {
				return err
			}
		}
		if len(r) > 27 && r[27] != "" {
			if err = json.Unmarshal([]byte(r[27]), &x.Continued); err != nil {
				return err
			}
		}
		x.DeviceID = r[21]
		x.SyncedUpdatedAt = r[22]
		x.ActualDurationSeconds, _ = strconv.Atoi(r[23])
		if r[24] != "" {
			v, err := strconv.Atoi(r[24])
			if err != nil {
				return err
			}
			x.ManualPrice = &v
		}
		if err = json.Unmarshal([]byte(r[25]), &x.payments); err != nil {
			return err
		}
		for i := len(x.payments) - 1; i >= 0; i-- {
			if x.payments[i].Status == "resume_pending" || x.payments[i].Status == "resumed" {
				x.ResumeMarker = &x.payments[i]
				break
			}
		}
		x.TimeCorrected, err = strconv.ParseBool(r[26])
		if err != nil {
			return err
		}
		date, err := billingDay(x)
		if err != nil || date != day || x.ID == "" {
			return errors.New("invalid session date or ID")
		}
		f.sessions = append(f.sessions, x)
	}
}
func (s *BillingStore) saveLocked(f *dailyLedger) error {
	for day := range f.changed {
		path := filepath.Join(s.path, day+".csv")
		if err := writeDay(path, day, f.sessions); err != nil {
			return err
		}
	}
	return nil
}
func writeDay(path, day string, sessions []Session) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".billing-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	w := csv.NewWriter(file)
	if err = w.Write(dailyHeader); err != nil {
		return err
	}
	for _, x := range sessions {
		d, _ := billingDay(x)
		if d != day {
			continue
		}
		manual := ""
		if x.ManualPrice != nil {
			manual = strconv.Itoa(*x.ManualPrice)
		}
		records := x.payments
		if len(records) == 0 && x.ResumeMarker != nil {
			records = []paymentRecord{*x.ResumeMarker}
		}
		payments, err := json.Marshal(records)
		if err != nil {
			return err
		}
		row := append(rowFor(x), x.DeviceID, x.SyncedUpdatedAt, strconv.Itoa(x.ActualDurationSeconds), manual, string(payments), strconv.FormatBool(x.TimeCorrected))
		continued, err := json.Marshal(x.Continued)
		if err != nil {
			return err
		}
		row = append(row, string(continued), strconv.Itoa(x.Snacks))
		if err = w.Write(row); err != nil {
			return err
		}
	}
	w.Flush()
	if err = w.Error(); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

// Write a moved bill to its new day before removing the original row. If the
// second write fails, restore the destination using the pre-edit ledger.
func (s *BillingStore) saveBillEdit(f *dailyLedger, index int, x Session) error {
	old := f.sessions[index]
	from, err := billingDay(old)
	if err != nil {
		return err
	}
	to, err := billingDay(x)
	if err != nil {
		return err
	}
	if from == to {
		if err = f.put(index, x); err != nil {
			return err
		}
		return s.saveLocked(f)
	}
	before := slices.Clone(f.sessions)
	f.sessions[index] = x
	destination := filepath.Join(s.path, to+".csv")
	if err = writeDay(destination, to, f.sessions); err != nil {
		return err
	}
	if err = writeDay(filepath.Join(s.path, from+".csv"), from, f.sessions); err != nil {
		rollbackErr := writeDay(destination, to, before)
		f.sessions[index] = old
		return errors.Join(err, rollbackErr)
	}
	return nil
}
