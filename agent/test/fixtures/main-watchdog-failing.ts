// argv[2]: "throw" (thread throws at load), "missing" (nonexistent file) or "sync"
// (constructor throws synchronously). Keeps main alive until the parent signals it saw
// the unavailable line, calls stop() twice after the failure, then prints `done`.
import { startMainWatchdog } from "../../src/main-watchdog.js";

const mode = process.argv[2];
const threadUrl =
  mode === "throw" ? new URL("./main-watchdog-throwing-thread.mjs", import.meta.url)
  : mode === "missing" ? new URL("./main-watchdog-does-not-exist.mjs", import.meta.url)
  : "relative-path-is-rejected-synchronously.mjs";

const watchdog = startMainWatchdog({ threadUrl });
// Wait for the parent to send a byte after it has observed the unavailable line
// (bounded fallback so a lost byte cannot hang the fixture).
await new Promise<void>((resolve) => {
  const fallback = setTimeout(resolve, 15_000);
  process.stdin.once("data", () => { clearTimeout(fallback); resolve(); });
});
await watchdog.stop();
await watchdog.stop();
process.stdout.write("done\n");
// The open stdin pipe would otherwise keep the process alive.
process.stdin.pause();
process.stdin.unref();
