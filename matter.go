package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"
)

// matterBinary is the Matter bridge (matter/, compiled with Bun) for this
// platform. Release builds embed it with the "matter" build tag; other builds
// can point -matter-bin at one.
var matterBinary []byte

// BoostSettings shape the bridge's per-room heating boost switches in Google
// Home and the other Matter apps.
type BoostSettings struct {
	Minutes int             `json:"minutes"` // how long a boost lasts
	Delta   int             `json:"delta"`   // tenths of a degree above the room temperature
	Off     map[string]bool `json:"off"`     // room id -> no boost switch for this room
}

func (b BoostSettings) withDefaults() BoostSettings {
	if b.Minutes <= 0 {
		b.Minutes = 30
	}
	if b.Delta <= 0 {
		b.Delta = 30
	}
	if b.Off == nil {
		b.Off = map[string]bool{}
	}
	return b
}

// MatterStatus is what the bridge last reported about itself.
type MatterStatus struct {
	Version      string   `json:"version"`
	Port         int      `json:"port"`
	Commissioned bool     `json:"commissioned"`
	ManualCode   string   `json:"manualCode"`
	QRCode       string   `json:"qrCode"`
	QRRows       []string `json:"qrRows"` // QR modules, "1" = dark
	Fabrics      []struct {
		Label    string `json:"label"`
		VendorID int    `json:"vendorId"`
	} `json:"fabrics"`
	Devices      int    `json:"devices"`
	HubReachable bool   `json:"hubReachable"`
	Error        string `json:"error"`
}

// Matter runs the bridge as a child process, restarts it if it stops, and
// keeps it supplied with the dashboard's address and the hub secret.
type Matter struct {
	hub      *Hub
	bin      string // path to an external binary; empty means the embedded one
	dir      string // state directory (pairings, identity)
	port     int    // Matter UDP port
	localURL string // where the bridge reaches the dashboard's /data/ API

	mu       sync.Mutex
	status   *MatterStatus
	running  bool
	lastErr  string
	stdin    io.WriteCloser
	boost    BoostSettings
	boostVer int // bumped on every change, so a running bridge gets it
	alert    MatterAlert
	alertVer int
}

// MatterAlert is the waste guard's "Heating alert" contact sensor: shown while
// the guard is on, open while one of its alerts is active.
type MatterAlert struct {
	Enabled bool         `json:"enabled"` // the Heating alert sensor is shown
	On      bool         `json:"on"`
	Texts   []MatterText `json:"texts"`           // devices named with the active alerts (option)
	Hours   *float64     `json:"hours,omitempty"` // the last 24 hours' heating, as a temperature sensor (option)
}

// MatterText is a device named with an alert. Its ID is new for every
// occurrence, so the apps announce it as a new device each time.
type MatterText struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// SetAlert updates the guard's devices; a running bridge applies it within seconds.
func (m *Matter) SetAlert(a MatterAlert) {
	if m == nil {
		return
	}
	a.On = a.On && a.Enabled
	m.mu.Lock()
	if !reflect.DeepEqual(a, m.alert) {
		m.alert, m.alertVer = a, m.alertVer+1
	}
	m.mu.Unlock()
}

// SetBoost changes the boost switches; a running bridge applies it within seconds.
func (m *Matter) SetBoost(b BoostSettings) {
	m.mu.Lock()
	m.boost, m.boostVer = b.withDefaults(), m.boostVer+1
	m.mu.Unlock()
}

func NewMatter(hub *Hub, bin, dir string, port int, listen string) *Matter {
	return &Matter{hub: hub, bin: bin, dir: dir, port: port, localURL: loopbackURL(listen)}
}

// Available reports whether there is a bridge binary to run.
func (m *Matter) Available() bool {
	return m.bin != "" || len(matterBinary) > 0
}

// loopbackURL turns a listen address into one this machine can always reach.
func loopbackURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://127.0.0.1:8080"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// executable returns the bridge binary to run, unpacking the embedded one into
// the state directory (named by its hash, so an upgrade unpacks a fresh copy).
func (m *Matter) executable() (string, error) {
	if m.bin != "" {
		return m.bin, nil
	}
	sum := sha256.Sum256(matterBinary)
	name := "wiser-matter-" + hex.EncodeToString(sum[:6])
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binDir := filepath.Join(m.dir, "bin")
	path := filepath.Join(binDir, name)
	if b, err := os.ReadFile(path); err == nil && sha256.Sum256(b) == sum {
		return path, nil
	}
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return "", err
	}
	old, _ := filepath.Glob(filepath.Join(binDir, "wiser-matter-*"))
	for _, f := range old {
		os.Remove(f)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, matterBinary, 0o700); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

// Run keeps the bridge running until ctx ends.
func (m *Matter) Run(ctx context.Context) {
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := m.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		m.mu.Lock()
		m.running = false
		if err != nil {
			m.lastErr = err.Error()
		}
		m.mu.Unlock()
		if time.Since(started) > 5*time.Minute {
			backoff = 5 * time.Second
		}
		log.Printf("matter: bridge stopped (%v); restarting in %s", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 2*time.Minute)
	}
}

func (m *Matter) runOnce(ctx context.Context) error {
	exe, err := m.executable()
	if err != nil {
		return fmt.Errorf("unpacking the bridge: %w", err)
	}
	cmd := exec.Command(exe, "--state-dir", filepath.Join(m.dir, "state"), "--udp-port", fmt.Sprint(m.port))
	cmd.Env = append(os.Environ(), "WISER_MATTER_VERSION="+version)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	m.mu.Lock()
	m.running, m.lastErr, m.stdin = true, "", stdin
	m.mu.Unlock()
	log.Printf("matter: bridge started (pid %d, UDP port %d)", cmd.Process.Pid, m.port)

	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			if line := strings.TrimRight(sc.Text(), " "); line != "" {
				log.Printf("matter: %s", line)
			}
		}
	}()
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			var st MatterStatus
			if json.Unmarshal(sc.Bytes(), &st) == nil {
				m.mu.Lock()
				m.status = &st
				m.mu.Unlock()
			}
		}
	}()

	// Hand over the dashboard address and secret now and whenever the secret
	// changes (setup page). The bridge uses the same secret as the hub.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	sent, sentBoost, sentAlert := "", -1, -1
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		if _, secret := m.hub.Target(); secret != sent {
			msg, _ := json.Marshal(map[string]string{"url": m.localURL, "secret": secret})
			if m.send(msg) == nil {
				sent = secret
			}
		}
		m.mu.Lock()
		ver, boost := m.boostVer, m.boost
		m.mu.Unlock()
		if ver != sentBoost {
			msg, _ := json.Marshal(map[string]any{"boost": boost})
			if m.send(msg) == nil {
				sentBoost = ver
			}
		}
		m.mu.Lock()
		aVer, alert := m.alertVer, m.alert
		m.mu.Unlock()
		if aVer != sentAlert {
			msg, _ := json.Marshal(map[string]any{"alert": alert})
			if m.send(msg) == nil {
				sentAlert = aVer
			}
		}
		select {
		case err := <-done:
			m.mu.Lock()
			m.stdin = nil
			m.mu.Unlock()
			return err
		case <-ctx.Done():
			m.mu.Lock()
			m.stdin = nil
			m.mu.Unlock()
			// Closing stdin asks the bridge to shut down cleanly.
			stdin.Close()
			select {
			case <-done:
			case <-time.After(8 * time.Second):
				cmd.Process.Kill()
				<-done
			}
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// Reset removes every pairing, so the bridge can be added again from scratch.
func (m *Matter) Reset() error {
	return m.send([]byte(`{"cmd":"reset"}`))
}

// send writes one JSON line to the bridge.
func (m *Matter) send(msg []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stdin == nil {
		return errors.New("the Matter bridge is not running")
	}
	_, err := m.stdin.Write(append(msg, '\n'))
	return err
}

// State is what /api/matter reports.
func (m *Matter) State() map[string]any {
	if m == nil {
		return map[string]any{"available": false}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return map[string]any{
		"available": m.Available(),
		"running":   m.running,
		"error":     m.lastErr,
		"status":    m.status,
		"boost":     m.boost,
		"alert":     m.alert,
	}
}
