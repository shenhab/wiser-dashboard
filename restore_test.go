package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveAndRestoreHubData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub-data.json")
	old := NewStore()
	old.set("Room", []byte(`[{"id":1,"Name":"Upper Floor","CalculatedTemperature":215}]`), time.Second)
	old.set("System", []byte(`{"ActiveSystemVersion":"3.18.3"}`), time.Second)
	old.setError("Zigbee", "timeout") // no data: not saved
	if err := old.Save(path, "192.168.1.5"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("saved file: %v, mode %v; want owner-only", err, fi.Mode().Perm())
	}

	s := NewStore()
	if n := s.Load(path, "192.168.1.5"); n != 2 {
		t.Fatalf("restored %d sections, want 2", n)
	}
	room := s.Snapshot()["Room"]
	if !room.Restored || !strings.Contains(string(room.Data), "Upper Floor") || room.Fetched.IsZero() {
		t.Errorf("restored room: %+v", room)
	}
	// The hub answering replaces it with live data.
	s.set("Room", []byte(`[]`), time.Second)
	if s.Snapshot()["Room"].Restored {
		t.Error("live data still marked restored")
	}

	if n := NewStore().Load(path, "192.168.1.9"); n != 0 {
		t.Errorf("another hub's data was restored (%d sections)", n)
	}
	b, _ := os.ReadFile(path)
	var d savedData
	json.Unmarshal(b, &d)
	d.Saved = time.Now().Add(-restoreMaxAge - time.Hour)
	b, _ = json.Marshal(d)
	os.WriteFile(path, b, 0o600)
	if n := NewStore().Load(path, "192.168.1.5"); n != 0 {
		t.Errorf("data older than %v was restored", restoreMaxAge)
	}
}

func TestRestoredDataIsNotANewReading(t *testing.T) {
	long := time.Now().Add(-2 * time.Hour)
	snap := map[string]Entry{
		"Room":   {Data: json.RawMessage(`[{"id":1,"Name":"Upper Floor","CalculatedTemperature":215,"CurrentSetPoint":200}]`), Fetched: long, Restored: true},
		"status": {Data: json.RawMessage(`{"uptime":100,"freeHeap":20000}`), Fetched: long, Restored: true},
	}
	if _, ok := sampleFrom(snap); ok {
		t.Error("restored data was added to the history")
	}
	h := NewHealth("")
	h.check(snap)
	if len(h.events) != 0 || !h.seen.IsZero() {
		t.Errorf("restored data made health events %v / samples", h.events)
	}

	store := NewStore()
	store.entries["Room"] = &Entry{Data: snap["Room"].Data, Fetched: long, Restored: true}
	hub := NewHub()
	hub.SetTarget("192.168.1.5", "secret")
	srv := &Server{hub: hub, store: store}
	hubOffline := func() bool {
		for _, a := range srv.alerts() {
			if a.Kind == "hub-offline" {
				return true
			}
		}
		return false
	}
	if hubOffline() {
		t.Error("restored data raised the hub-offline alert right after a restart")
	}
	// But a hub that stays silent after the restart is still reported.
	old := startedAt
	startedAt = time.Now().Add(-staleAfter - time.Minute)
	defer func() { startedAt = old }()
	if !hubOffline() {
		t.Error("no hub-offline alert when the hub hasn't answered since the restart")
	}
}
