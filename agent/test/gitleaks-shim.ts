// issue #1597 M2 — a gitleaks STAND-IN for tests. Every overlay-less checkpoint publish is now
// secret-scanned first and an untrusted scan publishes nothing (fail-closed), so a test host
// without gitleaks (the CI test-agent runner) would silently stop publishing. Test GitCaches pass
// `{ gitleaksBin: defaultGitleaksShim() }`; a test that wants the real binary or a
// finding/failing scanner overrides it.
//
// The shim is a small node script (absolute shebang, so PATH need not carry node) modelling the
// MEASURED behaviour of gitleaks v8.30.1:
//  - git mode walks the `--log-opts` revisions (space-separated, `^` floors honoured) and COUNTS
//    only commits whose `git log -p` has a textual hunk in a non-deleted file — reproduced with
//    `git log --numstat --diff-filter=d` — and neither diffs nor counts merges; it reports a finding
//    for any ADDED line carrying a GitHub-PAT-shaped value (so ordinary fixtures scan clean);
//  - stdin mode scans a diff fed on stdin and prints `scanned ~<bytes fed> bytes`.
// Modes: `detect` (the above), `fail` (exits nonzero: an instrument failure), `slowclean` (sleeps
// `sleepMs` IGNORING `--timeout` — as git mode does — then exits 0 with a clean report and a
// matching "N commits scanned": a silent partial scan our deadline must catch), `stdinlie` (as
// `detect`, but stdin mode reports one byte fewer than it was fed). Every call is
// appended to `<shim>.calls` as "<mode> <log-opts>".

import fs from "node:fs";
import os from "node:os";
import path from "node:path";

export function writeGitleaksShim(
  dir: string,
  mode: "detect" | "fail" | "slowclean" | "stdinlie",
  opts: { sleepMs?: number } = {},
): string {
  const shim = path.join(dir, `gitleaks-shim-${mode}-${opts.sleepMs ?? 0}`);
  fs.writeFileSync(
    shim,
    `#!${process.execPath}
const { execFileSync } = require("child_process");
const fs = require("fs");
const a = process.argv.slice(2);
const opt = (name) => (a.indexOf(name) >= 0 ? a[a.indexOf(name) + 1] : undefined);
const mode = ${JSON.stringify(mode)};
fs.appendFileSync(${JSON.stringify(`${shim}.calls`)}, a[0] + " " + (opt("--log-opts") || "-") + "\\n");
if (mode === "fail") { process.stderr.write("fatal: instrument broke\\n"); process.exit(2); }
const report = opt("--report-path");
const re = new RegExp("gh" + "p_[A-Za-z0-9]{36}");
const findings = [];
const scanDiff = (text, commit) => {
  let file = "";
  for (const line of text.split("\\n")) {
    if (line.startsWith("+++ b/")) file = line.slice(6);
    else if (line.startsWith("+") && !line.startsWith("+++") && re.test(line)) {
      findings.push({ File: file, StartLine: 1, Commit: commit, RuleID: "github-pat", Secret: "REDACTED" });
    }
  }
};
if (a[0] === "stdin") {
  // Like gitleaks stdin: EVERY line fed is content (the caller feeds only added lines).
  const text = fs.readFileSync(0);
  for (const line of text.toString("utf8").split("\\n")) {
    if (re.test(line)) findings.push({ File: "", StartLine: 1, Commit: "", RuleID: "github-pat", Secret: "REDACTED" });
  }
  fs.writeFileSync(report, JSON.stringify(findings));
  const n = mode === "stdinlie" ? text.length - 1 : text.length;
  process.stderr.write("INF scanned ~" + n + " bytes (" + n + " bytes) in 1ms\\n");
  return;
}
const src = a[1];
const revs = opt("--log-opts").split(" ").filter(Boolean);
const git = (args) => execFileSync("git", ["-C", src, ...args], { encoding: "utf8", maxBuffer: 1 << 28 });
const numstat = git(["log", "--no-merges", "--format=%x00%H", "--numstat", "--diff-filter=d", ...revs]);
const counted = numstat.split("\\0").slice(1).filter((b) =>
  b.split("\\n").slice(1).some((l) => { const m = /^(\\d+)\\t(\\d+)\\t/.exec(l); return m && Number(m[1]) + Number(m[2]) > 0; }),
).length;
const finish = () => {
  if (mode === "detect" || mode === "stdinlie") {
    for (const c of git(["rev-list", "--no-merges", ...revs]).split("\\n").filter(Boolean)) {
      scanDiff(git(["show", "--format=", "--unified=0", c]), c);
    }
  }
  fs.writeFileSync(report, JSON.stringify(findings));
  process.stderr.write("INF " + counted + " commits scanned.\\n");
  process.exit(0);
};
if (mode === "slowclean") setTimeout(finish, ${opts.sleepMs ?? 0});
else finish();
`,
    { mode: 0o755 },
  );
  return shim;
}

/** The scan ranges a shim was called with, in order. */
export const shimCalls = (shim: string): string[] =>
  fs.existsSync(`${shim}.calls`) ? fs.readFileSync(`${shim}.calls`, "utf8").split("\n").filter(Boolean) : [];

let defaultShim: string | undefined;

/** One process-wide detecting shim (clean on ordinary fixtures), removed when the process exits. */
export function defaultGitleaksShim(): string {
  if (defaultShim && fs.existsSync(defaultShim)) return defaultShim;
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-gitleaks-shim-"));
  process.once("exit", () => fs.rmSync(dir, { recursive: true, force: true }));
  defaultShim = writeGitleaksShim(dir, "detect");
  return defaultShim;
}
