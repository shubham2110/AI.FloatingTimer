//go:build windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
)

var configuredNames = struct {
	sync.RWMutex
	names    map[string]string
	previous []string
}{names: make(map[string]string)}

func editedName(path, fallback string) string {
	configuredNames.RLock()
	defer configuredNames.RUnlock()
	if name := configuredNames.names[path]; name != "" {
		return name
	}
	return fallback
}

func previousDeviceName(name string) bool {
	if name == "" {
		return false
	}
	configuredNames.RLock()
	defer configuredNames.RUnlock()
	for _, previous := range configuredNames.previous {
		if strings.EqualFold(previous, name) {
			return true
		}
	}
	return false
}

type systemRename struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func saveSystemName(filename string, change systemRename) error {
	configuredNames.Lock()
	defer configuredNames.Unlock()
	raw, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	var config map[string]json.RawMessage
	if err = json.Unmarshal(raw, &config); err != nil {
		return err
	}
	if config == nil {
		return errors.New("invalid timer configuration")
	}
	value, _ := json.Marshal(change.Name)
	var previous []string
	if change.Path == "" {
		if raw := config["previous_friendly_names"]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &previous); err != nil {
				return err
			}
		}
		old := configuredNames.names[""]
		if old == "" {
			_ = json.Unmarshal(config["friendly_name"], &old)
		}
		if old == "" {
			old = appConfig.FriendlyName
		}
		if old == "" {
			old, _ = os.Hostname()
		}
		if old != "" && !strings.EqualFold(old, change.Name) {
			previous = append(previous, old)
		}
		config["previous_friendly_names"], _ = json.Marshal(previous)
		config["friendly_name"] = value
	} else {
		var timers []map[string]json.RawMessage
		if err = json.Unmarshal(config["on_behalf_of"], &timers); err != nil {
			return err
		}
		found := false
		for _, timer := range timers {
			var id string
			if json.Unmarshal(timer["id"], &id) == nil && "/"+id == change.Path {
				timer["name"] = value
				found = true
				break
			}
		}
		if !found {
			return errors.New("on-behalf timer is not configured on this PC")
		}
		config["on_behalf_of"], err = json.Marshal(timers)
		if err != nil {
			return err
		}
	}
	if err := writeTimerConfig(filename, config); err != nil {
		return err
	}
	configuredNames.names[change.Path] = change.Name
	if change.Path == "" {
		configuredNames.previous = previous
	}
	return nil
}

func systemRenameHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "PUT required", 405)
		return
	}
	var c systemRename
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&c) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid system name"})
		return
	}
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" || len(c.Name) > 120 {
		writeJSON(w, 400, map[string]string{"error": "name must be 1–120 bytes"})
		return
	}
	oldIdentity := localNodeIdentity()
	if err := saveSystemName(appDataPath(ConfigFileName), c); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if c.Path == "" {
		billingStore.mu.Lock()
		billingStore.deviceID = c.Name
		billingStore.mu.Unlock()
	}
	identity := localNodeIdentity()
	for _, old := range identityPCs(localRegistryPC().Address, oldIdentity) {
		if billingPath(old.Address) == c.Path {
			if err := store.renameAliases(old, c.Name, identity.Name); err != nil {
				logf("registry", "could not update names after saving config: %v", err)
			}
			break
		}
	}
	writeJSON(w, 200, identity)
}

func systemGroup(pc PC) string {
	path := billingPath(pc.Address)
	name := strings.ToLower(pc.Name)
	if path != "" {
		owner := pc.Owner
		if owner == "" {
			owner = strings.TrimSuffix(strings.TrimRight(pc.Address, "/"), path)
		}
		return name + "\x00" + path + "\x00" + strings.ToLower(owner)
	}
	return name
}

func cleanOfflineHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", 405)
		return
	}
	snapshot := store.list()
	groups := map[string][]PC{}
	for _, pc := range snapshot {
		groups[systemGroup(pc)] = append(groups[systemGroup(pc)], pc)
	}
	offline := map[string]PC{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, 16)
	for _, aliases := range groups {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, pc := range aliases {
				select {
				case slots <- struct{}{}:
				case <-r.Context().Done():
					return
				}
				ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, timerEndpoint(pc.Address, "status"), nil)
				var online bool
				if err == nil {
					resp, err := timerHTTP.Do(req)
					if err == nil {
						online = resp.StatusCode == http.StatusOK
						resp.Body.Close()
					}
				}
				cancel()
				<-slots
				if online || r.Context().Err() != nil {
					return
				}
			}
			mu.Lock()
			for _, pc := range aliases {
				offline[pc.ID] = pc
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if r.Context().Err() != nil {
		return
	}
	store.Lock()
	defer store.Unlock()
	previous := append([]PC(nil), store.PCs...)
	removed := 0
	for i, pc := range store.PCs {
		if old, ok := offline[pc.ID]; ok && reflect.DeepEqual(old, pc) {
			store.PCs[i].Deleted = true
			store.PCs[i].UpdatedAt = time.Now().UTC()
			removed++
		}
	}
	if removed > 0 {
		if err := store.commitLocked(previous); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
	}
	writeJSON(w, 200, map[string]int{"removed": removed})
}

func writeTimerConfig(filename string, config map[string]json.RawMessage) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(filename), ".timer-config-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(append(data, '\n'))
	closeErr := tmp.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), filename); err != nil {
		return err
	}
	return nil
}
