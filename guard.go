package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The waste guard (docs/waste-guard.md) limits heating that isn't on the
// schedule and wasn't given an end: after a room has heated for N minutes
// above what its schedule last set, it goes back to that. This file holds the
// decisions as plain functions; guard_run.go applies them to the hub.

// GuardSettings are saved in the config file. Zero values mean the defaults.
type GuardSettings struct {
	Mode          string          `json:"mode,omitempty"`           // "" off, "watch" (log only) or "on"
	Minutes       int             `json:"minutes,omitempty"`        // N: heating allowed above the schedule
	OffTemp       int             `json:"off_temp,omitempty"`       // tenths: where rooms without a schedule go back to
	Exclude       map[string]bool `json:"exclude,omitempty"`        // "room-2", "hotwater-2" -> left alone
	SkipOwn       bool            `json:"skip_own,omitempty"`       // don't limit changes made from the dashboard, Matter or MCP
	OnTemp        int             `json:"on_temp,omitempty"`        // tenths: a schedule at or above this counts as on
	ScheduleHours int             `json:"schedule_hours,omitempty"` // schedule alarm: longest on-stretch allowed
	DailyHours    int             `json:"daily_hours,omitempty"`    // 24-hour heating limit
	MatterText    bool            `json:"matter_text,omitempty"`    // show alert texts as devices in Matter apps
	MatterHours   bool            `json:"matter_hours,omitempty"`   // show the last 24 hours' heating as a temperature sensor
}

const (
	defaultGuardMinutes  = 60
	defaultGuardOffTemp  = 100
	defaultGuardOnTemp   = 170
	defaultScheduleHours = 12
	defaultDailyHours    = 12
)

func (g GuardSettings) withDefaults() GuardSettings {
	if g.Minutes == 0 {
		g.Minutes = defaultGuardMinutes
	}
	if g.OffTemp == 0 {
		g.OffTemp = defaultGuardOffTemp
	}
	if g.OnTemp == 0 {
		g.OnTemp = defaultGuardOnTemp
	}
	if g.ScheduleHours == 0 {
		g.ScheduleHours = defaultScheduleHours
	}
	if g.DailyHours == 0 {
		g.DailyHours = defaultDailyHours
	}
	if g.Exclude == nil {
		g.Exclude = map[string]bool{}
	}
	return g
}

func (g GuardSettings) validate() error {
	switch {
	case g.Mode != "" && g.Mode != "watch" && g.Mode != "on":
		return fmt.Errorf("mode must be off, watch or on")
	case g.Minutes != 0 && (g.Minutes < 15 || g.Minutes > 480):
		return fmt.Errorf("the time limit must be 15 minutes to 8 hours")
	case g.OffTemp != 0 && (g.OffTemp < 50 || g.OffTemp > 200):
		return fmt.Errorf("the guard off temperature must be 5° to 20°")
	case g.OnTemp != 0 && (g.OnTemp < 100 || g.OnTemp > 250):
		return fmt.Errorf("the \"considered on\" temperature must be 10° to 25°")
	case g.ScheduleHours != 0 && (g.ScheduleHours < 2 || g.ScheduleHours > 24):
		return fmt.Errorf("the schedule alarm must be 2 to 24 hours")
	case g.DailyHours != 0 && (g.DailyHours < 1 || g.DailyHours > 23):
		return fmt.Errorf("the 24-hour limit must be 1 to 23 hours")
	}
	return nil
}

// ---------------------------------------------------------------- schedules

const weekMinutes = 7 * 24 * 60

// step is one schedule change: from minute At of the week (Monday 00:00 is 0)
// the value is Value (tenths of °C for heating, 1/0 for on/off schedules).
type step struct {
	At    int
	Value int
}

// hhmmMinutes turns the hub's 630 into 390 minutes; 2400 is the end of the day.
func hhmmMinutes(t int) int {
	return min(t/100*60+t%100, 24*60)
}

// parseSchedules reads the hub's /data/schedules/ into steps per schedule id.
// Heating days are {"Time":[630,...],"DegreesC":[210,...]}; on/off days are
// [630,-700,...]: on at 06:30, off at 07:00.
func parseSchedules(raw json.RawMessage) map[int][]step {
	var all map[string][]map[string]json.RawMessage
	if json.Unmarshal(raw, &all) != nil {
		return nil
	}
	out := map[int][]step{}
	for kind, list := range all {
		for _, sc := range list {
			var id int
			if json.Unmarshal(sc["id"], &id) != nil {
				continue
			}
			var steps []step
			for d, day := range weekDays {
				if kind == "OnOff" {
					var times []int
					json.Unmarshal(sc[day], &times)
					for _, t := range times {
						on := 1
						if t < 0 {
							t, on = -t, 0
						}
						steps = append(steps, step{d*1440 + hhmmMinutes(t), on})
					}
					continue
				}
				var slots struct {
					Time     []int
					DegreesC []int
				}
				json.Unmarshal(sc[day], &slots)
				for i, t := range slots.Time {
					if i < len(slots.DegreesC) {
						steps = append(steps, step{d*1440 + hhmmMinutes(t), slots.DegreesC[i]})
					}
				}
			}
			sort.SliceStable(steps, func(i, j int) bool { return steps[i].At < steps[j].At })
			out[id] = steps
		}
	}
	return out
}

// scheduleAt is what a schedule says at minute m of the week: the value its
// last change set (a day's first change carries the previous day's last
// value until then, wrapping from Sunday to Monday), and the next change to a
// different value with the minutes until it. ok is false for an empty schedule.
func scheduleAt(steps []step, m int) (cur int, next step, until int, ok bool) {
	if len(steps) == 0 {
		return 0, step{}, 0, false
	}
	m = ((m % weekMinutes) + weekMinutes) % weekMinutes
	i := sort.Search(len(steps), func(i int) bool { return steps[i].At > m }) - 1
	if i < 0 {
		i = len(steps) - 1 // still on Sunday's last change
	}
	cur = steps[i].Value
	for k := 1; k <= len(steps); k++ {
		s := steps[(i+k)%len(steps)]
		if s.Value != cur {
			d := (s.At - m + weekMinutes) % weekMinutes
			if d == 0 {
				d = weekMinutes
			}
			return cur, s, d, true
		}
	}
	return cur, step{}, -1, true // the same all week
}

// longestOn is the longest stretch (minutes) a schedule stays at or above on,
// across midnight and from Sunday into Monday, and the minute of the week it
// starts. A schedule on all week gives weekMinutes and start -1.
func longestOn(steps []step, on int) (minutes, start int) {
	if len(steps) == 0 {
		return 0, 0
	}
	type seg struct{ from, length int }
	var segs []seg
	for i, s := range steps {
		end := weekMinutes + steps[0].At
		if i+1 < len(steps) {
			end = steps[i+1].At
		}
		segs = append(segs, seg{s.At, end - s.At})
	}
	// The stretch before Monday's first change belongs to Sunday's last one.
	isOn := func(i int) bool { return steps[i].Value >= on }
	all := true
	for i := range steps {
		all = all && isOn(i)
	}
	if all {
		return weekMinutes, -1
	}
	// Start from a change that is off, then walk once round the week.
	first := 0
	for isOn(first) {
		first++
	}
	run, runStart := 0, 0
	for k := 1; k <= len(segs); k++ {
		i := (first + k) % len(segs)
		if !isOn(i) {
			run = 0
			continue
		}
		if run == 0 {
			runStart = segs[i].from
		}
		run += segs[i].length
		if run > minutes {
			minutes, start = run, runStart
		}
	}
	return minutes, start
}

// weekTime formats a minute of the week as "Mon 06:30".
func weekTime(m int) string {
	m = ((m % weekMinutes) + weekMinutes) % weekMinutes
	return fmt.Sprintf("%s %02d:%02d", weekDays[m/1440][:3], m%1440/60, m%60)
}

// hubWeekMinute is the hub's local time as a minute of the week, from the
// System section's LocalDateAndTime moved on by the time since it was read.
func hubWeekMinute(sys json.RawMessage, fetched, now time.Time) (int, bool) {
	var s struct {
		LocalDateAndTime struct {
			Day  string
			Time *int
		}
	}
	if json.Unmarshal(sys, &s) != nil || s.LocalDateAndTime.Time == nil {
		return 0, false
	}
	d := -1
	for i, n := range weekDays {
		if strings.EqualFold(n, s.LocalDateAndTime.Day) {
			d = i
		}
	}
	if d < 0 {
		return 0, false
	}
	m := d*1440 + hhmmMinutes(*s.LocalDateAndTime.Time) + int(now.Sub(fetched)/time.Minute)
	return ((m % weekMinutes) + weekMinutes) % weekMinutes, true
}

// hubClockDrift is how far the hub's clock is from this computer's.
func hubClockDrift(sys json.RawMessage, fetched, now time.Time) (time.Duration, bool) {
	var s struct{ UnixTime int64 }
	if json.Unmarshal(sys, &s) != nil || s.UnixTime <= 0 {
		return 0, false
	}
	hubNow := s.UnixTime + int64(now.Sub(fetched).Seconds())
	return time.Duration(abs64(hubNow-now.Unix())) * time.Second, true
}

// ---------------------------------------------------------------- decisions

// guardEnv is what the guard knows about the house at one moment.
type guardEnv struct {
	Week     int  // hub's minute of the week
	Away     bool // away mode is on
	Comfort  bool // comfort mode (early start) is on
	Preheat  int  // longest early start, minutes
	AwayHW   bool // away mode also turns the hot water off
	Settings GuardSettings
}

// roomView is the part of a hub Room the guard looks at.
type roomView struct {
	ID                    int    `json:"id"`
	Name                  string `json:"Name"`
	Mode                  string `json:"Mode"`
	CalculatedTemperature int    `json:"CalculatedTemperature"`
	CurrentSetPoint       int    `json:"CurrentSetPoint"`
	ScheduledSetPoint     *int   `json:"ScheduledSetPoint"`
	SetpointOrigin        string `json:"SetpointOrigin"`
	OverrideType          string `json:"OverrideType"`
	PercentageDemand      int    `json:"PercentageDemand"`
	ScheduleID            int    `json:"ScheduleId"`
}

// hotWaterView is the part of a hub HotWater the guard looks at.
type hotWaterView struct {
	ID                         int    `json:"id"`
	Mode                       string `json:"Mode"`
	WaterHeatingState          string `json:"WaterHeatingState"`
	ScheduledWaterHeatingState string `json:"ScheduledWaterHeatingState"`
	HotWaterDescription        string `json:"HotWaterDescription"`
	OverrideType               string `json:"OverrideType"`
	ScheduleID                 int    `json:"ScheduleId"`
}

// Kinds of verdict.
const (
	vOnSchedule = "schedule"  // the target is what the schedule set (or its early start)
	vRespected  = "respected" // away, window, a timed change: left alone
	vBelow      = "below"     // lower than the schedule: saves energy
	vAbove      = "above"     // higher than the schedule, with no end chosen
)

// verdict is why a room (or the hot water) has its target, for the guard.
type verdict struct {
	Kind     string `json:"kind"`
	Why      string `json:"why"`
	Baseline int    `json:"baseline"` // where it goes back to: tenths, or 1/0 for hot water
	Target   int    `json:"target"`
	Heating  bool   `json:"heating"`
	Manual   bool   `json:"manual"` // Manual mode: going back sets a target instead of cancelling
	NoSched  bool   `json:"noSchedule,omitempty"`
}

// decideRoom compares a room's target with what its schedule last set.
func decideRoom(r roomView, steps []step, env guardEnv) verdict {
	s := env.Settings.withDefaults()
	v := verdict{
		Target:  r.CurrentSetPoint,
		Heating: r.PercentageDemand > 0 || (r.CalculatedTemperature > -1000 && r.CurrentSetPoint > r.CalculatedTemperature),
		Manual:  r.Mode == "Manual" || r.SetpointOrigin == "FromManualMode",
	}
	origin := r.SetpointOrigin
	switch {
	case env.Away || strings.Contains(origin, "Away"):
		v.Kind, v.Why = vRespected, "away mode"
		return v
	case strings.Contains(origin, "Window"):
		v.Kind, v.Why = vRespected, "open window"
		return v
	case strings.Contains(origin, "Boost"):
		v.Kind, v.Why = vRespected, "a timed change"
		return v
	case strings.Contains(origin, "Comfort"):
		v.Kind, v.Why = vRespected, "comfort mode's early start"
		return v
	}
	cur, next, until, ok := scheduleAt(steps, env.Week)
	switch {
	case r.ScheduleID != 0 && r.ScheduledSetPoint != nil:
		v.Baseline = *r.ScheduledSetPoint // the hub's own answer
	case ok:
		v.Baseline = cur
	case r.ScheduleID == 0:
		v.Baseline, v.NoSched = s.OffTemp, true
	default:
		// A schedule we haven't read yet: don't guess.
		v.Kind, v.Why = vRespected, "schedule not read yet"
		return v
	}
	switch {
	case v.Target == v.Baseline:
		v.Kind, v.Why = vOnSchedule, "on schedule"
	case ok && env.Comfort && next.Value == v.Target && until > 0 && until <= env.Preheat:
		v.Kind, v.Why = vOnSchedule, "early start for "+weekTime(next.At)
	case v.Target <= v.Baseline || v.Target <= -200:
		v.Kind, v.Why = vBelow, "below the schedule"
	default:
		v.Kind = vAbove
		switch {
		case v.Manual:
			v.Why = "Manual mode"
		case origin == "FromManualOverride":
			v.Why = "set by hand"
		case origin != "":
			v.Why = spaced(strings.TrimPrefix(origin, "From"))
		default:
			v.Why = "not from the schedule"
		}
	}
	return v
}

// decideHotWater is decideRoom for the hot water: on or off against its schedule.
func decideHotWater(w hotWaterView, steps []step, env guardEnv) verdict {
	on := w.WaterHeatingState == "On"
	v := verdict{Target: b2i(on), Heating: on, Manual: w.Mode == "Manual"}
	desc := w.HotWaterDescription
	switch {
	case (env.Away && env.AwayHW) || strings.Contains(desc, "Away"):
		v.Kind, v.Why = vRespected, "away mode"
		return v
	case strings.Contains(desc, "Boost"):
		v.Kind, v.Why = vRespected, "a timed change"
		return v
	}
	cur, _, _, ok := scheduleAt(steps, env.Week)
	switch {
	case w.ScheduledWaterHeatingState != "":
		v.Baseline = b2i(w.ScheduledWaterHeatingState == "On")
	case ok:
		v.Baseline = cur
	default:
		v.NoSched = true
	}
	switch {
	case v.Target == v.Baseline:
		v.Kind, v.Why = vOnSchedule, "on schedule"
	case !on:
		v.Kind, v.Why = vBelow, "off while the schedule says on"
	case v.Manual:
		v.Kind, v.Why = vAbove, "Manual mode"
	default:
		v.Kind, v.Why = vAbove, "turned on by hand"
	}
	return v
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------- timers

// guardTrack follows one change above the schedule. It's saved, so a restart
// doesn't hand out a fresh N minutes.
type guardTrack struct {
	Target     int       `json:"target"`            // the target being followed; a new one starts over
	Seen       time.Time `json:"seen"`              // when the guard first saw it
	HeatFrom   time.Time `json:"heatFrom"`          // when the timer started (the room began heating)
	Own        bool      `json:"own,omitempty"`     // made from the dashboard, Matter or MCP
	Returns    int       `json:"returns,omitempty"` // attempts to go back
	LastReturn time.Time `json:"lastReturn"`
	Watched    bool      `json:"watched,omitempty"` // watch only: "would return" already logged
}

// Actions the guard can take on a step.
const (
	aNone      = ""          // nothing to follow
	aSeen      = "seen"      // a new change above the schedule
	aWatching  = "watching"  // above the schedule but not heating yet
	aTimer     = "timer"     // heating, timer running
	aExempt    = "exempt"    // the dashboard's own change, and those are left alone
	aReturn    = "return"    // time's up: go back to the schedule
	aReturning = "returning" // went back; waiting for the hub to show it
	aFailed    = "failed"    // went back three times and the hub still shows the change
)

const (
	guardRetryAfter = 2 * time.Minute
	guardMaxReturns = 3
)

// guardStep moves a track on with a fresh verdict. It returns the track to
// keep (nil when there's nothing to follow), the action, whether the room
// just started heating, and when the timer runs out.
func guardStep(prev *guardTrack, v verdict, own bool, now time.Time, s GuardSettings) (next *guardTrack, act string, started bool, deadline time.Time) {
	s = s.withDefaults()
	if v.Kind != vAbove {
		return nil, aNone, false, time.Time{}
	}
	act = aTimer
	if prev == nil || prev.Target != v.Target {
		next, act = &guardTrack{Target: v.Target, Seen: now, Own: own}, aSeen
	} else {
		t := *prev
		next = &t
	}
	if next.Own && s.SkipOwn {
		return next, aExempt, false, time.Time{}
	}
	if next.HeatFrom.IsZero() {
		if !v.Heating {
			if act != aSeen {
				act = aWatching
			}
			return next, act, false, time.Time{}
		}
		next.HeatFrom, started = now, true
	}
	deadline = next.HeatFrom.Add(time.Duration(s.Minutes) * time.Minute)
	switch {
	case now.Before(deadline):
	case !next.LastReturn.IsZero() && now.Sub(next.LastReturn) < guardRetryAfter:
		act = aReturning
	case next.Returns >= guardMaxReturns:
		act = aFailed
	default:
		act = aReturn
	}
	return next, act, started, deadline
}

// ---------------------------------------------------------------- 24 hours

// heatingTotals adds up the samples since a time: minutes any heating zone
// ran, minutes each room asked for heat, and minutes with data at all.
func heatingTotals(samples []Sample) (house int, rooms map[string]int, covered int) {
	rooms = map[string]int{}
	per := int(historyInterval / time.Minute)
	for _, s := range samples {
		covered += per
		for _, on := range s.Ch {
			if on == 1 {
				house += per
				break
			}
		}
		for id, v := range s.Rooms {
			if v[2] > 0 {
				rooms[id] += per
			}
		}
	}
	return house, rooms, covered
}

// hmShort formats minutes as "14h 20m", for Matter device names.
func hmShort(m int) string {
	switch {
	case m < 60:
		return fmt.Sprintf("%dm", m)
	case m%60 == 0:
		return fmt.Sprintf("%dh", m/60)
	}
	return fmt.Sprintf("%dh %dm", m/60, m%60)
}

// matterName cuts a text to Matter's 32-character limit for device names.
func matterName(s string) string {
	r := []rune(s)
	if len(r) > 32 {
		return strings.TrimSpace(string(r[:31])) + "…"
	}
	return s
}

// hm formats minutes as "14 h 20 min".
func hm(m int) string {
	switch {
	case m < 60:
		return fmt.Sprintf("%d min", m)
	case m%60 == 0:
		return fmt.Sprintf("%d h", m/60)
	}
	return fmt.Sprintf("%d h %d min", m/60, m%60)
}
