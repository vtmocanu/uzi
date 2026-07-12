// Self-improvement run support (PRD #46 Decision 10, M5). A self_improve run is
// the ordinary issue runner with three deltas: it works a FIXED branch so the
// worker's idempotent createMergeRequest reuses one open MR across cycles; its MR
// description carries its OWN test-suite evidence (there is no CI on the uzi repo);
// and it flags changes to guard-critical paths for extra-careful human review. The
// primary directive is untouched — the bot still never merges to main.

import { execFile } from "node:child_process";

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
// human merge, and this flag draws the reviewer's eye there.
export const GUARD_CRITICAL_PATTERNS: RegExp[] = [
  /agent\/src\/guardrails\.ts/,
  /api\/internal\/middleware\/auth/,
  /api\/internal\/secretbox\//,
  /api\/internal\/vault\//,
  // workersvc claim + token assembly (the paths that open the forge PAT / Anthropic token).
  /api\/internal\/workersvc\/(claim|service)\.go/,
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
// relative to the worktree root.
export interface SelfImproveCheck {
  name: string;
  cwd: string;
  command: string;
  args: string[];
}

// SELF_IMPROVE_CHECKS are the uzi repo's own gates (CLAUDE.md): the Go suite, the
// web + agent suites, and the web build (which runs check-docs + tsc). npm test in
// web/agent needs installed deps; a check that cannot run is reported "skipped",
// never "failed", so a bare worktree does not masquerade as a test failure.
export const SELF_IMPROVE_CHECKS: SelfImproveCheck[] = [
  { name: "api: go test ./...", cwd: "api", command: "go", args: ["test", "./..."] },
  { name: "web: npm test", cwd: "web", command: "npm", args: ["test"] },
  { name: "web: npm run build", cwd: "web", command: "npm", args: ["run", "build"] },
  { name: "agent: npm test", cwd: "agent", command: "npm", args: ["test"] },
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

// defaultCheckRunner runs a check via execFile with a wall-clock cap, mapping exit
// 0 → passed, non-zero → failed, and a spawn error (toolchain/deps missing) →
// skipped. It captures only the exit status, never the (potentially secret-bearing)
// command output — the run-message redactor does not cover a third-party test's
// stdout, so none of it reaches the MR.
export function defaultCheckRunner(timeoutMs = 15 * 60 * 1000): CheckRunner {
  return (check, worktreePath) =>
    new Promise<CheckResult>((resolve) => {
      execFile(
        check.command,
        check.args,
        { cwd: `${worktreePath}/${check.cwd}`, timeout: timeoutMs, maxBuffer: 1 << 20 },
        (error) => {
          if (!error) {
            resolve({ name: check.name, status: "passed", detail: "exit 0" });
            return;
          }
          const e = error as NodeJS.ErrnoException & { code?: string | number; killed?: boolean };
          if (e.code === "ENOENT") {
            resolve({ name: check.name, status: "skipped", detail: "command not available in the worker" });
            return;
          }
          if (e.killed) {
            resolve({ name: check.name, status: "skipped", detail: "timed out" });
            return;
          }
          resolve({ name: check.name, status: "failed", detail: typeof e.code === "number" ? `exit ${e.code}` : "failed" });
        },
      );
    });
}

// selfImproveMrSection composes the MR-description addendum for a self_improve run:
// the guard-critical flag (when any path was touched) and the test-suite evidence.
// Returns "" only when there is nothing to add (no checks, no hits) — the caller
// always has at least the checks, so in practice it is non-empty.
export function selfImproveMrSection(guardHits: string[], checks: CheckResult[]): string {
  const lines: string[] = ["", "---", "### Self-improvement run"];

  if (guardHits.length > 0) {
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
