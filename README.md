# wiser-dashboard

A single-binary web dashboard for a Wiser heating hub, using the hub's local REST API. No cloud, no dependencies.

## Run

```sh
./wiser-dashboard                       # finds the hub, then asks for anything missing in the browser
./wiser-dashboard -hub 192.168.1.50 -secret-file ~/wiser_secret.txt
```

Open `http://<this-machine>:8080`. Settings are saved to the path printed as `-config`
(default: your OS config dir, `wiser-dashboard/config.json`, mode 0600).

| Flag | Meaning |
|---|---|
| `-listen :8080` | address to serve on |
| `-hub IP` | hub address (skips discovery) |
| `-secret` / `-secret-file` | hub secret |
| `-password` | require a password for the dashboard (basic auth, any username); also `WISER_DASHBOARD_PASSWORD` |
| `-no-discover` | don't scan the network at startup |
| `-config PATH` | where to save hub IP and secret |

History (one sample a minute, kept 14 days) is recorded to `history.jsonl` next to the config file;
the hub itself keeps none. From it the dashboard works out boiler starts and run lengths, and how fast
each room warms up (for the "ready ≈" times). `health.jsonl` logs the hub's own restarts, Wi-Fi drops and
outages. Zone labels and alert settings set in the dashboard are stored in the config too.

`GET /api/alerts` returns the current alerts (`count` of warnings and critical ones, a `summary` line and
the full list); the Alerts page has a ready-made Home Assistant REST sensor and notification automation.

**Anyone who can open the dashboard can control the heating.** Use `-password` if your network isn't trusted.

## Run as a service (Linux)

`deploy/wiser-dashboard.service` (also in the Linux release archives) runs it under systemd as its own
locked-down user, starts it at boot and restarts it if it stops. From the unpacked archive:

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin wiser-dashboard
sudo install -m 755 wiser-dashboard /usr/local/bin/
sudo install -m 640 -g wiser-dashboard ~/wiser_secret.txt /etc/wiser-dashboard.secret
sudo install -m 644 wiser-dashboard.service /etc/systemd/system/
sudo systemctl enable --now wiser-dashboard
sudo ./open-firewall.sh                 # only if the machine has a firewall, see below
journalctl -u wiser-dashboard -f        # its log
```

It finds the hub by itself; add `-hub IP` to `ExecStart` (then `sudo systemctl daemon-reload`) to skip
the scan. Config, history and health logs are kept in `/var/lib/wiser-dashboard`. To keep history from an
earlier install, stop the service, copy `history.jsonl` and `health.jsonl` there, run
`sudo chown wiser-dashboard: /var/lib/wiser-dashboard/*`, and start it again. A dashboard password goes in
`/etc/wiser-dashboard.env` as `WISER_DASHBOARD_PASSWORD=...`.

To upgrade, install the new binary over `/usr/local/bin/wiser-dashboard` and `sudo systemctl restart wiser-dashboard`.

## Google Home, Apple Home and Alexa (Matter)

The dashboard has a built-in Matter bridge, so the heating shows up in Google Home, Apple Home, Alexa and
SmartThings without Home Assistant: each room as a thermostat (temperature, target, heat/off), the hot
water as a switch (on = 30-minute boost, off = off). Voice control, routines and Gemini work with it, and so
does control away from home through your Google Nest speaker or display, Apple home hub or Echo.

Open **Advanced → Google Home** and scan the QR code in your app (Google Home: **+ → Device → New device →
Matter-enabled device**). Your phone and the app's hub (e.g. a Nest speaker) must be on the same network as
the dashboard. Pairing is kept across restarts; the same page lists paired apps and can remove them.

- Included in the release downloads for every platform except 32-bit Raspberry Pi OS (armv7); use the
  64-bit OS there.
- It uses UDP port 5540 and mDNS (UDP 5353), besides the dashboard's TCP 8080. `-matter-port` changes
  the port, `-matter=false` turns the bridge off. See [Firewall](#firewall).
- It reads and controls the hub through the dashboard's own `/data/` API with the hub secret, like Home
  Assistant does, so it adds no load on the hub.
- The bridge is a separate program (`matter/`, TypeScript on [matter.js](https://github.com/project-chip/matter.js),
  compiled with Bun) packed inside the dashboard, which unpacks it next to its config and supervises it.
  That's why release downloads are 30–40 MB.
- It isn't Matter-certified, so apps warn about an uncertified device when pairing; that's expected.

## Firewall

The dashboard needs TCP 8080 (the dashboard and Home Assistant), UDP 5540 (Matter) and UDP 5353 (mDNS,
how Google Home and the others find the bridge), reachable from your home network.

- **Linux:** `sudo ./open-firewall.sh` (in the Linux download, and in `deploy/`). It works out your local
  network itself, finds which firewall is in use (ufw, firewalld or nftables) and opens those ports to the
  local network only, never to the internet. `--dry-run` shows what it would do; pass a network
  (`sudo ./open-firewall.sh 192.168.1.0/24`) to choose it yourself. Running it again changes nothing, and
  with no firewall active it does nothing.
- **Windows:** on first start Windows asks whether to allow it on networks: allow **Private networks**.
  If you missed it, Windows Security → Firewall & network protection → Allow an app through firewall.
- **macOS:** allow the incoming-connections prompt if the firewall is on, and **Local Network** access
  when asked (System Settings → Privacy & Security → Local Network); Matter can't find or answer
  controllers without it.

## Home Assistant

The Wiser integration reads the hub's whole state in one request (`/data/v2/domain/`), which hubs with a bad
Wi-Fi link can't deliver. The dashboard serves the same API under `/data/` on its own port:

- full-state reads (domain, network, schedules, status, OpenTherm) come from the dashboard's cache, instantly;
- everything else, including all commands, is forwarded to the hub ahead of polling, and the cache updates
  from the hub's reply so the next read shows the change.

It accepts the hub's own secret, always the one the dashboard is using, and nothing else.
In Home Assistant, add the Wiser integration manually: **Host** = this machine's IP, **Port** = `8080`
(or your `-listen` port), **Secret** = the hub secret. Ignore the hub if HA discovers it directly.
The dashboard password (`-password`) doesn't apply to `/data/`.

## How it finds the hub and secret

- **Hub:** scans each local /24 for port 80 answering `401 {"Error":"Unauthorized"}` (the hub doesn't advertise over mDNS), then confirms with the secret.
- **Secret:** tries `GET /secret`. Wiser hubs only serve it in setup mode, from their own Wi-Fi at `192.168.8.1`.
  Otherwise paste it on the setup page.

## Hub quirks it works around

- One connection at a time, ~5 s apart: every request goes through one queue, commands ahead of polling.
- Responses over ~2.7 KB stall (the hub retransmits most TCP segments), so `/data/domain/` is never fetched whole;
  sections are polled individually.

## Download

Ready-made builds for Linux (x86-64, arm64, Raspberry Pi armv7), macOS (Intel, Apple silicon) and
Windows (x86-64, arm64) are on the [Releases](../../releases) page. Pick the one matching `uname -m`
(`x86_64` → amd64, `aarch64` → arm64, `armv7l` → armv7), unpack it and run `./wiser-dashboard`.
`./wiser-dashboard -version` shows which build you have.

## Build

One script builds it on Windows, macOS and Linux alike. It needs only [Go](https://go.dev/dl/); with
[Node.js](https://nodejs.org) installed it also builds and embeds the Matter bridge (it installs the
bridge's dependencies itself).

```sh
go run ./tools/build                       # this machine -> ./wiser-dashboard (.exe on Windows)
go run ./tools/build -all                  # every platform -> dist/
go run ./tools/build -target linux/arm64   # chosen platforms (comma-separated) -> dist/
go run ./tools/build -all -package         # release archives (.tar.gz / .zip) -> dist/
go run ./tools/build -matter=off           # without the Matter bridge (-matter=on: fail without Node.js)
```

`./build.sh` (macOS, Linux) and `build.cmd` (Windows) are shortcuts for the same, e.g. `./build.sh -all`.
Plain `go build -o wiser-dashboard .` also works and gives a build without the Matter bridge.

The bridge's tests pair a real Matter controller with it against a mock hub. From `matter/`, after a
build: `node test/e2e.mjs dist/wiser-matter-<os>-<arch>` (the bridge alone) and
`node test/system.mjs ../wiser-dashboard` (the whole app).

GitHub Actions (`.github/workflows/ci.yml`) checks every push and pull request on Linux, macOS and
Windows (gofmt, vet, tests, a native build) and pairs a Matter controller with the bridge on each, then
builds and packages every platform with the same build script; the archives are kept as workflow
artifacts. To publish a release, push a version tag:

```sh
git tag v1.0.0 && git push origin v1.0.0   # a tag with a dash, like v1.1.0-rc1, is marked pre-release
```
