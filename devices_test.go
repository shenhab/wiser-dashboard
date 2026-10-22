package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// deviceHub is a fake hub for the add-device wizard: it keeps rooms and
// devices, applies room changes (unless ignoreRoomChanges) and logs writes.
type deviceHub struct {
	mu                sync.Mutex
	rooms             []map[string]any
	devices           []map[string]any
	writes            []string
	ignoreRoomChanges bool
}

func (h *deviceHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	path := strings.TrimSuffix(r.URL.Path, "/")
	if r.Method != "GET" {
		h.writes = append(h.writes, r.Method+" "+path+" "+string(body))
	}
	switch {
	case r.Method == "GET" && path == "/data/domain/Room":
		json.NewEncoder(w).Encode(h.rooms)
	case r.Method == "GET" && path == "/data/domain/Device":
		json.NewEncoder(w).Encode(h.devices)
	case r.Method == "POST" && path == "/data/domain/Room":
		var req map[string]string
		json.Unmarshal(body, &req)
		room := map[string]any{"id": float64(len(h.rooms) + 1), "Name": req["name"]}
		h.rooms = append(h.rooms, room)
		json.NewEncoder(w).Encode(room)
	case r.Method == "PATCH" && strings.HasPrefix(path, "/data/domain/Room/"):
		var change map[string]any
		json.Unmarshal(body, &change)
		for _, rm := range h.rooms {
			if "/data/domain/Room/"+strings.TrimSuffix(strings.TrimSuffix(jsonNum(rm["id"]), ".0"), "") == path {
				if !h.ignoreRoomChanges {
					for k, v := range change {
						rm[k] = v
					}
				}
				json.NewEncoder(w).Encode(rm)
				return
			}
		}
		w.WriteHeader(404)
	case r.Method == "PATCH":
		w.Write([]byte("{}"))
	default:
		w.WriteHeader(404)
	}
}

func jsonNum(v any) string { b, _ := json.Marshal(v); return string(b) }

func (h *deviceHub) room(id int) map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, rm := range h.rooms {
		if intOf(rm["id"]) == id {
			return rm
		}
	}
	return nil
}

func (h *deviceHub) roomWrites() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, w := range h.writes {
		if strings.HasPrefix(w, "PATCH /data/domain/Room/") {
			n++
		}
	}
	return n
}

func newDeviceTestServer(t *testing.T) (*httptest.Server, *deviceHub) {
	t.Helper()
	h := &deviceHub{
		rooms: []map[string]any{
			{"id": float64(1), "Name": "Upper Floor", "RoomStatId": float64(2)},
			{"id": float64(2), "Name": "Ground Floor", "RoomStatId": float64(1)},
			{"id": float64(3), "Name": "Spare Room"},
			{"id": float64(4), "Name": "Kitchen", "SmartValveIds": []any{float64(7)}},
		},
		devices: []map[string]any{
			{"id": float64(1), "ProductType": "RoomStat"},
			{"id": float64(2), "ProductType": "RoomStat"},
			{"id": float64(5), "ProductType": "RoomStat"}, // new, in no room
			{"id": float64(6), "ProductType": "RoomStat"}, // new, in no room
			{"id": float64(7), "ProductType": "iTRV"},
			{"id": float64(8), "ProductType": "iTRV"}, // new
			{"id": float64(9), "ProductType": "SmartPlug"},
		},
	}
	hubSrv := httptest.NewServer(h)
	t.Cleanup(hubSrv.Close)
	hub := NewHub()
	hub.SetTarget(strings.TrimPrefix(hubSrv.URL, "http://"), "hub-secret")
	store := NewStore()
	srv := &Server{hub: hub, store: store, poller: NewPoller(hub, store), history: NewHistory(""), health: NewHealth(""), listen: "127.0.0.1:0"}
	ts := httptest.NewServer(srv.routes())
	t.Cleanup(ts.Close)
	return ts, h
}

func post(t *testing.T, ts *httptest.Server, path string, body any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(body)
	res, err := http.Post(ts.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return out
}

func TestAttachDevice(t *testing.T) {
	ts, h := newDeviceTestServer(t)

	if r := post(t, ts, "/api/devices/attach", map[string]int{"device": 5, "room": 3}); r["ok"] != true {
		t.Fatalf("thermostat into an empty room: %v", r)
	}
	if got := intOf(h.room(3)["RoomStatId"]); got != 5 {
		t.Errorf("Spare Room's thermostat is %d, want 5", got)
	}

	if r := post(t, ts, "/api/devices/attach", map[string]int{"device": 8, "room": 4}); r["ok"] != true {
		t.Fatalf("valve into a room with a valve: %v", r)
	}
	if got := idList(h.room(4)["SmartValveIds"]); len(got) != 2 || got[0] != 7 || got[1] != 8 {
		t.Errorf("Kitchen's valves are %v, want [7 8]", got)
	}

	// Refused before anything is written to the hub.
	writes := h.roomWrites()
	for _, c := range []struct {
		device, room int
		want         string
	}{
		{6, 1, "already has a thermostat"}, // Upper Floor has device 2
		{1, 3, "already in Ground Floor"},  // device 1 belongs to Ground Floor
		{9, 3, "smart plug isn't attached"},
		{42, 3, "no device 42"},
		{8, 99, "no room 99"},
	} {
		r := post(t, ts, "/api/devices/attach", map[string]int{"device": c.device, "room": c.room})
		if r["ok"] != false || !strings.Contains(r["error"].(string), c.want) {
			t.Errorf("device %d into room %d: %v, want an error about %q", c.device, c.room, r, c.want)
		}
	}
	if h.roomWrites() != writes {
		t.Error("a refused attach still wrote to the hub")
	}

	// Attaching what's already there changes nothing.
	if r := post(t, ts, "/api/devices/attach", map[string]int{"device": 2, "room": 1}); r["ok"] != true || h.roomWrites() != writes {
		t.Errorf("re-attaching an attached device: %v", r)
	}
}

func TestAttachDeviceHubIgnoresIt(t *testing.T) {
	ts, h := newDeviceTestServer(t)
	h.ignoreRoomChanges = true
	r := post(t, ts, "/api/devices/attach", map[string]int{"device": 8, "room": 4})
	if r["ok"] != false || !strings.Contains(r["error"].(string), "didn't attach") {
		t.Fatalf("hub ignoring the change: %v", r)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	last := h.writes[len(h.writes)-1]
	if last != `PATCH /data/domain/Room/4 {"SmartValveIds":[7]}` {
		t.Errorf("the room wasn't put back; last write: %s", last)
	}
}

func TestCreateRoomAndPermitJoin(t *testing.T) {
	ts, h := newDeviceTestServer(t)
	r := post(t, ts, "/api/rooms", map[string]string{"name": "Office"})
	if r["ok"] != true || intOf(r["room"]) != 5 {
		t.Fatalf("create room: %v", r)
	}
	if r := post(t, ts, "/api/rooms", map[string]string{"name": "office"}); r["ok"] != false {
		t.Errorf("duplicate room name accepted: %v", r)
	}
	if r := post(t, ts, "/api/rooms", map[string]string{"name": "  "}); r["ok"] != false {
		t.Errorf("empty room name accepted: %v", r)
	}

	if r := post(t, ts, "/api/devices/permit-join", map[string]int{}); r["ok"] != true || r["until"] == nil {
		t.Fatalf("permit join: %v", r)
	}
	if r := post(t, ts, "/api/devices/permit-join", map[string]int{"seconds": 3600}); r["ok"] != false {
		t.Errorf("an hour-long permit join accepted: %v", r)
	}
	if r := post(t, ts, "/api/devices/identify", map[string]any{"device": 5, "on": true}); r["ok"] != true {
		t.Errorf("identify: %v", r)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	want := []string{
		`POST /data/domain/Room {"name":"Office"}`,
		`PATCH /data/domain/System/RequestPermitJoin 120`,
		`PATCH /data/domain/Device/5 {"Identify":true}`,
	}
	if strings.Join(h.writes, "\n") != strings.Join(want, "\n") {
		t.Errorf("hub writes:\n%s\nwant:\n%s", strings.Join(h.writes, "\n"), strings.Join(want, "\n"))
	}
}
