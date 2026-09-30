# ADR-1742: A finalize-pending record, a one-shot resume allowance, and honest restart recovery

**Status**: Accepted (issue #1742: worker record, register snapshot, api attested pass and CLI
wording committed on this branch; the hosted-worker restart acceptance below is the maintainer's,
post-merge, out of band).
**Date**: 2026-09-30
**Issue**: [vtmocanu/uzi#1742](https://github.com/vtmocanu/uzi/issues/1742). No PRD is linked;
this ADR carries the decisions.
**Builds on**: [adr/1390-outage-requeue-readoption.md](1390-outage-requeue-readoption.md) (the
register orphan pass, the terminal-pending lease and the overflow closure),
[adr/1393-worker-outbox-outcomes.md](1393-worker-outbox-outcomes.md) (the authenticated outbox and
the write-ahead terminal journal) and
[adr/1296-durable-run-recovery.md](1296-durable-run-recovery.md) (custody holds and recovery
bundles).

## Context

The #1730 incident: a hosted worker's pod was restarted after the agent had finished its work and
while the worker was still finalizing (fetch-back, push, completion permit, merge request). The
run's outcome had not been written anywhere durable yet, because the terminal journal (ADR-1393)
is written only at the very end of finalize. On restart the worker registered with no register
snapshot, and the api's register-time orphan pass treated the run like any other orphan of a
restarted worker: an earlier eviction had already spent the run's re-queue budget
(`RUN_MAX_REQUEUES`, default 1), so the run was failed `worker_lost` although its work was done.
Issue #1742 asks for two things: close that missing-journal window, and make `uzi run recovery`
honest about what survives such a restart.

Two facts bound the design. First, nothing the worker can write before finalize ends proves the
run *succeeded*: the outcome is the terminal journal, and synthesizing one early would be a lie
that a completion permit would then have to trust. Second, the api already re-runs a re-queued
run through the ordinary claim path, which re-runs the executor at the next claim generation and
completes only through the completion permit (generation fence and status check). So the fix
does not have to complete anything; it only has to stop the api failing the run.

## Decision

### D1: a worker-local finalize-pending record that is not an outcome

The runner writes `finalize-<G>.json` (G = the flight's claim generation) in the run's outbox
directory (`agent/src/outbox.ts`):

- **When.** In `phasePreflightHandoff`, immediately after `executor.run` resolves, before the
  ticker stop, the running-report drain and every later finalize await. It is written only for a
  finalize-bound result: the non-terminal early returns `phasePublish` already takes (`pausedAt`,
  `completionHeld`, `walled`, `switchReleased`) are excluded through one shared predicate
  (`isFinalizeBoundResult`). A rejected `executor.run` (a disk park, a pause-now signal, an
  error) never reaches the write. The write is awaited; if it fails (outbox disabled, `ENOSPC`)
  the run logs `finalize record not written` and continues exactly as before.
- **What.** `{run_id, claim_generation, since}` only: no status, no report body, no phase. It is
  never a terminal journal and never creates a terminal lease; committed files are never read as
  completion authority.
- **MAC domain.** Its own domain label, `uzi.outbox.finalize.v1`, beside the terminal journal's, so
  a finalize record can never be replayed as a terminal journal (or any other kind) under the
  worker-local key. It reuses the outbox's temp-file write with fsync, the no-replace `link`
  install and the directory fsync.
- **Durable point.** The worker logs `finalize record durable` (with `run_id` and
  `claim_generation`) only after the temp file's fsync, the no-replace link and the directory
  fsync have all succeeded. That line, together with the file's presence, is the proof both
  restart procedures wait on; the feed's executor `result` line precedes the write and proves
  nothing.
- **Retirement while running.** The record is retired only when G's fate is durably recorded
  elsewhere: G's terminal journal is installed (the #1391 lease takes over); the terminal send
  resolved when no journal could be installed; or the api accepted a park, pause, hold or
  server-wall transition for G (limit, disk, recovery, pause and vault-lock parks, the
  completion hold, the server-side wall park). It is deliberately **not** retired on the
  graceful-shutdown branch, nor in the claim-fence, stale-claim, running-ack-terminal and
  credential-switch "released" arms: none of those is an accepted park for G, and the api acts on a
  record only for a run still running at that exact generation.
- **Boot.** `Outbox.init()` loads authenticated finalize records next to the terminal journals; an
  unauthenticated record is ignored (left on disk). `listPendingFinalizes()` excludes any run that
  also has a pending terminal journal on this worker, so the journal and its lease win. A boot
  that resolves a terminal journal also retires that run's finalize records at generations up to
  the journal's, once the journal is settled.

### D2: carry it on the register snapshot, retire after an accepted Register

The register request's `active_snapshot` gains an optional `finalize_resume:
[{run_id, claim_generation}]`, sent on the **register** snapshot only, never on a heartbeat or
claim snapshot (so `ReadoptRunsFromSnapshot` and ClaimRun's dedupe never see it). A worker with
finalize records but no pending terminal journal now also sends a register snapshot, with
`pending_overflow` false. The worker offers at most 256 entries, one per run (the highest
generation wins), oldest first; omitted records stay on disk for the next boot.

On the api, `finalize_resume` is validated **independently** of `Active`: valid uuids, generation
>= 0, no duplicate run, at most `ACTIVE_SNAPSHOT_MAX_ENTRIES` entries. A wrongly typed or invalid
list is dropped with a warning and never fails the register or discards the rest of the snapshot.
The wire shape is pinned by the shared fixture `fixtures/worker-register-snapshot/finalize-resume.json`,
read by tests on both sides.

After a register the api accepts (2xx), the worker retires exactly the offered set (and any
lower-generation record of the same offered runs). It does not re-list the directory, so a record
a live G+1 flight writes later is never touched. A failed register retires nothing. There is no
acknowledgment protocol.

### D3: the attested register pass and the one-shot resume allowance

Migration `00276_run_finalize_resume.sql` (additive) adds `runs.finalize_resume_generation BIGINT
NULL`: the exact claim generation at which the allowance below re-queued the run over budget. NULL
means never used; once set it never fires again for that run. An explicit provenance column is
used, rather than inferring "allowance spent" from `requeue_count`, for the same reason as
ADR-1390 D2's `stale_requeue_generation`: provenance must be a fact, not an inference.

In `Register`'s transaction, after the snapshot apply and before the existing orphan pass, two
queries handle the **attested** runs. A run is attested when it is owned by the registering worker
(`worker_id`), not released (`claim_released_at IS NULL`), `running`, not a chat run, listed with
a `claim_generation` **equal** to `runs.claim_generation` (exact generation, never "<="), and has
no live exact-generation `terminal_pending` lease.

- `RequeueAttestedFinalizeRuns` re-queues an attested run when it is **under budget**
  (`requeue_count < RUN_MAX_REQUEUES`; an ordinary requeue, no allowance mark), or when it is
  **over budget and the one-shot allowance is available** (`RUN_MAX_REQUEUES > 0` and
  `finalize_resume_generation IS NULL`), in which case it also sets `finalize_resume_generation =
  claim_generation`. It sets the same columns as `RequeueWorkerRuns` and never decrements
  `requeue_count`, so ADR-1390 D2's refund rule is untouched.
- `FailAttestedFinalizeRunsOverCap` fails an attested run that is over budget with the allowance
  unavailable (already used, or `RUN_MAX_REQUEUES=0`), `fail_origin = worker_lost`, exactly like
  `FailWorkerRunsOverCap`. The two queries are disjoint; failing runs first.
- Non-attested runs go through the unchanged `FailWorkerRunsOverCap` / `RequeueWorkerRuns`. The
  requeued run is re-claimed by the same worker (affinity) at G+1 by the unchanged `ClaimRun`, and
  completes only through the unchanged permit and generation fences.
- `RUN_MAX_REQUEUES=0` stays "never re-queue": the allowance is off at 0.
- The api logs `finalize_resume_offered`, `finalize_requeued`, `finalize_allowance_used` and
  `finalize_failed` on the register commit line ("worker register active snapshot committed"), and
  publishes the transitions post-commit.

**The deliberate `pending_overflow` exemption (a narrow refinement of ADR-1390 D11).** Both
attested queries ignore the worker-level `pending_overflow` closure that `FailWorkerRunsOverCap`
and `RequeueWorkerRuns` honour. The closure protects *unlisted outcomes*; an attested run has
none, because the worker lists no run that also has a pending terminal journal, and the
exact-generation lease predicate still applies. It is needed because the register's own overflow
flag (set whenever another run on the same worker has a pending terminal) would otherwise block the
fix on any multi-run worker. What still protects a claim is `ClaimRun`: it excludes every run of a
worker whose `pending_overflow_until` is in the future, so a run re-queued here cannot be
re-claimed while the closure lasts, and no second execution starts over another run's unsent
outcome (the #1393 guarantee stands).

**Why this is not a broad exemption.** It needs a worker-authenticated record for that exact run
and generation, still `running`, owned by the registering worker. It fires at most once per run and
is off at `RUN_MAX_REQUEUES=0`. It never considers eviction or infrastructure cause. A lying worker
holding its own join token gains at most one extra re-queue of a run it already owns.

### D4: recovery honesty

**(a) A finalization pin becomes an exportable archive.** The finalization pin (the committed head
H, not the early start-tip pin) now records, MAC-covered on the generation's recovery record,
`bareDir` (the bare repository's basename), `defaultBranch` and `finalizationPin: true`; the
pinned record for the generation is updated in place, and a `bundled` or `uploaded` record is never
re-pointed. Old records without the fields still authenticate.

At worker boot the worker snapshots the previous process's recovery records **before register** and
sweeps only that snapshot, so a record a live G+1 flight creates is never touched. For a
snapshotted `pinned` record with `finalizationPin`:

- the bare must resolve (basename-validated) and contain the record's authenticated `sourceSha`,
  else the record is marked `needs_action` with reason `source_not_verifiable_after_restart` and
  nothing is reserved (this reason is re-evaluated on a later boot);
- a `sourceSha` already reachable from the bare's default branch is marked
  `no_unpublished_work_after_restart`; this reading is valid only for a finalization pin, never
  for an early pin;
- otherwise the worker produces a self-contained bundle of H with **no forge fetch and no forge
  PAT**, journals it, and uploads it at the record's exact generation. The hold becomes
  `archive_ready` and `uzi run export` works without cluster access. An oversized bundle
  (`oversized`), a producer failure (`bundle_failed`) or an upload failure
  (`restart_upload_failed`, the bundle is kept for the next boot) leave the hold needing a
  decision.

The bundle size cap (64 MiB, `RECOVERY_MAX_BUNDLE_BYTES`) is enforced on the bytes git writes while
streaming, not after the fact: the git child is killed and the partial file removed as soon as the
cap is passed. Because the restart bundle is PAT-less it cannot subtract forge history, so on a
large repository it may be oversized where a forge-assisted bundle would fit; that hold then stays
`needs_action` with `oversized` and retained source custody.

A `pinned` record without `finalizationPin` (the early start-tip pin, a shutdown, pause or
restore-point pin that moved the source off the finalization head, or an older record that cannot
say) is never bundled at restart and never read as "no unpublished work": it is marked
`early_pin_only_after_restart`. These reasons live on the worker-local recovery record and its
`recovery restart sweep` log line (one per processed record, ids and outcome only); they are not
added to the server hold DTO.

This refines ADR-1296 D5; see its 2026-09-30 amendment.

**(b) The early-cut limit (no new capture path for G).** The early cut is a crash before
fetch-back and the finalization pin, with committed work only in the runner clone. Such a cut
carries a finalize record, so D3 applies, but G's own recovery record is still the early base pin.
G's own hold gets **no archive** at this cut. The existing `PendingRecoveryCaptureError`
predecessor-clone path cannot supply one: it runs only inside `phaseClone` of a *new* claim of the
same run on the same worker, and every pin, reserve and upload it makes uses that claim's
generation, G+1; a failed run is never re-claimed; and it needs the journaled clone path to still
exist. What happens in each case:

- **The run resumes** (re-queued, re-claimed at G+1 on the same worker, clone present): the
  existing path verifies the work and captures it under **G+1** (`recovery_wait` park, then
  settle). This is unchanged behaviour.
- **The run is failed instead** (over budget, allowance spent, or `RUN_MAX_REQUEUES=0`): G's hold
  reports retained **`source_only`** custody with no available archive.
- **On a Docker-lane worker** the attempt clone lives on `/data/runner`, an `emptyDir`, and does
  not survive a pod loss; there the work that existed only in the clone is gone (whatever reached
  the worker's bare tracking ref at a checkpoint remains).

**(c) No archive is stated, not implied.** The server still derives `source_only`; there is no DTO
or server change and `--json` is unchanged. `uzi run recovery` (both the owner view and the per-run
view) now prints, for a `source_only` hold, `hold <id>: no recovery archive; custody of worker
<name>'s local source is retained (export unavailable; it may be the only copy)`. It offers
`uzi run export` only for an open hold that has an available archive, and `uzi run discard` for
`source_only` and `needs_action` holds, with a warning that the retained source may be the only
copy. `landing_state` is not widened.

### D5: diagnostics

Worker log lines: `finalize record durable` (the durable-point proof), `finalize record not
written` (with a reason), `register finalize snapshot` (count and a sample of generations) and
`recovery restart sweep` (per record). Api register counts as in D3.

## Invariants future code must respect

- A finalize record is **never an outcome and never a lease**. Nothing may complete a run, create a
  terminal journal or a `terminal_pending` lease from it, or read committed files as completion
  authority. A run completes only through the completion permit at its own claim generation.
- The api may act on a finalize attestation only for a run that is owned by the registering worker,
  still `running`, unreleased, not chat, at the **exact** attested generation, with no live
  exact-generation terminal lease. Widening any of these widens what a worker token can do to a
  budget decision.
- The allowance is **one-shot per run** through `runs.finalize_resume_generation`, off at
  `RUN_MAX_REQUEUES=0`, and no path may decrement `requeue_count` for it.
- Any writer that re-queues a run must keep `ClaimRun`'s worker-level overflow exclusion as the
  gate on the re-claim.
- The record and the terminal journal are mutually exclusive on the offered list: a run with a
  pending terminal journal is never attested.
- An early recovery pin is never bundled at restart and never classified as "no unpublished work".

## Consequences and known limits

- **The residual crash window.** A crash before the record is durable (after the agent's final turn
  but before `executor.run` resolves, or during the record's own write before the link and the
  directory fsync complete) leaves no record and behaves exactly as before this change: re-queued
  under budget, failed `worker_lost` over budget. Executor success cannot be proved across that
  window and this design does not try to.
- **Ordinary-path retirement invariant.** On the ordinary path (the offered run has no pending
  terminal journal on the worker and no live exact-generation terminal lease at the api), with an
  api that has this change, no offered generation remains `running` under this worker's claim after
  the accepted Register. It is not claimed after *any* accepted Register: a live exact-generation
  lease keeps G `running` (the #1390/#1391 protection).
- **Retirement-gap known limit (no protocol added).** If the api lacks this change, or drops an
  invalid `finalize_resume` list, *and* another run's pending terminal sets `pending_overflow`, then
  G can stay `running` after an accepted Register while the worker retires G's record. G is then
  handled by today's heartbeat missing-snapshot path (`FailRunsMissingFromSnapshot` /
  `RequeueRunsMissingFromSnapshot`), which never reads the record; the record would only have
  mattered at a second restart. The api rolls out before the worker fleet, so the older-api case is
  transitional. No acknowledgment or fenced proof is added for it.
- **The live-lease plus MAC-rejected-journal mixed case** (a live exact-generation terminal lease
  meeting a journal that fails its MAC) belongs to #1974 and is unchanged here.
- **Restarts slower than the stale windows** are still failed by the sweeper before the worker can
  register; the record cannot help a worker that never comes back in time.
- **Attempt bounds.** A run that used the allowance has one more attempt than the budget: the
  honest lifetime bound of the per-attempt question cap and answer deadline becomes `x
  (RUN_MAX_REQUEUES + 2)` for that run (`x (RUN_MAX_REQUEUES + 1)` otherwise).
- **The 256-entry cap** mirrors the api's default `ACTIVE_SNAPSHOT_MAX_ENTRIES`. That limit is
  operator-configurable: an api set below the offered count drops the whole list, and the worker
  still retires the offered records after the accepted register (the retirement-gap limit above).
- **Rollout skew.** An older api ignores `finalize_resume` and follows today's path. A new worker
  now sends a register snapshot in more cases; on any api version that runs
  `ReplaceWorkerActiveRuns` in register mode, which is harmless for a freshly restarted worker.
- **The D4(b) early-cut limit** above, including the Docker-lane pod-loss case.
- **Re-execution cost.** The resumed attempt re-runs the executor, as every under-budget restart
  already does; on Docker-lane workers it runs without the earlier session, from the tracking-ref
  commits.
- **Not shipped here:** the hosted acceptance below has not been run, and the compose e2e cases in
  `e2e/phases/52-api-outage-outbox.sh` run in the nightly E2E workflow, not in the worker's own
  gate.

## Hosted acceptance

The incident happened on a hosted Kubernetes worker, so cross-process acceptance is a named,
maintainer-owned, post-merge step: **#1742 hosted-worker restart acceptance**. The worker never
runs it and nothing under `.github/workflows` is touched. Every name below is a placeholder.

### Why a CNI-native deny rule, scoped to one pod

A standard Kubernetes NetworkPolicy cannot cut one pod off from the api: its rules are additive
allow-lists, and the api's ingress policy (`deploy/chart/templates/api-networkpolicy.yaml`)
already admits the workers, so adding another NetworkPolicy can never take that access away. The
cut therefore needs a policy that can *deny*, and it must apply to **only the scratch worker pod**
so no other hosted worker or run is touched. Do not assume the cluster's CNI:

- **Where Antrea is installed** (the hosted workers' egress already uses Antrea-native policy, see
  `deploy/chart/templates/worker-fqdn-egress.yaml`), use a namespaced `crd.antrea.io`
  NetworkPolicy: `appliedTo` a `podSelector` matching only the scratch worker pod (its unique
  `uzi.dev/hosted-worker-id` label), one egress `Drop` rule to the api pods, at a priority that
  wins over the worker-egress policy (a lower number wins; the chart's default worker-egress
  policy is priority 5 in tier `application`, so pick a number below it in the same tier, or a
  tier evaluated earlier).
- **Otherwise**, the supported equivalent on a CNI that implements it: a Kubernetes
  AdminNetworkPolicy (`policy.networking.k8s.io`) with a `Deny` egress rule to the api pods, its
  `subject` a `pods` selector (namespace selector plus the same unique pod label) matching only
  the scratch worker pod, and a priority that wins. An AdminNetworkPolicy is cluster-scoped and is
  evaluated before ordinary NetworkPolicies. Check that the cluster's CNI actually implements it:
  the API server accepting the object proves nothing about enforcement.

Both objects use the exact name `uzi-1742-acceptance-<n>` and the pod-only scope.

### Preconditions

- A dedicated scratch hosted worker with persistent `/data`, serving only a scratch repository.
- The cut stops the scratch worker's heartbeats. The stale sweeper would fail an over-budget run
  after two stale windows (`WORKER_HEARTBEAT_STALE`, default 45s), so for the duration of the
  check the maintainer raises `WORKER_HEARTBEAT_STALE` on the api and restores it afterwards. This
  is a global api setting, so its restoration is part of cleanup, not an afterthought.
- Deploy the release first (api, then the worker image).

### Cleanup contract (install it first)

The order is fixed: **record the prior `WORKER_HEARTBEAT_STALE`, install the trap, then change the
setting and apply the deny rule.** The trap fires on `EXIT INT TERM`, so success, failure,
interrupt and a `set -e` abort all run it, and it does both things: deletes the exact-named deny
object **and** restores `WORKER_HEARTBEAT_STALE` to the recorded prior value. It then verifies both
restorations. Sketch (a Bash shell; `<...>` are placeholders; set `DENY_KIND` to the resource that
matches the chosen policy type, fully qualified so it cannot resolve to a standard
NetworkPolicy):

```bash
DENY_NAME='uzi-1742-acceptance-<n>'
DENY_KIND='networkpolicies.crd.antrea.io'          # Antrea; or adminnetworkpolicies.policy.networking.k8s.io
DENY_NS_ARGS=(-n '<worker-namespace>')             # empty array () for the cluster-scoped AdminNetworkPolicy
API_NS='<api-namespace>'; API_DEPLOY='<api-deployment>'

api_stale() {   # the api's configured value; empty = unset (the 45s default)
  kubectl -n "$API_NS" get deploy "$API_DEPLOY" \
    -o jsonpath='{.spec.template.spec.containers[?(@.name=="api")].env[?(@.name=="WORKER_HEARTBEAT_STALE")].value}'
}
set_stale() {   # use your deployment's own mechanism if the api env is managed elsewhere
  if [ -n "$1" ]; then kubectl -n "$API_NS" set env "deploy/$API_DEPLOY" "WORKER_HEARTBEAT_STALE=$1"
  else kubectl -n "$API_NS" set env "deploy/$API_DEPLOY" 'WORKER_HEARTBEAT_STALE-'; fi
  kubectl -n "$API_NS" rollout status "deploy/$API_DEPLOY"
}

PRIOR_STALE="$(api_stale)"                         # 1. record FIRST
echo "recorded prior WORKER_HEARTBEAT_STALE='${PRIOR_STALE}' (empty = unset)"

cleanup() {                                        # 2. install BEFORE any change
  local rc=$?; trap - EXIT INT TERM
  kubectl "${DENY_NS_ARGS[@]}" delete "$DENY_KIND" "$DENY_NAME" --ignore-not-found
  set_stale "$PRIOR_STALE"
  if kubectl "${DENY_NS_ARGS[@]}" get "$DENY_KIND" "$DENY_NAME" >/dev/null 2>&1; then
    echo "CLEANUP FAILED: $DENY_NAME still exists" >&2; rc=1; fi
  if [ "$(api_stale)" != "$PRIOR_STALE" ]; then
    echo "CLEANUP FAILED: WORKER_HEARTBEAT_STALE is not '${PRIOR_STALE}'" >&2; rc=1; fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

set_stale '<raised-value>'                         # 3. only now change the setting
kubectl "${DENY_NS_ARGS[@]}" delete "$DENY_KIND" "$DENY_NAME" --ignore-not-found   # leftover from a dead shell
```

If the shell itself dies before the trap runs, re-run step 0 of the procedure: it deletes the same
exact name and restores the setting from the value recorded on the issue. After the trap the
maintainer confirms both restorations by hand as well (the object is absent; the api's
`WORKER_HEARTBEAT_STALE` equals the recorded value). The scratch runs are cancelled or left
terminal; no other object is created.

### Procedure

Two separate scratch runs, A (completion control) and B (one-shot control). Each cut is
deterministic: the outage is applied **before** the executor hands off, and the restart happens
only after the durable point is proven.

The **durable point** has two parts, both required: the scratch worker's log shows `finalize record
durable run_id=<id> claim_generation=<G>`, **and** `kubectl exec <scratch-pod> -- test -f
/data/outbox/<id>/finalize-<G>.json` succeeds. The feed's executor `result` line is **not** a
trigger.

0. Run the cleanup contract above through its last step (record, trap, raise the stale window,
   delete any leftover object).
1. **Run A.** Start a small `task` run on the scratch repository and wait until it is `running`.
   While its executor is still working, apply the deny policy (the outage now precedes the
   hand-off, so no terminal can land). Confirm the log has **no** `finalize record durable` line
   for A at G yet. If it already does, the hand-off beat the cut: abort this attempt (the trap
   cleans up) and repeat with a new scratch run.
2. Wait for the durable point at G (both parts).
3. In the api database set A's `requeue_count = RUN_MAX_REQUEUES` (the state after an earlier
   eviction).
4. Restart only the agent process: kill it abruptly (SIGKILL, so no graceful-shutdown path runs)
   so the container restarts in place and `/data` survives; confirm the pod's container
   `restartCount` rose and the same pod kept its `/data`. Then delete the deny policy by its exact
   name so the restarted process can register.
5. Expect for A: the scratch worker's log shows `register finalize snapshot` including G; the api's
   register commit log line shows `finalize_allowance_used=1`; `runs.finalize_resume_generation =
   G`; A is re-claimed at G+1 and reaches `completed` with its merge request through the normal
   completion permit, never `failed`/`worker_lost`.
6. **Run B**, a new scratch run. Repeat steps 1 to 4 at its generation G; B is re-queued under the
   allowance.
7. When B shows `running` at G+1, re-apply the deny policy **before** the re-run's executor
   returns. Wait for the durable point at G+1 (both parts), then kill the agent process and delete
   the policy.
8. Expect for B: `failed` with `fail_origin = worker_lost`; `finalize_resume_generation` still G
   (the allowance is not reused); and `uzi run recovery <B>` shows either an `archive_ready` hold
   whose archive `uzi run export` downloads (a finalization-pinned source), or a `source_only` hold
   printed as "no recovery archive; custody ... retained (export unavailable ...)". It must never
   show a silent empty hold.
9. Optional pod-loss variant (Docker lane): delete the pod instead of killing the process at step
   4. The finalize record on `/data` still drives the allowance. For a cut before fetch-back, G's
   hold reports `source_only` (D4b).
10. Let the trap run (or run the same two deletions and restorations by hand), verify both
    restorations, and record the observed outcome on issue #1742.
