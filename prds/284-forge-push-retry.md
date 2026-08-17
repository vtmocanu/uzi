# PRD #284: Retry transient forge push/MR failures instead of discarding completed runs

**GitLab Issue**: [#284](https://github.com/vtmocanu/uzi/-/issues/284)
**Status**: Draft (created 2026-08-09; revised the same day — an architect review verified every load-bearing citation and surfaced the `SweepRunningTimeout` constraint, after which the owner scoped this PRD to **Layer A + the failure-notification gap** and deferred **Layer B** (surviving a sustained outage) to a follow-up PRD)
**Priority**: Medium
**Related**:
- [#46](https://github.com/vtmocanu/uzi/-/issues/46) — the in-app notification inbox (`api/internal/notifysvc`, `00060_notifications.sql`). This PRD adds the 5th `notifysvc.Notify` caller and closes the gap that a failed run emits no inbox notification.
- [#47](https://github.com/vtmocanu/uzi/-/issues/47) — run-health flag (`⚠` + Slack nudge). The notification uses `notifysvc`, not health: health is server-derived from telemetry (`api/internal/workersvc/health.go`, single writer `SetRunHealth` at `health.go:171`), so the worker cannot push a reason. See D6.
- [#35](https://github.com/vtmocanu/uzi/-/issues/35) — the `limit_wait` usage-limit park. Precedent for the **deferred** Layer B: a non-terminal status the wall sweep ignores, on-disk state preserved, promoted back to `queued` by the sweeper. The `SweepRunningTimeout` analysis (D5/D7) is what makes Layer B a deliberate follow-up rather than a quick add.
- [#88](https://github.com/vtmocanu/uzi/-/issues/88) — clarification bounds (`question_max`, `question_timeout_seconds`). Config-plumbing precedent for the **deferred** Layer B: a worker-enforced limit configured server-side and shipped in `ClaimConfig` "because a worker env var is unreachable on hosted k8s" (`agent/src/protocol.ts:370-377`).

## Problem

A transient forge error on the worker's final `git push` fails the whole run **terminally**, throwing away the agent's already-completed work.

Observed live on issue #216 (run `8fc2fa47`, 2026-08-09):

```
git push origin refs/uzi-runner/agent/issue-216:refs/heads/agent/issue-216 failed:
fatal: unable to access 'https://github.com/vtmocanu/uzi.git/':
HTTP/2 stream 1 reset by server (error 0x2 INTERNAL_ERROR)
```

The agent had finished; the committed diff was already fetched into the worker's bare repo (`agent/src/runner.ts:937`) **before** the push at `runner.ts:1044`. One dropped TCP stream discarded all of it. The only recovery was a fresh run (`22ff3af6`) that re-implemented the entire change from scratch, re-spending the owner's Anthropic budget on work that was already done.

Two concrete gaps in the code:

1. **Zero retry on the worker→forge hop.** `RepoCache.pushBranch` (`agent/src/git.ts:321`) → `runGit` (`git.ts:833`) throws on the first error; there is no retry loop, no backoff, no transient-vs-permanent classification. `createMergeRequest` (`agent/src/forge.ts:100`) is *already idempotent* — on a duplicate-MR response it calls `findOpenMr` and returns the existing MR rather than throwing (`forge.ts:100-120`; all three drivers, D3) — but it too has no retry/backoff/classification, so a transient 502 or connection reset while opening the MR fails the run. A stream reset, a 5xx, or a connection reset is treated identically to an auth failure or a protected-branch rejection: one attempt, then the run is `failed` (`runner.ts:1156-1181`). The requeue machinery (`requeue_count`, `RUN_MAX_REQUEUES`) does not help — it fires only on worker death / stale heartbeat, and a failed push comes from a live, heartbeating worker.
2. **A failed run reaches no in-app notification.** The `❌ Failed` Slack DM (`api/internal/slacksvc/notifier.go:1047`) fires only for users who linked and opted into Slack. `notifysvc.Notify` has four callers (judge `handler/judge_worker.go:223`, self-improve `selfimprove/engine.go:261` and `:289`, schedule-paused `schedsvc/scheduler.go:491`) — run lifecycle failures are not among them — so a user without Slack gets no notification at all; the failure surfaces only if they happen to be looking at the board.

The irony: the repo already contains the exact retry pattern we need. `agent/src/client.ts:443` `isTransient()` (`status >= 500 || 408 || 429`, plus network/timeout) drives a bounded backoff schedule `[1s,2s,4s,8s,16s]` (`DEFAULT_TERMINAL_RETRY_SCHEDULE`, `client.ts:50`) — but it protects only the worker→**API** status callback, never the worker→**forge** push.

## Solution

Two layers, of which **only Layer A ships in this PRD** (plus the independent failure-notification fix). Layer A — a bounded fast retry with a transient-vs-permanent classifier — fully fixes the observed #216 incident and depends on nothing else. **Layer B** (surviving a *sustained* forge outage) is **deferred to a follow-up PRD**: review found it cannot safely sit in `running` (the wall sweep would kill it — D5/D7), and its right guard is a real design choice with no urgency behind it, because Layer A already prevents the data loss. See *Deferred: Layer B* below for the analysis the follow-up should start from.

### Layer A — bounded fast retry on push + MR-create (the fix for #216)

Wrap `pushBranch` and the whole `createMergeRequest` call in a backoff retry loop that classifies the error first:

- **Transient ⇒ retry** (bounded, fast — the `[1s,2s,4s,8s,16s]` shape, ~30s total): git stderr matching `stream .* reset` / `INTERNAL_ERROR` / `Connection reset` / `Could not resolve host` / `500`/`502`/`503` / timeout; forge HTTP `>= 500 || 408 || 429`, reusing the `isTransient` shape. (Classifier precedence is load-bearing — see M1: permanent patterns win, and git's generic `Could not read from remote repository` trailer is **not** transient on its own, because it also trails an auth/permission denial.)
- **Permanent ⇒ fail fast, never retry**: auth failure, `protected branch`, non-fast-forward / `[rejected]`, 401/403/404/422. **This is the safety-critical half** — retrying a guardrail or protected-branch rejection would be wrong and must not happen (D2, D9).

The push is idempotent on retry (non-forced, same commits → "Everything up-to-date"). MR-create is **already** idempotent (D3), so M2 only wraps the existing call — but the loop must wrap the *entire* `createMergeRequest` (POST → duplicate → `findOpenMr` GET), not just classify its final thrown status: a duplicate POST followed by a *transient* `findOpenMr` failure surfaces as a 409/422 (not in `isTransient`), so classifying only the thrown status would fail a run whose MR actually exists (D3).

A single stream reset clears on attempt 2 within seconds, so **Layer A alone would have saved run `8fc2fa47`**. Its schedule can be a worker-side constant (like `DEFAULT_TERMINAL_RETRY_SCHEDULE`), so Layer A needs **no** new `ClaimConfig` field and does not gate on any config plumbing.

### Failure-notification gap (independent of Layer A)

A `failed` run should land an in-app notification via `notifysvc.Notify` (its 5th caller), so a user **without** Slack gets an inbox badge on failure instead of only the conditional Slack DM. `notifysvc.Notify` (`api/internal/notifysvc/service.go:111`) **persists an inbox row first, then best-effort Slack DM**, so one call covers both surfaces. This closes gap 2 above and is server-side work independent of the retry (M5). Scope — all failures vs the forge case only — is Open Decision 1.

### Deferred: Layer B — surviving a sustained outage (follow-up PRD)

This is recorded, not built here, so the follow-up starts from the verified analysis rather than re-deriving it.

**The constraint.** When Layer A's fast retries are exhausted and the forge is genuinely unreachable, a run should wait the outage out rather than fail — but **it cannot do that in `running` status.** `SweepRunningTimeout` (`api/internal/store/queries/runtime.sql:1300`, driven from `service.go:4232`) fails any `running` non-chat run whose `started_at` is older than `COALESCE(budget_wall_seconds, RUN_TIMEOUT)` (default **2h**, `config.go:671`), heartbeat-independent, and the clock runs from `started_at` — not from when the push begins. So an unguarded `running` park would be killed mid-retry with reason `"run exceeded RUN_TIMEOUT"`, its eventual `completed` silently dropped by the `SetRunCompleted` WHERE-guard (`runtime.sql:1125`), a wasteful judge pass queued (`service.go:4245`), and an MR left open on a `failed` run (D7).

**The two candidate mechanisms** (the follow-up must choose):
- **A distinct `limit_wait`-style park status** (e.g. `forge_wait`) that the wall sweep ignores, releases the worker slot, preserves on-disk state, and is promoted back to `queued` by the sweeper (`PromoteLimitWaitRuns`, `service.go:4334`) with resume affinity. More capable — it decouples outage-survival from the run's remaining wall budget entirely — at the cost of growing the 9-status contract (WS-hub unknown-rewrite, CLI skill, `run wait --until`, web board).
- **A lighter marker + budget bound**: keep the run in `running`, add a nullable `runs` marker that exempts it from the sweep (`AND <marker> IS NULL`), and cap the extended window at `min(configured, remaining wall budget)`. No new status, but it has a real weakness recorded in **D10**: the push is the last thing a run does, so a long run reaches it with little wall budget left and its bounded window shrinks toward zero — least protection for exactly the long runs that motivated this PRD.

**Config plumbing rides with Layer B.** The extended-retry cadence and a max-window tunable belong to Layer B, delivered per-claim via `ClaimConfig` (`agent/src/protocol.ts:363`, read worker-side from `ctx.config` at `sdk-executor.ts:429`, defaults in `config.go`, overridable via `api.config` → `api-configmap.yaml:10-12`), not worker env vars (unreachable on hosted k8s, #88). It is *not* needed for Layer A.

**Per-attempt timeout** is a Layer B concern too: `GIT_TIMEOUT_MS` = 10m (`git.ts:116`) means a *hung* connection blocks up to 10 minutes per attempt, so extended retry likely wants a shorter per-attempt git timeout.

## Milestones

### Committed in this PRD (Layer A + failure notification)

- [x] **M1 — Error classifier.** A worker-side transient-vs-permanent classifier for git stderr and forge HTTP status, unit-tested against real error strings. Named as a deliberate second implementation of the `isTransient` decision (`client.ts:443`) and pinned to it with a shared table. **Precedence is explicit and tested (D9, S2):** permanent patterns are checked **first and win**; an error matching only git's generic `Could not read from remote repository` trailer is **not** transient (it also trails auth/permission denials); an unmatched error defaults to **permanent (fail fast)**. Pin `LANG=C`/`LC_ALL=C` for the push subprocess (or match stable tokens) so stderr matching does not depend on locale.
- [x] **M2 — Layer A retry.** Wrap `pushBranch` (`git.ts:321`) and the whole `createMergeRequest` call (`forge.ts:100`) in a bounded backoff loop using M1's classifier and a worker-side constant schedule. Push idempotent on retry; MR-create is already idempotent — treat its adopt-existing return as success, and wrap the entire call so a transient `findOpenMr` failure re-runs rather than fails. Permanent errors bypass the loop and fail as today. **Independent of config plumbing; this is the #216 fix.**
- [x] **M5 — Close the failure-notification gap.** A `failed` run emits an in-app notification via `notifysvc.Notify` (5th caller), so non-Slack users get an inbox badge on failure. Scope per Open Decision 1 (recommend general run-failure). Independent server-side work.
- [x] **M6 — Tests.** Agent `node --test`: classifier table with **discriminating** cases — (i) #216 stream-reset ⇒ retry; (ii) auth failure ⇒ no retry; (iii) `protected branch` ⇒ no retry; (iv) non-fast-forward/`[rejected]` ⇒ no retry; (v) stderr containing BOTH transient and permanent substrings ⇒ **permanent wins** (precedence); (vi) the bare `Could not read from remote repository` trailer ⇒ **not** transient — plus an assertion each case is exercised; fast-retry succeeds on Nth attempt; permanent fails fast; MR-create adopt-existing + transient-`findOpenMr` re-run. API LiveDB (`./e2e/run-store-it.sh`): failed-run `notifysvc.Notify` writes an inbox row + attempts Slack. Follow `.claude/rules/agent.md` (`--test-timeout=120000`) and `.claude/rules/go.md`. *(Realized as a DB-free unit test, not the LiveDB e2e lane as originally scoped: `run_failure_notifier_test.go` drives `RunFailureNotifier.handle` against fakes for `GetRunByID` + `InsertNotification`, covering the stop_kind gate table above; the two generated queries themselves already have store-level coverage. It is inbox-only by design — Slack is deliberately **not** attempted, since the existing slacksvc failed-DM already covers Slack and a second attempt here would double-DM opted-in users.)*
- [x] **M7 — Docs + ADR.** `ARCHITECTURE.md` run-lifecycle (push retry + the new failure notification); a `docs/` note if the inbox copy needs one. Write **`adr/0284-forge-push-retry-classifier.md`** capturing the never-retry-a-permanent-rejection invariant and the permanent-first precedence rule (D9). Note in the ADR/PRD that Layer B (extended-outage park) is a deliberate follow-up.

### Deferred to a follow-up PRD (Layer B — extended-outage survival)

- **M3 — Config plumbing** (the retry-window tunables via `ClaimConfig`) and **M4 — the forge-unreachable park** (guarded against `SweepRunningTimeout`; mechanism per *Deferred: Layer B*) move to a follow-up PRD, together with their tests (park exemption from the wall sweep, sweeper promotion, `ClaimConfig` carries the tunables) and docs. Nothing urgent rides on them — Layer A prevents the data loss. Not yet filed; see D5/D7/D10 for the starting analysis.

## Success criteria

**Committed (Layer A + notification):**
1. A transient push error (the #216 stream reset) is retried and the run **completes** instead of failing — no re-implementation, no re-spent budget.
2. A **permanent** rejection (auth, protected branch, non-fast-forward) still fails **immediately** — never retried. The `main`-never-touched directive is unaffected (retrying an idempotent push to the agent branch touches nothing protected).
3. A failed run lands an in-app notification for users **without** Slack.
4. MR-create retried after a lost response does **not** create a duplicate MR. *(Already holds today via `forge.ts:100-120`; M2 must not regress it, and must additionally survive a transient `findOpenMr` during a duplicate.)*

**Deferred (Layer B follow-up):** during a sustained outage the run parks (not killed by `SweepRunningTimeout`) up to a configurable window, emits exactly one park notification, then fails with a reason naming the outage.

## Decision Log

- **D1 — Retry in the worker, not via requeue.** The completed diff is already in the worker's bare repo; retrying the push reuses it. Requeue (`RUN_MAX_REQUEUES`) is worker-death recovery and would re-provision/re-run — the expensive path this PRD exists to avoid. Requeue stays untouched.
- **D2 — Classify, then retry.** The one non-negotiable is that permanent forge rejections fail fast. A blanket retry-on-any-error would retry protected-branch and auth rejections, weakening a guardrail. The classifier (M1) is the load-bearing piece; the backoff loop is the easy part.
- **D3 — MR-create is ALREADY idempotent — do not rebuild it.** All three drivers adopt an existing MR on the duplicate status (base `createMergeRequest` `forge.ts:100-120`; `findOpenMr` gitlab/forgejo/github on 409, github also 422), and `runner.ts:1058-1061` documents this as battle-tested for ci_fix/resume. SC4 holds today. M2 wraps this call in the retry loop and treats adopt-existing as success. The one edge to *add*: wrap the whole call so a transient `findOpenMr` failure after a duplicate POST re-runs instead of throwing a (permanent-classified) 409/422.
- **D5 — Layer B is deferred, because parking safely is a real design choice, not a quick add.** Review found the decisive constraint (D7): `SweepRunningTimeout` (`runtime.sql:1300`, `RUN_TIMEOUT` 2h from `started_at`) would kill an unguarded `running` park before its window elapsed. The right guard is either a new non-terminal status (the `limit_wait` model, #35 — more capable, grows the status contract) or a marker+budget bound (lighter, but see D10). Since Layer A already prevents the data loss, this choice carries no urgency and is deferred to a follow-up PRD rather than rushed alongside the incident fix.
- **D6 — Health (#47) is not the notification hook.** Health is derived server-side from telemetry and written through the single `SetRunHealth` writer (`workersvc/health.go:171`); there is no worker-driven health path and the enum is CHECK-constrained (`00057_run_health.sql`). `notifysvc.Notify` avoids both a worker health endpoint and an enum migration.
- **D7 — The `running`-park failure modes (why D5 defers rather than ships a `running` park).** An unguarded `running` park hits four distinct failures, all confirmed: (a) killed by the wall sweep before the window; (b) wrong failure reason `"run exceeded RUN_TIMEOUT"`; (c) a wasteful judge pass enqueued on timeout (`service.go:4245`); (d) the MR-on-failed-run race — the worker recovers, opens the MR, then `SetRunCompleted` updates 0 rows against the now-`failed` row (`runtime.sql:1125`), leaving an open MR on a failed run. Any Layer B mechanism must avoid all four.
- **D8 — Slot release vs hold is a Layer B tradeoff (recorded for the follow-up).** `limit_wait` *releases* the worker slot and preserves on-disk state; a marker-based `running` park would *hold* it. During a forge-wide outage holding costs nothing (no other run can push either), but releasing also stops the wall timer and lets the fleet re-plan. The follow-up decides.
- **D9 — Record the never-retry-a-permanent-rejection invariant in an ADR.** The classifier's guarantee that a permanent forge rejection (auth, protected branch, non-fast-forward) is *never* retried is guardrail-adjacent and long-lived — a future edit to the transient pattern list could silently weaken it. That meets the repo's bar for an ADR ("an invariant a future change would silently break"), with `adr/0035-run-limit-retry.md` as direct lifecycle precedent. Write `adr/0284-forge-push-retry-classifier.md` (M7).
- **D10 — The bound-window guard is weaker than it looks (for the follow-up, so the mistake isn't repeated).** Setting Layer B's extended window to `min(configured, remaining wall budget)` ties outage-survival to the run's leftover *work* budget. Because the push is the last thing a run does, a long run reaches it with little budget left and the window shrinks toward zero — the least protection for exactly the long runs that motivated this PRD. The follow-up should prefer a wall-timer-decoupled mechanism (the distinct-status park) unless a lighter option is proven sufficient.

## Risks & mitigations

- **Retrying a permanent rejection weakens a guardrail.** Mitigated by M1's explicit permanent-error list and permanent-first precedence (fail-fast), SC2, the ADR (D9), and tests against real rejection strings including non-fast-forward and mixed-substring precedence (M6 cases iii–vi).
- **Classifier drift from `isTransient`.** Two implementations of one decision. Mitigated by the shared differential table (M1/M6) and locale-pinning.
- **A transient `findOpenMr` during a duplicate POST could fail a run whose MR exists.** Mitigated by wrapping the *whole* `createMergeRequest` in the retry loop, not just its final thrown status (D3, M2).

## Out of scope

- **Layer B (extended-outage park) and its config plumbing** — deferred to a follow-up PRD (D5), not permanently out of scope.
- Retrying non-push forge operations (clone, fetch, label writes, poller sync) — this PRD is the worker's final push/MR hop, the observed failure. A general forge-client retry layer is a candidate follow-up, noted not built.
- Changing the requeue / worker-death recovery path.
- Rebuilding MR-create idempotency (already exists — D3).

## Open decisions (for the user)

1. **Failure-inbox-notify scope (M5).** Emit the in-app notification on **all** run failures (general — closes #46's gap broadly) or only on a subset? **Recommend general.** Confirm.

*(The Layer B park mechanism, its window/cadence, and the entering-park signal are deferred with Layer B to a follow-up PRD — see D5/D7/D8/D10.)*

## Milestone parallelization

**Layer A (M1→M2)** is independent of everything else and independently shippable as the #216 fix — its retry schedule is a worker-side constant, no config plumbing needed. **M5** (failure-notify) is independent server-side work. So M1→M2 and M5 can land in parallel; M6 tests gate on their track; M7 docs+ADR independent. Single repo, one MR.

---

*Drafted 2026-08-09 against code at HEAD (`7112ab60`); revised the same day after an architect review that verified every load-bearing citation and found the `SweepRunningTimeout` constraint (D5/D7), the pre-existing MR idempotency (D3), and Layer A's independence from config plumbing. The owner then scoped this PRD to Layer A + the failure-notification gap and deferred Layer B (extended-outage survival) to a follow-up PRD, after weighing the distinct-status park against a marker+budget bound (D10). Research and review confirmed the worker-config plumbing (`ClaimConfig` vs worker env), the health-vs-notify hook, and the four `running`-park failure modes against `agent/src/protocol.ts`, `api/internal/workersvc/health.go`, `api/internal/notifysvc/service.go`, and `api/internal/store/queries/runtime.sql`.*
