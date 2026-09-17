package workersvc

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

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

// ReplaceWorkerActiveRuns validates a worker's active-run snapshot and, when valid, applies it
// atomically inside the caller's transaction (PRD #1390 M2a). qtx MUST be a transaction-bound
// *store.Queries — the function issues several statements that are only correct together.
//
// Returns (applied, err): applied reports whether rows/leases were written. In heartbeat and
// register mode an invalid snapshot returns (false, nil) with a warning and touches nothing; in
// claim mode it returns (false, ErrActiveSnapshotInvalid). A real DB error is returned verbatim
// (a 500), never swallowed.
func (s *Service) ReplaceWorkerActiveRuns(ctx context.Context, qtx *store.Queries, wkr store.Worker, snap *ActiveSnapshot, mode snapshotMode) (bool, error) {
	if snap == nil {
		return false, nil
	}
	// invalid returns the mode-appropriate (applied, err): ignore for heartbeat/register, a
	// sentinel error for claim. The warning names the worker and the reason so an operator can
	// see a persistently-rejected snapshot without a debugger.
	invalid := func(reason string) (bool, error) {
		slog.Warn("active snapshot rejected", "worker_id", wkr.ID.String(), "mode", mode, "reason", reason)
		if mode == snapshotModeClaim {
			return false, ErrActiveSnapshotInvalid
		}
		return false, nil
	}

	// Nonce (heartbeat/claim only; register is nonce-exempt). A snapshot whose nonce is not the
	// worker's CURRENT one is discarded — this is what makes a delayed high-epoch snapshot from a
	// previous worker process (whose nonce the register rotated away) rejected across an api
	// restart, when the epoch alone could not.
	if mode != snapshotModeRegister {
		if !wkr.SnapshotRegisterNonce.Valid || snap.RegisterNonce != wkr.SnapshotRegisterNonce.String {
			return invalid("register nonce mismatch")
		}
		// Epoch must strictly advance (equal or older is a stale/duplicate capture).
		if snap.SnapshotEpoch <= wkr.SnapshotEpoch {
			return invalid("stale snapshot epoch")
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
			return invalid("entry run_id is not a uuid")
		}
		if e.ClaimGeneration < 0 {
			return invalid("entry claim_generation is negative")
		}
		if !snapshotPhases[e.Phase] {
			return invalid("entry phase is not in the allowed set")
		}
		if seen[id] {
			return invalid("duplicate run_id in snapshot")
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
	if len(entries) > s.p.ActiveSnapshotMaxEntries {
		return invalid("snapshot exceeds the absolute entry ceiling")
	}
	liveCap := s.p.ActiveSnapshotMaxEntries
	if wkr.MaxConcurrentRuns.Valid {
		liveCap = int(wkr.MaxConcurrentRuns.Int32) + 2
	}
	if liveCount > liveCap {
		return invalid("snapshot exceeds the live-entry cap")
	}
	if pendingCount > s.p.WorkerOutboxMaxPending {
		return invalid("snapshot exceeds the pending-entry cap")
	}

	// ---- Apply (validated) --------------------------------------------------
	// TerminalPendingLease is a small bounded config duration (env TERMINAL_PENDING_LEASE); its
	// whole seconds never come near the int32 range, so the truncation is safe.
	leaseSeconds := int32(s.p.TerminalPendingLease.Seconds())
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
		rows, err := qtx.UpsertWorkerActiveRun(ctx, store.UpsertWorkerActiveRunParams{
			WorkerID:        wkr.ID,
			RunID:           e.runID,
			ClaimGeneration: e.claimGeneration,
			Phase:           e.phase,
			TerminalPending: e.terminalPending,
			LeaseSeconds:    leaseSeconds,
			SnapshotEpoch:   snap.SnapshotEpoch,
		})
		if err != nil {
			return false, err
		}
		if rows == 0 {
			// The run is not owned by this worker — a worker can only describe its own runs, so
			// the entry is DROPPED (never persisted, never leased) and logged, not an error.
			slog.Warn("active snapshot entry dropped: run not owned by worker",
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
