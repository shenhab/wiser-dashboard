// wiser-matter is the Matter bridge that wiser-dashboard runs alongside itself.
// It shows each Wiser room as a thermostat and the hot water as a plug to any
// Matter controller (Google Home, Apple Home, Alexa, SmartThings), and the
// waste guard's alert as a contact sensor.
//
// It talks to the dashboard's hub-compatible API (/data/...) with the hub
// secret, exactly as Home Assistant does. The dashboard sends its address and
// the secret as JSON lines on stdin; this program reports its pairing state as
// JSON lines on stdout. Matter's own log goes to stderr.

import { Endpoint, Environment, Logger, LogLevel, ServerNode, VendorId } from "@matter/main";
import { AggregatorEndpoint } from "@matter/main/endpoints/aggregator";
import { BridgedDeviceBasicInformationServer } from "@matter/main/behaviors/bridged-device-basic-information";
import { ThermostatServer } from "@matter/main/behaviors/thermostat";
import { Thermostat } from "@matter/main/clusters/thermostat";
import { ContactSensorDevice } from "@matter/main/devices/contact-sensor";
import { OnOffPlugInUnitDevice, OnOffPlugInUnitRequirements } from "@matter/main/devices/on-off-plug-in-unit";
import { TemperatureSensorDevice } from "@matter/main/devices/temperature-sensor";
import { ThermostatDevice } from "@matter/main/devices/thermostat";
import { QrCode } from "@matter/main/types";
import { SessionManager } from "@matter/protocol";
import { randomInt } from "node:crypto";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { createInterface } from "node:readline";

const VERSION = process.env.WISER_MATTER_VERSION ?? "dev";
const POLL_MS = 5_000;
const UNREACHABLE_AFTER_MS = 120_000;
const HOLD_AFTER_WRITE_MS = 20_000; // ignore stale polls right after a command
const OFF = -200; // the hub's "off" setpoint, in tenths of a degree
const HOT_WATER_ON = 1100;
const HOT_WATER_BOOST_MINUTES = 30;
const MIN_SETPOINT = 500; // Matter units: hundredths of a degree
const MAX_SETPOINT = 3000;
const DEFAULT_SETPOINT = 2000;

// ---------------------------------------------------------------- arguments

function arg(name: string, fallback: string): string {
    const i = process.argv.indexOf(`--${name}`);
    return i >= 0 && process.argv[i + 1] ? process.argv[i + 1] : fallback;
}
const storageDir = arg("state-dir", "./matter-state");
const port = Number(arg("udp-port", "5540"));

// Matter's log goes to stderr so stdout carries only status lines.
Logger.format = "plain";
// WISER_MATTER_LOG=debug (or info) shows what controllers ask the bridge, for
// investigating an app's behaviour; the dashboard forwards the lines to its log.
Logger.level = ({ debug: LogLevel.DEBUG, info: LogLevel.INFO, warn: LogLevel.WARN } as Record<string, LogLevel>)[process.env.WISER_MATTER_LOG ?? ""] ?? LogLevel.NOTICE;
Logger.destinations.default.write = (text: string) => void process.stderr.write(text + "\n");

function report(msg: Record<string, unknown>) {
    process.stdout.write(JSON.stringify(msg) + "\n");
}
function log(msg: string) {
    process.stderr.write(`wiser-matter: ${msg}\n`);
}

// ---------------------------------------------------------------- identity

// Each install gets its own random pairing code, kept with Matter's storage.
interface Identity {
    passcode: number;
    discriminator: number;
    serial: string;
}
const INVALID_PASSCODES = new Set([
    0, 11111111, 22222222, 33333333, 44444444, 55555555, 66666666, 77777777, 88888888, 99999999, 12345678, 87654321,
]);

function loadIdentity(): Identity {
    mkdirSync(storageDir, { recursive: true });
    const file = join(storageDir, "identity.json");
    try {
        return JSON.parse(readFileSync(file, "utf8")) as Identity;
    } catch {
        let passcode = 0;
        while (INVALID_PASSCODES.has(passcode)) passcode = randomInt(1, 99999999);
        const id: Identity = {
            passcode,
            discriminator: randomInt(0, 4096),
            serial: randomInt(0, 2 ** 47).toString(16).padStart(12, "0"),
        };
        writeFileSync(file, JSON.stringify(id), { mode: 0o600 });
        return id;
    }
}

// ---------------------------------------------------------------- dashboard link

// Per-room heating boost switches: on = current temperature + delta for
// minutes, off = back to the schedule. Set from the dashboard's Google Home page.
interface BoostSettings {
    minutes: number;
    delta: number; // tenths of a degree
    off: Record<string, boolean>; // room id -> no switch
}
let boostSettings: BoostSettings = { minutes: 30, delta: 30, off: {} };

// The waste guard's "Heating alert" sensor: there while the guard is on, open
// while one of its alerts is active, so Apple Home can notify and Google Home
// or Alexa routines can start on it. Optionally, up to three more sensors named
// with the alerts themselves (Matter can't carry text, but a new device's name
// shows up), and the last 24 hours' heating as a temperature sensor.
interface AlertState {
    enabled: boolean;
    on: boolean;
    texts: { id: string; text: string }[];
    hours: number | null;
    refresh: boolean; // ask the apps to subscribe again after a device is added or removed
}
let alertState: AlertState = { enabled: false, on: false, texts: [], hours: null, refresh: true };
let alertReceived = false; // the dashboard has said which guard devices to show

let dashboardURL = process.env.WISER_URL ?? "";
let secret = process.env.WISER_SECRET ?? "";

// Settings can arrive on stdin before the Matter node exists; they're kept and
// the first sync applies them.
let nodeReady = false;
let syncChain: Promise<void> = Promise.resolve();

createInterface({ input: process.stdin }).on("line", line => {
    try {
        const msg = JSON.parse(line);
        if (typeof msg.url === "string") dashboardURL = msg.url.replace(/\/$/, "");
        if (typeof msg.secret === "string") secret = msg.secret;
        if (msg.cmd === "reset") void reset();
        if (msg.boost && typeof msg.boost === "object") {
            boostSettings = { ...boostSettings, ...msg.boost };
            syncSoon();
        }
        if (msg.alert && typeof msg.alert === "object") {
            alertReceived = true;
            alertState = {
                enabled: !!msg.alert.enabled,
                on: !!msg.alert.on,
                // Each occurrence of an alert comes with its own id, so the apps
                // announce it as a new device even when the text is the same.
                texts: (Array.isArray(msg.alert.texts) ? msg.alert.texts : [])
                    .map((t: any) => (typeof t === "string" ? { id: "", text: t } : { id: String(t?.id ?? ""), text: String(t?.text ?? "") }))
                    .filter((t: { text: string }) => t.text)
                    .slice(0, 3),
                hours: typeof msg.alert.hours === "number" ? msg.alert.hours : null,
                refresh: msg.alert.refresh !== false,
            };
            syncSoon();
        }
    } catch (e) {
        log(`ignoring a bad line on stdin: ${(e as Error).message}: ${line.slice(0, 80)}`);
    }
});
// The dashboard closing our stdin means it has gone: stop with it.
process.stdin.on("end", () => void shutdown());

async function hub(method: string, path: string, body?: unknown): Promise<any> {
    if (!dashboardURL || !secret) throw new Error("waiting for the dashboard");
    const res = await fetch(dashboardURL + path, {
        method,
        // The dashboard tells our changes apart from Home Assistant's (waste guard).
        headers: { SECRET: secret, "Content-Type": "application/json", "X-Wiser-Client": "matter" },
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: AbortSignal.timeout(60_000),
    });
    const text = await res.text();
    if (!res.ok) throw new Error(`${method} ${path}: ${res.status} ${text.slice(0, 120)}`);
    return text ? JSON.parse(text) : null;
}

// ---------------------------------------------------------------- Matter node

const identity = loadIdentity();
Environment.default.vars.set("storage.path", storageDir);

const server = await ServerNode.create({
    id: "wiser",
    network: { port },
    commissioning: { passcode: identity.passcode, discriminator: identity.discriminator },
    productDescription: { name: "Wiser Heating", deviceType: AggregatorEndpoint.deviceType },
    basicInformation: {
        vendorName: "wiser-dashboard",
        vendorId: VendorId(0xfff1), // test vendor ID: no Matter certification
        productName: "Wiser Heating Bridge",
        productLabel: "Wiser Heating",
        productId: 0x8000,
        nodeLabel: "Wiser Heating",
        serialNumber: `wiser-${identity.serial}`,
        uniqueId: `wiser-matter-${identity.serial}`,
        softwareVersion: 1,
        softwareVersionString: VERSION,
        hardwareVersion: 1,
        hardwareVersionString: "1",
    },
});
const bridge = new Endpoint(AggregatorEndpoint, { id: "wiser" });
await server.add(bridge);

// Switches act on the controller's on/off commands rather than on their state
// changing: a repeated "on" then restarts a boost, and an "off" can't be lost
// to a hub reading applied in between. toggle() and the timed commands go
// through on() and off() too.
const switchCommands = new Map<string, (on: boolean) => void>(); // endpoint id -> handler
class CommandOnOffServer extends OnOffPlugInUnitRequirements.OnOffServer {
    override async on() {
        await super.on();
        switchCommands.get(this.endpoint.id)?.(true);
    }
    override async off() {
        await super.off();
        switchCommands.get(this.endpoint.id)?.(false);
    }
}

const RoomDevice = ThermostatDevice.with(BridgedDeviceBasicInformationServer, ThermostatServer.with(Thermostat.Feature.Heating));
const HotWaterDevice = OnOffPlugInUnitDevice.with(BridgedDeviceBasicInformationServer, CommandOnOffServer);
const AlertDevice = ContactSensorDevice.with(BridgedDeviceBasicInformationServer);
const HoursDevice = TemperatureSensorDevice.with(BridgedDeviceBasicInformationServer);

const rooms = new Map<number, Endpoint<typeof RoomDevice>>();
const hotWater = new Map<number, Endpoint<typeof HotWaterDevice>>();
const boosts = new Map<number, Endpoint<typeof HotWaterDevice>>(); // room id -> boost switch
let alertSensor: Endpoint<typeof AlertDevice> | undefined;
const alertTexts = new Map<string, Endpoint<typeof AlertDevice>>(); // endpoint id -> a sensor named with an alert
let hoursSensor: Endpoint<typeof HoursDevice> | undefined;
const HOURS_LABEL = "Heating hours (last 24h)";
let lastDomain: any = null;
const heldUntil = new Map<string, number>(); // endpoint id -> time its pending command settles
const commandAt = new Map<string, number>(); // endpoint id -> time of its last command
const lastOnSetpoint = new Map<number, number>(); // room id -> last setpoint that wasn't "off"

function hold(key: string) {
    commandAt.set(key, Date.now());
    heldUntil.set(key, Date.now() + HOLD_AFTER_WRITE_MS);
}
// fetchedAt is set when each hub reading is requested: a reading requested
// before a command is out of date for that endpoint, however long it took.
let fetchedAt = 0;
function held(key: string) {
    return (heldUntil.get(key) ?? 0) > Date.now() || (commandAt.get(key) ?? -1) >= fetchedAt;
}

function label(name: string) {
    return name.slice(0, 32); // Matter's limit for nodeLabel
}

// A device keeps its endpoint number when it's added back (the Matter spec
// expects that of a bridge). The library forgets a deleted endpoint's number,
// and Google Home ties a device to its number: added back under a new one, the
// device showed as offline until Google's next rediscovery, up to 20 minutes.
const numbersFile = join(storageDir, "endpoint-numbers.json");
const numbers = new Map<string, number>();
try {
    for (const [id, n] of Object.entries(JSON.parse(readFileSync(numbersFile, "utf8")) as Record<string, number>)) {
        if (Number.isInteger(n) && n > 1) numbers.set(id, n);
    }
} catch {}
function numberFor(id: string): { number?: number } {
    const n = numbers.get(id);
    return n === undefined ? {} : { number: n };
}
async function addToBridge(ep: Endpoint<any>) {
    await bridge.add(ep);
    if (typeof ep.number === "number" && numbers.get(ep.id) !== ep.number) {
        numbers.set(ep.id, ep.number);
        try {
            writeFileSync(numbersFile, JSON.stringify(Object.fromEntries(numbers)), { mode: 0o600 });
        } catch (e) {
            log(`couldn't save endpoint numbers: ${(e as Error).message}`);
        }
    }
    nudgeControllersSoon();
}

// toMatter turns the hub's tenths of a degree into Matter's hundredths.
function toMatter(tenths: number) {
    return Math.min(MAX_SETPOINT, Math.max(MIN_SETPOINT, tenths * 10));
}
// toHub rounds Matter's hundredths to the hub's half-degree steps.
function toHub(hundredths: number) {
    return Math.round(hundredths / 50) * 5;
}

async function command(key: string, path: string, body: unknown, what: string) {
    hold(key);
    try {
        await hub("PATCH", path, body);
        log(what);
    } catch (e) {
        heldUntil.delete(key);
        log(`${what} failed: ${(e as Error).message}`);
    }
}

// Controllers often send a setpoint per button press; send the hub the last one.
const pendingSetpoint = new Map<number, ReturnType<typeof setTimeout>>();

function addRoom(room: any) {
    const id = `room-${room.id}`;
    const ep = new Endpoint(RoomDevice, {
        id,
        ...numberFor(id),
        bridgedDeviceBasicInformation: {
            nodeLabel: label(room.Name ?? `Room ${room.id}`),
            productName: "Wiser room",
            uniqueId: `${identity.serial}-${id}`,
            reachable: true,
        },
        thermostat: {
            controlSequenceOfOperation: Thermostat.ControlSequenceOfOperation.HeatingOnly,
            systemMode: Thermostat.SystemMode.Heat,
            occupiedHeatingSetpoint: DEFAULT_SETPOINT,
            absMinHeatSetpointLimit: MIN_SETPOINT,
            minHeatSetpointLimit: MIN_SETPOINT,
            absMaxHeatSetpointLimit: MAX_SETPOINT,
            maxHeatSetpointLimit: MAX_SETPOINT,
        },
    });
    rooms.set(room.id, ep);
    return ep;
}

function watchRoom(roomId: number, ep: Endpoint<typeof RoomDevice>) {
    const key = `room-${roomId}`;
    const path = `/data/domain/Room/${roomId}`;
    ep.events.thermostat.occupiedHeatingSetpoint$Changed.on((value, _old, context) => {
        if (context?.fabric === undefined) return; // our own update from the hub
        clearTimeout(pendingSetpoint.get(roomId));
        hold(key);
        pendingSetpoint.set(
            roomId,
            setTimeout(() => {
                const sp = toHub(value);
                lastOnSetpoint.set(roomId, sp);
                void command(key, path, { RequestOverride: { Type: "Manual", SetPoint: sp } }, `${key} set to ${sp / 10}°`);
            }, 1_000),
        );
    });
    ep.events.thermostat.systemMode$Changed.on((mode, _old, context) => {
        if (context?.fabric === undefined) return;
        if (mode === Thermostat.SystemMode.Off) {
            void command(key, path, { RequestOverride: { Type: "Manual", SetPoint: OFF } }, `${key} off`);
        } else {
            const sp = lastOnSetpoint.get(roomId) ?? toHub(ep.state.thermostat.occupiedHeatingSetpoint ?? DEFAULT_SETPOINT);
            void command(key, path, { RequestOverride: { Type: "Manual", SetPoint: sp } }, `${key} on at ${sp / 10}°`);
        }
    });
}

function addHotWater(hw: any) {
    const id = `hotwater-${hw.id}`;
    const ep = new Endpoint(HotWaterDevice, {
        id,
        ...numberFor(id),
        bridgedDeviceBasicInformation: {
            nodeLabel: "Hot water",
            productName: "Wiser hot water",
            uniqueId: `${identity.serial}-${id}`,
            reachable: true,
        },
    });
    hotWater.set(hw.id, ep);
    return ep;
}

function watchHotWater(hwId: number, ep: Endpoint<typeof HotWaterDevice>) {
    const key = `hotwater-${hwId}`;
    const path = `/data/domain/HotWater/${hwId}`;
    // On boosts for half an hour; off turns it off until the next scheduled change.
    switchCommands.set(ep.id, on => {
        const ov = on
            ? { Type: "Manual", SetPoint: HOT_WATER_ON, DurationMinutes: HOT_WATER_BOOST_MINUTES, Originator: "App" }
            : { Type: "Manual", SetPoint: OFF };
        void command(key, path, { RequestOverride: ov }, on ? "hot water boost" : "hot water off");
    });
}

function boostLabel(room: any) {
    return label(`${room.Name ?? `Room ${room.id}`} heating boost`);
}

function addBoost(room: any) {
    const id = `boost-${room.id}`;
    const ep = new Endpoint(HotWaterDevice, {
        id,
        ...numberFor(id),
        bridgedDeviceBasicInformation: {
            nodeLabel: boostLabel(room),
            productName: "Wiser room boost",
            uniqueId: `${identity.serial}-${id}`,
            reachable: true,
        },
    });
    boosts.set(room.id, ep);
    return ep;
}

// boosting: the hub is running a timed override in this room (from this
// switch, the dashboard or the Wiser app).
function boosting(room: any) {
    return room.OverrideType && room.OverrideType !== "None" && (room.OverrideTimeoutUnixTime ?? 0) > Date.now() / 1000;
}

// boostSetpoint is the current temperature plus the delta, never below the
// room's current target, in the hub's half-degree steps (tenths).
function boostSetpoint(room: any) {
    const temp = typeof room.CalculatedTemperature === "number" && room.CalculatedTemperature > -1000 ? room.CalculatedTemperature : null;
    const target = typeof room.CurrentSetPoint === "number" && room.CurrentSetPoint > OFF ? room.CurrentSetPoint : null;
    let sp = temp !== null ? temp + boostSettings.delta : (target ?? 200) + boostSettings.delta;
    if (target !== null) sp = Math.max(sp, target);
    return Math.min(MAX_SETPOINT / 10, Math.max(MIN_SETPOINT / 10, Math.round(sp / 5) * 5));
}

function watchBoost(roomId: number, ep: Endpoint<typeof HotWaterDevice>) {
    const key = `boost-${roomId}`;
    const path = `/data/domain/Room/${roomId}`;
    switchCommands.set(ep.id, on => {
        hold(`room-${roomId}`); // the thermostat shows the boost once the hub reports it
        if (!on) {
            void command(key, path, { RequestOverride: { Type: "None" } }, `${key} cancelled`);
            return;
        }
        const room = (lastDomain?.Room ?? []).find((r: any) => r.id === roomId) ?? { id: roomId };
        const sp = boostSetpoint(room);
        const minutes = boostSettings.minutes;
        void command(
            key,
            path,
            { RequestOverride: { Type: "Manual", SetPoint: sp, DurationMinutes: minutes, Originator: "App" } },
            `${key} to ${sp / 10}° for ${minutes} min`,
        );
    });
}

// ---------------------------------------------------------------- sync from the hub

let lastGood = 0;
let reachable = true;
let lastError = "";

async function setReachable(value: boolean) {
    if (reachable === value) return;
    reachable = value;
    for (const ep of [...rooms.values(), ...hotWater.values(), ...boosts.values(), ...guardDevices()]) {
        await ep.set({ bridgedDeviceBasicInformation: { reachable: value } });
    }
    log(value ? "hub reachable again" : "hub unreachable");
}

async function syncBoost(room: any) {
    let ep = boosts.get(room.id);
    if (boostSettings.off[String(room.id)]) {
        if (ep) {
            boosts.delete(room.id);
            await ep.delete();
            nudgeControllersSoon();
            log(`removed ${boostLabel(room)}`);
        }
        return;
    }
    if (!ep) {
        ep = addBoost(room);
        await addToBridge(ep);
        watchBoost(room.id, ep);
        log(`added ${boostLabel(room)}`);
    }
    const update: Record<string, any> = { bridgedDeviceBasicInformation: { nodeLabel: boostLabel(room) } };
    if (!held(`boost-${room.id}`)) update.onOff = { onOff: !!boosting(room) };
    await ep.set(update);
}

// syncAlert adds, updates or removes the Heating alert sensor. A contact
// sensor reads "closed" (true) normally and "open" (false) on an alert.
async function syncAlert() {
    if (!alertState.enabled) {
        if (alertSensor) {
            const ep = alertSensor;
            alertSensor = undefined;
            await ep.delete();
            nudgeControllersSoon();
            log("removed Heating alert");
        }
        return;
    }
    if (!alertSensor) {
        alertSensor = new Endpoint(AlertDevice, {
            id: "heating-alert",
            ...numberFor("heating-alert"),
            bridgedDeviceBasicInformation: {
                nodeLabel: "Heating alert",
                productName: "Wiser waste guard alert",
                uniqueId: `${identity.serial}-heating-alert`,
                reachable,
            },
            booleanState: { stateValue: !alertState.on },
        });
        await addToBridge(alertSensor);
        log("added Heating alert");
    }
    if (alertSensor.state.booleanState.stateValue !== !alertState.on) {
        await alertSensor.set({ booleanState: { stateValue: !alertState.on } });
        log(alertState.on ? "Heating alert open" : "Heating alert closed");
    }
}

// guardDevices are the waste guard's endpoints, for reachability and counts.
function guardDevices(): Endpoint<any>[] {
    return [...(alertSensor ? [alertSensor] : []), ...alertTexts.values(), ...(hoursSensor ? [hoursSensor] : [])];
}

// textId names an alert-text sensor after the alert's occurrence (or its
// text, from an older dashboard), so the same alert keeps its device and a new
// one gets a new device, whose name the apps then announce.
function textId(t: { id: string; text: string }) {
    if (/^[0-9a-z]{1,16}$/.test(t.id)) return `alert-text-${t.id}`;
    let h = 0x811c9dc5;
    for (const c of t.text) h = Math.imul(h ^ c.codePointAt(0)!, 0x01000193) >>> 0;
    return `alert-text-${h.toString(16).padStart(8, "0")}`;
}

async function syncAlertTexts() {
    const want = new Map(alertState.texts.map(t => [textId(t), t.text]));
    for (const [id, ep] of alertTexts) {
        if (!want.has(id)) {
            alertTexts.delete(id);
            await ep.delete();
            nudgeControllersSoon();
            log(`removed alert text device ${id}`);
        }
    }
    for (const [id, text] of want) {
        if (alertTexts.has(id)) continue;
        const ep = new Endpoint(AlertDevice, {
            id,
            bridgedDeviceBasicInformation: {
                nodeLabel: label(text),
                productName: "Wiser waste guard alert text",
                uniqueId: `${identity.serial}-${id}`,
                reachable,
            },
            booleanState: { stateValue: false }, // open: it only exists while the alert does
        });
        alertTexts.set(id, ep);
        await addToBridge(ep);
        log(`added alert text device "${text}"`);
    }
}

// syncHours shows the last 24 hours' heating as a temperature: 13.5° is 13.5 hours.
async function syncHours() {
    if (alertState.hours === null) {
        if (hoursSensor) {
            const ep = hoursSensor;
            hoursSensor = undefined;
            await ep.delete();
            nudgeControllersSoon();
            log("removed heating hours sensor");
        }
        return;
    }
    const value = Math.round(Math.min(24, Math.max(0, alertState.hours)) * 100);
    if (!hoursSensor) {
        hoursSensor = new Endpoint(HoursDevice, {
            id: "heating-hours",
            ...numberFor("heating-hours"),
            bridgedDeviceBasicInformation: {
                nodeLabel: HOURS_LABEL,
                productName: "Wiser heating hours",
                uniqueId: `${identity.serial}-heating-hours`,
                reachable,
            },
            temperatureMeasurement: { measuredValue: value, minMeasuredValue: 0, maxMeasuredValue: 2400 },
        });
        await addToBridge(hoursSensor);
        log("added heating hours sensor");
    } else if (hoursSensor.state.temperatureMeasurement.measuredValue !== value) {
        await hoursSensor.set({ temperatureMeasurement: { measuredValue: value } });
    }
}

// A controller learns of an endpoint added after it subscribed only when it
// subscribes again. Google Home does that on its own schedule and shows the
// device as offline until then. Closing the controllers' sessions politely (the
// Matter CloseSession message, as at shutdown) makes them subscribe again at
// once, and the new subscription covers the new endpoint. Only once online:
// at startup the devices are there before any controller connects.
let online = false; // set once the node is online; cleared while it resets or shuts down
let nudgeTimer: ReturnType<typeof setTimeout> | undefined;
function nudgeControllersSoon() {
    if (!online || !alertState.refresh) return;
    clearTimeout(nudgeTimer);
    nudgeTimer = setTimeout(() => nudgeControllers().catch(e => log(`asking controllers to subscribe again failed: ${(e as Error).message}`)), 2_000);
}
async function nudgeControllers() {
    if (!online) return;
    const sessions = [...server.env.get(SessionManager).sessions].filter(s => !s.isPase && !s.isClosing);
    if (!sessions.length) return;
    log(`asking ${sessions.length} controller session(s) to subscribe again`);
    for (const session of sessions) {
        try {
            await session.closeSubscriptions(true);
            await session.initiateClose();
        } catch (e) {
            log(`closing a controller session failed: ${(e as Error).message}`);
        }
    }
}

// Syncs run one at a time: the poll timer and settings changes both start them.
function syncSoon() {
    if (!nodeReady) return syncChain;
    syncChain = syncChain.then(sync).catch(e => log(`sync failed: ${(e as Error).message}`));
    return syncChain;
}

async function sync() {
    await syncAlert();
    await syncAlertTexts();
    await syncHours();
    let domain: any;
    const requestedAt = Date.now();
    try {
        domain = await hub("GET", "/data/domain/");
        lastGood = Date.now();
        lastError = "";
        lastDomain = domain;
        fetchedAt = requestedAt;
    } catch (e) {
        lastError = (e as Error).message;
        if (lastGood && Date.now() - lastGood > UNREACHABLE_AFTER_MS) await setReachable(false);
        return;
    }
    await setReachable(true);

    for (const room of domain.Room ?? []) {
        let ep = rooms.get(room.id);
        if (!ep) {
            ep = addRoom(room);
            await addToBridge(ep);
            watchRoom(room.id, ep);
            log(`added room ${room.Name}`);
        }
        await syncBoost(room);
        if (held(`room-${room.id}`)) continue;
        const update: Record<string, number> = {};
        if (typeof room.CalculatedTemperature === "number" && room.CalculatedTemperature > -1000) {
            update.externalMeasuredIndoorTemperature = room.CalculatedTemperature * 10;
        }
        const sp = room.CurrentSetPoint;
        if (typeof sp === "number" && sp > OFF) {
            lastOnSetpoint.set(room.id, sp);
            update.occupiedHeatingSetpoint = toMatter(sp);
            update.systemMode = Thermostat.SystemMode.Heat;
        } else if (sp === OFF) {
            update.systemMode = Thermostat.SystemMode.Off;
        }
        await ep.set({
            thermostat: update,
            bridgedDeviceBasicInformation: { nodeLabel: label(room.Name ?? `Room ${room.id}`) },
        });
    }
    for (const [id, ep] of rooms) {
        if (!(domain.Room ?? []).some((r: any) => r.id === id)) {
            rooms.delete(id);
            await ep.delete();
            nudgeControllersSoon();
            await boosts.get(id)?.delete();
            nudgeControllersSoon();
            boosts.delete(id);
            log(`removed room ${id}`);
        }
    }

    for (const hw of domain.HotWater ?? []) {
        let ep = hotWater.get(hw.id);
        if (!ep) {
            ep = addHotWater(hw);
            await addToBridge(ep);
            watchHotWater(hw.id, ep);
        }
        if (held(`hotwater-${hw.id}`)) continue;
        await ep.set({ onOff: { onOff: hw.WaterHeatingState === "On" } });
    }
}

// ---------------------------------------------------------------- status

// qrRows turns matter.js's text-art QR code into rows of "1" (dark) and "0"
// (light), so the dashboard can draw it. In the art each character is two
// module rows, and a filled half is a light module.
function qrRows(code: string): string[] {
    const art = QrCode.encode(code).split("\n").slice(1, -2); // drop the quiet-zone border rows
    const top: Record<string, boolean> = { "█": true, "▀": true, "▄": false, "⠀": false };
    const bottom: Record<string, boolean> = { "█": true, "▀": false, "▄": true, "⠀": false };
    const rows: string[] = [];
    for (const line of art) {
        const cells = [...line].slice(1, -1); // drop the side borders
        rows.push(cells.map(c => (top[c] ? "0" : "1")).join(""));
        rows.push(cells.map(c => (bottom[c] ? "0" : "1")).join(""));
    }
    return rows.slice(0, rows[0]?.length ?? 0); // square; the last half row is padding
}

function status() {
    const codes = server.state.commissioning.pairingCodes;
    const fabrics = (server.state.operationalCredentials.fabrics ?? []).map(f => ({
        label: f.label,
        vendorId: Number(f.vendorId),
    }));
    report({
        type: "status",
        version: VERSION,
        port,
        // The pairings themselves are what count: after Google Home pairs, the
        // commissioned flag can still read false while its fabric is there.
        commissioned: server.state.commissioning.commissioned || fabrics.length > 0,
        manualCode: codes.manualPairingCode,
        qrCode: codes.qrPairingCode,
        qrRows: qrRows(codes.qrPairingCode),
        fabrics,
        devices: rooms.size + hotWater.size + boosts.size + guardDevices().length,
        hubReachable: reachable && lastGood > 0,
        error: lastError,
    });
}

async function reset() {
    log("removing all pairings");
    online = false;
    clearTimeout(nudgeTimer);
    numbers.clear();
    try {
        writeFileSync(numbersFile, "{}", { mode: 0o600 });
    } catch {}
    await server.erase();
    await server.start();
    online = true;
    status();
}

let closing = false;
async function shutdown() {
    if (closing) return;
    closing = true;
    online = false;
    clearTimeout(nudgeTimer);
    try {
        await server.close();
    } finally {
        process.exit(0);
    }
}
process.on("SIGTERM", () => void shutdown());
process.on("SIGINT", () => void shutdown());

// ---------------------------------------------------------------- run

server.lifecycle.commissioned.on(() => status());
server.lifecycle.decommissioned.on(() => status());
server.events.commissioning.fabricsChanged.on(() => status());

nodeReady = true;
// Add the devices before going online, so controllers see them at pairing and
// after every restart. That includes the waste guard's: Google Home can leave a
// device added while it's connected unreachable for a long while, so wait
// (briefly: an older dashboard never says) for the dashboard to say which.
for (let i = 0; i < 12 && rooms.size === 0; i++) {
    await syncSoon();
    if (rooms.size === 0) await new Promise(r => setTimeout(r, POLL_MS));
}
for (let i = 0; i < 30 && !alertReceived; i++) await new Promise(r => setTimeout(r, 500));
await syncSoon();
await server.start();
online = true;
log(`Matter bridge ${VERSION} on UDP port ${port}, ${rooms.size} rooms`);
status();

let lastStatus = "";
setInterval(async () => {
    await syncSoon();
    // Pairing changes are also caught here: after the last controller removes
    // the bridge, matter.js resets the node without the lifecycle events.
    const fabrics = server.state.operationalCredentials.fabrics?.length ?? 0;
    const now = JSON.stringify([reachable, lastGood > 0, lastError, rooms.size, hotWater.size, boosts.size, guardDevices().length, fabrics]);
    if (now !== lastStatus) {
        lastStatus = now;
        status();
    }
}, POLL_MS);
