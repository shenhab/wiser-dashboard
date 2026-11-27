package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPollerKeepsMissingSectionsQuiet: a section the hub doesn't have (404)
// is left out, not shown as an error, on a hub that never answered for it.
// Any other failure still is, and so is a section the hub used to answer.
func TestPollerKeepsMissingSectionsQuiet(t *testing.T) {
	answers := map[string]int{"/data/domain/Room": 200, "/data/domain/Device": 404, "/data/status/": 500}
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, known := answers[r.URL.Path]
		if !known {
			status = http.StatusNotFound
		}
		if status == http.StatusOK {
			w.Write([]byte(`[{"id":1,"Name":"Lounge"}]`))
			return
		}
		w.WriteHeader(status)
		w.Write([]byte(`{"Error":"Not Found"}`))
	}))
	t.Cleanup(hubSrv.Close)
	hub := NewHub()
	hub.SetTarget(hostPort(hubSrv.URL), "hub-secret")
	store := NewStore()
	p := NewPoller(hub, store)
	fetch := func(section string) { p.fetch(context.Background(), section) }

	fetch("Room")
	fetch("Device")
	fetch("status")
	snap := store.Snapshot()
	if _, ok := snap["Device"]; ok {
		t.Errorf("a section the hub doesn't have is shown: %+v", snap["Device"])
	}
	if e, ok := snap["status"]; !ok || !strings.Contains(e.Error, "HTTP 500") {
		t.Errorf("a failing section isn't shown as failing: %+v", e)
	}
	if e, ok := snap["Room"]; !ok || e.Error != "" || len(e.Data) == 0 {
		t.Errorf("a section that answered: %+v", e)
	}

	// A section the hub used to answer keeps its data, and shows the failure.
	answers["/data/domain/Room"] = 404
	fetch("Room")
	if e := store.Snapshot()["Room"]; e.Error == "" || len(e.Data) == 0 {
		t.Errorf("a section that stopped answering: %+v", e)
	}
}
