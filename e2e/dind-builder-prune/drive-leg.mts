// Driver for the #4291 builder-prune harness (e2e/dind-builder-prune/run.sh).
//
// Run as: cd agent && node --import tsx ../e2e/dind-builder-prune/drive-leg.mts <mode> [flags]
//
// It imports the REAL controller from agent/src and points it at an OWNED nested daemon:
//   real      the production argv through the real exec path (default spawn, /usr/bin/docker,
//             the default buildx plugin path, dockerExecEnvironment + DOCKER_BUILDKIT=1) over
//             TCP, then an abort mid-prune that must group-kill and settle within the bound.
//   pressure  the real controller under REAL df pressure of the nested data root. A TEST-ONLY
//             exec shim replaces only the process spawn: it asserts the exact production argv
//             against a literal copy kept in this file, then runs the scaled variant inside
//             the nested daemon. --switch off proves the leg is absent without its switch.
//
// Exit codes: 0 pass, 3 no relief observed (the designed failure of --switch off), 1 anything else.
// Nothing here touches the worker's own daemon: every docker call names the owned container
// (docker exec) or the owned daemon's address.

import { execFileSync, spawn } from "node:child_process";
import { readdirSync, readFileSync } from "node:fs";
import {
  createDindBuilderPrune,
  DIND_BUILDER_PRUNE_ARGV,
  type BuilderPruneExec,
  type BuilderPruneExecResult,
  type DindBuilderPruneOutcome,
} from "../../agent/src/dind-builder-prune.js";
import type { DindMeterSample } from "../../agent/src/dind-meter.js";
import { DindPruneGate, TICK_MS } from "../../agent/src/dind-prune.js";
import type { Logger } from "../../agent/src/log.js";

/** An independent literal copy of the one production argv (not derived from the product constant). */
const PRODUCTION_ARGV_LITERAL: readonly string[] = [
  "builder",
  "prune",
  "-f",
  "--filter",
  "type=regular",
  "--filter",
  "until=24h",
  "--max-used-space",
  "4GiB",
];

const EXIT_NO_RELIEF = 3;

function say(msg: string): void {
  console.log(`DRIVER: ${msg}`);
}

function sleepMs(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}

function parseFlags(argv: string[]): Map<string, string> {
  const flags = new Map<string, string>();
  for (let i = 0; i < argv.length; i += 2) {
    const k = argv[i];
    const v = argv[i + 1];
    if (!k?.startsWith("--") || v === undefined) throw new Error(`bad flag list near ${String(k)}`);
    flags.set(k.slice(2), v);
  }
  return flags;
}

function need(flags: Map<string, string>, name: string): string {
  const v = flags.get(name);
  if (!v) throw new Error(`missing --${name}`);
  return v;
}

/**
 * The production prune must never be pointed at the worker's own daemon: only an explicit
 * tcp:// address that differs from this process's DOCKER_HOST is accepted (no unix sockets).
 */
function safeDockerHost(flags: Map<string, string>): string {
  const host = need(flags, "docker-host");
  if (!host.startsWith("tcp://")) throw new Error(`refusing --docker-host ${host}: only tcp:// is allowed`);
  if (host === process.env["DOCKER_HOST"]) throw new Error(`refusing --docker-host ${host}: equals this process's DOCKER_HOST`);
  const hostname = new URL(host.replace(/^tcp:/, "http:")).hostname.replace(/^\[|\]$/g, "");
  if (hostname === "localhost" || hostname === "::1" || hostname.startsWith("127.") || hostname === "0.0.0.0") {
    throw new Error(`refusing --docker-host ${host}: loopback may be the worker's own daemon`);
  }
  return host;
}

interface Capture {
  lines: string[];
  fields: Array<{ msg: string; fields: Record<string, unknown> }>;
  logger: Logger;
}

function captureLogger(): Capture {
  const lines: string[] = [];
  const fields: Array<{ msg: string; fields: Record<string, unknown> }> = [];
  const emit =
    (level: string) =>
    (msg: string, f?: Record<string, unknown>): void => {
      const line = `${level} ${msg} ${f ? JSON.stringify(f) : ""}`.trim();
      lines.push(line);
      fields.push({ msg, fields: f ?? {} });
      console.log(`  [controller] ${line}`);
    };
  const logger: Logger = {
    debug: emit("debug"),
    info: emit("info"),
    warn: emit("warn"),
    error: emit("error"),
    addSecret(): void {},
    removeSecret(): void {},
    child: () => logger,
  };
  return { lines, fields, logger };
}

/** Real df of the nested data root, read synchronously through the owned container. */
function dfSample(container: string, user: string, dataPath: string): DindMeterSample {
  const out = execFileSync(
    "docker",
    ["exec", "-u", user, container, "stat", "-f", "-c", "%S %b %f %c %d", dataPath],
    { encoding: "utf8", timeout: 20_000 },
  );
  const [frsize, blocks, bfree, files, ffree] = out.trim().split(/\s+/).map(Number) as [number, number, number, number, number];
  if (![frsize, blocks, bfree, files, ffree].every(Number.isFinite)) throw new Error(`unparseable stat -f output: ${out}`);
  return {
    epochS: Math.floor(Date.now() / 1000),
    bytesUsed: (blocks - bfree) * frsize,
    bytesTotal: blocks * frsize,
    inodesUsed: files - ffree,
    inodesTotal: files,
  };
}

function pct(s: DindMeterSample): number {
  return Math.round((s.bytesUsed / s.bytesTotal) * 1000) / 10;
}

/** A real controller sleep would wait 5 s per poll for the after sample; keep the test quick. */
const fastSleep = (ms: number): Promise<void> => sleepMs(Math.min(ms, 1000));

type Tickable = { tick(signal?: AbortSignal): Promise<DindBuilderPruneOutcome> };

/** Ticks (distinct sample epochs, 1.1 s apart) until an attempt outcome; bounded. */
async function tickUntilAttempt(ctrl: Tickable, max: number): Promise<DindBuilderPruneOutcome> {
  const seen: string[] = [];
  for (let i = 0; i < max; i++) {
    const outcome = await ctrl.tick();
    seen.push(outcome);
    say(`tick ${i + 1} -> ${outcome}`);
    if (!["no-sample", "pending", "backoff", "below", "in-flight"].includes(outcome)) return outcome;
    await sleepMs(1100);
  }
  throw new Error(`no attempt outcome in ${max} ticks (${seen.join(",")})`);
}

/** Processes still holding a docker builder prune (read from /proc; never used to select a kill). */
function pruneClientsAlive(): string[] {
  const found: string[] = [];
  for (const entry of readdirSync("/proc")) {
    if (!/^\d+$/.test(entry)) continue;
    try {
      const cmd = readFileSync(`/proc/${entry}/cmdline`, "utf8").replaceAll("\u0000", " ");
      // docker/cli rewrites `builder` to `buildx` for the plugin child, so match the stable tail.
      if (cmd.includes(" prune") && cmd.includes("--max-used-space 4GiB")) found.push(`${entry}: ${cmd.trim()}`);
    } catch {
      // The process exited between readdir and read.
    }
  }
  return found;
}

const syntheticOver = (): DindMeterSample => ({
  epochS: Math.floor(Date.now() / 1000),
  bytesUsed: 90,
  bytesTotal: 100,
  inodesUsed: 1,
  inodesTotal: 100,
});

function newController(dockerHost: string, cap: Capture, extra: Record<string, unknown>) {
  return createDindBuilderPrune(
    {
      dindPruneEnabled: true,
      dindBuilderPruneEnabled: extra["switch"] !== false,
      dockerWiring: { dockerHost },
      workerName: "dbp-e2e",
    },
    {
      gate: new DindPruneGate(),
      log: cap.logger,
      sleep: fastSleep,
      ...(extra["readSample"] ? { readSample: extra["readSample"] as () => DindMeterSample } : {}),
      ...(extra["exec"] ? { exec: extra["exec"] as BuilderPruneExec } : {}),
    },
  );
}

async function realMode(flags: Map<string, string>): Promise<number> {
  const dockerHost = safeDockerHost(flags);
  say(`real exec path against ${dockerHost}; the sample is SYNTHETIC here (only the exec path is under test)`);
  say(`TICK_MS in production is ${TICK_MS}; this driver ticks every 1.1 s with distinct epochs`);

  // 1. completed through the real path.
  const cap1 = captureLogger();
  const ctrl1 = newController(dockerHost, cap1, { readSample: syntheticOver });
  if (!ctrl1) throw new Error("controller unexpectedly undefined");
  const outcome1 = await tickUntilAttempt(ctrl1, 6);
  say(`REAL-PATH production argv outcome=${outcome1}`);
  const completedLogged = cap1.lines.some((l) => l.includes("dind-builder-prune-completed"));
  if (outcome1 !== "completed" || !completedLogged) {
    say(`FAIL: real path outcome ${outcome1}, completed logged=${completedLogged}`);
    return 1;
  }
  say("PASS: real path completed (rc 0) with the default exec, /usr/bin/docker and the default plugin path");

  // 2. abort mid-prune: group kill and confirmed settlement within the bound.
  const cap2 = captureLogger();
  const ctrl2 = newController(dockerHost, cap2, { readSample: syntheticOver });
  if (!ctrl2) throw new Error("controller unexpectedly undefined");
  const first = await ctrl2.tick();
  say(`abort controller tick 1 -> ${first}`);
  await sleepMs(1100);
  const ac = new AbortController();
  const startedAt = Date.now();
  setTimeout(() => ac.abort(), 40);
  const outcome2 = await ctrl2.tick(ac.signal);
  const settledMs = Date.now() - startedAt;
  say(`ABORT of a just-started prune client, outcome=${outcome2} after ${settledMs} ms`);
  await sleepMs(500);
  const alive = pruneClientsAlive();
  const killedByAbort = cap2.fields.some(
    (f) => f.msg.startsWith("dind-builder-prune-unknown") && f.fields["kill_reason"] === "abort",
  );
  const unsettledLogged = cap2.lines.some((l) => l.includes("unsettled"));
  say(`ABORT evidence: kill_reason=abort logged=${killedByAbort}, unsettled logged=${unsettledLogged}, surviving prune clients=${alive.length}`);
  for (const a of alive) say(`  surviving: ${a}`);
  if (outcome2 !== "unknown" || !killedByAbort || unsettledLogged || alive.length > 0 || settledMs > 10_000) {
    say("FAIL: abort mid-prune did not group-kill and settle within the bound");
    return 1;
  }
  say("PASS: abort mid-prune: group kill, close observed within the 10 s settle bound, no -unsettled log, no surviving client");

  // 3. the daemon is not wedged by the killed client.
  const cap3 = captureLogger();
  const ctrl3 = newController(dockerHost, cap3, { readSample: syntheticOver });
  if (!ctrl3) throw new Error("controller unexpectedly undefined");
  const outcome3 = await tickUntilAttempt(ctrl3, 6);
  if (outcome3 !== "completed") {
    say(`FAIL: prune after the aborted client ended ${outcome3}`);
    return 1;
  }
  say("PASS: a fresh controller completed the production argv after the aborted client");
  return 0;
}

/** The TEST-ONLY exec shim: asserts the production argv, then runs the scaled variant in the nested daemon. */
function makeShim(flags: Map<string, string>): BuilderPruneExec {
  const container = need(flags, "container");
  const user = need(flags, "user");
  const sock = need(flags, "inner-sock");
  const floorS = need(flags, "floor-s");
  const cap = need(flags, "cap");
  const dockerHost = safeDockerHost(flags);
  return (argv, opts): Promise<BuilderPruneExecResult> => {
    if (JSON.stringify(argv) !== JSON.stringify(PRODUCTION_ARGV_LITERAL)) {
      throw new Error(`shim received a non-production argv: ${JSON.stringify(argv)}`);
    }
    if (JSON.stringify(argv) !== JSON.stringify(DIND_BUILDER_PRUNE_ARGV)) throw new Error("literal and product constant differ");
    if (opts.file !== "/usr/bin/docker") throw new Error(`unexpected spawn file ${opts.file}`);
    if (opts.env["DOCKER_BUILDKIT"] !== "1") throw new Error("DOCKER_BUILDKIT=1 missing from the exec env");
    if (opts.env["DOCKER_HOST"] !== dockerHost) throw new Error("DOCKER_HOST missing from the exec env");
    say("shim: received exactly the production argv; DOCKER_BUILDKIT=1 and DOCKER_HOST set");
    const scaled = ["builder", "prune", "-f", "--filter", "type=regular", "--filter", `until=${floorS}s`, "--max-used-space", cap];
    say(`TEST-ONLY scaled argv: docker ${scaled.join(" ")}`);
    return new Promise((resolve) => {
      const child = spawn(
        "docker",
        [
          "exec", "-u", user, container, "env", "-i",
          "PATH=/spike/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
          "HOME=/spike/home", "DOCKER_CONFIG=/spike/cfg", "DOCKER_BUILDKIT=1",
          "docker", "--host", sock, ...scaled,
        ],
        { stdio: ["ignore", "pipe", "pipe"] },
      );
      let stdout = "";
      let stderr = "";
      const timer = setTimeout(() => child.kill("SIGKILL"), 120_000);
      child.stdout.on("data", (c: Buffer) => (stdout += c.toString("utf8")));
      child.stderr.on("data", (c: Buffer) => (stderr += c.toString("utf8")));
      child.once("error", (e) => resolve({ kind: "spawn-error", error: String(e) }));
      child.once("close", (code, signal) => {
        clearTimeout(timer);
        say(`shim: scaled prune rc=${code} signal=${signal}`);
        for (const l of stdout.split("\n").slice(-4)) if (l.trim()) say(`  prune| ${l}`);
        if (stderr.trim()) say(`  prune-stderr| ${stderr.trim().slice(0, 300)}`);
        resolve({ kind: "exit", code, signal, stdout, stderr });
      });
    });
  };
}

async function pressureMode(flags: Map<string, string>): Promise<number> {
  const dockerHost = safeDockerHost(flags);
  const container = need(flags, "container");
  const user = need(flags, "user");
  const dataPath = need(flags, "data-path");
  const switchOn = (flags.get("switch") ?? "on") !== "off";
  const cap = captureLogger();
  const read = (): DindMeterSample => dfSample(container, user, dataPath);
  const ctrl = newController(dockerHost, cap, {
    switch: switchOn,
    readSample: read,
    ...(switchOn ? { exec: makeShim(flags) } : {}),
  });
  const before = read();
  say(`before: data root ${pct(before)}% bytes used (${before.bytesUsed} of ${before.bytesTotal})`);
  if (pct(before) < 85) {
    say("FAIL: fixture is not over the 85% high watermark");
    return 1;
  }

  if (!ctrl) {
    say("controller is undefined (UZI_DIND_BUILDER_PRUNE_ENABLED=false): the leg does not exist");
    await sleepMs(6000);
    const after = read();
    say(`after 6 s: data root ${pct(after)}%`);
    if (pct(after) < 75) {
      say("FAIL: pressure fell without the leg; the fixture is not isolating the leg");
      return 1;
    }
    say("FAIL(no relief): pressure is unchanged without the leg");
    return EXIT_NO_RELIEF;
  }

  const outcome = await tickUntilAttempt(ctrl, 8);
  const after = read();
  say(`outcome=${outcome}; after: data root ${pct(after)}%`);
  const loggedAfter = cap.fields.find((f) => f.msg.startsWith("dind-builder-prune-after"));
  const loggedPct = typeof loggedAfter?.fields["bytes_pct"] === "number" ? (loggedAfter.fields["bytes_pct"] as number) : undefined;
  say(`controller after-sample bytes_pct=${String(loggedPct)}`);
  if (outcome !== "completed") {
    say(`FAIL: outcome ${outcome}, not completed`);
    return 1;
  }
  if (!(pct(after) < 75) || loggedPct === undefined || !(loggedPct < 75)) {
    say("FAIL: post sample is not below 75%");
    return 1;
  }
  say("PASS: completed, post sample below 75% (own df and the controller's logged after-sample)");
  return 0;
}

async function main(): Promise<number> {
  const [mode, ...rest] = process.argv.slice(2);
  const flags = parseFlags(rest);
  if (mode === "real") return realMode(flags);
  if (mode === "pressure") return pressureMode(flags);
  throw new Error(`unknown mode ${String(mode)}`);
}

main().then(
  (code) => process.exit(code),
  (err: unknown) => {
    console.error(`DRIVER: FAIL: ${err instanceof Error ? err.stack ?? err.message : String(err)}`);
    process.exit(1);
  },
);
