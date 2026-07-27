// Self-improvement run support (PRD #46 Decision 10, M5). A self_improve run is
// the ordinary issue runner with three deltas: it works a FIXED branch so the
// worker's idempotent createMergeRequest reuses one open MR across cycles; its MR
// description carries its OWN test-suite evidence (the worker's own proof,
// alongside uzi's CI which independently verifies it since PRD #52); and it flags
// changes to guard-critical paths for extra-careful human review. The check
// evidence + the dependency install run under the cap-less `runner` uid (PRD #51 M4,
// buildCheckEnv below; the install itself is js-deps.ts since PRD #121 M1/M2), so a
// hostile self-improvement change's test code cannot read the worker's 0400 token
// file — the same-OS-user residual this used to carry is closed for the local (A1)
// path. The primary directive is untouched — the bot still never merges to main.

import { spawn } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { killRunnerGroup, runnerCommand } from "./runner-uid.js";
import { buildCheckEnv } from "./sdk-env.js";

// Re-exported so the existing importers (runner.ts, the tests) keep one obvious home for
// the self-improve check vocabulary; sdk-env.ts is the definition.
export { buildCheckEnv };

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
// Go check honest-skips (ENOENT) and the npm checks depend on the js-deps installer
// (PRD #121) having installed node_modules — which now happens BEFORE the agent's
// first turn as well as before these checks. M9 makes real evidence POSSIBLE; the
// honest skip (M8) is the fallback whenever a toolchain/dep is genuinely absent —
// never a false pass/fail.
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

// The scrubbed replacement env these checks (and the js-deps install) run under is
// `buildCheckEnv`, which now lives in sdk-env.ts beside `buildSdkEnv` (PRD #121). It moved
// there because it is a GENERIC subprocess-env builder with three consumers — these checks,
// the runner's self-improve block, and the executor's dependency install — and a generic
// executor should not import a run-kind-specific module to get one. Its security rationale
// moved with it; read that comment before touching anything that feeds a check subprocess.

// defaultCheckRunner runs a check via `spawn` under the SCRUBBED env (buildCheckEnv)
// with a wall-clock cap that can actually enforce itself under the runner-uid split
// (#153 — see the load-bearing note at the spawn site). It captures only the exit status,
// never the (potentially secret-bearing) command output: the output is not read at all
// (`stdio: "ignore"`), because the run-message redactor does not cover a third-party
// test's stdout, so none of it can reach the MR.
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
//   - killed by any other signal (e.g. OOM)        → skipped
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
    // #154: a SURVIVING `node_modules` is not the same as an installed one. The
    // existence check above passes for a tree the installer failed to build, and the
    // check then runs against stale deps and reports a real-looking failure — the exact
    // outcome the status mapping below exists to prevent.
    //
    // Measured, and this is the shape the fix is keyed to: with `package.json` declaring
    // a dependency the lockfile does not carry, `npm ci` REFUSES (EUSAGE, "can only
    // install packages when your package.json and package-lock.json … are in sync"),
    // exits 1, and leaves the previous `node_modules` intact — with the newly-declared
    // dependency absent from it. So the tell is not the directory, it is the gap between
    // what the manifest declares and what the tree contains.
    if (check.requires === "node_modules") {
      const missing = missingDeclaredDeps(cwd);
      if (missing.length > 0) {
        return Promise.resolve({
          name: check.name,
          status: "skipped" as const,
          detail: `dependencies out of date in the worker (missing: ${missing.slice(0, 3).join(", ")})`,
        });
      }
    }
    // PRD #51 M4: the check runs agent-authored test code (go test / vitest / tsc) — an
    // untrusted surface — so under the `runner` uid (setpriv wrapper); single-uid (#58)
    // runs it directly. ENOENT/127 still classify as "skipped" (the wrapper preserves the
    // target's exit semantics; a missing runner uid is a #58 single-uid start where the
    // wrapper is absent).
    const wc = runnerCommand(check.command, check.args);
    return new Promise<CheckResult>((resolve) => {
      let timedOut = false;
      let settled = false;
      // THE `spawn` + `detached: true` + `killRunnerGroup` TRIO IS LOAD-BEARING (#153).
      // Reverting to `execFile` with its `timeout` option looks like a simplification and
      // silently reintroduces a measured defect: execFile's timeout kills from the WORKER
      // uid, which is EPERM against a process running as `runner`, so under the PRD #51
      // split the wall-clock cap could not kill anything. Measured on this exact shape —
      // worker execFile'ing `setpriv --reuid runner … sleep 120` with a 2s cap called back
      // at 2008ms carrying `code: "EPERM"` while the runner's `sleep 120` was still alive
      // 6s later; the same-uid control killed correctly. A check could outlive its cap and
      // orphan.
      //   - `detached: true` makes the child a process-GROUP leader, which is the shape
      //     `killRunnerGroup` documents and what lets the kill reach any grandchild the
      //     test spawned. `execFile` does not even forward `detached` (it copies a fixed
      //     option subset to spawn), so the flag would be dropped without a word.
      //   - `killRunnerGroup` reuids via setpriv under the split and signals directly on a
      //     #58 single-uid start, so both modes can actually reap. A plain `child.kill()`
      //     cannot.
      // `stdio: "ignore"` keeps the no-output-capture property STRUCTURAL rather than
      // disciplinary — stronger than execFile's buffer-then-discard, and it also retires a
      // `maxBuffer` that would have killed a merely-verbose passing suite.
      const child = spawn(wc.command, wc.args, { cwd, env, detached: true, stdio: "ignore" });
      const timer = setTimeout(() => {
        timedOut = true;
        killRunnerGroup(child.pid);
      }, timeoutMs);
      // The live child keeps the loop alive on its own; an un-unref'd timer would hold it
      // for the FULL cap even after a fast check finished.
      timer.unref();
      const done = (result: CheckResult): void => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        resolve(result);
      };

      child.on("error", (err) => {
        // The check never started, so it has no verdict — "skipped" either way. (Under
        // execFile a non-ENOENT spawn failure used to land in the final branch and be
        // reported as "failed", which is the accusation this mapping exists to avoid.)
        const code = (err as NodeJS.ErrnoException).code;
        done({
          name: check.name,
          status: "skipped",
          detail: code === "ENOENT" ? "command not available in the worker" : "could not start the check",
        });
      });
      child.on("close", (code, signalName) => {
        // A check killed by the cap has no verdict. Under execFile this arrived as
        // `e.killed`, which is NOT set on this path — so before #153 a timed-out check
        // reported "failed", exactly the false accusation the mapping forbids.
        if (timedOut) return done({ name: check.name, status: "skipped", detail: "timed out" });
        if (code === 0) return done({ name: check.name, status: "passed", detail: "exit 0" });
        // 127 is "command not found" from the shell or an npm script whose binary is
        // absent — never a real test failure, so it is a skip, not a failure.
        if (code === 127) return done({ name: check.name, status: "skipped", detail: "command/binary not available (exit 127)" });
        // Killed by a signal we did not send (an OOM kill, an operator SIGTERM): the
        // check produced no verdict, so it is not evidence of a failure either.
        if (code === null) return done({ name: check.name, status: "skipped", detail: `killed (${signalName})` });
        done({ name: check.name, status: "failed", detail: `exit ${code}` });
      });
    });
  };
}

/**
 * The dependencies `<cwd>/package.json` DECLARES that are absent from its
 * `node_modules` (#154). Empty ⇒ nothing contradicts "the deps are installed".
 *
 * DERIVED FROM WHAT THE PROJECT DECLARES, never a flat requirement, and that is the
 * load-bearing part rather than a nicety: `npm ci` on a ZERO-DEPENDENCY project exits 0
 * and creates no `node_modules` at all (measured), so any unconditional requirement would
 * turn a genuine success into a reported failure — the same lie as passing a stale tree,
 * pointing the other way. A project that declares nothing yields `[]` here and its check
 * runs, exactly as before.
 *
 * Deliberately conservative about what counts as declared:
 *   - `dependencies` + `devDependencies` only. `optionalDependencies` may legitimately be
 *     absent (that is what optional means — a platform-mismatched binary is not installed
 *     and nothing is wrong), and `peerDependencies` may be intentionally unmet.
 *   - An unreadable or malformed package.json yields `[]`. We cannot tell, and unknown
 *     must never be reported as a failure.
 * Presence is a shallow existence check per package (`node_modules/<name>`, scope-aware),
 * not a version match: the goal is to catch a tree the installer did not build, not to
 * re-implement `npm ci`'s own reconciliation.
 */
export function missingDeclaredDeps(cwd: string): string[] {
  let declared: string[];
  try {
    const parsed = JSON.parse(readFileSync(`${cwd}/package.json`, "utf8")) as {
      dependencies?: Record<string, unknown>;
      devDependencies?: Record<string, unknown>;
    };
    declared = [...Object.keys(parsed.dependencies ?? {}), ...Object.keys(parsed.devDependencies ?? {})];
  } catch {
    return [];
  }
  return declared.filter((name) => !existsSync(`${cwd}/node_modules/${name}`));
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
