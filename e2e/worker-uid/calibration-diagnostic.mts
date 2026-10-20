// Node assert diagnostic calibration: measurement only, no thresholds. Launched only by
// calibration.py (measure_diagnostics) as `calibration-diagnostic.mts <workload>`.
// The parent spawns a CHILD node running the workload so an OOM kill of the child is
// observable, then prints exactly one compact JSON result between marker lines.
//
// The GUARDED leg (`guarded-many-mismatch`) runs the same many-mismatch workload as a guarded
// command through the real Claude command lane, launcher, reader and monitor
// (calibration-claude-guard.mts) and prints one compact JSON measurement between the same markers.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import fs from "node:fs";
import { performance } from "node:perf_hooks";
import { isDeepStrictEqual } from "node:util";

export const BEGIN = "UZI_NODE_DIAGNOSTIC_BEGIN";
export const END = "UZI_NODE_DIAGNOSTIC_END";
export const WORKLOADS = ["many-mismatch", "equivalent", "bounded-diagnostic"] as const;
export type Workload = (typeof WORKLOADS)[number];
export const GUARDED_WORKLOAD = "guarded-many-mismatch";
// Pairs per array; every second pair mismatches in the mismatching workloads.
export const PAIRS = 200_000;
export const CHILD_DEADLINE_MS = 20_000;
const CHILD_STDOUT_BYTES = 4096;

export function buildPairs(count: number, mismatchEvery: number | null): [number, string][] {
  return Array.from({ length: count }, (_, i): [number, string] =>
    [i, mismatchEvery !== null && i % mismatchEvery === 0 ? "changed-" + i : "value-" + i]);
}

export interface ChildResult {
  outcome: "passed" | "assertion" | "other_error";
  messageBytes: number | null;
  mismatches: number | null;
  maxRssKb: number;
  ms: number;
}

// Runs in the child. Never prints or returns the assertion message, only its byte length.
export function runWorkload(workload: Workload, pairs = PAIRS): ChildResult {
  const started = performance.now();
  const actual = buildPairs(pairs, null);
  const expected = buildPairs(pairs, workload === "equivalent" ? null : 2);
  let outcome: ChildResult["outcome"] = "passed";
  let messageBytes: number | null = null;
  let mismatches: number | null = null;
  if (workload === "bounded-diagnostic") {
    // Bounded alternative: a boolean compare, a mismatch count and a fixed short message.
    if (!isDeepStrictEqual(actual, expected)) {
      mismatches = 0;
      for (let i = 0; i < pairs; i++) if (!isDeepStrictEqual(actual[i], expected[i])) mismatches++;
      try { assert.fail(`arrays differ in ${mismatches} of ${pairs} pairs`); } catch (error) {
        outcome = "assertion";
        messageBytes = Buffer.byteLength(String((error as Error).message));
      }
    }
  } else {
    try { assert.deepStrictEqual(actual, expected); } catch (error) {
      if (error instanceof assert.AssertionError) {
        outcome = "assertion";
        // Reading .message forces any lazily generated diff.
        messageBytes = Buffer.byteLength(String(error.message));
      } else outcome = "other_error";
    }
  }
  return { outcome, messageBytes, mismatches, maxRssKb: process.resourceUsage().maxRSS, ms: Math.round(performance.now() - started) };
}

function readText(file: string): string | null {
  try { return fs.readFileSync(file, "utf8").slice(0, 4096); } catch { return null; }
}
function oomKillCount(events: string | null): number | null {
  const match = events?.match(/^oom_kill (\d+)$/m);
  return match ? Number(match[1]) : null;
}

async function parent(workload: Workload) {
  const started = performance.now();
  const child = spawn(process.execPath, [...process.execArgv, new URL(import.meta.url).pathname, "--child", workload],
    { stdio: ["ignore", "pipe", "ignore"] });
  let stdout = "";
  child.stdout.on("data", (chunk: Buffer) => { if (stdout.length < CHILD_STDOUT_BYTES) stdout += chunk.toString("utf8"); });
  let timedOut = false;
  const timer = setTimeout(() => { timedOut = true; child.kill("SIGKILL"); }, CHILD_DEADLINE_MS);
  const [code, signal] = await new Promise<[number | null, NodeJS.Signals | null]>((resolve) => {
    child.once("close", (c, s) => resolve([c, s]));
    child.once("error", () => resolve([null, null]));
  });
  clearTimeout(timer);
  let reported: ChildResult | null = null;
  try {
    const line = stdout.trim().split("\n").pop() ?? "";
    reported = line ? JSON.parse(line) as ChildResult : null;
  } catch { reported = null; }
  const events = readText("/sys/fs/cgroup/memory.events");
  const peak = Number(readText("/sys/fs/cgroup/memory.peak")?.trim());
  const result = {
    schemaVersion: 1, workload, nodeVersion: process.version, pairs: PAIRS,
    childDeadlineMs: CHILD_DEADLINE_MS,
    child: { exitCode: code, signal, timedOut, wallMs: Math.round(performance.now() - started), reported },
    cgroup: { memoryPeakBytes: Number.isFinite(peak) ? peak : null, oomKill: oomKillCount(events) },
  };
  console.log(BEGIN);
  console.log(JSON.stringify(result));
  console.log(END);
}

// The guarded leg: the child command line is this same file's `--child many-mismatch` mode.
async function guarded() {
  const { measureGuardedLeg } = await import("./calibration-claude-guard.mjs");
  const quote = (value: string) => "'" + value.replaceAll("'", "'\\''") + "'";
  const command = [process.execPath, ...process.execArgv, new URL(import.meta.url).pathname, "--child", "many-mismatch"].map(quote).join(" ");
  const limitBytes = Number(readText("/sys/fs/cgroup/memory.max")?.trim());
  const scratch = process.env["TMPDIR"] ?? "/tmp";
  const worktree = scratch + "/guarded-worktree";
  fs.mkdirSync(worktree, { recursive: true });
  const env = { PATH: "/usr/local/bin:/usr/bin:/bin", LANG: "C", HOME: process.env["HOME"] ?? scratch, TMPDIR: scratch };
  const measured = await measureGuardedLeg({ command, limitBytes, worktree, env });
  const result = { schemaVersion: 1, workload: GUARDED_WORKLOAD, nodeVersion: process.version, pairs: PAIRS,
    childWorkload: "many-mismatch", ...measured };
  console.log(BEGIN);
  console.log(JSON.stringify(result));
  console.log(END);
}

function isWorkload(value: unknown): value is Workload {
  return (WORKLOADS as readonly unknown[]).includes(value);
}

if (process.argv[1]?.endsWith("calibration-diagnostic.mts")) {
  const args = process.argv.slice(2);
  if (args[0] === "--child") {
    if (!isWorkload(args[1])) throw new Error("unknown workload");
    console.log(JSON.stringify(runWorkload(args[1])));
  } else if (args[0] === GUARDED_WORKLOAD) {
    await guarded();
  } else {
    if (!isWorkload(args[0])) throw new Error("unknown workload");
    await parent(args[0]);
  }
}
