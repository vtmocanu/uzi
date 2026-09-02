# PRD #1030: Worker resume durability — hold affinity through a roll + reliable forge checkpoints

**GitHub Issue**: [#1030](https://github.com/vtmocanu/uzi/issues/1030)
**Priority**: High
**Parent incident**: run #1009 (`e4d60edb`) `resume_lineage_break`, 2026-09-02

> This PRD is written to be implemented by an offline uzi worker (restricted egress: forge + `*.anthropic.com` + package caches, no open web). Every external fact is a live measurement from this deployment, baked in below. All code anchors are `path:line @ main 8079f6b`; symbols are given so they survive line drift — **re-derive each anchor at implementation time** (`grep` the symbol), do not trust the line number blind. Two mechanism claims below were verified by a throwaway probe against the pinned go-git v5.19.2; those are marked "verified by probe".

## Problem

On 2026-09-02 a batch of runs hit the shared 5-hour Anthropic limit (reset 14:00Z) and parked (`wait_on_limit=true`). At the same moment the hosted worker fleet auto-rolled its image (`agent-base` `0.75.0 → 0.75.1`, deploy revision 54, pod swap at **13:52:00Z**). Run #1009 had its warm working clone and its committed branch tip `df441342` (23 commits, all four milestones, already reviewed) on worker `8fab6b82`'s **persistent** PVC. On resume (~13:51:56Z) #1009 was re-claimed by a **different** live worker (`8e1fef71`) that had no clone, emitted `resume_lineage_break` ("no earlier work could be recovered for this run on this worker — starting from the default branch"), cold-started from the default branch, and redid the whole run. The work was only recovered by manual PVC surgery (PR #1028).

Two independent defects combined:

1. **The affinity rule releases a parked run the instant its owner worker is `draining` (cordoned for a roll)** — even though a roll is transient (same worker row, same PVC, new pod). So a routine image roll during a rate-limit park forces a cold cross-worker resume.
2. **The forge-checkpoint safety net that is supposed to carry the unpushed tree across workers silently failed.** uzi already publishes checkpoints to `refs/uzi-checkpoints/<branch>` (24 such refs exist live on GitHub), but **`refs/uzi-checkpoints/agent/issue-1009` was never created** — every publish for #1009 errored server-side (`pushbroker: push: object not found`), invisibly to the run.

`main` is never touched by any of this work; the guardrails are unchanged.

## Root cause

### Defect A — affinity falls open on a roll-cordon (the trigger)

- A parked run keeps its `worker_id`: `SetRunLimitWait` (`api/internal/store/queries/runtime.sql`) sets `running → limit_wait` and never touches `worker_id`; `PromoteLimitWaitRuns` sets `limit_wait → queued`, also keeping `worker_id` (retained deliberately for affinity). So the worker still counts as **Busy** — the `busy` column in `ListHostedWorkersForController` (`api/internal/store/queries/hosted_workers.sql`, "does this worker hold any non-terminal run") counts any run whose `status NOT IN ('completed','failed','cancelled')`. Both `limit_wait` and `queued` are non-terminal. **The worker was correctly cordoned for the roll, not force-rolled.** ("Keep the worker Busy so it cordons" is therefore already the behavior — it is *not* the fix.)
- A cordoned worker refuses **all** claims, including its own promoted run: the pre-`ClaimRun` early return in `api/internal/workersvc/service.go` claim path, `if wkr.DrainingSince.Valid { return nil, nil }`.
- The claim affinity leg in `ClaimRun` (`api/internal/store/queries/runtime.sql`) holds a run to its prior worker only while that worker is a live, **non-draining** claim target:
  ```sql
  AND (r.worker_id IS NULL
       OR r.worker_id = @worker_id
       OR NOT EXISTS (SELECT 1 FROM workers ow
             WHERE ow.id = r.worker_id
               AND ow.last_heartbeat_at IS NOT NULL
               AND ow.last_heartbeat_at >= @heartbeat_cutoff
               AND ow.draining_since IS NULL)   -- <- fall open when owner is DRAINING
       OR r.updated_at < @affinity_cutoff)       -- <- 2h ceiling
  ```
  When the owner is draining (cordoned for the roll), the `draining_since IS NULL` requirement fails → the `NOT EXISTS` becomes TRUE → affinity falls open → any live non-draining peer claims the run cold. In #1009 that cold claim (by `8e1fef71`) is also what *idled* `8fab6b82`, which then let its deferred roll fire at 13:52:00Z.
- **The wedge**: ADR-628 D3a chose fall-open-on-drain because "a dead (heartbeat-stale) or draining owner **will never resume it**, so pinning to it is strictly worse than re-claiming elsewhere" (`adr/0628-cross-worker-resume-durability.md` D3a rationale). That is TRUE for a death/teardown but **FALSE for a roll** — the same worker row and PVC come back.
- **Roll vs teardown is distinguishable** (the discriminator; conclusion verified, citations corrected):
  - A **roll** sets `draining_since` (`Cordon` / `CordonHostedWorker`) and keeps the worker **row** and its PVC; `RegisterWorker` (`api/internal/store/queries/runtime.sql`, clears `draining_since` on register) restores it.
  - A **teardown** deletes the DB `workers` row **API-side** — `DeleteWorkerForUser` (`api/internal/store/queries/runtime.sql`, user revocation) or `ReapEphemeralWorkers` (`api/internal/store/queries/hosted_workers.sql`, ephemeral GC). That row deletion is what makes the next controller `Poll` (`api/internal/hostedsvc/service.go`) omit the worker, which then triggers the controller's kube teardown (`controller/internal/kube/materializer.go` `teardown()`, which deletes only kube objects — the controller holds no DB credential). So the DB row is gone **before** the kube teardown, and `teardown ⟺ row absent`.
  - Therefore **worker-row presence + `draining_since`** is a sufficient roll-vs-teardown discriminator with no new column: `ClaimRun`'s affinity `NOT EXISTS (… workers ow …)` already falls open when the row is gone (teardown); the fix only needs to stop falling open when the row is present **and** draining (roll).

### Defect B — the checkpoint publish failed every time (the safety net that should have saved #1009)

Forge checkpointing already exists end to end: `agent/src/runner.ts` `publishCheckpointBestEffort` → `agent/src/git.ts` `checkpointPack` (a **non-thin** incremental pack `git pack-objects --revs --stdout ^<excludeRef>`, `excludeRef` = the worker's bare-clone default at pack time) → `agent/src/client.ts` `publishCheckpoint` (`POST /api/worker/runs/{id}/publish`, join token, no PAT) → `api/internal/handler/worker_protocol.go` `WorkerRunPublish` → `api/internal/workersvc/service.go` `Publish` → `api/internal/pushbroker/pushbroker.go` `Publish` (go-git; depth-1 fetch of ≤3 refs; strict-descendant; **non-forced** push to `refs/uzi-checkpoints/<branch>`). The resume path already **fetches** the remote checkpoint before falling back to default (`agent/src/git.ts` mirrors `+refs/uzi-checkpoints/*:refs/uzi-checkpoints/*` on every claim; `runnerCloneForBranch` prefers it). So the recovery leg is not the problem — **the publish itself failed.**

Live evidence from this deployment's api logs (namespace `uzi`, context `meta-dev-02`), all `level=ERROR msg="worker run publish"` for run #1009:

```
11:51:50Z  publish: pushbroker: push: object not found
12:25:22Z  publish: pushbroker: push: object not found
12:45:08Z  publish: pushbroker: push: object not found
12:49:46Z  publish: pushbroker: push: object not found
13:03:58Z  publish: pushbroker: push: object not found
13:04:17Z  publish: pushbroker: push: object not found
13:07:22Z  publish: pushbroker: fetch: context canceled
13:18:42Z  publish: pushbroker: push: object not found
13:22:39Z  publish: pushbroker: push: object not found
13:27:32Z  publish: pushbroker: push: object not found   (x3)
14:11:51Z  publish: pushbroker: push: object not found
```

~12 × `pushbroker: push: object not found` across the whole run (`git ls-remote origin 'refs/uzi-checkpoints/*'` on 2026-09-02 shows 24 refs, `agent/issue-1009` absent).

**Mechanism (verified by probe against go-git v5.19.2):** `pushbroker.Publish` calls `remote.PushContext`, and the failure is `PushContext`'s **own** send-set computation, raised locally before any bytes reach the forge — not a transport step. `PushContext` computes `haves` (the remote-advertised ref hashes) and calls `revlist.Objects(storer, [tip], haves)`, walking from the tip and excluding the advertised default. The broker's storer holds only a **depth-1** snapshot of origin's **current** default `D_new` (from `fetchBaseRefs`; depth-1 is deliberate, to bound memory). When `main` has advanced since the worker cloned, the branch-point's parent (the worker's **old** default `D_old`) and every subtree `main` changed since are not reachable from `D_new`'s depth-1 tip and are absent from the storer → `plumbing.ErrObjectNotFound` → `object not found`. It is **self-perpetuating**: because the ref never lands, every later publish is again a "first publish" and fails identically (matching #1009's 12 identical errors). The 24 existing refs are runs where `main` was quiet since clone (`D_old == D_new`) or where a *second* publish ran after a checkpoint ref already existed (then `haves` contains the checkpoint tip and the walk terminates). The 13:07Z `fetch: context canceled` is the 60s publish-duration ceiling expiring inside `fetchBaseRefs`, transient and unrelated. **Not** thin packs (the pack is non-thin — `git.ts` `checkpointPack` has no `--thin`, `pushbroker.go` header says "raw (non-thin)"), **not** the remote (it has all of `D_old`), **not** workflow scope.

### Observability defects that hid both (necessary but neither was #1009's cause)

- **Publish ERRORs never reach the run feed** (this is what hid #1009): the api answered 500; `agent/src/client.ts` `publishCheckpoint` collapses any non-2xx to `null`; `agent/src/runner.ts` returns `false` on `null` **without logging** (the `runLog.warn` fires only on a thrown error). The api `slog.Error`s it (that is how we found the cause) but emits no `run_messages` event and no metric, and the log line carries no run id. The worker was silent all 13 times.
- **Skip-reported-as-success** (a separate latent bug, G1): `agent/src/runner.ts` publish handler returns `res !== null`; the api answers 200 `{published:false, skipped:...}` for a skip (e.g. `workflow_scope`), which is truthy → `lastPublishedTip` advances, the run log prints "published", nothing retries. `.published` is read nowhere in `agent/src`; `agent/src/protocol.ts` `PublishResponse.skipped` omits `"workflow_scope"` (which the api does return). This did not cause #1009 (that was a 500, not a skip) but silently masks the workflow-scope class for other runs.

### What each fix recovers

- **Defect A fix** keeps the run on **its own worker**. Because a Busy worker is cordoned and **not** rolled until idle, the run resumes on the **old, still-alive draining pod** (no pod swap in the normal case) → both the warm git clone (no re-implement) **and** the in-memory SDK session (no re-plan) survive → `resume_continued`, not `resume_lineage_break`. This is what would have prevented #1009 outright.
- **Defect B fix** makes the forge copy the reliable cross-worker carrier for the cases Defect A cannot cover: a worker that is genuinely **gone** (node death, PVC reclaim, ephemeral teardown), and any resume that still lands on a different worker.

Cross-worker **session/transcript** durability beyond same-worker resume remains an explicit non-goal (deferred to #556); this PRD does not attempt it.

## Scope

**In scope**: Defect A (affinity through a roll), Defect B (pushbroker publish fix), and the observability + adoption + cleanup + shutdown hardening that makes the checkpoint net trustworthy.

**Out of scope** (separate follow-up issues, under *Deferred*): the broker-side `.github` overlay wrapper for behind-on-workflows checkpoint durability (G2), forge checkpoints for non-issue run kinds (G5), a `runs.checkpoint_*` owner-anchor column, chat-run affinity (see M2), and cross-worker session durability (#556).

**No database migration.** The roll-vs-teardown discriminator uses existing columns (`draining_since`, worker-row presence); the M2 change is a `ClaimRun` query + service change. This keeps the branch free of a goose migration and its numbering coordination, and free of `.github/workflows/**` (the worker PAT lacks `workflow` scope).

## Milestones

Each milestone lands with its own red-then-green regression test (watched failing on the unfixed code, per the repo's testing discipline) — there is no separate "tests" milestone.

### M1 — Reliable, observable checkpoint publish (the real #1009 cause)
- **Fix `pushbroker: push: object not found` by forwarding the worker's pack** instead of letting go-git recompute the send-set. **Verified by probe**: replacing `remote.PushContext` with a manual receive-pack session that sets `req.Packfile` to the worker's pack sends the bytes verbatim (go-git's `packp` `Encode` is a byte passthrough; the send-set walk lives only in `PushContext`, not the transport), and reachability is then done by the **remote** against its full store, which has `D_old`. In the probe, this landed the ref with only the depth-1 default snapshot local. Seam: `api/internal/pushbroker/pushbroker.go` `Publish` — replace the `remote.PushContext` call with `client.NewClient(ep)` → `NewReceivePackSession(ep, auth)` → `AdvertisedReferencesContext` (reuse the listing already done in `fetchBaseRefs`) → `packp.NewReferenceUpdateRequestFromCapabilities(ar.Capabilities)` with one `Command{Name: <checkpoint ref>, Old: ar.References[<ref>] (or plumbing.ZeroHash if absent), New: <tip>}` → `req.Packfile = io.NopCloser(bytes.NewReader(<worker pack>))` → `sess.ReceivePack(ctx, req)` → read `rs.Error()`. Setting `Old` to the advertised value gives a **server-side compare-and-swap** (a stronger never-forced guarantee than today's client-side check). Handle `Old == New` (already up to date) before sending. Map the report-status text for a moved ref (non-fast-forward / `failed to update ref` / `cannot lock`) to `ErrNotDescendant`, and keep the existing workflow-scope-rejection detection on that same report-status line. Keep the existing pre-push steps (budget scan, applying the pack for the ancestry checks, strict-descendant). This also removes the revlist+re-encode (cuts per-publish memory/CPU) and needs no wire change, no new header, no forge capability. **Fallback only if a transport surprise appears**: a bounded-depth fetch reaching `merge-base(tip, default)` (reintroduces the OOM tension depth-1 was chosen to avoid) — do not implement unless (g) is empirically blocked.
- **Fix the pre-existing residual this exposes**: `strictDescends`/`descendsOrEqual` (`pushbroker.go`) use `Commit.IsAncestor`, which walks from the tip and, on a genuine non-descendant, runs past the pack into `D_old` and returns `object not found` as a 5xx instead of `ErrNotDescendant`. Map `ErrObjectNotFound` from those two calls to `ErrNotDescendant`.
- **Regression test — the existing `file://` harness reproduces (verified by probe); no `git-http-backend` needed.** A `file://` URL has a scheme, so go-git takes the plain in-memory-storer walk (not the local-endpoint shortcut), exactly as over HTTPS. Add `TestPublishFirstCheckpointAfterDefaultAdvanced` to `api/internal/pushbroker/pushbroker_test.go`: as `TestPublishFirstCheckpointOnNeverPushedBranch`, but after `pack := f.pack(tip, base)`, `checkout main`, commit to a directory the branch did **not** touch, `f.pushMain()` (so `D_new != D_old`), then `Publish`. Unfixed → error contains `object not found`, `originRef(...) == ""`; fixed → nil error and `originRef(...) == tip`. Keep every existing test green (the second-publish / non-ff / pack-bomb cases exercise the same new send path).
- **Surface publish failures to the run feed** (both the error path — necessary for #1009 — and the skip path G1):
  - `agent/src/client.ts` `publishCheckpoint` returns the HTTP status on failure instead of collapsing every non-2xx to `null`.
  - `agent/src/runner.ts` `publishCheckpointBestEffort`: on `null`/non-2xx **or** `published !== true`, emit a run-feed `status` line naming the outcome (`checkpoint publish failed: HTTP <code>` / `skipped: <reason>`) and a `runLog.warn`, do not advance `lastPublishedTip`, key success on `published === true`; dedupe to one line per distinct outcome per run so the 20-minute time-gate does not spam.
  - The **park path** result must be explicit on the feed (the ADR-628 trip-wire that was missing on the publish side): "park checkpoint published to origin" vs "park checkpoint NOT published — a resume on another worker will restart from the default branch".
  - Add `"workflow_scope"` to `agent/src/protocol.ts` `PublishResponse.skipped`.
  - Server side: `api/internal/handler/worker_protocol.go` `WorkerRunPublish`'s `slog.Error("worker run publish", …)` gains `run_id`, `worker_id`, `branch`, and a `checkpoint_publish_failures_total{reason}` metric.
  - Tests: a fake api answering `{published:false, skipped:"workflow_scope"}` leaves `lastPublishedTip` unset, retries, and emits a feed line; a fake api answering 500 emits a feed line and does not advance the tip.
- **Validation**: the unit test above is the gate. A real-forge confirmation ("a long run whose `main` moved lands a checkpoint ref, visible via `git ls-remote`") is a **manual/maintainer** check, not worker-gated.

### M2 — Hold affinity through a roll (the trigger fix)
Two seams edited **together** (either alone is incorrect):
- **`api/internal/store/queries/runtime.sql` `ClaimRun`** (affinity leg): hold the pin when the owner worker **row exists and is draining**, regardless of heartbeat — the ~2 min pod-swap edge exceeds the 45s heartbeat-stale threshold, so a heartbeat-gated test would wrongly fall open mid-swap. Fall open immediately only when the row is **gone** (teardown) or the owner is **heartbeat-stale with `draining_since IS NULL`** (death/hang, ADR-628 D3a's protected case). Bounded by the existing `@affinity_cutoff` 2h ceiling.
- **`api/internal/workersvc/service.go` claim path**: a draining worker must be able to claim **only its own** promoted run, never new/unclaimed/fallen-open runs (which would violate PRD #422 D7 "a draining worker claims nothing new"). Do **not** simply lift the `if wkr.DrainingSince.Valid { return nil, nil }` early return — that would let it reach `ClaimRun` and claim anything eligible. Instead thread a `@claimant_draining` flag into `ClaimRun` and add `AND (NOT @claimant_draining OR r.worker_id = @worker_id)`, so a draining worker's claim is scoped to its own run. The normal (non-draining) path is unchanged.
- **Result**: the parked run's owner is cordoned and not rolled until idle, so the **old, still-alive draining pod re-claims its own promoted run** and resumes it in place; the roll is deferred until the run completes. Verify the session recovers (see D5): the old pod is never killed in this path, so the in-memory SDK session is intact → `resume_continued`. (The new-pod path occurs only past the 24h drain deadline or a force-roll; there, recovery falls to M1's checkpoint.)
- **Preserve ADR-628 D3a's protected case**: a killed/torn-down owner (row gone, or heartbeat-stale with `draining_since IS NULL`) must still fall open immediately — test it.
- **Tests**: (a) a run whose owner is cordoned for a roll stays pinned and is re-claimed by that same worker (not a peer); (b) a run whose owner is **draining and heartbeat-stale** (the pod-swap edge) stays pinned; (c) a run whose owner is **killed (heartbeat-stale, not draining)** or whose **row is deleted** falls open to a peer immediately; (d) a draining worker does **not** claim a new/unclaimed run.
- **Constants** (do not change; cite in tests): `WORKER_AFFINITY_CEILING` 2h, `WORKER_HEARTBEAT_STALE` 45s (`api/internal/config/config.go`), drain deadline 24h (`controller/internal/config/config.go`).
- **Chat runs are out of scope** and must stay working: `ClaimChatRun` (`api/internal/store/queries/chat.sql`) uses a *different, older* affinity leg (`worker_id = self OR updated_at < cutoff`, no liveness/drain leg), so it does not have this bug and this change must not touch it. Chat runs are interactive and re-askable, with no unpushed forge branch to lose — deferring them is safe. (M5 fixes ADR-628's stale "universal predicate" wording accordingly.)

### M3 — Don't discard a valid checkpoint when `main` advanced during a park
- On a **resume** (`claim.session_id != null`, threaded into `runnerCloneForBranch` as a flag) with no `origin/<branch>` (unpushed branch), adopt the mirrored checkpoint using the disjoint-history guard only — **no** ancestry test against the *current default* — mirroring the same-worker tracking leg's rule and rationale in `agent/src/git.ts`. Keep the strict strict-descendant test when `origin/<branch>` **exists** (that is genuinely competing published work). This subsumes the #759 `wip(park):` marker rescue via the existing adopt-time unwrap path.
- **Validation**: a resume that fetches a good checkpoint adopts it even though `main` moved during the park; committed milestones are preserved. A **fresh** run (`session_id == null`) does **not** inherit a prior run's checkpoint.

### M4 — Publish on shutdown + checkpoint cleanup
- **Publish on graceful shutdown**: add `publishCheckpointBestEffort` to the SIGTERM/shutdown branch (`agent/src/runner.ts`, currently fetch-back only) under a short budget within the k8s termination grace (30s default; no `terminationGracePeriodSeconds` is set in `controller/internal/kube/materializer.go` — confirmed absent). Bounds loss for roll-while-running / eviction / OOM / node-drain to the last checkpoint instead of ≤20 min.
- **Cleanup stale checkpoint refs**: add a best-effort ref delete (`api/internal/pushbroker` go-git delete refspec `:refs/uzi-checkpoints/<branch>`) called from the terminal transitions in `api/internal/workersvc/service.go` `SetState` (completed, failed, cancelled) plus the server-side cancel path. Stale refs otherwise block a later run on the same issue with a `not_descendant` skip. `failed` is terminal and is **not** requeued (the shutdown path deliberately does not report `failed`), so deleting on `failed` cannot race a requeue-resume; note that it removes the forge-side breadcrumb for a failed run, but the PVC `refs/uzi-runner/*` remains the primary recovery path (per the uzi-watcher skill).
- **Validation**: a terminal run leaves no `refs/uzi-checkpoints/<branch>`; a SIGTERM mid-run lands a final checkpoint; a subsequent run on the same issue is not blocked by a stale ref. The one-off sweep of the 24 existing orphans is a **maintainer** action (needs network + PAT), tracked separately from the worker-gated code.

### M5 — Docs, ADR, specs
- **Amend `adr/0628-cross-worker-resume-durability.md`** (refine the existing decision, do not mint a new ADR number): record that D3a's "a draining owner will never resume it" holds only for death/teardown, not for a roll; document the roll-vs-teardown discriminator (row presence + `draining_since`) and the two-seam affinity change; **fix the stale `WORKER_AFFINITY_CEILING` default (the ADR says 30m; the shipped default is 2h)**; and **correct the "universal predicate" claim** (the affinity leg is `ClaimRun`-only — `ClaimChatRun` has a different, older leg). Note the pushbroker publish fix and the observability additions.
- Update `docs/` where worker rolls / resume behavior are described (e.g. the `WORKER_AFFINITY_CEILING` configuration reference entry) and `ARCHITECTURE.md`'s run-lifecycle/agent-runtime section — additively, in its own paragraph (note PR #1028 also edits `ARCHITECTURE.md`; keep this to a distinct section to avoid a rebase clash).
- Append a `specs/ai.md` decision section. **The section number is a draft — assign the next free number at landing** (three sibling PRDs also append to `specs/ai.md`; renumber on the landing rebase, same discipline as goose numbers).
- Check `api/cmd/uzi/` for any CLI surface reporting resume/checkpoint/worker-drain state; update if a field changed (repo convention). None is expected — confirm.

## Success criteria (all worker-gated unless marked)
1. `pushbroker.Publish` no longer fails `object not found` for a run whose `main` advanced mid-run; the new `file://` regression test is red on the unfixed code and green after the fix, and every existing pushbroker test stays green.
2. A publish skip or error is visible in the run feed; a skip does not advance `lastPublishedTip`; `PublishResponse.skipped` includes `"workflow_scope"`; the park-path publish result is stated on the feed.
3. A run parked while its owner worker is cordoned for a roll is re-claimed by **that same worker** (no `resume_lineage_break`), including across the draining+heartbeat-stale pod-swap edge; a run whose owner is **killed** or whose **row is deleted** still falls open to a peer immediately; a draining worker never claims a new run. (All by test.)
4. A **resume** adopts a checkpoint that diverged from a moved default (committed milestones not set aside); a **fresh** run does not inherit a prior run's checkpoint. (Note the residual in D8: a resumed run that never published its own checkpoint can still adopt a prior run's checkpoint on the same branch until the owner-anchor lands.)
5. Graceful shutdown publishes a final checkpoint; every terminal transition deletes the run's checkpoint ref (by test). **Maintainer follow-up (not worker-gated)**: the 24 existing orphan refs are swept once.
6. `task gate:api`, `task gate:controller`, and `task gate:agent` green (lint 0 issues, deadcode unchanged, tests pass with `-race`).
7. No `.github/workflows/**` in the branch diff (implementation or validation).
8. `adr/0628` and the docs reflect the new behavior truthfully (incl. the 30m→2h and universal-predicate corrections); `specs/ai.md` section added with a landing-assigned number.

## Deferred (separate follow-up issues — file, do not implement here)
- **G2 — behind-on-workflows checkpoint durability**: a broker-side `.github` overlay wrapper commit. Riskiest change (go-git object synthesis in the secrets-holding api); its own PRD. Until then, M1's loud skip makes the gap visible.
- **G5 — forge checkpoints for non-issue run kinds** (`task`, `self_improve`, `prompt`, `mr_rework`, `ci_fix`), currently `unsupported` at `api/internal/workersvc/service.go` `Publish`.
- **Owner anchor** (`runs.checkpoint_tip`/`checkpoint_run_id`) so adoption never inherits a foreign checkpoint even on a resume — a schema change; M3's resume-gating + M4's cleanup cover the near term.
- **Chat-run affinity** (`ClaimChatRun`) — its own older leg; interactive/re-askable, low value.
- **Cross-worker session/transcript durability** beyond same-worker resume — #556.

## Decision Log
- **D1**: "Keep the parked run's worker Busy so it cordons instead of force-rolling" is **already the behavior** (verified: `SetRunLimitWait`/`PromoteLimitWaitRuns` keep `worker_id`; the `busy` column counts non-terminal runs). It is not the fix; the fix is to hold *affinity* through the resulting cordon (M2).
- **D2**: Distinguish roll from teardown by **worker-row presence + `draining_since`**, not a new "drain reason" column — no schema change. A teardown deletes the row **API-side** (`DeleteWorkerForUser`/`ReapEphemeralWorkers`) *before* the controller's kube teardown runs, so `teardown ⟺ row absent`; the existing `NOT EXISTS` already handles that case.
- **D3**: #1009's checkpoint never landed because of `pushbroker: push: object not found` (api logs), **not** a workflow-scope skip. Verified by probe against go-git v5.19.2: `remote.PushContext` recomputes the send-set via `revlist.Objects` and hits `D_old` (absent from the broker's depth-1 fetch of `D_new`) — a "first publish after `main` moved" that is self-perpetuating.
- **D4**: The fix is to **forward the worker's (non-thin) pack via a manual receive-pack session** rather than let go-git recompute the send-set (probe arm B: lands the ref with only the depth-1 snapshot local, because the *remote* does reachability with its full store). Bounded-depth fetch is a fallback, not the plan. The "push a non-thin pack" idea is a no-op — the pack is already non-thin.
- **D5**: M2's same-worker resume recovers **both** the git tree and the SDK session in the normal (cordon, no roll) case, because the old pod is never killed → `resume_continued`, no re-plan. Even in the new-pod edge (24h deadline / force-roll), the SDK HOME is on the PVC (`$UZI_DATA_DIR`; the park path preserves HOME), so the transcript should resolve — **M2 must verify** two things it depends on: (a) a rolled worker's new pod re-binds the **same** PVC (the whole "same worker row, same PVC" premise), and (b) the agent-image bump across a roll does not invalidate the SDK transcript (version skew). Cross-worker session durability stays out of scope (#556).
- **D6**: The `object not found` regression is invisible to the pre-existing `pushbroker_test.go` suite (it packs against the *current* main, so `D_old == D_new`). The new test must advance `main` after building the pack. It can stay on the existing `file://` harness — **verified by probe** that `file://` reproduces (a `file://` URL has a scheme, so go-git does not take the local-endpoint shortcut); no `git-http-backend` needed.
- **D7 (accepted tradeoff)**: Holding the pin on `draining regardless of heartbeat` means a **draining-and-dead** worker (image-pull failure / crashloop on the new tag) holds its run until the 2h `@affinity_cutoff` ceiling — a regression vs ADR-628's one-poll recovery for that rare subcase, but bounded (no infinite stall: `updated_at` was refreshed at park→queued, so the ceiling fires ~2h after the last promotion). Accepted as rare and bounded. Optional refinement for the implementer: a shorter sub-ceiling for `draining + heartbeat-stale` that still covers the ~2 min swap but recovers a failed roll faster.
- **D8 (known residuals, eyes-open)**: (a) M2 defers an urgent image roll behind a long parked run (up to 24h) — the normal cost of cordoning any Busy worker, named here because ADR-628 D3a fell open specifically to let that roll proceed. (b) For this workflow-heavy repo, a genuinely-dead worker during a window where `main` gained `.github/workflows` changes still loses work even post-M1 (the fixed push then hits the workflow-scope skip); G2 is what closes that. (c) A resumed run that never published its own checkpoint can adopt a *prior* run's diverged checkpoint on the same branch (M3's relaxed rule + best-effort M4 cleanup); the owner-anchor (Deferred) is the real fix.
