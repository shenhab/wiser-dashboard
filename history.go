package main

import (
	"bufio"
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// The hub keeps no history, so the dashboard samples the cached state once a
// minute and keeps the last historyDays days, in memory and in a JSONL file.

const (
	historyDays     = 14
	historyInterval = time.Minute
)

// Sample is one minute of house state. Temperatures are in tenths of °C, like the hub.
type Sample struct {
	T     int64             `json:"t"` // unix seconds
	Rooms map[string][4]int `json:"r"` // room id -> [temp, setpoint, demand %, humidity % or -1]
	Ch    map[string]int    `json:"c"` // heating channel id -> relay on (1) / off (0)
	HW    int               `json:"w"` // hot water relay on (1) / off (0) / unknown (-1)
}

type History struct {
	mu      sync.RWMutex
	samples []Sample
	path    string
}

func NewHistory(path string) *History {
	h := &History{path: path}
	h.load()
	return h
}

func (h *History) load() {
	if h.path == "" {
		return
	}
	f, err := os.Open(h.path)
	if err != nil {
		return
	}
	defer f.Close()
	cutoff := time.Now().Add(-historyDays * 24 * time.Hour).Unix()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var s Sample
		if json.Unmarshal(sc.Bytes(), &s) == nil && s.T >= cutoff {
			h.samples = append(h.samples, s)
		}
	}
	// Rewrite without the expired lines so the file doesn't grow forever.
	h.rewrite()
}

func (h *History) rewrite() {
	if err := os.MkdirAll(filepath.Dir(h.path), 0o700); err != nil {
		return
	}
	tmp := h.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, s := range h.samples {
		enc.Encode(s)
	}
	w.Flush()
	f.Close()
	os.Rename(tmp, h.path)
}

func (h *History) append(s Sample) {
	h.mu.Lock()
	h.samples = append(h.samples, s)
	cutoff := time.Now().Add(-historyDays * 24 * time.Hour).Unix()
	i := sort.Search(len(h.samples), func(i int) bool { return h.samples[i].T >= cutoff })
	h.samples = h.samples[i:]
	h.mu.Unlock()
	if h.path == "" {
		return
	}
	f, err := os.OpenFile(h.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf("history: %v", err)
		return
	}
	json.NewEncoder(f).Encode(s)
	f.Close()
}

// Run records a sample every minute while the cached state is fresh.
func (h *History) Run(ctx context.Context, store *Store) {
	t := time.NewTicker(historyInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s, ok := sampleFrom(store.Snapshot()); ok {
				h.append(s)
			}
		}
	}
}

func sampleFrom(snap map[string]Entry) (Sample, bool) {
	room, ok := snap["Room"]
	if !ok || room.Data == nil || room.Restored || time.Since(room.Fetched) > 3*time.Minute {
		return Sample{}, false
	}
	var rooms []struct {
		ID                    int
		RoomStatID            int `json:"RoomStatId"`
		CalculatedTemperature int
		CurrentSetPoint       int
		PercentageDemand      int
	}
	if json.Unmarshal(room.Data, &rooms) != nil {
		return Sample{}, false
	}
	var stats []struct {
		ID               int
		MeasuredHumidity *int
	}
	json.Unmarshal(snap["RoomStat"].Data, &stats)
	humidity := map[int]int{}
	for _, st := range stats {
		if st.MeasuredHumidity != nil {
			humidity[st.ID] = *st.MeasuredHumidity
		}
	}

	s := Sample{T: time.Now().Unix(), Rooms: map[string][4]int{}, Ch: map[string]int{}, HW: -1}
	for _, r := range rooms {
		hum, ok := humidity[r.RoomStatID]
		if !ok {
			hum = -1
		}
		s.Rooms[strconv.Itoa(r.ID)] = [4]int{r.CalculatedTemperature, r.CurrentSetPoint, r.PercentageDemand, hum}
	}
	var chans []struct {
		ID                int
		HeatingRelayState string
	}
	json.Unmarshal(snap["HeatingChannel"].Data, &chans)
	for _, c := range chans {
		s.Ch[strconv.Itoa(c.ID)] = onOff(c.HeatingRelayState)
	}
	var hw []struct{ HotWaterRelayState string }
	if json.Unmarshal(snap["HotWater"].Data, &hw) == nil && len(hw) > 0 {
		s.HW = onOff(hw[0].HotWaterRelayState)
	}
	return s, true
}

func onOff(state string) int {
	if state == "On" {
		return 1
	}
	return 0
}

// Since returns samples newer than t, thinned to at most max points.
func (h *History) Since(t time.Time, max int) []Sample {
	h.mu.RLock()
	defer h.mu.RUnlock()
	i := sort.Search(len(h.samples), func(i int) bool { return h.samples[i].T >= t.Unix() })
	src := h.samples[i:]
	step := 1
	if max > 0 && len(src) > max {
		step = (len(src) + max - 1) / max
	}
	out := make([]Sample, 0, len(src)/step+1)
	for j := 0; j < len(src); j += step {
		out = append(out, src[j])
	}
	return out
}

// samplesGap is the longest silence between samples that still counts as
// continuous; anything longer means the dashboard or the hub was away.
const samplesGap = 3 * 60

// shortRun is the run length (minutes) below which a boiler start counts as
// very short. The hub pulses the boiler within its cycle rate (e.g. 6 an hour,
// so 10-minute cycles), so runs under 10 minutes are normal; under 5 the
// boiler barely warms up.
const shortRun = 5

// CycleStats summarises the relay runs that started on one day.
type CycleStats struct {
	Starts  int `json:"starts"`  // off -> on switches seen
	Minutes int `json:"minutes"` // on-time of the runs counted here
	Short   int `json:"short"`   // finished runs shorter than shortRun
	Longest int `json:"longest"`
	Runs    int `json:"runs"` // finished runs (for averages)
}

func (c *CycleStats) add(minutes int, counted, finished bool) {
	c.Minutes += minutes
	c.Longest = max(c.Longest, minutes)
	if counted {
		c.Starts++
	}
	if finished && counted {
		c.Runs++
		if minutes < shortRun {
			c.Short++
		}
	}
}

// DayCycles is how often each relay switched on per local calendar day.
// Boiler is any heating channel or hot water on: that's when the boiler fires.
type DayCycles struct {
	Day      string                `json:"day"`
	Boiler   CycleStats            `json:"boiler"`
	Channels map[string]CycleStats `json:"channels"`
	HotWater CycleStats            `json:"hotWater"`
}

// Cycles counts relay runs per day. A run already on when recording started
// (or resumed after a gap) adds its minutes but isn't counted as a start.
func (h *History) Cycles(days int) []DayCycles {
	h.mu.RLock()
	defer h.mu.RUnlock()
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -(days - 1))
	out := make([]DayCycles, days)
	idx := map[string]int{}
	for i := range out {
		d := start.AddDate(0, 0, i).Format("2006-01-02")
		out[i] = DayCycles{Day: d, Channels: map[string]CycleStats{}}
		idx[d] = i
	}
	type run struct {
		on, known, partial bool
		start, last        int64
	}
	runs := map[string]*run{}
	// record books a run against the day it started.
	record := func(key string, r *run, end int64, finished bool) {
		i, ok := idx[time.Unix(r.start, 0).Format("2006-01-02")]
		if !ok {
			return
		}
		mins := int((end - r.start + 30) / 60)
		switch key {
		case "boiler":
			out[i].Boiler.add(mins, !r.partial, finished)
		case "hw":
			out[i].HotWater.add(mins, !r.partial, finished)
		default:
			c := out[i].Channels[key]
			c.add(mins, !r.partial, finished)
			out[i].Channels[key] = c
		}
	}
	var prevT int64
	for _, s := range h.samples {
		if prevT != 0 && s.T-prevT > samplesGap {
			// The gap hides when runs ended: close them at the last sample.
			for key, r := range runs {
				if r.on {
					record(key, r, r.last, false)
				}
			}
			runs = map[string]*run{}
		}
		prevT = s.T
		state := map[string]bool{"hw": s.HW == 1}
		anyOn := s.HW == 1
		for ch, on := range s.Ch {
			state[ch] = on == 1
			anyOn = anyOn || on == 1
		}
		state["boiler"] = anyOn
		for key, on := range state {
			r := runs[key]
			if r == nil {
				r = &run{}
				runs[key] = r
			}
			switch {
			case on && !r.on:
				*r = run{on: true, known: true, partial: !r.known, start: s.T}
			case !on && r.on:
				record(key, r, s.T, true)
				r.on = false
			}
			r.known, r.last = true, s.T
		}
	}
	for key, r := range runs {
		if r.on {
			record(key, r, r.last, false)
		}
	}
	return out
}

// HeatRate is how fast a room warmed while it was calling for heat.
type HeatRate struct {
	PerHour float64 `json:"perHour"` // °C per hour, median of the heat-ups seen
	Runs    int     `json:"runs"`
}

// HeatRates learns each room's warm-up speed from heat-ups of at least 20
// minutes in the recorded history.
func (h *History) HeatRates() map[string]HeatRate {
	h.mu.RLock()
	defer h.mu.RUnlock()
	type up struct {
		t0, t1       int64
		temp0, temp1 int
		active       bool
	}
	cur := map[string]*up{}
	rates := map[string][]float64{}
	finish := func(id string, u *up) {
		if u.active && u.t1-u.t0 >= 20*60 && u.temp1 > u.temp0 {
			rates[id] = append(rates[id], float64(u.temp1-u.temp0)/10/(float64(u.t1-u.t0)/3600))
		}
		u.active = false
	}
	var prevT int64
	for _, s := range h.samples {
		gap := prevT != 0 && s.T-prevT > samplesGap
		prevT = s.T
		for id, v := range s.Rooms {
			u := cur[id]
			if u == nil {
				u = &up{}
				cur[id] = u
			}
			if gap {
				finish(id, u)
			}
			temp, target, demand := v[0], v[1], v[2]
			heating := demand > 0 && target > temp && temp > -1000
			switch {
			case heating && !u.active:
				*u = up{t0: s.T, t1: s.T, temp0: temp, temp1: temp, active: true}
			case heating:
				u.t1, u.temp1 = s.T, temp
			case u.active:
				finish(id, u)
			}
		}
	}
	for id, u := range cur {
		finish(id, u)
	}
	out := map[string]HeatRate{}
	for id, rs := range rates {
		sort.Float64s(rs)
		m := rs[len(rs)/2]
		if len(rs)%2 == 0 {
			m = (rs[len(rs)/2-1] + rs[len(rs)/2]) / 2
		}
		out[id] = HeatRate{PerHour: m, Runs: len(rs)}
	}
	return out
}

// DayRuntime is how long each relay was on, per local calendar day.
type DayRuntime struct {
	Day      string         `json:"day"` // YYYY-MM-DD
	Channels map[string]int `json:"channels"`
	Heating  int            `json:"heating"` // any heating channel on
	HotWater int            `json:"hotWater"`
	Covered  int            `json:"covered"` // minutes of the day with data
}

func (h *History) Runtime(days int) []DayRuntime {
	h.mu.RLock()
	defer h.mu.RUnlock()
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -(days - 1))
	out := make([]DayRuntime, days)
	idx := map[string]int{}
	for i := range out {
		d := start.AddDate(0, 0, i).Format("2006-01-02")
		out[i] = DayRuntime{Day: d, Channels: map[string]int{}}
		idx[d] = i
	}
	minutes := int(historyInterval / time.Minute)
	for _, s := range h.samples {
		i, ok := idx[time.Unix(s.T, 0).Format("2006-01-02")]
		if !ok {
			continue
		}
		out[i].Covered += minutes
		anyOn := 0
		for ch, on := range s.Ch {
			out[i].Channels[ch] += on * minutes
			anyOn |= on
		}
		out[i].Heating += anyOn * minutes
		if s.HW == 1 {
			out[i].HotWater += minutes
		}
	}
	return out
}

// RollingPoint is how long the heating ran in the 24 hours before T.
type RollingPoint struct {
	T       int64 `json:"t"`       // unix seconds, on the hour
	Heating int   `json:"heating"` // minutes any heating zone ran
	Covered int   `json:"covered"` // minutes of those 24 hours with data
}

// Rolling24h is the 24-hour heating total on every hour of the recorded
// history, for the trend on the waste guard page. Hours whose 24 hours are
// mostly missing (the dashboard was off) are left out.
func (h *History) Rolling24h(now time.Time) []RollingPoint {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if len(h.samples) == 0 {
		return nil
	}
	per := int(historyInterval / time.Minute)
	// on[i] counts the samples before i with a heating zone running.
	on := make([]int, len(h.samples)+1)
	for i, s := range h.samples {
		on[i+1] = on[i]
		for _, v := range s.Ch {
			if v == 1 {
				on[i+1]++
				break
			}
		}
	}
	idx := func(t int64) int { return sort.Search(len(h.samples), func(i int) bool { return h.samples[i].T > t }) }
	const day = 24 * 3600
	first := h.samples[0].T + day
	t := (first + 3599) / 3600 * 3600
	var out []RollingPoint
	for ; t <= now.Unix(); t += 3600 {
		a, b := idx(t-day), idx(t)
		p := RollingPoint{T: t, Heating: (on[b] - on[a]) * per, Covered: (b - a) * per}
		if p.Covered >= 20*60 {
			out = append(out, p)
		}
	}
	return out
}
