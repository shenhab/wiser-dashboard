# Changelog

What changed in each release of Wiser Dashboard. The newest is at the top. Versions follow
[semantic versioning](https://semver.org): a new feature raises the middle number, a fix the last one.

Every pull request that changes what users see adds a line under **Unreleased**; releasing moves those lines under the
new version's heading.

## Unreleased

### Added
- **Waste guard** (off by default, Advanced → Waste guard). A room turned up by hand above its schedule, with no end
  chosen, may heat for 60 minutes (yours to set), then goes back to what its schedule last set, whoever turned it up:
  the Wiser app, a thermostat, Home Assistant, Google Home, an AI assistant or the dashboard. The schedule, comfort
  mode's early start, boosts and other timed changes, away mode, open windows and anything turned down are left alone.
  Rooms without a schedule go to a temperature you choose. Hot water switched on by hand works the same way.
  - **Watch only** mode logs what it would do, to try the rules first; an activity log shows what it saw and did.
  - **Keep for another hour** on the room card restarts the timer.
  - Alarms for a schedule that stays on longer than you allow and for too much heating in 24 hours, the figure shown
    large on the home page, plus a 14-day trend of the heating time in any 24 hours.
  - Alerts reach the dashboard, Home Assistant, and Google Home, Apple Home and Alexa through a **Heating alert**
    contact sensor (can be hidden), optional devices named with each alert that Google Home announces as they appear,
    and an optional **Heating hours (last 24h)** sensor that reads the hours as degrees.
  - AI assistants can see what the guard is doing.
  - The rules are in [docs/waste-guard.md](docs/waste-guard.md).
- After a restart or an update, the dashboard starts with the last data it had instead of an empty page while the hub
  answers.

### Changed
- The Matter bridge goes online only once the waste guard's devices are in place, so Google Home can reach them after
  a restart like the rooms.

## 1.6.0 (2026-10-22)

### Added
- **Add a device** wizard (beta): pairing mode, the new device found, made to blink, and attached to a room (a new one
  if needed), with the hub's acceptance checked. Identify now switches itself off after a while. (#11)

## 1.5.3 (2026-10-22)

### Fixed
- The Google Home page shows "Paired" whenever a controller holds a pairing. (#10)

## 1.5.2 (2026-10-22)

### Fixed
- The copy buttons work when the dashboard is opened over plain HTTP. (#9)

## 1.5.1 (2026-10-22)

### Fixed
- The MCP server reports away mode from the hub's own flag. (#8)

## 1.5.0 (2026-10-22)

### Added
- Releases are signed, and the updater refuses anything unsigned. (#7)

## 1.4.0 (2026-10-22)

### Added
- The dashboard shows its version and new releases, and updates with one click, or by itself if you opt in. (#6)

## 1.3.2 (2026-10-22)

### Added
- One-line installers for Linux, macOS and Windows. (#5)

### Fixed
- Matter switches act on the controller's commands, and hub readings older than a command are ignored. (#4)

## 1.3.1 (2026-10-22)

### Fixed
- Durations no longer show as "1h 60m". (#3)

### Docs
- The README is now a product overview, with screenshots, and there's a CONTRIBUTING guide and the MIT licence.
  (#1, #2)

## 1.3.0 (2026-10-22)

### Added
- An MCP server, so Claude and other AI assistants can read and control the heating.

## 1.2.0 (2026-10-22)

### Added
- Per-room heating boost switches in Google Home, Apple Home and Alexa.

## 1.1.1 (2026-10-22)

### Added
- `open-firewall.sh` opens the ports the dashboard needs to the local network.
- A cross-platform build script: `go run ./tools/build`.

### Fixed
- The systemd unit allows setrlimit.
- The build reinstalls the Matter bridge's dependencies when Bun can't run.

### Build
- CI publishes only the release archives.

## 1.1.0 (2026-10-22)

### Added
- A built-in Matter bridge for Google Home, Apple Home and Alexa.

### Build
- Matter tests on macOS CI runners.

## 1.0.2 (2026-10-22)

### Added
- A hardened systemd unit for Linux.

## 1.0.1 (2026-10-22)

### Build
- CI checks with the current Go and uses the Node 24 versions of the actions.

## 1.0.0 (2026-10-22)

### Added
- The Wiser heating dashboard: rooms, hot water, schedules and quick actions, talking to the hub locally.
- Hub settings, alerts, boiler cycles, warm-up times and hub health.
- A hub-compatible API for Home Assistant, answered from the dashboard's cache.
- CI, and release builds for Linux, macOS and Windows.
