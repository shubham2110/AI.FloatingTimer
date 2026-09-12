//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Address is the shared registry key; ID remains stable within each manager.
// Deleted records are retained so an offline peer cannot resurrect a removal.
type PC struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Address   string    `json:"address"`
	UIPort    int       `json:"ui_port,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	LastSeen  time.Time `json:"last_seen,omitempty"`
	Deleted   bool      `json:"deleted,omitempty"`
	Inactive  bool      `json:"inactive,omitempty"` // Local preference; never imported from peers.
}

type PCStore struct {
	sync.RWMutex
	path string
	PCs  []PC
}

func (s *PCStore) load() error {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var records []PC
	if err := json.Unmarshal(data, &records); err != nil {
		return err
	}
	for i := range records {
		if err := normalizePC(&records[i]); err != nil {
			return err
		}
	}
	s.Lock()
	defer s.Unlock()
	s.PCs = nil
	for _, pc := range records {
		s.mergeLocked(pc)
	}
	return nil
}

// commitLocked rolls back memory if persistence fails; callers hold the lock.
func (s *PCStore) commitLocked(previous []PC) error {
	data, err := json.MarshalIndent(s.PCs, "", "  ")
	if err == nil {
		err = os.WriteFile(s.path+".tmp", append(data, '\n'), 0644)
	}
	if err == nil {
		err = os.Rename(s.path+".tmp", s.path)
	}
	if err != nil {
		s.PCs = previous
	}
	return err
}

func (s *PCStore) snapshot() []PC {
	s.RLock()
	defer s.RUnlock()
	return append([]PC{}, s.PCs...)
}

func (s *PCStore) list() []PC {
	result := []PC{}
	for _, pc := range s.snapshot() {
		if !pc.Deleted {
			result = append(result, pc)
		}
	}
	return result
}

func (s *PCStore) find(id string) (PC, bool) {
	for _, pc := range s.list() {
		if pc.ID == id {
			return pc, true
		}
	}
	return PC{}, false
}

func normalizePC(pc *PC) error {
	address, err := normalizeAddress(pc.Address)
	if err != nil {
		return err
	}
	if pc.UIPort < 0 || pc.UIPort > 65535 {
		return errors.New("UI port must be between 1 and 65535")
	}
	pc.Address = strings.ToLower(address)
	if pc.ID == "" || strings.IndexFunc(pc.ID, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	}) >= 0 {
		pc.ID = stablePCID(pc.Address)
	}
	if strings.TrimSpace(pc.Name) == "" {
		pc.Name = pc.Address
	}
	return nil
}

// Metadata uses last-write-wins, with a deterministic tie break. Observations
// only advance LastSeen; they never undo an edit or a deletion.
func newerPC(a, b PC) bool {
	if !a.UpdatedAt.Equal(b.UpdatedAt) {
		return a.UpdatedAt.After(b.UpdatedAt)
	}
	if a.Deleted != b.Deleted {
		return a.Deleted
	}
	return fmt.Sprintf("%s\x00%05d", a.Name, a.UIPort) > fmt.Sprintf("%s\x00%05d", b.Name, b.UIPort)
}

func (s *PCStore) mergeLocked(pc PC) bool {
	for i, old := range s.PCs {
		if old.Address != pc.Address {
			continue
		}
		next := old
		if newerPC(pc, old) {
			next = pc
			next.ID = old.ID
		}
		next.Inactive = old.Inactive
		if pc.LastSeen.After(next.LastSeen) {
			next.LastSeen = pc.LastSeen
		}
		if old.LastSeen.After(next.LastSeen) {
			next.LastSeen = old.LastSeen
		}
		if next.UIPort == 0 {
			next.UIPort = pc.UIPort
		}
		s.PCs[i] = next
		return next != old
	}
	pc.ID = s.availableID(pc.ID, pc.Address)
	s.PCs = append(s.PCs, pc)
	return true
}

// A moved record keeps its ID, so restoring its old address may need a new ID.
func (s *PCStore) availableID(id, address string) string {
	for suffix := 1; ; suffix++ {
		conflict := false
		for _, pc := range s.PCs {
			if pc.ID == id && pc.Address != address {
				conflict = true
				break
			}
		}
		if !conflict {
			return id
		}
		id = stablePCID(fmt.Sprintf("%s#%d", address, suffix))
	}
}

func (s *PCStore) merge(peers []PC) (int, error) {
	for i := range peers {
		peers[i].Inactive = false
		if err := normalizePC(&peers[i]); err != nil {
			return 0, err
		}
	}
	s.Lock()
	defer s.Unlock()
	previous := append([]PC{}, s.PCs...)
	changed := 0
	for _, pc := range peers {
		if s.mergeLocked(pc) {
			changed++
		}
	}
	if changed > 0 {
		if err := s.commitLocked(previous); err != nil {
			return 0, err
		}
	}
	return changed, nil
}

// edit handles local add/update/remove as one durable transaction.
func (s *PCStore) edit(id string, pc PC, remove bool) (PC, int, error) {
	if !remove {
		if err := normalizePC(&pc); err != nil {
			return pc, http.StatusBadRequest, err
		}
		pc.Deleted, pc.Inactive, pc.LastSeen = false, false, time.Time{}
	}
	s.Lock()
	defer s.Unlock()
	index := -1
	for i, old := range s.PCs {
		if old.ID == id && !old.Deleted {
			index = i
		}
	}
	if id != "" && index < 0 {
		return pc, http.StatusNotFound, errors.New("PC not found")
	}
	previous := append([]PC{}, s.PCs...)
	now := time.Now().UTC()
	// Advance beyond every known revision, even if the local clock moved back.
	for _, old := range s.PCs {
		if !now.After(old.UpdatedAt) {
			now = old.UpdatedAt.Add(time.Nanosecond)
		}
	}
	if remove {
		pc = s.PCs[index]
		pc.Deleted, pc.UpdatedAt = true, now
		s.PCs[index] = pc
	} else {
		for i, old := range s.PCs {
			if i != index && old.Address == pc.Address && !old.Deleted {
				return pc, http.StatusConflict, errors.New("a PC with this address already exists")
			}
		}
		pc.ID, pc.UpdatedAt = s.availableID(stablePCID(pc.Address), pc.Address), now
		if index >= 0 {
			old := s.PCs[index]
			pc.ID, pc.LastSeen = old.ID, old.LastSeen
			pc.Inactive = old.Inactive
			if pc.UIPort == 0 {
				pc.UIPort = old.UIPort
			}
			if old.Address != pc.Address {
				// Keep a tombstone for the old address when moving a record.
				old.ID, old.Deleted, old.UpdatedAt = stablePCID(old.Address), true, now
				s.PCs[index] = old
				pc.LastSeen = time.Time{}
			} else {
				s.PCs = append(s.PCs[:index], s.PCs[index+1:]...)
			}
		}
		// Explicit add can restore a tombstone. Keep the selected ID on updates.
		for i := len(s.PCs) - 1; i >= 0; i-- {
			if s.PCs[i].Address == pc.Address {
				s.PCs = append(s.PCs[:i], s.PCs[i+1:]...)
			}
		}
		s.PCs = append(s.PCs, pc)
	}
	if err := s.commitLocked(previous); err != nil {
		return pc, http.StatusInternalServerError, fmt.Errorf("could not save PC list: %w", err)
	}
	return pc, http.StatusOK, nil
}

func (s *PCStore) active() []PC {
	result := []PC{}
	for _, pc := range s.list() {
		if !pc.Inactive {
			result = append(result, pc)
		}
	}
	return result
}

func (s *PCStore) setActive(id string, active bool) (PC, error) {
	s.Lock()
	defer s.Unlock()
	for i, pc := range s.PCs {
		if pc.ID != id || pc.Deleted {
			continue
		}
		previous := append([]PC{}, s.PCs...)
		pc.Inactive = !active
		s.PCs[i] = pc
		return pc, s.commitLocked(previous)
	}
	return PC{}, errors.New("PC not found")
}
