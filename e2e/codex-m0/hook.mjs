// Only invoked by the credential-free fixture, with its owned log path.
import { appendFile } from "node:fs/promises";

const [mode, log] = process.argv.slice(2);
let raw = "";
for await (const chunk of process.stdin) {
  raw += chunk;
  if (raw.length > 1024 * 1024) throw new Error("M0 hook input exceeds limit");
}
const input = JSON.parse(raw);
await appendFile(log, `${JSON.stringify({
  mode, event: input.hook_event_name, tool: input.tool_name,
  input: input.tool_input,
})}\n`);
if (mode === "deny" || (mode === "deny-marker" && raw.includes("stdin-marker"))) {
  process.stderr.write("M0 deterministic hook denial\n");
  process.exitCode = 2;
} else if (mode === "malformed") {
  process.stdout.write('{"hookSpecificOutput":');
} else if (mode === "timeout") {
  // Longer than the configured one-second hook limit. No subprocesses.
  await new Promise((resolve) => setTimeout(resolve, 3000));
} else if (mode !== "allow" && mode !== "deny-marker") {
  throw new Error("unknown M0 hook mode");
}
