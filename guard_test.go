package main

import (
	"encoding/json"
	"testing"
	"time"
)

// at is a minute of the week: at(0, 6, 30) is Monday 06:30.
func at(day, h, m int) int { return day*1440 + h*60 + m }

// typical: 21° 06:30-09:00 and 17:00-22:30, 16° otherwise, every day.
func typical() []step {
	var s []step
	for d := 0; d < 7; d++ {
		s = append(s, step{at(d, 6, 30), 210}, step{at(d, 9, 0), 160}, step{at(d, 17, 0), 210}, step{at(d, 22, 30), 160})
	}
	return s
}

func TestParseSchedules(t *testing.T) {
	raw := json.RawMessage(`{"Heating":[{"id":2,"Monday":{"Time":[630,2200],"DegreesC":[210,-200]},"Tuesday":{"Time":[],"DegreesC":[]}}],
		"OnOff":[{"id":1000,"Monday":[630,-700],"Sunday":[2300]}]}`)
	got := parseSchedules(raw)
	if want := []step{{at(0, 6, 30), 210}, {at(0, 22, 0), -200}}; !equalSteps(got[2], want) {
		t.Errorf("heating = %v, want %v", got[2], want)
	}
	if want := []step{{at(0, 6, 30), 1}, {at(0, 7, 0), 0}, {at(6, 23, 0), 1}}; !equalSteps(got[1000], want) {
		t.Errorf("on/off = %v, want %v", got[1000], want)
	}
}

func equalSteps(a, b []step) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestScheduleAt(t *testing.T) {
	oneSlot := []step{{at(2, 7, 0), 200}} // Wednesday 07:00 20°, nothing else all week
	tests := []struct {
		name      string
		steps     []step
		m         int
		cur       int
		nextAt    int
		nextValue int
		until     int
		ok        bool
	}{
		{"morning", typical(), at(0, 7, 0), 210, at(0, 9, 0), 160, 120, true},
		{"exactly at a change", typical(), at(0, 9, 0), 160, at(0, 17, 0), 210, 480, true},
		{"night carries the evening's value past midnight", typical(), at(1, 3, 0), 160, at(1, 6, 30), 210, 210, true},
		{"Monday before its first change is still Sunday's", typical(), at(0, 0, 10), 160, at(0, 6, 30), 210, 380, true},
		{"Sunday night wraps to Monday", typical(), at(6, 23, 0), 160, at(0, 6, 30), 210, 450, true},
		{"one value all week", oneSlot, at(5, 12, 0), 200, 0, 0, -1, true},
		{"empty schedule", nil, at(1, 0, 0), 0, 0, 0, 0, false},
		{"minute past the end of the week wraps", typical(), weekMinutes + at(0, 7, 0), 210, at(0, 9, 0), 160, 120, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cur, next, until, ok := scheduleAt(tt.steps, tt.m)
			if cur != tt.cur || ok != tt.ok || until != tt.until || (until > 0 && (next.At != tt.nextAt || next.Value != tt.nextValue)) {
				t.Errorf("scheduleAt = %d, %v, %d, %v; want %d, {%d %d}, %d, %v", cur, next, until, ok, tt.cur, tt.nextAt, tt.nextValue, tt.until, tt.ok)
			}
		})
	}
}

func TestLongestOn(t *testing.T) {
	// The typical schedule with Monday's 22:30 drop missing: 21° from Monday
	// 17:00 until Tuesday 09:00.
	forgot := []step{}
	for _, s := range typical() {
		if s.At != at(0, 22, 30) {
			forgot = append(forgot, s)
		}
	}
	tests := []struct {
		name    string
		steps   []step
		on      int
		minutes int
		start   int
	}{
		{"typical schedule", typical(), 170, 330, at(0, 17, 0)},
		{"16° setback counts as on below the threshold", typical(), 160, weekMinutes, -1},
		{"forgotten evening drop", forgot, 170, 16 * 60, at(0, 17, 0)},
		{"on all week", []step{{0, 200}}, 170, weekMinutes, -1},
		{"off all week", []step{{0, -200}}, 170, 0, 0},
		{"on across Sunday into Monday", []step{{at(0, 8, 0), 160}, {at(6, 20, 0), 210}}, 170, 12 * 60, at(6, 20, 0)},
		{"hot water on 06:30-07:00", []step{{at(0, 6, 30), 1}, {at(0, 7, 0), 0}}, 1, 30, at(0, 6, 30)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, s := longestOn(tt.steps, tt.on)
			if m != tt.minutes || s != tt.start {
				t.Errorf("longestOn = %d from %s, want %d from %s", m, weekTime(s), tt.minutes, weekTime(tt.start))
			}
		})
	}
}

func ip(v int) *int { return &v }

func TestDecideRoom(t *testing.T) {
	env := guardEnv{Week: at(0, 20, 0)} // Monday 20:00: the typical schedule says 21°, next 16° at 22:30
	room := func(target int, origin string) roomView {
		return roomView{ID: 1, Name: "Lounge", Mode: "Auto", CalculatedTemperature: 190, CurrentSetPoint: target,
			ScheduledSetPoint: ip(210), SetpointOrigin: origin, ScheduleID: 1}
	}
	tests := []struct {
		name    string
		r       roomView
		steps   []step
		env     guardEnv
		kind    string
		base    int
		heating bool
	}{
		{"on schedule", room(210, "FromSchedule"), typical(), env, vOnSchedule, 210, true},
		{"set to the schedule's value by hand is on schedule", room(210, "FromManualOverride"), typical(), env, vOnSchedule, 210, true},
		{"higher by hand", room(230, "FromManualOverride"), typical(), env, vAbove, 210, true},
		{"higher, room already warm", func() roomView { r := room(230, "FromManualOverride"); r.CalculatedTemperature = 235; return r }(), typical(), env, vAbove, 210, false},
		{"lower by hand", room(180, "FromManualOverride"), typical(), env, vBelow, 210, false},
		{"turned off", room(-200, "FromManualOverride"), typical(), env, vBelow, 210, false},
		{"timed boost", room(250, "FromBoost"), typical(), env, vRespected, 0, true},
		{"away mode from the room", room(160, "FromAwayMode"), typical(), env, vRespected, 0, false},
		{"away mode from the system", room(230, "FromManualOverride"), typical(), guardEnv{Week: env.Week, Away: true}, vRespected, 0, true},
		{"open window", room(50, "FromWindowDetection"), typical(), env, vRespected, 0, false},
		{"Manual mode above the schedule", func() roomView { r := room(220, "FromManualMode"); r.Mode = "Manual"; return r }(), typical(), env, vAbove, 210, true},
		{"no schedule: the guard off temperature is the baseline", roomView{ID: 3, CurrentSetPoint: 200, CalculatedTemperature: 150}, nil, env, vAbove, 100, true},
		{"no schedule, at the off temperature", roomView{ID: 3, CurrentSetPoint: 100, CalculatedTemperature: 150}, nil, env, vOnSchedule, 100, false},
		{"schedule not read yet", roomView{ID: 3, CurrentSetPoint: 230, ScheduleID: 9}, nil, env, vRespected, 0, true},
		{"the hub's scheduled value wins", func() roomView { r := room(210, "FromManualOverride"); r.ScheduledSetPoint = ip(160); return r }(), typical(), env, vAbove, 160, true},
		{"computed from the schedule without the hub's value", func() roomView { r := room(230, "FromManualOverride"); r.ScheduledSetPoint = nil; return r }(), typical(), env, vAbove, 210, true},
		{"demand counts as heating", func() roomView {
			r := room(230, "FromManualOverride")
			r.CalculatedTemperature = 240
			r.PercentageDemand = 20
			return r
		}(), typical(), env, vAbove, 210, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := decideRoom(tt.r, tt.steps, tt.env)
			if v.Kind != tt.kind || (tt.kind != vRespected && v.Baseline != tt.base) || v.Heating != tt.heating {
				t.Errorf("decideRoom = %+v; want kind %s, baseline %d, heating %v", v, tt.kind, tt.base, tt.heating)
			}
		})
	}
}

func TestDecideRoomComfortMode(t *testing.T) {
	// Monday 16:00, one hour before 21° at 17:00: the hub may pre-heat.
	r := roomView{ID: 1, Mode: "Auto", CalculatedTemperature: 170, CurrentSetPoint: 210, ScheduledSetPoint: ip(160), SetpointOrigin: "FromSchedule", ScheduleID: 1}
	steps := typical()
	if v := decideRoom(r, steps, guardEnv{Week: at(0, 16, 0), Comfort: true, Preheat: 180}); v.Kind != vOnSchedule {
		t.Errorf("early start inside the limit = %+v, want on schedule", v)
	}
	if v := decideRoom(r, steps, guardEnv{Week: at(0, 13, 0), Comfort: true, Preheat: 180}); v.Kind != vAbove {
		t.Errorf("four hours early = %+v, want above", v)
	}
	if v := decideRoom(r, steps, guardEnv{Week: at(0, 16, 0), Comfort: false, Preheat: 180}); v.Kind != vAbove {
		t.Errorf("comfort mode off = %+v, want above", v)
	}
	r.SetpointOrigin = "FromComfortMode"
	if v := decideRoom(r, steps, guardEnv{Week: at(0, 13, 0)}); v.Kind != vRespected {
		t.Errorf("the hub saying comfort mode = %+v, want respected", v)
	}
}

func TestDecideHotWater(t *testing.T) {
	env := guardEnv{Week: at(0, 12, 0)}
	hwSteps := []step{{at(0, 6, 30), 1}, {at(0, 7, 0), 0}}
	w := func(on, sched, desc string) hotWaterView {
		return hotWaterView{ID: 2, Mode: "Auto", WaterHeatingState: on, ScheduledWaterHeatingState: sched, HotWaterDescription: desc, ScheduleID: 1000}
	}
	tests := []struct {
		name string
		w    hotWaterView
		env  guardEnv
		kind string
	}{
		{"off on schedule", w("Off", "Off", "FromSchedule"), env, vOnSchedule},
		{"on by schedule", w("On", "On", "FromSchedule"), env, vOnSchedule},
		{"on by hand", w("On", "Off", "FromManualOverride"), env, vAbove},
		{"boosted", w("On", "Off", "FromBoost"), env, vRespected},
		{"off by hand", w("Off", "On", "FromManualOverride"), env, vBelow},
		{"away turns it off", w("Off", "On", "FromAwayMode"), env, vRespected},
		{"computed from the schedule", hotWaterView{ID: 2, WaterHeatingState: "On", ScheduleID: 1000}, env, vAbove},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if v := decideHotWater(tt.w, hwSteps, tt.env); v.Kind != tt.kind {
				t.Errorf("decideHotWater = %+v, want %s", v, tt.kind)
			}
		})
	}
}

func TestGuardStep(t *testing.T) {
	t0 := time.Date(2026, 10, 23, 22, 0, 0, 0, time.UTC)
	s := GuardSettings{Mode: "on", Minutes: 60}
	above := verdict{Kind: vAbove, Target: 230, Baseline: 160}
	heating := above
	heating.Heating = true
	after := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }

	// One evening: 23° set at 22:00 while the room is warm, heating from
	// 23:10, back to the schedule at 00:10.
	type tick struct {
		at      int
		v       verdict
		act     string
		started bool
	}
	ticks := []tick{
		{0, above, aSeen, false},
		{30, above, aWatching, false},
		{70, heating, aTimer, true},
		{100, above, aTimer, false}, // the timer keeps running when it pauses
		{130, heating, aReturn, false},
	}
	var tr *guardTrack
	for _, tk := range ticks {
		var act string
		var started bool
		tr, act, started, _ = guardStep(tr, tk.v, false, after(tk.at), s)
		if act != tk.act || started != tk.started {
			t.Fatalf("at +%d min: act %q started %v, want %q %v", tk.at, act, started, tk.act, tk.started)
		}
	}
	if !tr.HeatFrom.Equal(after(70)) {
		t.Errorf("timer started %v, want %v", tr.HeatFrom, after(70))
	}

	// After going back: wait for the hub, retry, then give up.
	tr.Returns, tr.LastReturn = 1, after(130)
	if _, act, _, _ := guardStep(tr, heating, false, after(131), s); act != aReturning {
		t.Errorf("a minute after going back: %q, want %q", act, aReturning)
	}
	if _, act, _, _ := guardStep(tr, heating, false, after(133), s); act != aReturn {
		t.Errorf("still there after two minutes: %q, want %q", act, aReturn)
	}
	tr.Returns, tr.LastReturn = 3, after(140)
	if _, act, _, _ := guardStep(tr, heating, false, after(143), s); act != aFailed {
		t.Errorf("after three attempts: %q, want %q", act, aFailed)
	}

	// A new target is a new deliberate change: the timer starts over.
	higher := heating
	higher.Target = 240
	next, act, started, deadline := guardStep(tr, higher, false, after(150), s)
	if act != aSeen || !started || !deadline.Equal(after(210)) || next.Returns != 0 {
		t.Errorf("new target: %q started %v deadline %v returns %d", act, started, deadline, next.Returns)
	}

	// Back on schedule: nothing to follow.
	if next, act, _, _ := guardStep(tr, verdict{Kind: vOnSchedule}, false, after(160), s); next != nil || act != aNone {
		t.Errorf("on schedule: %v %q", next, act)
	}

	// The dashboard's own change, with own changes left alone.
	skip := s
	skip.SkipOwn = true
	if _, act, _, _ := guardStep(nil, heating, true, t0, skip); act != aExempt {
		t.Errorf("own change with SkipOwn: %q, want %q", act, aExempt)
	}
	if _, act, _, _ := guardStep(nil, heating, true, t0, s); act != aSeen {
		t.Errorf("own change without SkipOwn: %q, want %q", act, aSeen)
	}
}

func TestHeatingTotals(t *testing.T) {
	var samples []Sample
	for i := 0; i < 120; i++ {
		s := Sample{T: int64(i * 60), Rooms: map[string][4]int{"1": {200, 210, 0, -1}, "2": {180, 210, 0, -1}}, Ch: map[string]int{"1": 0, "3": 0}}
		if i < 90 {
			s.Ch["1"] = 1
			s.Rooms["2"] = [4]int{180, 210, 60, -1}
		}
		if i >= 60 && i < 100 {
			s.Ch["3"] = 1 // overlaps zone 1 for 30 minutes
			s.Rooms["1"] = [4]int{200, 210, 30, -1}
		}
		samples = append(samples, s)
	}
	house, rooms, covered := heatingTotals(samples)
	if house != 100 || rooms["1"] != 40 || rooms["2"] != 90 || covered != 120 {
		t.Errorf("heatingTotals = %d, %v, %d; want 100, {1:40 2:90}, 120", house, rooms, covered)
	}
}

func TestHubWeekMinute(t *testing.T) {
	now := time.Now()
	sys := json.RawMessage(`{"LocalDateAndTime":{"Day":"Sunday","Time":2355}}`)
	if m, ok := hubWeekMinute(sys, now.Add(-10*time.Minute), now); !ok || m != at(0, 0, 5) {
		t.Errorf("Sunday 23:55 read 10 minutes ago = %s, want Mon 00:05", weekTime(m))
	}
	if _, ok := hubWeekMinute(json.RawMessage(`{}`), now, now); ok {
		t.Error("no clock should not be ok")
	}
}

func TestGuardSettingsValidate(t *testing.T) {
	for _, bad := range []GuardSettings{{Mode: "maybe"}, {Minutes: 5}, {OffTemp: 300}, {OnTemp: 50}, {ScheduleHours: 30}, {DailyHours: 24}} {
		if bad.validate() == nil {
			t.Errorf("%+v should be refused", bad)
		}
	}
	if err := (GuardSettings{Mode: "on", Minutes: 90, OffTemp: 120}).validate(); err != nil {
		t.Errorf("valid settings refused: %v", err)
	}
}

func TestHM(t *testing.T) {
	for m, want := range map[int]string{45: "45 min", 60: "1 h", 860: "14 h 20 min"} {
		if got := hm(m); got != want {
			t.Errorf("hm(%d) = %q, want %q", m, got, want)
		}
	}
}
