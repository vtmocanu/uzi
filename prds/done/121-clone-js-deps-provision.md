# PRD #121: Pre-provision cloned-repo JS deps before the agent works (+ pre-scan accuracy + gate honesty)

**GitLab Issue**: [#121](https://gitlab.example.com/vtmocanu/uzi/-/issues/121)
**Status**: **COMPLETE (2026-07-27).** M1, M2, M3 and M6 shipped in v0.11.9; **M5, the acceptance run, is satisfied** on run `71d83432` against a worker verified to be running this code (see its milestone entry for the measured evidence). **M4 is SPLIT OUT** into its own increment and is deliberately not delivered here.

*It took two acceptance runs, and the first is the more instructive: every mechanism worked and the benefit was still lost, because nothing told the agent its dependencies already existed. Fixed by issue #157 and shipped in v0.11.11.*

*The substantive output was never the feature. The PRD's Trust posture premise — that a frozen `--ignore-scripts` install runs no repo-authored code — was found **FALSE for yarn and pnpm**, measured, and that section is rewritten. `specs/human.md` carries the resulting constraint as user-ratified contract.*

*The substantive output of implementation was not the feature. The PRD's Trust posture premise — that a frozen `--ignore-scripts` install runs no repo-authored code — was found **FALSE for yarn and pnpm**, measured, and that section is rewritten. `specs/human.md` now carries the resulting constraint as user-ratified contract.*

*Earlier status, retained because the decisions still bind: Draft (created 2026-07-22; revised same day after a fable adversarial review — reversed the install-flags decision to keep `--ignore-scripts` (the esbuild premise was empirically false, tested), rescoped M4 as net-new machinery, fixed the M2 hoist point to after `provisionRunTools`, added the trust-posture rationale and a k8s single-uid hedge; see the Decision Log).*
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
   (`api/internal/workersvc/judge.go`) is a judge-time regex scan of the
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
     depth/count), and pick the installer per lockfile; a root `workspaces` monorepo
     resolves to a single root install.

     *The four command strings drafted here (`pnpm i --frozen-lockfile`, `yarn
     --frozen-lockfile`, `bun install`, …) are **superseded** and were removed
     2026-07-27 rather than corrected. Not one of the three non-npm strings matched what
     shipped, and a fact-check found that only the bun one had been caught. Quoting
     command strings in a design document means maintaining a second copy of
     `INSTALL_COMMANDS` that nothing tests — **`agent/src/js-deps.ts` is the source of
     truth**, and the Trust posture table above carries the security-relevant flags with
     the reason each is there.*
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

   **Clarification added 2026-07-26 after implementation, because this wording misleads.**
   "An `npm run <script>` … that wraps it" reads as though the wrapping alone is what
   suffices. It is not. Suppression of a wrapped tool depends **entirely on the package
   manager's echo (`> tsc --noEmit`) surviving into the captured `tool_result`** — it is
   never inferred from "npm ran green". Measured: the same trace with real npm output
   suppresses `tsc`, and with a hand-written empty `tool_result` does not. The code is
   correct and documents its residual; this sentence was the thing that could have sent
   someone looking for an inference the implementation deliberately does not make.
3. **Gate honesty.** When a plan *declared* gate commands that ended up
   **unrunnable** (install failed: no registry egress, lockfile drift), don't let
   the run `signal_done` with a green MR implying tests ran — annotate the MR /
   downgrade the delivery to "unverified". The post-run check phase already
   distinguishes "ran+failed" from "skipped" (self-improve's honest skip); surface
   that distinction. Part 1 makes gates runnable in the common case; Part 3 is the
   safety net for when install genuinely can't happen.

## Implementation Milestones

- [x] **M1 — Generalized dependency discovery + install (extract from
  `prepareCheckDeps`).** A package-manager-aware, lockfile-driven installer that
  discovers JS project dirs in a clone (bounded), runs the **frozen `--ignore-scripts`
  install** (*as drafted this said "unchanged from `prepareCheckDeps` — a pure relocation,
  no flag change". **That stopped being true during implementation** and the Decision Log
  records why: yarn gained `YARN_IGNORE_PATH=1`, pnpm gained `--ignore-pnpmfile` and
  `--config.manage-package-manager-versions=false`, and bun gained `--frozen-lockfile`.
  The relocation was pure until the security correction; it is not now.*)
  under the runner uid + scrubbed env, and returns per-dir install/skip results. Unit
  tests over fixture clones (npm/pnpm/yarn/bun, monorepo workspaces, no-lockfile,
  install-failure → honest skip).
- [x] **M2 — Pre-agent provisioning at the right lifecycle point, with defined join
  semantics.** Call the M1 installer inside the executor **after `provisionRunTools`
  (the call site in `sdk-executor.ts`) and before the plan turn** — NOT at `runner.ts:~210`,
  which predates `toolEnv` and would use the image's npm instead of the run's
  provisioned node. Kick it off async so it **overlaps the plan turn** (and, on
  human-gated runs, the `awaiting_approval` wait), then **join it before the first
  implement turn** so the agent never races it and never collides with an
  agent-initiated `npm ci` in the same dir (npm has no cross-process `node_modules`
  lock). `prepareCheckDeps` at its `claim.kind === "self_improve"` call site in `runner.ts` collapses to reusing M1. **Wall-clock:**
  near-zero for human-gated runs (hidden behind the approval wait); for **autopilot**
  (`gatePlan`'s autopilot branch in `runner.ts` short-circuits with no wait) the overlap window is
  only the plan turn, so a slow install can add real wall-clock before implement —
  acceptable, and stated honestly rather than claimed away.
- [x] **M3 — Pre-scan accuracy (`scanCommandNotFound`).** Suppress a `ToolMiss` for a
  tool the same run later ran successfully. **RESOLVED: widened the query to the invocation
  side.** As drafted, this note said `judgeSignal` "today fetches only `tool_result`
  payloads (`ListToolResultPayloadsForRun`)" and left the choice open between widening and a
  text heuristic over the payloads. *(That sentence is now historical — the query it names
  no longer exists; it was replaced by `ListToolTraceForRun`, which returns `seq, kind,
  payload` for `tool_use` and `tool_result` ordered by `seq ASC`.)*

  The heuristic option was ruled out on a fact that settles it: **the command lives only in
  `tool_use`** — `tool_result` carries `{tool_use_id, content, is_error}` and no command —
  and **a successful `tsc --noEmit` prints nothing**, so its `tool_result` has no text to
  heuristic over. A heuristic could therefore only ever catch the npm-wrapper arm, leaving
  direct invocation false-flagged permanently — and direct invocation gets *more* common
  after M1/M2, not less.

  A latent defect surfaced while ruling on it, worth keeping: the old query carried
  `ORDER BY seq ASC` in SQL and then **discarded the guarantee at the type boundary** by
  returning `[][]byte`, so "later" meant "larger slice index" and folding `ASC→DESC`
  reddened nothing. Under the widened row type `seq` enters the function and "later" is
  assertable. Go unit tests use authored fumble-then-succeed traces rather than a captured
  one, deliberately: after M2 lands, real traces stop fumbling, so a snapshot fixture would
  have nothing to suppress and would read as full coverage while discriminating nothing.
- [ ] **M4 — Gate honesty (net-new machinery — heaviest milestone; may split).** This
  is NOT just surfacing an existing signal: the ran/failed/skipped mapping today lives
  only in self-improve's `defaultCheckRunner` (`defaultCheckRunner` in `self-improve.ts`) for hardcoded
  uzi checks on self_improve runs, and nothing anywhere extracts or tracks a *plan's
  declared gate commands*. M4 needs net-new work: extract the gate commands a plan
  declares, track whether each actually ran, and annotate the MR / downgrade the
  delivery to "unverified" when they didn't. Given the cost, consider shipping M1–M3
  first and splitting M4 into its own increment/PRD.

  **→ SPLIT OUT 2026-07-26**, taking the option this bullet offers. Not deferred for
  cost: **M4's own premise is unmeasured.** It is the safety net for runs where the
  install genuinely cannot happen, and nobody knows how large that residual is until M5
  runs against real traffic — building the net before measuring the fall is the wrong
  order. It also turns on a *product* decision rather than a design one (reliable gate
  extraction needs `submit_plan` to gain a structured `gates` field, which changes **what
  a human approves at the plan gate**), and it touches the exact `sdk-executor.ts` and
  `runner.ts` regions M2 restructured. Free-text extraction was assessed and **rejected in
  both directions**: it harvests `git checkout -b` out of fenced blocks as a "gate" (false
  banners, and reviewers learn to ignore a banner that cries wolf — which destroys the only
  thing M4 ships), and it yields zero gates from "I'll run the repo's test suites", passing
  **vacuously** — the precise failure M4 exists to prevent. Full design preserved for the
  follow-up increment.
- [x] **M5 — Verified on a real JS run.** A run touching `web/` completes with the
  agent never hitting `command not found` for a gate tool and never running a
  manual `npm ci`; the judge pre-scan no longer false-flags tsc/vitest. Capture
  the evidence (activity log / review).

  **→ POST-DEPLOY, and not completable in the increment that writes the code.** It needs
  the `agent/` changes built into the worker image and deployed: merge → `v*` tag → Harbor
  publish → ArgoCD sync to dev-cluster. **Do not tick this from unit tests.** Discovery
  finding the right dirs and the install succeeding under the runner uid with the scrubbed
  env are different claims, and only the second one is M5. A live uzi instance is reachable
  via the `uzi` CLI, so the verification itself is cheap once deployed.

  **⚠ A GREEN `./e2e/run-e2e.sh` DOES NOT COVER THIS MILESTONE, and the branch shipped with
  one.** Measured 2026-07-26: e2e passed 201/0 on this work, and it executed **zero lines of
  the agent-side change.** The harness runs `UZI_E2E_EXECUTOR=stub` → `StubExecutor`, while
  the entire M2 install lives in `SdkExecutor.execute`. Recorded here because a 201-PASS e2e
  sitting next to a PRD whose headline criterion is about agent behaviour is precisely the
  artifact a later reader cites as proof — and it would be a false claim, from a genuinely
  green gate, with nothing in the gate's own output to reveal it.

  **→ SATISFIED 2026-07-27, on run `71d83432` (issue #116), against a worker verified to be
  running this code.** Both criteria measured on the complete 887-message trace:

  | criterion | result |
  |---|---|
  | zero agent-initiated `npm ci` | **0** install commands, whole run |
  | zero gate-tool `command not found` | **0** real failures |
  | provisioning fired | `seq 3` kick-off → `seq 67` *"installed JS dependencies in agent, web"* |

  The agent ran roughly 45 gate invocations across **both** provisioned workspaces (`npm test`,
  `npm run typecheck`, `npx vitest`, `npm run build` in `web/`; `npm test`, `npm run typecheck`,
  `node --test` in `agent/`) without a missing tool and without preceding any of them with an
  install. Issue #116 was chosen over a web-only PRD precisely so both workspaces would be
  exercised.

  **The deploy chain was verified link by link rather than assumed**: tag `v0.11.11` → commit
  `4ab9d58d` → worker reporting `0.11.11+g4ab9d58d`, with the #157 merge confirmed an ancestor
  of the tagged tree.

  **This took two runs, and the first one is the more instructive.** On v0.11.9 the install
  worked perfectly — both workspaces provisioned before the agent's first tool call, zero
  gate-tool `command not found` — and the agent ran `npm ci` twice anyway, because **nothing
  told it the deps existed**. Its plan said *"npm ci (fresh worktree has empty node_modules)"*,
  which was true when written: the plan turn precedes the join. Since `npm ci` deletes
  `node_modules` first, it destroyed the provisioned tree and rebuilt it, so the feature's
  entire wall-clock benefit was lost while every mechanism in it worked. Issue #157 fixed the
  missing half by telling the agent, and this run's plan never mentions an install.

  **One methodology note, because an instrument lied during the analysis.** An interim pass
  flagged `eslint: not found` and it was uzi's own fixture text at
  `judge_prescan_test.go:164` — the agent was *reading* the PRD #121 pre-scan tests. A regex
  over message payloads cannot distinguish a shell error from source quoting one. The verdict
  above counts only `tool_result` entries with `is_error: true`, which cannot match source
  text. **A weaker instrument would have failed this milestone on a false positive.**

  What e2e *did* prove is worth stating so the run is not dismissed either: the judge funnel
  is still full-wire green with the widened `ListToolTraceForRun` inside `judgeSignal`, and
  the merge broke no part of the run lifecycle. Note also that e2e does **not** exercise the
  pre-scan's suppression path — the `jq: command not found` plant that would have was dropped
  by PRD #97 M4.
- [x] **M6 — Docs.** Update the relevant docs/specs (`specs/ai.md` design note per
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
defense-in-depth posture (the `buildCheckEnv` security comment block, now in `agent/src/sdk-env.ts`, documents it as a supply-chain
*reduction*), it matches what the extracted routine already does (M1 becomes a pure
relocation, no install-behavior change), and it works for the toolchain that motivated
this PRD. The exception is a package with a *genuine* native postinstall (e.g.
`better-sqlite3`, `bcrypt`); for those the agent can run the build explicitly under the
same runner uid (which already executes the repo's test code) — a rare, per-run
escalation, not the default. Full-scripts install is NOT adopted.

## Trust posture — why auto-install is OK by default

> **REWRITTEN 2026-07-26, during implementation. The original version of this section was
> WRONG, and the error was load-bearing.** It argued that auto-install clears uzi's
> repo-borne-config bar "*because of the `--ignore-scripts` default*: … no repo-authored
> script runs". That premise is **false for yarn and pnpm**. It was believed by everyone
> who read it, including the coder who then wrote the same claim, as an absolute, into the
> module header; two validators found it independently, each by a different route.
>
> Rewritten rather than annotated, because the defect was not a missing caveat. A reader of
> the old text concluded the safety property *falls out of* `--ignore-scripts` — precisely
> the belief that produced the bug, and the one that invites the next person to drop a
> mitigation as redundant.

Repo-borne provisioning config is deliberately gated in uzi: `repo_devbox_opt_in`
(migration `00047`) is per-repo opt-in, default OFF, and a repo's devbox
`init_hook`/scripts are NEVER executed by a run. Auto-running a repo's lockfile install
pre-approval must clear the same bar.

**`--ignore-scripts` alone does not clear it.** It suppresses *lifecycle* scripts, but
every package manager except npm has a second, separate channel by which a repo-committed
file becomes executable code during `install`. Measured offline (`--network none`) inside
the pinned `node:22-alpine` base image, running the exact commands this feature issues:

| manager | repo-controlled vector | under `--ignore-scripts` alone | what closes it |
|---|---|---|---|
| yarn 1.22.22 | `.yarnrc.yml` `yarnPath:` (also classic `.yarnrc` `yarn-path`) | **repo JS executed, exit 0** — before any flag is parsed | `YARN_IGNORE_PATH=1` |
| pnpm | `.pnpmfile.cjs` | **executed** | `--ignore-pnpmfile` |
| pnpm | `packageManager:` + `manage-package-manager-versions` (on by default) | **fetches and executes a declared pnpm** | `--config.manage-package-manager-versions=false` |
| bun | the repo's own `postinstall` | ran without the flag | `--ignore-scripts` — here it is the *only* thing stopping it |
| npm | `packageManager:` handoff | no handoff | — |

The yarn case is the sharpest: `yarnPath` wins over the corepack `packageManager` check, so
the layout `yarn set version berry` produces **is** the exploit, not a contrived fixture.
`enableScripts: false` is irrelevant there, because the release file *is* the code.

**The bar is therefore cleared by the mitigations in that right-hand column, not by
`--ignore-scripts`.** Each is load-bearing; none is redundant. Removing any one re-opens a
path by which a cloned repo executes its own code, pre-approval, on every run touching that
manager. The install still runs under the same `runner` uid + scrubbed env that already
executes the repo's tests, so the blast radius stays bounded — but bounded blast radius was
never the claim this section was making.

Two limits, stated because their absence is what made the old text dangerous:

1. **This is not an exhaustive audit of any package manager, and NO SINGLE ENVIRONMENT HAS
   EXECUTED ALL FOUR.** That is the sharpest limit here and it is easy to miss, because the
   coverage looks complete when the reports are read together. The shipped worker image has
   **npm and yarn** and lacks pnpm and bun; the machine that ran the independent installer
   validation had **npm, pnpm and bun** and lacked yarn. So the manager the shipped image
   actually uses is the one whose install arm was never executed end-to-end by the validator
   — `YARN_IGNORE_PATH` is pinned as argv+env by a mutation fold, and the vector probes were
   run inside `node:22-alpine` by other agents, but that is two partial coverages rather than
   one complete one. **Treat yarn as the residual that matters most.**

   It is the set of vectors three agents probed. What was deliberately NOT probed, recorded
   as clearly as what was:
   **bun entirely** (no binary in the image, so the bun row rests on a single uncorroborated
   probe; `bunfig.toml` `preload` is the obvious untested candidate — this is the largest
   gap); **corepack shims** (present in the image but not enabled — `corepack enable` would
   make `packageManager` drive a download-and-execute for yarn *and* npm); **a real Berry as
   the `yarn` on PATH** (`YARN_IGNORE_PATH` is a yarn-1 mechanism and Berry's behaviour is
   untested. *Corrected 2026-07-27: this previously said "Berry's rejection of
   `--frozen-lockfile` makes an honest failure the likely outcome". **Berry ACCEPTS
   `--frozen-lockfile`** as a hidden deprecated alias for `--immutable` — in
   `plugin-essentials`' `install.ts` it is declared `Option.Boolean('--frozen-lockfile',
   {hidden: true})` and passed to `reportOptionDeprecations` with a callback setting
   `immutable`, carrying **no `error` key**, so unlike `--production` it does not abort and
   the install proceeds. Using a false rejection to bound a SECURITY residual is the worst
   place to have got this wrong. Note also that Berry defines its own `ignorePath` boolean
   consumed at `checkYarnPath`, whose env form is `YARN_IGNORE_PATH` — so the mitigation
   plausibly carries onto Berry as well. That is a source read, not an execution; the
   "untested" caveat stands, now without the false reasoning.*); and
   **lockfile-embedded specifiers** (`git:`, `file:`, `link:`,
   arbitrary `resolved` URLs) plus `.npmrc` registry redirection.

   **`--ignore-scripts` bounds what runs at install time, not what the install PLACES on
   disk.** Attacker-chosen code landing in `node_modules` executes when the agent later runs
   a gate. That is not a *new* exposure — the repo's own test files already execute under
   the same uid — but it is a different claim from "no repo code runs", and conflating the
   two is how this section went wrong the first time.
2. **If we ever default to a full-scripts install, the reasoning breaks entirely** and
   auto-install must become opt-in. Recorded so the tradeoff is never silently crossed —
   and note that the original text carried exactly that sentence *while its main claim was
   already false*, which is why "we wrote the tradeoff down" is not by itself a defence.

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

- **Untrusted install code.** *(Corrected 2026-07-27. This bullet previously read
  "Mitigated by `--ignore-scripts` (no repo-authored script runs)" — the exact claim the
  Trust posture section above rewrites as FALSE, left standing in the same file by the
  author of that rewrite. It is the third copy of this claim found on this branch, after
  the module header and the Decision Log entry, and the one nobody chased. **A retraction
  propagates only as far as the people holding copies, and grepping for the retracted
  SENTENCE is what leaves the copy that paraphrases it.**)*
  Mitigated by the **per-manager mitigations in the Trust posture table above** —
  `--ignore-scripts` alone does not hold this line for yarn or pnpm — plus the existing
  runner-uid + scrubbed-env sandbox, the same boundary that already runs the repo's tests.
  **Caveat:** on a `#58` single-uid start there is no
  runner-uid split (`agent/src/sdk-env.ts`, the single-uid branch of the `buildCheckEnv`
  security comment block — *the fix that replaced this bullet's original stale line number
  named `self-improve.ts`, which is where that block used to live before M2 moved it; a
  second fact-check pass caught it. Correcting a stale anchor by hand is itself a claim
  about where code lives now*), so isolation is weaker there; since k8s
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
- **The overlap leaves a plan-turn race that CANNOT be closed, only named** (found
  2026-07-26 in the M2 architectural-fit pass). M2 joins the install before the first
  implement turn, so the agent never races it *while implementing*. But the **plan turn**
  runs with `permissionMode: "bypassPermissions"` and full Bash, and `guardrails.ts` has
  **no package-manager rules of any kind** (verified: `grep -c` for `npm|pnpm|yarn|bun
  install` returns 0). So a planning agent exploring the repo can run its own `npm ci` in
  the same directory while the worker's install is mid-flight — the exact `node_modules`
  corruption the join exists to prevent, moved one phase earlier.

  **This is inherent to the design, not an oversight.** The overlap with the plan turn *is*
  the wall-clock argument for the whole feature; closing the race means giving up the
  benefit. So it is documented rather than fixed.

  The failure is worse than "no deps", which is why it is worth naming rather than
  shrugging at: a half-written `node_modules` still EXISTS, so `defaultCheckRunner`'s
  `requires: "node_modules"` pre-flight passes, the check runs against a corrupt tree, and
  it reports a real-looking failure — "accusing good code of failing", the precise outcome
  `self-improve.ts` is written to prevent. It shares that coupling with a separate
  pre-existing bug found three times independently during this PRD (a *failed* `npm ci`
  also leaves the directory behind, and the same pre-flight passes); both are filed
  separately.

  Exposure is one turn, and the blast radius is a failed install rather than a security
  property. Mitigating it properly would mean either a guardrail denying package-manager
  invocations during the plan turn, or a lock the agent's own `npm` would have to respect —
  neither is in scope here.

## Related Work

- **PRD #51 M4** — the runner uid + scrubbed env that already sandboxes `npm ci`
  and repo test execution (the trust boundary this PRD reuses).
- **PRD #46 Decision 4** — the deterministic command-not-found pre-scan
  (`scanCommandNotFound`) M3 refines.
- **`self-improve.ts` `prepareCheckDeps`** — the install routine M1 extracted and
  generalized. *Past tense as of 2026-07-27: the function no longer exists — M2 deleted it
  rather than leaving a shim, because two install paths for one job is the drift this PRD
  removes. Its post-agent call site in `runner.ts` now calls `installJsDeps`. This entry
  said "the **existing** install routine" and "its **current** call site" until the
  fact-check caught it: a present-tense claim about deleted code is a wrong doc, not a
  typo, by the rule this branch adopted.*

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
  **⚠ Item (4) of this entry is now RETRACTED in part** — "`--ignore-scripts` clears the
  bar without a new opt-in" is false as stated; see the 2026-07-26 entry below. Left in
  place rather than edited, because the entry is the record of what was decided that day
  and the retraction is more useful than a clean-looking log.

- 2026-07-26 — **Implemented. M1–M3 landed; M4 split out; the trust-posture premise was
  found false and closed.** Decisions taken during implementation, in descending order of
  consequence:

  1. **The Trust posture section's central premise was FALSE, and is rewritten above.**
     `--ignore-scripts` does not mean "no repo-authored code runs": yarn execs a
     repo-committed `yarnPath` before parsing any flag, and pnpm executes `.pnpmfile.cjs`
     and (via `manage-package-manager-versions`) a `packageManager`-declared pnpm. Found
     independently by a reviewer and an auditor, each measuring offline inside the pinned
     `node:22-alpine` image; a third measurement by the coder confirmed all four managers
     and turned the auditor's *inferred* second pnpm vector into a fact. **Ruling: restore
     the premise, do not renegotiate it** — crossing that tradeoff is a decision about
     uzi's security posture and belongs to the owner, not to a milestone. Closed with
     per-manager mitigations; all four managers retained, none dropped. Consequence for the
     module's contract: it now adds exactly one env key on exactly one arm (`YARN_IGNORE_PATH`),
     where previously it added none — pinned per-manager by a test so the property stays
     checkable rather than becoming folklore.
  2. **M4 split into its own increment**, exercising this PRD's own pre-authorization.
     M4 is the safety net for a residual whose size is unknown until M5 measures it, and
     its load-bearing dependency is a *product* question (`submit_plan` gaining a structured
     `gates` field changes what a human approves at the gate). Free-text extraction of
     declared gates was assessed and rejected in both directions: it harvests `git checkout
     -b` out of fenced blocks as a "gate" (false banners, which reviewers learn to ignore)
     and yields zero gates from "I'll run the repo's test suites" (passes vacuously — the
     exact failure M4 exists to prevent). Full design preserved for the follow-up increment.
  3. **M3 widens the query to the invocation side** rather than using a text heuristic. The
     deciding fact: the command lives only in `tool_use`, and a successful `tsc --noEmit`
     prints nothing, so `tool_result` has no text to heuristic over. A latent defect
     surfaced on the way: the old query ordered by `seq` in SQL and then discarded the
     guarantee by returning `[][]byte`, so folding `ASC→DESC` reddened nothing.
  4. **M5 is a POST-DEPLOY step**, not completable in the PRD run that writes the code: it
     needs the worker image built and deployed (merge → `v*` tag → Harbor → ArgoCD). Ticking
     it from unit tests would conflate "discovery finds the right dirs" with "the install
     succeeds under the runner uid with the scrubbed env".
  5. **The post-agent self-improve install stays UNCONDITIONAL.** A skip guard was proposed
     and then retracted after a case analysis: `npm install <pkg>` reconciles `node_modules`
     in place (a presence probe is *correct* there), a hand-edited `package.json` leaves the
     lockfile untouched (so a lockfile-diff guard skips it), and `npm ci` refuses with
     `EUSAGE` while `node_modules` SURVIVES — so that case is already broken today,
     independent of this PRD. Every predicate is wrong on some case, and a wrong skip runs
     the checks against stale deps, producing a failure that looks real and is the harness's
     fault. Correctness dominates the bounded latency of one extra install on one run kind.
  6. **`--frozen-lockfile` added to the bun arm**, where the milestone bullet said bare
     `bun install`: a non-frozen install rewrites the lockfile *in the clone*, dirtying the
     tree the agent diffs and the MR shows. The M1 prose already said "the frozen
     `--ignore-scripts` install", so the bullet was an oversight, not a decision.
  7. **Deps-ready is corroborated only for projects that DECLARE dependencies.** An
     unconditional `node_modules` existence check was instructed and correctly refused:
     `npm ci` on a zero-dependency project exits 0 creating no `node_modules`, so the
     unconditional form turns a genuine success into a reported failure — the same lie as a
     false ready, pointing the other way.
  8. **`prepareCheckDeps` deleted, not shimmed.** Two install paths for one job is the drift
     this PRD removes, and its hardcoded `["web","agent"]` was the bug.

  Filed separately, each found independently by two or more agents: the
  `requires: "node_modules"` pre-flight passing on a *failed* install (three discoveries);
  and `defaultCheckRunner` carrying the same `execFile`+`timeout` defect under the uid split
  that this PRD fixed in `js-deps` (measured: the timeout kills from the worker uid and gets
  `EPERM`, leaving the runner process alive past its cap).
