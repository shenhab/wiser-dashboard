// wiser-matter is the Matter bridge that wiser-dashboard runs alongside itself.
// It shows each Wiser room as a thermostat and the hot water as a plug to any
// Matter controller (Google Home, Apple Home, Alexa, SmartThings).
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
import { OnOffPlugInUnitDevice } from "@matter/main/devices/on-off-plug-in-unit";
import { ThermostatDevice } from "@matter/main/devices/thermostat";
import { QrCode } from "@matter/main/types";
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
Logger.level = LogLevel.NOTICE;
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

let dashboardURL = process.env.WISER_URL ?? "";
let secret = process.env.WISER_SECRET ?? "";

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
    } catch {
        log("ignoring a bad line on stdin");
    }
});
// The dashboard closing our stdin means it has gone: stop with it.
process.stdin.on("end", () => void shutdown());

async function hub(method: string, path: string, body?: unknown): Promise<any> {
    if (!dashboardURL || !secret) throw new Error("waiting for the dashboard");
    const res = await fetch(dashboardURL + path, {
        method,
        headers: { SECRET: secret, "Content-Type": "application/json" },
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

const RoomDevice = ThermostatDevice.with(BridgedDeviceBasicInformationServer, ThermostatServer.with(Thermostat.Feature.Heating));
const HotWaterDevice = OnOffPlugInUnitDevice.with(BridgedDeviceBasicInformationServer);

const rooms = new Map<number, Endpoint<typeof RoomDevice>>();
const hotWater = new Map<number, Endpoint<typeof HotWaterDevice>>();
const boosts = new Map<number, Endpoint<typeof HotWaterDevice>>(); // room id -> boost switch
let lastDomain: any = null;
const heldUntil = new Map<string, number>(); // endpoint id -> time its pending command settles
const lastOnSetpoint = new Map<number, number>(); // room id -> last setpoint that wasn't "off"

function hold(key: string) {
    heldUntil.set(key, Date.now() + HOLD_AFTER_WRITE_MS);
}
function held(key: string) {
    return (heldUntil.get(key) ?? 0) > Date.now();
}

function label(name: string) {
    return name.slice(0, 32); // Matter's limit for nodeLabel
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
    ep.events.onOff.onOff$Changed.on((on, _old, context) => {
        if (context?.fabric === undefined) return;
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
    ep.events.onOff.onOff$Changed.on((on, _old, context) => {
        if (context?.fabric === undefined) return;
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
    for (const ep of [...rooms.values(), ...hotWater.values(), ...boosts.values()]) {
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
            log(`removed ${boostLabel(room)}`);
        }
        return;
    }
    if (!ep) {
        ep = addBoost(room);
        await bridge.add(ep);
        watchBoost(room.id, ep);
        log(`added ${boostLabel(room)}`);
    }
    const update: Record<string, any> = { bridgedDeviceBasicInformation: { nodeLabel: boostLabel(room) } };
    if (!held(`boost-${room.id}`)) update.onOff = { onOff: !!boosting(room) };
    await ep.set(update);
}

// Syncs run one at a time: the poll timer and settings changes both start them.
let syncChain: Promise<void> = Promise.resolve();
function syncSoon() {
    syncChain = syncChain.then(sync).catch(e => log(`sync failed: ${(e as Error).message}`));
    return syncChain;
}

async function sync() {
    let domain: any;
    try {
        domain = await hub("GET", "/data/domain/");
        lastGood = Date.now();
        lastError = "";
        lastDomain = domain;
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
            await bridge.add(ep);
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
            await boosts.get(id)?.delete();
            boosts.delete(id);
            log(`removed room ${id}`);
        }
    }

    for (const hw of domain.HotWater ?? []) {
        let ep = hotWater.get(hw.id);
        if (!ep) {
            ep = addHotWater(hw);
            await bridge.add(ep);
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
        commissioned: server.state.commissioning.commissioned,
        manualCode: codes.manualPairingCode,
        qrCode: codes.qrPairingCode,
        qrRows: qrRows(codes.qrPairingCode),
        fabrics,
        devices: rooms.size + hotWater.size + boosts.size,
        hubReachable: reachable && lastGood > 0,
        error: lastError,
    });
}

async function reset() {
    log("removing all pairings");
    await server.erase();
    await server.start();
    status();
}

let closing = false;
async function shutdown() {
    if (closing) return;
    closing = true;
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

// Add the devices before going online, so controllers see them at pairing.
for (let i = 0; i < 12 && rooms.size === 0; i++) {
    await syncSoon();
    if (rooms.size === 0) await new Promise(r => setTimeout(r, POLL_MS));
}
await server.start();
log(`Matter bridge ${VERSION} on UDP port ${port}, ${rooms.size} rooms`);
status();

let lastStatus = "";
setInterval(async () => {
    await syncSoon();
    // Pairing changes are also caught here: after the last controller removes
    // the bridge, matter.js resets the node without the lifecycle events.
    const fabrics = server.state.operationalCredentials.fabrics?.length ?? 0;
    const now = JSON.stringify([reachable, lastGood > 0, lastError, rooms.size, hotWater.size, boosts.size, fabrics]);
    if (now !== lastStatus) {
        lastStatus = now;
        status();
    }
}, POLL_MS);
