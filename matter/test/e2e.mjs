// End-to-end test for wiser-matter, with no real hub involved:
//   mock dashboard /data/ API  <-  wiser-matter (compiled)  <-  Matter controller
// The controller pairs over the local network like Google Home would, then
// reads the rooms and sends commands; the mock records what reaches the "hub".
//
//   node test/e2e.mjs path/to/wiser-matter
import { Environment, Logger, LogLevel } from "@matter/main";
import { BooleanState, BridgedDeviceBasicInformation, GeneralCommissioning, OnOff, TemperatureMeasurement, Thermostat } from "@matter/main/clusters";
import { ManualPairingCodeCodec } from "@matter/main/types";
import { CommissioningController } from "@project-chip/matter.js";
import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createInterface } from "node:readline";

const BIN = process.argv[2] ?? "dist/wiser-matter";
const SECRET = "test-secret";
const PORT = 18080;
const MATTER_PORT = 5560;
Logger.level = LogLevel.WARN;

let failures = 0;
function check(name, ok, detail = "") {
    console.log(`${ok ? "PASS" : "FAIL"}  ${name}${detail ? "  (" + detail + ")" : ""}`);
    if (!ok) failures++;
}
const sleep = ms => new Promise(r => setTimeout(r, ms));
async function until(fn, ms = 20_000) {
    const end = Date.now() + ms;
    while (Date.now() < end) {
        const v = await fn();
        if (v) return v;
        await sleep(250);
    }
    return undefined;
}

// ---------------------------------------------------------------- mock dashboard
const domain = {
    System: {},
    Room: [
        { id: 1, Name: "Upper Floor", CalculatedTemperature: 195, CurrentSetPoint: -200 },
        { id: 2, Name: "Ground Floor", CalculatedTemperature: 201, CurrentSetPoint: 200 },
    ],
    HotWater: [{ id: 2, WaterHeatingState: "Off" }],
};
const patches = [];
let unauthorized = 0;
// A slow hub: readings are captured when asked and answered later, like a real
// hub's would be, so a reply can arrive after a command with the old state.
let readDelay = 0;
let readsInFlight = 0;
const mock = createServer((req, res) => {
    if (req.headers.secret !== SECRET) {
        unauthorized++;
        res.writeHead(401).end('{"Error":"Unauthorized"}');
        return;
    }
    let body = "";
    req.on("data", c => (body += c));
    req.on("end", () => {
        if (req.method === "GET" && req.url === "/data/domain/") {
            const reading = JSON.stringify(domain);
            readsInFlight++;
            setTimeout(() => {
                readsInFlight--;
                res.writeHead(200, { "Content-Type": "application/json" }).end(reading);
            }, readDelay);
            return;
        }
        const m = req.url.match(/^\/data\/domain\/(Room|HotWater)\/(\d+)$/);
        if (req.method === "PATCH" && m) {
            const data = JSON.parse(body);
            patches.push({ path: req.url, data, client: req.headers["x-wiser-client"] });
            const item = domain[m[1]].find(x => x.id === Number(m[2]));
            const ov = data.RequestOverride;
            if (m[1] === "Room" && ov.Type === "None") {
                // back to the schedule, which says 20° in this mock
                Object.assign(item, { CurrentSetPoint: 200, OverrideType: "None", OverrideTimeoutUnixTime: 0 });
            } else if (m[1] === "Room") {
                item.CurrentSetPoint = ov.SetPoint;
                item.OverrideType = "Manual";
                item.OverrideTimeoutUnixTime = ov.DurationMinutes ? Math.floor(Date.now() / 1000) + ov.DurationMinutes * 60 : 0;
            }
            else item.WaterHeatingState = ov.SetPoint > 0 ? "On" : "Off";
            res.writeHead(200).end(JSON.stringify(item));
            return;
        }
        res.writeHead(404).end();
    });
});
await new Promise(r => mock.listen(PORT, "127.0.0.1", r));

// ---------------------------------------------------------------- the bridge
const storage = mkdtempSync(join(tmpdir(), "wiser-matter-test-"));
let bridgeLog = "";
const statuses = [];
function startBridge() {
    const b = spawn(BIN, ["--state-dir", join(storage, "bridge"), "--udp-port", String(MATTER_PORT)], {
        stdio: ["pipe", "pipe", "pipe"],
    });
    b.stderr.on("data", d => (bridgeLog += d));
    createInterface({ input: b.stdout }).on("line", l => statuses.push(JSON.parse(l)));
    return b;
}
async function stopBridge(b) {
    b.stdin.end();
    return Promise.race([new Promise(r => b.on("exit", r)), sleep(10_000).then(() => "timeout")]);
}
let bridge = startBridge();
// The waste guard is off: no guard devices, and nothing to wait for.
bridge.stdin.write(JSON.stringify({ alert: { enabled: false, on: false } }) + "\n");
// First a wrong secret, to check it is refused, then the right one.
bridge.stdin.write(JSON.stringify({ url: `http://127.0.0.1:${PORT}`, secret: "wrong" }) + "\n");
await until(() => unauthorized > 0, 10_000);
check("wrong secret is refused by the API", unauthorized > 0);
bridge.stdin.write(JSON.stringify({ secret: SECRET }) + "\n");

const first = await until(() => statuses.find(s => s.devices >= 5), 60_000);
check("bridge starts and reports pairing codes", !!first?.manualCode, first && `manual ${first.manualCode}, ${first.devices} devices`);
if (!first) {
    console.log(bridgeLog);
    process.exit(1);
}
check("bridge starts unpaired", first.commissioned === false);

// Without mDNS (macOS CI runners can't grant Local Network permission) a
// controller can't pair, so stop after checking that the bridge runs.
if (process.env.MATTER_TEST_STARTUP_ONLY) {
    const code = await stopBridge(bridge);
    check("bridge exits when the dashboard goes away", code === 0, `exit ${code}`);
    mock.close();
    rmSync(storage, { recursive: true, force: true });
    console.log(failures ? `\n${failures} FAILED` : "\nALL PASSED (startup only)");
    process.exit(failures ? 1 : 0);
}

// ---------------------------------------------------------------- the controller
const env = Environment.default;
env.vars.set("storage.path", join(storage, "controller"));
const controller = new CommissioningController({
    environment: { environment: env, id: "controller" },
    autoConnect: false,
    adminFabricLabel: "e2e test",
});
await controller.start();

const { passcode, longDiscriminator, shortDiscriminator } = ManualPairingCodeCodec.decode(first.manualCode);
const nodeId = await controller.commissionNode({
    commissioning: {
        regulatoryLocation: GeneralCommissioning.RegulatoryLocationType.IndoorOutdoor,
        regulatoryCountryCode: "XX",
    },
    discovery: {
        identifierData: longDiscriminator !== undefined ? { longDiscriminator } : { shortDiscriminator },
        discoveryCapabilities: { onIpNetwork: true },
    },
    passcode,
});
check("controller pairs with the bridge (found over mDNS)", !!nodeId);

const node = await controller.getNode(nodeId);
node.connect();
await until(() => node.initialized, 30_000);
const paired = await until(() => statuses.findLast(s => s.commissioned && s.fabrics?.length), 10_000);
check("bridge reports it is paired", !!paired, paired && `fabric "${paired.fabrics[0].label}"`);

async function byName(name) {
    for (const d of node.getDevices().flatMap(d => [d, ...d.getChildEndpoints()])) {
        const info = d.getClusterClient(BridgedDeviceBasicInformation.Cluster);
        if (info && (await info.getNodeLabelAttribute()) === name) return d;
    }
}
const upper = await byName("Upper Floor");
const ground = await byName("Ground Floor");
const hw = await byName("Hot water");
check("rooms and hot water appear as devices", !!(upper && ground && hw));
const upperBoost = await byName("Upper Floor heating boost");
const groundBoost = await byName("Ground Floor heating boost");
check("each room has a heating boost switch", !!(upperBoost && groundBoost));

const tUpper = upper.getClusterClient(Thermostat.Cluster);
const tGround = ground.getClusterClient(Thermostat.Cluster);
const hwOnOff = hw.getClusterClient(OnOff.Cluster);

check("room temperature", (await tUpper.getLocalTemperatureAttribute(true)) === 1950, `${await tUpper.getLocalTemperatureAttribute(true)}`);
check("room that is off shows Off", (await tUpper.getSystemModeAttribute(true)) === Thermostat.SystemMode.Off);
check(
    "room that is on shows Heat at 20°",
    (await tGround.getSystemModeAttribute(true)) === Thermostat.SystemMode.Heat &&
        (await tGround.getOccupiedHeatingSetpointAttribute(true)) === 2000,
);
check("hot water shows off", (await hwOnOff.getOnOffAttribute(true)) === false);

// Commands
patches.length = 0;
await tGround.setOccupiedHeatingSetpointAttribute(2150);
let p = await until(() => patches.find(x => x.path === "/data/domain/Room/2"));
check(
    "setpoint 21.5° reaches the hub",
    p?.data.RequestOverride.Type === "Manual" && p?.data.RequestOverride.SetPoint === 215,
    JSON.stringify(p?.data),
);

patches.length = 0;
await tGround.setOccupiedHeatingSetpointAttribute(2200);
await tGround.setOccupiedHeatingSetpointAttribute(2250);
await sleep(3_000);
check("quick presses send only the last setpoint", patches.length === 1 && patches[0].data.RequestOverride.SetPoint === 225, JSON.stringify(patches.map(x => x.data.RequestOverride.SetPoint)));

patches.length = 0;
await tUpper.setSystemModeAttribute(Thermostat.SystemMode.Heat);
p = await until(() => patches.find(x => x.path === "/data/domain/Room/1"));
check("turning a room on sets a manual setpoint", p?.data.RequestOverride.SetPoint > 0, JSON.stringify(p?.data));

patches.length = 0;
await tGround.setSystemModeAttribute(Thermostat.SystemMode.Off);
p = await until(() => patches.find(x => x.path === "/data/domain/Room/2"));
check("turning a room off sends -200", p?.data.RequestOverride.SetPoint === -200, JSON.stringify(p?.data));

patches.length = 0;
await hwOnOff.on();
p = await until(() => patches.find(x => x.path === "/data/domain/HotWater/2"));
check(
    "hot water on = 30 min boost",
    p?.data.RequestOverride.SetPoint === 1100 && p?.data.RequestOverride.DurationMinutes === 30,
    JSON.stringify(p?.data),
);
patches.length = 0;
await hwOnOff.off();
p = await until(() => patches.find(x => x.path === "/data/domain/HotWater/2"));
check("hot water off", p?.data.RequestOverride.SetPoint === -200, JSON.stringify(p?.data));

// Saying "on" again while it's on restarts the boost (the command counts, not the state).
await hwOnOff.on();
await until(() => patches.some(x => x.path === "/data/domain/HotWater/2" && x.data.RequestOverride.SetPoint === 1100));
patches.length = 0;
await hwOnOff.on();
p = await until(() => patches.find(x => x.path === "/data/domain/HotWater/2"));
check("hot water on again restarts the boost", p?.data.RequestOverride.DurationMinutes === 30, JSON.stringify(p?.data));

// On then straight away off, while a slow hub reading taken before both is on its way:
// the old reading must not undo the "on", or the "off" would be lost.
readDelay = 3000;
for (let i = 0; i < 3; i++) {
    await until(() => readsInFlight > 0, 15_000);
    patches.length = 0;
    await hwOnOff.on();
    await hwOnOff.off();
    await until(() => patches.filter(x => x.path === "/data/domain/HotWater/2").length >= 2, 15_000);
    const sent = patches.filter(x => x.path === "/data/domain/HotWater/2").map(x => x.data.RequestOverride.SetPoint);
    check(`on then off during a slow hub reading reaches the hub (try ${i + 1})`, JSON.stringify(sent) === "[1100,-200]", JSON.stringify(sent));
    await sleep(4_000);
}
readDelay = 0;

// Heating boost switches: on = current temperature + 3° for 30 min, off = back to schedule.
const gBoost = groundBoost.getClusterClient(OnOff.Cluster);
const uBoost = upperBoost.getClusterClient(OnOff.Cluster);
check("boost switch starts off", (await gBoost.getOnOffAttribute(true)) === false);
Object.assign(domain.Room[1], { CalculatedTemperature: 183, CurrentSetPoint: 200, OverrideType: "None", OverrideTimeoutUnixTime: 0 });
await sleep(6_000); // let the bridge see the new room state
patches.length = 0;
await gBoost.on();
p = await until(() => patches.find(x => x.path === "/data/domain/Room/2"));
check(
    "boost: 18.3° + 3° = 21.5° (half-degree steps) for 30 min",
    p?.data.RequestOverride.SetPoint === 215 && p?.data.RequestOverride.DurationMinutes === 30 && p?.data.RequestOverride.Type === "Manual",
    JSON.stringify(p?.data),
);
const shownOn = await until(async () => (await gBoost.getOnOffAttribute(true)) === true, 40_000);
check("boost switch shows on while the hub is boosting", !!shownOn);
patches.length = 0;
await gBoost.off();
p = await until(() => patches.find(x => x.path === "/data/domain/Room/2"));
check("boost off: back to schedule", p?.data.RequestOverride.Type === "None", JSON.stringify(p?.data));

// A boost never lowers a higher target.
Object.assign(domain.Room[0], { CalculatedTemperature: 190, CurrentSetPoint: 240, OverrideType: "None", OverrideTimeoutUnixTime: 0 });
await sleep(6_000);
patches.length = 0;
await uBoost.on();
p = await until(() => patches.find(x => x.path === "/data/domain/Room/1"));
check("boost keeps a higher target (24° stays 24°)", p?.data.RequestOverride.SetPoint === 240, JSON.stringify(p?.data));

// A boost that ends on the hub turns the switch off by itself.
domain.Room[0].OverrideTimeoutUnixTime = Math.floor(Date.now() / 1000) - 1;
domain.Room[0].OverrideType = "None";
const endedOff = await until(async () => (await uBoost.getOnOffAttribute(true)) === false, 60_000);
check("boost switch turns off when the boost ends", !!endedOff);

// Settings from the dashboard: length, increase, and which rooms get a switch.
bridge.stdin.write(JSON.stringify({ boost: { minutes: 60, delta: 20, off: { 1: true } } }) + "\n");
const removed = await until(() => statuses.findLast(s => s.devices === 4), 20_000);
check("turning a room's switch off in settings removes it", !!removed);
patches.length = 0;
await gBoost.on();
p = await until(() => patches.find(x => x.path === "/data/domain/Room/2"));
check("new settings apply: +2° for 60 min", p?.data.RequestOverride.DurationMinutes === 60 && p?.data.RequestOverride.SetPoint === 205, JSON.stringify(p?.data));
bridge.stdin.write(JSON.stringify({ boost: { minutes: 30, delta: 30, off: {} } }) + "\n");
const back = await until(() => statuses.findLast(s => s.devices === 5), 20_000);
check("turning it back on brings the switch back", !!back);

check("the bridge marks its requests for the waste guard", patches.every(x => x.client === "matter"), JSON.stringify(patches.map(x => x.client)));

// The waste guard's Heating alert: a contact sensor, closed normally, open on an alert.
bridge.stdin.write(JSON.stringify({ alert: { enabled: true, on: false } }) + "\n");
check("turning the waste guard on adds the Heating alert", !!(await until(() => statuses.findLast(s => s.devices === 6), 20_000)));
// A device added after pairing can take the controller a while to pick up (slow CI runners especially).
const alertDev = await until(() => byName("Heating alert"), 90_000);
const alertState = alertDev?.getClusterClient(BooleanState.Cluster);
check("Heating alert reads closed with no alert", !!(await until(async () => alertState && (await alertState.getStateValueAttribute(true)) === true, 20_000)), alertDev ? "" : "controller didn't see the device");
bridge.stdin.write(JSON.stringify({ alert: { enabled: true, on: true } }) + "\n");
check("an alert opens the Heating alert", !!(await until(async () => (await alertState?.getStateValueAttribute(true)) === false, 20_000)));
// Options: alert texts as device names, and the 24 hours' heating as a temperature.
bridge.stdin.write(JSON.stringify({ alert: { enabled: true, on: true, texts: [{ id: "a1", text: "Lounge back to 16°" }], hours: 13.3 } }) + "\n");
check("alert text and hours devices are added", !!(await until(() => statuses.findLast(s => s.devices === 8), 20_000)));
check("an alert text device is named with the alert", !!(await until(() => byName("Lounge back to 16°"), 30_000)));
const hoursDev = await until(() => byName("Heating hours (last 24h)"), 30_000);
const hoursTemp = hoursDev?.getClusterClient(TemperatureMeasurement.Cluster);
check("the hours sensor shows 13.3 h as 13.3°", (await hoursTemp?.getMeasuredValueAttribute(true)) === 1330, String(await hoursTemp?.getMeasuredValueAttribute(true)));
bridge.stdin.write(JSON.stringify({ alert: { enabled: true, on: true, texts: [{ id: "b2", text: "Heating over 12h in 24h" }], hours: 14 } }) + "\n");
check("a new alert replaces the text device", !!(await until(async () => (await byName("Heating over 12h in 24h")) && !(await byName("Lounge back to 16°")), 30_000)));
// The same text again, as a new occurrence: a new device, so the apps announce it.
bridge.stdin.write(JSON.stringify({ alert: { enabled: true, on: true, texts: [{ id: "c3", text: "Heating over 12h in 24h" }], hours: 14 } }) + "\n");
check("a repeat of the same alert is a new device", !!(await until(() => bridgeLog.includes("removed alert text device alert-text-b2") && bridgeLog.includes("added alert text device"), 20_000)));
// Only the alert texts, without the permanent Heating alert.
bridge.stdin.write(JSON.stringify({ alert: { enabled: false, on: true, texts: [{ id: "c3", text: "Heating over 12h in 24h" }], hours: 14 } }) + "\n");
check("the Heating alert can be hidden while alert texts show", !!(await until(async () => !(await byName("Heating alert")) && (await byName("Heating over 12h in 24h")), 30_000)));
check("the hours sensor follows", !!(await until(async () => (await hoursTemp?.getMeasuredValueAttribute(true)) === 1400, 20_000)));
bridge.stdin.write(JSON.stringify({ alert: { enabled: false, on: false, texts: [], hours: 14 } }) + "\n");
check("the hours sensor stays with the guard off", !!(await until(() => statuses.findLast(s => s.devices === 6), 20_000)));
bridge.stdin.write(JSON.stringify({ alert: { enabled: false, on: false } }) + "\n");
check("turning the waste guard off removes the Heating alert", !!(await until(() => statuses.findLast(s => s.devices === 5), 20_000)));

// Changes on the hub side reach the controller (held values settle after 20 s).
domain.Room[0].CalculatedTemperature = 230;
const seen = await until(async () => (await tUpper.getLocalTemperatureAttribute(true)) === 2300, 40_000);
check("temperature change on the hub reaches the controller", !!seen);

// A restart (reboot, upgrade) must keep the pairing.
let code = await stopBridge(bridge);
check("bridge stops cleanly", code === 0, `exit ${code}`);
check("no storage left unclosed", !bridgeLog.includes("was not closed"));
const before = statuses.length;
bridge = startBridge();
bridge.stdin.write(JSON.stringify({ url: `http://127.0.0.1:${PORT}`, secret: SECRET }) + "\n");
// This time the guard is on: its sensor must be there before the bridge goes online.
bridge.stdin.write(JSON.stringify({ alert: { enabled: true, on: false } }) + "\n");
const again = await until(() => statuses.slice(before).find(s => s.devices >= 5), 60_000);
check("after a restart the Heating alert is there from the start", again?.devices === 6, `${again?.devices} devices`);
check("after a restart the bridge is still paired", again?.commissioned === true && again?.fabrics?.length === 1);
domain.Room[1].CalculatedTemperature = 185;
const reconnected = await until(async () => {
    try {
        return (await tGround.getLocalTemperatureAttribute(true)) === 1850;
    } catch {
        return false;
    }
}, 90_000);
check("controller reconnects after the restart without re-pairing", !!reconnected);
patches.length = 0;
await hwOnOff.on();
p = await until(() => patches.find(x => x.path === "/data/domain/HotWater/2"));
check("commands work after the restart", p?.data.RequestOverride.SetPoint === 1100);

// Unpairing
await node.decommission();
const unpaired = await until(() => statuses.findLast(s => s.commissioned === false && statuses.indexOf(s) > statuses.indexOf(paired)), 15_000);
check("removing the bridge from the controller is reported", !!unpaired);

// Shutdown: closing stdin stops the bridge.
await controller.close();
code = await stopBridge(bridge);
check("bridge exits when the dashboard goes away", code === 0, `exit ${code}`);

mock.close();
rmSync(storage, { recursive: true, force: true });
if (failures) console.log("\n--- bridge log ---\n" + bridgeLog.split("\n").slice(-40).join("\n"));
console.log(failures ? `\n${failures} FAILED` : "\nALL PASSED");
process.exit(failures ? 1 : 0);
