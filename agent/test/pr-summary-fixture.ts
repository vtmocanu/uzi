// PRD #1798 M2: one shared pr_summary fixture, so the Claude path (scanSignals on an
// mcp__uzi__signal_done tool_use) and the Codex path (the signal_done dynamic tool through the
// CodexExecutor) are proven to yield the SAME claim shape from the same input (D13).
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

/** The raw `pr_summary` a lead passes, including members the parser must drop or trim. */
export const PR_SUMMARY_INPUT = {
  what: "  Runs now show a plain-English description on their pull request.  ",
  why: "Reviewers could not tell what a run changed without reading the diff.",
  changes: ["The PR body opens with a short summary.", "", 7, "  Verification lists only checks that ran. "],
  verification: [
    { command: "npm test", result: "pass" },
    { command: "task lint", result: "skipped" },
    { command: "", result: "pass" },
    { command: "tsc --noEmit", result: "fail" },
  ],
  scope_notes: [
    { kind: "deferred", text: "The editor pass lands in a later milestone." },
    { kind: "removed", text: "not a valid kind" },
  ],
  review_pointers: ["The clamp in signals.ts."],
  // The model can never supply the stamp: the parser ignores it.
  verifiedAtSha: "0".repeat(40),
};

/** What both harnesses must carry for PR_SUMMARY_INPUT, before the executor's HEAD stamp. */
export const PR_SUMMARY_EXPECTED = {
  what: "Runs now show a plain-English description on their pull request.",
  why: "Reviewers could not tell what a run changed without reading the diff.",
  changes: ["The PR body opens with a short summary.", "Verification lists only checks that ran."],
  verification: [
    { command: "npm test", result: "pass" },
    { command: "tsc --noEmit", result: "fail" },
  ],
  scope_notes: [{ kind: "deferred", text: "The editor pass lands in a later milestone." }],
  review_pointers: ["The clamp in signals.ts."],
};

/** A throwaway git repository with one commit. Returns its path and its HEAD sha. */
export function makeGitRepo(): { dir: string; head: string } {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-prsum-wt-"));
  const git = (...args: string[]): string =>
    execFileSync("git", ["-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", ...args], {
      encoding: "utf8",
    });
  git("init", "-q");
  fs.writeFileSync(path.join(dir, "a.txt"), "a\n");
  git("add", "a.txt");
  git("commit", "-q", "-m", "init");
  return { dir, head: git("rev-parse", "HEAD").trim() };
}
