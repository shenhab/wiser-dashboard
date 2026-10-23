package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "wsr_test-token"

// fakeHub stands in for the Wiser hub: it records the changes it receives and,
// like the real hub, answers each with the full updated item.
type fakeHub struct {
	mu      sync.Mutex
	items   map[string]map[string]any // "/data/domain/Room/2" -> item
	patches []struct {
		path string
		body map[string]any
	}
}

// load keeps a copy of every item of a section, by its PATCH path.
func (f *fakeHub) load(section, data string) {
	if f.items == nil {
		f.items = map[string]map[string]any{}
	}
	if strings.HasPrefix(data, "{") {
		var item map[string]any
		json.Unmarshal([]byte(data), &item)
		f.items["/data/domain/"+section] = item
		return
	}
	var items []map[string]any
	json.Unmarshal([]byte(data), &items)
	for _, it := range items {
		f.items["/data/domain/"+section+"/"+itoa(int64(intOf(it["id"])))] = it
	}
}

func (f *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("SECRET") != "hub-secret" {
		w.WriteHeader(401)
		return
	}
	if r.Method == "PATCH" {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.patches = append(f.patches, struct {
			path string
			body map[string]any
		}{r.URL.Path, body})
		item := f.items[r.URL.Path]
		if item == nil {
			w.WriteHeader(404)
			return
		}
		if ov, ok := body["RequestOverride"].(map[string]any); ok {
			if ov["Type"] == "None" {
				item["OverrideType"] = "None"
				// Items that carry their schedule's value go back to it, as on the hub.
				if sp, ok := item["ScheduledSetPoint"]; ok {
					item["CurrentSetPoint"], item["SetpointOrigin"] = sp, "FromSchedule"
				}
				if st, ok := item["ScheduledWaterHeatingState"]; ok {
					item["WaterHeatingState"], item["HotWaterDescription"] = st, "FromSchedule"
				}
			} else if sp, ok := ov["SetPoint"]; ok {
				item["CurrentSetPoint"], item["OverrideType"] = sp, "Manual"
				if _, ok := item["ScheduledWaterHeatingState"]; ok {
					item["WaterHeatingState"] = map[bool]string{true: "On", false: "Off"}[intOf(sp) > 0]
				}
			}
		}
		json.NewEncoder(w).Encode(item)
		return
	}
	w.WriteHeader(404)
}

func (f *fakeHub) take() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, p := range f.patches {
		out = append(out, map[string]any{"path": p.path, "ov": p.body["RequestOverride"]})
	}
	f.patches = nil
	return out
}

// newMCPTestServer is a dashboard with cached hub data, a little history and
// a fake hub behind it.
func newMCPTestServer(t *testing.T) (*httptest.Server, *fakeHub) {
	t.Helper()
	fh := &fakeHub{}
	hubSrv := httptest.NewServer(fh)
	t.Cleanup(hubSrv.Close)

	hub := NewHub()
	hub.SetTarget(strings.TrimPrefix(hubSrv.URL, "http://"), "hub-secret")
	store := NewStore()
	until := time.Now().Add(40 * time.Minute).Unix()
	cache := func(section, data string) {
		store.set(section, []byte(data), 0)
		fh.load(section, data)
	}
	cache("Room", `[
		{"id":1,"Name":"Upper Floor","CalculatedTemperature":195,"CurrentSetPoint":-200,"PercentageDemand":0,"SetpointOrigin":"FromSchedule","OverrideType":"None","ScheduleId":1},
		{"id":2,"Name":"Ground Floor","CalculatedTemperature":183,"CurrentSetPoint":200,"PercentageDemand":45,"SetpointOrigin":"FromBoost","OverrideType":"Manual","OverrideTimeoutUnixTime":`+itoa(until)+`,"ScheduleId":2}
	]`)
	cache("HotWater", `[{"id":2,"WaterHeatingState":"Off","Mode":"Auto","OverrideType":"None","HotWaterRelayState":"Off","ScheduleId":1000}]`)
	cache("HeatingChannel", `[{"id":1,"Name":"Channel-1","RoomIds":[2],"HeatingRelayState":"On"},{"id":3,"Name":"Channel-3","RoomIds":[1],"HeatingRelayState":"Off"}]`)
	cache("System", `{"AwayModeSetPointLimit":150,"OverrideType":"Away"}`)
	store.set("schedules", []byte(`{"Heating":[{"id":2,"Name":"Ground","Monday":{"Time":[630,2200],"DegreesC":[210,-200]}}],"OnOff":[{"id":1000,"Monday":[630,-700,1530,-1600]}]}`), 0)

	history := NewHistory("")
	now := time.Now().Unix()
	for i := int64(0); i < 120; i++ {
		history.append(Sample{T: now - (120-i)*60, Rooms: map[string][4]int{"1": {190, -200, 0, -1}, "2": {180 + int(i/20), 200, 50, -1}}, Ch: map[string]int{"1": 1}, HW: 0})
	}

	srv := &Server{hub: hub, store: store, poller: NewPoller(hub, store), history: history, health: NewHealth(""),
		listen: "127.0.0.1:0", mcpToken: testToken, labels: map[string]string{"1": "Ground Floor zone"}}
	ts := httptest.NewServer(srv.routes())
	t.Cleanup(ts.Close)
	return ts, fh
}

func itoa(v int64) string { b, _ := json.Marshal(v); return string(b) }

// rpc posts one JSON-RPC message and decodes the reply.
func rpc(t *testing.T, ts *httptest.Server, method string, params any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": params})
	req, _ := http.NewRequest("POST", ts.URL+"/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("%s: HTTP %d %s", method, res.StatusCode, b)
	}
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	if out["id"] != float64(7) || out["jsonrpc"] != "2.0" {
		t.Fatalf("%s: bad envelope %v", method, out)
	}
	return out
}

// call runs a tool; it returns the structured result, or the error text.
func call(t *testing.T, ts *httptest.Server, tool string, args map[string]any) (map[string]any, string) {
	t.Helper()
	res := rpc(t, ts, "tools/call", map[string]any{"name": tool, "arguments": args})["result"].(map[string]any)
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if res["isError"] == true {
		return nil, text
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%s: text isn't JSON: %v", tool, err)
	}
	return out, ""
}

func TestMCPAccess(t *testing.T) {
	ts, _ := newMCPTestServer(t)
	post := func(auth, origin string) int {
		req, _ := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	for _, c := range []struct {
		name, auth, origin string
		want               int
	}{
		{"no token", "", "", 401},
		{"wrong token", "Bearer nope", "", 401},
		{"hub secret is not the token", "Bearer hub-secret", "", 401},
		{"right token", "Bearer " + testToken, "", 200},
		{"foreign web page", "Bearer " + testToken, "http://evil.example", 403},
		{"the dashboard's own page", "Bearer " + testToken, ts.URL, 200},
	} {
		if got := post(c.auth, c.origin); got != c.want {
			t.Errorf("%s: HTTP %d, want %d", c.name, got, c.want)
		}
	}
	req, _ := http.NewRequest("GET", ts.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 405 {
		t.Errorf("GET: HTTP %d, want 405", res.StatusCode)
	}
}

func TestMCPProtocol(t *testing.T) {
	ts, _ := newMCPTestServer(t)
	init := rpc(t, ts, "initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test"}})["result"].(map[string]any)
	if init["protocolVersion"] != "2025-03-26" {
		t.Errorf("known version not kept: %v", init["protocolVersion"])
	}
	if init["serverInfo"].(map[string]any)["name"] != "wiser-dashboard" || init["capabilities"].(map[string]any)["tools"] == nil {
		t.Errorf("bad initialize result: %v", init)
	}
	if v := rpc(t, ts, "initialize", map[string]any{"protocolVersion": "1999-01-01"})["result"].(map[string]any)["protocolVersion"]; v != mcpProtocols[0] {
		t.Errorf("unknown version: got %v, want our newest", v)
	}

	// A notification gets 202 and no body.
	req, _ := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 202 {
		t.Errorf("notification: HTTP %d, want 202", res.StatusCode)
	}
	if e := rpc(t, ts, "no/such/method", nil)["error"].(map[string]any); e["code"] != float64(-32601) {
		t.Errorf("unknown method: %v", e)
	}

	tools := rpc(t, ts, "tools/list", nil)["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 13 {
		t.Errorf("%d tools, want 13", len(tools))
	}
	for _, x := range tools {
		tool := x.(map[string]any)
		ann := tool["annotations"].(map[string]any)
		readOnly := strings.HasPrefix(tool["name"].(string), "get_")
		if ann["readOnlyHint"] != readOnly || tool["inputSchema"].(map[string]any)["type"] != "object" || tool["description"] == "" {
			t.Errorf("tool %v: bad description or annotations", tool["name"])
		}
	}
}

func TestMCPReadTools(t *testing.T) {
	ts, _ := newMCPTestServer(t)
	st, errText := call(t, ts, "get_status", nil)
	if errText != "" {
		t.Fatal(errText)
	}
	rooms := st["rooms"].([]any)
	upper, ground := rooms[0].(map[string]any), rooms[1].(map[string]any)
	if upper["temperature"] != 19.5 || upper["target"] != nil || upper["off"] != true || upper["setpoint_source"] != "schedule" {
		t.Errorf("upper floor: %v", upper)
	}
	if ground["target"] != 20.0 || ground["heating"] != true || ground["setpoint_source"] != "boost" || ground["override_until"] == nil {
		t.Errorf("ground floor: %v", ground)
	}
	if st["away_mode"] != true {
		t.Error("away mode set on the hub isn't reported (no room shows it as its target's source)")
	}
	if st["boiler_running"] != true || st["hot_water"].(map[string]any)["heating"] != false {
		t.Errorf("boiler/hot water: %v", st)
	}
	if z := st["zones"].([]any)[0].(map[string]any); z["name"] != "Ground Floor zone" {
		t.Errorf("zone label not used: %v", z)
	}

	h, errText := call(t, ts, "get_history", map[string]any{"hours": 3, "room": "ground"})
	if errText != "" {
		t.Fatal(errText)
	}
	points := h["points"].([]any)
	if len(points) < 6 || h["interval_minutes"] != float64(10) {
		t.Errorf("history: %d points every %v min", len(points), h["interval_minutes"])
	}
	last := points[len(points)-1].(map[string]any)
	if _, ok := last["rooms"].(map[string]any)["Ground Floor"]; !ok || len(last["rooms"].(map[string]any)) != 1 || last["heating_percent"] != float64(100) {
		t.Errorf("history point: %v", last)
	}

	sc, errText := call(t, ts, "get_schedules", nil)
	if errText != "" {
		t.Fatal(errText)
	}
	found := 0
	for _, x := range sc["schedules"].([]any) {
		s := x.(map[string]any)
		monday, _ := json.Marshal(s["days"].(map[string]any)["Monday"])
		switch s["type"] {
		case "OnOff":
			found++
			if string(monday) != `[{"off":"07:00","on":"06:30"},{"off":"16:00","on":"15:30"}]` || s["used_by"].([]any)[0] != "hot water" {
				t.Errorf("hot water schedule: %s %v", monday, s["used_by"])
			}
		case "Heating":
			found++
			if string(monday) != `[{"from":"06:30","temperature":21},{"from":"22:00","temperature":"off"}]` || s["used_by"].([]any)[0] != "Ground Floor" {
				t.Errorf("heating schedule: %s %v", monday, s["used_by"])
			}
		}
	}
	if found != 2 {
		t.Errorf("schedules: %v", sc)
	}
	for _, tool := range []string{"get_boiler_cycles", "get_warmup_rates", "get_alerts", "get_hub_health"} {
		if _, errText := call(t, ts, tool, nil); errText != "" {
			t.Errorf("%s: %s", tool, errText)
		}
	}
}

func TestMCPControlTools(t *testing.T) {
	ts, fh := newMCPTestServer(t)
	ov := func(tool string, args map[string]any) (string, map[string]any) {
		t.Helper()
		if _, errText := call(t, ts, tool, args); errText != "" {
			t.Fatalf("%s: %s", tool, errText)
		}
		p := fh.take()
		if len(p) != 1 {
			t.Fatalf("%s: %d changes reached the hub, want 1", tool, len(p))
		}
		return p[0]["path"].(string), p[0]["ov"].(map[string]any)
	}
	same := func(name string, got map[string]any, want string) {
		t.Helper()
		g, _ := json.Marshal(got)
		if string(g) != want {
			t.Errorf("%s: sent %s, want %s", name, g, want)
		}
	}

	path, got := ov("set_room_temperature", map[string]any{"room": "ground", "temperature": 21.3, "minutes": 60})
	same("set 21.3° for 60 min (half-degree steps)", got, `{"DurationMinutes":60,"Originator":"App","SetPoint":215,"Type":"Manual"}`)
	if path != "/data/domain/Room/2" {
		t.Errorf("room by partial name went to %s", path)
	}
	_, got = ov("set_room_temperature", map[string]any{"room": "1", "temperature": 19})
	same("set until the schedule changes", got, `{"SetPoint":190,"Type":"Manual"}`)
	_, got = ov("boost_room", map[string]any{"room": "Ground Floor"})
	same("boost: 18.3 + 3 = 21.5 for 30 min", got, `{"DurationMinutes":30,"Originator":"App","SetPoint":215,"Type":"Manual"}`)
	_, got = ov("boost_room", map[string]any{"room": "Ground Floor", "increase": 1, "minutes": 90})
	same("boost keeps the higher target (21.5, from the boost above, > 18.3 + 1)", got, `{"DurationMinutes":90,"Originator":"App","SetPoint":215,"Type":"Manual"}`)
	_, got = ov("room_off", map[string]any{"room": "upper"})
	same("room off", got, `{"SetPoint":-200,"Type":"Manual"}`)
	path, got = ov("hot_water", map[string]any{"action": "boost"})
	same("hot water boost", got, `{"DurationMinutes":30,"Originator":"App","SetPoint":1100,"Type":"Manual"}`)
	if path != "/data/domain/HotWater/2" {
		t.Errorf("hot water went to %s", path)
	}
	_, got = ov("hot_water", map[string]any{"action": "schedule"})
	same("hot water back to schedule", got, `{"Type":"None"}`)
	path, got = ov("set_away_mode", map[string]any{"on": true})
	same("away on at the hub's away temperature", got, `{"SetPoint":150,"Type":2}`)
	if path != "/data/domain/System" {
		t.Errorf("away went to %s", path)
	}

	if _, errText := call(t, ts, "resume_schedule", map[string]any{"room": "all"}); errText != "" || len(fh.take()) != 2 {
		t.Errorf("resume all: %q", errText)
	}

	// Refused before anything reaches the hub.
	for _, c := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"set_room_temperature", map[string]any{"room": "ground", "temperature": 40}, "between 5 and 30"},
		{"set_room_temperature", map[string]any{"room": "ground", "temperature": 20, "minutes": 5}, "minutes"},
		{"set_room_temperature", map[string]any{"room": "floor", "temperature": 20}, "Upper Floor"},
		{"set_room_temperature", map[string]any{"room": "ground", "temperature": 20, "colour": "red"}, "unknown field"},
		{"boost_room", map[string]any{"room": "ground", "increase": 9}, "increase"},
		{"hot_water", map[string]any{"action": "on", "minutes": 30}, "only apply to"},
		{"hot_water", map[string]any{"action": "explode"}, "boost, on, off or schedule"},
		{"set_away_mode", map[string]any{}, "required"},
	} {
		if _, errText := call(t, ts, c.tool, c.args); !strings.Contains(errText, c.want) {
			t.Errorf("%s %v: error %q, want it to mention %q", c.tool, c.args, errText, c.want)
		}
	}
	if p := fh.take(); len(p) != 0 {
		t.Errorf("refused calls still reached the hub: %v", p)
	}
}

func TestMCPStdioRelay(t *testing.T) {
	ts, _ := newMCPTestServer(t)
	c := &http.Client{Timeout: 10 * time.Second}
	reply, err := relay(c, ts.URL+"/mcp", testToken, []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`))
	if err != nil || !bytes.Contains(reply, []byte(`"get_status"`)) || bytes.Contains(reply, []byte("\n")) {
		t.Errorf("relay: %v %.80s", err, reply)
	}
	if reply, err := relay(c, ts.URL+"/mcp", testToken, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); err != nil || reply != nil {
		t.Errorf("notification relayed a reply: %s %v", reply, err)
	}
	_, err = relay(c, ts.URL+"/mcp", "wrong", []byte(`{"jsonrpc":"2.0","id":4,"method":"ping"}`))
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("wrong token: %v", err)
	}
	if e := errorReply([]byte(`{"jsonrpc":"2.0","id":4,"method":"ping"}`), err); !bytes.Contains(e, []byte(`"id":4`)) {
		t.Errorf("error reply: %s", e)
	}
}
