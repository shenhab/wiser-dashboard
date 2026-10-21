package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// answering 401 {"Error":"Unauthorized"}. The hub does not advertise itself
// over mDNS. With a secret, candidates are confirmed against /data/domain/System.
func discover(ctx context.Context, secret string) []Candidate {
	hosts := scanTargets()
	client := newHubClient(8 * time.Second) // a busy hub delays connects by ~5s

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
			if !looksLikeWiser(client, ip) {
				return
			}
			c := Candidate{IP: ip}
			if secret != "" {
				if info, err := validateHub(ip, secret); err == nil {
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

func looksLikeWiser(c *http.Client, ip string) bool {
	status, body, err := hubRequest(c, ip, "", "GET", "/", nil)
	return err == nil && status == http.StatusUnauthorized && strings.Contains(string(body), `"Error":"Unauthorized"`)
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

// validateHub checks that ip/secret really reach a Wiser hub.
func validateHub(ip, secret string) (string, error) {
	c := newHubClient(15 * time.Second)
	status, body, err := hubRequest(c, ip, secret, "GET", "/data/domain/System", nil)
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
		return "", errors.New("device answered but does not look like a Wiser hub")
	}
	return strings.TrimSpace(sys.BrandName + " " + sys.ActiveSystemVersion), nil
}

// fetchSecret tries the hub's /secret endpoint. Wiser hubs only serve it in
// setup mode, from their own Wi-Fi access point (192.168.8.1), so the LAN
// address is tried first and the setup AP second.
func fetchSecret(ip string) (secret, from string, err error) {
	c := newHubClient(10 * time.Second)
	tried := []string{}
	for _, host := range uniq(ip, setupAPAddress) {
		if host == "" {
			continue
		}
		tried = append(tried, host)
		status, body, rerr := hubRequest(c, host, "", "GET", "/secret", nil)
		s := strings.Trim(strings.TrimSpace(string(body)), `"`)
		if rerr != nil || status != http.StatusOK || s == "" || strings.Contains(s, "Error") {
			continue
		}
		if ip != "" {
			if _, verr := validateHub(ip, s); verr != nil {
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
