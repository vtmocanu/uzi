# ADR-1296: custody holds are H-free and claim-scoped; archive captures are H-bound and immutable

**Status**: Implemented (PRD #1296, M1-M6 committed on this branch — M5, owner-facing
UX, landed in `9e3e7362`, before this ADR); M7 (integration proof and hosted
acceptance handoff) is still pending, ahead of merge/feature acceptance.
**Date**: 2026-09-13
**Issue**: [vtmocanu/uzi#1296](https://github.com/vtmocanu/uzi/issues/1296)
**PRD**: [prds/1296-durable-run-recovery.md](../prds/1296-durable-run-recovery.md) — carries
the full Decision Log (D1-D9), the milestone breakdown and the verified code anchors; this
ADR restates the invariants a future edit to a claim clause, a custody-release path or a
cleanup guard must not silently break.

## Context

A run can commit real, useful work and still fail before it publishes: a workflow or
secret preflight rejects the push, the remote rejects a non-fast-forward, or base
alignment against a moving default branch is exhausted. Before this PRD, the only
recovery paths were a mutable tracking ref inside the worker's own bare repository (gone
the moment the worker or its PVC is torn down) and, for some failures, a lossy unified
diff capped at 512 KiB with no binary content and no history. An owner without shell
access to the worker had no reliable way to get the original commits back, and an
ephemeral worker's own cleanup could destroy the only copy.

This PRD adds automatic, durable, owner-only capture of the original committed head at
the finalization boundary, stored encrypted in PostgreSQL (the only mandatory durable
store both compose and k8s already have), independent of the worker's continued
existence. It deliberately does not touch the pre-existing `recovery_wait` transient
in-place retry (ADR-1197) — that mechanism resumes a *live, still-running* turn from a
verified local checkpoint; this one preserves the *original committed history* of a run
that is finalizing (successfully or not) and may be about to terminate.

## Decision

### Two records with different lifetimes, not one

A **custody hold** (`recovery_custody_holds`) is claim-scoped and **H-free**: it is
created in the *same* successful `ClaimRun` transaction as the new
`runs.claim_generation` counter increment, for a recovery-capable worker on a
code-publishing profile, before any model work starts. It reserves owner-scoped custody
admission capacity with no dependency on crypto, byte quota, or even a captured
head existing yet.

An **archive capture** (`recovery_captures`, with its ordered `recovery_capture_chunks`)
is a later, immutable artifact bound to an *open* hold once a head H is actually
resolved. A hold's existence is never evidence that bytes are archived; only a capture's
`state='available'` with a bound manifest is. Repeated capture attempts within a hold are
distinct rows, keyed by an idempotency identity from the worker's local source journal,
so a retried registration returns the same row rather than duplicating it
(`UNIQUE (hold_id, idempotency_key)`).

Keeping these separate lets custody admission — the thing that must block new work when
an owner is holding too much unresolved recovery — be reserved cheaply and immediately,
without waiting on the worker to actually produce bytes, while the capture itself stays
free to fail, retry, or need action without ever un-reserving that admission slot.

### The two-lifetime foreign-key model

`recovery_custody_holds` carries two *different* kinds of pointer to the same worker and
run, and conflating them is the mistake this schema is built to prevent:

- **Immutable provenance** — `original_worker_id` / `original_worker_identity` are plain
  value columns, not foreign keys, and are **never nulled** after insert. They exist so a
  worker's own post-terminal recovery retry (finishing registration or upload for a hold
  it already owns) can still be authenticated after the run and even after the worker row
  is gone.
- **Live custody enforcement** — `live_worker_id` / `live_run_id` are nullable foreign
  keys, `ON DELETE RESTRICT`, non-null only while the hold is `open`. Every destructive
  cleanup path (worker deletion, ephemeral teardown, the reaper, PVC recycle) is blocked
  by these while custody is open. Releasing a hold clears **both** to `NULL` atomically in
  the same update that flips `state` to `released`/`discarded` — that clears the
  restriction and lets ordinary teardown proceed, without touching the immutable
  provenance columns beside them.

A future change must not collapse these two pairs into one FK: doing so would either let
a delete race the RESTRICT (if it were nullable-without-provenance) or block deletion
forever after a legitimate release (if it were never nullable at all).

### Nothing recovery-related cascades from owner, repo, or run

`recovery_custody_holds.user_id`/`repo_id`/`run_id` and `recovery_captures.run_id`/
`user_id` are plain columns with **no** `ON DELETE CASCADE`. A capture is also `ON DELETE
RESTRICT` from its parent hold — releasing or discarding a hold must never silently
destroy an already-registered archive. The one deliberate cascade in the schema is
`recovery_capture_chunks ON DELETE CASCADE` from its capture: a capture's own explicit
discard takes its bytes with it, but nothing upstream of a capture (owner deletion,
account deletion, run deletion) is allowed to make custody or archived bytes disappear as
a side effect. Every one of those higher-level deletes must be taught to check custody
explicitly (M4); none of them may rely on the database silently doing the right thing.

### AAD binds capture, chunk index, and length

Each ~1 MiB chunk is sealed with `secretbox.SealWithAAD`, with the additional
authenticated data built from `(capture_id, chunk_index, length)`
(`api/internal/recovery/crypto.go`, `chunkAAD`). This is not merely "encrypted at rest":
it means a stored chunk cannot be reordered, truncated, or spliced from a different
capture and still decrypt — any substitution, reorder, or length mismatch fails
authentication rather than silently producing corrupt plaintext. A future storage
migration (e.g. moving chunks to object storage) must preserve this binding, not merely
the encryption.

### Upload and ready-transition commit together, or not at all

One streaming HTTP request carries a capture's complete bundle. The API splits it into
chunks and writes every chunk plus the `available` state transition inside **one bounded
database transaction**; oversize input is rejected mid-stream (`ErrOversize`) rather than
truncated, and an interrupted or rejected upload rolls back every chunk from that attempt
while leaving the separate hold/capture reservation intact. There is no partially-written
"ready" archive: a reader either sees the fully-committed manifest and chunk set, or sees
the capture still in `preparing`/`uploading`/`needs_action`. A retry of the same capture
identity starts the byte stream from zero rather than resuming or splicing.

### Generation-aware, per-hold custody release (no cross-generation data loss)

A run can carry more than one open hold at once — a crashed worker's generation-1 claim
and a re-claiming worker's generation-2 claim both left `open`. The release path is
**per-hold, not per-run**: `ReleaseCustodyHold` releases the one hold a completed
operation actually resolved, never every open hold sharing that run id. The reconciler's
"releasable on completion" query is additionally **generation-aware**
(`r.status = 'completed' AND h.generation = r.claim_generation`), so a run's terminal
generation only ever releases its own matching hold — a still-open hold from an earlier,
orphaned generation is left alone, retained until its own capture or explicit discard.
This is the deliberately conservative, no-data-loss choice: a cross-worker orphaned hold
staying open (and thus counting against the owner's custody-admission limit and blocking
that worker's deletion) is an acceptable cost against silently dropping the last copy of
a generation's committed work. (Corrected 2026-09-13: an earlier per-run-id release
implementation did exactly the wrong thing here — releasing a completed generation-2 hold
also released an unrelated, still-uncaptured generation-1 hold on the same run. Fixed by
switching every release path — the worker's own terminal report and the boot/periodic
reconciler alike — to resolve and release one specific hold id.)

### Owner-only authorization, never the viewer seam

Every recovery-metadata and byte-serving endpoint sits under the authenticated
`RequireUser` route group and resolves ownership through `workersvc.GetRun(ctx, user.ID,
runID)` — the strict owner-only accessor, the same one private follow-up-input reads use.
None of them use `GetRunForViewer`, which is the ordinary owner-or-admin read used for run
display; an admin viewing a foreign owner's run gets exactly the same 404 an unrelated
user would. This is an application-level access rule, not an end-to-end secrecy
guarantee: see the Consequences section below.

### The checkpoint/park mechanism stays separate

`recovery_wait` (ADR-1197) and this PRD's archive capture are two different mechanisms
that must not be merged or made to depend on each other. `recovery_wait` resumes a live,
still-claimed, still-running turn from a verified local checkpoint and retained SDK
session — it exists to avoid re-planning after a transient empty SDK result.
Archive capture exists for a run that is finalizing, published or not, and whose worker
may be torn down entirely afterward; it never substitutes `checkpoint_tip` for the final
committed head, and it does not park or resume a run by itself. A future change that
wants to reuse one mechanism's plumbing for the other should treat that as a new decision,
not an implicit consequence of either existing one.

## Consequences

- **A stolen database dump or backup does not yield plaintext archives.** Every chunk is
  sealed under `UZI_SECRET_KEY`-derived AEAD with capture/chunk-bound AAD; only the API
  process holds that key, and the worker that produced the archive never receives it.
- **This is not a defense against the operator who runs the API.** Anyone who holds
  `UZI_SECRET_KEY` (the same key that already protects every other secret-at-rest kind in
  this codebase — forge PATs, the per-user vault's connection-level fallback) can decrypt
  any archive. The owner-only authorization above stops another *user* (including an
  admin using the ordinary viewer path) from reading someone else's archive; it does not
  hide archives from whoever administers the deployment. This mirrors the existing
  documented boundary in [docs/vault-threat-model.md](../docs/vault-threat-model.md) and
  is not a new or different guarantee.
- **A cross-generation orphaned hold is a visible, actionable cost, not a silent one.** It
  counts against the per-owner open-hold ceiling (default 8; see ADR-1751 for the continuation exemption) and
  keeps its worker undeleteable, surfaced as the `retaining_unpublished_work` worker flag
  and the `reasonCustodyLimit` queued-run reason. The cost clears when the hold's work is
  durably captured (which releases the hold via the reconciler) — there is no automatic
  eviction and no admin override. A capless orphan (one whose worker died before it
  reserved any capture) has **no owner self-serve discharge today**: `DiscardCaptureForOwner`
  discards a *capture*, not the hold, and there is no discard-hold action yet, so such a
  hold is retained until an operator clears it. Adding an owner-scoped discard-hold action
  (the D3 "owner explicitly confirms discard" leg for the capless case) is tracked in
  [#1318](https://github.com/vtmocanu/uzi/issues/1318).
- **Mixed fleets degrade honestly, not silently.** A worker or API that predates this
  contract simply never opens a hold, so a run executed there reports `unsupported`
  recovery rather than a promise that was never kept. Rolling the fleet forward protects
  runs going forward only; it does not retroactively protect a run that already executed
  on an old worker.
- **A schema or query change that reintroduces a per-run (rather than per-hold) release,
  or that adds `ON DELETE CASCADE` from `users`/`repos`/`runs`/`workers` onto any recovery
  table, silently reopens the exact data-loss class this ADR's decisions close.** Treat
  either change as a regression requiring the same generation-aware, RESTRICT-by-default
  review this PRD went through, not a routine schema edit.

---

## Amendment 2026-09-14 — PRD #1349: generation-exact custody, park/early-terminal capture, causal limit routing, and owner disposition

**Status**: Implemented (PRD #1349 M1-M6 committed on this branch); M7 (integration
proof and this documentation update) is in progress; M8 (hosted-k8s acceptance and
release) is maintainer-owned and **not shipped** in this work.
**Issue**: [vtmocanu/uzi#1349](https://github.com/vtmocanu/uzi/issues/1349)
**PRD**: [prds/1349-recovery-custody-hardening.md](../prds/1349-recovery-custody-hardening.md) —
carries the full Decision Log (D1-D12), the milestone breakdown and the verified code anchors.

The decisions above stand unchanged — the two-record model, the two-lifetime FK model, the
RESTRICT-by-default cascade rules, the AAD binding, and the owner-only-never-viewer
authorization all carry forward. PRD #1296 made a custody *release* generation-aware in the
reconciler but left the worker-facing reserve/release paths keyed on `(run_id, worker_id)`,
released a verified-empty hold only inside finalization capture, and gave an owner no way to
discharge a capless orphan hold (flagged above as [#1318](https://github.com/vtmocanu/uzi/issues/1318)).
Hosted acceptance on `v0.83.0-rc.1` exposed those gaps; this amendment records the decisions
that close them.

### Custody identity is the exact claim generation, end to end (D1)

Reserve, release, no-output disposition, park capture, and retry now key on
`(run_id, claim_generation)` — the identity this schema chose but did not thread through the
wire. The server already serialized `claim_generation`; the agent now echoes it in
`ClaimResponse` and stamps it on every recovery record and custody operation, and every
run-plus-worker release sweep is replaced by an exact evidence-bound operation.

**Invariant**: a newer same-worker generation can never drop an older generation's only copy.
The reconciler already had this property (see "Generation-aware, per-hold custody release"
above); this extends it to the worker's own reserve/release paths, where a completed
generation-2 claim could previously match and release a still-uncaptured generation-1 hold on
the same run and worker.

### Mixed fleets advertise v2 and retain on ambiguity (D11)

A recovery-capable worker advertises the additive `recovery_archive_v2` capability alongside
`recovery_archive_v1` (`api/internal/capability/capability.go`) and names its generation on
reserve/release. A new server accepting an older (v1) or ambiguous worker resolves custody
only where exactly one unambiguous current hold exists; anything ambiguous **retains** the
hold and returns `needs_action` rather than guessing which copy to drop. A v2 worker never
invokes v2 operations against an older server.

**Invariant**: a missing or ambiguous generation never widens back into run-plus-worker
matching. Retention — a visible, actionable cost (see PRD #1296's Consequences) — is always
preferred over a guess that could drop the last copy.

### Release only from per-hold durable evidence, ancestry-checked at disposition time (D2)

One exact hold may leave `open` only after one of: (1) that generation's full head was
published, (2) an available archive is bound to that hold, (3) the hold's own source-holding
worker proves no unpublished committed output against freshly fetched forge history, or (4) the
owner explicitly discards that exact held source. A current generation may settle an *earlier*
generation only when a settlement-time ancestry check proves the earlier source is contained in
the durable published/archived head. Tracking/checkpoint adoption supplies a candidate source,
never lasting proof — a later rebase or amend can break ancestry before publication — so the
ancestry check runs against the *final* durable head at disposition time. Same-worker identity,
the same branch, a newer MR, a later generation, or capture absence are never proof of
continuity, and a default-branch reseed can never supply the earlier source.

**Implementation note (issue #1582).** For the publication path (a completed run's
published branch head; archive-disposition settlement is not implemented), the
settlement-time ancestry check above is server-side and forge-proven: the worker only reports candidate SHAs (the predecessor's
journaled source, its adopted tip, and its own pushed head) and the api independently reads
the branch head once, then asks the forge's own comparison primitive whether each candidate
is an ancestor of it — GitHub's compare `status`, GitLab's `merge_base`, or, on Forgejo,
a two-direction compare (`head...candidate` empty AND `candidate...head` nonempty), because
Forgejo empties the commit list whenever a merge base cannot be found, so an empty
`head...candidate` alone is not proof of ancestry. Any rate limit, error, truncation, or
inconclusive answer resolves to unknown, never to a release, and the hold is retained. The
candidates are worker-reported; the api binds them to its own record only where one exists (a
capture of the hold registered before the successor claimed, or the consumed completion permit's
head on an interlocked run), so the proof establishes containment of the reported commits.

### Graceful park and early-terminal disposition (D4)

PRD #1296 released a verified-empty hold only inside finalization capture. A graceful park
(`recovery_wait`, `limit_wait`, owner pause, shutdown/drain) and an early live-worker
`failed`/`cancelled` exit both bypassed that boundary, so a provably-empty hold could stay open
forever. The agent now dispositions custody by exact generation on those paths:

- **provably empty** (a verified fresh-forge comparison shows no unpublished committed output
  for this generation) → release the exact hold before requeue;
- **committed work** → pin the exact restore-point head in the authenticated journal, produce a
  generation-bound bundle, upload through the encrypted archive service, and release only after
  the ready acknowledgement;
- **upload or unverifiable** → RETAIN the hold and local source, record `needs_action`, and
  keep retrying model-free — never fabricate a capture or release to make parking succeed.

The unconditional local pin is credential-free; the credentialed git work (the fresh forge
fetch, the bundle produce) runs inside the reap-before-credentialed-git boundary, after the
agent tree is reaped, so a shutdown/pause stays a fast pin-and-retain and the credentialed
settle never blocks the control transition.

**Invariant**: a hard termination with no usable grace window (hard node-pressure eviction,
OOM, node loss, force delete) remains outside the guarantee — such custody is retained for
owner action, never released on a guess. The archive covers only committed Git history
represented by the verified checkpoint; it is an owner recovery artifact, **not** an automatic
cross-worker resume source (cross-worker resume still adopts the best-effort
tracking/checkpoint ref, and may redo work when that ref was unavailable).

### Causal empty-turn → limit routing (D5)

ADR-1197 (referenced above) parks every positively empty SDK turn as `recovery_wait`. That is
now narrowed: a turn that is *simultaneously* positively empty (`num_turns == 0`, no model
activity, plan, question, or completion) **and** carries that attempt's final latest-wins
rate-limit verdict `status == rejected` routes to the existing usage-limit path (`limit_wait`)
instead. The already-normalized final verdict is threaded from `HarnessTerminal` into
`TurnResult`; the server stays the authority for reset/type validation, fallback scheduling,
token selection, and `wait_on_limit` (so with `wait_on_limit=false` the newly recognized limit
fails fast rather than cycling as recovery).

**Invariant**: cause is never inferred from a token label, headroom, or timing correlation. An
earlier `rejected` followed by a later `allowed`/`allowed_warning`, a nonempty success, or any
empty turn without a final same-attempt `rejected` stays on `recovery_wait`. This adds no
lifetime cap to `recovery_wait` cycles (issue #1197's retry policy is unchanged); it only
routes the causally-corroborated case.

### Owner disposition surface (D6-D10)

PRD #1296 left a capless orphan hold with no owner self-serve discharge (#1318). This
amendment ships it. Owners can:

- **list** retained holds: `GET /api/recovery/holds` returns owner-scoped rows plus an
  aggregate (`open_holds`, `custody_hold_limit`, `decision_needed`, `blocked_runs`); each row
  carries a server-derived `attention` state, deliberately distinct from its capture state, so
  a healthy active hold is not presented as an incident (D6);
- **discard** one exact held source:
  `DELETE /api/runs/{id}/recovery-holds/{holdID}?confirm=discard` — the confirmation is
  validated **before any SQL**, ownership is enforced in the mutating SQL as well as the
  handler's owner-or-404 gate, and the operation marks the hold `discarded`, nulls its live
  worker/run references, discards its non-ready (preparing/uploading/needs-action) captures,
  and never touches an available archive or a sibling hold.

CLI: `uzi run recovery <run-id>` (list) and `uzi run discard <run-id> --hold <hold-id> --yes`
(discard). Both endpoints sit under `RequireUser` (session cookie or `uzc_`/`uza_` Bearer),
preserving the owner-only-never-viewer authorization above; the owner-wide list returns only
the caller's own holds, and the run-scoped discard resolves ownership through `GetRun`, so a
foreign owner or a read-only admin acting on someone else's run gets a 404. Archive deletion
(`DELETE /api/runs/{id}/archives/{captureID}`) stays artifact cleanup: with the parent hold
still open it does not pretend to resolve custody.

**Invariant**: a discard and a racing worker upload lock the hold and its captures in the same
order, so either discard wins and retry cannot revive it, or upload wins and its available
archive survives while the exact hold settles safely.

### One coalesced owner-level Slack episode (D10)

The redundant per-run custody Slack nudge is suppressed; the admission crossing is coalesced by
owner into at most one blocked-custody episode DM (facts only — open holds, blocked runs, a web
deep link, and the exact `uzi run discard <run-id> --hold <hold-id> --yes` command), respecting
the existing health-notification enablement and cooldown. The decision-needed breakdown is
surfaced in the web board alert rather than recomputed in the notifier loop, so the DM stays a
light aggregate read (see the Decision Log). The per-run web/CLI custody pill stays as row
context. Clearing below the limit closes the episode; a later crossing can notify again.

---

## Amendment 2026-09-16 — PRD #1392: a fifth release-evidence class for a generation that never adopted a source

**Status**: Accepted (PRD #1392 M1-M4 committed on this branch).
**Issue**: [vtmocanu/uzi#1392](https://github.com/vtmocanu/uzi/issues/1392)
**PRD**: [prds/1392-forge-unreachable-preclone-park.md](../prds/1392-forge-unreachable-preclone-park.md)

D2 above (as amended by PRD #1349) permits a hold to leave `open` only on one of four kinds of
durable evidence: publication, an available archive, a fresh-forge no-output proof, or an owner
discard. All four presume the claim reached a source to publish, archive, prove empty against,
or discard — a bare, worktree, or committed head that exists, even if empty.

That presumption does not hold for a **pre-clone** forge park (ADR-1197's 2026-09-16 amendment):
when `ensureClone` itself exhausts its retries with a transient forge error, the claim's
generation never adopted any source at all — no bare, no worktree, no committed head, nothing to
fetch against and nothing to prove empty. Evidence (3), the fresh-forge no-output proof, cannot
be produced here: there is no local source to compare against fetched forge history, and running
that comparison would need the very clone the forge is currently refusing.

This amendment adds a **fifth** release-evidence class, `no_adopted_source`, for exactly this
case: a generation whose claim opened a custody hold (fact 6 in the PRD, every recovery-capable
claim on a code-publishing profile does) but never adopted a source releases that hold on
`no_adopted_source` — no forge proof required or possible, because there is no source to prove
anything against. The release happens inside the same locked transaction that parks the run
(`SetState`, cause `forge_unreachable`), settling custody and parking atomically rather than as
a best-effort follow-up call — closing the same "park after releasing cannot promise zero holds"
gap D3 in the PRD's own Decision Log names.

**Invariant, matching the existing four:** `no_adopted_source` never substitutes for
`forge_no_output` on a generation that *did* adopt a source (even an empty one) — that generation
still needs the real fresh-forge comparison. And it settles only the exact (run, worker,
generation) hold the transaction validated; anything already on disk belongs to an earlier
generation and is inventoried separately after the next successful clone, exactly as D2 already
requires for every other evidence class. See
[adr/1392-forge-unreachable-preclone-park.md](1392-forge-unreachable-preclone-park.md) for the
full decision record.
