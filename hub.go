package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The Wiser hub accepts one TCP connection at a time and makes the next
// connect wait ~5s, so every request goes through a single worker.
// Writes (user commands) jump ahead of background polling.

type result struct {
	Status int
	Body   []byte
	Err    error
	Dur    time.Duration
}

type job struct {
	method string
	path   string
	body   []byte
	done   chan result
}

type Hub struct {
	mu     sync.RWMutex
	ip     string
	secret string
	gen    atomic.Int64 // bumps whenever the target changes

	client *hubClient
	trust  *hubTrust
	high   chan *job
	low    chan *job
	queued atomic.Int32
	busy   atomic.Bool
}

func NewHub() *Hub {
	h := &Hub{
		trust: &hubTrust{},
		high:  make(chan *job, 64),
		low:   make(chan *job, 64),
	}
	h.client = newHubClient(20*time.Second, h.trust, trustPinned)
	go h.worker()
	return h
}

func (h *Hub) SetTarget(ip, secret string) {
	h.mu.Lock()
	h.ip, h.secret = ip, secret
	h.mu.Unlock()
	h.gen.Add(1)
}

func (h *Hub) Target() (ip, secret string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.ip, h.secret
}

func (h *Hub) Ready() bool {
	ip, secret := h.Target()
	return ip != "" && secret != ""
}

// Do queues a request and waits for it. high=true for user commands.
func (h *Hub) Do(ctx context.Context, method, path string, body []byte, high bool) result {
	j := &job{method: method, path: path, body: body, done: make(chan result, 1)}
	ch := h.low
	if high {
		ch = h.high
	}
	h.queued.Add(1)
	select {
	case ch <- j:
	case <-ctx.Done():
		h.queued.Add(-1)
		return result{Err: ctx.Err()}
	}
	select {
	case r := <-j.done:
		return r
	case <-ctx.Done():
		return result{Err: ctx.Err()}
	}
}

func (h *Hub) worker() {
	for {
		var j *job
		select {
		case j = <-h.high:
		default:
			select {
			case j = <-h.high:
			case j = <-h.low:
			}
		}
		h.busy.Store(true)
		j.done <- h.exec(j)
		h.busy.Store(false)
		h.queued.Add(-1)
	}
}

func (h *Hub) exec(j *job) result {
	ip, secret := h.Target()
	if ip == "" {
		return result{Err: errors.New("hub IP not configured")}
	}
	start := time.Now()
	status, body, err := h.client.request(ip, secret, j.method, j.path, j.body)
	return result{Status: status, Body: body, Err: err, Dur: time.Since(start)}
}

// ---------------------------------------------------------------- https

// A first-generation hub talks plain http. Second-generation hubs refuse it
// and redirect to https, with a certificate they sign themselves. Nothing can
// check that certificate the usual way: it names no host (its only
// alternative name is 0.0.0.0). So the dashboard pins it instead. Once the
// hub presenting it has proved it knows the secret, the certificate is saved
// and every later connection must show the same one. As with ssh, the first
// connection is taken on trust.

// trustMode says what becomes of a certificate that is neither the pinned one
// nor one the system trusts.
type trustMode int

const (
	trustPinned  trustMode = iota // refused: the poller and discovery, which learn nothing
	trustNew                      // pinned once the hub proves it knows the secret, if none is pinned yet
	trustReplace                  // pinned in place of the pinned one: the setup page, for a replaced or reset hub
)

type hubTrust struct {
	mu       sync.Mutex
	where    map[string]string // hub address -> where it answers: itself over http, or its https address
	trusted  string            // SHA-256 of the hub's certificate, as hex; "" until one is trusted
	onChange func()            // the pin changed; called without the lock held
}

// base is where a hub's requests go: its address over http, or the https
// address it redirected to. ok is false until the hub has been asked how it
// talks.
func (t *hubTrust) base(ip string) (base string, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	base, ok = t.where[ip]
	return base, ok
}

// remember notes where a hub answers.
func (t *hubTrust) remember(ip, base string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.where == nil {
		t.where = map[string]string{}
	}
	t.where[ip] = base
}

// forget makes the next request ask the hub again how it talks.
func (t *hubTrust) forget(ip string) {
	t.mu.Lock()
	delete(t.where, ip)
	t.mu.Unlock()
}

func (t *hubTrust) pinned() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.trusted
}

// setPin is the certificate saved with the hub, read at startup.
func (t *hubTrust) setPin(sum string) {
	t.mu.Lock()
	t.trusted = sum
	t.mu.Unlock()
}

// pin trusts a certificate from here on. An empty sum (the hub spoke plain
// http, or showed a certificate the system trusts) leaves the pin alone.
func (t *hubTrust) pin(sum string) {
	if sum == "" {
		return
	}
	t.mu.Lock()
	changed := t.trusted != sum
	t.trusted = sum
	f := t.onChange
	t.mu.Unlock()
	if changed && f != nil {
		f()
	}
}

// hubClient talks to hubs. The first request to an address finds out, without
// the secret, whether it answers plain http or only https; the answer is kept
// with the trust, which every client of the hub shares.
type hubClient struct {
	http  *http.Client // carries the secret, so it checks the hub's certificate
	probe *http.Client // asks how a hub talks: no secret, no certificate check
	trust *hubTrust
	mode  trustMode

	mu   sync.Mutex
	seen string // the certificate a learning connection accepted, until the hub proves itself
}

// hubRoots are the authorities trusted for hubs' certificates; nil means the
// system's own (tests substitute theirs).
var hubRoots *x509.CertPool

func newHubClient(timeout time.Duration, t *hubTrust, mode trustMode) *hubClient {
	c := &hubClient{trust: t, mode: mode}
	// VerifyConnection does the checking, so the usual one is switched off.
	secure := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}
	secure.VerifyConnection = c.check
	c.http = &http.Client{
		Timeout:       timeout,
		CheckRedirect: c.checkRedirect,
		Transport: &http.Transport{
			// The hub sends Connection: close anyway; reusing sockets only confuses it.
			DisableKeepAlives:     true,
			DialContext:           (&net.Dialer{Timeout: 12 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 12 * time.Second,
			TLSClientConfig:       secure,
		},
	}
	c.probe = &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			DisableKeepAlives:     true,
			DialContext:           (&net.Dialer{Timeout: 12 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 12 * time.Second,
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		},
	}
	return c
}

// httpsRedirect is how the redirect policy hands an http -> https redirect
// back: the caller sends the request again over https.
type httpsRedirect struct{ base string }

func (e *httpsRedirect) Error() string { return "redirected to " + e.base }

// checkRedirect leaves http -> https redirects to the caller and refuses to
// follow one from https back to http, which would send the secret in clear.
func (c *hubClient) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	from, to := via[len(via)-1].URL.Scheme, req.URL.Scheme
	switch {
	case from == "http" && to == "https":
		return &httpsRedirect{base: "https://" + req.URL.Host}
	case from == "https" && to == "http":
		return errors.New("refused to follow a redirect from https to http")
	}
	return nil
}

// check is the certificate check of every connection that carries the secret.
// A certificate the system trusts passes. Any other must be the pinned one,
// unless this connection may learn.
func (c *hubClient) check(cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("the hub presented no certificate")
	}
	leaf := cs.PeerCertificates[0]
	pool := x509.NewCertPool()
	for _, ic := range cs.PeerCertificates[1:] {
		pool.AddCert(ic)
	}
	opts := x509.VerifyOptions{DNSName: cs.ServerName, Intermediates: pool, Roots: hubRoots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if _, err := leaf.Verify(opts); err == nil {
		return nil
	}
	id := certID(leaf.Raw)
	pinned := c.trust.pinned()
	switch {
	case pinned == id:
		return nil
	case c.mode == trustReplace || (c.mode == trustNew && pinned == ""):
		// Pinned by validateHub, once the hub has proved it knows the secret.
		c.mu.Lock()
		c.seen = id
		c.mu.Unlock()
		return nil
	case pinned == "":
		return errors.New("the hub talks https with a certificate of its own, which isn't trusted yet: save the hub on the setup page to trust it")
	}
	return fmt.Errorf("the hub's certificate has changed (now %s): it isn't the one trusted for this hub. "+
		"If the hub was replaced or reset, save it again on the setup page to trust the new certificate", shortID(id))
}

// takeSeen is the certificate this client's connections accepted without
// trusting, if any: what the hub showed while proving it knows the secret.
func (c *hubClient) takeSeen() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.seen
	c.seen = ""
	return id
}

// request performs one request to a hub. On a read error the partial body is
// still returned, which matters for the first-generation hub: big responses
// stall part-way through.
func (c *hubClient) request(ip, secret, method, path string, body []byte) (int, []byte, error) {
	base, ok := c.trust.base(ip)
	if !ok {
		var err error
		if base, err = c.how(ip); err != nil {
			return 0, nil, err
		}
		c.trust.remember(ip, base)
	}
	status, out, err, https := c.send(base, secret, method, path, body)
	if https != "" {
		// The hub has come to insist on https. It only redirected, so nothing was done.
		c.trust.remember(ip, https)
		status, out, err, _ = c.send(https, secret, method, path, body)
	}
	return status, out, err
}

// send performs the request at base, an http or https address. https is set
// when the hub redirected it to one, in which case the request was not made.
func (c *hubClient) send(base, secret, method, path string, body []byte) (int, []byte, error, string) {
	var rd io.Reader
	if len(body) > 0 {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		return 0, nil, err, ""
	}
	if secret != "" {
		req.Header.Set("SECRET", secret)
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if res == nil {
		return 0, nil, err, ""
	}
	defer res.Body.Close()
	var to *httpsRedirect
	if errors.As(err, &to) {
		return res.StatusCode, nil, nil, to.base
	}
	if err != nil {
		return 0, nil, err, ""
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	return res.StatusCode, b, err, ""
}

// how finds where a hub answers, without sending it the secret: the https
// address it redirects plain http to, or its own address if it answers that.
// A hub that only talks https may not listen for plain http at all, so when
// nothing answers there, or only to refuse plain http, https is tried too.
func (c *hubClient) how(ip string) (string, error) {
	status, loc, body, err := c.peek("http://" + ip + "/")
	if err == nil {
		if base := httpsBase(status, loc); base != "" {
			return base, nil
		}
		if !refusesPlain(status, body) {
			return "http://" + ip, nil
		}
		err = fmt.Errorf("plain http refused with HTTP %d", status)
	}
	if _, _, _, terr := c.peek("https://" + ip + "/"); terr == nil {
		return "https://" + ip, nil
	}
	return "", err
}

// refusesPlain says whether an answer to plain http is a TLS server turning
// it away, as one does when its port is asked for plain http.
func refusesPlain(status int, body []byte) bool {
	return status == http.StatusBadRequest && bytes.Contains(body, []byte("HTTPS"))
}

// peek asks without the secret and follows no redirect: the answer's status,
// Location and body.
func (c *hubClient) peek(u string) (int, string, []byte, error) {
	res, err := c.probe.Get(u)
	if err != nil {
		return 0, "", nil, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
	return res.StatusCode, res.Header.Get("Location"), body, nil
}

// httpsBase is the https address a redirect points to, or "" if it isn't one
// from http to https.
func httpsBase(status int, location string) string {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return ""
	}
	u, err := url.Parse(location)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return ""
	}
	return "https://" + u.Host
}

// certID identifies a certificate: the SHA-256 of it, as hex.
func certID(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// shortID is enough of a certificate's ID to recognise it by eye.
func shortID(id string) string {
	return strings.ToUpper(id[:min(16, len(id))])
}
