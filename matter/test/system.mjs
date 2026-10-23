// Whole-system test, with no real hub involved:
//   mock Wiser hub  <-  wiser-dashboard (with the bridge embedded)  <-  Matter controller
// Checks that the dashboard unpacks and runs the bridge, that commands reach
// the hub, that /api/matter reports pairing, and that pairing survives a
// restart of the whole app.
//
//   node test/system.mjs path/to/wiser-dashboard
import { Environment, Logger, LogLevel } from "@matter/main";
import { BridgedDeviceBasicInformation, GeneralCommissioning, OnOff, Thermostat } from "@matter/main/clusters";
import { ManualPairingCodeCodec } from "@matter/main/types";
import { CommissioningController } from "@project-chip/matter.js";
import { spawn } from "node:child_process";
import { existsSync, mkdtempSync, readdirSync, rmSync } from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";

const BIN = process.argv[2] ?? "../wiser-dashboard";
const SECRET = "system-test-secret";
const HUB = "127.0.0.1:18095";
const DASH = "127.0.0.1:18096";
Logger.level = LogLevel.ERROR;

let failures = 0;
function check(name, ok, detail = "") {
    console.log(`${ok ? "PASS" : "FAIL"}  ${name}${detail ? "  (" + detail + ")" : ""}`);
    if (!ok) failures++;
}
const sleep = ms => new Promise(r => setTimeout(r, ms));
async function until(fn, ms = 20_000) {
    const end = Date.now() + ms;
    while (Date.now() < end) {
        try {
            const v = await fn();
            if (v) return v;
        } catch {}
        await sleep(300);
    }
}

// ---------------------------------------------------------------- mock hub
const hub = {
    System: { LocalDateAndTime: {}, HeatingButtonOverrideState: "Off" },
    Room: [
        { id: 1, Name: "Upper Floor", CalculatedTemperature: 195, CurrentSetPoint: -200, OverrideType: "None" },
        { id: 2, Name: "Ground Floor", CalculatedTemperature: 201, CurrentSetPoint: 200, OverrideType: "None" },
    ],
    HotWater: [{ id: 2, WaterHeatingState: "Off", Mode: "Auto" }],
};
const patches = [];
const mock = createServer((req, res) => {
    if (req.headers.secret !== SECRET) return res.writeHead(401).end('{"Error":"Unauthorized"}');
    let body = "";
    req.on("data", c => (body += c));
    req.on("end", () => {
        const m = req.url.match(/^\/data\/domain\/(\w+)\/?(\d+)?\/?$/);
        if (req.method === "GET" && m && hub[m[1]] !== undefined) {
            return res.writeHead(200, { "Content-Type": "application/json" }).end(JSON.stringify(hub[m[1]]));
        }
        if (req.method === "PATCH" && m?.[2]) {
            const data = JSON.parse(body);
            patches.push({ path: req.url, data });
            const item = hub[m[1]].find(x => x.id === Number(m[2]));
            const ov = data.RequestOverride;
            if (ov && m[1] === "Room") item.CurrentSetPoint = ov.SetPoint;
            if (ov && m[1] === "HotWater") item.WaterHeatingState = ov.SetPoint > 0 ? "On" : "Off";
            return res.writeHead(200).end(JSON.stringify(item));
        }
        res.writeHead(404).end();
    });
});
await new Promise(r => mock.listen(18095, "127.0.0.1", r));

// ---------------------------------------------------------------- the app
const dir = mkdtempSync(join(tmpdir(), "wiser-system-test-"));
let appLog = "";
function startApp() {
    const p = spawn(
        BIN,
        ["-hub", HUB, "-secret", SECRET, "-listen", DASH, "-config", join(dir, "config.json"), "-no-discover", "-matter-port", "5561"],
        { stdio: ["ignore", "pipe", "pipe"] },
    );
    p.stdout.on("data", d => (appLog += d));
    p.stderr.on("data", d => (appLog += d));
    return p;
}
async function stopApp(p) {
    p.kill("SIGTERM");
    return Promise.race([new Promise(r => p.on("exit", c => r(c))), sleep(15_000).then(() => "timeout")]);
}
const api = async path => (await fetch(`http://${DASH}${path}`)).json();

let app = startApp();
const ready = await until(async () => {
    const m = await api("/api/matter");
    return m.status?.devices >= 3 && m;
}, 90_000);
check("dashboard unpacks and starts the bridge", ready?.running === true, ready && `${ready.status.devices} devices`);
check("bridge was unpacked into the config folder", existsSync(join(dir, "matter", "bin")) && readdirSync(join(dir, "matter", "bin")).length === 1);
check("bridge reports its version", !!ready?.status?.version, ready?.status?.version);
if (!ready) {
    console.log(appLog.slice(-4000));
    process.exit(1);
}

// Without mDNS (macOS CI runners) a controller can't pair; stop here.
if (process.env.MATTER_TEST_STARTUP_ONLY) {
    const exit = await stopApp(app);
    check("dashboard stops cleanly with the bridge", exit === 0, `exit ${exit}`);
    mock.close();
    rmSync(dir, { recursive: true, force: true });
    console.log(failures ? `\n${failures} FAILED` : "\nALL PASSED (startup only)");
    process.exit(failures ? 1 : 0);
}

// ---------------------------------------------------------------- pair and control
const env = Environment.default;
env.vars.set("storage.path", join(dir, "controller"));
const controller = new CommissioningController({
    environment: { environment: env, id: "controller" },
    autoConnect: false,
    adminFabricLabel: "system test",
});
await controller.start();
const code = ManualPairingCodeCodec.decode(ready.status.manualCode);
const nodeId = await controller.commissionNode({
    commissioning: { regulatoryLocation: GeneralCommissioning.RegulatoryLocationType.IndoorOutdoor, regulatoryCountryCode: "XX" },
    discovery: {
        identifierData: code.longDiscriminator !== undefined ? { longDiscriminator: code.longDiscriminator } : { shortDiscriminator: code.shortDiscriminator },
        discoveryCapabilities: { onIpNetwork: true },
    },
    passcode: code.passcode,
});
check("controller pairs with the bridge inside the dashboard", !!nodeId);
const node = await controller.getNode(nodeId);
node.connect();
await until(() => node.initialized, 30_000);

const paired = await until(async () => (await api("/api/matter")).status?.commissioned, 15_000);
check("/api/matter shows it is paired", !!paired);

async function find(name) {
    for (const d of node.getDevices().flatMap(d => [d, ...d.getChildEndpoints()])) {
        const info = d.getClusterClient(BridgedDeviceBasicInformation.Cluster);
        if (info && (await info.getNodeLabelAttribute()) === name) return d;
    }
}
const ground = (await find("Ground Floor"))?.getClusterClient(Thermostat.Cluster);
const hw = (await find("Hot water"))?.getClusterClient(OnOff.Cluster);
check("rooms come from the hub through the dashboard", (await ground?.getLocalTemperatureAttribute(true)) === 2010);

await ground.setOccupiedHeatingSetpointAttribute(2100);
let p = await until(() => patches.find(x => x.path === "/data/domain/Room/2"), 30_000);
check("setpoint reaches the hub through the dashboard", p?.data.RequestOverride.SetPoint === 210, JSON.stringify(p?.data));
await hw.on();
p = await until(() => patches.find(x => x.path === "/data/domain/HotWater/2"), 30_000);
check("hot water boost reaches the hub", p?.data.RequestOverride.DurationMinutes === 30);

// Boost switch settings: saved by the dashboard, applied by the bridge.
const devicesBefore = (await api("/api/matter")).status.devices;
const saved = await (await fetch(`http://${DASH}/api/matter/boost`, { method: "POST", body: JSON.stringify({ minutes: 60, delta: 20, off: { 1: true } }) })).json();
check("boost settings are accepted", saved.ok === true, JSON.stringify(saved));
const fewer = await until(async () => (await api("/api/matter")).status?.devices === devicesBefore - 1, 20_000);
check("the bridge drops Upper Floor's boost switch", !!fewer, `${devicesBefore} -> ${devicesBefore - 1}`);
const bad = await (await fetch(`http://${DASH}/api/matter/boost`, { method: "POST", body: JSON.stringify({ minutes: 1000, delta: 20 }) })).json();
check("out-of-range settings are refused", bad.ok === false);

// The waste guard: turning it on adds the Heating alert sensor, off removes it.
const guardPost = async body => (await fetch(`http://${DASH}/api/guard/settings`, { method: "POST", body: JSON.stringify(body) })).json();
const withoutAlert = (await api("/api/matter")).status.devices;
check("waste guard settings are accepted", (await guardPost({ mode: "watch" })).ok === true);
check("the waste guard adds the Heating alert to the bridge", !!(await until(async () => (await api("/api/matter")).status.devices === withoutAlert + 1, 30_000)));
await guardPost({ mode: "" });
check("turning the waste guard off removes it", !!(await until(async () => (await api("/api/matter")).status.devices === withoutAlert, 30_000)));

// ---------------------------------------------------------------- restart the whole app
let exit = await stopApp(app);
check("dashboard stops cleanly with the bridge", exit === 0, `exit ${exit}`);
check("bridge shut down with it", /bridge started/.test(appLog) && !/was not closed/.test(appLog));
app = startApp();
const again = await until(async () => {
    const m = await api("/api/matter");
    return m.status?.devices >= 3 && m;
}, 90_000);
check("after restarting the app it is still paired", again?.status?.commissioned === true);
const kept = await until(async () => {
    const m = await api("/api/matter");
    return m.boost?.minutes === 60 && m.boost?.off?.["1"] === true && m.status?.devices === devicesBefore - 1;
}, 30_000);
check("boost settings survive the restart", !!kept);
hub.Room[1].CalculatedTemperature = 175;
const fresh = await until(async () => (await ground.getLocalTemperatureAttribute(true)) === 1750, 120_000);
check("controller reconnects and sees new hub data", !!fresh);

// ---------------------------------------------------------------- reset from the dashboard
const r = await (await fetch(`http://${DASH}/api/matter/reset`, { method: "POST" })).json();
check("reset is accepted", r.ok === true, JSON.stringify(r));
const unpaired = await until(async () => (await api("/api/matter")).status?.commissioned === false, 20_000);
check("after reset it is unpaired and shows a new pairing code", !!unpaired);

await controller.close();
exit = await stopApp(app);
check("final stop", exit === 0, `exit ${exit}`);
mock.close();
rmSync(dir, { recursive: true, force: true });
if (failures) console.log("\n--- app log ---\n" + appLog.split("\n").slice(-60).join("\n"));
console.log(failures ? `\n${failures} FAILED` : "\nALL PASSED");
process.exit(failures ? 1 : 0);
