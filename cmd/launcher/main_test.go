//go:build windows

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestDeadlineKeepsDownloadInBackground(t *testing.T) {
	finish := make(chan struct{})
	launched := make(chan struct{})
	ended := make(chan error, 1)
	installs := 0
	go func() {
		ended <- run(20*time.Millisecond, func() error { installs++; return nil }, func() error { <-finish; return nil }, func() error { close(launched); return nil })
	}()
	select {
	case <-launched:
	case <-time.After(time.Second):
		t.Fatal("fallback did not launch on deadline")
	}
	select {
	case <-ended:
		t.Fatal("download worker was abandoned")
	default:
	}
	close(finish)
	if err := <-ended; err != nil {
		t.Fatal(err)
	}
	if installs != 1 {
		t.Fatalf("installed after app started: %d", installs)
	}
}

func TestFastUpdateAndOffline(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			installs, launches := 0, 0
			err := run(time.Second, func() error { installs++; return nil }, func() error {
				if failed {
					return fmt.Errorf("offline")
				}
				return nil
			}, func() error { launches++; return nil })
			want := 2
			if failed {
				want = 1
			}
			if err != nil || launches != 1 || installs != want {
				t.Fatalf("err=%v launches=%d installs=%d", err, launches, installs)
			}
		})
	}
}

func fixture(t *testing.T) ([]byte, asset) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := digestFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	return b, asset{Name: "OverlayTimer.exe", Size: int64(len(b)), Digest: digest, URL: "https://github.com/shubham2110/AI.FloatingTimer/releases/download/latest/OverlayTimer.exe"}
}

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDownloadInstallAndUnchanged(t *testing.T) {
	b, a := fixture(t)
	dir := t.TempDir()
	current := filepath.Join(dir, "OverlayTimer.exe")
	if err := os.WriteFile(current, []byte("old app"), 0600); err != nil {
		t.Fatal(err)
	}
	downloads := 0
	// Both executables can be published together, in any asset order.
	launcher := a
	launcher.Name = "FloatingTimerLauncher.exe"
	launcher.URL = "https://github.com/shubham2110/AI.FloatingTimer/releases/download/latest/FloatingTimerLauncher.exe"
	metadata, _ := json.Marshal(map[string]any{"assets": []asset{launcher, a}})
	u := &updater{dir: dir, endpoint: "https://example.test/release", client: &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		data := metadata
		if r.URL.String() == a.URL {
			downloads++
			data = b
		} else if r.URL.String() != "https://example.test/release" {
			t.Fatalf("unexpected asset request: %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data)), Header: make(http.Header)}, nil
	})}}
	if err := u.check(); err != nil {
		t.Fatal(err)
	}
	old, _ := os.ReadFile(current)
	if string(old) != "old app" {
		t.Fatal("download replaced active app")
	}
	if err := u.installPending(); err != nil {
		t.Fatal(err)
	}
	if err := verify(current, a); err != nil {
		t.Fatal(err)
	}
	backup, _ := os.ReadFile(u.path("OverlayTimer.previous.exe"))
	if string(backup) != "old app" {
		t.Fatal("backup lost")
	}
	if err := u.check(); err != nil {
		t.Fatal(err)
	}
	if downloads != 1 {
		t.Fatalf("unchanged release redownloaded: %d", downloads)
	}
}

func TestCorruptPendingPreservesCurrent(t *testing.T) {
	_, a := fixture(t)
	u := &updater{dir: t.TempDir()}
	if err := os.WriteFile(u.path("OverlayTimer.exe"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(u.path("OverlayTimer.pending.exe"), []byte("truncated"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(u.path("update-pending.json"), a); err != nil {
		t.Fatal(err)
	}
	if err := u.installPending(); err == nil {
		t.Fatal("accepted corrupt download")
	}
	b, _ := os.ReadFile(u.path("OverlayTimer.exe"))
	if string(b) != "old" {
		t.Fatal("current app damaged")
	}
}

func TestRecoverInterruptedReplacement(t *testing.T) {
	u := &updater{dir: t.TempDir()}
	if err := os.WriteFile(u.path("OverlayTimer.previous.exe"), []byte("backup"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := u.installPending(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(u.path("OverlayTimer.exe"))
	if string(b) != "backup" {
		t.Fatal("backup not restored")
	}
}

func TestLockedCurrentKeepsPending(t *testing.T) {
	b, a := fixture(t)
	u := &updater{dir: t.TempDir()}
	for _, name := range []string{"OverlayTimer.exe", "OverlayTimer.pending.exe"} {
		if err := os.WriteFile(u.path(name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeJSON(u.path("update-pending.json"), a); err != nil {
		t.Fatal(err)
	}
	path, _ := windows.UTF16PtrFromString(u.path("OverlayTimer.exe"))
	h, err := windows.CreateFile(path, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.installPending(); err == nil {
		windows.CloseHandle(h)
		t.Fatal("replaced locked executable")
	}
	windows.CloseHandle(h)
	if err := verify(u.path("OverlayTimer.exe"), a); err != nil {
		t.Fatal(err)
	}
	if err := verify(u.path("OverlayTimer.pending.exe"), a); err != nil {
		t.Fatal(err)
	}
	if err := u.installPending(); err != nil {
		t.Fatal("retry after unlock:", err)
	}
}

func TestBadDownloadDoesNotBecomePending(t *testing.T) {
	b, a := fixture(t)
	for _, mode := range []string{"truncated", "digest", "http", "missing-asset"} {
		t.Run(mode, func(t *testing.T) {
			metadata, _ := json.Marshal(map[string]any{"assets": []asset{a}})
			u := &updater{dir: t.TempDir(), endpoint: "https://example.test/release", client: &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
				data, status := metadata, 200
				if mode == "missing-asset" {
					data = []byte(`{"assets":[]}`)
				}
				if r.URL.String() == a.URL {
					data = append([]byte(nil), b...)
					switch mode {
					case "truncated":
						data = data[:len(data)/2]
					case "digest":
						data[len(data)-1] ^= 1
					case "http":
						status = 503
					}
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(data)), Header: make(http.Header)}, nil
			})}}
			if err := u.check(); err == nil {
				t.Fatal("accepted bad release")
			}
			if _, err := os.Stat(u.path("update-pending.json")); !os.IsNotExist(err) {
				t.Fatal("published bad download")
			}
		})
	}
}
