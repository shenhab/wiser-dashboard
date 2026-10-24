package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// guardTest is a dashboard with a fake hub behind it and the waste guard on.
type guardTest struct {
	t     *testing.T
	g     *Guard
	srv   *Server
	fh    *fakeHub
	store *Store
	dir   string
}

// sixteenAllWeek is a heating schedule at 16° all week, and the hot water
// on 06:30-07:00 every day.
const sixteenAllWeek = `{"Heating":[{"id":1,"Name":"Lounge","Monday":{"Time":[0],"DegreesC":[160]},"Tuesday":{"Time":[0],"DegreesC":[160]},
	"Wednesday":{"Time":[0],"DegreesC":[160]},"Thursday":{"Time":[0],"DegreesC":[160]},"Friday":{"Time":[0],"DegreesC":[160]},
	"Saturday":{"Time":[0],"DegreesC":[160]},"Sunday":{"Time":[0],"DegreesC":[160]}}],
	"OnOff":[{"id":1000,"Monday":[630,-700],"Tuesday":[630,-700],"Wednesday":[630,-700],"Thursday":[630,-700],"Friday":[630,-700],"Saturday":[630,-700],"Sunday":[630,-700]}]}`

// lounge is a room set to 23° by hand while its schedule says 16°, heating.
const lounge = `{"id":1,"Name":"Lounge","Mode":"Auto","CalculatedTemperature":190,"CurrentSetPoint":230,"ScheduledSetPoint":160,
	"SetpointOrigin":"FromManualOverride","OverrideType":"Manual","PercentageDemand":80,"ScheduleId":1}`

const hotWaterOff = `{"id":2,"Mode":"Auto","WaterHeatingState":"Off","ScheduledWaterHeatingState":"Off","HotWaterDescription":"FromSchedule","OverrideType":"None","ScheduleId":1000}`

func hubSystem(now time.Time, clockOff time.Duration) string {
	return fmt.Sprintf(`{"UnixTime":%d,"LocalDateAndTime":{"Day":%q,"Time":%d},"OverrideType":"None","ComfortModeEnabled":false,"PreheatTimeLimit":10800}`,
		now.Add(clockOff).Unix(), now.Weekday().String(), now.Hour()*100+now.Minute())
}

func newGuardTest(t *testing.T, rooms, hotWater string, s GuardSettings) *guardTest {
	t.Helper()
	fh := &fakeHub{}
	hubSrv := httptest.NewServer(fh)
	t.Cleanup(hubSrv.Close)
	hub := NewHub()
	hub.SetTarget(strings.TrimPrefix(hubSrv.URL, "http://"), "hub-secret")
	store := NewStore()
	gt := &guardTest{t: t, fh: fh, store: store, dir: t.TempDir()}
	gt.cache("Room", rooms)
	gt.cache("HotWater", "["+hotWater+"]")
	gt.cache("System", hubSystem(time.Now(), 0))
	store.set("schedules", []byte(sixteenAllWeek), 0)
	gt.srv = &Server{hub: hub, store: store, poller: NewPoller(hub, store), history: NewHistory(""), health: NewHealth(""), listen: "127.0.0.1:0"}
	gt.g = NewGuard(gt.srv, gt.dir, s)
	gt.srv.guard = gt.g
	return gt
}

func (gt *guardTest) cache(section, data string) {
	gt.store.set(section, []byte(data), 0)
	gt.fh.load(section, data)
}

func (gt *guardTest) tick() { gt.g.tick(context.Background(), time.Now()) }

// age moves a timer back, as if the room had been heating that long.
func (gt *guardTest) age(key string, d time.Duration) {
	gt.g.mu.Lock()
	defer gt.g.mu.Unlock()
	tr := gt.g.tracks[key]
	if tr == nil || tr.HeatFrom.IsZero() {
		gt.t.Fatalf("%s has no running timer", key)
	}
	tr.HeatFrom = tr.HeatFrom.Add(-d)
}

func (gt *guardTest) state(key string) guardStatus {
	gt.g.mu.Lock()
	defer gt.g.mu.Unlock()
	return gt.g.status[key]
}

func (gt *guardTest) kinds() []string {
	gt.g.mu.Lock()
	defer gt.g.mu.Unlock()
	var out []string
	for _, e := range gt.g.events {
		out = append(out, e.Kind)
	}
	return out
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestGuardReturnsARoom(t *testing.T) {
	gt := newGuardTest(t, "["+lounge+"]", hotWaterOff, GuardSettings{Mode: "on"})
	gt.tick()
	if st := gt.state("room-1"); st.State != "timer" || st.Baseline != 160 || st.Until == 0 {
		t.Fatalf("after the first look: %+v", st)
	}
	gt.tick()
	if p := gt.fh.take(); len(p) != 0 {
		t.Fatalf("sent %v before the time was up", p)
	}

	gt.age("room-1", 61*time.Minute)
	gt.tick()
	p := gt.fh.take()
	if len(p) != 1 || p[0]["path"] != "/data/domain/Room/1" || mustJSON(p[0]["ov"]) != `{"Type":"None"}` {
		t.Fatalf("going back sent %v", p)
	}
	var found bool
	for _, a := range gt.srv.alerts() {
		found = found || (a.Kind == "guard-returned" && a.Level == "warning" && strings.Contains(a.Title, "Lounge"))
	}
	if !found {
		t.Errorf("no alert after going back: %v", gt.srv.alerts())
	}

	gt.tick() // the cache now shows the schedule's 16°
	if st := gt.state("room-1"); st.State != vOnSchedule {
		t.Errorf("after going back: %+v", st)
	}
	if p := gt.fh.take(); len(p) != 0 {
		t.Errorf("sent again: %v", p)
	}
	if got, want := strings.Join(gt.kinds(), ","), "seen,heating,returned,back"; got != want {
		t.Errorf("activity = %s, want %s", got, want)
	}
}

func TestGuardWatchOnly(t *testing.T) {
	gt := newGuardTest(t, "["+lounge+"]", hotWaterOff, GuardSettings{Mode: "watch"})
	gt.tick()
	gt.age("room-1", 61*time.Minute)
	gt.tick()
	gt.tick()
	if p := gt.fh.take(); len(p) != 0 {
		t.Fatalf("watch only sent %v", p)
	}
	if st := gt.state("room-1"); st.State != "would-return" {
		t.Errorf("status %+v, want would-return", st)
	}
	if got, want := strings.Join(gt.kinds(), ","), "seen,heating,would-return"; got != want {
		t.Errorf("activity = %s, want %s (logged once)", got, want)
	}
	al := gt.g.Alerts()
	if len(al) != 1 || al[0].Level != "info" || !strings.HasPrefix(al[0].Title, "Watch only") {
		t.Errorf("alerts = %v", al)
	}
}

func TestGuardManualModeAndNoSchedule(t *testing.T) {
	manual := strings.NewReplacer(`"Mode":"Auto"`, `"Mode":"Manual"`, `"FromManualOverride"`, `"FromManualMode"`).Replace(lounge)
	noSched := `{"id":3,"Name":"Study","Mode":"Auto","CalculatedTemperature":150,"CurrentSetPoint":200,"SetpointOrigin":"FromManualOverride","OverrideType":"Manual","PercentageDemand":50,"ScheduleId":0}`
	gt := newGuardTest(t, "["+manual+","+noSched+"]", hotWaterOff, GuardSettings{Mode: "on", OffTemp: 120})
	gt.tick()
	gt.age("room-1", time.Hour)
	gt.age("room-3", time.Hour)
	gt.tick()
	got := map[string]string{}
	for _, p := range gt.fh.take() {
		got[p["path"].(string)] = mustJSON(p["ov"])
	}
	if want := `{"SetPoint":160,"Type":"Manual"}`; got["/data/domain/Room/1"] != want {
		t.Errorf("Manual mode went back with %s, want %s (mode kept, the schedule's value)", got["/data/domain/Room/1"], want)
	}
	if want := `{"SetPoint":120,"Type":"Manual"}`; got["/data/domain/Room/3"] != want {
		t.Errorf("no schedule went back with %s, want %s (the guard off temperature)", got["/data/domain/Room/3"], want)
	}
	gt.tick()
	if st := gt.state("room-3"); st.State != vOnSchedule {
		t.Errorf("at the off temperature: %+v", st)
	}
}

func TestGuardHotWater(t *testing.T) {
	onByHand := `{"id":2,"Mode":"Auto","WaterHeatingState":"On","ScheduledWaterHeatingState":"Off","HotWaterDescription":"FromManualOverride","OverrideType":"Manual","ScheduleId":1000}`
	room := strings.Replace(lounge, `"CurrentSetPoint":230`, `"CurrentSetPoint":160`, 1)
	gt := newGuardTest(t, "["+room+"]", onByHand, GuardSettings{Mode: "on", Minutes: 30})
	gt.tick()
	if st := gt.state("hotwater-2"); st.State != "timer" {
		t.Fatalf("hot water on by hand: %+v", st)
	}
	gt.age("hotwater-2", 31*time.Minute)
	gt.tick()
	p := gt.fh.take()
	if len(p) != 1 || p[0]["path"] != "/data/domain/HotWater/2" || mustJSON(p[0]["ov"]) != `{"Type":"None"}` {
		t.Fatalf("hot water went back with %v", p)
	}
	gt.tick()
	if st := gt.state("hotwater-2"); st.State != vOnSchedule {
		t.Errorf("after going back: %+v", st)
	}
}

func TestGuardLeavesAlone(t *testing.T) {
	boost := strings.Replace(lounge, `"FromManualOverride"`, `"FromBoost"`, 1)
	tests := []struct {
		name  string
		rooms string
		s     GuardSettings
		clock time.Duration
		state string
	}{
		{"a timed boost", "[" + boost + "]", GuardSettings{Mode: "on"}, 0, vRespected},
		{"an excluded room", "[" + lounge + "]", GuardSettings{Mode: "on", Exclude: map[string]bool{"room-1": true}}, 0, "excluded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gt := newGuardTest(t, tt.rooms, hotWaterOff, tt.s)
			gt.tick()
			if st := gt.state("room-1"); st.State != tt.state {
				t.Errorf("status %+v, want %s", st, tt.state)
			}
			if p := gt.fh.take(); len(p) != 0 {
				t.Errorf("sent %v", p)
			}
		})
	}
}

func TestGuardPausesOnAWrongClock(t *testing.T) {
	gt := newGuardTest(t, "["+lounge+"]", hotWaterOff, GuardSettings{Mode: "on"})
	gt.tick()
	gt.age("room-1", 2*time.Hour)
	gt.cache("System", hubSystem(time.Now(), time.Hour))
	gt.tick()
	if st := gt.state("room-1"); st.State != "paused" || !strings.Contains(st.Why, "clock") {
		t.Errorf("status %+v, want paused for the clock", st)
	}
	if p := gt.fh.take(); len(p) != 0 {
		t.Errorf("sent %v with a wrong clock", p)
	}
}

func TestGuardKeep(t *testing.T) {
	gt := newGuardTest(t, "["+lounge+"]", hotWaterOff, GuardSettings{Mode: "on"})
	gt.tick()
	gt.age("room-1", 59*time.Minute)
	if err := gt.g.Keep("room-1"); err != nil {
		t.Fatal(err)
	}
	gt.age("room-1", 30*time.Minute)
	gt.tick()
	if p := gt.fh.take(); len(p) != 0 {
		t.Errorf("sent %v within the kept time", p)
	}
	if st := gt.state("room-1"); st.State != "timer" || time.Until(time.Unix(st.Until, 0)) < 25*time.Minute {
		t.Errorf("after keep: %+v", st)
	}
	if err := gt.g.Keep("room-9"); err == nil {
		t.Error("keeping a room that isn't followed should fail")
	}
}

func TestGuardOwnChanges(t *testing.T) {
	room := strings.Replace(lounge, `"CurrentSetPoint":230`, `"CurrentSetPoint":160`, 1)
	for _, skip := range []bool{true, false} {
		t.Run(fmt.Sprintf("skip own %v", skip), func(t *testing.T) {
			gt := newGuardTest(t, "["+room+"]", hotWaterOff, GuardSettings{Mode: "on", SkipOwn: skip})
			gt.tick()
			// The dashboard's page sets 23°.
			ts := httptest.NewServer(gt.srv.routes())
			defer ts.Close()
			res, err := http.Post(ts.URL+"/api/request", "application/json",
				strings.NewReader(`{"method":"PATCH","path":"/data/domain/Room/1","body":{"RequestOverride":{"Type":"Manual","SetPoint":230}}}`))
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			gt.fh.take()
			gt.tick()
			want := "timer"
			if skip {
				want = "exempt"
			}
			if st := gt.state("room-1"); st.State != want {
				t.Errorf("status %+v, want %s", st, want)
			}
		})
	}
}

func TestGuardKeepsTimersAcrossRestarts(t *testing.T) {
	gt := newGuardTest(t, "["+lounge+"]", hotWaterOff, GuardSettings{Mode: "on"})
	gt.tick()
	gt.age("room-1", 50*time.Minute)
	gt.tick() // saves
	before := gt.g.tracks["room-1"].HeatFrom

	again := NewGuard(gt.srv, gt.dir, GuardSettings{Mode: "on"})
	tr := again.tracks["room-1"]
	if tr == nil || !tr.HeatFrom.Equal(before) {
		t.Fatalf("restored track %+v, want the timer from %v", tr, before)
	}
	if len(again.events) != 2 {
		t.Errorf("restored %d events, want 2", len(again.events))
	}
}

func TestGuardOffForgets(t *testing.T) {
	gt := newGuardTest(t, "["+lounge+"]", hotWaterOff, GuardSettings{Mode: "on"})
	gt.tick()
	gt.g.SetSettings(GuardSettings{})
	gt.tick()
	if len(gt.g.tracks) != 0 || gt.g.Alerts() != nil {
		t.Errorf("off kept %v / %v", gt.g.tracks, gt.g.Alerts())
	}
}

func TestGuardAlarms(t *testing.T) {
	warm := strings.ReplaceAll(sixteenAllWeek, `"DegreesC":[160]`, `"DegreesC":[210]`)
	room := strings.Replace(lounge, `"CurrentSetPoint":230`, `"CurrentSetPoint":210`, 1)
	gt := newGuardTest(t, "["+room+"]", hotWaterOff, GuardSettings{Mode: "on"})
	gt.store.set("schedules", []byte(warm), 0)

	// 13 hours of heating in the last 24, the Lounge asking for all of it.
	now := time.Now()
	for i := 24 * 60; i > 0; i-- {
		on := 0
		if i <= 13*60 {
			on = 1
		}
		gt.srv.history.append(Sample{T: now.Add(-time.Duration(i) * time.Minute).Unix(),
			Rooms: map[string][4]int{"1": {200, 210, on * 50, -1}}, Ch: map[string]int{"1": on}, HW: 0})
	}
	gt.tick()
	byKind := map[string]Alert{}
	for _, a := range gt.srv.alerts() {
		byKind[a.Kind] = a
	}
	if a := byKind["schedule-long"]; a.Figure != "the whole week" || !strings.Contains(a.Title, "Lounge") {
		t.Errorf("schedule alarm = %+v", a)
	}
	if a := byKind["heating-24h"]; a.Figure != "13 h" || !strings.Contains(a.Detail, "Lounge 13 h") {
		t.Errorf("24-hour alarm = %+v", a)
	}

	// It clears only half an hour under the limit.
	gt.g.mu.Lock()
	defer gt.g.mu.Unlock()
	for _, c := range []struct {
		limit int
		on    bool
	}{{13, true}, {14, false}} {
		// 13 h against a 13 h limit is on; against 14 h it's 60 min under: off.
		s := gt.g.settings
		s.DailyHours = c.limit
		gt.g.workOutAlarms(gt.store.Snapshot(), s.withDefaults(), now)
		if gt.g.heatAlarm != c.on {
			t.Errorf("limit %d h: alarm %v, want %v", c.limit, gt.g.heatAlarm, c.on)
		}
	}
	gt.g.heatAlarm = true
	s := gt.g.settings
	s.DailyHours = 13
	gt.g.workOutAlarms(gt.store.Snapshot(), s.withDefaults(), now.Add(20*time.Minute)) // 12 h 40 min: inside the margin
	if !gt.g.heatAlarm {
		t.Error("the alarm cleared inside the half-hour margin")
	}
}

func TestGuardAPI(t *testing.T) {
	gt := newGuardTest(t, "["+lounge+"]", hotWaterOff, GuardSettings{})
	ts := httptest.NewServer(gt.srv.routes())
	defer ts.Close()
	post := func(path, body string) map[string]any {
		res, err := http.Post(ts.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return out
	}
	if r := post("/api/guard/settings", `{"mode":"on","minutes":5}`); r["ok"] != false {
		t.Errorf("5 minutes accepted: %v", r)
	}
	if r := post("/api/guard/settings", `{"mode":"on","minutes":90}`); r["ok"] != true {
		t.Fatalf("valid settings refused: %v", r)
	}
	if s := gt.g.Settings(); s.Mode != "on" || s.Minutes != 90 {
		t.Errorf("settings now %+v", s)
	}
	gt.tick()
	if r := post("/api/guard/keep", `{"key":"room-1"}`); r["ok"] != true {
		t.Errorf("keep: %v", r)
	}
	res, err := http.Get(ts.URL + "/api/guard?onTemp=150")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var g struct {
		Settings GuardSettings
		Preview  []Alert
		Activity []GuardEvent
		State    struct{ Status map[string]guardStatus }
	}
	json.NewDecoder(res.Body).Decode(&g)
	if g.Settings.Minutes != 90 || g.Settings.OnTemp != defaultGuardOnTemp {
		t.Errorf("settings %+v", g.Settings)
	}
	if len(g.Preview) != 1 || g.Preview[0].Kind != "schedule-long" {
		t.Errorf("16° all week with 15° as on should be flagged: %v", g.Preview)
	}
	if len(g.Activity) == 0 || g.Activity[0].Kind != "kept" {
		t.Errorf("activity newest first: %v", g.Activity)
	}
	if g.State.Status["room-1"].State != "timer" {
		t.Errorf("status %v", g.State.Status)
	}
}

func TestMatterAlertPayload(t *testing.T) {
	alerts := []Alert{
		{Kind: "guard-returned", Level: "warning", Short: "Lounge back to 16°"},
		{Kind: "schedule-long", Level: "warning", Short: "Bedroom schedule on 16h"},
		{Kind: "guard-returned", Level: "info", Short: "Watch: ignored"},
		{Kind: "heating-24h", Level: "warning", Short: "Heating over 12h in 24h"},
		{Kind: "guard-returned", Level: "warning", Short: "Study back to 16°"},
	}
	h := 13.3
	a := matterAlert(alerts, nil, GuardSettings{MatterText: true}, &h)
	var names []string
	for _, x := range a.Texts {
		names = append(names, x.Text)
	}
	if !a.On || !a.Enabled || *a.Hours != 13.3 || strings.Join(names, "|") != "Heating over 12h in 24h|Lounge back to 16°|Bedroom schedule on 16h" {
		t.Errorf("payload = %+v %v", a, names)
	}
	// A later occurrence of the same alert is a new device; the same one keeps its device.
	again := alerts[0]
	again.Since = 1000
	if alertTextID(again) == alertTextID(alerts[0]) {
		t.Error("a new occurrence kept the old device")
	}
	if alertTextID(alerts[0]) != alertTextID(alerts[0]) {
		t.Error("the same occurrence changed device")
	}
	if a := matterAlert(alerts, nil, GuardSettings{MatterText: true, MatterNoAlert: true}, nil); a.Enabled || len(a.Texts) != 3 {
		t.Errorf("Heating alert hidden, texts kept: %+v", a)
	}
	if a := matterAlert(alerts, nil, GuardSettings{}, nil); len(a.Texts) != 0 || !a.On || a.Hours != nil {
		t.Errorf("without the text option = %+v", a)
	}
	off := map[string]bool{"guard-returned": true, "schedule-long": true, "heating-24h": true}
	if a := matterAlert(alerts, off, GuardSettings{MatterText: true}, nil); a.On || len(a.Texts) != 0 {
		t.Errorf("silenced alerts = %+v", a)
	}
}

func TestMatterName(t *testing.T) {
	if got := matterName("Ground Floor Living Room back to 16°"); len([]rune(got)) != 32 || !strings.HasSuffix(got, "…") {
		t.Errorf("matterName = %q (%d)", got, len([]rune(got)))
	}
	if got := matterName("Lounge back to 16°"); got != "Lounge back to 16°" {
		t.Errorf("short name changed: %q", got)
	}
}

func TestGuardSendsMatterDevices(t *testing.T) {
	gt := newGuardTest(t, "["+lounge+"]", hotWaterOff, GuardSettings{MatterHours: true})
	m := NewMatter(gt.srv.hub, "", t.TempDir(), 0, "127.0.0.1:0")
	gt.srv.matter = m
	now := time.Now()
	for i := 120; i > 0; i-- {
		gt.srv.history.append(Sample{T: now.Add(-time.Duration(i) * time.Minute).Unix(), Ch: map[string]int{"1": b2i(i <= 90)}})
	}
	// Guard off: no alert sensor, but the hours sensor still shows 1.5 h.
	gt.tick()
	if a := m.alert; a.Enabled || a.Hours == nil || *a.Hours != 1.5 {
		t.Fatalf("guard off: %+v", a)
	}
	// Guard on and the room returned: the alert sensor opens, named devices on request.
	gt.g.SetSettings(GuardSettings{Mode: "on", MatterText: true, MatterHours: true})
	gt.tick()
	gt.age("room-1", time.Hour)
	gt.tick()
	if a := m.alert; !a.Enabled || !a.On || len(a.Texts) != 1 || a.Texts[0].Text != "Lounge back to 16°" {
		t.Errorf("after going back: %+v", a)
	}
}

func TestRolling24h(t *testing.T) {
	h := NewHistory("")
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	// 48 hours of samples; the heating runs 06:00-10:00 every day.
	for m := 48 * 60; m > 0; m-- {
		ts := now.Add(-time.Duration(m) * time.Minute)
		on := b2i(ts.Hour() >= 6 && ts.Hour() < 10)
		h.samples = append(h.samples, Sample{T: ts.Unix(), Ch: map[string]int{"1": on}})
	}
	pts := h.Rolling24h(now)
	if len(pts) != 25 { // every hour from 24 h after the first sample to now
		t.Fatalf("%d points, want 25", len(pts))
	}
	last := pts[len(pts)-1]
	if last.T != now.Unix() || last.Heating != 240 || last.Covered != 24*60-1 { // the last sample is a minute before now
		t.Errorf("last point %+v, want 240 minutes over a full day", last)
	}
}

func TestStampAlarms(t *testing.T) {
	dir := t.TempDir()
	g := NewGuard(&Server{}, dir, GuardSettings{})
	t0 := time.Unix(1000, 0)
	day := []Alert{{Kind: "heating-24h", Title: "12 h 5 min of heating"}, {Kind: "schedule-long", Title: "Bedroom's schedule keeps it heated for 16 h"}}
	first := g.stampAlarms(append([]Alert(nil), day...), t0)
	// A minute later the figure in the title has moved on: still the same occurrence.
	later := []Alert{{Kind: "heating-24h", Title: "12 h 6 min of heating"}, day[1]}
	second := g.stampAlarms(later, t0.Add(time.Minute))
	if second[0].Since != first[0].Since || second[1].Since != first[1].Since {
		t.Errorf("occurrence changed: %v -> %v", first, second)
	}
	// It clears, then comes back: a new occurrence.
	g.stampAlarms(nil, t0.Add(2*time.Minute))
	third := g.stampAlarms(append([]Alert(nil), day...), t0.Add(3*time.Minute))
	if third[0].Since == first[0].Since {
		t.Error("an alarm that came back kept its old occurrence")
	}
	// Kept across a restart.
	g.mu.Lock()
	g.save()
	g.mu.Unlock()
	if again := NewGuard(&Server{}, dir, GuardSettings{}); again.alarmSince["heating-24h"] != third[0].Since {
		t.Errorf("after a restart: %v", again.alarmSince)
	}
}
