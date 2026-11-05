// Starts the watchdog and returns without stop(): the process must still exit.
import { startMainWatchdog } from "../../src/main-watchdog.js";

startMainWatchdog({ beatMs: 25, pollMs: 25, reportIntervalMs: 100 });
