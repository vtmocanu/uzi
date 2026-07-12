// Self-improvement run support (PRD #46 Decision 10, M5). A self_improve run is
// the ordinary issue runner with three deltas: it works a FIXED branch so the
// worker's idempotent createMergeRequest reuses one open MR across cycles; its MR
// description carries its OWN test-suite evidence (there is no CI on the uzi repo);
// and it flags changes to guard-critical paths for extra-careful human review. The
// primary directive is untouched — the bot still never merges to main.

import { execFile } from "node:child_process";
import { existsSync } from "node:fs";

// SELF_IMPROVE_BRANCH is the fixed branch every self_improve cycle pushes to.
// Reusing one branch is what lets an open self-improvement MR be extended (the
// worker's createMergeRequest is idempotent per branch, git.ts pushBranch never
// forces), so successive cycles are tested together (Decision 10).
export const SELF_IMPROVE_BRANCH = "uzi/self-improve";

// GUARD_CRITICAL_PATTERNS match the paths whose change most needs careful human
// review (Decision 10, audit C1): a self_improve MR touching any of them is flagged
// loudly. A self_improve run cannot weaken its own guardrails at runtime (it runs
// its compiled guardrails.ts with settingSources:[]; the checked-out copy never
// loads) — the risk is the MERGED, later-deployed artifact, so the fence is at the
// human merge, and this flag draws the reviewer's eye there. The set is deliberately
// broad on token custody (M5 audit): an injected change could otherwise be steered
// into an UNFLAGGED token-handling file precisely to dodge the reviewer alert.
export const GUARD_CRITICAL_PATTERNS: RegExp[] = [
  // Agent-side guardrails + worker token custody: the SDK executor/env that fence
  // the agent subprocess, and git.ts which injects the PAT via env-scoped git config.
  /agent\/src\/guardrails\.ts/,
  /agent\/src\/sdk-executor\.ts/,
  /agent\/src\/sdk-env\.ts/,
  /agent\/src\/git\.ts/,
  // API auth + secret/vault crypto.
  /api\/internal\/middleware\/auth/,
  /api\/internal\/secretbox\//,
  /api\/internal\/vault\//,
  // workersvc token custody: claim/token assembly (service.go = assembleClaim +
  // token open, judge.go = assembleJudgeClaim, claim.go = ClaimSecrets) PLUS any
  // future token/secret/claim-named file in the package, so a token change can't
  // land in an unflagged sibling.
  /api\/internal\/workersvc\/(claim|service|judge)\.go/,
  /api\/internal\/workersvc\/[^/]*(token|secret|claim)[^/]*\.go/,
  // compose secret wiring.
  /docker-compose[^/]*\.ya?ml$/,
  /(^|\/)\.env(\.|$)/,
];

// flagGuardPaths returns the subset of changed files that touch a guard-critical
// path, de-duplicated and in input order.
export function flagGuardPaths(changedFiles: string[]): string[] {
  const hits: string[] = [];
  for (const f of changedFiles) {
    const file = f.trim();
    if (file === "") continue;
    if (GUARD_CRITICAL_PATTERNS.some((re) => re.test(file)) && !hits.includes(file)) {
      hits.push(file);
    }
  }
  return hits;
}

// SelfImproveCheck is one test/build command the runner executes after the agent
// finishes, so the MR carries its own pass/fail evidence (Decision 10). cwd is
// relative to the worktree root. `requires` is an optional prerequisite path
// (relative to cwd) that must exist for the check to be MEANINGFUL — see the
// pre-flight in defaultCheckRunner.
export interface SelfImproveCheck {
  name: string;
  cwd: string;
  command: string;
  args: string[];
  requires?: string;
}

// SELF_IMPROVE_CHECKS are the uzi repo's own gates (CLAUDE.md): the Go suite, the
// web + agent suites, and the web build (which runs check-docs + tsc).
//
// The npm checks declare `requires: "node_modules"`. A fresh clone has none, and
// running `npm test` there does NOT fail with ENOENT (npm itself exists) — it exits
// 127 because vitest/tsc are missing, which is indistinguishable from a real test
// failure by exit code alone. Pre-flighting the prerequisite is what keeps the
// contract honest: a check that CANNOT run is reported "skipped" with the reason,
// never "failed", so a bare worktree never masquerades as a test failure (M8).
export const SELF_IMPROVE_CHECKS: SelfImproveCheck[] = [
  { name: "api: go test ./...", cwd: "api", command: "go", args: ["test", "./..."] },
  { name: "web: npm test", cwd: "web", command: "npm", args: ["test"], requires: "node_modules" },
  { name: "web: npm run build", cwd: "web", command: "npm", args: ["run", "build"], requires: "node_modules" },
  { name: "agent: npm run typecheck", cwd: "agent", command: "npm", args: ["run", "typecheck"], requires: "node_modules" },
  { name: "agent: npm test", cwd: "agent", command: "npm", args: ["test"], requires: "node_modules" },
];

export type CheckStatus = "passed" | "failed" | "skipped";

export interface CheckResult {
  name: string;
  status: CheckStatus;
  // detail is a short human note (an exit code or the reason a check was skipped);
  // it never carries full command output.
  detail: string;
}

// CheckRunner executes one check and reports its outcome. Injectable so tests drive
// the composition without spawning real subprocesses.
export type CheckRunner = (check: SelfImproveCheck, worktreePath: string) => Promise<CheckResult>;

// runSelfImproveChecks runs every check best-effort and returns their results in
// order. Best-effort: a runner that throws is recorded as "skipped" so a flaky
// environment never fails the run — the MR still lands with whatever evidence was
// gathered, and a human reviews.
export async function runSelfImproveChecks(worktreePath: string, runner: CheckRunner): Promise<CheckResult[]> {
  const results: CheckResult[] = [];
  for (const check of SELF_IMPROVE_CHECKS) {
    try {
      results.push(await runner(check, worktreePath));
    } catch (err) {
      results.push({ name: check.name, status: "skipped", detail: `could not run: ${errText(err)}` });
    }
  }
  return results;
}

// defaultCheckRunner runs a check via execFile with a wall-clock cap. It captures
// only the exit status, never the (potentially secret-bearing) command output — the
// run-message redactor does not cover a third-party test's stdout, so none of it
// reaches the MR.
//
// The status mapping is deliberately conservative: a check only reports "failed"
// when it actually RAN and genuinely failed. Everything that means "this check could
// not run here" is "skipped", with the reason (M8 — the MR's evidence must never
// accuse good code of failing):
//   - prerequisite missing (e.g. no node_modules)  → skipped  [pre-flight, no spawn]
//   - command not in the worker (ENOENT)           → skipped
//   - exit 127 (command/binary not found by the    → skipped
//     shell or the npm script, e.g. vitest/tsc)
//   - killed by the wall-clock cap                 → skipped
//   - any other non-zero exit                      → failed   [a real failure]
export function defaultCheckRunner(timeoutMs = 15 * 60 * 1000): CheckRunner {
  return (check, worktreePath) => {
    const cwd = `${worktreePath}/${check.cwd}`;
    // Pre-flight: a declared prerequisite that is absent means the check cannot
    // produce a meaningful verdict. Report it honestly instead of running the
    // command and misreading its 127 as a test failure.
    if (check.requires && !existsSync(`${cwd}/${check.requires}`)) {
      return Promise.resolve({
        name: check.name,
        status: "skipped" as const,
        detail:
          check.requires === "node_modules"
            ? "dependencies not installed in the worker"
            : `prerequisite missing: ${check.requires}`,
      });
    }
    return new Promise<CheckResult>((resolve) => {
      execFile(
        check.command,
        check.args,
        { cwd, timeout: timeoutMs, maxBuffer: 1 << 20 },
        (error) => {
          if (!error) {
            resolve({ name: check.name, status: "passed", detail: "exit 0" });
            return;
          }
          // execFile's error carries `code` as the ENOENT-style string on a spawn
          // failure, or the numeric exit status when the command ran and exited
          // non-zero — so it is genuinely `string | number` here.
          const e = error as Error & { code?: string | number; killed?: boolean };
          if (e.code === "ENOENT") {
            resolve({ name: check.name, status: "skipped", detail: "command not available in the worker" });
            return;
          }
          if (e.killed) {
            resolve({ name: check.name, status: "skipped", detail: "timed out" });
            return;
          }
          // 127 is "command not found" from the shell or an npm script whose binary is
          // absent — never a real test failure, so it is a skip, not a failure.
          if (e.code === 127) {
            resolve({ name: check.name, status: "skipped", detail: "command/binary not available (exit 127)" });
            return;
          }
          resolve({ name: check.name, status: "failed", detail: typeof e.code === "number" ? `exit ${e.code}` : "failed" });
        },
      );
    });
  };
}

// selfImproveMrSection composes the MR-description addendum for a self_improve run:
// the guard-critical flag (when any path was touched) and the test-suite evidence.
// guardHits is null when the changed-file diff could NOT be computed — that surfaces
// loudly (fail-closed) so a diff failure never silently suppresses the flag on a
// guard-touching MR (M5 audit). Returns "" only when there is nothing to add (no
// checks, no hits) — the caller always has at least the checks, so it is non-empty.
export function selfImproveMrSection(guardHits: string[] | null, checks: CheckResult[]): string {
  const lines: string[] = ["", "---", "### Self-improvement run"];

  if (guardHits === null) {
    lines.push(
      "",
      "> ⚠️ **Guard-path check: UNAVAILABLE (diff failed).** The worker could not compute the",
      "> changed-file list, so it could not check for guard-critical paths. Review this change",
      "> for any touch of guardrails, auth, secret/vault, worker token assembly, or compose",
      "> secret wiring MANUALLY before merging.",
    );
  } else if (guardHits.length > 0) {
    lines.push(
      "",
      "> ⚠️ **Guard-critical paths touched — review with extra care.** This change modifies",
      "> files on uzi's security-critical surface (guardrails, auth, secret/vault, worker",
      "> token assembly, or compose secret wiring). Verify it does not weaken any guardrail",
      "> before merging:",
      ...guardHits.map((f) => `> - \`${f}\``),
    );
  }

  if (checks.length > 0) {
    lines.push("", "**Test evidence** (run by the worker — this repo has no CI):", "");
    for (const c of checks) {
      lines.push(`- ${checkEmoji(c.status)} ${c.name} — ${c.status} (${c.detail})`);
    }
    // A skipped check proves NOTHING. Say so plainly, so a reviewer never reads a
    // wall of "skipped" as a wall of "passed" (M8): the worker image may lack the
    // toolchain (no Go) or the fresh clone its dependencies (no node_modules).
    const skipped = checks.filter((c) => c.status === "skipped");
    if (skipped.length > 0) {
      lines.push(
        "",
        `> ⚠️ **${skipped.length} of ${checks.length} checks were SKIPPED — skipped is NOT passed.**`,
        "> Those suites did not run here (the worker lacks the toolchain or the freshly cloned",
        "> worktree has no installed dependencies), so this MR carries NO evidence for them.",
        "> Run them yourself before merging.",
      );
    }
  }

  lines.push(
    "",
    "The bot cannot merge to `main` (protected-branch merge rights are humans only). A human must review and merge.",
  );
  return lines.join("\n");
}

function checkEmoji(status: CheckStatus): string {
  switch (status) {
    case "passed":
      return "✅";
    case "failed":
      return "❌";
    default:
      return "⚠️";
  }
}

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
