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
	"strings"
	"time"
)

var dailyHeader = append(slices.Clone(csvSessionFields), "PC name (owner)", "Synced version", "Time spent seconds", "Manual price", "Payments", "Time corrected", "Continued bill", "Snacks")

type dailyLedger struct {
	sessions     []Session
	changed      map[string]bool
	changedPaths map[string]bool
	sourcePaths  []string
	root         string
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
	if f.changed == nil {
		f.changed = make(map[string]bool)
	}
	if f.changedPaths == nil {
		f.changedPaths = make(map[string]bool)
	}
	if i == len(f.sessions) {
		f.sessions = append(f.sessions, x)
		path := ""
		if f.root != "" {
			path = billingPCDayPath(f.root, x.PCName, day)
		}
		f.sourcePaths = append(f.sourcePaths, path)
		f.changed[day] = true
		if path != "" {
			f.changedPaths[path] = true
		}
	} else {
		old, _ := billingDay(f.sessions[i])
		if old != day {
			return errors.New("session start date cannot change")
		}
		for len(f.sourcePaths) <= i {
			f.sourcePaths = append(f.sourcePaths, "")
		}
		oldPath := f.sourcePaths[i]
		path := oldPath
		if f.root != "" {
			if path == "" {
				path = filepath.Join(f.root, day+".csv")
			} else if f.sessions[i].PCName != x.PCName {
				path = billingPCDayPath(f.root, x.PCName, day)
			}
			if oldPath != "" && sameBillingPath(oldPath, path) {
				path = oldPath
			}
		}
		f.sessions[i] = x
		if oldPath != path {
			if oldPath != "" {
				f.changedPaths[oldPath] = true
			}
			f.sourcePaths[i] = path
		}
		if path != "" {
			f.changedPaths[path] = true
		}
		f.changed[day] = true
	}
	return nil
}

func billingPCFolderName(name string) string {
	var safe strings.Builder
	for _, char := range strings.TrimSpace(name) {
		if char < 32 || strings.ContainsRune(`<>:"/\|?*`, char) {
			safe.WriteByte('_')
		} else {
			safe.WriteRune(char)
		}
	}
	folder := strings.TrimRight(strings.TrimSpace(safe.String()), " .")
	if folder == "" || folder == "." || folder == ".." {
		folder = "Unassigned"
	}
	base := strings.ToUpper(strings.SplitN(folder, ".", 2)[0])
	switch base {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		folder = "_" + folder
	}
	return folder
}

func billingPCDayPath(root, name, day string) string {
	return filepath.Join(root, billingPCFolderName(name), day+".csv")
}

func sameBillingPath(left, right string) bool {
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}

func (s *BillingStore) openLocked() (*dailyLedger, error) {
	f := &dailyLedger{
		changed: make(map[string]bool), changedPaths: make(map[string]bool), root: s.path,
	}
	files, err := os.ReadDir(s.path)
	if err != nil {
		return nil, err
	}
	for _, entry := range files {
		name := entry.Name()
		if entry.IsDir() {
			pcPath := filepath.Join(s.path, name)
			pcFiles, err := os.ReadDir(pcPath)
			if err != nil {
				return nil, err
			}
			for _, pcFile := range pcFiles {
				if pcFile.IsDir() || filepath.Ext(pcFile.Name()) != ".csv" {
					continue
				}
				if _, err := time.Parse("2006-01-02.csv", pcFile.Name()); err != nil {
					continue
				}
				path := filepath.Join(pcPath, pcFile.Name())
				if err := f.readDay(path, pcFile.Name()[:10]); err != nil {
					return nil, fmt.Errorf("%s: %w", path, err)
				}
			}
			continue
		}
		if filepath.Ext(name) != ".csv" {
			continue
		}
		if _, err := time.Parse("2006-01-02.csv", name); err != nil {
			continue
		}
		path := filepath.Join(s.path, name)
		if err := f.readDay(path, name[:10]); err != nil {
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
		f.sourcePaths = append(f.sourcePaths, path)
	}
}

func (s *BillingStore) readDayLocked(day string) (*dailyLedger, error) {
	f := &dailyLedger{
		changed: make(map[string]bool), changedPaths: make(map[string]bool), root: s.path,
	}
	read := func(path string) error {
		if err := f.readDay(path, day); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := read(filepath.Join(s.path, day+".csv")); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.path)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(s.path, entry.Name(), day+".csv")
		if err := read(path); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return f, nil
}

func (s *BillingStore) saveLocked(f *dailyLedger) error {
	if f.root != "" && len(f.changedPaths) > 0 {
		paths := make([]string, 0, len(f.changedPaths))
		for path := range f.changedPaths {
			paths = append(paths, path)
		}
		slices.Sort(paths)
		for _, path := range paths {
			day := strings.TrimSuffix(filepath.Base(path), ".csv")
			if _, err := time.Parse(time.DateOnly, day); err != nil {
				return fmt.Errorf("invalid billing CSV path %q", path)
			}
			if err := writeDayForSource(path, day, f); err != nil {
				return err
			}
		}
		return nil
	}
	for day := range f.changed {
		path := filepath.Join(s.path, day+".csv")
		if err := writeDay(path, day, f.sessions); err != nil {
			return err
		}
	}
	return nil
}

func writeDayForSource(path, day string, f *dailyLedger) error {
	sessions := make([]Session, 0)
	for i, session := range f.sessions {
		if i < len(f.sourcePaths) && sameBillingPath(f.sourcePaths[i], path) {
			sessions = append(sessions, session)
		}
	}
	return writeDay(path, day, sessions)
}

func writeDay(path, day string, sessions []Session) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".billing-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := writeDayCSV(file, day, sessions); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func writeDayCSV(output io.Writer, day string, sessions []Session) error {
	w := csv.NewWriter(output)
	if err := w.Write(dailyHeader); err != nil {
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
		if err := w.Write(row); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
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
	if f.root == "" {
		f.root = s.path
	}
	for len(f.sourcePaths) <= index {
		f.sourcePaths = append(f.sourcePaths, "")
	}
	oldPath := f.sourcePaths[index]
	if oldPath == "" {
		oldPath = filepath.Join(s.path, from+".csv")
		f.sourcePaths[index] = oldPath
	}
	newPath := oldPath
	if sameBillingPath(filepath.Dir(oldPath), s.path) {
		if old.PCName != x.PCName {
			newPath = billingPCDayPath(s.path, x.PCName, to)
		} else {
			newPath = filepath.Join(s.path, to+".csv")
		}
	} else {
		newPath = billingPCDayPath(s.path, x.PCName, to)
	}
	before := slices.Clone(f.sessions)
	beforePaths := slices.Clone(f.sourcePaths)
	f.sessions[index] = x
	f.sourcePaths[index] = newPath
	if f.changed == nil {
		f.changed = make(map[string]bool)
	}
	if f.changedPaths == nil {
		f.changedPaths = make(map[string]bool)
	}
	f.changedPaths[oldPath] = true
	f.changedPaths[newPath] = true
	f.changed[from] = true
	f.changed[to] = true
	if err = s.saveLocked(f); err != nil {
		f.sessions[index] = old
		f.sourcePaths = beforePaths
		snapshot := &dailyLedger{sessions: before, sourcePaths: beforePaths}
		rollbackErr := errors.Join(
			writeDayForSource(oldPath, from, snapshot),
			writeDayForSource(newPath, to, snapshot),
		)
		return errors.Join(err, rollbackErr)
	}
	return nil
}
