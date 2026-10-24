package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Alerts are worked out from the cached hub data and the health log each time
// they're asked for. The UI shows them, and Home Assistant can read
// /api/alerts with a REST sensor to send notifications.

// AlertSettings are saved in the config file.
type AlertSettings struct {
	MinRoomTemp int             `json:"min_room_temp,omitempty"` // tenths of °C; 0 means defaultMinRoomTemp
	Off         map[string]bool `json:"off,omitempty"`           // alert kinds turned off
}

const defaultMinRoomTemp = 120

// alertKinds lists every alert the dashboard can raise, for the settings page.
var alertKinds = []struct{ Kind, Label string }{
	{"hub-offline", "Hub not answering"},
	{"battery", "Thermostat battery low"},
	{"signal", "Thermostat signal weak"},
	{"cold-room", "Room below the minimum temperature"},
	{"window", "Open window detected"},
	{"hub-restart", "Hub restarted itself"},
	{"wifi", "Hub Wi-Fi trouble"},
	{"clock", "Hub clock wrong"},
	{"pending", "Change not reaching a thermostat"},
	{"away", "Away mode on"},
	{"schedule-empty", "Room schedule is off all week"},
	{"guard-returned", "Waste guard returned a room or the hot water to its schedule"},
	{"schedule-long", "A schedule keeps heating on too long (waste guard)"},
	{"heating-24h", "Too much heating in 24 hours (waste guard)"},
}

type Alert struct {
	Kind   string `json:"kind"`
	Level  string `json:"level"` // "critical", "warning" or "info"
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	Figure string `json:"figure,omitempty"` // a number to show large, e.g. "14 h 20 min"
	Short  string `json:"short,omitempty"`  // at most 32 characters, for a Matter device name
	Since  int64  `json:"since,omitempty"`  // unix time this occurrence began (waste guard alerts)
}

var levelRank = map[string]int{"critical": 0, "warning": 1, "info": 2}

func (s *Server) alerts() []Alert {
	s.mu.Lock()
	set := s.alertSettings
	s.mu.Unlock()
	minTemp := set.MinRoomTemp
	if minTemp == 0 {
		minTemp = defaultMinRoomTemp
	}
	snap := s.store.Snapshot()
	var out []Alert
	add := func(kind, level, title, detail string) {
		if !set.Off[kind] {
			out = append(out, Alert{Kind: kind, Level: level, Title: title, Detail: detail})
		}
	}

	if ip, secret := s.hub.Target(); ip == "" || secret == "" {
		return nil
	}
	room := snap["Room"]
	if room.Data == nil {
		return nil // still starting up
	}
	// Data saved before a restart is as old as the downtime; until the hub
	// answers, judge it by how long this run has gone without a reply.
	since := room.Fetched
	if room.Restored && startedAt.After(since) {
		since = startedAt
	}
	if age := time.Since(since); age > staleAfter {
		add("hub-offline", "critical", "The hub isn't answering",
			fmt.Sprintf("No reply for %s. Heating keeps running on the hub's own schedule.", age.Round(time.Minute)))
	}

	var rooms []struct {
		ID                    int `json:"id"`
		Name                  string
		CalculatedTemperature int
		ScheduleId            int
		RoomStatId            int
		SetpointOrigin        string
		WindowState           string
	}
	json.Unmarshal(room.Data, &rooms)
	for _, r := range rooms {
		if t := r.CalculatedTemperature; t > -1000 && t < minTemp {
			add("cold-room", "warning", r.Name+" is cold",
				fmt.Sprintf("%.1f°C, below your %.1f°C minimum.", float64(t)/10, float64(minTemp)/10))
		}
		if r.WindowState == "Open" || strings.Contains(r.SetpointOrigin, "Window") {
			add("window", "warning", "Open window in "+r.Name, "Heating there is paused until the temperature recovers.")
		}
	}

	var devices []struct {
		ID                       int `json:"id"`
		ProductType              string
		BatteryLevel             string
		DisplayedSignalStrength  string
		PendingZigbeeMessageMask int
	}
	json.Unmarshal(snap["Device"].Data, &devices)
	statRoom := map[int]string{}
	for _, r := range rooms {
		statRoom[r.RoomStatId] = r.Name
	}
	for _, d := range devices {
		if d.ID == 0 {
			continue
		}
		name := d.ProductType
		if n, ok := statRoom[d.ID]; ok {
			name = n + " thermostat"
		}
		switch d.BatteryLevel {
		case "Low", "Dead":
			add("battery", "critical", name+": replace the batteries", "Battery level "+d.BatteryLevel+".")
		case "OneThird":
			add("battery", "warning", name+": batteries running low", "About a third left.")
		}
		switch d.DisplayedSignalStrength {
		case "Poor", "VeryPoor", "NoSignal":
			add("signal", "warning", name+": weak signal", "Signal "+spaced(d.DisplayedSignalStrength)+". Move it, or the hub, closer.")
		}
		if d.PendingZigbeeMessageMask > 0 {
			add("pending", "info", name+": a change is waiting", "Battery thermostats collect changes when they next wake, usually within a few minutes.")
		}
	}

	var sys struct {
		UnixTime     int64
		OverrideType string
	}
	if e := snap["System"]; e.Data != nil && json.Unmarshal(e.Data, &sys) == nil && sys.UnixTime > 0 {
		hubNow := sys.UnixTime + int64(time.Since(e.Fetched).Seconds())
		if d := time.Duration(abs64(hubNow-time.Now().Unix())) * time.Second; d > 5*time.Minute && time.Since(e.Fetched) < staleAfter {
			add("clock", "warning", "The hub's clock is off by "+d.Round(time.Minute).String(), "Schedules run at the wrong time. Restarting the hub with internet access usually fixes it.")
		}
		if sys.OverrideType == "Away" {
			add("away", "info", "Away mode is on", "")
		}
	}

	if s.health != nil {
		_, events := s.health.Since(time.Now().Add(-24 * time.Hour))
		drops := 0
		for _, e := range events {
			switch e.Kind {
			case "restart":
				add("hub-restart", "warning", "The hub restarted at "+time.Unix(e.T, 0).Format("Mon 15:04"), e.Detail)
			case "wifi-drop":
				drops++
			}
		}
		if drops >= 3 {
			add("wifi", "warning", fmt.Sprintf("The hub lost Wi-Fi %d times in 24 hours", drops), "Its link to the router is unreliable.")
		}
	}
	var network struct {
		TcpStats_SinceBoot struct{ SegmentsSent_Count, SegmentRetransmissions_Count float64 }
	}
	if json.Unmarshal(snap["network"].Data, &network) == nil {
		if t := network.TcpStats_SinceBoot; t.SegmentsSent_Count > 200 && t.SegmentRetransmissions_Count/t.SegmentsSent_Count > 0.2 {
			add("wifi", "info", fmt.Sprintf("%.0f%% of the hub's network traffic is resent", 100*t.SegmentRetransmissions_Count/t.SegmentsSent_Count),
				"A weak Wi-Fi link. The dashboard works around it.")
		}
	}

	if raw := snap["schedules"].Data; raw != nil {
		var all map[string][]map[string]json.RawMessage
		json.Unmarshal(raw, &all)
		empty := map[int]bool{}
		for _, s := range all["Heating"] {
			var id int
			json.Unmarshal(s["id"], &id)
			off := true
			for _, d := range []string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"} {
				var day struct{ DegreesC []int }
				json.Unmarshal(s[d], &day)
				for _, v := range day.DegreesC {
					if v > -200 {
						off = false
					}
				}
			}
			empty[id] = off
		}
		for _, r := range rooms {
			if empty[r.ScheduleId] {
				add("schedule-empty", "info", r.Name+" has no heating scheduled", "It only heats when you boost or set a temperature.")
			}
		}
	}

	for _, a := range s.guard.Alerts() {
		if !set.Off[a.Kind] {
			out = append(out, a)
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return levelRank[out[i].Level] < levelRank[out[j].Level] })
	return out
}

func spaced(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}

// handleAlerts serves the alerts for the UI and for a Home Assistant REST
// sensor: "count" is critical + warning alerts, "summary" one line of them.
func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	list := s.alerts()
	count, worst := 0, "ok"
	var lines []string
	for _, a := range list {
		if a.Level != "info" {
			count++
			lines = append(lines, a.Title)
		}
		if worst == "ok" || levelRank[a.Level] < levelRank[worst] {
			worst = a.Level
		}
	}
	if list == nil {
		list = []Alert{}
	}
	writeJSON(w, 200, map[string]any{"count": count, "level": worst, "summary": strings.Join(lines, "; "), "alerts": list})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	var req AlertSettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if req.MinRoomTemp != 0 && (req.MinRoomTemp < 50 || req.MinRoomTemp > 250) {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "minimum temperature must be between 5° and 25°"})
		return
	}
	s.mu.Lock()
	s.alertSettings = req
	s.mu.Unlock()
	ip, secret := s.hub.Target()
	if err := s.save(ip, secret); err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
