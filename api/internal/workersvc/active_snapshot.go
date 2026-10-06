package workersvc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// ActiveSnapshot is the worker's report of the run-lane attempts it is executing (PRD #1390
// M2a, the shared wire contract with #1391). It rides every heartbeat and run-lane claim
// (stamped with the register nonce) and, for a restarting worker's pending outcomes, the
// register request (nonce-exempt, epoch 0). The api stores the latest one per worker by atomic
// full replacement, keyed by (worker, run), ordered by the worker-monotonic snapshot_epoch.
//
// The JSON field names/types are the byte-for-byte contract the agent mirrors: do not rename a
// field or change a type without changing both ends.
type ActiveSnapshot struct {
	// SnapshotEpoch is the worker-process-monotonic counter (starts at 1) that lets the api
	// order snapshots the independent heartbeat and claim loops captured, and discard a delayed
	// older one. A register-carried snapshot is 0.
	SnapshotEpoch int64 `json:"snapshot_epoch"`
	// RegisterNonce is the value the worker got from the register response. PRESENT on a
	// heartbeat/claim snapshot, ABSENT (empty) on a register-carried one (there is no nonce
	// yet — it is the one snapshot exempt from the nonce check).
	RegisterNonce string `json:"register_nonce"`
	// Active is the set of attempts the worker is executing.
	Active []ActiveRunEntry `json:"active"`
	// PendingOverflow is set when the worker has more pending outcomes than it may list; while
	// its server-side closure is unexpired every run it owns is closed to every claimant and the
	// worker itself is refused any claim (#1391, D11).
	PendingOverflow bool `json:"pending_overflow"`
	// FinalizeResume (issue #1742) is carried ONLY on a register snapshot: the attempts whose
	// executor finished and whose worker holds an authenticated finalize-pending record, but no
	// journaled terminal outcome, at the named exact claim generation. It is never an outcome and
	// never a lease; Register uses it to re-queue (never complete) the run through the ordinary
	// claim path. Omitted by an older worker, ignored by an older api (the snapshot is parsed
	// leniently).
	FinalizeResume []FinalizeResumeEntry `json:"finalize_resume,omitempty"`

	// finalizeResumeMalformed is set by UnmarshalJSON when finalize_resume had a wrong wire type
	// and was dropped. Only the register path (validateFinalizeResume) logs it, with the worker id;
	// heartbeat and claim snapshots stay silent about the field.
	finalizeResumeMalformed bool
}

// UnmarshalJSON decodes an ActiveSnapshot with FinalizeResume decoded as a SEPARATE lenient step
// (issue #1742): a finalize_resume value of the wrong wire type (a string claim_generation, a
// float, a non-string run_id, a non-array) drops only the finalize list, with a warning, and never
// fails the decode of the rest of the snapshot, so a malformed attestation cannot discard valid
// Active terminal_pending leases or pending_overflow (#1391). Every other field keeps the strict
// typed decode it had before the finalize list existed.
func (s *ActiveSnapshot) UnmarshalJSON(data []byte) error {
	// json.Unmarshaler convention: a JSON null is a no-op.
	if string(bytes.TrimSpace(data)) == "null" {
		return nil
	}
	type plain ActiveSnapshot
	aux := struct {
		*plain
		FinalizeResume json.RawMessage `json:"finalize_resume"`
	}{plain: (*plain)(s)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	s.FinalizeResume = nil
	s.finalizeResumeMalformed = false
	if len(aux.FinalizeResume) == 0 || string(aux.FinalizeResume) == "null" {
		return nil
	}
	var list []FinalizeResumeEntry
	if err := json.Unmarshal(aux.FinalizeResume, &list); err != nil {
		s.finalizeResumeMalformed = true
		return nil
	}
	s.FinalizeResume = list
	return nil
}

// FinalizeResumeEntry is one finalize-pending attempt a restarting worker attests on its register
// snapshot (issue #1742): the run and the exact claim generation whose executor finished.
type FinalizeResumeEntry struct {
	RunID           string `json:"run_id"`
	ClaimGeneration int64  `json:"claim_generation"`
}

// ActiveRunEntry is one attempt in an ActiveSnapshot. run_id is a uuid string; claim_generation
// is the generation the attempt was claimed at; phase is the status the worker sees the attempt
// in; terminal_pending is true while the attempt's outcome is journaled on the worker but not
// yet accepted by the api (#1391).
type ActiveRunEntry struct {
	RunID           string `json:"run_id"`
	ClaimGeneration int64  `json:"claim_generation"`
	Phase           string `json:"phase"`
	TerminalPending bool   `json:"terminal_pending"`
}

// snapshotPhases is the closed set an ActiveRunEntry.Phase must belong to — the run lane's four
// live phases (judge/review attempts are listed as 'running'). Mirrors the CHECK constraint on
// worker_active_runs.phase (migration 00236).
var snapshotPhases = map[string]bool{
	"running":           true,
	"awaiting_approval": true,
	"awaiting_input":    true,
	"awaiting_followup": true,
}

// snapshotMode selects how ReplaceWorkerActiveRuns treats an invalid snapshot and how it
// deletes prior rows (PRD #1390 M2a).
type snapshotMode int

const (
	// snapshotModeHeartbeat: full replacement, nonce+epoch checked, invalid ⇒ ignored (rows
	// and leases left exactly as they were, a warning logged, no error) so a heartbeat is never
	// turned into a 400.
	snapshotModeHeartbeat snapshotMode = iota
	// snapshotModeClaim: full replacement, nonce+epoch checked, invalid ⇒ ErrActiveSnapshotInvalid
	// so the caller fails the claim closed (M3 uses this).
	snapshotModeClaim
	// snapshotModeRegister: nonce-exempt (no nonce exists yet) and no epoch gate (the register
	// just reset the epoch under a fresh nonce). Deletes only ordinary rows absent from the
	// snapshot and PRESERVES leased rows, so #1391's pending outcomes survive the restart.
	snapshotModeRegister
)

// ErrActiveSnapshotInvalid is the sentinel a claim-mode snapshot validation failure returns, so
// the claim caller fails closed (400, no claim, no side effect) — PRD #1390 M2a / M3.
var ErrActiveSnapshotInvalid = errors.New("active run snapshot failed validation")

// mintSnapshotNonce returns a fresh per-registration nonce (256 bits, base64url) every snapshot
// must echo (PRD #1390 M2a, D3). Reuses the jointoken minting shape (crypto/rand → RawURLEncoding);
// it is not a credential, so it carries no prefix and is stored in cleartext on the worker row.
func mintSnapshotNonce() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("mint snapshot nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// validatedEntry is a shape-validated, parsed ActiveRunEntry ready to persist.
type validatedEntry struct {
	runID           uuid.UUID
	claimGeneration int64
	phase           string
	terminalPending bool
}

// validatedFinalizeResume is the shape-validated form of ActiveSnapshot.FinalizeResume: two
// parallel slices (the attested pair i is ids[i], generations[i]) ready to pass to the attested
// finalize queries. Run ids are unique.
type validatedFinalizeResume struct {
	ids         []uuid.UUID
	generations []int64
}

// validateFinalizeResume validates ActiveSnapshot.FinalizeResume INDEPENDENTLY of Active (issue
// #1742): a bad finalize list never drops a valid Active list and vice versa. It returns ok=false
// for an absent list, and for an invalid one (a non-uuid run_id, a negative generation, a
// duplicate run_id, or more than ActiveSnapshotMaxEntries entries) after a warning: the list is
// then ignored and Register carries on, never failing. Only the register path calls it; the
// heartbeat and claim snapshots never read the field. A wrongly TYPED list never reaches here:
// ActiveSnapshot.UnmarshalJSON already dropped it without failing the rest of the snapshot.
func (s *Service) validateFinalizeResume(wkr store.Worker, snap *ActiveSnapshot) (validatedFinalizeResume, bool) {
	var out validatedFinalizeResume
	if snap != nil && snap.finalizeResumeMalformed {
		slog.Warn("finalize_resume has an invalid wire shape; ignoring the list and keeping the rest of the snapshot",
			"worker_id", wkr.ID.String())
		return out, false
	}
	if snap == nil || len(snap.FinalizeResume) == 0 {
		return out, false
	}
	reject := func(reason string) (validatedFinalizeResume, bool) {
		slog.Warn("finalize resume list rejected; ignoring it", "worker_id", wkr.ID.String(), "reason", reason)
		return validatedFinalizeResume{}, false
	}
	if len(snap.FinalizeResume) > s.p.ActiveSnapshotMaxEntries {
		return reject("list exceeds the absolute entry ceiling")
	}
	seen := make(map[uuid.UUID]bool, len(snap.FinalizeResume))
	for _, e := range snap.FinalizeResume {
		id, err := uuid.Parse(e.RunID)
		if err != nil {
			return reject("entry run_id is not a uuid")
		}
		if e.ClaimGeneration < 0 {
			return reject("entry claim_generation is negative")
		}
		if seen[id] {
			return reject("duplicate run_id in list")
		}
		seen[id] = true
		out.ids = append(out.ids, id)
		out.generations = append(out.generations, e.ClaimGeneration)
	}
	return out, true
}

// normalizeActiveSnapshot only validates wire input; it performs no database reads or writes.
func normalizeActiveSnapshot(p Params, wkr store.Worker, snap *ActiveSnapshot, mode snapshotMode) ([]validatedEntry, string) {
	if snap == nil {
		return nil, ""
	}
	// Nonce (heartbeat/claim only; register is nonce-exempt). A snapshot whose nonce is not the
	// worker's CURRENT one is discarded — this is what makes a delayed high-epoch snapshot from a
	// previous worker process (whose nonce the register rotated away) rejected across an api
	// restart, when the epoch alone could not.
	if mode != snapshotModeRegister {
		if !wkr.SnapshotRegisterNonce.Valid || snap.RegisterNonce != wkr.SnapshotRegisterNonce.String {
			return nil, "register nonce mismatch"
		}
		// Epoch must strictly advance (equal or older is a stale/duplicate capture).
		if snap.SnapshotEpoch <= wkr.SnapshotEpoch {
			return nil, "stale snapshot epoch"
		}
	}

	// Shape-validate every entry. A single bad entry rejects the WHOLE snapshot (it is not a
	// trustworthy report) — distinct from an UNOWNED entry, which is dropped per-entry below.
	entries := make([]validatedEntry, 0, len(snap.Active))
	seen := make(map[uuid.UUID]bool, len(snap.Active))
	var liveCount, pendingCount int
	for _, e := range snap.Active {
		id, perr := uuid.Parse(e.RunID)
		if perr != nil {
			return nil, "entry run_id is not a uuid"
		}
		if e.ClaimGeneration < 0 {
			return nil, "entry claim_generation is negative"
		}
		if !snapshotPhases[e.Phase] {
			return nil, "entry phase is not in the allowed set"
		}
		if seen[id] {
			return nil, "duplicate run_id in snapshot"
		}
		seen[id] = true
		if e.TerminalPending {
			pendingCount++
		} else {
			liveCount++
		}
		entries = append(entries, validatedEntry{
			runID:           id,
			claimGeneration: e.ClaimGeneration,
			phase:           e.Phase,
			terminalPending: e.TerminalPending,
		})
	}

	// Caps: total under the absolute server ceiling; live under max_concurrent_runs + 2 (live
	// slots plus judge/review headroom); pending under the outbox quota. A worker that never
	// advertised its concurrency cap has no per-type live cap — the total ceiling still bounds it.
	if len(entries) > p.ActiveSnapshotMaxEntries {
		return nil, "snapshot exceeds the absolute entry ceiling"
	}
	liveCap := p.ActiveSnapshotMaxEntries
	if wkr.MaxConcurrentRuns.Valid {
		liveCap = int(wkr.MaxConcurrentRuns.Int32) + 2
	}
	if liveCount > liveCap {
		return nil, "snapshot exceeds the live-entry cap"
	}
	if pendingCount > p.WorkerOutboxMaxPending {
		return nil, "snapshot exceeds the pending-entry cap"
	}

	return entries, ""
}

func (s *Service) validatedActiveSnapshot(wkr store.Worker, snap *ActiveSnapshot, mode snapshotMode) ([]validatedEntry, bool, error) {
	entries, reason := normalizeActiveSnapshot(s.p, wkr, snap, mode)
	if reason != "" {
		slog.Warn("active snapshot rejected", "worker_id", wkr.ID.String(), "mode", mode, "reason", reason)
		if mode == snapshotModeClaim {
			return nil, false, ErrActiveSnapshotInvalid
		}
		return nil, false, nil
	}
	return entries, snap != nil, nil
}

// ReplaceWorkerActiveRuns validates a worker's active-run snapshot and, when valid, applies it
// atomically inside the caller's transaction (PRD #1390 M2a). qtx MUST be a transaction-bound
// *store.Queries — the function issues several statements that are only correct together.
//
// Returns (applied, err): applied reports whether rows/leases were written. In heartbeat and
// register mode an invalid snapshot returns (false, nil) with a warning and touches nothing; in
// claim mode it returns (false, ErrActiveSnapshotInvalid). A real DB error is returned verbatim
// (a 500), never swallowed.
// ReplaceWorkerActiveRuns is the compatibility entrypoint for a provided transaction.
// It acquires the worker row itself and uses the same frozen ledger as production callers.
func (s *Service) ReplaceWorkerActiveRuns(ctx context.Context, qtx *store.Queries, wkr store.Worker, snap *ActiveSnapshot, mode snapshotMode) (bool, error) {
	if snap == nil {
		return false, nil
	}
	locked, err := qtx.GetWorkerForUpdate(ctx, wkr.ID)
	if err != nil {
		return false, err
	}
	entries, valid, err := s.validatedActiveSnapshot(locked, snap, mode)
	if err != nil || !valid {
		return false, err
	}
	locks, err := captureWorkerRecoveryLocks(ctx, qtx, locked.ID, entries, validatedFinalizeResume{})
	if err != nil {
		return false, err
	}
	return s.applyWorkerActiveSnapshot(ctx, qtx, locked, snap, mode, entries, locks)
}

func (s *Service) applyWorkerActiveSnapshot(ctx context.Context, qtx *store.Queries, wkr store.Worker, snap *ActiveSnapshot, mode snapshotMode, entries []validatedEntry, locks workerRecoveryLockSet) (bool, error) {
	frozen, parents, err := locks.parameters()
	if err != nil {
		return false, err
	}
	if _, err := qtx.LockFrozenWorkerSnapshotRuns(ctx, store.LockFrozenWorkerSnapshotRunsParams{
		WorkerID: pgconv.UUID(wkr.ID), RunIds: validatedSnapshotIDs(entries), FrozenTargets: frozen, LockedParentIds: parents,
	}); err != nil {
		return false, err
	}
	// ---- Apply (validated) --------------------------------------------------
	// TerminalPendingLease is a small bounded config duration (env TERMINAL_PENDING_LEASE); its
	// whole seconds never come near the int32 range, so the truncation is safe.
	leaseSeconds := int32(s.p.TerminalPendingLease.Seconds())

	// Issue #1994: terminal_pending_since is the first-seen time of a pending entry. Read the
	// prior pending rows BEFORE any delete so the value can be carried forward. The rule: an
	// entry keeps the prior since only when it is pending now AND the prior row was pending at
	// the SAME claim generation with a non-NULL since; otherwise it is stamped fresh (now() when
	// pending, NULL when live). So since is preserved across heartbeat renewals and a register,
	// and reset when the entry clears, is omitted, or the generation changes. All three callers
	// lock the worker row first, so this read-then-write cannot race a sibling snapshot.
	//
	// KNOWN LIMIT: under pending_overflow the worker (agent/src/active-run-registry.ts,
	// selectPendingForBuild) lists blocked-first fixed slots plus ONE slot that round-robins over
	// the omitted pending entries. In heartbeat/claim mode an omitted entry's row is deleted by
	// DeleteWorkerActiveRuns below, so its since resets each time it rotates back in and those
	// runs never reach the outcome-undelivered health threshold. The worker-level pending_overflow lease still
	// protects them from claim and reconcile.
	type priorPending struct {
		gen   int64
		since pgtype.Timestamptz
	}
	priorRows, err := qtx.ListWorkerPendingSince(ctx, wkr.ID)
	if err != nil {
		return false, err
	}
	prior := make(map[uuid.UUID]priorPending, len(priorRows))
	for _, r := range priorRows {
		prior[r.RunID] = priorPending{gen: r.ClaimGeneration, since: r.TerminalPendingSince}
	}

	if mode == snapshotModeRegister {
		// Preserve leased rows; drop only ordinary rows the snapshot no longer lists.
		keep := make([]uuid.UUID, 0, len(entries))
		for _, e := range entries {
			keep = append(keep, e.runID)
		}
		if _, err := qtx.DeleteOrdinaryWorkerActiveRunsNotIn(ctx, store.DeleteOrdinaryWorkerActiveRunsNotInParams{
			WorkerID:   wkr.ID,
			KeepRunIds: keep,
		}); err != nil {
			return false, err
		}
	} else if _, err := qtx.DeleteWorkerActiveRuns(ctx, wkr.ID); err != nil {
		return false, err
	}

	for _, e := range entries {
		var pendingSince pgtype.Timestamptz
		if p, ok := prior[e.runID]; ok && e.terminalPending && p.gen == e.claimGeneration && p.since.Valid {
			pendingSince = p.since
		}
		rows, err := qtx.UpsertFrozenWorkerActiveRun(ctx, store.UpsertFrozenWorkerActiveRunParams{
			FrozenTargets: frozen, LockedParentIds: parents,
			WorkerID:        wkr.ID,
			RunID:           e.runID,
			ClaimGeneration: e.claimGeneration,
			Phase:           e.phase,
			TerminalPending: e.terminalPending,
			LeaseSeconds:    leaseSeconds,
			PendingSince:    pendingSince,
			SnapshotEpoch:   snap.SnapshotEpoch,
		})
		if err != nil {
			return false, err
		}
		if rows == 0 {
			// UpsertFrozenWorkerActiveRun rejected ownership or frozen identity drift.
			// The entry is not persisted or leased; log without further discovery.
			slog.Warn("active snapshot entry dropped: ownership or frozen identity changed",
				"worker_id", wkr.ID.String(), "run_id", e.runID.String())
		}
	}

	if _, err := qtx.SetWorkerSnapshotState(ctx, store.SetWorkerSnapshotStateParams{
		SnapshotEpoch:   snap.SnapshotEpoch,
		PendingOverflow: snap.PendingOverflow,
		LeaseSeconds:    leaseSeconds,
		ID:              wkr.ID,
	}); err != nil {
		return false, err
	}
	return true, nil
}
