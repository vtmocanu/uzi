# PRD #121: Pre-provision cloned-repo JS deps before the agent works (+ pre-scan accuracy + gate honesty)

**GitLab Issue**: [#121](https://gitlab.example.com/vtmocanu/uzi/-/issues/121)
**Status**: Draft (created 2026-07-22; revised same day after a fable adversarial review — reversed the install-flags decision to keep `--ignore-scripts` (the esbuild premise was empirically false, tested), rescoped M4 as net-new machinery, fixed the M2 hoist point to after `provisionRunTools`, added the trust-posture rationale and a k8s single-uid hedge; see the Decision Log)
**Priority**: Medium

## Problem

A worker seeds a fresh runner clone per run, so the target repo's JS dependencies
(`node_modules` in `web/`, `agent/`, or wherever) are **absent** when the agent
starts. The first quality-gate command the agent runs — `npm test`, `vitest`,
`tsc` — fails with `command not found`, and the agent has to discover it must run
`npm ci` itself, mid-task, before any gate passes. That wasted cycle recurs on
every JS-touching run and was flagged repeatedly by the judge:

- run **2ebc093e** (#118): `vitest: not found`, `npm ci` mid-task.
- run **95afd872** (#117): `web/ node_modules` install needed in the runner clone.
- run **bdc4c634** (#67): the pre-scan already knew `vitest` was missing, yet the
  run proceeded.
- run **4d4762cf** (#87): `sh: tsc: not found`, `npm ci` (108 pkgs) before the
  `agent/` workspace could typecheck/test.

Two structural companions surfaced by the same reviews:

1. **Pre-scan false positives.** The API's `scanCommandNotFound`
   (`api/internal/workersvc/judge.go:82`) is a judge-time regex scan of the
   finished run's `tool_result` payloads for `foo: command not found` (a text-pattern
   match, not an exit code). It flags a tool from a **single command-not-found hit**,
   with no notion that the agent later ran that tool successfully via an npm script
   (`node_modules/.bin`). So `tsc`/`vitest`/`eslint` get reported as "missing" even
   when the run's `npm run typecheck` succeeded (run **e7c31999**, #115).
2. **Gate honesty.** A run can `signal_done` and open a green MR when the plan's
   declared gate commands were never runnable (no `node_modules`, install failed).
   The green MR then implies tests that never executed (run **bdc4c634**).

### Why this is the current state (in code)

The machinery to install deps already exists but fires at the **wrong time and
for the wrong consumer**:

- **`prepareCheckDeps`** (`agent/src/self-improve.ts`) already runs
  `npm ci --ignore-scripts` in each dir under the scrubbed env + the cap-less
  `runner` uid (PRD #51 M4), best-effort — a failed install leaves `node_modules`
  absent and the check reports an honest "skipped", never a false pass.
- But it is called at **`runner.ts:388`, AFTER the agent finishes, and only inside
  the `if (claim.kind === "self_improve")` branch** — so an ordinary issue run gets
  **no dependency install at all**, ever; only self-improve runs get even the post-run
  install. The interactive agent on a normal run never benefits. And its dir list is
  **hardcoded `["web", "agent"]`** — uzi's own layout, not a general repo's.

So the fix is largely a **relocation + generalization** of an already-trusted,
already-sandboxed routine, plus two companion accuracy fixes.

## Solution Overview

Three coherent parts (the same judge reviews flagged all three):

1. **Provision the clone's JS deps *before* the agent's first turn.** Hoist the
   `prepareCheckDeps`-style install to run right after the runner clone is seeded
   (`runner.ts:~210`), reusing the exact sandbox already in place (runner uid +
   scrubbed env, best-effort → honest skip on failure). Two upgrades:
   - **Generalize dir discovery**: instead of hardcoded `web/agent`, find dirs
     with a `package.json` + a lockfile (excluding `node_modules`, bounded
     depth/count), and pick the installer per lockfile: `package-lock.json →
     npm ci`, `pnpm-lock.yaml → pnpm i --frozen-lockfile`, `yarn.lock → yarn
     --frozen-lockfile`, `bun.lockb → bun install`; a root `workspaces` monorepo
     resolves to a single root install.
   - **Overlap the latency**: kick the install off **concurrently with the plan
     turn + approval wait** (which includes human latency), so deps are ready by
     the time the agent implements — near-zero added wall-clock, and nothing is
     wasted if the plan is rejected at the gate (the install just gets discarded
     with the clone).
2. **Pre-scan accuracy.** In `scanCommandNotFound`, suppress a `ToolMiss` for tool
   X when the same run's trace later shows X running successfully (a non-127 exit,
   or an `npm run <script>` / `node_modules/.bin/X` invocation that wraps it).
   This kills the tsc/vitest/eslint false positives. (Part 1 also shrinks these
   naturally — with `node_modules` present the agent stops fumbling bare `tsc`.)
3. **Gate honesty.** When a plan *declared* gate commands that ended up
   **unrunnable** (install failed: no registry egress, lockfile drift), don't let
   the run `signal_done` with a green MR implying tests ran — annotate the MR /
   downgrade the delivery to "unverified". The post-run check phase already
   distinguishes "ran+failed" from "skipped" (self-improve's honest skip); surface
   that distinction. Part 1 makes gates runnable in the common case; Part 3 is the
   safety net for when install genuinely can't happen.

## Implementation Milestones

- [ ] **M1 — Generalized dependency discovery + install (extract from
  `prepareCheckDeps`).** A package-manager-aware, lockfile-driven installer that
  discovers JS project dirs in a clone (bounded), runs the **frozen `--ignore-scripts`
  install** (unchanged from `prepareCheckDeps` — a pure relocation, no flag change)
  under the runner uid + scrubbed env, and returns per-dir install/skip results. Unit
  tests over fixture clones (npm/pnpm/yarn/bun, monorepo workspaces, no-lockfile,
  install-failure → honest skip).
- [ ] **M2 — Pre-agent provisioning at the right lifecycle point, with defined join
  semantics.** Call the M1 installer inside the executor **after `provisionRunTools`
  (`sdk-executor.ts:240`) and before the plan turn (`:266`)** — NOT at `runner.ts:~210`,
  which predates `toolEnv` and would use the image's npm instead of the run's
  provisioned node. Kick it off async so it **overlaps the plan turn** (and, on
  human-gated runs, the `awaiting_approval` wait), then **join it before the first
  implement turn** so the agent never races it and never collides with an
  agent-initiated `npm ci` in the same dir (npm has no cross-process `node_modules`
  lock). `prepareCheckDeps` at `runner.ts:388` collapses to reusing M1. **Wall-clock:**
  near-zero for human-gated runs (hidden behind the approval wait); for **autopilot**
  (`gatePlan` short-circuits with no wait, `runner.ts:587-607`) the overlap window is
  only the plan turn, so a slow install can add real wall-clock before implement —
  acceptable, and stated honestly rather than claimed away.
- [ ] **M3 — Pre-scan accuracy (`scanCommandNotFound`).** Suppress a `ToolMiss` for a
  tool the same run later ran successfully. Note: `judgeSignal` today fetches only
  `tool_result` payloads (`ListToolResultPayloadsForRun`, `judge.go:231`) — no
  `tool_use` rows or structured exit codes — so "later ran green" needs either widening
  the query to the invocation side or a text heuristic over the payloads; scope that in
  M3. The suppression itself is safe (a genuinely-absent tool cannot later run green).
  Go unit tests with fumble-then-succeed traces.
- [ ] **M4 — Gate honesty (net-new machinery — heaviest milestone; may split).** This
  is NOT just surfacing an existing signal: the ran/failed/skipped mapping today lives
  only in self-improve's `defaultCheckRunner` (`self-improve.ts:227-249`) for hardcoded
  uzi checks on self_improve runs, and nothing anywhere extracts or tracks a *plan's
  declared gate commands*. M4 needs net-new work: extract the gate commands a plan
  declares, track whether each actually ran, and annotate the MR / downgrade the
  delivery to "unverified" when they didn't. Given the cost, consider shipping M1–M3
  first and splitting M4 into its own increment/PRD.
- [ ] **M5 — Verified on a real JS run.** A run touching `web/` completes with the
  agent never hitting `command not found` for a gate tool and never running a
  manual `npm ci`; the judge pre-scan no longer false-flags tsc/vitest. Capture
  the evidence (activity log / review).
- [ ] **M6 — Docs.** Update the relevant docs/specs (`specs/ai.md` design note per
  repo convention; `CLAUDE.md`/`ARCHITECTURE.md` if the run lifecycle description
  needs it).

## The install-flags decision: keep `--ignore-scripts` (verified)

Default to **`npm ci --ignore-scripts`** — the flag `prepareCheckDeps` already uses.
An earlier draft of this PRD argued for dropping `--ignore-scripts` (full install) on
the theory that it leaves native deps like `esbuild` (vitest/vite's dependency)
unbuilt. **That premise is false, and was tested:** since esbuild 0.16 the platform
binary ships as an `optionalDependencies` package (`@esbuild/<platform>`), not a
postinstall download, so `npm ci --ignore-scripts` installs a fully-runnable esbuild
(verified 2026-07-22: `npm ci --ignore-scripts` with esbuild 0.25.5 →
`node_modules/.bin/esbuild --version` works, binary from `@esbuild/darwin-arm64`). The
common JS gate toolchain (vitest, vite, tsc, eslint) works under `--ignore-scripts`.

So `--ignore-scripts` stays the default: it preserves the codebase's existing
defense-in-depth posture (`self-improve.ts:151-152` documents it as a supply-chain
*reduction*), it matches what the extracted routine already does (M1 becomes a pure
relocation, no install-behavior change), and it works for the toolchain that motivated
this PRD. The exception is a package with a *genuine* native postinstall (e.g.
`better-sqlite3`, `bcrypt`); for those the agent can run the build explicitly under the
same runner uid (which already executes the repo's test code) — a rare, per-run
escalation, not the default. Full-scripts install is NOT adopted.

## Trust posture — why auto-install is OK by default

Repo-borne provisioning config is deliberately gated in uzi: `repo_devbox_opt_in`
(migration `00047`) is per-repo opt-in, default OFF, and a repo's devbox
`init_hook`/scripts are NEVER executed by a run. Auto-running a repo's lockfile install
pre-approval must clear the same bar — and it does, *because of the `--ignore-scripts`
default*: a frozen `--ignore-scripts` install does lockfile resolution + fetches
published package tarballs into `node_modules`; no repo-authored script runs, unlike a
devbox `init_hook`. It also runs under the same `runner` uid + scrubbed env that already
executes the repo's tests. So it needs no new opt-in gate. (If we ever default to
full-scripts install, that reasoning breaks and auto-install must become opt-in —
recorded here so the tradeoff is never silently crossed.)

## Success Criteria

- On a JS-touching run, the agent runs its declared gates with **zero** manual
  `npm ci` and **zero** `command not found` for gate tools.
- Added wall-clock from provisioning is negligible (hidden behind the plan +
  approval wait).
- The judge pre-scan no longer reports node_modules-local tools as missing when the
  run actually ran them.
- A run whose declared gates could not run is never presented as a clean green MR
  implying they passed.
- The install stays inside the existing trust boundary (runner uid + scrubbed env);
  the worker's credentials are never exposed to repo install code.

## Risks & Mitigations

- **Untrusted install code.** Mitigated by `--ignore-scripts` (no repo-authored script
  runs) plus the existing runner-uid + scrubbed-env sandbox — the same boundary that
  already runs the repo's tests. **Caveat:** on a `#58` single-uid start there is no
  runner-uid split (`self-improve.ts:154-157`), so isolation is weaker there; since k8s
  is now the primary runtime (CLAUDE.md), verify the k8s worker posture
  (`docs/proc-hardening.md`) rather than assuming the split holds everywhere.
- **Install latency on every JS run.** Mitigated by overlapping with the plan +
  approval wait (M2); optionally a follow-up caches `node_modules` by lockfile hash
  across runs (out of scope here, noted as a future optimization).
- **Package-manager sprawl (npm/pnpm/yarn/bun, monorepos).** Mitigated by
  lockfile-driven detection with a bounded search and an honest-skip fallback (an
  unrecognized layout just isn't pre-installed — same as today, never worse).
- **Pre-scan over-suppression.** M3 must only suppress a miss when the *same* tool
  later ran green — not blanket-ignore 127s (a genuinely absent tool must still be
  reported).

## Related Work

- **PRD #51 M4** — the runner uid + scrubbed env that already sandboxes `npm ci`
  and repo test execution (the trust boundary this PRD reuses).
- **PRD #46 Decision 4** — the deterministic command-not-found pre-scan
  (`scanCommandNotFound`) M3 refines.
- **`self-improve.ts` `prepareCheckDeps`** — the existing install routine M1
  extracts and generalizes; **`runner.ts:388`** — its current (post-agent) call
  site.

## Decision Log

- 2026-07-22 — Created. Split out of a judge-review triage of missing/friction
  tools across 16 runs, as the non-tool-install item (you can't bake a cloned
  repo's `node_modules` into an image). Sibling items handled separately: chromium
  browser reliability → PRD #120; openssl → baked into the worker toolchain (MR
  !103); SVG rasterizer → dismissed (chromium covers it).
- 2026-07-22 — Fable adversarial review. Problem + mechanism claims verified against
  code (all five cited runs confirmed via `uzi review show`). Applied: (1) **reversed
  the key decision** — the original default (full `npm ci`) rested on a false premise
  that `--ignore-scripts` leaves `esbuild` unbuilt; tested and disproven (esbuild ≥0.16
  ships the platform binary via `optionalDependencies`, so `--ignore-scripts` yields a
  runnable esbuild), so `--ignore-scripts` **stays** the default (also matches the
  extracted routine → M1 is a pure relocation); (2) **rescoped M4** as net-new
  machinery (plan-gate-command extraction + tracking + MR annotation), not "surface an
  existing distinction" — flagged as splittable; (3) **fixed the M2 hoist point** to
  after `provisionRunTools` (`sdk-executor.ts:240`), before the plan turn — an install
  at `runner.ts:210` predates `toolEnv` and would use the image npm; defined
  join-before-implement semantics + the concurrent-`npm ci` race; softened the
  wall-clock claim for autopilot; (4) added the **trust-posture** rationale (the
  `repo_devbox_opt_in` / migration `00047` precedent — `--ignore-scripts` clears the
  bar without a new opt-in) and a **k8s single-uid** hedge for the sandbox claim; (5)
  corrected "exit-127" → "command-not-found text match". Confirmed non-issues: deferring
  lockfile-hash caching is right (the runner clone is torn down each run,
  `runner.ts:453-456`; only the bare clone persists — caching is separate work).
