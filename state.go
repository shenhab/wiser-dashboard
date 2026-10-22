package main

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Entry is the latest copy of one hub endpoint.
type Entry struct {
	Data    json.RawMessage `json:"data,omitempty"`
	Fetched time.Time       `json:"fetched"`
	Error   string          `json:"error,omitempty"`
	Millis  int64           `json:"ms"`
	// Restored marks data loaded from before a restart; it's cleared when the
	// hub next answers for this section.
	Restored bool `json:"restored,omitempty"`
}

type Store struct {
	mu      sync.RWMutex
	entries map[string]*Entry
}

func NewStore() *Store { return &Store{entries: map[string]*Entry{}} }

func (s *Store) Snapshot() map[string]Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]Entry, len(s.entries))
	for k, v := range s.entries {
		out[k] = *v
	}
	return out
}

func (s *Store) Clear() {
	s.mu.Lock()
	s.entries = map[string]*Entry{}
	s.mu.Unlock()
}

func (s *Store) set(name string, data []byte, dur time.Duration) {
	s.mu.Lock()
	s.entries[name] = &Entry{Data: append(json.RawMessage(nil), data...), Fetched: time.Now(), Millis: dur.Milliseconds()}
	s.mu.Unlock()
}

// setError keeps the last good data so the UI does not go blank on a glitch.
func (s *Store) setError(name, msg string) {
	s.mu.Lock()
	e := s.entries[name]
	if e == nil {
		e = &Entry{}
		s.entries[name] = e
	}
	e.Error = msg
	s.mu.Unlock()
}

// MergeWrite folds the hub's reply to a PATCH on /data/domain/<Section>[/<id>]
// into the cache, so the UI reflects a command without waiting for a poll.
// It returns the section that should be re-fetched instead when it can't merge.
func (s *Store) MergeWrite(path string, reply []byte) (refresh string) {
	section, id, ok := parseDomainPath(path)
	if !ok {
		if strings.HasPrefix(path, "/data/schedules/") && s.mergeSchedule(path, reply) {
			return ""
		}
		return sectionFor(path)
	}
	if !json.Valid(reply) {
		return section
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[section]
	if e == nil {
		return section
	}
	if id < 0 {
		if len(reply) > 0 && reply[0] == '{' && len(e.Data) > 0 && e.Data[0] == '{' {
			e.Data = append(json.RawMessage(nil), reply...)
			return ""
		}
		return section
	}
	var items []json.RawMessage
	if json.Unmarshal(e.Data, &items) != nil {
		return section
	}
	for i, it := range items {
		if itemID(it) == id {
			items[i] = append(json.RawMessage(nil), reply...)
			if b, err := json.Marshal(items); err == nil {
				e.Data = b
				return ""
			}
		}
	}
	return section
}

// mergeSchedule folds the reply to a PATCH on /data/schedules/<Type>/<id> into
// the cached schedule list.
func (s *Store) mergeSchedule(path string, reply []byte) bool {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(path, "/data/schedules/"), "/"), "/")
	if len(parts) != 2 || !json.Valid(reply) {
		return false
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || itemID(reply) != id {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries["schedules"]
	if e == nil {
		return false
	}
	var all map[string]json.RawMessage
	var items []json.RawMessage
	if json.Unmarshal(e.Data, &all) != nil || json.Unmarshal(all[parts[0]], &items) != nil {
		return false
	}
	for i, it := range items {
		if itemID(it) == id {
			items[i] = append(json.RawMessage(nil), reply...)
			b, err := json.Marshal(items)
			if err != nil {
				return false
			}
			all[parts[0]] = b
			if b, err = json.Marshal(all); err != nil {
				return false
			}
			e.Data = b
			return true
		}
	}
	return false
}

// itemID returns an item's "id", or -1 if it has none.
func itemID(raw json.RawMessage) int64 {
	var head struct {
		ID *int64 `json:"id"`
	}
	if json.Unmarshal(raw, &head) != nil || head.ID == nil {
		return -1
	}
	return *head.ID
}

// sectionFor names the cached section a hub path belongs to.
func sectionFor(path string) string {
	switch {
	case strings.HasPrefix(path, "/data/schedules"):
		return "schedules"
	case strings.HasPrefix(path, "/data/network"):
		return "network"
	case strings.HasPrefix(path, "/data/domain/"):
		return strings.SplitN(strings.TrimPrefix(path, "/data/domain/"), "/", 2)[0]
	}
	return ""
}

// parseDomainPath accepts /data/domain/Room and /data/domain/Room/2.
// id is -1 for a whole section. Deeper paths are not mergeable.
func parseDomainPath(path string) (section string, id int64, ok bool) {
	rest, found := strings.CutPrefix(path, "/data/domain/")
	if !found {
		return "", 0, false
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	switch len(parts) {
	case 1:
		return parts[0], -1, parts[0] != ""
	case 2:
		n, err := strconv.ParseInt(parts[1], 10, 64)
		return parts[0], n, err == nil
	}
	return "", 0, false
}

func sectionPath(name string) string {
	switch name {
	case "network":
		return "/data/network/"
	case "schedules":
		return "/data/schedules/"
	case "opentherm":
		return "/data/opentherm/"
	case "status":
		return "/data/status/"
	}
	// Never "/data/domain/" itself: this hub stalls on responses over ~2.7 KB.
	return "/data/domain/" + name
}

var (
	fastSections   = []string{"Room", "HeatingChannel", "HotWater", "RoomStat"}
	mediumSections = []string{"Device", "System"}
	slowSections   = []string{"schedules", "Schedule", "network", "Zigbee", "Cloud", "SmartValve", "SmartPlug", "opentherm", "status"}
	// Sections other Wiser hubs may have; probed once, kept if the hub answers.
	extraCandidates = []string{"UpgradeInfo", "DeviceCapabilityMatrix", "Shutter", "Light", "Moment",
		"LightSchedule", "ShutterSchedule", "SmartSensor", "PowerTagE", "Equipment", "Bridge"}
)

// Poller rotates through the endpoints. With ~5s per request a fast cycle
// (rooms, boiler, hot water) completes roughly every 20s.
type Poller struct {
	hub   *Hub
	store *Store
	kick  chan string

	mu     sync.Mutex
	extras []string
}

func NewPoller(h *Hub, s *Store) *Poller {
	return &Poller{hub: h, store: s, kick: make(chan string, 32)}
}

// Refresh asks for a section to be fetched next (empty = full cycle).
func (p *Poller) Refresh(section string) {
	select {
	case p.kick <- section:
	default:
	}
}

func (p *Poller) Extras() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.extras...)
}

func (p *Poller) Run(ctx context.Context) {
	cycle := 0
	gen := p.hub.gen.Load()
	probed := false
	for ctx.Err() == nil {
		if g := p.hub.gen.Load(); g != gen {
			gen, cycle, probed = g, 0, false
			p.store.Clear()
			p.mu.Lock()
			p.extras = nil
			p.mu.Unlock()
		}
		if !p.hub.Ready() {
			p.wait(ctx, 2*time.Second)
			continue
		}
		for _, name := range p.plan(cycle) {
			if p.hub.gen.Load() != gen {
				break
			}
			p.drainKicks(ctx)
			p.fetch(ctx, name)
		}
		if !probed && p.hub.gen.Load() == gen {
			probed = true
			p.probeExtras(ctx)
		}
		cycle++
		p.wait(ctx, time.Second)
	}
}

func (p *Poller) plan(cycle int) []string {
	names := append([]string(nil), fastSections...)
	if cycle%3 == 0 {
		names = append(names, mediumSections...)
	}
	if cycle%10 == 0 {
		names = append(names, slowSections...)
		names = append(names, p.Extras()...)
	}
	return names
}

func (p *Poller) wait(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case s := <-p.kick:
		p.handleKick(ctx, s)
	case <-time.After(d):
	}
}

func (p *Poller) drainKicks(ctx context.Context) {
	for {
		select {
		case s := <-p.kick:
			p.handleKick(ctx, s)
		default:
			return
		}
	}
}

func (p *Poller) handleKick(ctx context.Context, section string) {
	if section == "" {
		for _, n := range p.plan(0) {
			p.fetch(ctx, n)
		}
		return
	}
	p.fetch(ctx, section)
}

func (p *Poller) fetch(ctx context.Context, name string) {
	r := p.hub.Do(ctx, "GET", sectionPath(name), nil, false)
	switch {
	case r.Err != nil:
		p.store.setError(name, r.Err.Error())
	case r.Status != 200:
		p.store.setError(name, "HTTP "+strconv.Itoa(r.Status)+": "+strings.TrimSpace(string(r.Body)))
	case !json.Valid(r.Body):
		p.store.setError(name, "incomplete response from hub")
	default:
		p.store.set(name, r.Body, r.Dur)
	}
}

func (p *Poller) probeExtras(ctx context.Context) {
	var found []string
	for _, name := range extraCandidates {
		r := p.hub.Do(ctx, "GET", sectionPath(name), nil, false)
		if r.Err == nil && r.Status == 200 && json.Valid(r.Body) {
			found = append(found, name)
			p.store.set(name, r.Body, r.Dur)
		}
	}
	p.mu.Lock()
	p.extras = found
	p.mu.Unlock()
}
