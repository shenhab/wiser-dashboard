package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// What each generation of hub answers without the secret.
const (
	v1Denied = `{"Error":"Unauthorized"}`
	v2Denied = `{"Error":"{\"Error\":\"Unauthorized\"}","StatusCode":401}`
)

// hubFake answers like a hub when it is given its secret, and notes every
// request: "tls GET /data/domain/Room +secret" (the + when the secret came).
type hubFake struct {
	secret string
	mu     sync.Mutex
	seen   []string
}

func (f *hubFake) note(where string, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := where + " " + r.Method + " " + r.URL.Path
	if r.Header.Get("SECRET") != "" {
		s += " +secret"
	}
	f.seen = append(f.seen, s)
}

func (f *hubFake) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

// secretsSent counts the noted requests that carried the secret to where.
func (f *hubFake) secretsSent(where string, from int) int {
	n := 0
	for _, s := range f.requests()[from:] {
		if strings.HasPrefix(s, where+" ") && strings.HasSuffix(s, "+secret") {
			n++
		}
	}
	return n
}

func (f *hubFake) answer(w http.ResponseWriter, r *http.Request, denied string) {
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("SECRET") != f.secret {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(denied))
		return
	}
	switch r.URL.Path {
	case "/data/domain/System":
		w.Write([]byte(`{"ActiveSystemVersion":"1.0.0","BrandName":"WiserHub"}`))
	case "/data/domain/Room":
		w.Write([]byte(`[{"id":1,"Name":"Lounge"}]`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// hubKey is a certificate with the key that serves connections with it.
type hubKey struct {
	cert *x509.Certificate
	tls  tls.Certificate
}

// newHubKey makes a certificate like a second-generation hub's: it names no
// host (only 0.0.0.0, unless validFor says otherwise) and lives a hundred
// years from notBefore, which may even be in the future. Signed by issuer
// when given, by itself otherwise.
func newHubKey(t *testing.T, notBefore time.Time, validFor string, issuer *hubKey) *hubKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "WiserHeat000000.local", Organization: []string{"Schneider Electric Controls UK"}},
		NotBefore:    notBefore,
		NotAfter:     notBefore.AddDate(100, 0, 0),
		IPAddresses:  []net.IP{net.IPv4zero},
		// The hub's certificate carries neither; a leaf the system should
		// trust is given the usual limits.
		BasicConstraintsValid: true,
	}
	signer, parent := key, tmpl
	if issuer != nil {
		signer, parent = issuer.tls.PrivateKey.(*ecdsa.PrivateKey), issuer.cert
		tmpl.KeyUsage = x509.KeyUsageDigitalSignature
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	} else {
		tmpl.IsCA = true
	}
	if validFor != "" {
		tmpl.IPAddresses = []net.IP{net.ParseIP(validFor)}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	chain := [][]byte{der}
	if issuer != nil {
		chain = append(chain, issuer.cert.Raw)
	}
	return &hubKey{cert: cert, tls: tls.Certificate{Certificate: chain, PrivateKey: key}}
}

// secureHub stands in for a hub that refuses plain http, as second-generation
// ones do: its plain server only redirects to its TLS one, which answers. The
// certificate it shows can be changed, as it is by a hub replaced or reset.
type secureHub struct {
	fake *hubFake

	plain      *httptest.Server
	secureAddr string
	// redirect, when set, is where the TLS server sends requests on.
	redirect string
	// redirects is whether the plain server redirects; a first-generation
	// hub answers there itself.
	redirects bool
	mu        sync.Mutex
	key       *hubKey
}

func newSecureHub(t *testing.T, key *hubKey) *secureHub {
	h := &secureHub{fake: &hubFake{secret: "hub-secret"}, key: key, redirects: true}
	h.secureAddr = serveTLS(t, h.certificate, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.fake.note("tls", r)
		h.mu.Lock()
		to := h.redirect
		h.mu.Unlock()
		if to != "" {
			http.Redirect(w, r, to+r.URL.RequestURI(), http.StatusFound)
			return
		}
		h.fake.answer(w, r, v2Denied)
	}))
	h.plain = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.fake.note("plain", r)
		h.mu.Lock()
		on := h.redirects
		h.mu.Unlock()
		if !on {
			h.fake.answer(w, r, v1Denied)
			return
		}
		// As the hub does: the same address again, over https.
		http.Redirect(w, r, "https://"+h.secureAddr+r.URL.RequestURI(), http.StatusMovedPermanently)
	}))
	t.Cleanup(h.plain.Close)
	return h
}

// certificate is the one the hub shows now.
func (h *secureHub) certificate() *tls.Certificate {
	h.mu.Lock()
	defer h.mu.Unlock()
	return &h.key.tls
}

// serveTLS runs a handler on a local address over https, showing whichever
// certificate cert returns at the time. It returns the address.
func serveTLS(t *testing.T, cert func() *tls.Certificate, h http.Handler) string {
	t.Helper()
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return cert(), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return l.Addr().String()
}

// show is the hub presenting another certificate, as one replaced or reset does.
func (h *secureHub) show(key *hubKey) {
	h.mu.Lock()
	h.key = key
	h.mu.Unlock()
}

func (h *secureHub) set(redirect string, redirects bool) {
	h.mu.Lock()
	h.redirect, h.redirects = redirect, redirects
	h.mu.Unlock()
}

// address is how the dashboard is given the hub: where plain http would go.
func (h *secureHub) address() string { return hostPort(h.plain.URL) }

func hostPort(u string) string {
	return strings.TrimPrefix(strings.TrimPrefix(u, "http://"), "https://")
}

func sameList(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func checkRequests(t *testing.T, f *hubFake, want ...string) {
	t.Helper()
	if got := f.requests(); !sameList(got, want) {
		t.Errorf("the hub got:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func validate(t *testing.T, tr *hubTrust, mode trustMode, ip, secret string) error {
	t.Helper()
	_, err := validateHub(newHubClient(5*time.Second, tr, mode), ip, secret)
	return err
}

// room is one request through the hub's queue, as the dashboard's poller makes it.
func room(h *Hub) result {
	return h.Do(context.Background(), "GET", "/data/domain/Room", nil, true)
}

// ---------------------------------------------------------------- plain http

func TestPlainHubAsksOnceAndNeedsNoCertificate(t *testing.T) {
	fake := &hubFake{secret: "hub-secret"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.note("plain", r)
		fake.answer(w, r, v1Denied)
	}))
	t.Cleanup(srv.Close)
	tr := &hubTrust{}
	ip := hostPort(srv.URL)

	if err := validate(t, tr, trustNew, ip, "hub-secret"); err != nil {
		t.Fatal(err)
	}
	if tr.pinned() != "" {
		t.Error("a certificate was pinned for a hub that talks plain http")
	}
	// The first request asks how the hub talks, without the secret.
	checkRequests(t, fake, "plain GET /", "plain GET /data/domain/System +secret")

	// From then on the hub is asked directly, by every client that shares the trust.
	if err := validate(t, tr, trustPinned, ip, "hub-secret"); err != nil {
		t.Fatal(err)
	}
	checkRequests(t, fake, "plain GET /", "plain GET /data/domain/System +secret", "plain GET /data/domain/System +secret")
}

// ---------------------------------------------------------------- https

func TestHTTPSHubIsPinnedWhenItProvesTheSecret(t *testing.T) {
	// Like the hub's when it has just been set up: not valid yet.
	key := newHubKey(t, time.Now().Add(10*time.Minute), "", nil)
	h := newSecureHub(t, key)
	tr := &hubTrust{}

	if err := validate(t, tr, trustNew, h.address(), "hub-secret"); err != nil {
		t.Fatal(err)
	}
	if want := certID(key.cert.Raw); tr.pinned() != want {
		t.Errorf("pinned %q, want the hub's certificate %q", tr.pinned(), want)
	}
	// The secret went over https only, and only once the hub had said so.
	checkRequests(t, h.fake, "plain GET /", "tls GET /data/domain/System +secret")

	// A later client (the poller's: it learns nothing) goes straight there.
	if err := validate(t, tr, trustPinned, h.address(), "hub-secret"); err != nil {
		t.Fatal(err)
	}
	checkRequests(t, h.fake, "plain GET /", "tls GET /data/domain/System +secret", "tls GET /data/domain/System +secret")
}

func TestHTTPSHubWhoseCertificateIsntTrustedGetsNoSecret(t *testing.T) {
	h := newSecureHub(t, newHubKey(t, time.Now(), "", nil))
	hub := NewHub()
	hub.SetTarget(h.address(), "hub-secret")

	res := room(hub)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "isn't trusted yet") {
		t.Fatalf("got %v, want the certificate refused until the hub is saved", res.Err)
	}
	if n := h.fake.secretsSent("tls", 0) + h.fake.secretsSent("plain", 0); n != 0 {
		t.Errorf("%d request(s) carried the secret to a hub whose certificate isn't trusted", n)
	}
}

func TestHTTPSHubCertificateChange(t *testing.T) {
	before, after := newHubKey(t, time.Now(), "", nil), newHubKey(t, time.Now(), "", nil)
	h := newSecureHub(t, before)
	tr := &hubTrust{}
	if err := validate(t, tr, trustNew, h.address(), "hub-secret"); err != nil {
		t.Fatal(err)
	}

	// The hub is replaced, or reset: another certificate, the same address.
	h.show(after)
	n := len(h.fake.requests())
	err := validate(t, tr, trustNew, h.address(), "hub-secret")
	if err == nil || !strings.Contains(err.Error(), "certificate has changed") {
		t.Fatalf("got %v, want the new certificate refused", err)
	}
	if h.fake.secretsSent("tls", n) != 0 {
		t.Error("the secret was sent to a hub showing a certificate that isn't trusted")
	}

	// Saving the hub again trusts the new certificate only once its hub
	// proves the secret.
	if err := validate(t, tr, trustReplace, h.address(), "wrong"); err == nil {
		t.Fatal("a wrong secret was accepted")
	}
	if want := certID(before.cert.Raw); tr.pinned() != want {
		t.Errorf("the certificate was replaced on the strength of a wrong secret: %q, want %q", tr.pinned(), want)
	}
	if err := validate(t, tr, trustReplace, h.address(), "hub-secret"); err != nil {
		t.Fatal(err)
	}
	if want := certID(after.cert.Raw); tr.pinned() != want {
		t.Errorf("pinned %q, want the new certificate %q", tr.pinned(), want)
	}
}

func TestHubThatOnlyListensForHTTPS(t *testing.T) {
	h := newSecureHub(t, newHubKey(t, time.Now(), "", nil))
	h.plain.Close() // nothing answers plain http
	tr := &hubTrust{}
	ip := h.secureAddr

	if err := validate(t, tr, trustNew, ip, "hub-secret"); err != nil {
		t.Fatal(err)
	}
	if tr.pinned() == "" {
		t.Error("the hub's certificate wasn't pinned")
	}
	checkRequests(t, h.fake, "tls GET /", "tls GET /data/domain/System +secret")
}

func TestHubThatComesToInsistOnHTTPS(t *testing.T) {
	// A first-generation hub's address that now answers with a second-generation hub.
	key := newHubKey(t, time.Now(), "", nil)
	h := newSecureHub(t, key)
	h.set("", false)
	tr := &hubTrust{}
	if err := validate(t, tr, trustNew, h.address(), "hub-secret"); err != nil {
		t.Fatal(err)
	}
	checkRequests(t, h.fake, "plain GET /", "plain GET /data/domain/System +secret")

	h.set("", true)
	if err := validate(t, tr, trustNew, h.address(), "hub-secret"); err != nil {
		t.Fatal(err)
	}
	// That request was only redirected, so it is sent again over https; and
	// the hub is known to answer there from then on.
	checkRequests(t, h.fake, "plain GET /", "plain GET /data/domain/System +secret",
		"plain GET /data/domain/System +secret", "tls GET /data/domain/System +secret")
	if want := certID(key.cert.Raw); tr.pinned() != want {
		t.Errorf("pinned %q, want %q", tr.pinned(), want)
	}
	if err := validate(t, tr, trustPinned, h.address(), "hub-secret"); err != nil {
		t.Fatal(err)
	}
	checkRequests(t, h.fake, "plain GET /", "plain GET /data/domain/System +secret",
		"plain GET /data/domain/System +secret", "tls GET /data/domain/System +secret", "tls GET /data/domain/System +secret")
}

func TestHubCertificateTrustedByTheSystemNeedsNoPin(t *testing.T) {
	ca := newHubKey(t, time.Now(), "", nil)
	h := newSecureHub(t, newHubKey(t, time.Now(), "127.0.0.1", ca))
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	old := hubRoots
	hubRoots = roots
	defer func() { hubRoots = old }()
	tr := &hubTrust{}

	// The poller's client, which learns nothing, connects without any pin.
	if err := validate(t, tr, trustPinned, h.address(), "hub-secret"); err != nil {
		t.Fatal(err)
	}
	if tr.pinned() != "" {
		t.Error("a certificate the system trusts was pinned")
	}
}

func TestHubRedirectBackToHTTPIsRefused(t *testing.T) {
	h := newSecureHub(t, newHubKey(t, time.Now(), "", nil))
	h.set(h.plain.URL, true)
	tr := &hubTrust{}

	err := validate(t, tr, trustNew, h.address(), "hub-secret")
	if err == nil || !strings.Contains(err.Error(), "https to http") {
		t.Fatalf("got %v, want the redirect refused", err)
	}
	if h.fake.secretsSent("plain", 0) != 0 {
		t.Error("the secret followed a redirect into plain http")
	}
	if tr.pinned() != "" {
		t.Error("a certificate was pinned for a hub that proved nothing")
	}
}

// ---------------------------------------------------------------- finding hubs

func TestLooksLikeWiserFindsBothGenerations(t *testing.T) {
	client := newHubClient(5*time.Second, &hubTrust{}, trustPinned)

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(v1Denied))
	}))
	t.Cleanup(plain.Close)
	if wiser, https := looksLikeWiser(client, hostPort(plain.URL)); !wiser || https {
		t.Errorf("first-generation hub: wiser=%v https=%v, want true false", wiser, https)
	}

	secure := newSecureHub(t, newHubKey(t, time.Now(), "", nil))
	if wiser, https := looksLikeWiser(client, secure.address()); !wiser || !https {
		t.Errorf("second-generation hub: wiser=%v https=%v, want true true", wiser, https)
	}

	// Something else that moves to https isn't a hub, and learns nothing.
	other := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(other.Close)
	moves := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.RequestURI(), http.StatusMovedPermanently)
	}))
	t.Cleanup(moves.Close)
	if wiser, _ := looksLikeWiser(client, hostPort(moves.URL)); wiser {
		t.Error("a device that only redirects to https was taken for a hub")
	}
}

// ---------------------------------------------------------------- attaching

func TestAttachToHubOverHTTPS(t *testing.T) {
	// Not valid yet, like the certificate of a hub just set up.
	before, after := newHubKey(t, time.Now().Add(10*time.Minute), "", nil), newHubKey(t, time.Now(), "", nil)
	h := newSecureHub(t, before)
	path := filepath.Join(t.TempDir(), "config.json")
	dashboard := func() (*httptest.Server, *Hub) {
		t.Helper()
		hub := NewHub()
		s := NewStore()
		srv := &Server{hub: hub, store: s, poller: NewPoller(hub, s), history: NewHistory(""), health: NewHealth(""), listen: "127.0.0.1:0", configPath: path}
		ts := httptest.NewServer(srv.routes())
		t.Cleanup(ts.Close)
		return ts, hub
	}
	ts, _ := dashboard()
	if out := post(t, ts, "/api/setup/save", map[string]string{"ip": h.address(), "secret": "hub-secret"}); out["ok"] != true {
		t.Fatalf("save: %v", out["error"])
	}
	cfg, err := loadConfig(path)
	if err != nil || cfg.HubIP != h.address() || cfg.Secret != "hub-secret" || cfg.HubCert != certID(before.cert.Raw) {
		t.Fatalf("saved: hub %q, certificate %q (%v); want %q and %q", cfg.HubIP, cfg.HubCert, err, h.address(), certID(before.cert.Raw))
	}

	// After a restart, the saved certificate is what lets the dashboard connect.
	ts, hub := dashboard()
	hub.trust.setPin(cfg.HubCert)
	hub.SetTarget(cfg.HubIP, cfg.Secret)
	if res := room(hub); res.Err != nil || res.Status != 200 || !strings.Contains(string(res.Body), "Lounge") {
		t.Fatalf("after a restart: HTTP %d %v %s", res.Status, res.Err, res.Body)
	}

	// A hub replaced or reset at the same address is refused, with nothing
	// sent, until it is saved again.
	h.show(after)
	n := len(h.fake.requests())
	if res := room(hub); res.Err == nil || !strings.Contains(res.Err.Error(), "certificate has changed") {
		t.Fatalf("got HTTP %d %v, want the new certificate refused", res.Status, res.Err)
	}
	if h.fake.secretsSent("tls", n) != 0 {
		t.Error("the secret was sent to the hub's replacement")
	}
	if out := post(t, ts, "/api/setup/save", map[string]string{"ip": h.address(), "secret": "hub-secret"}); out["ok"] != true {
		t.Fatalf("save again: %v", out["error"])
	}
	if cfg, _ = loadConfig(path); cfg.HubCert != certID(after.cert.Raw) {
		t.Errorf("saved certificate %q, want the new one %q", cfg.HubCert, certID(after.cert.Raw))
	}
	if res := room(hub); res.Err != nil || res.Status != 200 {
		t.Fatalf("after saving again: HTTP %d %v", res.Status, res.Err)
	}
}
