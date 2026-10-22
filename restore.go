package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

// The hub's data is kept in a file next to the config, so after a restart
// (an update, say) the dashboard starts with what it last knew instead of an
// empty page while a slow hub answers. Restored sections are marked as such
// until the hub answers for them, and aren't taken as new readings.

type savedData struct {
	HubIP    string           `json:"hubIP"`
	Saved    time.Time        `json:"saved"`
	Sections map[string]Entry `json:"sections"`
}

// startedAt is when this run of the dashboard started.
var startedAt = time.Now()

// restoreMaxAge is how old saved data may be and still be shown.
const restoreMaxAge = 7 * 24 * time.Hour

// Load fills the store from a file saved for the same hub. It returns how
// many sections it restored.
func (s *Store) Load(path, hubIP string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var d savedData
	if json.Unmarshal(b, &d) != nil || d.HubIP == "" || d.HubIP != hubIP || time.Since(d.Saved) > restoreMaxAge {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for name, e := range d.Sections {
		if e.Data == nil || s.entries[name] != nil {
			continue
		}
		e := e
		e.Error, e.Restored = "", true
		s.entries[name] = &e
		n++
	}
	return n
}

// Save writes the store's data (not errors) for this hub, owner-only. It
// writes to a temporary file first so a crash can't leave half a file.
func (s *Store) Save(path, hubIP string) error {
	if hubIP == "" {
		return nil
	}
	d := savedData{HubIP: hubIP, Saved: time.Now(), Sections: map[string]Entry{}}
	for name, e := range s.Snapshot() {
		if e.Data != nil {
			e.Error = ""
			d.Sections[name] = e
		}
	}
	if len(d.Sections) == 0 {
		return nil
	}
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".hub-data-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// RunSaver saves the data every minute, and once more when ctx ends.
func (s *Store) RunSaver(ctx context.Context, path string, hub *Hub) {
	save := func() {
		ip, _ := hub.Target()
		if err := s.Save(path, ip); err != nil {
			log.Printf("saving hub data: %v", err)
		}
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			save()
			return
		case <-t.C:
			save()
		}
	}
}
