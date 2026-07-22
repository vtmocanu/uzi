# PRD #121: Pre-provision cloned-repo JS deps before the agent works (+ pre-scan accuracy + gate honesty)

**GitLab Issue**: [#121](https://gitlab.example.com/vtmocanu/uzi/-/issues/121)
**Status**: Draft (created 2026-07-22)
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
   finished run's `tool_result` payloads for `foo: command not found`. It flags a
   tool from a **single exit-127**, with no notion that the agent later ran that
   tool successfully via an npm script (`node_modules/.bin`). So `tsc`/`vitest`/
   `eslint` get reported as "missing" even when the run's `npm run typecheck`
   succeeded (run **e7c31999**, #115).
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
- But it is called at **`runner.ts:388`, AFTER the agent finishes**, purely to
  make the post-run *automated* gate checks runnable. The interactive agent never
  benefits. And its dir list is **hardcoded `["web", "agent"]`** — uzi's own
  layout, not a general repo's.

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
  discovers JS project dirs in a clone (bounded), runs the frozen install under
  the runner uid + scrubbed env, and returns per-dir install/skip results. Unit
  tests over fixture clones (npm/pnpm/yarn/bun, monorepo workspaces, no-lockfile,
  install-failure → honest skip).
- [ ] **M2 — Pre-agent provisioning, overlapped with the plan/approval wait.**
  Call the M1 installer right after the runner clone is seeded, running
  concurrently with the plan turn + approval so the latency hides behind existing
  wait time; deps are present before the first implement turn. `prepareCheckDeps`
  at `runner.ts:388` collapses to reusing M1 (or a no-op if already installed).
- [ ] **M3 — Pre-scan accuracy (`scanCommandNotFound`).** Suppress a `ToolMiss`
  for a tool the same trace later ran successfully (npm-script / `node_modules/.bin`
  / non-127 exit). Go unit tests with traces that fumble-then-succeed.
- [ ] **M4 — Gate honesty.** Detect declared-but-unrunnable gate commands and
  surface them (MR annotation / "unverified" delivery) so a green MR never implies
  tests that never ran. Tests for the ran / failed / skipped mapping.
- [ ] **M5 — Verified on a real JS run.** A run touching `web/` completes with the
  agent never hitting `command not found` for a gate tool and never running a
  manual `npm ci`; the judge pre-scan no longer false-flags tsc/vitest. Capture
  the evidence (activity log / review).
- [ ] **M6 — Docs.** Update the relevant docs/specs (`specs/ai.md` design note per
  repo convention; `CLAUDE.md`/`ARCHITECTURE.md` if the run lifecycle description
  needs it).

## The key decision: `--ignore-scripts` vs full install

`prepareCheckDeps` uses `npm ci --ignore-scripts` (blocks lifecycle scripts as a
supply-chain *reduction*). But `--ignore-scripts` leaves native deps unbuilt
(e.g. `esbuild`, which `vitest`/`vite` depend on), so some gates can still
half-fail — defeating the point.

**Recommendation: full `npm ci` (scripts allowed) under the `runner` uid** for the
pre-agent provision. Rationale: the `runner` uid *is* the sandbox for untrusted
code execution (cap-less, no credentials, PRD #51 M4), and the agent will execute
the repo's test/build code under that same uid anyway — so a postinstall script is
**not a new trust boundary**, just earlier. Keep `--ignore-scripts` only as a
defense-in-depth option if we later decide a hostile lockfile's install phase is a
meaningfully different risk from its test phase. (This PRD proceeds with
full-install-under-runner-uid unless the owner chooses otherwise.)

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

- **Untrusted install code.** Mitigated by reusing the existing runner-uid +
  scrubbed-env sandbox — the same boundary that already runs the repo's tests. Full
  vs `--ignore-scripts` is the decision above.
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
  !103); SVG rasterizer → dismissed (chromium covers it). Proceeding with
  full-install-under-runner-uid as the default pending owner confirmation.
