package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// mcpTools are what the MCP server offers. Read tools work from the
// dashboard's cache and history; control tools send overrides to the hub.
var mcpTools = []mcpTool{
	// ---------------------------------------------------------------- read
	{
		name: "get_status", title: "Heating status", readOnly: true,
		description: "Current state of every room (temperature, target, whether it's heating, what set the target and until when), the hot water, the boiler and away mode.",
		schema:      schema(nil, nil),
		run: func(s *Server, _ context.Context, _ json.RawMessage) (any, error) {
			return s.mcpStatus()
		},
	},
	{
		name: "get_history", title: "Temperature history", readOnly: true,
		description: "Room temperatures and targets, and how much the heating and hot water ran, over the last hours (recorded once a minute, kept 14 days), averaged into intervals.",
		schema: schema(nil, map[string]any{
			"hours": intProp("How far back, default 24", 1, historyDays*24),
			"room":  roomProp,
		}),
		run: func(s *Server, _ context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Hours int    `json:"hours"`
				Room  string `json:"room"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.Hours == 0 {
				a.Hours = 24
			}
			if err := checkRange("hours", float64(a.Hours), 1, historyDays*24); err != nil {
				return nil, err
			}
			return s.mcpHistory(a.Hours, a.Room)
		},
	},
	{
		name: "get_boiler_cycles", title: "Boiler starts", readOnly: true,
		description: "Per day: how often the boiler and each heating zone and the hot water started, total minutes, runs shorter than 5 minutes (short cycling) and the longest run.",
		schema:      schema(nil, map[string]any{"days": intProp("How many days, default 7", 1, historyDays)}),
		run: func(s *Server, _ context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Days int `json:"days"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.Days == 0 {
				a.Days = 7
			}
			if err := checkRange("days", float64(a.Days), 1, historyDays); err != nil {
				return nil, err
			}
			return map[string]any{"short_run_minutes": shortRun, "days": s.history.Cycles(a.Days), "zones": s.zoneNames()}, nil
		},
	},
	{
		name: "get_warmup_rates", title: "Warm-up speed", readOnly: true,
		description: "How fast each room warms up while heating (°C per hour, median of recorded heat-ups of 20 minutes or more), useful to estimate how long a room needs.",
		schema:      schema(nil, nil),
		run: func(s *Server, _ context.Context, _ json.RawMessage) (any, error) {
			names := s.roomNames()
			out := map[string]any{}
			for id, r := range s.history.HeatRates() {
				out[nameOr(names, id)] = map[string]any{"degrees_per_hour": r.PerHour, "heat_ups_measured": r.Runs}
			}
			return map[string]any{"rooms": out}, nil
		},
	},
	{
		name: "get_alerts", title: "Alerts", readOnly: true,
		description: "Current problems and warnings: hub offline, low batteries, weak signal, cold rooms, open windows, hub restarts, Wi-Fi trouble, wrong clock, pending changes, away mode, empty schedules.",
		schema:      schema(nil, nil),
		run: func(s *Server, _ context.Context, _ json.RawMessage) (any, error) {
			al := s.alerts()
			if al == nil {
				al = []Alert{}
			}
			return map[string]any{"alerts": al}, nil
		},
	},
	{
		name: "get_hub_health", title: "Hub health", readOnly: true,
		description: "The hub's own connection health: latest Wi-Fi signal, retransmissions and drops, and the restarts, Wi-Fi drops and outages logged over the last days.",
		schema:      schema(nil, map[string]any{"days": intProp("How many days of events, default 7", 1, historyDays)}),
		run: func(s *Server, _ context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Days int `json:"days"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.Days == 0 {
				a.Days = 7
			}
			if err := checkRange("days", float64(a.Days), 1, historyDays); err != nil {
				return nil, err
			}
			samples, events := s.health.Since(time.Now().AddDate(0, 0, -a.Days))
			out := map[string]any{"events": []any{}}
			if n := len(samples); n > 0 {
				l := samples[n-1]
				out["latest"] = map[string]any{
					"time": time.Unix(l.T, 0).Format(time.RFC3339), "uptime_hours": l.Uptime / 3600, "wifi_signal_dbm": l.RSSI,
					"tcp_retransmit_percent": l.Retrans, "wifi_drops_since_boot": l.Drops, "access_point_not_found": l.ApMissing,
					"auth_failures": l.AuthFail, "free_memory_bytes": l.FreeHeap,
				}
			}
			var ev []any
			for _, e := range events {
				ev = append(ev, map[string]any{"time": time.Unix(e.T, 0).Format(time.RFC3339), "kind": e.Kind, "detail": e.Detail})
			}
			if ev != nil {
				out["events"] = ev
			}
			return out, nil
		},
	},
	{
		name: "get_schedules", title: "Schedules", readOnly: true,
		description: "The weekly heating schedules (time and temperature per day) and the hot water schedule (on/off times), and which rooms use which.",
		schema:      schema(nil, nil),
		run: func(s *Server, _ context.Context, _ json.RawMessage) (any, error) {
			return s.mcpSchedules()
		},
	},

	// ---------------------------------------------------------------- control
	{
		name: "set_room_temperature", title: "Set a room's temperature",
		description: "Set a room's target temperature, for some minutes or (without minutes) until its schedule next changes.",
		schema: schema([]string{"room", "temperature"}, map[string]any{
			"room":        roomProp,
			"temperature": numProp("Target in °C, half-degree steps", 5, 30),
			"minutes":     intProp("How long; leave out to hold until the schedule's next change", 15, 1440),
		}),
		run: func(s *Server, ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Room        string  `json:"room"`
				Temperature float64 `json:"temperature"`
				Minutes     int     `json:"minutes"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if err := checkRange("temperature", a.Temperature, 5, 30); err != nil {
				return nil, err
			}
			if err := checkMinutes(a.Minutes, 1440); err != nil {
				return nil, err
			}
			return s.roomOverride(ctx, a.Room, int(math.Round(a.Temperature*2))*5, a.Minutes)
		},
	},
	{
		name: "boost_room", title: "Boost a room",
		description: "Warm a room up for a while: its current temperature plus an increase (never lower than its current target), then back to the schedule. Defaults come from the dashboard's boost settings (normally +3° for 30 minutes).",
		schema: schema([]string{"room"}, map[string]any{
			"room":     roomProp,
			"minutes":  intProp("How long, default from the dashboard's boost settings", 15, 240),
			"increase": numProp("°C above the room's current temperature, default from the dashboard's boost settings", 0.5, 5),
		}),
		run: func(s *Server, ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Room     string  `json:"room"`
				Minutes  int     `json:"minutes"`
				Increase float64 `json:"increase"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			s.mu.Lock()
			b := s.boost.withDefaults()
			s.mu.Unlock()
			if a.Minutes == 0 {
				a.Minutes = b.Minutes
			}
			delta := b.Delta
			if a.Increase != 0 {
				if err := checkRange("increase", a.Increase, 0.5, 5); err != nil {
					return nil, err
				}
				delta = int(math.Round(a.Increase * 10))
			}
			if err := checkRange("minutes", float64(a.Minutes), 15, 240); err != nil {
				return nil, err
			}
			room, err := s.findRoom(a.Room)
			if err != nil {
				return nil, err
			}
			return s.roomOverride(ctx, a.Room, boostTarget(room, delta), a.Minutes)
		},
	},
	{
		name: "room_off", title: "Turn a room off",
		description: "Stop heating a room, for some minutes or (without minutes) until its schedule next changes.",
		schema: schema([]string{"room"}, map[string]any{
			"room":    roomProp,
			"minutes": intProp("How long; leave out to keep it off until the schedule's next change", 15, 1440),
		}),
		run: func(s *Server, ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Room    string `json:"room"`
				Minutes int    `json:"minutes"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if err := checkMinutes(a.Minutes, 1440); err != nil {
				return nil, err
			}
			return s.roomOverride(ctx, a.Room, -200, a.Minutes)
		},
	},
	{
		name: "resume_schedule", title: "Back to schedule",
		description: "Cancel any override (set temperature, boost, off) on a room, or on every room with room \"all\", so it follows its schedule again.",
		schema:      schema([]string{"room"}, map[string]any{"room": map[string]any{"type": "string", "description": "Room name or id, or \"all\" for every room"}}),
		run: func(s *Server, ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Room string `json:"room"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			var targets []map[string]any
			if strings.EqualFold(strings.TrimSpace(a.Room), "all") {
				rooms, err := s.roomList()
				if err != nil {
					return nil, err
				}
				targets = rooms
			} else {
				room, err := s.findRoom(a.Room)
				if err != nil {
					return nil, err
				}
				targets = []map[string]any{room}
			}
			var done []any
			for _, r := range targets {
				id := intOf(r["id"])
				if err := s.patchHub(ctx, fmt.Sprintf("/data/domain/Room/%d", id), map[string]any{"RequestOverride": map[string]any{"Type": "None"}}); err != nil {
					return nil, fmt.Errorf("%s: %v", strOf(r["Name"]), err)
				}
				done = append(done, s.roomAfter(id))
			}
			return map[string]any{"ok": true, "done": "back to schedule", "rooms": done}, nil
		},
	},
	{
		name: "hot_water", title: "Hot water",
		description: "Control the hot water: \"boost\" heats it for some minutes (default 30) then follows the schedule; \"on\" and \"off\" hold until the schedule's next change; \"schedule\" cancels any override.",
		schema: schema([]string{"action"}, map[string]any{
			"action":  map[string]any{"type": "string", "enum": []string{"boost", "on", "off", "schedule"}},
			"minutes": intProp("Boost length, default 30", 15, 480),
		}),
		run: func(s *Server, ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				Action  string `json:"action"`
				Minutes int    `json:"minutes"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			var hw []map[string]any
			if err := s.section("HotWater", &hw); err != nil || len(hw) == 0 {
				return nil, fmt.Errorf("this hub has no hot water")
			}
			id := intOf(hw[0]["id"])
			var ov map[string]any
			done := ""
			switch a.Action {
			case "boost":
				if a.Minutes == 0 {
					a.Minutes = 30
				}
				if err := checkRange("minutes", float64(a.Minutes), 15, 480); err != nil {
					return nil, err
				}
				ov = map[string]any{"Type": "Manual", "SetPoint": 1100, "DurationMinutes": a.Minutes, "Originator": "App"}
				done = "hot water boosted " + minutesText(a.Minutes)
			case "on", "off":
				if a.Minutes != 0 {
					return nil, fmt.Errorf("minutes only apply to \"boost\"")
				}
				sp := 1100
				if a.Action == "off" {
					sp = -200
				}
				ov = map[string]any{"Type": "Manual", "SetPoint": sp}
				done = "hot water " + a.Action + " until the schedule's next change"
			case "schedule":
				ov = map[string]any{"Type": "None"}
				done = "hot water back to schedule"
			default:
				return nil, fmt.Errorf("action must be boost, on, off or schedule")
			}
			if err := s.patchHub(ctx, fmt.Sprintf("/data/domain/HotWater/%d", id), map[string]any{"RequestOverride": ov}); err != nil {
				return nil, err
			}
			return map[string]any{"ok": true, "done": done}, nil
		},
	},
	{
		name: "set_away_mode", title: "Away mode",
		description: "Turn away mode on (rooms held at the hub's away temperature, and hot water off if the hub is set to) or off.",
		schema:      schema([]string{"on"}, map[string]any{"on": map[string]any{"type": "boolean"}}),
		run: func(s *Server, ctx context.Context, args json.RawMessage) (any, error) {
			var a struct {
				On *bool `json:"on"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return nil, err
			}
			if a.On == nil {
				return nil, fmt.Errorf("on is required")
			}
			var sys map[string]any
			s.section("System", &sys)
			ov := map[string]any{"Type": 0, "SetPoint": 0}
			done := "away mode off"
			if *a.On {
				away := 100
				if v, ok := num(sys["AwayModeSetPointLimit"]); ok {
					away = int(v)
				}
				ov = map[string]any{"Type": 2, "SetPoint": away}
				done = "away mode on"
				if away <= -200 {
					done += " (heating off while away)"
				} else {
					done += fmt.Sprintf(" (rooms held at %g°)", float64(away)/10)
				}
			}
			if err := s.patchHub(ctx, "/data/domain/System", map[string]any{"RequestOverride": ov}); err != nil {
				return nil, err
			}
			return map[string]any{"ok": true, "done": done}, nil
		},
	},
}

func checkMinutes(m, max int) error {
	if m == 0 {
		return nil
	}
	return checkRange("minutes", float64(m), 15, float64(max))
}

// boostTarget is the same rule as the Matter boost switches: the current
// temperature plus delta (tenths), never below the room's target, in the
// hub's half-degree steps, 5..30°.
func boostTarget(room map[string]any, delta int) int {
	target, hasTarget := num(room["CurrentSetPoint"])
	hasTarget = hasTarget && target > -200
	base := 200.0
	if t, ok := num(room["CalculatedTemperature"]); ok && t > -1000 {
		base = t
	} else if hasTarget {
		base = target
	}
	sp := base + float64(delta)
	if hasTarget && target > sp {
		sp = target
	}
	return int(math.Min(300, math.Max(50, math.Round(sp/5)*5)))
}

// roomOverride sets a room's setpoint (tenths; -200 is off) for minutes, or
// until the schedule's next change when minutes is 0.
func (s *Server) roomOverride(ctx context.Context, ref string, setpoint, minutes int) (any, error) {
	room, err := s.findRoom(ref)
	if err != nil {
		return nil, err
	}
	id := intOf(room["id"])
	ov := map[string]any{"Type": "Manual", "SetPoint": setpoint}
	if minutes > 0 {
		ov["DurationMinutes"] = minutes
		ov["Originator"] = "App"
	}
	if err := s.patchHub(ctx, fmt.Sprintf("/data/domain/Room/%d", id), map[string]any{"RequestOverride": ov}); err != nil {
		return nil, err
	}
	what := fmt.Sprintf("%s set to %g° %s", strOf(room["Name"]), float64(setpoint)/10, minutesText(minutes))
	if setpoint <= -200 {
		what = fmt.Sprintf("%s off %s", strOf(room["Name"]), minutesText(minutes))
	}
	return map[string]any{"ok": true, "done": what, "room": s.roomAfter(id)}, nil
}

// ---------------------------------------------------------------- read helpers

func (s *Server) roomNames() map[string]string {
	names := map[string]string{}
	rooms, _ := s.roomList()
	for _, r := range rooms {
		names[strconv.Itoa(intOf(r["id"]))] = strOf(r["Name"])
	}
	return names
}

// zoneNames lists the heating zones (channels) by id, with the dashboard's labels.
func (s *Server) zoneNames() map[string]string {
	var chans []map[string]any
	s.section("HeatingChannel", &chans)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for _, c := range chans {
		id := strconv.Itoa(intOf(c["id"]))
		name := s.labels[id]
		if name == "" {
			name = strOf(c["Name"])
		}
		out[id] = name
	}
	return out
}

func nameOr(names map[string]string, id string) string {
	if n := names[id]; n != "" {
		return n
	}
	return "room " + id
}

func (s *Server) mcpStatus() (any, error) {
	rooms, err := s.roomList()
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	var rs []any
	away := false
	for _, r := range rooms {
		rs = append(rs, roomStatus(r))
		away = away || strOf(r["SetpointOrigin"]) == "FromAwayMode"
	}
	out["rooms"] = rs
	out["away_mode"] = away

	var hw []map[string]any
	if s.section("HotWater", &hw) == nil && len(hw) > 0 {
		w := hw[0]
		h := map[string]any{"heating": strOf(w["WaterHeatingState"]) == "On", "mode": strOf(w["Mode"]), "override": strOf(w["OverrideType"])}
		if t := whenUnix(w["OverrideTimeoutUnixTime"]); t != nil && strOf(w["OverrideType"]) != "None" {
			h["override_until"] = t
		}
		out["hot_water"] = h
	}

	var chans []map[string]any
	s.section("HeatingChannel", &chans)
	zones := s.zoneNames()
	names := s.roomNames()
	boilerOn := false
	var zs []any
	for _, c := range chans {
		on := strOf(c["HeatingRelayState"]) == "On"
		boilerOn = boilerOn || on
		var roomNames []string
		if ids, ok := c["RoomIds"].([]any); ok {
			for _, id := range ids {
				roomNames = append(roomNames, nameOr(names, strconv.Itoa(intOf(id))))
			}
		}
		zs = append(zs, map[string]any{"name": zones[strconv.Itoa(intOf(c["id"]))], "relay_on": on, "rooms": roomNames})
	}
	if len(hw) > 0 && strOf(hw[0]["HotWaterRelayState"]) == "On" {
		boilerOn = true
	}
	out["zones"] = zs
	out["boiler_running"] = boilerOn

	e := s.store.Snapshot()["Room"]
	out["hub"] = map[string]any{
		"data_age_seconds": int(time.Since(e.Fetched).Seconds()),
		"fresh":            time.Since(e.Fetched) < staleAfter,
	}
	return out, nil
}

func (s *Server) mcpHistory(hours int, roomRef string) (any, error) {
	only := ""
	if roomRef != "" {
		room, err := s.findRoom(roomRef)
		if err != nil {
			return nil, err
		}
		only = strconv.Itoa(intOf(room["id"]))
	}
	names := s.roomNames()
	samples := s.history.Since(time.Now().Add(-time.Duration(hours)*time.Hour), 1<<30)
	if len(samples) == 0 {
		return map[string]any{"interval_minutes": 0, "points": []any{}, "note": "no history recorded yet"}, nil
	}
	// About 100 points at most, in round intervals of at least 10 minutes.
	step := int64(10 * 60)
	for _, m := range []int64{10, 15, 30, 60, 120, 180, 240} {
		step = m * 60
		if int64(hours)*3600/step <= 100 {
			break
		}
	}
	type acc struct {
		temp, n  map[string]int
		target   map[string]int
		demand   map[string]int
		heatOn   int
		hwOn, hw int
		total    int
	}
	var keys []int64
	buckets := map[int64]*acc{}
	for _, sm := range samples {
		k := sm.T / step * step
		b := buckets[k]
		if b == nil {
			b = &acc{temp: map[string]int{}, n: map[string]int{}, target: map[string]int{}, demand: map[string]int{}}
			buckets[k] = b
			keys = append(keys, k)
		}
		b.total++
		for id, v := range sm.Rooms {
			if only != "" && id != only {
				continue
			}
			if v[0] > -1000 && v[0] < 1000 {
				b.temp[id] += v[0]
				b.n[id]++
			}
			b.target[id] = v[1]
			b.demand[id] += v[2]
		}
		for _, on := range sm.Ch {
			if on == 1 {
				b.heatOn++
				break
			}
		}
		if sm.HW >= 0 {
			b.hw++
			b.hwOn += sm.HW
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	var points []any
	for _, k := range keys {
		b := buckets[k]
		rooms := map[string]any{}
		for id, n := range b.n {
			rooms[nameOr(names, id)] = map[string]any{
				"temperature":    math.Round(float64(b.temp[id])/float64(n)) / 10,
				"target":         celsius(float64(b.target[id])),
				"demand_percent": b.demand[id] / b.total,
			}
		}
		p := map[string]any{
			"time":            time.Unix(k, 0).Format("2006-01-02 15:04"),
			"rooms":           rooms,
			"heating_percent": 100 * b.heatOn / b.total,
		}
		if b.hw > 0 {
			p["hot_water_percent"] = 100 * b.hwOn / b.hw
		}
		points = append(points, p)
	}
	return map[string]any{
		"interval_minutes": step / 60,
		"note":             "temperature is the average over each interval, target the last one (null = off), heating_percent the share of the interval any heating zone ran",
		"points":           points,
	}, nil
}

func hhmm(t int) string {
	if t >= 2400 {
		t = 0
	}
	return fmt.Sprintf("%02d:%02d", t/100, t%100)
}

var weekDays = []string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}

func (s *Server) mcpSchedules() (any, error) {
	var all map[string][]map[string]any
	if err := s.section("schedules", &all); err != nil {
		return nil, err
	}
	// Which rooms and hot water use each schedule.
	usedBy := map[int][]string{}
	rooms, _ := s.roomList()
	for _, r := range rooms {
		id := intOf(r["ScheduleId"])
		usedBy[id] = append(usedBy[id], strOf(r["Name"]))
	}
	var hw []map[string]any
	s.section("HotWater", &hw)
	for _, w := range hw {
		id := intOf(w["ScheduleId"])
		usedBy[id] = append(usedBy[id], "hot water")
	}

	var out []any
	for kind, list := range all {
		for _, sc := range list {
			id := intOf(sc["id"])
			days := map[string]any{}
			for _, d := range weekDays {
				switch kind {
				case "OnOff":
					// [630, -700]: on at 06:30, off at 07:00
					raw, _ := sc[d].([]any)
					var slots []any
					var on string
					for _, v := range raw {
						t := intOf(v)
						if t > 0 {
							on = hhmm(t)
						} else if on != "" {
							slots = append(slots, map[string]string{"on": on, "off": hhmm(-t)})
							on = ""
						}
					}
					if on != "" {
						slots = append(slots, map[string]string{"on": on, "off": "24:00"})
					}
					if slots == nil {
						slots = []any{}
					}
					days[d] = slots
				default:
					day, _ := sc[d].(map[string]any)
					times, _ := day["Time"].([]any)
					temps, _ := day["DegreesC"].([]any)
					var steps []any
					for i, t := range times {
						var temp any = "off"
						if i < len(temps) {
							if c := celsius(temps[i]); c != nil {
								temp = c
							}
						}
						steps = append(steps, map[string]any{"from": hhmm(intOf(t)), "temperature": temp})
					}
					if steps == nil {
						steps = []any{}
					}
					days[d] = steps
				}
			}
			out = append(out, map[string]any{"id": id, "name": strOf(sc["Name"]), "type": kind, "used_by": usedBy[id], "days": days})
		}
	}
	return map[string]any{"schedules": out}, nil
}
