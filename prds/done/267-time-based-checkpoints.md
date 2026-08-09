# PRD #267: Time-based checkpoints — bound data-loss exposure between milestone boundaries

**GitLab Issue**: [vtmocanu/uzi#267](https://gitlab.example.com/vtmocanu/uzi/-/issues/267)
**Status**: Complete — implemented and reviewed 2026-08-09 (all milestones M1–M4 shipped on `agent/issue-267`; auditor CONFIRMED the credential-safety invariant on the new `reap:false` publish path; the shipped env is `CHECKPOINT_INTERVAL`, a Go-style duration string). Originally architect-reviewed 2026-08-09 (crux CONFIRMED GO via PRD #122 Decisions 10b/14).
**Priority**: High (top priority)
**Created**: 2026-08-09
**Related**: PRD #122 (milestone-structured runs — M6 milestone checkpoint, M8 brokered origin publish, Decisions 10/10b), PRD #218 (`fetchAgentBranch` credential-free fetch-back), `agent/src/runner.ts`, `agent/src/sdk-executor.ts`, `agent/src/git.ts`

> Every file:line citation below is at `cd746a0e` (2026-08-09). Re-derive before acting: a line number without a SHA is not a citation.

## Problem

uzi publishes a run's committed work to origin **only at milestone boundaries**. The
per-iteration checkpoint exists but does not reach origin, so a run inside a long
milestone accrues many iterations of committed work that live only on one worker's disk.
If that disk is lost (a k8s pod eviction or crash) before the milestone completes, a
resuming worker seeds from origin/default and the whole in-milestone effort is lost.

The mechanism, verified at HEAD:

- **Milestone-cooperative checkpoint** (`sdk-executor.ts:1154-1156`): when the lead
  signals a completed milestone (`turn.checkpoint && !turn.done`), the runner fires
  `checkpoint({reap: true})` — reap the agent tree, fetch the committed work back, **and
  broker a pack to origin** (`refs/uzi-checkpoints/<branch>`, `runner.ts:803`
  `publishCheckpointBestEffort`).
- **Iteration-boundary fallback** (`sdk-executor.ts:1161-1165`, PRD #122 Decision 10b):
  reached every iteration where the lead did **not** cooperatively checkpoint. It fires
  `checkpoint({reap: false})`, which fetches the committed work back to the worker's
  **local** bare-repo tracking ref only — and `reap:false` **deliberately does not
  publish to origin** (`runner.ts:797-810`, *"the reap:false fallback must not
  publish"*).

So the interval between origin-visible checkpoints equals **however long a milestone
takes**, which is unbounded in wall-clock terms — and worst on a long *first* milestone,
where nothing is on origin yet at all. **Live evidence**: run #191 (`13563aaf`) at
iteration **7/40** with **0** milestones completed has **no** `refs/uzi-checkpoints/agent/issue-191`
on origin (`git ls-remote origin 'refs/uzi-checkpoints/*'` shows only issue-240/241 from
older runs). All of its work is on a single hosted worker's disk.

The worker cannot mitigate this itself: `git push` is denied by the guardrail
(`agent/src/guardrails.ts`, `REASON_PUSH`) — the worker never pushes, all origin writes
go through the api's checkpoint broker.

## Solution Overview

**Let the existing `reap:false` iteration-boundary checkpoint publish to origin too, on a
time budget.** At an iteration boundary, if a configurable interval (default 20m) has
elapsed since the last origin publish **and** the branch tip has moved (new committed
work), broker the checkpoint pack to origin — **without reaping** the agent tree.

This is a small, contained change because the hard parts already exist:

- The iteration-boundary checkpoint **already runs every iteration** and already does a
  **credential-free fetch-back without reaping** (`sdk-executor.ts:1165`) — the precedent
  that "credential-free git at an iteration boundary, agent tree alive" is safe is already
  set.
- The **tip-moved skip guard** already exists (`runner.ts:776`, *"checkpoint skipped:
  branch tip unmoved since last checkpoint"*), so an idle branch costs nothing.
- The **origin-publish broker** already exists (`publishCheckpointBestEffort` →
  `client.publishCheckpoint`, `client.ts:222`), and **cross-worker recovery** from
  `refs/uzi-checkpoints/<branch>` already works (`git.ts:222-232`, `seededFrom:
  "checkpoint"`) — both reused unchanged.

The only new behaviour is a **time-gated origin publish on the non-reap path**. Worst-case
loss becomes **~one interval** of committed work regardless of milestone length.

## Design Decisions

1. **Reuse the `reap:false` path; do not add a new trigger.** It already fires every
   iteration and already fetches back without reaping. We add a time-gated publish to it,
   not a parallel mechanism.
2. **Time-gate the publish. The reason `reap:false` didn't publish is CONFIRMED to be
   scope + broker-cost, not a correctness invariant — so this is safe.** Architect review
   reconstructed it from `prds/done/122`: Decision 10b's no-reap is *behavioral* ("the reap
   there buys consistency, not security"; the fetch-back is credential-free), and Decision
   14 records that the M8 *broker* carries no PAT on the worker, so it "no longer needs to
   reap at the fallback checkpoint" — the reap/publish coupling was a property of the
   *rejected worker-side push*, not the broker. M8 then simply chose *milestone
   granularity* (scope), reinforced by broker cost (the OOM saga: ~787 MiB/checkpoint until
   v0.20.2's shallow-fetch fix, ~47 MiB now). A time-gate (≤1 publish/interval/run) removes
   that cost. So M1 does not *reconstruct* the rationale (done, here); it **rewrites the
   now-stale `runner.ts:799` comment** ("the reap:false fallback must not publish") and
   records the superseded reasoning.
3. **Publish WITHOUT reaping, preserving the credential-safety invariant.** The
   `reap:false` path must not kill the agent tree (a backgrounded dev server the lead
   reuses must survive — Decision 10b, `sdk-executor.ts:1163`). Origin publish is
   **credential-free** (a pack streamed to the api via `client.publishCheckpoint`, not a
   PAT-bearing `git push`), and the fetch-back is credential-free (`file://`, PRD #218).
   So the load-bearing B1/M4 invariant — *no credentialed git while the agent tree is
   alive* — is preserved. **The auditor confirms this on the new path; it is not asserted.**
4. **Only committed work is captured.** The fetch-back reads commits, not the dirty tree,
   so the protection is only as good as the lead's local commit cadence. Pair with a
   one-line lead-template nudge to commit at least every ~interval.
5. **Configurable interval as a duration string, default `20m`, `0` disables.** ↳review
   (MAJOR): name it **`CHECKPOINT_INTERVAL`** and read it via the existing
   `duration(env, "CHECKPOINT_INTERVAL", "20m")` helper (`agent/src/config.ts`), which
   matches the repo idiom (`WORKER_HEARTBEAT_INTERVAL="15s"`, etc.) and where
   `parseDuration("0") == 0` disables cleanly. A `_SECONDS` name is a trap: `duration()`
   reads a bare number as **milliseconds** (so `…=1200` would be 1.2s → a publish almost
   every iteration), and `positiveInt()` *falls back* on `0` rather than disabling. Home is
   a worker-side env; server-served central config (`RUN_MAX_ITERATIONS`'s shape) is a
   deferred alternative in Out of Scope.
6. **Default ON.** To actually protect runs. The tip-moved skip + the time-gate bound the
   cost, and PRD #122 M8 already hardened the broker. Recorded alternative: default `0`
   (off) until broker cost is measured in production.
7. **Cross-worker recovery is unchanged.** A time checkpoint writes the same
   `refs/uzi-checkpoints/<branch>` mirror a milestone checkpoint does, so `git.ts`'s
   `seededFrom: "checkpoint"` recovery needs no change.
8. **Best-effort, never fails the run** — the same contract every existing checkpoint
   holds (`runner.ts:764`).
9. **The publish gate keys on a separate `lastPublishedTip`, NOT the existing fetch-back
   skip.** ↳review (MAJOR): `runner.ts:776` returns from the *whole* callback when the tip
   is unmoved *since the last fetch-back*. Because the time-gate defers a publish past a
   fetch-back, a commit made just before a quiet stretch (≥ interval of review/test
   iterations with no new commit) would hit that early-return every iteration, the publish
   gate would never be evaluated, and the commit would never reach origin — reshipping the
   exact window this PRD closes. So track `lastPublishedTip` (an OID) separately: skip only
   the *fetch* when the tip is unmoved since the last fetch, but still evaluate the publish
   gate against `cloneTip !== lastPublishedTip`. This is why M1's "reap:true path
   unchanged" is *behaviourally equivalent*, not byte-identical — the shared skip logic is
   restructured.

## Touchpoints

| Area | Files | Nature |
| --- | --- | --- |
| Publish-without-reap | `agent/src/runner.ts` (the `checkpoint` callback, `:765-810`: currently gates `publishCheckpointBestEffort` on `opts.reap`) | Allow origin publish on the non-reap path when the time-gate is open |
| Time-gate + trigger | `agent/src/runner.ts` (track `lastPublish` time + `lastPublishedTip` OID as sibling `let`s to `barePath` at `:382`; publish gate keys on `cloneTip !== lastPublishedTip`, NOT the `:776` fetch-skip — Decision 9), `agent/src/sdk-executor.ts:1165` (the `reap:false` call site) | Publish at most once per interval; reset the gate on any origin publish |
| Checkpoint opts | `agent/src/executor.ts:213` (`checkpoint?(opts:{reap, progress})`) | Extend only if the time-gate cannot live entirely inside the runner callback |
| Config | `agent/src/config.ts` `loadConfig()` (add a `Config` field via `duration(...)`), threaded to the Runner as a scalar `RunnerOptions` field at `main.ts:134-137` (alongside `pollMs`/`planApprovalTimeoutMs`) | `CHECKPOINT_INTERVAL` duration (default `20m`; `0` disables) |
| Visibility | `agent/src/runner.ts` (checkpoint callback's report/log) | A log line / subtle running-report signal when a time checkpoint publishes |
| Lead nudge | builtin `lead` template (`api/internal/agenttmpl/builtins/lead.md`) | One line: commit local work at least every ~interval so checkpoints capture it |
| Docs | `docs/` | Document the checkpoint model (milestone + time) and the new env |

## Milestones

**Phase graph:** M1 (safe decoupling) gates everything. M2 (time-gate + config) needs M1.
M3 (visibility + nudge) and M4 (docs) follow M2 and are mutually independent.

- [x] **M1 — Safe origin-publish on the non-reap path.** The rationale is already settled
      (Decision 2): PRD #122 Decision 14 dissolved the reap/publish coupling for the broker,
      so this is a scope/cost choice, not a correctness invariant. Refactor the `checkpoint`
      callback so a `reap:false` checkpoint **can** publish to `refs/uzi-checkpoints/<branch>`
      **without** `killAgentTree`, and **rewrite the stale `runner.ts:799` comment** ("the
      reap:false fallback must not publish") to record the superseded reasoning. The
      credential-safety invariant holds by construction — `publishCheckpointBestEffort` →
      `git.checkpointPack` (`git pack-objects`, local objects, **no PAT**) →
      `client.publishCheckpoint` (worker join token, already in memory) — so no PAT-bearing
      git child ever runs under the agent uid. **Verified**: a `reap:false` checkpoint
      publishes to origin and performs **zero** `killAgentTree` calls; the **auditor**
      confirms no credentialed git runs while the agent tree is alive on the new path; the
      milestone (`reap:true`) path is **behaviourally equivalent** (not necessarily
      byte-identical — Decision 9 restructures the shared skip logic and may legitimately
      skip a redundant re-publish of an already-published milestone tip).

- [x] **M2 — Time-gated trigger + config.** Track `lastPublish` time and `lastPublishedTip`
      (Decision 9); on the `reap:false` path publish only when
      `now - lastPublish >= CHECKPOINT_INTERVAL` **and** `cloneTip !== lastPublishedTip`
      (new committed work not yet on origin) — do **not** gate this on the `:776` fetch-skip.
      Any origin publish (milestone or time) updates both. Add `CHECKPOINT_INTERVAL` via
      `duration(...)` (default `20m`; `0` disables). **Verified**: with a short test
      interval, the first iteration boundary past the interval publishes and earlier ones do
      not; **a commit whose tip then goes idle for ≥ the interval (only review/test
      iterations follow, no new commits) still publishes once** — the exact regression a
      naive `:776` reuse would reship; an iteration with nothing new-since-last-publish
      publishes nothing; `0` disables the time path entirely (milestone-only behaviour); a
      milestone publish resets the gate so the next time publish is a full interval later;
      **at most one origin publish per interval per run** (no per-iteration spam).

- [x] **M3 — Visibility + commit cadence.** Emit a log line / subtle running-report signal
      when a time checkpoint publishes, so the "work is now safe on origin" moment is
      observable (mirroring the milestone checkpoint's running report). Add a one-line note
      to the builtin `lead` template to **commit local work frequently so checkpoints
      capture it** (worded generically — the lead does not know the interval value, which is
      worker env and not in its prompt). **Verified**: a time checkpoint emits the signal;
      the builtin-template parse test passes and the note is present.

- [x] **M4 — Docs.** Document the two-tier checkpoint model (milestone-cooperative +
      time-gated iteration-boundary) and `CHECKPOINT_INTERVAL`. **Verified**:
      `node web/scripts/check-docs.mjs` passes.

## Success Criteria

- A run inside a long milestone has its committed work on origin within **~one interval
  plus at most one turn** (the publish fires at the next iteration boundary, and a turn is
  atomic), not only at milestone completion.
- Worst-case data loss from a worker-disk loss is bounded to **~`CHECKPOINT_INTERVAL`**
  of committed work (plus any uncommitted work, which is out of scope).
- Cross-worker recovery from a time checkpoint works **identically** to a milestone
  checkpoint (same `refs/uzi-checkpoints/<branch>` mirror + `git.ts` seed path).
- The milestone (`reap:true`) path is unchanged, and the worker still never `git push`es
  (publish stays broker-mediated).
- `CHECKPOINT_INTERVAL=0` fully restores milestone-only behaviour.
- At most **one origin publish per interval per run** — no per-iteration broker spam.

## Out of Scope (deliberate)

- Checkpointing **uncommitted** working-tree changes (only committed commits are captured;
  a dirty-tree snapshot is a heavier, separate mechanism).
- **Mid-turn** checkpoints (the SDK turn is atomic; the iteration boundary is the safe
  point — reflected in Decision 3).
- **Server-served / central** interval config (worker env is the initial home; a
  server-served knob like `RUN_MAX_ITERATIONS` is a follow-up).
- Changing the recovery/seed logic (`git.ts` `seededFrom: "checkpoint"` is reused as-is).
- Reducing milestone sizes (a PRD-authoring discipline, orthogonal to this mechanism).

## Risks

- **Decoupling publish from reap — RESOLVED, not open.** Architect review confirmed via
  PRD #122 Decisions 10b/14 that the coupling was scope + broker-cost (dissolved for the
  broker, which carries no PAT), not a correctness invariant. The residual is the *trigger
  correctness* the time-gate introduces (Decision 9's `lastPublishedTip`), covered by M2's
  idle-commit test.
- **Broker load.** Mitigated by the time-gate (≤1 publish/interval/run), the
  publish-if-new-work gate, and PRD #122 M8's existing OOM hardening (~47 MiB/checkpoint
  post shallow-fetch). ↳review: the broker's `/publish` endpoint rate-limiter is still an
  open TODO (defense-in-depth) — not a blocker here, but worth pairing.
- **Only committed work is captured.** Mitigated by M3's commit-cadence nudge and
  documented plainly.
- **Default-on adds publishes to every run.** Mitigated by the modest interval; the
  recorded fallback is to default `0` until broker cost is measured.

## Validation

- Agent: `cd agent && npm run typecheck && npm test` (and `npm run lint`). The time-gate,
  the no-reap publish, and the interval/skip behaviour are unit-testable through the
  `checkpoint` callback against a fake broker/git seam that `runner`/`executor` tests
  already use.
- No API/web changes expected (worker-only) unless config is server-served (out of scope).
  Controller untouched.
- Docs: `node web/scripts/check-docs.mjs`.

## Decision Log

- **2026-08-09 — Scope.** Reuse the existing `reap:false` iteration-boundary checkpoint;
  add a **time-gated origin publish** to it rather than a new trigger. The hard pieces
  (no-reap fetch-back, tip-moved skip, broker, cross-worker recovery) already exist.
- **2026-08-09 — The crux, CONFIRMED (was "to verify").** Architect review reconstructed it
  from `prds/done/122` Decisions 10b + 14: the `reap:false`-doesn't-publish was scope +
  broker-cost, and the reap/publish coupling was a property of the *rejected worker-side
  push*, dissolved once the broker (no PAT on the worker) landed. So a time-gated
  publish-without-reap is safe; M1 rewrites the stale `runner.ts:799` comment.
- **2026-08-09 (↳review) — two must-fix corrections applied.** (a) The publish gate keys on
  a separate `lastPublishedTip`, not the `:776` fetch-skip, or a commit that goes idle
  before the interval would never publish (Decision 9). (b) The interval is
  `CHECKPOINT_INTERVAL` read via `duration(...)` default `20m`, not `…_SECONDS` (a bare
  `_SECONDS` number reads as milliseconds and `positiveInt` won't let `0` disable).
- **2026-08-09 — Config (SUPERSEDED by the ↳review correction above).** The original draft
  proposed worker env `CHECKPOINT_INTERVAL_SECONDS`, default `1200` (20m). As shipped this is
  `CHECKPOINT_INTERVAL` (a Go-style duration string via `duration(...)`, default `20m`), `0`
  disables; server-served central config deferred.
- **2026-08-09 — Evidence.** Checkpoint model verified at `cd746a0e`
  (`sdk-executor.ts:1154-1165`, `runner.ts:765-810`, `executor.ts:203-213`,
  `client.ts:222`, `git.ts:222-232`); live gap measured on run #191 (no
  `refs/uzi-checkpoints/agent/issue-191` on origin at iteration 7/40).
