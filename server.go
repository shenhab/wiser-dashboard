package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

type Server struct {
	hub        *Hub
	store      *Store
	poller     *Poller
	history    *History
	health     *Health
	configPath string
	password   string
	listen     string
	matter     *Matter

	mu            sync.Mutex
	labels        map[string]string // heating channel id -> display name
	alertSettings AlertSettings
	boost         BoostSettings // Matter boost switches
	mcpToken      string
	mcpOff        bool // -mcp=false
	updater       *Updater
	autoUpdate    bool
	candidates    []Candidate
	discovering   bool
	lastScan      time.Time
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServer(http.FS(static)))
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("POST /api/request", s.handleRequest)
	mux.HandleFunc("POST /api/refresh", s.handleRefresh)
	mux.HandleFunc("POST /api/channel-name", s.handleChannelName)
	mux.HandleFunc("POST /api/room-zone", s.handleRoomZone)
	mux.HandleFunc("GET /api/history", s.handleHistory)
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/alerts", s.handleAlerts)
	mux.HandleFunc("POST /api/settings", s.handleSettings)
	mux.HandleFunc("GET /api/matter", s.handleMatter)
	mux.HandleFunc("POST /api/matter/reset", s.handleMatterReset)
	mux.HandleFunc("POST /api/matter/boost", s.handleMatterBoost)
	mux.HandleFunc("GET /api/mcp", s.handleMCPInfo)
	mux.HandleFunc("POST /api/mcp/token", s.handleMCPToken)
	mux.HandleFunc("GET /api/update", s.handleUpdate)
	mux.HandleFunc("POST /api/update/check", s.handleUpdateCheck)
	mux.HandleFunc("POST /api/update/install", s.handleUpdateInstall)
	mux.HandleFunc("POST /api/update/auto", s.handleUpdateAuto)
	mux.HandleFunc("POST /api/devices/permit-join", s.handlePermitJoin)
	mux.HandleFunc("POST /api/devices/identify", s.handleIdentify)
	mux.HandleFunc("POST /api/devices/attach", s.handleAttachDevice)
	mux.HandleFunc("GET /api/devices/unassigned", s.handleUnassigned)
	mux.HandleFunc("POST /api/rooms", s.handleCreateRoom)
	mux.HandleFunc("GET /api/setup", s.handleSetup)
	mux.HandleFunc("POST /api/setup/discover", s.handleDiscover)
	mux.HandleFunc("POST /api/setup/fetch-secret", s.handleFetchSecret)
	mux.HandleFunc("POST /api/setup/save", s.handleSave)
	var ui http.Handler = mux
	if s.password != "" {
		ui = s.basicAuth(mux)
	}
	// The hub-compatible API checks the hub's secret instead of the dashboard password.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/data/") {
			s.serveHubAPI(w, r)
			return
		}
		// The MCP server checks its own token instead of the dashboard password.
		if r.URL.Path == "/mcp" && !s.mcpOff {
			s.serveMCP(w, r)
			return
		}
		ui.ServeHTTP(w, r)
	})
}

func (s *Server) basicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pw, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(pw), []byte(s.password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="Wiser dashboard"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	ip, secret := s.hub.Target()
	s.mu.Lock()
	labels := make(map[string]string, len(s.labels))
	for k, v := range s.labels {
		labels[k] = v
	}
	settings := s.alertSettings
	s.mu.Unlock()
	if settings.MinRoomTemp == 0 {
		settings.MinRoomTemp = defaultMinRoomTemp
	}
	writeJSON(w, 200, map[string]any{
		"configured":   ip != "" && secret != "",
		"hubIP":        ip,
		"channelNames": labels,
		"alerts":       s.alerts(),
		"passwordSet":  s.password != "",
		"lanURL":       s.lanURL(),
		"settings":     map[string]any{"minRoomTemp": settings.MinRoomTemp, "off": settings.Off, "kinds": alertKinds},
		"queue":        s.hub.queued.Load(),
		"busy":         s.hub.busy.Load(),
		"sections":     s.store.Snapshot(),
		"extras":       s.poller.Extras(),
		"matter":       s.matter.State(),
		"version":      version,
		"update":       s.updater.State(),
		"now":          time.Now(),
	})
}

// lanURL is the first network address other devices can reach the dashboard at.
func (s *Server) lanURL() string {
	if urls := localURLs(s.listen); len(urls) > 1 {
		return urls[1]
	}
	return ""
}

type rawRequest struct {
	Method  string          `json:"method"`
	Path    string          `json:"path"`
	Body    json.RawMessage `json:"body,omitempty"`
	RawBody string          `json:"rawBody,omitempty"`
}

// handleRequest passes any request straight to the hub: this is what gives
// the UI (and the console) full control of every endpoint.
func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	var req rawRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request: " + err.Error()})
		return
	}
	req.Method = strings.ToUpper(strings.TrimSpace(req.Method))
	if req.Method == "" {
		req.Method = "GET"
	}
	if !strings.HasPrefix(req.Path, "/") {
		writeJSON(w, 400, map[string]string{"error": "path must start with /"})
		return
	}
	body := []byte(req.RawBody)
	if len(body) == 0 && len(req.Body) > 0 && string(req.Body) != "null" {
		body = req.Body
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	res := s.forward(ctx, req.Method, req.Path, body)

	out := map[string]any{"status": res.Status, "body": string(res.Body), "ms": res.Dur.Milliseconds()}
	if res.Err != nil {
		out["error"] = res.Err.Error()
	}
	writeJSON(w, 200, out)
}

// forward sends a request to the hub ahead of polling and, for writes, updates
// the cache so the dashboard and Home Assistant see the change straight away.
func (s *Server) forward(ctx context.Context, method, path string, body []byte) result {
	res := s.hub.Do(ctx, method, path, body, true)
	if method == "GET" {
		return res
	}
	log.Printf("%s %s -> %d %v", method, path, res.Status, res.Err)
	if res.Err == nil && res.Status >= 200 && res.Status < 300 {
		sec := sectionFor(path)
		if method == "PATCH" {
			sec = s.store.MergeWrite(path, res.Body)
		}
		if sec != "" {
			s.poller.Refresh(sec)
		}
	}
	return res
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Section string `json:"section"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	s.poller.Refresh(req.Section)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleHistory serves recorded samples (?hours=24, max 14 days) and the
// per-day relay runtime (?days=7).
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	hours, err := strconv.Atoi(r.URL.Query().Get("hours"))
	if err != nil || hours <= 0 {
		hours = 24
	}
	hours = min(hours, historyDays*24)
	days, err := strconv.Atoi(r.URL.Query().Get("days"))
	if err != nil || days <= 0 {
		days = 7
	}
	days = min(days, historyDays)
	writeJSON(w, 200, map[string]any{
		"samples":   s.history.Since(time.Now().Add(-time.Duration(hours)*time.Hour), 1500),
		"runtime":   s.history.Runtime(days),
		"cycles":    s.history.Cycles(days),
		"heatRates": s.history.HeatRates(),
	})
}

// handleHealth serves the hub health log (?days=7, max 14 days).
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	days, err := strconv.Atoi(r.URL.Query().Get("days"))
	if err != nil || days <= 0 {
		days = 7
	}
	samples, events := s.health.Since(time.Now().AddDate(0, 0, -min(days, historyDays)))
	if samples == nil {
		samples = []HealthSample{}
	}
	if events == nil {
		events = []HealthEvent{}
	}
	writeJSON(w, 200, map[string]any{"samples": samples, "events": events})
}

func (s *Server) handleChannelName(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "id is required"})
		return
	}
	s.mu.Lock()
	if s.labels == nil {
		s.labels = map[string]string{}
	}
	if name := strings.TrimSpace(req.Name); name == "" {
		delete(s.labels, req.ID) // back to the hub's name
	} else {
		s.labels[req.ID] = name
	}
	s.mu.Unlock()
	ip, secret := s.hub.Target()
	if err := s.save(ip, secret); err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	ip, secret := s.hub.Target()
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, 200, map[string]any{
		"hubIP":       ip,
		"hasSecret":   secret != "",
		"candidates":  s.candidates,
		"discovering": s.discovering,
		"configPath":  s.configPath,
	})
}

// runDiscovery is shared by startup and the setup page.
func (s *Server) runDiscovery(ctx context.Context) []Candidate {
	s.mu.Lock()
	if s.discovering {
		s.mu.Unlock()
		for {
			time.Sleep(500 * time.Millisecond)
			s.mu.Lock()
			if !s.discovering {
				c := s.candidates
				s.mu.Unlock()
				return c
			}
			s.mu.Unlock()
		}
	}
	s.discovering = true
	s.mu.Unlock()

	_, secret := s.hub.Target()
	c := discover(ctx, secret)

	s.mu.Lock()
	s.candidates, s.discovering, s.lastScan = c, false, time.Now()
	s.mu.Unlock()
	return c
}

func (s *Server) handleDiscover(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"candidates": s.runDiscovery(r.Context())})
}

func (s *Server) handleFetchSecret(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IP string `json:"ip"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.IP == "" {
		req.IP, _ = s.hub.Target()
	}
	secret, from, err := fetchSecret(req.IP)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// Keep the secret server-side; the browser never sees it.
	if req.IP == "" {
		req.IP = from
	}
	if err := s.apply(req.IP, secret); err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "from": from})
}

func (s *Server) handleSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IP     string `json:"ip"`
		Secret string `json:"secret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	req.IP, req.Secret = strings.TrimSpace(req.IP), strings.TrimSpace(req.Secret)
	if req.Secret == "" {
		_, req.Secret = s.hub.Target() // keep the current one
	}
	if req.IP == "" || req.Secret == "" {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "both the hub IP and the secret are needed"})
		return
	}
	info, err := validateHub(req.IP, req.Secret)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "could not connect: " + err.Error()})
		return
	}
	if err := s.apply(req.IP, req.Secret); err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "info": info})
}

func (s *Server) apply(ip, secret string) error {
	if cur, curSecret := s.hub.Target(); cur != ip || curSecret != secret {
		s.hub.SetTarget(ip, secret)
	}
	if err := s.save(ip, secret); err != nil {
		return err
	}
	log.Printf("hub set to %s, saved to %s", ip, s.configPath)
	return nil
}

func (s *Server) save(ip, secret string) error {
	if s.configPath == "" {
		return nil
	}
	s.mu.Lock()
	cfg := Config{HubIP: ip, Secret: secret, ChannelNames: s.labels, Alerts: s.alertSettings, Boost: s.boost, MCPToken: s.mcpToken, AutoUpdate: s.autoUpdate}
	err := saveConfig(s.configPath, cfg)
	s.mu.Unlock()
	return err
}

// handleMatter reports the Matter bridge: pairing codes, paired controllers.
func (s *Server) handleMatter(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.matter.State())
}

// handleMatterReset removes every Matter pairing.
func (s *Server) handleMatterReset(w http.ResponseWriter, r *http.Request) {
	if s.matter == nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "this build has no Matter bridge"})
		return
	}
	if err := s.matter.Reset(); err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleMatterBoost saves the boost switch settings and hands them to the bridge.
func (s *Server) handleMatterBoost(w http.ResponseWriter, r *http.Request) {
	var b BoostSettings
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "bad request: " + err.Error()})
		return
	}
	if b.Minutes < 15 || b.Minutes > 240 || b.Delta < 5 || b.Delta > 50 {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "minutes must be 15-240 and the increase 0.5-5°"})
		return
	}
	s.mu.Lock()
	s.boost = b.withDefaults()
	s.mu.Unlock()
	if s.matter != nil {
		s.matter.SetBoost(b)
	}
	ip, secret := s.hub.Target()
	if err := s.save(ip, secret); err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleMCPInfo tells the settings page how to connect an AI assistant.
func (s *Server) handleMCPInfo(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	token := s.mcpToken
	s.mu.Unlock()
	base := s.lanURL()
	if base == "" {
		base = "http://" + r.Host
	}
	writeJSON(w, 200, map[string]any{"enabled": !s.mcpOff, "url": base + "/mcp", "token": token, "tools": len(mcpTools)})
}

// handleMCPToken replaces the MCP token; assistants using the old one lose access.
func (s *Server) handleMCPToken(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.mcpToken = newMCPToken()
	s.mu.Unlock()
	ip, secret := s.hub.Target()
	if err := s.save(ip, secret); err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleUpdate reports this version and whether a newer one is available.
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.updater.State())
}

func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	s.updater.Check(r.Context())
	writeJSON(w, 200, s.updater.State())
}

// handleUpdateInstall starts the update; the dashboard restarts when it's done.
func (s *Server) handleUpdateInstall(w http.ResponseWriter, r *http.Request) {
	st := s.updater.State()
	if !st.Available || st.Busy != "" {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "no update to install right now"})
		return
	}
	go s.updater.Install(context.Background())
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleUpdateAuto turns automatic updates on or off.
func (s *Server) handleUpdateAuto(w http.ResponseWriter, r *http.Request) {
	var req struct {
		On bool `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "bad request"})
		return
	}
	s.mu.Lock()
	s.autoUpdate = req.On
	s.mu.Unlock()
	ip, secret := s.hub.Target()
	if err := s.save(ip, secret); err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
