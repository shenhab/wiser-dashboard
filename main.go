// Command wiser-dashboard is a self-contained web dashboard for a Drayton /
// Schneider Wiser heating hub, talking to the hub's local REST API.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	listen := flag.String("listen", ":8080", "address the dashboard listens on")
	hubIP := flag.String("hub", "", "hub IP address (skips discovery)")
	secret := flag.String("secret", "", "hub secret")
	secretFile := flag.String("secret-file", "", "read the hub secret from this file")
	configPath := flag.String("config", defaultConfigPath(), "where the hub IP and secret are saved")
	password := flag.String("password", "", "require this password (HTTP basic auth, any username) to open the dashboard")
	noDiscover := flag.Bool("no-discover", false, "do not scan the network for the hub at startup")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Printf("ignoring unreadable config %s: %v", *configPath, err)
	}
	if *hubIP != "" {
		cfg.HubIP = *hubIP
	}
	if *secretFile != "" {
		b, err := os.ReadFile(*secretFile)
		if err != nil {
			log.Fatalf("reading secret file: %v", err)
		}
		cfg.Secret = strings.TrimSpace(string(b))
	}
	if *secret != "" {
		cfg.Secret = *secret
	}
	if *password == "" {
		*password = os.Getenv("WISER_DASHBOARD_PASSWORD")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hub := NewHub()
	store := NewStore()
	poller := NewPoller(hub, store)
	history := NewHistory(filepath.Join(filepath.Dir(*configPath), "history.jsonl"))
	health := NewHealth(filepath.Join(filepath.Dir(*configPath), "health.jsonl"))
	srv := &Server{hub: hub, store: store, poller: poller, history: history, health: health, configPath: *configPath, password: *password, listen: *listen,
		labels: cfg.ChannelNames, alertSettings: cfg.Alerts}

	hub.SetTarget(cfg.HubIP, cfg.Secret)
	go poller.Run(ctx)
	go history.Run(ctx, store)
	go health.Run(ctx, store, hub)
	go bootstrap(ctx, srv, cfg, !*noDiscover)

	httpSrv := &http.Server{Addr: *listen, Handler: srv.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		httpSrv.Shutdown(sctx)
	}()
	printURLs(*listen)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// bootstrap fills in whatever is missing: it finds the hub, then tries to
// fetch its secret. Anything it can't work out is asked for on the setup page.
func bootstrap(ctx context.Context, srv *Server, cfg Config, allowDiscovery bool) {
	if cfg.HubIP != "" && cfg.Secret != "" {
		if info, err := validateHub(cfg.HubIP, cfg.Secret); err == nil {
			log.Printf("connected to %s at %s", info, cfg.HubIP)
			srv.apply(cfg.HubIP, cfg.Secret)
		} else {
			log.Printf("hub %s: %v (fix it on the setup page)", cfg.HubIP, err)
		}
		return
	}
	if cfg.HubIP == "" && allowDiscovery {
		log.Printf("looking for the hub on the local network...")
		cands := srv.runDiscovery(ctx)
		switch {
		case len(cands) == 1 || (len(cands) > 1 && cands[0].Verified && !cands[1].Verified):
			cfg.HubIP = cands[0].IP
			log.Printf("found hub at %s", cfg.HubIP)
		case len(cands) > 1:
			log.Printf("found %d possible hubs; pick one on the setup page", len(cands))
			return
		default:
			log.Printf("no hub found; enter its IP on the setup page")
			return
		}
	}
	if cfg.HubIP == "" {
		return
	}
	if cfg.Secret == "" {
		s, from, err := fetchSecret(cfg.HubIP)
		if err != nil {
			log.Printf("hub at %s, but no secret: enter it on the setup page", cfg.HubIP)
			srv.hub.SetTarget(cfg.HubIP, "")
			return
		}
		log.Printf("fetched the hub secret from %s", from)
		cfg.Secret = s
	}
	srv.apply(cfg.HubIP, cfg.Secret)
}

func printURLs(listen string) {
	if _, _, err := net.SplitHostPort(listen); err != nil {
		log.Printf("listening on %s", listen)
		return
	}
	log.Printf("dashboard: %s", strings.Join(localURLs(listen), "  "))
}

// localURLs lists the addresses the dashboard can be opened at: localhost
// first, then this machine's network addresses (private LAN ranges first).
func localURLs(listen string) []string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil
	}
	if host != "" && host != "0.0.0.0" && host != "::" {
		return []string{"http://" + net.JoinHostPort(host, port)}
	}
	urls := []string{fmt.Sprintf("http://localhost:%s", port)}
	var lan, other []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
			u := fmt.Sprintf("http://%s:%s", ipn.IP, port)
			if ipn.IP.To4()[0] == 192 && ipn.IP.To4()[1] == 168 {
				lan = append(lan, u)
			} else {
				other = append(other, u)
			}
		}
	}
	return append(append(urls, lan...), other...)
}
