# PRD #284: Retry transient forge push/MR failures instead of discarding completed runs

**GitLab Issue**: [#284](https://gitlab.example.com/vtmocanu/uzi/-/issues/284)
**Status**: Draft (created 2026-08-09; revised after architect review — Layer B redesigned around the wall-sweep constraint)
**Priority**: Medium
**Related**:
- [#47](https://gitlab.example.com/vtmocanu/uzi/-/issues/47) — run-health flag (`⚠` + Slack nudge). Layer B borrows its *notification* intent but not its mechanism: health is server-derived from telemetry (`api/internal/workersvc/health.go`, single writer `SetRunHealth` at `health.go:171`), so the worker cannot push a "forge unreachable" reason. See D6.
- [#46](https://gitlab.example.com/vtmocanu/uzi/-/issues/46) — the in-app notification inbox (`api/internal/notifysvc`, `00060_notifications.sql`). This PRD adds the 5th `notifysvc.Notify` caller and closes the gap that a failed run emits no inbox notification.
- [#35](https://gitlab.example.com/vtmocanu/uzi/-/issues/35) — the `limit_wait` usage-limit park. The **park precedent** Layer B now adopts: a non-terminal status the wall sweep ignores, on-disk state preserved, promoted back to `queued` by the sweeper. See D5.
- [#88](https://gitlab.example.com/vtmocanu/uzi/-/issues/88) — clarification bounds (`question_max`, `question_timeout_seconds`). The **config-plumbing precedent**: a worker-enforced limit configured server-side and shipped in `ClaimConfig` "because a worker env var is unreachable on hosted k8s" (`agent/src/protocol.ts:370-377`).

## Problem

A transient forge error on the worker's final `git push` fails the whole run **terminally**, throwing away the agent's already-completed work.

Observed live on issue #216 (run `8fc2fa47`, 2026-08-09):

```
git push origin refs/uzi-runner/agent/issue-216:refs/heads/agent/issue-216 failed:
fatal: unable to access 'https://gitlab.example.com/vtmocanu/uzi.git/':
HTTP/2 stream 1 reset by server (error 0x2 INTERNAL_ERROR)
```

The agent had finished; the committed diff was already fetched into the worker's bare repo (`agent/src/runner.ts:937`) **before** the push at `runner.ts:1044`. One dropped TCP stream discarded all of it. The only recovery was a fresh run (`22ff3af6`) that re-implemented the entire change from scratch, re-spending the owner's Anthropic budget on work that was already done.

Two concrete gaps in the code:

1. **Zero retry on the worker→forge hop.** `RepoCache.pushBranch` (`agent/src/git.ts:321`) → `runGit` (`git.ts:833`) throws on the first error; there is no retry loop, no backoff, no transient-vs-permanent classification. `createMergeRequest` (`agent/src/forge.ts:100`) is *already idempotent* — on a duplicate-MR response it calls `findOpenMr` and returns the existing MR rather than throwing (`forge.ts:100-120`; all three drivers, D3) — but it too has no retry/backoff/classification, so a transient 502 or connection reset while opening the MR fails the run. A stream reset, a 5xx, or a connection reset is treated identically to an auth failure or a protected-branch rejection: one attempt, then the run is `failed` (`runner.ts:1156-1181`). The requeue machinery (`requeue_count`, `RUN_MAX_REQUEUES`) does not help — it fires only on worker death / stale heartbeat, and a failed push comes from a live, heartbeating worker.
2. **A failed run reaches no in-app notification.** The `❌ Failed` Slack DM (`api/internal/slacksvc/notifier.go:1042`) fires only for users who linked and opted into Slack. `notifysvc.Notify` has four callers (judge `handler/judge_worker.go:223`, self-improve `selfimprove/engine.go:261` and `:289`, schedule-paused `schedsvc/scheduler.go:491`) — run lifecycle failures are not among them — so a user without Slack gets no notification at all; the failure surfaces only if they happen to be looking at the board.

The irony: the repo already contains the exact retry pattern we need. `agent/src/client.ts:443` `isTransient()` (`status >= 500 || 408 || 429`, plus network/timeout) drives a bounded backoff schedule `[1s,2s,4s,8s,16s]` (`DEFAULT_TERMINAL_RETRY_SCHEDULE`, `client.ts:50`) — but it protects only the worker→**API** status callback, never the worker→**forge** push.

## Solution

Two layers. **Layer A (fast retry) is sound, independent, and fully fixes the observed incident — ship it first.** Layer B (extended outage handling) required a redesign after review: a run cannot safely sit in `running` for a long outage (the wall sweep kills it — see D5), so Layer B adopts the `limit_wait` park model.

### Layer A — bounded fast retry on push + MR-create (the fix for #216)

Wrap `pushBranch` and the whole `createMergeRequest` call in a backoff retry loop that classifies the error first:

- **Transient ⇒ retry** (bounded, fast — the `[1s,2s,4s,8s,16s]` shape, ~30s total): git stderr matching `stream .* reset` / `INTERNAL_ERROR` / `Connection reset` / `Could not resolve host` / `Could not read from remote` / `500`/`502`/`503` / timeout; forge HTTP `>= 500 || 408 || 429`, reusing the `isTransient` shape.
- **Permanent ⇒ fail fast, never retry**: auth failure, `protected branch`, non-fast-forward / `[rejected]`, 401/403/404/422. **This is the safety-critical half** — retrying a guardrail or protected-branch rejection would be wrong and must not happen.

The push is idempotent on retry (non-forced, same commits → "Everything up-to-date"). MR-create is **already** idempotent (D3), so M2 only wraps the existing call — but the loop must wrap the *entire* `createMergeRequest` (POST → duplicate → `findOpenMr` GET), not just classify its final thrown status: a duplicate POST followed by a *transient* `findOpenMr` failure surfaces as a 409/422 (not in `isTransient`), so classifying only the thrown status would fail a run whose MR actually exists (D3).

A single stream reset clears on attempt 2 within seconds, so **Layer A alone would have saved run `8fc2fa47`**. Its schedule can be a worker-side constant (like `DEFAULT_TERMINAL_RETRY_SCHEDULE`), so Layer A needs **no** new `ClaimConfig` field and does not gate on M3.

### Layer B — extended retry through a real outage, via the `limit_wait` park model

When Layer A's fast retries are exhausted and the forge is genuinely unreachable, the run should wait out the outage rather than fail. **It cannot do this in `running` status**: `SweepRunningTimeout` (`api/internal/store/queries/runtime.sql:1300`, driven from `service.go:4232`) fails any `running` non-chat run whose `started_at` is older than `COALESCE(budget_wall_seconds, RUN_TIMEOUT)` (default **2h**, `config.go:671`) — and the clock runs from `started_at`, not from when the push begins. So a `running` park would be killed mid-retry with reason `"run exceeded RUN_TIMEOUT"`, its eventual `completed` silently dropped by the `SetRunCompleted` WHERE-guard (`runtime.sql:1125`), a wasteful judge pass queued (`service.go:4245`), and an MR left open on a `failed` run (D5, D7).

The codebase already solves exactly this for the usage-limit case: `limit_wait` (#35) is a non-terminal status the wall sweep ignores (it requires `status='running'`), preserves on-disk state, releases the worker slot, and is promoted back to `queued` by the sweeper (`PromoteLimitWaitRuns`, `service.go:4334`). Layer B models a **forge-unreachable park** on it. Releasing the slot is the right behavior here: during a forge-wide outage no other run can make progress either, so nothing is lost, and the run stops burning a wall timer (D8).

**Flagging (the "notifications" half).** On *entering* the park, emit a single `notifysvc.Notify(owner, kind="forge_retry", …)`. `notifysvc.Notify` (`api/internal/notifysvc/service.go:111`) **persists an inbox row first, then best-effort Slack DM** — so one call lands both the in-app badge (for everyone, including non-Slack users) and the Slack DM (for the linked). Deduped so a multi-hour outage produces one "waiting on forge" notification, not one per attempt.

**Outcomes.** Recovery ⇒ the run is promoted back to `queued`, re-claimed (resume affinity), and completes normally. Give-up after a configurable max park ⇒ `failed` with a clear reason ("forge unreachable after &lt;window&gt;"), which now also emits an in-app notification (M5).

**Per-attempt timeout caveat.** `GIT_TIMEOUT_MS` = 10m (`git.ts:116`): a *fast* failure returns in seconds, but a *hung* connection blocks up to 10 minutes per attempt. The park cadence and max-window math must account for this, and extended retry likely wants a shorter per-attempt git timeout (Open Decision 2).

### Config plumbing (settles the investigation)

Worker config is delivered per-claim via `ClaimConfig` (`agent/src/protocol.ts:363`), server-derived, read worker-side from `ctx.config` (`agent/src/sdk-executor.ts:429`) with worker defaults as fallback — **not** worker env vars, which are unreachable on hosted k8s (#88 precedent). So:

- Add tunables to `api/internal/config/config.go` beside the run knobs (`RunTimeout` :671, `RunLimitMaxWaits` default 5 at :687, `RunLimitMaxPark` at :688): the extended-retry cadence and a **max park window** with a default (proposed `PUSH_RETRY_MAX_WINDOW`; see Open Decision 2).
- Thread them into `ClaimConfig` (new optional fields, `<= 0`/absent ⇒ worker default, per the #88 convention).
- Override in k8s via `api.config` in `deploy/chart/values.yaml` — the ConfigMap `range` (`api-configmap.yaml:10-12`) picks up any key, wired via `envFrom` (`api-deployment.yaml:59-61`) with a `checksum/config` rollout annotation. No template edit.

## Milestones

- [ ] **M1 — Error classifier.** A worker-side transient-vs-permanent classifier for git stderr and forge HTTP status, unit-tested against real error strings. Named as a deliberate second implementation of the `isTransient` decision (`client.ts:443`) and pinned to it with a shared table. Pin `LANG=C`/`LC_ALL=C` for the push subprocess (or match stable tokens) so stderr matching does not depend on locale.
- [ ] **M2 — Layer A retry.** Wrap `pushBranch` (`git.ts:321`) and the whole `createMergeRequest` call (`forge.ts:100`) in a bounded backoff loop using M1. Push idempotent on retry; MR-create is already idempotent — treat its adopt-existing return as success, and wrap the entire call so a transient `findOpenMr` failure re-runs rather than fails. Permanent errors bypass the loop and fail as today. **Independent of M3; shippable as the #216 fix.**
- [ ] **M3 — Config plumbing.** New tunables in `config.go` (defaults) → `ClaimConfig` (optional fields) → worker (`ctx.config`, fallback to worker defaults). Older-server absence ⇒ worker defaults. `deploy/chart/values.yaml` comment documents the keys.
- [ ] **M4 — Layer B forge-wait park.** Add a `limit_wait`-style forge-unreachable park (status + wall-sweep exemption + sweeper promotion, modeled on #35). Worker enters the park after M2's fast retries exhaust; server emits `notifysvc.Notify(kind="forge_retry")` once (deduped); promotion re-claims with resume affinity; give-up after the max window ⇒ `failed` with a forge-outage reason. **Gates on M3** (max-window tunable) and the park-status decision (Open Decision 1).
- [ ] **M5 — Close the failure-notification gap.** A `failed` run emits an in-app notification via `notifysvc.Notify` (5th caller), so non-Slack users get an inbox badge on failure. Recommend general run-failure, not just the forge give-up (Open Decision 3). **Independent of Layer B.**
- [ ] **M6 — Tests.** Agent `node --test`: classifier table with **discriminating** cases — (i) #216 stream-reset ⇒ retry; (ii) auth failure ⇒ no retry; (iii) `protected branch` ⇒ no retry; (iv) non-fast-forward/`[rejected]` ⇒ no retry; (v) stderr containing BOTH transient and permanent substrings ⇒ **permanent wins** (precedence) — plus an assertion each case is exercised; fast-retry succeeds on Nth attempt; permanent fails fast; MR-create adopt-existing + transient-`findOpenMr` re-run; park give-up boundary. API LiveDB (`./e2e/run-store-it.sh`): `forge_retry` notify writes an inbox row + attempts Slack; failed-run notify; park exempt from `SweepRunningTimeout`; promotion path; `ClaimConfig` carries the tunables. Follow `.claude/rules/agent.md` (`--test-timeout=120000`) and `.claude/rules/go.md`.
- [ ] **M7 — Docs.** `ARCHITECTURE.md` run-lifecycle (push retry, the forge-wait park, the new notification), a `docs/` note on the config knobs, and the `deploy/chart/values.yaml`/CLAUDE.md config references. Update the status list if a new park status lands (CLI skill, `run wait --until`, WS hub).

## Success criteria

1. A transient push error (the #216 stream reset) is retried and the run **completes** instead of failing — no re-implementation, no re-spent budget. (Layer A alone satisfies this.)
2. A **permanent** rejection (auth, protected branch, non-fast-forward) still fails **immediately** — never retried. The `main`-never-touched directive is unaffected (retrying an idempotent push to the agent branch touches nothing protected).
3. During a sustained forge outage the run **parks** (not `running`) up to the configured max window and is **not** killed by `SweepRunningTimeout`; on give-up it fails with a reason naming the outage, not `"run exceeded RUN_TIMEOUT"` and not a bare stream-reset string.
4. Entering the park produces **exactly one** notification per run (inbox row + best-effort Slack), not one per attempt.
5. A failed run lands an in-app notification for users **without** Slack.
6. The max window (and cadence) is configurable via `api.config` Helm values with a sane default; compose reads the same env with the same default.
7. MR-create retried after a lost response does **not** create a duplicate MR. *(Already holds today via `forge.ts:100-120`; M2 must not regress it, and must additionally survive a transient `findOpenMr` during a duplicate.)*

## Decision Log

- **D1 — Retry in the worker, not via requeue.** The completed diff is already in the worker's bare repo; retrying the push reuses it. Requeue (`RUN_MAX_REQUEUES`) is worker-death recovery and would re-provision/re-run — the expensive path this PRD exists to avoid. Requeue stays untouched.
- **D2 — Classify, then retry.** The one non-negotiable is that permanent forge rejections fail fast. A blanket retry-on-any-error would retry protected-branch and auth rejections, weakening a guardrail. The classifier (M1) is the load-bearing piece; the backoff loop is the easy part.
- **D3 — MR-create is ALREADY idempotent — do not rebuild it.** All three drivers adopt an existing MR on the duplicate status (base `createMergeRequest` `forge.ts:100-120`; `findOpenMr` gitlab `:193`/409, forgejo `:227`/409, github `:276`/409+422), and `runner.ts:1058-1061` documents this as battle-tested for ci_fix/resume. SC7 holds today. M2 wraps this call in the retry loop and treats adopt-existing as success. The one edge to *add*: wrap the whole call so a transient `findOpenMr` failure after a duplicate POST re-runs instead of throwing a (permanent-classified) 409/422.
- **D5 — Layer B parks; it does NOT stay in `running` (reverses the first draft).** The first draft kept the run in `running` and rejected a new status. Review found the decisive constraint: `SweepRunningTimeout` (`runtime.sql:1300`, `RUN_TIMEOUT` 2h from `started_at`) would fail the parked run before the window elapsed — the extended window is not an independent knob, it competes with the agent's own working time for the same wall budget. So Layer B adopts the `limit_wait` park model (#35): a non-terminal status the wall sweep ignores. The status-contract cost D5 originally cited is real but was already paid once for `limit_wait`; the cost of *not* paying it is a killed run. (Open Decision 1 lets the user choose the park mechanism.)
- **D6 — Health (#47) is not the hook.** Health is derived server-side from telemetry and written through the single `SetRunHealth` writer (`workersvc/health.go:171`); there is no worker-driven health path and the enum is CHECK-constrained (`00057_run_health.sql`). `notifysvc.Notify` avoids both a worker health endpoint and an enum migration.
- **D7 — The `running`-park failure modes are the reason D5 flipped.** For the record, a `running` park hits four distinct failures, all confirmed: (a) killed by the wall sweep before the window; (b) wrong failure reason `"run exceeded RUN_TIMEOUT"` (contradicts SC3); (c) a wasteful judge pass enqueued on timeout (`service.go:4245`); (d) the MR-on-failed-run race — the worker recovers, opens the MR, then `SetRunCompleted` updates 0 rows against the now-`failed` row (`runtime.sql:1125`), leaving an open MR on a failed run. A `limit_wait`-style park avoids all four.
- **D8 — Release the slot during the park (revises "holding costs nothing").** The first draft said holding the worker slot costs nothing during an outage. True, but the `limit_wait` precedent *releases* the slot and preserves on-disk state — strictly better, because it also stops the wall timer and lets the fleet re-plan. Layer B releases and re-claims with resume affinity on promotion.

## Risks & mitigations

- **Retrying a permanent rejection weakens a guardrail.** Mitigated by M1's explicit permanent-error list (fail-fast) and SC2; the classifier is tested against real rejection strings including non-fast-forward and mixed-substring precedence (M6 cases iii–v).
- **The park is a status-contract change.** A new non-terminal status (or a `runs`-column carve-out) touches the wall sweep, the sweeper promotion, and — for a new status — the 9-status list (WS hub unknown-rewrite, CLI skill, `run wait --until`, web board). Scoped in M4/M7; Open Decision 1 lets the user pick the lighter mechanism. (This corrects the first draft's false "no schema change to `runs`" claim.)
- **`GIT_TIMEOUT_MS`=10m per attempt** can make a hung-connection outage retry only once per 10m and eat wall budget. Mitigated by a shorter per-attempt git timeout during extended retry (Open Decision 2).
- **Notification spam during a long outage.** Mitigated by dedup (M4): one `forge_retry` notify per run per outage.
- **Classifier drift from `isTransient`.** Two implementations of one decision. Mitigated by the shared differential table (M1/M6) and locale-pinning.

## Out of scope

- Retrying non-push forge operations (clone, fetch, label writes, poller sync) — this PRD is the worker's final push/MR hop, the observed failure. A general forge-client retry layer is a candidate follow-up, noted not built.
- Changing the requeue / worker-death recovery path.
- Rebuilding MR-create idempotency (already exists — D3).

## Open decisions (for the user)

1. **Layer B park mechanism, and whether it ships in this PRD.** Review reversed the first draft's "no new status" (D5). Three options: **(A)** adopt a `limit_wait`-style park status (`forge_wait`) — the recommended, precedented path; **(B)** keep `running` but exempt forge-waiting runs from the wall sweep via a nullable `runs` marker (smaller surface, still a schema change); **(C)** restrict this PRD to **Layer A + M5** (the incident fix + the notification gap, both independent and low-risk) and spin Layer B into its own PRD. **Recommend: ship Layer A + M5 now, and do Layer B as (A).** Confirm.
2. **Max park window, cadence, and per-attempt git timeout.** Proposed: Layer A fast retries `[1s,2s,4s,8s,16s]` (~30s); then park with extended retry up to `PUSH_RETRY_MAX_WINDOW` (default TBD — 15m? 1h? a generous window costs nothing once the slot is released), with a **shorter per-attempt git timeout** than the 10m `GIT_TIMEOUT_MS` so the cadence is real. Confirm the numbers.
3. **Failure-inbox-notify scope (M5).** Emit the in-app notification on **all** run failures (general — closes #46's gap broadly) or only on the forge give-up? **Recommend general.** Confirm.
4. **The worker→server "entering park" signal.** Options: (a) an optional field on the existing `reportState`/state callback (smallest surface, no new route); (b) a narrow new worker endpoint. **Recommend (a).** Confirm.

## Milestone parallelization

**Layer A (M1→M2) is independent of everything else and independently shippable** as the #216 fix — its retry schedule is a worker-side constant, no `ClaimConfig` needed. **M5** (failure-notify) is independent server-side work. **M3** (config plumbing) gates only **M4** (the park), which also gates on Open Decision 1. So: M1→M2 and M5 can land immediately and in parallel; M3→M4 follow once the park mechanism is chosen. M6 tests gate on their track; M7 docs independent. Single repo, one or two MRs depending on whether Layer B is split (Open Decision 1C).

---

*Drafted 2026-08-09 against code at HEAD (`7112ab60`); revised the same day after an architect review that verified every load-bearing citation and found the `SweepRunningTimeout` constraint (D5/D7), the pre-existing MR idempotency (D3), and Layer A's independence from M3. Two research passes plus the review confirmed the worker-config plumbing (`ClaimConfig` vs worker env) and the health-vs-notify hook against `agent/src/protocol.ts`, `api/internal/workersvc/health.go`, and `api/internal/notifysvc/service.go`.*
