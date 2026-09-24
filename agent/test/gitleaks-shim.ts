// issue #1597 M2 — a gitleaks STAND-IN for tests. Every overlay-less checkpoint publish is now
// secret-scanned first and an untrusted scan publishes nothing (fail-closed), so a test host
// without gitleaks (the CI test-agent runner) would silently stop publishing. Test GitCaches pass
// `{ gitleaksBin: defaultGitleaksShim() }`; a test that wants the real binary or a
// finding/failing scanner overrides it.
//
// The shim is a small node script (absolute shebang, so PATH need not carry node). It walks the
// range with git plumbing and reports a finding for any ADDED line carrying a GitHub-PAT-shaped
// value (so on ordinary fixtures it is a clean, trusted scan), writes the gitleaks-shaped JSON
// report and the `N commits scanned` stderr line scanIsTrustworthy reads. `fail` mode exits
// nonzero (an instrument failure). Every call is appended to `<shim>.calls`.

import fs from "node:fs";
import os from "node:os";
import path from "node:path";

export function writeGitleaksShim(dir: string, mode: "detect" | "fail"): string {
  const shim = path.join(dir, `gitleaks-shim-${mode}`);
  fs.writeFileSync(
    shim,
    `#!${process.execPath}
const { execFileSync } = require("child_process");
const fs = require("fs");
const a = process.argv.slice(2);
fs.appendFileSync(${JSON.stringify(`${shim}.calls`)}, a[a.indexOf("--log-opts") + 1] + "\\n");
if (${JSON.stringify(mode)} === "fail") { process.stderr.write("fatal: instrument broke\\n"); process.exit(2); }
const src = a[1];
const range = a[a.indexOf("--log-opts") + 1];
const report = a[a.indexOf("--report-path") + 1];
const git = (args) => execFileSync("git", ["-C", src, ...args], { encoding: "utf8" });
const commits = git(["rev-list", range]).split("\\n").filter(Boolean);
const re = new RegExp("gh" + "p_[A-Za-z0-9]{36}");
const findings = [];
for (const c of commits) {
  let file = "";
  for (const line of git(["show", "--format=", "--unified=0", c]).split("\\n")) {
    if (line.startsWith("+++ b/")) file = line.slice(6);
    else if (line.startsWith("+") && !line.startsWith("+++") && re.test(line)) {
      findings.push({ File: file, StartLine: 1, Commit: c, RuleID: "github-pat", Secret: "REDACTED" });
    }
  }
}
fs.writeFileSync(report, JSON.stringify(findings));
process.stderr.write("INF " + commits.length + " commits scanned.\\n");
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
