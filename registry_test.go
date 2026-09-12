//go:build windows

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testStore(t *testing.T) *PCStore {
	t.Helper()
	return &PCStore{path: filepath.Join(t.TempDir(), "pcs.json")}
}

func mergeRecords(t *testing.T, s *PCStore, records ...PC) {
	t.Helper()
	if _, err := s.merge(records); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryMigrationAndStableIDs(t *testing.T) {
	s := testStore(t)
	legacy := `[{"id":"legacy","name":"Desk","address":"http://HOST:18081/","ui_port":19082},
		{"id":"duplicate","name":"Desk","address":"http://host:18081"}]`
	if err := os.WriteFile(s.path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.load(); err != nil {
		t.Fatal(err)
	}
	mergeRecords(t, s, PC{ID: "peer-id", Name: "Desk", Address: "http://host:18081", LastSeen: time.Now()})
	pcs := s.list()
	if len(pcs) != 1 || pcs[0].ID != "legacy" || pcs[0].UIPort != 19082 {
		t.Fatalf("migration lost identity/port or kept duplicates: %+v", pcs)
	}
	_, code, err := s.edit("", pcs[0], false)
	if err == nil || code != http.StatusConflict {
		t.Fatal("duplicate manual add was accepted")
	}
	edit := pcs[0]
	edit.Name, edit.UIPort = "Renamed", 0
	updated, _, err := s.edit(edit.ID, edit, false)
	if err != nil || updated.UIPort != 19082 || updated.ID != "legacy" {
		t.Fatalf("edit dropped custom port/ID: %+v %v", updated, err)
	}
}

func TestRegistryDeletionSurvivesSyncRestartAndProbe(t *testing.T) {
	a, b := testStore(t), testStore(t)
	pc := PC{Name: "Desk", Address: "http://192.168.1.2:18081"}
	created, _, err := a.edit("", pc, false)
	if err != nil {
		t.Fatal(err)
	}
	mergeRecords(t, b, a.snapshot()...)
	if _, _, err := a.edit(created.ID, PC{}, true); err != nil {
		t.Fatal(err)
	}
	mergeRecords(t, a, b.snapshot()...)
	pc.LastSeen = time.Now()
	mergeRecords(t, a, pc)
	if len(a.list()) != 0 {
		t.Fatal("deleted record was resurrected")
	}
	mergeRecords(t, b, a.snapshot()...)
	if len(b.list()) != 0 {
		t.Fatal("deletion did not propagate")
	}
	restarted := &PCStore{path: a.path}
	if err := restarted.load(); err != nil {
		t.Fatal(err)
	}
	if len(restarted.list()) != 0 || len(restarted.snapshot()) != 1 {
		t.Fatal("tombstone not persisted")
	}
	if _, _, err := restarted.edit("", pc, false); err != nil {
		t.Fatal(err)
	}
	mergeRecords(t, b, restarted.snapshot()...)
	if len(b.list()) != 1 {
		t.Fatal("explicit re-add did not restore record")
	}
}

func TestRegistryConflictsConverge(t *testing.T) {
	a, b := testStore(t), testStore(t)
	revision := time.Now().UTC()
	x := PC{ID: "a", Name: "Alpha", Address: "http://192.168.1.3:18081", UpdatedAt: revision, LastSeen: revision}
	y := x
	y.ID, y.Name, y.UIPort, y.LastSeen = "b", "Zulu", 19082, revision.Add(time.Minute)
	mergeRecords(t, a, x, y)
	mergeRecords(t, b, y, x)
	left, right := a.list()[0], b.list()[0]
	if left.ID != "a" || right.ID != "b" {
		t.Fatal("local IDs changed")
	}
	left.ID, right.ID = "", ""
	if left != right {
		t.Fatalf("metadata failed to converge: %+v %+v", left, right)
	}
	count, err := a.merge(b.snapshot())
	if count != 0 || err != nil {
		t.Fatal("identical gossip was not a no-op")
	}
}

func TestRegistryFailedWritesRollback(t *testing.T) {
	s := testStore(t)
	pc, _, err := s.edit("", PC{Name: "Desk", Address: "http://host:18081"}, false)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(s.snapshot())
	s.path = filepath.Join(t.TempDir(), "missing", "pcs.json")
	if _, _, err := s.edit(pc.ID, PC{}, true); err == nil {
		t.Fatal("expected save failure")
	}
	pc.UpdatedAt = time.Now().Add(time.Hour)
	pc.Name = "Changed"
	if _, err := s.merge([]PC{pc}); err == nil {
		t.Fatal("expected merge failure")
	}
	after, _ := json.Marshal(s.snapshot())
	if string(before) != string(after) {
		t.Fatalf("failed writes changed memory: %s", after)
	}
}

func TestRegistryAddressChangeSuppressesOldAddress(t *testing.T) {
	s := testStore(t)
	pc, _, err := s.edit("", PC{Name: "Desk", Address: "http://old:18081"}, false)
	if err != nil {
		t.Fatal(err)
	}
	old := pc
	pc.Address = "http://new:18081"
	updated, _, err := s.edit(pc.ID, pc, false)
	if err != nil {
		t.Fatal(err)
	}
	mergeRecords(t, s, old)
	if got := s.list(); len(got) != 1 || got[0].Address != pc.Address || updated.ID != old.ID {
		t.Fatalf("address edit was undone: %+v", got)
	}
	restored, _, err := s.edit("", old, false)
	if err != nil || restored.ID == updated.ID {
		t.Fatalf("restoring old address reused the moved record's ID: %+v %v", restored, err)
	}
}
