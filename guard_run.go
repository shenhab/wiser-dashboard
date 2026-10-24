package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Guard applies the waste guard's decisions (guard.go) to the hub. It looks
// at the cache after every poll, keeps its timers in guard.json so a restart
// doesn't reset them, and logs what it saw and did to guard.jsonl.
type Guard struct {
	srv      *Server
	path     string // guard.json
	logPath  string // guard.jsonl
	interval time.Duration

	mu         sync.Mutex
	settings   GuardSettings
	tracks     map[string]*guardTrack
	status     map[string]guardStatus
	events     []GuardEvent
	returned   map[string]guardReturn // key -> the last time it went back (alert for an hour)
	failed     map[string]string      // key -> name: couldn't go back
	own        map[string]ownWrite    // key -> the dashboard's last change to it
	heatAlarm  bool                   // the 24-hour alarm is on (it clears with a margin)
	alarms     []Alert                // schedule and 24-hour alarms, worked out once a minute
	alarmsAt   time.Time
	alarmSince map[string]int64 // alarm -> unix time it began
	saved      []byte
}

// guardStatus is what the dashboard shows for a room or the hot water.
type guardStatus struct {
	Key      string `json:"key"`
	Name     string `json:"name"`
	State    string `json:"state"` // a verdict kind, or watching, timer, would-return, returning, failed, exempt, excluded, paused
	Why      string `json:"why"`
	Target   int    `json:"target"`
	Baseline int    `json:"baseline"`
	Until    int64  `json:"until,omitempty"` // unix time the timer runs out
	Since    int64  `json:"since,omitempty"` // unix time the change was first seen
}

// GuardEvent is one line of the activity log.
type GuardEvent struct {
	T      int64  `json:"t"`
	Key    string `json:"key"`
	Name   string `json:"name"`
	Kind   string `json:"kind"` // seen, heating, returned, would-return, kept, failed, back, error
	Text   string `json:"text"`
	Origin string `json:"origin,omitempty"` // the hub's reason for the target, to check the rules against
}

type guardReturn struct {
	At    time.Time `json:"at"`
	Name  string    `json:"name"`
	Text  string    `json:"text"`
	Short string    `json:"short,omitempty"` // for Matter: "Lounge back to 16°"
	Watch bool      `json:"watch,omitempty"` // watch only: it would have gone back
}

type ownWrite struct {
	SetPoint int
	At       time.Time
}

// guardSaved is guard.json.
type guardSaved struct {
	Tracks    map[string]*guardTrack `json:"tracks"`
	Returned  map[string]guardReturn `json:"returned,omitempty"`
	HeatAlarm bool                   `json:"heatAlarm,omitempty"`
	Alarms    map[string]int64       `json:"alarms,omitempty"`
}

const (
	guardEventsKept = 500
	ownWriteWindow  = 10 * time.Minute
	guardFreshData  = 2 * time.Minute
)

func NewGuard(srv *Server, dir string, s GuardSettings) *Guard {
	g := &Guard{srv: srv, settings: s, interval: 10 * time.Second,
		tracks: map[string]*guardTrack{}, status: map[string]guardStatus{}, returned: map[string]guardReturn{},
		failed: map[string]string{}, own: map[string]ownWrite{}, alarmSince: map[string]int64{}}
	if dir != "" {
		g.path, g.logPath = filepath.Join(dir, "guard.json"), filepath.Join(dir, "guard.jsonl")
		g.load()
	}
	return g
}

func (g *Guard) load() {
	if b, err := os.ReadFile(g.path); err == nil {
		var d guardSaved
		if json.Unmarshal(b, &d) == nil {
			for k, t := range d.Tracks {
				if t != nil {
					g.tracks[k] = t
				}
			}
			for k, r := range d.Returned {
				g.returned[k] = r
			}
			g.heatAlarm = d.HeatAlarm
			for k, v := range d.Alarms {
				g.alarmSince[k] = v
			}
		}
	}
	f, err := os.Open(g.logPath)
	if err != nil {
		return
	}
	defer f.Close()
	cutoff := time.Now().AddDate(0, 0, -historyDays).Unix()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e GuardEvent
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.T >= cutoff {
			g.events = append(g.events, e)
		}
	}
	if len(g.events) > guardEventsKept {
		g.events = g.events[len(g.events)-guardEventsKept:]
	}
	// Rewrite without the expired lines so the file doesn't grow forever.
	tmp := g.logPath + ".tmp"
	if out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); err == nil {
		w := bufio.NewWriter(out)
		enc := json.NewEncoder(w)
		for _, e := range g.events {
			enc.Encode(e)
		}
		w.Flush()
		out.Close()
		os.Rename(tmp, g.logPath)
	}
}

// save writes guard.json when something changed. Call with g.mu held.
func (g *Guard) save() {
	if g.path == "" {
		return
	}
	b, err := json.Marshal(guardSaved{Tracks: g.tracks, Returned: g.returned, HeatAlarm: g.heatAlarm, Alarms: g.alarmSince})
	if err != nil || string(b) == string(g.saved) {
		return
	}
	tmp := g.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("guard: %v", err)
		return
	}
	if err := os.Rename(tmp, g.path); err != nil {
		log.Printf("guard: %v", err)
		return
	}
	g.saved = b
}

// logEvent adds to the activity log. Call with g.mu held.
func (g *Guard) logEvent(now time.Time, key, name, kind, text, origin string) {
	e := GuardEvent{T: now.Unix(), Key: key, Name: name, Kind: kind, Text: text, Origin: origin}
	g.events = append(g.events, e)
	if len(g.events) > guardEventsKept {
		g.events = g.events[len(g.events)-guardEventsKept:]
	}
	log.Printf("guard: %s: %s", name, text)
	if g.logPath == "" {
		return
	}
	f, err := os.OpenFile(g.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	json.NewEncoder(f).Encode(e)
	f.Close()
}

func (g *Guard) Settings() GuardSettings {
	if g == nil {
		return GuardSettings{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.settings
}

func (g *Guard) SetSettings(s GuardSettings) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.settings = s
	g.alarmsAt = time.Time{} // work the alarms out again with the new values
}

// Run checks the house every few seconds until ctx ends.
func (g *Guard) Run(ctx context.Context) {
	t := time.NewTicker(g.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.tick(ctx, time.Now())
		}
	}
}

// guardCommand is a change the guard sends to the hub.
type guardCommand struct {
	key, name, path, text, short string
	body                         map[string]any
}

// tick looks at the cache once and acts on what it finds.
func (g *Guard) tick(ctx context.Context, now time.Time) {
	snap := g.srv.store.Snapshot()
	g.srv.mu.Lock()
	alertOff := g.srv.alertSettings.Off
	g.srv.mu.Unlock()

	g.mu.Lock()
	s := g.settings.withDefaults()
	// The last 24 hours' heating, for the Matter "temperature" sensor. It
	// works whether or not the guard is on.
	var hours *float64
	if s.MatterHours && g.srv.history != nil {
		house, _, covered := heatingTotals(g.srv.history.Since(now.Add(-24*time.Hour), 0))
		if covered >= 60 {
			h := math.Round(float64(house)/6) / 10
			hours = &h
		}
	}
	if s.Mode == "" {
		// Off: forget everything, so turning it on starts with fresh timers.
		g.tracks, g.status, g.failed, g.returned, g.alarms, g.heatAlarm = map[string]*guardTrack{}, map[string]guardStatus{}, map[string]string{}, map[string]guardReturn{}, nil, false
		g.alarmSince = map[string]int64{}
		g.save()
		g.mu.Unlock()
		g.srv.matter.SetAlert(MatterAlert{Hours: hours})
		return
	}
	cmds := g.evaluate(snap, s, now)
	if now.Sub(g.alarmsAt) >= time.Minute {
		g.alarms = g.stampAlarms(g.workOutAlarms(snap, s, now), now)
		g.alarmsAt = now
	}
	g.save()
	g.mu.Unlock()

	for _, c := range cmds {
		g.send(ctx, c, now)
	}

	g.srv.matter.SetAlert(matterAlert(g.Alerts(), alertOff, s, hours))
}

// matterAlert is what the Matter bridge shows: the Heating alert sensor, open
// while a guard alert is active; optionally up to three devices named with the
// alerts (the 24 hours' heating first); and optionally the hours sensor.
func matterAlert(alerts []Alert, off map[string]bool, s GuardSettings, hours *float64) MatterAlert {
	a := MatterAlert{Enabled: !s.MatterNoAlert, Hours: hours}
	var texts []MatterText
	for _, al := range alerts {
		if al.Level == "info" || off[al.Kind] {
			continue
		}
		a.On = true
		if s.MatterText && al.Short != "" {
			t := MatterText{ID: alertTextID(al), Text: al.Short}
			if al.Kind == "heating-24h" {
				texts = append([]MatterText{t}, texts...)
			} else {
				texts = append(texts, t)
			}
		}
	}
	seen := map[string]bool{}
	for _, t := range texts {
		if !seen[t.ID] && len(a.Texts) < 3 {
			seen[t.ID] = true
			a.Texts = append(a.Texts, t)
		}
	}
	return a
}

// alertTextID names one occurrence of an alert: the same alert keeps its
// device while it lasts, and a later one gets a new device the apps announce.
func alertTextID(a Alert) string {
	h := fnv.New32a()
	fmt.Fprintf(h, "%s|%s|%d", a.Kind, a.Short, a.Since)
	return fmt.Sprintf("%08x", h.Sum32())
}

// stampAlarms gives each alarm the time it began, kept while it lasts (and
// across restarts), so its Matter device doesn't come back as new. Call with
// g.mu held.
func (g *Guard) stampAlarms(alarms []Alert, now time.Time) []Alert {
	seen := map[string]bool{}
	for i, a := range alarms {
		key := a.Kind
		if a.Kind != "heating-24h" {
			key += "|" + a.Title
		}
		since, ok := g.alarmSince[key]
		if !ok {
			since = now.Unix()
			g.alarmSince[key] = since
		}
		alarms[i].Since = since
		seen[key] = true
	}
	for k := range g.alarmSince {
		if !seen[k] {
			delete(g.alarmSince, k)
		}
	}
	return alarms
}

// evaluate decides for every room and the hot water. It returns the commands
// to send. Call with g.mu held.
func (g *Guard) evaluate(snap map[string]Entry, s GuardSettings, now time.Time) []guardCommand {
	room, sysE, schedE := snap["Room"], snap["System"], snap["schedules"]
	pause := ""
	switch {
	case room.Data == nil || sysE.Data == nil || schedE.Data == nil:
		pause = "waiting for the hub's data"
	case room.Restored || now.Sub(room.Fetched) > guardFreshData:
		pause = "waiting for fresh data from the hub"
	}
	week, ok := hubWeekMinute(sysE.Data, sysE.Fetched, now)
	if pause == "" && !ok {
		pause = "the hub didn't report its time"
	}
	if drift, ok := hubClockDrift(sysE.Data, sysE.Fetched, now); pause == "" && ok && drift > 5*time.Minute {
		pause = "the hub's clock is wrong"
	}
	if pause != "" {
		for k, st := range g.status {
			st.State, st.Why, st.Until = "paused", pause, 0
			g.status[k] = st
		}
		return nil
	}

	var sys struct {
		OverrideType            string
		ComfortModeEnabled      bool
		PreheatTimeLimit        int
		AwayModeAffectsHotWater bool
	}
	json.Unmarshal(sysE.Data, &sys)
	env := guardEnv{Week: week, Away: sys.OverrideType == "Away", Comfort: sys.ComfortModeEnabled,
		Preheat: sys.PreheatTimeLimit / 60, AwayHW: sys.AwayModeAffectsHotWater, Settings: s}
	scheds := parseSchedules(schedE.Data)

	seen := map[string]bool{}
	var cmds []guardCommand
	var rooms []roomView
	json.Unmarshal(room.Data, &rooms)
	for _, r := range rooms {
		key := fmt.Sprintf("room-%d", r.ID)
		seen[key] = true
		v := decideRoom(r, scheds[r.ScheduleID], env)
		if c := g.follow(key, r.Name, v, r.SetpointOrigin, s, now, false); c != nil {
			c.path = fmt.Sprintf("/data/domain/Room/%d", r.ID)
			switch {
			case v.NoSched:
				c.body = map[string]any{"RequestOverride": map[string]any{"Type": "Manual", "SetPoint": s.OffTemp}}
			case v.Manual:
				c.body = map[string]any{"RequestOverride": map[string]any{"Type": "Manual", "SetPoint": v.Baseline}}
			default:
				c.body = map[string]any{"RequestOverride": map[string]any{"Type": "None"}}
			}
			cmds = append(cmds, *c)
		}
	}
	var hws []hotWaterView
	json.Unmarshal(snap["HotWater"].Data, &hws)
	for _, w := range hws {
		key := fmt.Sprintf("hotwater-%d", w.ID)
		seen[key] = true
		v := decideHotWater(w, scheds[w.ScheduleID], env)
		if c := g.follow(key, "Hot water", v, w.HotWaterDescription, s, now, true); c != nil {
			c.path = fmt.Sprintf("/data/domain/HotWater/%d", w.ID)
			if v.Manual || v.NoSched {
				c.body = map[string]any{"RequestOverride": map[string]any{"Type": "Manual", "SetPoint": -200}}
			} else {
				c.body = map[string]any{"RequestOverride": map[string]any{"Type": "None"}}
			}
			cmds = append(cmds, *c)
		}
	}
	for k := range g.status {
		if !seen[k] {
			delete(g.status, k)
			delete(g.tracks, k)
		}
	}
	return cmds
}

// follow moves one room's (or the hot water's) timer on and logs what
// changed. It returns a command when it's time to go back. Call with g.mu held.
func (g *Guard) follow(key, name string, v verdict, origin string, s GuardSettings, now time.Time, hw bool) *guardCommand {
	st := guardStatus{Key: key, Name: name, State: v.Kind, Why: v.Why, Target: v.Target, Baseline: v.Baseline}
	prev := g.tracks[key]
	if s.Exclude[key] {
		st.State, st.Why = "excluded", "not covered by the waste guard"
		delete(g.tracks, key)
		delete(g.failed, key)
		g.status[key] = st
		return nil
	}
	val := func(x int) string {
		if hw {
			return map[int]string{0: "off", 1: "on"}[x]
		}
		return degText(x)
	}
	own := false
	if prev == nil || prev.Target != v.Target {
		if o, ok := g.own[key]; ok && now.Sub(o.At) < ownWriteWindow && (o.SetPoint == v.Target || (hw && o.SetPoint > 0 && v.Target == 1)) {
			own = true
		}
	}
	next, act, started, deadline := guardStep(prev, v, own, now, s)
	if next == nil {
		delete(g.tracks, key)
		delete(g.failed, key)
		if prev != nil {
			text := "back to " + val(v.Target) + " (" + v.Why + ")"
			if !prev.LastReturn.IsZero() {
				text = "now at " + val(v.Target) + ", as the schedule says"
				if hw {
					text = "now " + val(v.Target) + ", as the schedule says"
				}
			}
			g.logEvent(now, key, name, "back", text, origin)
		}
		g.status[key] = st
		return nil
	}
	g.tracks[key] = next
	st.Since = next.Seen.Unix()
	if !deadline.IsZero() {
		st.Until = deadline.Unix()
	}
	if act == aSeen {
		text := fmt.Sprintf("%s %s (%s), above the schedule's %s", map[bool]string{true: "turned", false: "set to"}[hw], val(v.Target), v.Why, val(v.Baseline))
		if next.Own {
			text += "; made from the dashboard, Google/Apple/Alexa or an AI assistant"
		}
		g.logEvent(now, key, name, "seen", text, origin)
	}
	if started {
		g.logEvent(now, key, name, "heating", fmt.Sprintf("started heating; back to %s at %s unless kept", val(v.Baseline), deadline.Local().Format("15:04")), origin)
	}
	cmd := (*guardCommand)(nil)
	switch act {
	case aSeen, aTimer:
		st.State = "timer"
		if next.HeatFrom.IsZero() {
			st.State = "watching"
		}
	case aWatching:
		st.State = "watching"
	case aExempt:
		st.State, st.Why = "exempt", "made from the dashboard, and those are left alone"
	case aReturning:
		st.State = "returning"
	case aFailed:
		st.State = "failed"
		if _, done := g.failed[key]; !done {
			g.failed[key] = name
			g.logEvent(now, key, name, "failed", fmt.Sprintf("still at %s after %d attempts to go back to %s", val(v.Target), next.Returns, val(v.Baseline)), origin)
		}
	case aReturn:
		mins := int(now.Sub(next.HeatFrom).Round(time.Minute) / time.Minute)
		text := fmt.Sprintf("%s for %s above the schedule, back to %s", map[bool]string{true: "on", false: "heated"}[hw], hm(mins), val(v.Baseline))
		if s.Mode == "watch" {
			st.State = "would-return"
			if !next.Watched {
				next.Watched = true
				g.returned[key] = guardReturn{At: now, Name: name, Text: "would have gone back to " + val(v.Baseline), Watch: true}
				g.logEvent(now, key, name, "would-return", "watch only: "+text, origin)
			}
			break
		}
		st.State = "returning"
		next.Returns++
		next.LastReturn = now
		short := name + " back to " + val(v.Baseline)
		if hw {
			short = "Hot water back " + val(v.Baseline)
		}
		cmd = &guardCommand{key: key, name: name, text: text, short: matterName(short)}
	}
	g.status[key] = st
	return cmd
}

// send goes back to the schedule through the dashboard's queue.
func (g *Guard) send(ctx context.Context, c guardCommand, now time.Time) {
	b, _ := json.Marshal(c.body)
	cctx, cancel := context.WithTimeout(withSource(ctx, "guard"), 60*time.Second)
	defer cancel()
	res := g.srv.forward(cctx, "PATCH", c.path, b)
	g.mu.Lock()
	defer g.mu.Unlock()
	if res.Err != nil || res.Status < 200 || res.Status > 299 {
		msg := fmt.Sprintf("HTTP %d", res.Status)
		if res.Err != nil {
			msg = res.Err.Error()
		}
		g.logEvent(now, c.key, c.name, "error", "couldn't go back: "+msg, "")
		return
	}
	g.returned[c.key] = guardReturn{At: now, Name: c.name, Text: c.text, Short: c.short}
	g.logEvent(now, c.key, c.name, "returned", c.text, "")
	g.save()
}

// Keep restarts a timer: another N minutes from now.
func (g *Guard) Keep(key string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	t := g.tracks[key]
	if t == nil {
		return fmt.Errorf("nothing to keep: it's on its schedule")
	}
	now := time.Now()
	if !t.HeatFrom.IsZero() {
		t.HeatFrom = now
	}
	t.Returns, t.LastReturn, t.Watched = 0, time.Time{}, false
	delete(g.failed, key)
	name := g.status[key].Name
	m := g.settings.withDefaults().Minutes
	g.logEvent(now, key, name, "kept", "kept for another "+hm(m), "")
	if st, ok := g.status[key]; ok && !t.HeatFrom.IsZero() {
		st.State, st.Until = "timer", now.Add(time.Duration(m)*time.Minute).Unix()
		g.status[key] = st
	}
	g.save()
	return nil
}

// noteOwn remembers a change made from the dashboard, Matter or MCP, so the
// guard can tell it apart when it sees it.
func (g *Guard) noteOwn(path string, body []byte) {
	if g == nil {
		return
	}
	section, id, ok := parseDomainPath(path)
	if !ok || id < 0 || (section != "Room" && section != "HotWater") {
		return
	}
	var req struct {
		RequestOverride *struct {
			Type     any
			SetPoint *int
		}
	}
	if json.Unmarshal(body, &req) != nil || req.RequestOverride == nil || req.RequestOverride.SetPoint == nil {
		return
	}
	key := "room-" + strconv.FormatInt(id, 10)
	if section == "HotWater" {
		key = "hotwater-" + strconv.FormatInt(id, 10)
	}
	g.mu.Lock()
	g.own[key] = ownWrite{SetPoint: *req.RequestOverride.SetPoint, At: time.Now()}
	g.mu.Unlock()
}

// ---------------------------------------------------------------- alarms

// workOutAlarms finds schedules that stay on too long and too much heating in
// the last 24 hours. Call with g.mu held.
func (g *Guard) workOutAlarms(snap map[string]Entry, s GuardSettings, now time.Time) []Alert {
	out := scheduleAlarms(snap, s)
	if g.srv.history == nil {
		return out
	}
	house, rooms, covered := heatingTotals(g.srv.history.Since(now.Add(-24*time.Hour), 0))
	limit := s.DailyHours * 60
	switch {
	case covered < 60:
		// Too little history to judge.
	case house >= limit:
		g.heatAlarm = true
	case house < limit-30:
		g.heatAlarm = false
	}
	if g.heatAlarm && covered >= 60 {
		names := roomNamesFrom(snap)
		type rm struct {
			name string
			m    int
		}
		var top []rm
		for id, m := range rooms {
			if m > 0 {
				top = append(top, rm{nameOr(names, id), m})
			}
		}
		sort.Slice(top, func(i, j int) bool { return top[i].m > top[j].m })
		var parts []string
		for i, r := range top {
			if i == 3 {
				break
			}
			parts = append(parts, r.name+" "+hm(r.m))
		}
		detail := fmt.Sprintf("Your limit is %d h.", s.DailyHours)
		if len(parts) > 0 {
			detail += " Asking for heat longest: " + strings.Join(parts, ", ") + "."
		}
		out = append(out, Alert{Kind: "heating-24h", Level: "warning", Title: hm(house) + " of heating in the last 24 hours",
			Detail: detail, Figure: hm(house), Short: fmt.Sprintf("Heating over %dh in 24h", s.DailyHours)})
	}
	return out
}

// scheduleAlarms lists the schedules that keep a room at or above the
// "considered on" temperature, or the hot water on, for too long.
func scheduleAlarms(snap map[string]Entry, s GuardSettings) []Alert {
	s = s.withDefaults()
	scheds := parseSchedules(snap["schedules"].Data)
	limit := s.ScheduleHours * 60
	var out []Alert
	describe := func(minutes, start int, what string) string {
		if start < 0 {
			return what + " all week."
		}
		return fmt.Sprintf("%s from %s to %s.", what, weekTime(start), weekTime(start+minutes))
	}
	var rooms []roomView
	json.Unmarshal(snap["Room"].Data, &rooms)
	for _, r := range rooms {
		steps := scheds[r.ScheduleID]
		if s.Exclude[fmt.Sprintf("room-%d", r.ID)] || len(steps) == 0 {
			continue
		}
		if m, start := longestOn(steps, s.OnTemp); m > limit {
			out = append(out, Alert{Kind: "schedule-long", Level: "warning",
				Title:  fmt.Sprintf("%s's schedule keeps it heated for %s", r.Name, longText(m)),
				Detail: describe(m, start, "At or above "+degText(s.OnTemp)) + fmt.Sprintf(" Your limit is %d h.", s.ScheduleHours),
				Figure: longText(m), Short: matterName(r.Name + " schedule on " + shortLong(m))})
		}
	}
	var hws []hotWaterView
	json.Unmarshal(snap["HotWater"].Data, &hws)
	for _, w := range hws {
		steps := scheds[w.ScheduleID]
		if s.Exclude[fmt.Sprintf("hotwater-%d", w.ID)] || len(steps) == 0 {
			continue
		}
		if m, start := longestOn(steps, 1); m > limit {
			out = append(out, Alert{Kind: "schedule-long", Level: "warning",
				Title:  "The hot water schedule keeps it on for " + longText(m),
				Detail: describe(m, start, "On") + fmt.Sprintf(" Your limit is %d h.", s.ScheduleHours),
				Figure: longText(m), Short: "Hot water schedule on " + shortLong(m)})
		}
	}
	return out
}

func shortLong(m int) string {
	if m >= weekMinutes {
		return "all week"
	}
	return hmShort(m)
}

func longText(m int) string {
	if m >= weekMinutes {
		return "the whole week"
	}
	return hm(m)
}

// Alerts are the guard's current alerts, for alerts() and Matter.
func (g *Guard) Alerts() []Alert {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.settings.Mode == "" {
		return nil
	}
	out := append([]Alert(nil), g.alarms...)
	now := time.Now()
	keys := make([]string, 0, len(g.returned))
	for k := range g.returned {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		r := g.returned[k]
		if now.Sub(r.At) > time.Hour {
			delete(g.returned, k)
			continue
		}
		if r.Watch {
			out = append(out, Alert{Kind: "guard-returned", Level: "info", Title: "Watch only: " + r.Name + " " + r.Text,
				Detail: "At " + r.At.Local().Format("15:04") + ". Turn the waste guard on to act on it."})
			continue
		}
		short := r.Short
		if short == "" {
			short = matterName(r.Name + " back to schedule")
		}
		out = append(out, Alert{Kind: "guard-returned", Level: "warning", Title: r.Name + " went back to its schedule",
			Detail: "At " + r.At.Local().Format("15:04") + ": " + r.Text + ". Turn it up again if you still need it.", Short: short, Since: r.At.Unix()})
	}
	failed := make([]string, 0, len(g.failed))
	for k := range g.failed {
		failed = append(failed, k) // in a fixed order, so the page doesn't redraw for nothing
	}
	sort.Strings(failed)
	for _, k := range failed {
		out = append(out, Alert{Kind: "guard-returned", Level: "warning", Title: "The waste guard couldn't return " + g.failed[k] + " to its schedule",
			Detail: "The hub kept the change after three attempts. Check it in the Wiser app.", Short: matterName(g.failed[k] + " not reset")})
	}
	return out
}

// State is what /api/state carries for the room cards.
func (g *Guard) State() map[string]any {
	if g == nil {
		return map[string]any{"mode": ""}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.settings.withDefaults()
	st := make(map[string]guardStatus, len(g.status))
	for k, v := range g.status {
		st[k] = v
	}
	return map[string]any{"mode": s.Mode, "minutes": s.Minutes, "status": st}
}

// ---------------------------------------------------------------- API

// handleGuard serves the waste guard page: settings, status, alarms, the last
// 24 hours, recorded daily heating and the activity log. ?onTemp=&scheduleHours=
// preview the schedule alarm for other values.
func (s *Server) handleGuard(w http.ResponseWriter, r *http.Request) {
	g := s.guard
	set := g.Settings().withDefaults()
	preview := set
	if v, err := strconv.Atoi(r.URL.Query().Get("onTemp")); err == nil && v >= 100 && v <= 250 {
		preview.OnTemp = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("scheduleHours")); err == nil && v >= 2 && v <= 24 {
		preview.ScheduleHours = v
	}
	snap := s.store.Snapshot()
	names := roomNamesFrom(snap)

	house, rooms, covered := heatingTotals(s.history.Since(time.Now().Add(-24*time.Hour), 0))
	roomTotals := map[string]int{}
	for id, m := range rooms {
		roomTotals[nameOr(names, id)] = m
	}
	// Whole days with data, for "your usual".
	days := s.history.Runtime(historyDays)
	var daily []map[string]any
	sum, n, busiest, busiestDay := 0, 0, 0, ""
	for i, d := range days {
		if d.Covered < 20*60 || i == len(days)-1 {
			continue
		}
		daily = append(daily, map[string]any{"day": d.Day, "heating": d.Heating})
		sum, n = sum+d.Heating, n+1
		if d.Heating > busiest {
			busiest, busiestDay = d.Heating, d.Day
		}
	}
	usual := 0
	if n > 0 {
		usual = sum / n
	}

	trend := s.history.Rolling24h(time.Now())
	if trend == nil {
		trend = []RollingPoint{}
	}

	g.mu.Lock()
	events := make([]GuardEvent, 0, 100)
	for i := len(g.events) - 1; i >= 0 && len(events) < 100; i-- {
		events = append(events, g.events[i])
	}
	g.mu.Unlock()
	alerts := g.Alerts()
	if alerts == nil {
		alerts = []Alert{}
	}
	previewAlarms := scheduleAlarms(snap, preview)
	if previewAlarms == nil {
		previewAlarms = []Alert{}
	}
	writeJSON(w, 200, map[string]any{
		"settings": set,
		"state":    g.State(),
		"alerts":   alerts,
		"preview":  previewAlarms,
		"last24h":  map[string]any{"heating": house, "rooms": roomTotals, "covered": covered, "limit": set.DailyHours * 60},
		"daily":    map[string]any{"usual": usual, "busiest": busiest, "busiestDay": busiestDay, "days": daily},
		"trend":    trend,
		"activity": events,
		"matter":   s.matter != nil,
	})
}

func (s *Server) handleGuardSettings(w http.ResponseWriter, r *http.Request) {
	var req GuardSettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "bad request: " + err.Error()})
		return
	}
	if err := req.validate(); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	prev := s.guard.Settings()
	s.guard.SetSettings(req)
	if prev.Mode != req.Mode {
		log.Printf("guard: mode %q -> %q", prev.Mode, req.Mode)
	}
	ip, secret := s.hub.Target()
	if err := s.save(ip, secret); err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleGuardKeep(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "key is required"})
		return
	}
	if err := s.guard.Keep(req.Key); err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- helpers

// degText formats tenths of °C as "21°" or "21.5°", and the hub's off as "off".
func degText(v int) string {
	if v <= -200 {
		return "off"
	}
	return strconv.FormatFloat(float64(v)/10, 'f', -1, 64) + "°"
}

func roomNamesFrom(snap map[string]Entry) map[string]string {
	var rooms []roomView
	json.Unmarshal(snap["Room"].Data, &rooms)
	names := map[string]string{}
	for _, r := range rooms {
		names[strconv.Itoa(r.ID)] = r.Name
	}
	return names
}

// Where a command came from, so the guard can tell the dashboard's own
// changes from the Wiser app's or Home Assistant's.
type sourceKey struct{}

func withSource(ctx context.Context, src string) context.Context {
	return context.WithValue(ctx, sourceKey{}, src)
}

func sourceOf(ctx context.Context) string {
	s, _ := ctx.Value(sourceKey{}).(string)
	return s
}
