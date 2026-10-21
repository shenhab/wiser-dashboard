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
| `-password` | require a password (basic auth, any username); also `WISER_DASHBOARD_PASSWORD` |
| `-no-discover` | don't scan the network at startup |
| `-config PATH` | where to save hub IP and secret |

History (one sample a minute, kept 14 days) is recorded to `history.jsonl` next to the config file;
the hub itself keeps none. Zone labels set in the dashboard are stored in the config too.

**Anyone who can open the dashboard can control the heating.** Use `-password` if your network isn't trusted.

## How it finds the hub and secret

- **Hub:** scans each local /24 for port 80 answering `401 {"Error":"Unauthorized"}` (the hub doesn't advertise over mDNS), then confirms with the secret.
- **Secret:** tries `GET /secret`. Wiser hubs only serve it in setup mode, from their own Wi-Fi at `192.168.8.1`.
  Otherwise paste it on the setup page.

## Hub quirks it works around

- One connection at a time, ~5 s apart: every request goes through one queue, commands ahead of polling.
- Responses over ~2.7 KB stall (the hub retransmits most TCP segments), so `/data/domain/` is never fetched whole;
  sections are polled individually.

## Build

```sh
go build -o wiser-dashboard .   # this machine
./build.sh                      # macOS, Linux (x86, arm64, Raspberry Pi), Windows into dist/
```
