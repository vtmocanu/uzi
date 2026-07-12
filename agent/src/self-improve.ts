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
// Evidence is CONDITIONAL, not automatic: a check produces real pass/fail only when
// its toolchain is present in the worker. `go` (and `nodejs`) reach the worker only
// if the connected uzi repo's tool profile provisions them (PRD #18 devbox tooling,
// threaded to the checks via buildCheckEnv's toolEnv). With an empty tool profile the
// Go check honest-skips (ENOENT) and the npm checks depend on prepareCheckDeps having
// installed node_modules. M9 makes real evidence POSSIBLE; the honest skip (M8) is the
// fallback whenever a toolchain/dep is genuinely absent — never a false pass/fail.
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

// ─── Check subprocess environment (M9 + audit, security load-bearing) ─────────
// The checks below (and prepareCheckDeps' `npm ci`) execute AGENT-AUTHORED code as
// the WORKER uid — worktree test files, package.json scripts, vite/tsc/go test —
// ENTIRELY OUTSIDE the SDK hook system (guardrails.ts constrains only the AGENT's
// Bash, not a worker-spawned execFile child). The worker process holds the decrypted
// forge PAT + Anthropic token, and its env carries the join token
// (UZI_WORKER_TOKEN[_FILE]) + UZI_API_URL. So a check subprocess gets a SCRUBBED
// REPLACEMENT env — the same discipline provision.ts uses for nix build hooks — never
// a process.env spread: the join-token/API vars are ABSENT BY CONSTRUCTION, so
// agent-authored code cannot read them to impersonate the worker
// (join token → /api/worker/runs/claim → bot forge PAT + the user's Anthropic token).
//
// RESIDUAL NOT CLOSED (documented loudly, provision.ts house style): the subprocess
// still runs as the uzi uid, and the join-token FILE at /run/secrets/worker_token is
// same-uid readable at a hardcoded, documented path (the worker's post-read unlink
// fails on the read-only secret mount, so the file persists). Agent-authored test
// code can read that file directly — a surface behind NO PreToolUse hook. `npm ci`
// runs with --ignore-scripts (prepareCheckDeps) to delete the lifecycle-script
// (preinstall/install/postinstall) code-exec entry path — a REDUCTION, not a close:
// the test runners still execute model-written code. Blast radius if exploited: join
// token → claim → the bot forge PAT (Developer role, structurally cannot merge
// protected `main`) + the user's own Anthropic token (which the agent already holds).
// The structural close is the k8s uid-split (agent under a DISTINCT uid from the
// worker), deferred to the remote-worker phase. This is the SAME residual class
// provision.ts documents for build hooks — but those are admin-VETTED packages,
// whereas these checks run code the model just wrote (M5 silently widened that
// residual; surfaced by the M9 audit and accepted for the MVP).

// buildCheckEnv is the scrubbed replacement env for a check subprocess. PATH comes
// from the provisioned toolEnv when present (so go/vitest/tsc resolve), else the
// worker's base PATH; HOME is a writable per-run dir (npm cache/config). Only what the
// toolchains + npm-over-HTTPS demonstrably need is added — never a worker secret.
export function buildCheckEnv(
  source: NodeJS.ProcessEnv,
  homeDir: string,
  toolEnv?: Record<string, string>,
): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = {
    PATH: toolEnv?.PATH ?? source.PATH,
    HOME: homeDir,
    // No interactive prompt if a check shells out to git; not a secret.
    GIT_TERMINAL_PROMPT: "0",
  };
  // TLS trust + locale so nix-provided toolchains and `npm ci` over HTTPS work. Prefer
  // the provisioned value, fall back to the image's; never invent, never carry else.
  for (const k of ["NIX_SSL_CERT_FILE", "SSL_CERT_FILE", "LOCALE_ARCHIVE"] as const) {
    const v = toolEnv?.[k] ?? source[k];
    if (v) env[k] = v;
  }
  return env;
}

// prepareCheckDeps installs node deps so the npm checks can actually run (M9),
// best-effort. `npm ci --ignore-scripts` in each dir under the SCRUBBED env:
// --ignore-scripts deletes the lifecycle-script code-exec entry path (a reduction,
// not a close). On any failure (no registry egress, lockfile drift) it leaves
// node_modules absent, so the check pre-flight reports an honest "skipped" — never a
// false pass, never a fabricated failure. Returns per-dir notes for logging only.
export async function prepareCheckDeps(
  worktreePath: string,
  env: NodeJS.ProcessEnv,
  dirs: string[] = ["web", "agent"],
  timeoutMs = 10 * 60 * 1000,
): Promise<{ dir: string; ok: boolean; detail: string }[]> {
  const out: { dir: string; ok: boolean; detail: string }[] = [];
  for (const dir of dirs) {
    const cwd = `${worktreePath}/${dir}`;
    if (!existsSync(`${cwd}/package.json`)) {
      out.push({ dir, ok: false, detail: "no package.json" });
      continue;
    }
    const ok = await new Promise<boolean>((resolve) => {
      execFile("npm", ["ci", "--ignore-scripts"], { cwd, env, timeout: timeoutMs, maxBuffer: 1 << 20 }, (error) =>
        resolve(!error),
      );
    });
    out.push({ dir, ok, detail: ok ? "npm ci --ignore-scripts ok" : "npm ci failed → checks skip honestly" });
  }
  return out;
}

// defaultCheckRunner runs a check via execFile under the SCRUBBED env (buildCheckEnv)
// with a wall-clock cap. It captures only the exit status, never the (potentially
// secret-bearing) command output — the run-message redactor does not cover a
// third-party test's stdout, so none of it reaches the MR.
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
export function defaultCheckRunner(env: NodeJS.ProcessEnv, timeoutMs = 15 * 60 * 1000): CheckRunner {
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
        { cwd, env, timeout: timeoutMs, maxBuffer: 1 << 20 },
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
