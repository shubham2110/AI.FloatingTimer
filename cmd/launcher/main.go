//go:build windows

// The launcher deliberately has no dependency on the timer's GUI or settings.
package main

import (
	"crypto/sha256"
	"debug/pe"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const releaseURL = "https://api.github.com/repos/shubham2110/AI.FloatingTimer/releases/tags/latest"
const maxDownload = int64(512 << 20)

type asset struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Digest  string `json:"digest"`
	URL     string `json:"browser_download_url"`
	Updated string `json:"updated_at"`
}

type updater struct {
	dir      string
	client   *http.Client
	endpoint string
}

func (u *updater) path(name string) string { return filepath.Join(u.dir, name) }

func digestFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func verify(path string, a asset) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() != a.Size {
		return fmt.Errorf("download size mismatch")
	}
	digest, err := digestFile(path)
	if err != nil {
		return err
	}
	if digest != a.Digest {
		return fmt.Errorf("download SHA-256 mismatch")
	}
	f, err := pe.Open(path)
	if err != nil {
		return fmt.Errorf("invalid Windows executable: %w", err)
	}
	defer f.Close()
	if f.Machine != pe.IMAGE_FILE_MACHINE_AMD64 || f.Characteristics&pe.IMAGE_FILE_EXECUTABLE_IMAGE == 0 || f.Characteristics&pe.IMAGE_FILE_DLL != 0 {
		return fmt.Errorf("expected a Windows x64 executable")
	}
	return nil
}

func writeJSON(path string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(path+".tmp", path)
}

// This runs only on the foreground path, never after the fallback app starts.
func (u *updater) installPending() error {
	current, backup := u.path("OverlayTimer.exe"), u.path("OverlayTimer.previous.exe")
	if _, err := os.Stat(current); os.IsNotExist(err) {
		if _, err := os.Stat(backup); err == nil {
			if err := os.Rename(backup, current); err != nil {
				return err
			}
		}
	}
	b, err := os.ReadFile(u.path("update-pending.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var a asset
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	pending := u.path("OverlayTimer.pending.exe")
	if err := verify(pending, a); err != nil {
		return err
	}
	if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
		return err
	}
	hadCurrent := false
	if _, err := os.Stat(current); err == nil {
		if err := os.Rename(current, backup); err != nil {
			return err
		}
		hadCurrent = true
	}
	if err := os.Rename(pending, current); err != nil {
		if hadCurrent {
			if restoreErr := os.Rename(backup, current); restoreErr != nil {
				return fmt.Errorf("install: %v; restore: %w", err, restoreErr)
			}
		}
		return err
	}
	_ = os.Remove(u.path("update-pending.json"))
	return nil
}

func (u *updater) get(url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "FloatingTimerLauncher")
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

// Download to a partial file; publish the pending manifest only after verification.
func (u *updater) check() error {
	resp, err := u.get(u.endpoint)
	if err != nil {
		return err
	}
	var release struct {
		Assets []asset `json:"assets"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&release)
	resp.Body.Close()
	if err != nil {
		return err
	}
	var selected *asset
	for i := range release.Assets {
		if release.Assets[i].Name == "OverlayTimer.exe" {
			selected = &release.Assets[i]
			break
		}
	}
	if selected == nil {
		return fmt.Errorf("release has no OverlayTimer.exe asset")
	}
	a := *selected
	if a.Size <= 0 || a.Size > maxDownload {
		return fmt.Errorf("invalid asset size")
	}
	if !strings.HasPrefix(a.Digest, "sha256:") || len(a.Digest) != 71 {
		return fmt.Errorf("release has no valid SHA-256 digest")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(a.Digest, "sha256:")); err != nil {
		return err
	}
	if !strings.HasPrefix(a.URL, "https://github.com/shubham2110/AI.FloatingTimer/releases/download/") {
		return fmt.Errorf("unexpected download URL")
	}
	if digest, err := digestFile(u.path("OverlayTimer.exe")); err == nil && digest == a.Digest {
		return nil
	}
	if err := verify(u.path("OverlayTimer.pending.exe"), a); err == nil {
		return writeJSON(u.path("update-pending.json"), a)
	}
	resp, err = u.get(a.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	partial := u.path("OverlayTimer.download")
	f, err := os.Create(partial)
	if err != nil {
		return err
	}
	defer os.Remove(partial)
	n, copyErr := io.Copy(f, io.LimitReader(resp.Body, a.Size+1))
	syncErr := f.Sync()
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n != a.Size {
		return fmt.Errorf("incomplete or oversized download")
	}
	if err := verify(partial, a); err != nil {
		return err
	}
	if err := os.Rename(partial, u.path("OverlayTimer.pending.exe")); err != nil {
		return err
	}
	return writeJSON(u.path("update-pending.json"), a)
}

// The worker can finish in the background, but only this goroutine may install.
func run(wait time.Duration, install, check, launch func() error) error {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	if err := install(); err != nil {
		log.Printf("pending update: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- check() }()
	select {
	case err := <-done:
		if err != nil {
			log.Printf("update check: %v", err)
		} else if err := install(); err != nil {
			log.Printf("install: %v", err)
		}
		return launch()
	case <-deadline.C:
		err := launch()
		log.Printf("foreground deadline reached; background download continues")
		if updateErr := <-done; updateErr != nil {
			log.Printf("background update: %v", updateErr)
		}
		return err
	}
}

func main() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	dir := filepath.Dir(exe)
	if f, err := os.OpenFile(filepath.Join(dir, "launcher.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); err == nil {
		defer f.Close()
		log.SetOutput(f)
	}
	// An OS lock is automatically released after crashes; no stale PID lock files.
	lock, err := os.OpenFile(filepath.Join(dir, "launcher.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		log.Print(err)
		return
	}
	defer lock.Close()
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(lock.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped); err != nil {
		log.Printf("launcher already active or lock unavailable: %v", err)
		return
	}
	defer windows.UnlockFileEx(windows.Handle(lock.Fd()), 0, 1, 0, &overlapped)
	u := &updater{dir: dir, endpoint: releaseURL, client: &http.Client{Timeout: 30 * time.Minute}}
	launch := func() error {
		start := func(name string) error {
			cmd := exec.Command(u.path(name), os.Args[1:]...)
			cmd.Dir = dir
			if err := cmd.Start(); err != nil {
				return err
			}
			return cmd.Process.Release()
		}
		if err := start("OverlayTimer.exe"); err != nil {
			log.Printf("app launch failed: %v; trying backup", err)
			return start("OverlayTimer.previous.exe")
		}
		return nil
	}
	if err := run(2*time.Minute, u.installPending, u.check, launch); err != nil {
		log.Printf("cannot launch timer: %v", err)
		message, _ := windows.UTF16PtrFromString("Floating Timer could not start. Keep OverlayTimer.exe beside the launcher. See launcher.log for details.")
		title, _ := windows.UTF16PtrFromString("Floating Timer Launcher")
		windows.NewLazySystemDLL("user32.dll").NewProc("MessageBoxW").Call(0, uintptr(unsafe.Pointer(message)), uintptr(unsafe.Pointer(title)), 0x10)
	}
}
