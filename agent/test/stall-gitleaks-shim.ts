// issue #4258 — a gitleaks stand-in that can be told, by a mode file the test flips, to STALL like a
// real long-running child. In `pass` mode it delegates to a `detect` shim (see gitleaks-shim.ts) so
// the first tick can arm the mid-run trigger; in `stall-git` / `stall-stdin` mode the matching
// invocation (git range scan, or the stdin path scan) records its pid in `<shim>.pids` and idles
// for `stallMs` with no release hook: only a kill (or the test's own cleanup) ends it.

import { AsyncResource } from "node:async_hooks";
import fs from "node:fs";
import path from "node:path";
import { writeGitleaksShim } from "./gitleaks-shim.js";

export type StallMode = "pass" | "stall-git" | "stall-stdin";

export interface StallShim {
  readonly bin: string;
  setMode: (mode: StallMode) => void;
  /** Pids of every invocation that stalled, in spawn order. */
  pids: () => number[];
}

export function writeStallShim(dir: string, opts: { stallMs?: number } = {}): StallShim {
  const base = writeGitleaksShim(dir, "detect");
  const bin = path.join(dir, "gitleaks-shim-stall");
  const modeFile = `${bin}.mode`;
  const pidFile = `${bin}.pids`;
  fs.writeFileSync(modeFile, "pass");
  fs.writeFileSync(
    bin,
    `#!${process.execPath}
const fs = require("fs");
const { spawnSync } = require("child_process");
const a = process.argv.slice(2);
const kind = a[0] === "stdin" ? "stdin" : "git";
const mode = fs.readFileSync(${JSON.stringify(modeFile)}, "utf8").trim();
if (mode === "stall-" + kind) {
  fs.appendFileSync(${JSON.stringify(pidFile)}, process.pid + "\\n");
  process.stdin.resume();
  setTimeout(() => process.exit(3), ${opts.stallMs ?? 120_000});
} else {
  const r = spawnSync(${JSON.stringify(base)}, a, { stdio: "inherit" });
  process.exit(r.status === null ? 1 : r.status);
}
`,
    { mode: 0o755 },
  );
  return {
    bin,
    setMode: (mode) => fs.writeFileSync(modeFile, mode),
    pids: () =>
      fs.existsSync(pidFile)
        ? fs.readFileSync(pidFile, "utf8").split("\n").filter(Boolean).map(Number)
        : [],
  };
}

export interface TickDriver {
  /** RunnerOptions that arm the mid-turn tick on a fake clock the test fires by hand. */
  options: {
    checkpointIntervalMs: number;
    checkpointTickIntervalMs: number;
    setTimer: (cb: () => void, ms: number) => () => void;
    setTickTimer: (cb: () => void, ms: number) => () => void;
    now: () => number;
    checkpointTestHooks: {
      onTickOutcome: (outcome: string) => void;
      tickKillGraceMs: number;
      retryBackoffMs: readonly [number, number];
      scanDeadlineMs: number;
      tickSpawn: { onSpawn: (pid: number) => void };
    };
  };
  /** Pass the checkpoint time gate and fire one tick; resolves to its outcome. */
  fire: () => Promise<string>;
  /** Pids of every child the runner's tick spawner started (the tick's and the seam's). */
  pids: number[];
}

export function makeTickDriver(opts: { scanDeadlineMs: number }): TickDriver {
  let tickCb: (() => void) | undefined;
  let fakeNow = 0;
  const outcomes: string[] = [];
  const waiters: Array<{ n: number; resolve: (o: string) => void }> = [];
  const pids: number[] = [];
  const onOutcome = (o: string): void => {
    outcomes.push(o);
    for (const w of waiters.slice()) {
      if (outcomes.length > w.n) {
        waiters.splice(waiters.indexOf(w), 1);
        w.resolve(outcomes[w.n]!);
      }
    }
  };
  return {
    pids,
    options: {
      checkpointIntervalMs: 1_000,
      checkpointTickIntervalMs: 424_242,
      setTimer: (cb, ms) => {
        const t = setTimeout(cb, ms);
        t.unref?.();
        return () => clearTimeout(t);
      },
      // Like a real setTimeout, the callback runs in the async context that ARMED it.
      setTickTimer: (cb) => {
        const bound = AsyncResource.bind(cb);
        tickCb = bound;
        return () => {
          if (tickCb === bound) tickCb = undefined;
        };
      },
      now: () => fakeNow,
      checkpointTestHooks: {
        onTickOutcome: onOutcome,
        tickKillGraceMs: 300,
        retryBackoffMs: [0, 0],
        scanDeadlineMs: opts.scanDeadlineMs,
        tickSpawn: { onSpawn: (pid) => { pids.push(pid); } },
      },
    },
    fire: () => {
      fakeNow += 1_001;
      const n = outcomes.length;
      const cb = tickCb;
      if (!cb) return Promise.reject(new Error("the tick timer is not armed"));
      cb();
      return outcomes.length > n
        ? Promise.resolve(outcomes[n]!)
        : new Promise<string>((resolve) => waiters.push({ n, resolve }));
    },
  };
}
