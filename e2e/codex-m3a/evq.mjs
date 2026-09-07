// PRD #1156 M3a — tiny NDJSON event query helper for the container controls.
//
// launch-cli (agent/src/codex/launch-cli.ts) writes one JSON object per line to stderr
// (`ready`, `final`, `spec-error`, `launch-failed`). This reads such a file, selects the
// LAST line whose `.event` equals the requested event, walks a dotted path into it and
// prints the value (arrays joined by spaces). It exists so the shell harness never has to
// parse JSON by hand and never has to reach for a /nix jq the worker uid should not run;
// node is on the worker PATH by absolute path.
//
//   node evq.mjs <file> <event> [dotted.path]
//
// Exit 3 when the requested event is absent (so the caller can distinguish "no such
// event" from an empty value).

import { readFileSync } from "node:fs";

const [, , file, event, path] = process.argv;
if (!file || !event) {
  process.stderr.write("usage: evq.mjs <file> <event> [dotted.path]\n");
  process.exit(2);
}

let obj;
for (const line of readFileSync(file, "utf8").split("\n")) {
  if (!line) continue;
  try {
    const parsed = JSON.parse(line);
    if (parsed && parsed.event === event) obj = parsed;
  } catch {
    /* non-JSON diagnostic line — ignore */
  }
}

if (obj === undefined) {
  process.stderr.write(`evq: no '${event}' event in ${file}\n`);
  process.exit(3);
}

let value = obj;
if (path) {
  for (const key of path.split(".")) {
    value = value == null ? undefined : value[key];
  }
}

if (Array.isArray(value)) process.stdout.write(`${value.join(" ")}\n`);
else if (value === undefined || value === null) process.stdout.write("\n");
else process.stdout.write(`${String(value)}\n`);
