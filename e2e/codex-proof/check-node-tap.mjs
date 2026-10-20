#!/usr/bin/env node
// Strict verdict over one `node --test --test-reporter=tap` stream (#3148).
//
// `node --test` exits 0 when a test or a whole `describe` is skipped, so a packaged proof
// whose suites skip (a renamed binary path, a missing env switch) reads green. This checker
// is the strict-mode half of the Codex proof runners: it passes only a COMPLETE summary that
// executed at least --min-tests tests, all of them passing, with nothing failed, cancelled,
// skipped or todo. A skipped `describe` never reaches the `# skipped` count (its tests are not
// counted at all), so every `# SKIP` / `# TODO` test-point directive is rejected as well.
//
//   node check-node-tap.mjs <tap-file> [--min-tests N]
//
// Exit: 0 = strict pass; 1 = the stream violates the strict contract; 2 = usage or unreadable
// input (the instrument itself is broken).
import { readFileSync } from "node:fs";
import { pathToFileURL } from "node:url";

const SUMMARY_KEYS = ["tests", "suites", "pass", "fail", "cancelled", "skipped", "todo", "duration_ms"];

// Node escapes `#` and `\` inside test names and forwarded console output, so an unescaped
// ` # SKIP` / ` # TODO` on a test-point line is a real directive, never test-controlled text.
const TEST_POINT = /^\s*(not )?ok \d+ - /;
const DIRECTIVE = /(?:^|[^\\])# (SKIP|TODO)(?:\s|$)/;

export function checkTap(text, minTests = 1) {
  const lines = text.split(/\r?\n/);
  const problems = [];

  // The run's own summary is the LAST unindented `# tests N` line followed by the fixed keys.
  let start = -1;
  for (let i = lines.length - 1; i >= 0; i--) {
    if (/^# tests \d+$/.test(lines[i])) { start = i; break; }
  }
  if (start < 0) {
    return { ok: false, problems: ["no top-level `# tests N` summary (incomplete or non-TAP output)"] };
  }
  const summary = {};
  for (let k = 0; k < SUMMARY_KEYS.length; k++) {
    const key = SUMMARY_KEYS[k];
    const m = new RegExp(`^# ${key} (\\d+(?:\\.\\d+)?)$`).exec(lines[start + k] ?? "");
    if (!m) {
      return { ok: false, problems: [`summary incomplete: expected \`# ${key} N\` at line ${start + k + 1}`] };
    }
    summary[key] = Number(m[1]);
  }
  if (!lines.slice(0, start).some((l) => /^1\.\.\d+$/.test(l))) {
    problems.push("no top-level TAP plan line (1..N)");
  }

  if (summary.tests < minTests) problems.push(`executed ${summary.tests} tests, need at least ${minTests}`);
  for (const key of ["fail", "cancelled", "skipped", "todo"]) {
    if (summary[key] !== 0) problems.push(`${key} ${summary[key]}`);
  }
  if (summary.pass !== summary.tests) problems.push(`pass ${summary.pass} != tests ${summary.tests}`);

  for (const [i, line] of lines.entries()) {
    if (!TEST_POINT.test(line)) continue;
    if (line.trimStart().startsWith("not ok")) problems.push(`line ${i + 1}: ${line.trim()}`);
    const d = DIRECTIVE.exec(line);
    if (d) problems.push(`line ${i + 1}: ${d[1]} directive: ${line.trim()}`);
  }
  return { ok: problems.length === 0, summary, problems };
}

function main(argv) {
  const args = argv.slice(2);
  let minTests = 1;
  const files = [];
  for (let i = 0; i < args.length; i++) {
    if (args[i] === "--min-tests") {
      minTests = Number(args[++i]);
      if (!Number.isInteger(minTests) || minTests < 1) {
        console.error("check-node-tap: --min-tests needs an integer >= 1");
        return 2;
      }
    } else {
      files.push(args[i]);
    }
  }
  if (files.length !== 1) {
    console.error("usage: check-node-tap.mjs <tap-file> [--min-tests N]");
    return 2;
  }
  let text;
  try {
    text = readFileSync(files[0], "utf8");
  } catch (err) {
    console.error(`check-node-tap: cannot read ${files[0]}: ${err.message}`);
    return 2;
  }
  const r = checkTap(text, minTests);
  if (r.ok) {
    console.log(`check-node-tap: OK tests=${r.summary.tests} pass=${r.summary.pass} (${files[0]})`);
    return 0;
  }
  console.error(`check-node-tap: FAIL (${files[0]})`);
  for (const p of r.problems) console.error(`  - ${p}`);
  return 1;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.exitCode = main(process.argv);
}
