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
	if !ok || room.Data == nil || time.Since(room.Fetched) > 3*time.Minute {
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
