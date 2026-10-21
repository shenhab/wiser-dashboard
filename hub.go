package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
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

	client *http.Client
	high   chan *job
	low    chan *job
	queued atomic.Int32
	busy   atomic.Bool
}

func NewHub() *Hub {
	h := &Hub{
		client: newHubClient(20 * time.Second),
		high:   make(chan *job, 64),
		low:    make(chan *job, 64),
	}
	go h.worker()
	return h
}

func newHubClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			// The hub sends Connection: close anyway; reusing sockets only confuses it.
			DisableKeepAlives:     true,
			DialContext:           (&net.Dialer{Timeout: 12 * time.Second}).DialContext,
			ResponseHeaderTimeout: 12 * time.Second,
		},
	}
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
	status, body, err := hubRequest(h.client, ip, secret, j.method, j.path, j.body)
	return result{Status: status, Body: body, Err: err, Dur: time.Since(start)}
}

// hubRequest performs one request. On a read error the partial body is still
// returned, which matters for this hub: big responses stall part-way through.
func hubRequest(c *http.Client, ip, secret, method, path string, body []byte) (int, []byte, error) {
	var rd io.Reader
	if len(body) > 0 {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, "http://"+ip+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if secret != "" {
		req.Header.Set("SECRET", secret)
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, b, err
}
