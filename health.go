package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Health keeps a 14-day log of how the hub itself is doing: its Wi-Fi and
// memory every time the slow poll refreshes them, plus events for restarts,
// Wi-Fi drops and spells when the hub stopped answering.

// HealthSample is the hub's state at one slow poll.
type HealthSample struct {
	T         int64   `json:"t"`
	Uptime    int64   `json:"up"` // seconds since the hub booted
	RSSI      int     `json:"rssi"`
	Retrans   float64 `json:"retx"`  // % of TCP segments resent since boot
	Drops     int     `json:"drops"` // Wi-Fi disconnects since boot
	ApMissing int     `json:"apMissing"`
	AuthFail  int     `json:"authFail"`
	FreeHeap  int     `json:"heap"`
}

// HealthEvent kinds: "restart", "wifi-drop", "offline", "online".
type HealthEvent struct {
	T      int64  `json:"t"`
	Kind   string `json:"kind"`
	Detail string `json:"detail,omitempty"`
}

type healthLine struct {
	S *HealthSample `json:"s,omitempty"`
	E *HealthEvent  `json:"e,omitempty"`
}

type Health struct {
	mu      sync.RWMutex
	samples []HealthSample
	events  []HealthEvent
	path    string
	seen    time.Time // fetch time of the status reading last recorded
	offline bool
}

func NewHealth(path string) *Health {
	h := &Health{path: path}
	h.load()
	return h
}

func (h *Health) load() {
	f, err := os.Open(h.path)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-historyDays * 24 * time.Hour).Unix()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l healthLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		if l.S != nil && l.S.T >= cutoff {
			h.samples = append(h.samples, *l.S)
		}
		if l.E != nil && l.E.T >= cutoff {
			h.events = append(h.events, *l.E)
		}
	}
	f.Close()
	sort.Slice(h.events, func(i, j int) bool { return h.events[i].T < h.events[j].T })
	h.rewrite()
}

func (h *Health) rewrite() {
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
	for i := range h.samples {
		enc.Encode(healthLine{S: &h.samples[i]})
	}
	for i := range h.events {
		enc.Encode(healthLine{E: &h.events[i]})
	}
	w.Flush()
	f.Close()
	os.Rename(tmp, h.path)
}

func (h *Health) write(l healthLine) {
	f, err := os.OpenFile(h.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	json.NewEncoder(f).Encode(l)
	f.Close()
}

func (h *Health) addEvent(e HealthEvent) {
	h.events = append(h.events, e)
	sort.Slice(h.events, func(i, j int) bool { return h.events[i].T < h.events[j].T })
	h.write(healthLine{E: &e})
}

// Run checks the cache once a minute.
func (h *Health) Run(ctx context.Context, store *Store, hub *Hub) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if hub.Ready() {
				h.check(store.Snapshot())
			}
		}
	}
}

func (h *Health) check(snap map[string]Entry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()

	// Spells when the hub stopped answering, judged by the room poll.
	if room := snap["Room"]; !room.Fetched.IsZero() {
		stale := now.Sub(room.Fetched) > 3*time.Minute
		switch {
		case stale && !h.offline:
			h.offline = true
			h.addEvent(HealthEvent{T: room.Fetched.Unix(), Kind: "offline", Detail: room.Error})
		case !stale && h.offline:
			h.offline = false
			h.addEvent(HealthEvent{T: now.Unix(), Kind: "online"})
		}
	}

	st := snap["status"]
	if st.Data == nil || !st.Fetched.After(h.seen) {
		return
	}
	h.seen = st.Fetched
	var status struct {
		Uptime          int64  `json:"uptime"`
		FreeHeap        int    `json:"freeHeap"`
		LastResetReason string `json:"lastResetReason"`
	}
	if json.Unmarshal(st.Data, &status) != nil {
		return
	}
	var net struct {
		Station struct {
			RSSI struct{ Current int }
		}
		TcpStats_SinceBoot struct {
			SegmentsSent_Count, SegmentRetransmissions_Count float64
		}
		HealthCheckStats struct {
			StationTransitionToDisconnected_Count int
			WifiStateApNotFound_Count             int
			WifiStateAuthFail_Count               int
		}
	}
	json.Unmarshal(snap["network"].Data, &net)
	s := HealthSample{
		T: st.Fetched.Unix(), Uptime: status.Uptime, FreeHeap: status.FreeHeap,
		RSSI:      net.Station.RSSI.Current,
		Drops:     net.HealthCheckStats.StationTransitionToDisconnected_Count,
		ApMissing: net.HealthCheckStats.WifiStateApNotFound_Count,
		AuthFail:  net.HealthCheckStats.WifiStateAuthFail_Count,
	}
	if tcp := net.TcpStats_SinceBoot; tcp.SegmentsSent_Count > 0 {
		s.Retrans = 100 * tcp.SegmentRetransmissions_Count / tcp.SegmentsSent_Count
	}

	// A restart is known from the boot time; log it once.
	boot := s.T - s.Uptime
	known := false
	for _, e := range h.events {
		if e.Kind == "restart" && abs64(e.T-boot) < 600 {
			known = true
			break
		}
	}
	if !known {
		h.addEvent(HealthEvent{T: boot, Kind: "restart", Detail: resetReason(status.LastResetReason)})
	}
	// Wi-Fi drops since the last reading of the same boot.
	if n := len(h.samples); n > 0 {
		prev := h.samples[n-1]
		if abs64((prev.T-prev.Uptime)-boot) < 600 && s.Drops > prev.Drops {
			h.addEvent(HealthEvent{T: s.T, Kind: "wifi-drop", Detail: fmt.Sprintf("%d disconnect(s) since %s", s.Drops-prev.Drops, time.Unix(prev.T, 0).Format("15:04"))})
		}
	}
	h.samples = append(h.samples, s)
	h.write(healthLine{S: &s})

	cutoff := now.Add(-historyDays * 24 * time.Hour).Unix()
	if len(h.samples) > 0 && h.samples[0].T < cutoff {
		i := sort.Search(len(h.samples), func(i int) bool { return h.samples[i].T >= cutoff })
		h.samples = h.samples[i:]
		j := sort.Search(len(h.events), func(i int) bool { return h.events[i].T >= cutoff })
		h.events = h.events[j:]
		h.rewrite()
	}
}

// Since returns the samples and events newer than t.
func (h *Health) Since(t time.Time) ([]HealthSample, []HealthEvent) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	i := sort.Search(len(h.samples), func(i int) bool { return h.samples[i].T >= t.Unix() })
	j := sort.Search(len(h.events), func(i int) bool { return h.events[i].T >= t.Unix() })
	return append([]HealthSample(nil), h.samples[i:]...), append([]HealthEvent(nil), h.events[j:]...)
}

func resetReason(r string) string {
	switch r {
	case "WDT_RST":
		return "watchdog: the hub froze and restarted itself"
	case "POWER_ON_RST", "PWR_ON_RST":
		return "power on"
	case "SW_RST", "SOFT_RST":
		return "software restart (update or command)"
	case "":
		return ""
	}
	return r
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
