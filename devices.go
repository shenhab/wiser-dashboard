package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Adding devices to the hub (the "Add a device" wizard): open the hub for
// new devices, make one blink, create a room, and attach a device to a room.
// The Wiser app is the official way; the hub's local API supports opening it
// for joining and creating rooms, and a room's devices are written to the
// same fields the hub reports them in (RoomStatId, SmartValveIds, ...), then
// read back to confirm the hub took the change.

// roomField is where each kind of device is attached on a room; list fields
// hold several devices, the thermostat field one.
var roomField = map[string]struct {
	name string
	list bool
}{
	"RoomStat":          {"RoomStatId", false},
	"iTRV":              {"SmartValveIds", true},
	"HeatingActuator":   {"HeatingActuatorIds", true},
	"UnderFloorHeating": {"UnderFloorHeatingIds", true},
}

// handlePermitJoin opens the hub for new devices for a while (2 minutes).
func (s *Server) handlePermitJoin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Seconds int `json:"seconds"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Seconds == 0 {
		req.Seconds = 120
	}
	if req.Seconds < 30 || req.Seconds > 300 {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "seconds must be 30-300"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	res := s.forward(ctx, "PATCH", "/data/domain/System/RequestPermitJoin", []byte(strconv.Itoa(req.Seconds)))
	if err := hubError(res); err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "the hub didn't open for new devices: " + err.Error()})
		return
	}
	s.poller.Refresh("Device")
	writeJSON(w, 200, map[string]any{"ok": true, "until": time.Now().Add(time.Duration(req.Seconds) * time.Second)})
}

// handleIdentify makes a device blink (or stop).
func (s *Server) handleIdentify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Device int  `json:"device"`
		On     bool `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Device <= 0 {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "device is required"})
		return
	}
	body, _ := json.Marshal(map[string]bool{"Identify": req.On})
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	if err := hubError(s.forward(ctx, "PATCH", fmt.Sprintf("/data/domain/Device/%d", req.Device), body)); err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleCreateRoom adds a room and returns its id.
func (s *Server) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 32 {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "the room needs a name of up to 32 characters"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	for _, rm := range s.hubRooms(ctx) {
		if strings.EqualFold(strOf(rm["Name"]), name) {
			writeJSON(w, 200, map[string]any{"ok": false, "error": fmt.Sprintf("there's already a room called %q", strOf(rm["Name"]))})
			return
		}
	}
	body, _ := json.Marshal(map[string]string{"name": name})
	res := s.hub.Do(ctx, "POST", "/data/domain/Room/", body, true)
	if err := hubError(res); err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "the hub didn't create the room: " + err.Error()})
		return
	}
	// Find it by name rather than trusting the reply's shape.
	for _, rm := range s.hubRooms(ctx) {
		if strings.EqualFold(strOf(rm["Name"]), name) {
			s.poller.Refresh("Room")
			writeJSON(w, 200, map[string]any{"ok": true, "room": intOf(rm["id"])})
			return
		}
	}
	writeJSON(w, 200, map[string]any{"ok": false, "error": "the hub accepted the new room but doesn't list it"})
}

// handleAttachDevice puts a device in a room and confirms the hub kept it.
func (s *Server) handleAttachDevice(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Device int `json:"device"`
		Room   int `json:"room"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Device <= 0 || req.Room <= 0 {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "device and room are required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	if err := s.attachDevice(ctx, req.Device, req.Room); err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) attachDevice(ctx context.Context, device, room int) error {
	dev, err := s.hubItem(ctx, "Device", device)
	if err != nil {
		return err
	}
	kind := strOf(dev["ProductType"])
	field, ok := roomField[kind]
	if !ok {
		return fmt.Errorf("a %s isn't attached to a room this way; use the Wiser app for it", kindName(kind))
	}
	path := fmt.Sprintf("/data/domain/Room/%d", room)
	before, err := s.hubItem(ctx, "Room", room)
	if err != nil {
		return err
	}
	for _, rm := range s.hubRooms(ctx) {
		if intOf(rm["id"]) != room && hasDevice(rm[field.name], device) {
			return fmt.Errorf("this %s is already in %s; take it out there first (Wiser app)", kindName(kind), strOf(rm["Name"]))
		}
	}

	var value any = device
	if field.list {
		ids := idList(before[field.name])
		if hasDevice(before[field.name], device) {
			return nil // already there
		}
		value = append(ids, device)
	} else if cur := intOf(before[field.name]); cur == device {
		return nil
	} else if cur != 0 {
		return fmt.Errorf("%s already has a thermostat (device %d); a room has only one", strOf(before["Name"]), cur)
	}

	body, _ := json.Marshal(map[string]any{field.name: value})
	if err := hubError(s.forward(ctx, "PATCH", path, body)); err != nil {
		return fmt.Errorf("the hub refused to attach it (%v); use the Wiser app for this step", err)
	}
	after, err := s.hubItem(ctx, "Room", room)
	if err == nil && hasDevice(after[field.name], device) {
		s.poller.Refresh("Device")
		return nil
	}
	// It didn't take: put the room back as it was.
	undo, _ := json.Marshal(map[string]any{field.name: before[field.name]})
	s.forward(ctx, "PATCH", path, undo)
	return fmt.Errorf("the hub accepted the change but didn't attach the %s; nothing was changed. Use the Wiser app for this step", kindName(kind))
}

// hubList reads a whole section straight from the hub, not the cache (the
// dashboard polls these lists already, so the hub is known to answer them).
func (s *Server) hubList(ctx context.Context, section string) ([]map[string]any, error) {
	res := s.hub.Do(ctx, "GET", "/data/domain/"+section, nil, true)
	if err := hubError(res); err != nil {
		return nil, fmt.Errorf("reading %s from the hub: %v", section, err)
	}
	var items []map[string]any
	if err := json.Unmarshal(res.Body, &items); err != nil {
		return nil, fmt.Errorf("reading %s from the hub: %v", section, err)
	}
	return items, nil
}

func (s *Server) hubItem(ctx context.Context, section string, id int) (map[string]any, error) {
	items, err := s.hubList(ctx, section)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		if intOf(it["id"]) == id {
			return it, nil
		}
	}
	return nil, fmt.Errorf("the hub has no %s %d", strings.ToLower(section), id)
}

func (s *Server) hubRooms(ctx context.Context) []map[string]any {
	rooms, _ := s.hubList(ctx, "Room")
	return rooms
}

func hubError(res result) error {
	if res.Err != nil {
		return res.Err
	}
	if res.Status < 200 || res.Status > 299 {
		return fmt.Errorf("HTTP %d %s", res.Status, strings.TrimSpace(string(res.Body)))
	}
	return nil
}

func idList(v any) []int {
	var out []int
	if l, ok := v.([]any); ok {
		for _, x := range l {
			out = append(out, intOf(x))
		}
	}
	return out
}

func hasDevice(v any, device int) bool {
	if intOf(v) == device && device != 0 {
		return true
	}
	for _, id := range idList(v) {
		if id == device {
			return true
		}
	}
	return false
}

func kindName(kind string) string {
	switch kind {
	case "RoomStat":
		return "room thermostat"
	case "iTRV":
		return "radiator valve"
	case "SmartPlug":
		return "smart plug"
	case "HeatingActuator":
		return "heating actuator"
	case "UnderFloorHeating":
		return "underfloor heating controller"
	case "":
		return "device"
	}
	return kind
}
