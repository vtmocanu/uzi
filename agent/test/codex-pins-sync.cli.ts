// `task codex-pins:sync`: rewrite every registered Codex pin site from the lock (#1565).
import { fileURLToPath } from "node:url";
import { syncCodexPins } from "./codex-pins.js";

const root = fileURLToPath(new URL("../../", import.meta.url));
try {
  const written = syncCodexPins(root);
  console.log(written.length ? `codex-pins:sync rewrote ${written.length} file(s):\n${written.join("\n")}` : "codex-pins:sync: every pin already matches the lock");
} catch (error) {
  console.error(`codex-pins:sync: ${error instanceof Error ? error.message : String(error)}`);
  process.exitCode = 1;
}
