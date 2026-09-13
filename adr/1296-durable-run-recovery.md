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
  counts against `CountUnresolvedCustodyHoldsForOwner`'s per-owner ceiling (default 8) and
  keeps its worker undeleteable, surfaced as the `retaining_unpublished_work` worker flag
  and the `reasonCustodyLimit` queued-run reason. Only the owner can recover or
  explicitly discard it to clear either effect — there is no automatic eviction and no
  admin override.
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
