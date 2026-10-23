# Waste guard

Most wasted heating isn't the schedule: it's a room turned up by hand and forgotten. The waste guard watches every
room and the hot water, whoever changed them (the Wiser app, a thermostat dial, Home Assistant, Google Home, an AI
assistant or this dashboard), and makes heating outside the schedule a deliberate choice instead of an accident.

It is **off by default**. Turn it on under **Advanced → Waste guard**.

## The rules

### What is left alone

- **The schedule.** A room whose target equals the target its schedule last set is on schedule.
- **Comfort mode's early start.** While the hub pre-heats for the next schedule change (the target equals the next
  change's temperature, inside the hub's early-start limit), the room counts as on schedule.
- **Timed changes from any source**: boosts and "hold for N hours". Choosing a duration is deliberate, so they are
  respected however long they are.
- **Away mode, open windows and frost protection.**
- **Targets below the schedule.** Turning a room down saves energy.

### What is limited

A room whose target is **above** its schedule's last trigger, with no end chosen (a dial turned, the Wiser app's
"until next change", "Hey Google, set the lounge to 23", Manual mode):

1. The guard starts watching it as soon as it sees the change.
2. When the room **starts heating** (its target is above its temperature, or it asks for heat), a timer of
   **N minutes** starts (60 by default).
3. When the timer runs out the room goes back to **the target its schedule last set**, even if it isn't heating at that
   moment (otherwise the waste is only postponed). A room without a schedule goes to the **guard off temperature**
   (10° by default).
4. **The room's mode never changes.** In Auto mode the override is cancelled; in Manual mode the manual target is set
   to the schedule's value.

Changing the target again is a new deliberate act and restarts the timer. **Keep for another N min** on the room card
does the same. The hot water follows the same rules with its schedule's on/off state: hot water switched on by hand
with no end goes back to its schedule after N minutes.

The guard pauses while the hub's clock is wrong (schedules would be judged against the wrong time) and while the hub's
data is stale or restored from before a restart.

### Watch only

In **watch only** mode the guard does everything except send commands: the activity log and the room cards say what it
*would* have done. Use it for a few days first to check the rules against your house.

## Alarms

Alarms never change anything; they make waste visible.

- **Schedule too long**: a schedule keeps a room at or above the **"considered on" temperature** (17° by default), or
  the hot water on, for more than **12 hours** in a row (across midnight and from Sunday into Monday). Found from the
  schedule alone, before anything is spent. The settings page previews which schedules a threshold flags.
- **24-hour heating**: the heating ran for more than the **limit** (12 hours by default) in the **last 24 hours**
  (rolling). The alert leads with the figure, for example **14 h 20 min**, and names the rooms that asked for heat
  longest. It clears once the total is half an hour under the limit, so it doesn't flicker. The settings page shows your
  usual daily heating and your busiest day from the recorded history, to choose the limit from.

Every time the guard returns a room it also raises an alert for an hour, saying what it did.

## Where alerts go

- The dashboard's home page, with the 24-hour figure shown large.
- Home Assistant, through the existing `/api/alerts` sensor (see Advanced → Alerts).
- **Matter**: one contact sensor, **Heating alert**, that opens while any waste guard alert is active. Apple Home can
  notify when it opens; Google Home and Alexa can use "opened" to start a routine that notifies or announces. Matter
  can't carry text, so the details are on the dashboard.

## Settings

| Setting | Default | Range |
|---|---|---|
| Waste guard | Off | Off, watch only, on |
| Time limit (N) | 60 min | 15 min – 8 h |
| Guard off temperature (rooms without a schedule) | 10° | 5° – 20° |
| Rooms and hot water covered | All | per room, hot water |
| Apply to changes made from this dashboard, Google/Apple/Alexa and AI assistants | On | on/off |
| "Considered on" temperature (schedule alarm) | 17° | 10° – 25° |
| Schedule alarm length | 12 h | 2 – 24 h |
| 24-hour heating limit | 12 h | 1 – 23 h |

The alarms can also be silenced one by one under Advanced → Alerts, like every other alert.

## How it works

- `guard.go` holds the decision as plain functions, tested as a table in `guard_test.go`:
  - `scheduleAt` works out a schedule's last and next trigger for a moment of the hub's week. A day's first slot
    carries the previous day's last setting until its first change, as the hub does.
  - `decideRoom` / `decideHotWater` turn a room's state, its schedule, the time and the settings into: leave alone,
    respected (with the reason), on schedule, watching, timer running, or return.
  - `longestOn` finds the longest stretch a schedule keeps a room on; `heatingTotals` adds up the last 24 hours.
- `guard_run.go` runs after every poll (about every 20 seconds). It keeps the timers in `guard.json` next to the config,
  so a restart or an update doesn't reset them, and appends what it saw and did to `guard.jsonl` (14 days).
- Returning a room goes through the same high-priority queue as every other command. If the hub still shows the change
  two minutes later the guard tries again, up to three times, then raises an alert.
- What counts as "timed" comes from the hub: timed overrides report `SetpointOrigin: FromBoost` (rooms) or a
  `HotWaterDescription` containing `Boost` (hot water). Untimed ones report `FromManualOverride`, Manual mode
  `FromManualMode`. The activity log records the hub's reason for every change it sees, so this can be checked on a
  real hub in watch only mode.
- The dashboard's own changes are told apart by where they came in: the dashboard's page, the MCP server, or the Matter
  bridge (which marks its requests). Changes through the hub-compatible API from anything else (Home Assistant) are
  external.

## API

- `GET /api/guard`: settings, each room's and the hot water's guard status, the alarms, the 24-hour totals, the
  recorded daily heating (for choosing the limit), and the activity log. `?onTemp=170&scheduleHours=12` previews the
  schedule alarm for other values.
- `POST /api/guard/settings`: save the settings.
- `POST /api/guard/keep` `{"key": "room-2"}` or `{"key": "hotwater-2"}`: restart that timer.
