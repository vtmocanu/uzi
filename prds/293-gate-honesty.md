# PRD #293: Gate honesty — a run must not red a gate it never touched, green a gate it never ran, or read a failed shell command as success

**GitLab Issue**: [vtmocanu/uzi#293](https://github.com/vtmocanu/uzi/-/issues/293)
**Status**: Partially implemented on branch `agent/issue-293` (2026-08-10) — M1 (documented boundary + passthrough test), M3 (graceful-skip), M4 (tests) and M5 (docs) are done; M2 shipped as annotate-on-`js_deps` (the approved narrowed scope), with declared-gate reconciliation deferred, so this PRD stays open. See **Delivery status** below. (Originally: Draft, reviewed 2026-08-10; two independent code-verified reviews folded in.)
**Priority**: Medium (recurring run-quality tax; one member seen in 3 runs, two at `confidence: medium`)
**Created**: 2026-08-10
**Related**: `agent/src/sdk-messages.ts`, `agent/src/sdk-executor.ts`, `agent/src/toolchain-preflight.ts`, `agent/src/signals.ts`, `Taskfile.yml` (`gate:*` / `deadcode:*` targets, and the `{{.CLI_ARGS}}` ban), `.claude/rules/agent.md`
**Related issues**: #282 (command-not-found pre-scan false negative), #228 (govulncheck/npm audit scan a different build than they ship), #206 (knip unused-export tier), #201-class (a change not reaching already-seeded workers)

> Every file:line citation below was derived at **`81c98455`**. A line number without a
> SHA is not a citation — re-derive before acting.

## Problem

uzi's quality gating does not honestly track what actually executed and what is actually
present on the worker, so a run can reach a green MR through three distinct failure modes,
all observed in real runs by uzi's own retrospective judge:

- **False red** — a cross-component umbrella gate fails on a sibling component whose
  toolchain is absent, even when the change touched none of that component's files. On a
  Go-only change the reviewer ran `task deadcode`, which fans into `deadcode:web` →
  `npm run knip` (`Taskfile.yml:482-493`, `:1535`); `knip` was not installed
  (`sh: knip: not found`, exit 127 → task exit 201). A validator then burns turns
  reasoning past a red that carries no information about the change. (Seen in 3 runs.)
- **False green** — `signal_done` fires and opens an MR with the plan's declared test
  gates never run. The motivating case is `vitest` absent on a run that still shipped.
  (`confidence: medium`.)
- **The primitive under the first** — a failed shell command is tagged `is_error: false`.
  The shell reported `cd: api: No such file or directory`, and the tool result still read
  as a success. If a non-zero exit is not surfaced as an error, a gate that never ran is
  indistinguishable from a gate that passed to any retrospective pass over the trace.
  (`confidence: medium`.)

### Provenance

Every item is an open (`todo`) recommendation from uzi's retrospective judge, read via
`uzi review backlog --json` on 2026-08-10. Run ids for trace re-reads:

| Theme | Runs | Judge confidence |
|---|---|---|
| Cross-component gate reds on absent sibling toolchain | `b1fb0aee`, `13563aaf`, `2ebc093e` | low (×3) |
| `signal_done` with declared gates unrun | `bdc4c634` | medium |
| Failed shell command tagged `is_error: false` | `84b6a933` | medium |

### Root-cause map (traced at `81c98455`, corrected against code)

- **The Bash `is_error` seam is a passthrough uzi may not own.** The live executor is
  `SdkExecutor` (`sdk-executor.ts`); the `executor.ts:981/1006` sites are `StubExecutor`
  health-simulation literals and are irrelevant. The per-command `tool_result` is ingested
  by `mapUser` (`sdk-messages.ts:160`): `is_error: block["is_error"] === true` — a **pure
  reflection** of the value the upstream Claude Code Bash tool already stamped. The
  `tool_result` carries **no exit code**, and the repo wires only **PreToolUse** hooks
  (`guardrails.ts`, `sdk-executor.ts:535-544`), **no PostToolUse** hook to inspect an exit
  code post-hoc. `isErrorResult` (`sdk-messages.ts:268`, used at `sdk-executor.ts:1722`,
  `chat-executor.ts:464`, `judge-runner.ts:298`) is a **different function**: it classifies
  the whole-turn `result` frame and is consumed to **fail the entire run** — editing it
  would change abort behavior, not Bash-result tagging. So the classification defect may
  not be fixable in-repo without either re-deriving `is_error` from result content or an
  upstream change (see M1, which is scoped as a spike for exactly this reason).
- **Boot preflight is base-tools-only and not at completion.** `toolchain-preflight.ts`
  resolves five tools (`python3`, `go`, `gcc`, `pip`, `openssl`; `REQUIRED_TOOLS:23`) at
  **registration** (`worker.ts:36`) to fail a stranded-PVC worker loud (PRD #92). It does
  **not** cover `knip`/`vitest`, and a missing base tool fails registration — so `go` can
  never be the missing gate binary on a run that exists. The mechanism that knows a
  missing executable mid/post-run is the server-side **command-not-found pre-scan**
  (`protocol.ts:498`, `judge-runner.ts:371`, issue #282), which runs retrospectively to
  feed the judge. These are two different mechanisms; M2 targets the pre-scan, not the boot
  preflight.
- **There is no declared-gate set and no gate-execution ledger.** `submit_plan` carries
  freeform `plan_md` plus optional `{id,title}` milestones (`signals.ts:154-165`);
  `signal_done` carries `summary`/`prd_done_path`/`milestones_completed`
  (`signals.ts:117-149`). Nothing declares *which* test gates the plan promised, and no
  code labels an executed Bash command as a gate (grep of `agent/src` finds no
  gate-execution tracking). The machine-readable declared set is the Taskfile `gate:*` /
  `deadcode:*` targets, **not** the prose `.claude/agent-team.md` (which its own rules say
  teammates never read). M2/M3 must read the Taskfile, and both the declared set and the
  "what ran" record must be built.

## Solution

Make the run honest about three questions — *did this command run, did it succeed, and was
it even the right scope*. The three milestones are largely independent (the earlier
"M1 → M2" hard dependency was wrong): M2's primary never-ran detection is driven by the
pre-scan and command-absence, not by M1's flag; M1 only informs M2's secondary
"ran and failed" state.

## Milestones

- [x] **M1 — Bash tool-result error classification (a spike, then a fix if uzi owns it).**
      First reproduce and locate: determine whether the `is_error:false` on a failed
      command originates in uzi's ingestion (`mapUser`, `sdk-messages.ts:160`) or is stamped
      upstream by the Claude Code Bash tool (the current evidence says upstream, since
      `mapUser` only reflects). If uzi can re-derive it (e.g. from result content /
      `command not found` / `No such file` markers, or via a PostToolUse mechanism if one
      can be wired), do so **without** flipping legitimate non-zero *findings* (a linter or
      a failing test the agent ran on purpose) to errors — see Decision D1 and the TDD-loop
      risk. If uzi does not own the classification, the deliverable is a written finding
      that says so and points at the upstream fix, not a forced in-repo edit. Do **not**
      change `isErrorResult` — it is the run-abort classifier, not this seam.
- [ ] **M2 — Honest completion gate (three states, not two).** At the completion handoff in
      `sdk-executor.ts` (around the `turn.done` break at `:1242` and the "never push
      un-gated work" guard at `:1504`), reconcile the plan's declared gates against a
      gate-execution ledger and the server-side pre-scan's known-missing set. Model three
      states per declared gate — **ran+passed**, **ran+failed** (M1 informs this), and
      **never-ran** — and on any non-passed state do one of: block completion, downgrade
      the delivery to "unverified," or annotate the MR that gates were simulated rather than
      run (Decision D2; default to annotate to avoid reddening currently-green runs). This
      milestone owns the unscoped middle: plumb pre-scan results into the completion path,
      build the declared-gate extraction (from the Taskfile targets) and the "what ran"
      ledger. It does not live in `signals.ts` (schema only) or `toolchain-preflight.ts`
      (boot only).
- [x] **M3 — Cross-component gate: graceful skip that is surfaced honestly.** An umbrella
      gate must not red on a sibling component whose toolchain is absent when the change
      touched none of that component's files. Preferred mechanism (Decision D3):
      **graceful-skip with an honest surface**, following the existing precedent
      (`lint:shell` / `lint:yaml` / `lint:formula` already "missing tool → loud SKIP,
      exit 0, CI arms `UZI_LINT_*_REQUIRED`", `Taskfile.yml:532-552`, `:628-651`) and
      extending it to `deadcode:web` (bare `npm run knip` today, `:1535`). The skip must be
      surfaced honestly (banner + a `*_REQUIRED` arming in CI) so a skipped gate and a
      passing gate do not look alike — which couples M3's correctness to the same honesty
      guarantee as M2. Rejected alternatives, with reasons, so they are not re-proposed:
      **scope-to-touched** conflicts with the Taskfile's documented `{{.CLI_ARGS}}` ban (a
      caller must not narrow the gate's scope); **prewarm** targets per-repo `node_modules`
      (not the baked `/opt/uzi-toolchain`), collides with the documented `npm ci`/`install`
      host-rewrite of `/opt/homebrew/bin/agent-browser` (`.claude/rules/agent.md`), and is
      defeated on already-seeded workers by the seed-nix-once PVC behavior.
- [x] **M4 — Tests** covering the surfaces that landed: a failed command classified as an
      error if M1 fixed it in-repo (else a test asserting the documented upstream boundary);
      `signal_done` blocking/annotating on an unrun or failed declared gate (M2); and an
      umbrella gate loud-skipping rather than red-ing on an absent sibling toolchain (M3).
      Follow the repo's `node --test` / Go test conventions per `.claude/rules/agent.md`.
- [x] **M5 — Docs + CHANGELOG.** Record the completion-gate posture and the
      graceful-skip surface where the relevant rule files already document gate reading
      (`.claude/rules/agent.md` / `.claude/rules/go.md` as applicable), and add a CHANGELOG
      `[Unreleased]` entry.

## Delivery status (branch `agent/issue-293`, 2026-08-10)

What actually shipped, where it diverges from the milestone text above:

- **M1 — done as a documented boundary (no in-repo `is_error` flip).** The spike confirmed
  uzi does not own the value: `mapUser` (`agent/src/sdk-messages.ts:160`) is a pure
  passthrough of the SDK-stamped flag, the `tool_result` carries no exit code, and only a
  PreToolUse hook is wired (no PostToolUse). Re-deriving from content would reintroduce the
  `echo "…command not found"` false positive the server-side scanner (`judge.go`
  `scanCommandNotFound`, specs/ai.md §400) is engineered around, and the only consumer of
  the per-Bash flag is presentational (`web/src/components/RunEvent.tsx:731`). Decision:
  leave it a passthrough, document the boundary (specs/ai.md), pin it with a test
  (`agent/test/sdk-messages.test.ts`). `isErrorResult` deliberately untouched. This is
  success-criterion 1's documented-boundary branch.
- **M2 — shipped as annotate-on-`js_deps`; declared-gate reconciliation DEFERRED (box left
  unchecked).** `ExecutorResult.gatesUnverified` is populated at the completion handoff from
  `depsResults.filter(r => !r.ok && r.manager !== "none")` (issue-run only, omitted-not-`[]`,
  `safeDirLabel`-clamped) and rendered as a "⚠️ Quality gates unverified" note on the
  issue-run MR body (`agent/src/runner.ts`), ANNOTATE posture (never blocks; byte-identical
  when empty). This meets success-criterion 2 for the motivating never-ran case (deps absent
  ⇒ vitest/knip could not run). **Not built** (hence the box stays unchecked): the Taskfile
  declared-gate extraction, the per-command "what ran" ledger, and pre-scan plumbing — the
  command-not-found pre-scan is server-side/retrospective (`api/internal/workersvc/judge.go`,
  judge-only) and not available at the lead worker's completion, and no execution ledger
  exists; `js_deps` is the repo's own recorded completion signal (specs/ai.md §399/§400). A
  follow-up can build the full three-state reconciliation on top of this annotation.
- **M3 — done.** `deadcode:web`/`deadcode:agent` route through `scripts/deadcode-knip.sh`:
  knip present ⇒ `npm run knip`; absent ⇒ loud SKIP + exit 0, with CI arming
  `UZI_DEADCODE_{WEB,AGENT}_REQUIRED=1` (`.gitlab-ci.yml`) ⇒ exit 2 (the `lint:formula`
  precedent). Rejected alternatives recorded in specs/ai.md.
- **M4 — done.** Tests pin the M1 boundary, the M2 population (including the `manager:"none"`
  exclusion) and the MR rendering (`agent/test/{sdk-messages,sdk-executor,runner-push-mr}.test.ts`).
  No bespoke shell test for the wrapper — its sibling gate-scripts (`lint-formula.sh`,
  `lint-shell.sh`) have none and are validated by CI.
- **M5 — done.** specs/ai.md decision section, CHANGELOG `[Unreleased]`, and
  `.claude/rules/{agent,web}.md` gate-cheat-sheet notes.

**Rollout caveat (#201-class):** the M1/M2 agent-code changes ship in the worker image, so
they reach only newly provisioned/re-provisioned workers; the M3 Taskfile/CI change takes
effect immediately in CI and any fresh checkout.

## Decisions to make in the plan

- **D1 — is_error scope (M1).** If re-derivation is possible: which failures flip
  `is_error` — shell-level failures (`cd`, `command not found`) only, or any non-zero exit?
  The former avoids the TDD-loop risk; the latter is broader but noisier. Resolve Open
  question 1 first.
- **D2 — completion-gate posture (M2).** Block, downgrade-to-unverified, or annotate. A
  hard block changes existing run behavior (it can turn today's green runs red) and must be
  confirmed before shipping; default to annotate.
- **D3 — cross-component mechanism (M3).** Graceful-skip-with-honest-surface is the
  recommended default (precedent + repo-consistent); scope-to-touched and prewarm are
  rejected above. Confirm before adopting either rejected option.

## Open questions

1. **Does uzi own the Bash `is_error` value at all?** Current evidence (mapUser is a
   passthrough, no exit code on the result, no PostToolUse hook) says the SDK/Claude Code
   Bash tool stamps it upstream. M1 must reproduce and confirm before committing to an
   in-repo fix vs an upstream finding.
2. **Block or annotate by default (D2)?** Owner decision; default to annotate.
3. **Where does the "what ran" ledger live** so both M2 and any future gate accounting can
   read it? M2 introduces it; confirm the seam (completion path in `sdk-executor.ts`).

## Parallelization plan

| Phase | Milestones | Depends on | Files touched (distinct) |
|---|---|---|---|
| 1 (parallel) | **M1** (spike: SDK ingestion) · **M3** (graceful-skip in Taskfile) | — | `agent/src/sdk-messages.ts` / `sdk-executor.ts` (M1 read/spike) vs `Taskfile.yml` (M3) |
| 2 | **M2** (completion gate) | pre-scan plumbing (not M1); M1 informs only the ran+failed state | `agent/src/sdk-executor.ts` + pre-scan wiring |
| 3 | **M4** (tests) · **M5** (docs) | M1–M3 | test files + rule/CHANGELOG docs |

M1 and M3 touch disjoint files and run in parallel. M2 depends on pre-scan plumbing rather
than on M1; it can start alongside M1 and consume M1's ran+failed signal if/when it lands.

## Risks and mitigations

- **M1 over-flips a legitimate non-zero result.** A linter or a failing test the agent ran
  on purpose exits non-zero to *report*, not to fail; flipping those to `is_error` makes
  the agent read a working tool as broken. Mitigate via D1 (shell-failures-only), cover in
  M4.
- **M1 injects noise into the very trace it aims to clean (the TDD loop).** The agent's
  write-test → run → see-red → fix loop produces non-zero exits by design. Under a
  flip-on-any-non-zero rule, every intermediate failing test becomes `is_error:true` in the
  persisted trace the judge reads — new false positives in exactly the retrospective tooling
  this PRD exists to make honest. (Verified today `mapUser`'s per-result `is_error` is
  consumed only presentationally in `web/src/components/RunEvent.tsx` plus the persisted
  trace — no loop-detector/limit/run-fail consumer — so this is a signal-quality risk, not a
  behavior-break.) Mitigate via D1.
- **M2 hard-block reddens currently-green runs.** Behavior change; default to
  annotate/downgrade and confirm before any block.
- **Delivery caveat (#201-class): a change may not reach already-seeded workers.**
  `toolchain-preflight.ts`'s header documents that seed-nix tars `/nix` into the per-worker
  PVC exactly once, so an already-seeded worker never re-seeds — which defeats any prewarm
  (a reason M3 rejects it) and means the M1/M2 agent-code changes reach only newly
  provisioned workers unless the rollout re-provisions. State the rollout in M5.
- **Reading the declared-gate set from the wrong place.** The declared set is the Taskfile
  `gate:*` / `deadcode:*` targets, not `.claude/agent-team.md` (prose/archive, unread by
  teammates). M2/M3 must read the Taskfile so the fix generalizes to any repo uzi runs on.

## Success criteria

1. A shell command that fails (`cd` into a missing dir, a missing binary) produces a
   persisted `tool_result` whose `is_error` is `true` — verifiable against the stored flag
   in the trace, not against LLM behavior — OR, if M1's spike shows uzi does not own the
   value, a written finding documents the upstream boundary and M4 asserts the current
   passthrough behavior.
2. `signal_done` on a run whose declared gates did not pass (never-ran or ran-and-failed)
   does not silently produce a green-looking MR: it blocks, downgrades, or annotates per
   D2, verifiable in the run feed / MR.
3. An umbrella gate run on a change that touches only one component loud-skips (with a
   `*_REQUIRED`-style surface), rather than red-ing, on a different component whose
   toolchain is absent.
4. New tests cover the surfaces that landed, and the repo's own gate stays green.
