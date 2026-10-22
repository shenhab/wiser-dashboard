package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The MCP server lets AI assistants (Claude and other Model Context Protocol
// clients) read and control the heating. It speaks MCP's streamable HTTP
// transport at /mcp: one JSON-RPC message per POST, answered with JSON. It
// keeps no sessions and sends nothing unasked, which the transport allows.
// Access needs the dashboard's MCP token (Authorization: Bearer <token>).

var mcpProtocols = []string{"2025-06-18", "2025-03-26", "2024-11-05"} // newest first

const mcpInstructions = `Controls a Drayton/Schneider Wiser home heating system: rooms (each a thermostat), hot water and the boiler.
Temperatures are in °C. Changes made here are overrides: they last for the given minutes, or until the room's schedule next changes, and resume_schedule cancels them.
Start with get_status. Use get_history, get_boiler_cycles and get_warmup_rates to explain how the heating behaved, and get_hub_health for connection problems.
The hub is slow (a few seconds per command), so make one change at a time and confirm what you are about to change with the user.`

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// newMCPToken makes a random access token.
func newMCPToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return "wsr_" + base64.RawURLEncoding.EncodeToString(b)
}

func (s *Server) serveMCP(w http.ResponseWriter, r *http.Request) {
	// Browsers send Origin; only the dashboard's own pages may (DNS rebinding).
	if o := r.Header.Get("Origin"); o != "" {
		if u, err := url.Parse(o); err != nil || u.Host != r.Host {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
	}
	s.mu.Lock()
	token := s.mcpToken
	s.mu.Unlock()
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" || !ok || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(token)) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="wiser-dashboard"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or wrong MCP token (Authorization: Bearer <token>; see Advanced → AI assistants)"})
		return
	}
	if r.Method != http.MethodPost {
		// No server-to-client stream and no sessions to end.
		w.Header().Set("Allow", "POST")
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// A batch (allowed before protocol 2025-06-18) or a single message.
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		var reqs []rpcRequest
		if err := json.Unmarshal(body, &reqs); err != nil {
			writeJSON(w, 200, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
			return
		}
		var out []rpcResponse
		for _, req := range reqs {
			if resp, ok := s.mcpHandle(r.Context(), req); ok {
				out = append(out, resp)
			}
		}
		if len(out) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, 200, out)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, 200, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
		return
	}
	resp, ok := s.mcpHandle(r.Context(), req)
	if !ok {
		w.WriteHeader(http.StatusAccepted) // a notification or a response: nothing to say
		return
	}
	writeJSON(w, 200, resp)
}

// mcpHandle answers one message; ok is false when it needs no answer.
func (s *Server) mcpHandle(ctx context.Context, req rpcRequest) (rpcResponse, bool) {
	if len(req.ID) == 0 || req.Method == "" {
		return rpcResponse{}, false
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		proto := mcpProtocols[0] // a version we don't know: offer ours, the client decides
		for _, v := range mcpProtocols {
			if v == p.ProtocolVersion {
				proto = v
			}
		}
		resp.Result = map[string]any{
			"protocolVersion": proto,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "wiser-dashboard", "title": "Wiser heating", "version": version},
			"instructions":    mcpInstructions,
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		list := make([]map[string]any, 0, len(mcpTools))
		for _, t := range mcpTools {
			list = append(list, t.describe())
		}
		resp.Result = map[string]any{"tools": list}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcError{-32602, "invalid params"}
			break
		}
		tool := findMCPTool(p.Name)
		if tool == nil {
			resp.Error = &rpcError{-32602, "unknown tool: " + p.Name}
			break
		}
		args := p.Arguments
		if len(args) == 0 || string(args) == "null" {
			args = json.RawMessage("{}")
		}
		out, err := tool.run(s, ctx, args)
		resp.Result = toolResult(out, err)
	default:
		resp.Error = &rpcError{-32601, "method not found: " + req.Method}
	}
	return resp, true
}

// toolResult wraps a tool's answer: JSON text for every client, plus the same
// object as structuredContent for clients that read it. Errors are reported as
// tool results (isError), so the assistant can see them and correct itself.
func toolResult(out any, err error) map[string]any {
	if err != nil {
		return map[string]any{"content": []map[string]any{{"type": "text", "text": err.Error()}}, "isError": true}
	}
	text, _ := json.MarshalIndent(out, "", "  ")
	res := map[string]any{"content": []map[string]any{{"type": "text", "text": string(text)}}}
	if _, isObj := out.(map[string]any); isObj {
		res["structuredContent"] = out
	}
	return res
}

// ---------------------------------------------------------------- tool plumbing

type mcpTool struct {
	name, title, description string
	readOnly                 bool
	destructive              bool // changes something that is hard to undo (none of ours are)
	schema                   map[string]any
	run                      func(s *Server, ctx context.Context, args json.RawMessage) (any, error)
}

func (t mcpTool) describe() map[string]any {
	return map[string]any{
		"name":        t.name,
		"title":       t.title,
		"description": t.description,
		"inputSchema": t.schema,
		"annotations": map[string]any{
			"title":           t.title,
			"readOnlyHint":    t.readOnly,
			"destructiveHint": t.destructive,
			"idempotentHint":  true,
			"openWorldHint":   false,
		},
	}
}

func findMCPTool(name string) *mcpTool {
	for i := range mcpTools {
		if mcpTools[i].name == name {
			return &mcpTools[i]
		}
	}
	return nil
}

// schema builds a JSON Schema object for a tool's arguments.
func schema(required []string, props map[string]any) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	sc := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		sc["required"] = required
	}
	return sc
}

func numProp(desc string, min, max float64) map[string]any {
	return map[string]any{"type": "number", "description": desc, "minimum": min, "maximum": max}
}

func intProp(desc string, min, max int) map[string]any {
	return map[string]any{"type": "integer", "description": desc, "minimum": min, "maximum": max}
}

var roomProp = map[string]any{"type": "string", "description": "Room name (or part of it, e.g. \"ground\") or room id, as get_status lists them"}

// decodeArgs reads a tool's arguments, refusing unknown ones.
func decodeArgs(args json.RawMessage, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(args)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("bad arguments: %v", err)
	}
	return nil
}

func checkRange(what string, v, min, max float64) error {
	if v < min || v > max {
		return fmt.Errorf("%s must be between %g and %g", what, min, max)
	}
	return nil
}

// ---------------------------------------------------------------- hub data

// section decodes one cached section of the hub's data.
func (s *Server) section(name string, v any) error {
	e, ok := s.store.Snapshot()[name]
	if !ok || e.Data == nil {
		return fmt.Errorf("no %s data from the hub yet (it may still be loading)", name)
	}
	return json.Unmarshal(e.Data, v)
}

func (s *Server) roomList() ([]map[string]any, error) {
	var rooms []map[string]any
	return rooms, s.section("Room", &rooms)
}

func num(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

func intOf(v any) int {
	f, _ := num(v)
	return int(f)
}

func strOf(v any) string {
	str, _ := v.(string)
	return str
}

// findRoom matches a room by id, full name or a unique part of the name.
func (s *Server) findRoom(ref string) (map[string]any, error) {
	rooms, err := s.roomList()
	if err != nil {
		return nil, err
	}
	ref = strings.TrimSpace(ref)
	var names []string
	var partial []map[string]any
	for _, r := range rooms {
		name := strOf(r["Name"])
		names = append(names, fmt.Sprintf("%q (id %d)", name, intOf(r["id"])))
		if fmt.Sprint(intOf(r["id"])) == ref || strings.EqualFold(name, ref) {
			return r, nil
		}
		if ref != "" && strings.Contains(strings.ToLower(name), strings.ToLower(ref)) {
			partial = append(partial, r)
		}
	}
	if len(partial) == 1 {
		return partial[0], nil
	}
	return nil, fmt.Errorf("no single room matches %q; the rooms are %s", ref, strings.Join(names, ", "))
}

// celsius turns the hub's tenths of a degree into °C, nil when unknown or off.
func celsius(v any) any {
	f, ok := num(v)
	if !ok || f <= -200 || f > 1000 {
		return nil
	}
	return f / 10
}

func whenUnix(v any) any {
	f, ok := num(v)
	if !ok || f <= 0 {
		return nil
	}
	return time.Unix(int64(f), 0).Format(time.RFC3339)
}

var originNames = map[string]string{
	"FromSchedule":       "schedule",
	"FromBoost":          "boost",
	"FromManualOverride": "temporary override",
	"FromManualMode":     "manual mode",
	"FromAwayMode":       "away mode",
}

func roomStatus(r map[string]any) map[string]any {
	origin := strOf(r["SetpointOrigin"])
	source := originNames[origin]
	if source == "" {
		source = strings.ToLower(strings.TrimPrefix(origin, "From"))
	}
	sp, _ := num(r["CurrentSetPoint"])
	out := map[string]any{
		"id":              intOf(r["id"]),
		"name":            strOf(r["Name"]),
		"temperature":     celsius(r["CalculatedTemperature"]),
		"target":          celsius(r["CurrentSetPoint"]),
		"off":             sp <= -200,
		"heating":         intOf(r["PercentageDemand"]) > 0 || strOf(r["ControlOutputState"]) == "On",
		"demand_percent":  intOf(r["PercentageDemand"]),
		"setpoint_source": source,
		"scheduled":       celsius(r["ScheduledSetPoint"]),
		"window_open":     strOf(r["WindowState"]) == "Open",
	}
	if t := whenUnix(r["OverrideTimeoutUnixTime"]); t != nil && strOf(r["OverrideType"]) != "None" {
		out["override_until"] = t
	}
	return out
}

// ---------------------------------------------------------------- hub commands

// patchHub sends one change through the dashboard's queue, so the cache,
// the dashboard, Home Assistant and the Matter bridge all see it.
func (s *Server) patchHub(ctx context.Context, path string, body any) error {
	if !s.hub.Ready() {
		return errors.New("the dashboard isn't connected to a hub yet")
	}
	b, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	res := s.forward(ctx, "PATCH", path, b)
	if res.Err != nil {
		return fmt.Errorf("the hub didn't answer: %v", res.Err)
	}
	if res.Status < 200 || res.Status > 299 {
		return fmt.Errorf("the hub refused the change (HTTP %d): %s", res.Status, strings.TrimSpace(string(res.Body)))
	}
	return nil
}

// roomAfter reports a room as the cache now has it.
func (s *Server) roomAfter(id int) any {
	rooms, err := s.roomList()
	if err != nil {
		return nil
	}
	for _, r := range rooms {
		if intOf(r["id"]) == id {
			return roomStatus(r)
		}
	}
	return nil
}

func minutesText(m int) string {
	if m <= 0 {
		return "until the schedule's next change"
	}
	if m%60 == 0 {
		return fmt.Sprintf("for %d h", m/60)
	}
	return fmt.Sprintf("for %d min", m)
}
