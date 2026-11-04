// Starts the real watchdog with short intervals, then blocks the main thread
// synchronously on fd 0 until the parent sends one byte.
import fs from "node:fs";
import { startMainWatchdog } from "../../src/main-watchdog.js";

const watchdog = startMainWatchdog({ beatMs: 25, pollMs: 25, stallMs: 200, reportIntervalMs: 100 });
fs.writeSync(2, "ready\n");

const buf = Buffer.alloc(1);
for (;;) {
  try {
    if (fs.readSync(0, buf, 0, 1, null) >= 0) break;
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code !== "EAGAIN") throw err;
  }
}
fs.writeSync(2, "unblocked\n");
// Give the timers and the watchdog thread a chance to observe the resumed heartbeat.
await new Promise<void>((resolve) => setTimeout(resolve, 400));
await watchdog.stop();
process.exit(0);
