// argv[2]: "throw" (thread throws at load), "missing" (nonexistent file) or "sync"
// (constructor throws synchronously). Keeps main alive briefly, calls stop() twice
// after the failure, then prints `done`.
import { startMainWatchdog } from "../../src/main-watchdog.js";

const mode = process.argv[2];
const threadUrl =
  mode === "throw" ? new URL("./main-watchdog-throwing-thread.mjs", import.meta.url)
  : mode === "missing" ? new URL("./main-watchdog-does-not-exist.mjs", import.meta.url)
  : "relative-path-is-rejected-synchronously.mjs";

const watchdog = startMainWatchdog({ threadUrl });
await new Promise<void>((resolve) => setTimeout(resolve, 1500));
await watchdog.stop();
await watchdog.stop();
process.stdout.write("done\n");
