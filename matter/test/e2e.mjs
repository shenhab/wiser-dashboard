// End-to-end test for wiser-matter, with no real hub involved:
//   mock dashboard /data/ API  <-  wiser-matter (compiled)  <-  Matter controller
// The controller pairs over the local network like Google Home would, then
// reads the rooms and sends commands; the mock records what reaches the "hub".
//
//   node test/e2e.mjs path/to/wiser-matter
import { Environment, Logger, LogLevel } from "@matter/main";
import { BridgedDeviceBasicInformation, GeneralCommissioning, OnOff, Thermostat } from "@matter/main/clusters";
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
            res.writeHead(200, { "Content-Type": "application/json" }).end(JSON.stringify(domain));
            return;
        }
        const m = req.url.match(/^\/data\/domain\/(Room|HotWater)\/(\d+)$/);
        if (req.method === "PATCH" && m) {
            const data = JSON.parse(body);
            patches.push({ path: req.url, data });
            const item = domain[m[1]].find(x => x.id === Number(m[2]));
            const ov = data.RequestOverride;
            if (m[1] === "Room") item.CurrentSetPoint = ov.SetPoint;
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
// First a wrong secret, to check it is refused, then the right one.
bridge.stdin.write(JSON.stringify({ url: `http://127.0.0.1:${PORT}`, secret: "wrong" }) + "\n");
await until(() => unauthorized > 0, 10_000);
check("wrong secret is refused by the API", unauthorized > 0);
bridge.stdin.write(JSON.stringify({ secret: SECRET }) + "\n");

const first = await until(() => statuses.find(s => s.devices >= 3), 60_000);
check("bridge starts and reports pairing codes", !!first?.manualCode, first && `manual ${first.manualCode}, ${first.devices} devices`);
if (!first) {
    console.log(bridgeLog);
    process.exit(1);
}
check("bridge starts unpaired", first.commissioned === false);

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

const devices = node.getDevices().flatMap(d => [d, ...d.getChildEndpoints()]);
async function byName(name) {
    for (const d of devices) {
        const info = d.getClusterClient(BridgedDeviceBasicInformation.Cluster);
        if (info && (await info.getNodeLabelAttribute()) === name) return d;
    }
}
const upper = await byName("Upper Floor");
const ground = await byName("Ground Floor");
const hw = await byName("Hot water");
check("rooms and hot water appear as devices", !!(upper && ground && hw));

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
const again = await until(() => statuses.slice(before).find(s => s.devices >= 3), 60_000);
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
