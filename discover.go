package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type Candidate struct {
	IP       string `json:"ip"`
	Verified bool   `json:"verified"`
	Info     string `json:"info,omitempty"`
}

// The hub's setup-mode access point always uses this address.
const setupAPAddress = "192.168.8.1"

// discover scans every local IPv4 /24 for the Wiser fingerprint: port 80
// answering 401 {"Error":"Unauthorized"}, or redirecting to https and
// answering the same there (second-generation hubs). The hub does not
// advertise itself over mDNS. With a secret, candidates are confirmed against
// /data/domain/System.
func discover(ctx context.Context, secret string, t *hubTrust) []Candidate {
	hosts := scanTargets()
	client := newHubClient(8*time.Second, t, trustPinned) // a busy hub delays connects by ~5s

	var (
		mu    sync.Mutex
		found []Candidate
		wg    sync.WaitGroup
		sem   = make(chan struct{}, 128)
	)
	for _, ip := range hosts {
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			wiser, https := looksLikeWiser(client, ip)
			if !wiser {
				return
			}
			c := Candidate{IP: ip}
			if https {
				c.Info = "Wiser hub over https. Connect to trust its certificate."
			}
			if secret != "" {
				if info, err := validateHub(client, ip, secret); err == nil {
					c.Verified, c.Info = true, info
				}
			}
			mu.Lock()
			found = append(found, c)
			mu.Unlock()
		}(ip)
	}
	wg.Wait()
	sort.Slice(found, func(i, j int) bool {
		if found[i].Verified != found[j].Verified {
			return found[i].Verified
		}
		return found[i].IP < found[j].IP
	})
	return found
}

// looksLikeWiser recognises a Wiser hub by what it answers without a secret.
// Both generations refuse /data/domain/System alike; the second takes only
// https, redirecting plain http there.
func looksLikeWiser(c *hubClient, ip string) (wiser, https bool) {
	status, loc, body, err := c.peek("http://" + ip + "/")
	if err != nil {
		return false, false
	}
	if status == http.StatusUnauthorized && strings.Contains(string(body), `"Error":"Unauthorized"`) {
		return true, false
	}
	base := httpsBase(status, loc)
	if base == "" {
		return false, false
	}
	status, _, body, err = c.peek(base + "/data/domain/System")
	if err != nil || status != http.StatusUnauthorized {
		return false, false
	}
	return strings.Contains(string(body), "Unauthorized"), true
}

func scanTargets() []string {
	seen := map[string]bool{}
	var out []string
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil || ip4.IsLinkLocalUnicast() {
				continue
			}
			// Cap at the /24 around our address to keep the scan quick.
			for i := 1; i < 255; i++ {
				h := fmt.Sprintf("%d.%d.%d.%d", ip4[0], ip4[1], ip4[2], i)
				if h != ip4.String() && !seen[h] {
					seen[h] = true
					out = append(out, h)
				}
			}
		}
	}
	return out
}

// validateHub checks that ip/secret really reach a Wiser hub. Over https with
// a certificate of the hub's own, that proof is also what gets the certificate
// pinned, on a client that may learn one.
func validateHub(c *hubClient, ip, secret string) (string, error) {
	status, body, err := c.request(ip, secret, "GET", "/data/domain/System", nil)
	if err != nil {
		return "", err
	}
	if status == http.StatusUnauthorized {
		return "", errors.New("hub rejected the secret (401)")
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("unexpected HTTP %d", status)
	}
	var sys struct {
		BrandName            string
		ActiveSystemVersion  string
		CloudConnectionState string `json:"CloudConnectionStatus"`
	}
	if err := json.Unmarshal(body, &sys); err != nil || sys.ActiveSystemVersion == "" {
		return "", fmt.Errorf("device answered but does not look like a Wiser hub (HTTP %d: %s)", status, excerpt(body))
	}
	if c.mode != trustPinned {
		if id := c.takeSeen(); id != "" {
			before := c.trust.pinned()
			c.trust.pin(id)
			switch before {
			case "":
				log.Printf("hub %s talks https: trusting its certificate (%s)", ip, shortID(id))
			case id:
			default:
				log.Printf("hub %s: its certificate changed from %s to %s; trusting the new one", ip, shortID(before), shortID(id))
			}
		}
	}
	return strings.TrimSpace(sys.BrandName + " " + sys.ActiveSystemVersion), nil
}

// excerpt is the start of a reply, on one line, for an error message.
func excerpt(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	switch {
	case s == "":
		return "an empty reply"
	case len(s) > 120:
		return s[:120] + "…"
	}
	return s
}

// fetchSecret tries the hub's /secret endpoint. Wiser hubs only serve it in
// setup mode, from their own Wi-Fi access point (192.168.8.1), so the LAN
// address is tried first and the setup AP second.
func fetchSecret(c *hubClient, ip string) (secret, from string, err error) {
	tried := []string{}
	for _, host := range uniq(ip, setupAPAddress) {
		if host == "" {
			continue
		}
		tried = append(tried, host)
		status, body, rerr := c.request(host, "", "GET", "/secret", nil)
		s := strings.Trim(strings.TrimSpace(string(body)), `"`)
		if rerr != nil || status != http.StatusOK || s == "" || strings.Contains(s, "Error") {
			continue
		}
		if ip != "" {
			if _, verr := validateHub(c, ip, s); verr != nil {
				continue
			}
		}
		return s, host, nil
	}
	return "", "", fmt.Errorf("the hub did not hand out its secret (tried %s). It only does so in setup mode: "+
		"press the setup button on the hub until the LED flashes, join its WiserHeatXXXXXX Wi-Fi, then try again",
		strings.Join(tried, ", "))
}

func uniq(xs ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
