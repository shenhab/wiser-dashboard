package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The Home Assistant Wiser integration reads /data/v2/domain/ in one request,
// which this hub can't serve (it stalls at ~2.7 KB). The dashboard answers the
// same API under /data/: the big reads come from the poller's cache, everything
// else is forwarded to the hub. Point Home Assistant at the dashboard's address
// and port, with the hub's own secret.

// cachedPaths are answered from the cache instead of the hub.
var cachedPaths = map[string]string{
	"/data/network/":   "network",
	"/data/schedules/": "schedules",
	"/data/opentherm/": "opentherm",
	"/data/openTherm/": "opentherm",
	"/data/status/":    "status",
}

// notDomain are cached sections that live outside /data/domain/.
var notDomain = map[string]bool{"network": true, "schedules": true, "opentherm": true, "status": true}

// staleAfter is how old the cached rooms may be before the API reports the hub
// as unavailable instead of serving old readings.
const staleAfter = 5 * time.Minute

func (s *Server) serveHubAPI(w http.ResponseWriter, r *http.Request) {
	_, secret := s.hub.Target()
	if secret == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("SECRET")), []byte(secret)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"Error": "Unauthorized"})
		return
	}
	// /data/v2/... and /data/... are the same API on this hub.
	path := "/data/" + strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/data/"), "v2/")

	if r.Method == http.MethodGet {
		if path == "/data/domain/" || path == "/data/domain" {
			b, err := s.domainJSON()
			if err != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"Error": err.Error()})
				return
			}
			writeRaw(w, http.StatusOK, b)
			return
		}
		name := cachedPaths[strings.TrimSuffix(path, "/")+"/"]
		if section, id, ok := parseDomainPath(path); ok && id < 0 {
			name = section
		}
		if e, ok := s.store.Snapshot()[name]; ok && e.Data != nil && time.Since(e.Fetched) < staleAfter {
			writeRaw(w, http.StatusOK, e.Data)
			return
		}
		if name == "opentherm" {
			path = "/data/opentherm/"
		}
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"Error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	res := s.forward(ctx, r.Method, path, body)
	if res.Err != nil && res.Status == 0 {
		writeJSON(w, http.StatusBadGateway, map[string]string{"Error": "hub: " + res.Err.Error()})
		return
	}
	writeRaw(w, res.Status, res.Body)
}

// domainJSON rebuilds the hub's full /data/domain/ reply from the sections the
// poller fetched one at a time.
func (s *Server) domainJSON() ([]byte, error) {
	snap := s.store.Snapshot()
	room := snap["Room"]
	if room.Data == nil || snap["System"].Data == nil {
		return nil, errors.New("still loading data from the hub")
	}
	if age := time.Since(room.Fetched); age > staleAfter {
		return nil, fmt.Errorf("no fresh data from the hub for %s", age.Round(time.Second))
	}
	out := make(map[string]json.RawMessage, len(snap))
	for name, e := range snap {
		if e.Data != nil && !notDomain[name] {
			out[name] = e.Data
		}
	}
	return json.Marshal(out)
}

func writeRaw(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}
