// Self-improvement run support (PRD #46 Decision 10, M5). A self_improve run is
// the ordinary issue runner with three deltas: it works a FIXED branch so the
// worker's idempotent createMergeRequest reuses one open MR across cycles; its MR
// description carries its OWN test-suite evidence (the worker's own proof,
// alongside uzi's CI which independently verifies it since PRD #52); and it flags
// changes to guard-critical paths for extra-careful human review. The check
// evidence + `npm ci` run under the cap-less `runner` uid (PRD #51 M4, buildCheckEnv
// / prepareCheckDeps below), so a hostile self-improvement change's test code cannot
// read the worker's 0400 token file — the same-OS-user residual this used to carry
// is closed for the local (A1) path. The primary directive is untouched — the bot
// still never merges to main.

import { execFile, spawn } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { killRunnerGroup, runnerCommand, runnerPath, runnerTmpdir } from "./runner-uid.js";

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
// CLOSED for the local path (PRD #51 M4): the check + `npm ci` subprocesses now run
// under the cap-less `runner` uid (runnerCommand, below), and the join-token FILE at
// /run/secrets/worker_token is 0400 worker-owned, so agent-authored test code — even
// though it executes model-written code the SDK hook system never sees — CANNOT read
// the worker's token at all. `npm ci` still runs with --ignore-scripts (prepareCheckDeps)
// as a defense-in-depth REDUCTION of the lifecycle-script code-exec path. What the
// same-uid residual used to expose (join token → claim → bot forge PAT + the user's
// Anthropic token) is no longer reachable by these checks on the A1 (root-started) path.
// On a #58 single-uid (non-root) start there is no split and the checks run as the sole
// uid (that PRD's accepted posture); the cross-container k8s form is mapped in
// docs/proc-hardening.md. (This was the same residual class provision.ts documented for
// build hooks; both are closed together by the M4 spawn-as-runner.)

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
    // The RUNNER PATH (PRD #51 M4): the provisioned toolchain PATH when present, else
    // the /nix-bearing runner image PATH under the split (checks run as `runner`), NOT
    // the worker's stripped PATH. Single-uid (#58): ALSO the image PATH since PRD #120 —
    // the entrypoint now pins UZI_RUNNER_PATH on both branches, so this no longer inherits
    // npm's run-script injections (/app/node_modules/.bin et al).
    PATH: toolEnv?.PATH ?? runnerPath(source),
    HOME: homeDir,
    // No interactive prompt if a check shells out to git; not a secret.
    GIT_TERMINAL_PROMPT: "0",
  };
  // 5-bis: check scratch on the runner's private 0700 TMPDIR under the split.
  const tmp = runnerTmpdir(source);
  if (tmp) env.TMPDIR = tmp;
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
    // PRD #51 M4: `npm ci` runs agent-authored package.json (even with --ignore-scripts,
    // the lockfile resolution + any allowed binary) — an untrusted surface, so under the
    // `runner` uid (setpriv wrapper). Single-uid (#58) runs it directly.
    const nci = runnerCommand("npm", ["ci", "--ignore-scripts"]);
    const ok = await new Promise<boolean>((resolve) => {
      execFile(nci.command, nci.args, { cwd, env, timeout: timeoutMs, maxBuffer: 1 << 20 }, (error) =>
        resolve(!error),
      );
    });
    out.push({ dir, ok, detail: ok ? "npm ci --ignore-scripts ok" : "npm ci failed → checks skip honestly" });
  }
  return out;
}

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
