// Worker-thread half of the main-thread watchdog (issue #3896). Plain JS with only
// node: builtins so it loads without the tsx loader. Observe-only: it logs RSS and the
// main thread's heartbeat age straight to fd 2 (a synchronous write that does not
// depend on the possibly blocked main event loop) and never kills or aborts anything.
import fs from "node:fs";
import { performance } from "node:perf_hooks";
import { workerData } from "node:worker_threads";

const { buffer, pollMs, reportIntervalMs, stallMs } = workerData;
const counter = new Int32Array(buffer);

function emit(level, msg, fields) {
  try {
    fs.writeSync(2, JSON.stringify({ ts: new Date().toISOString(), level, msg, ...fields }) + "\n");
  } catch {
    // EAGAIN or a closed stderr: drop the line, never disturb the run.
  }
}

let last = Atomics.load(counter, 0);
let lastChange = performance.now();
let lastReport = lastChange;
let stalledSince = null;

setInterval(() => {
  const now = performance.now();
  const value = Atomics.load(counter, 0);
  if (value !== last) {
    last = value;
    lastChange = now;
    if (stalledSince !== null) {
      emit("warn", "worker main thread resumed", {
        rss_bytes: process.memoryUsage.rss(),
        stalled_ms: Math.round(now - stalledSince),
      });
      stalledSince = null;
    }
  }
  const age = now - lastChange;
  if (stalledSince === null && age > stallMs) {
    stalledSince = lastChange;
    emit("warn", "worker main thread stalled", {
      rss_bytes: process.memoryUsage.rss(),
      heartbeat_age_ms: Math.round(age),
    });
  }
  if (now - lastReport >= reportIntervalMs) {
    lastReport = now;
    emit("info", "worker main watchdog", {
      rss_bytes: process.memoryUsage.rss(),
      heartbeat_age_ms: Math.round(age),
    });
  }
}, pollMs);
