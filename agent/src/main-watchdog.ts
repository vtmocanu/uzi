// Main-thread watchdog (issue #3896). The main thread bumps a SharedArrayBuffer
// counter on a timer; a worker thread (main-watchdog-thread.mjs) polls it and writes
// periodic RSS / heartbeat-age lines plus stall and resume lines to fd 2, so a blocked
// event loop is still visible. Observe-only and failure-isolated: any failure of the
// thread is logged once and the run continues without it.
import fs from "node:fs";
import { Worker } from "node:worker_threads";

export const MAIN_WATCHDOG_DEFAULTS = {
  reportIntervalMs: 15_000,
  stallMs: 30_000,
  beatMs: 1_000,
  pollMs: 1_000,
} as const;

export interface MainWatchdogOptions {
  reportIntervalMs?: number;
  stallMs?: number;
  beatMs?: number;
  pollMs?: number;
  /** Test-only: the worker entry to load. */
  threadUrl?: URL | string;
}

export interface MainWatchdog {
  stop(): Promise<void>;
}

type UnavailableReason = "start_failed" | "error" | "exit";

export function startMainWatchdog(opts: MainWatchdogOptions = {}): MainWatchdog {
  const cfg = { ...MAIN_WATCHDOG_DEFAULTS, ...opts };
  const threadUrl = opts.threadUrl ?? new URL("./main-watchdog-thread.mjs", import.meta.url);
  let timer: NodeJS.Timeout | undefined;
  let worker: Worker | undefined;
  let stopping = false;
  let exited = false;
  let failed = false;
  let stopPromise: Promise<void> | undefined;

  const fail = (reason: UnavailableReason, exitCode?: number): void => {
    if (failed) return;
    failed = true;
    if (timer !== undefined) clearInterval(timer);
    timer = undefined;
    const line: Record<string, unknown> = {
      ts: new Date().toISOString(), level: "warn", msg: "worker main watchdog unavailable", reason,
    };
    if (reason === "exit" && typeof exitCode === "number") line.exit_code = exitCode;
    try {
      fs.writeSync(2, JSON.stringify(line) + "\n");
    } catch {
      // Best effort only.
    }
  };

  try {
    const buffer = new SharedArrayBuffer(Int32Array.BYTES_PER_ELEMENT);
    const counter = new Int32Array(buffer);
    timer = setInterval(() => { Atomics.add(counter, 0, 1); }, cfg.beatMs);
    timer.unref();
    // execArgv [] keeps the tsx loader and test preloads out of the thread (#3031: no
    // unrecorded esbuild child).
    const w = new Worker(threadUrl, {
      workerData: {
        buffer, pollMs: cfg.pollMs, reportIntervalMs: cfg.reportIntervalMs, stallMs: cfg.stallMs,
      },
      execArgv: [],
    });
    worker = w;
    w.on("error", () => { fail("error"); });
    w.on("exit", (code) => {
      exited = true;
      // Accepted race: a thread that exits on its own after stop() set `stopping` but before this event is not logged (an "error" event still is).
      if (!stopping) fail("exit", code);
    });
    w.unref();
  } catch {
    fail("start_failed");
  }

  return {
    stop(): Promise<void> {
      if (stopPromise) return stopPromise;
      stopping = true;
      if (timer !== undefined) clearInterval(timer);
      timer = undefined;
      const w = worker;
      stopPromise = (async () => {
        if (w === undefined || exited) return;
        try {
          await w.terminate();
        } catch {
          // Swallowed: stopping an observer must never fail the run.
        }
      })();
      return stopPromise;
    },
  };
}
