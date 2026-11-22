package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Phases passed to Service.terminalStopFault (a test seam). attempt 0 is the first, in-line
// attempt of ResolvePublicationContext; reconciliation attempts are numbered from 1.
const (
	// terminalStopFaultStamp fails the stamp statement itself (a store error before any write).
	terminalStopFaultStamp = "stamp"
	// terminalStopFaultBeforeCommit rolls the transaction back and reports a commit failure:
	// a definite rollback.
	terminalStopFaultBeforeCommit = "before_commit"
	// terminalStopFaultAfterCommit commits and then reports a commit failure: a lost response.
	terminalStopFaultAfterCommit = "after_commit"
	// terminalStopFaultReconcileBegin runs at the start of each reconciliation attempt, before
	// its transaction opens (no lock is held), so a test can land a concurrent write there.
	terminalStopFaultReconcileBegin = "reconcile_begin"
)

// terminalStopReconcileBackoff is the wait before reconciliation attempts 2 and 3.
var terminalStopReconcileBackoff = []time.Duration{50 * time.Millisecond, 150 * time.Millisecond}

const terminalStopReconcileAttempts = 3

func (s *Service) terminalFault(phase string, attempt int) error {
	if s.terminalStopFault == nil {
		return nil
	}
	return s.terminalStopFault(phase, attempt)
}

// stopTerminalPublication runs inside ResolvePublicationContext's fenced transaction, after the
// worker/run locks and the unchanged-authority recheck, when the bound MR is merged or closed
// (issue #4616). It stamps the run's own stop (no cancel input: the run is mid-publication and
// reads stop_kind itself) and answers target_terminal. A guard miss (an existing stop_kind of
// any kind, a changed status) stamps nothing and answers forge_mismatch, the pre-existing
// refusal. ctx is the caller's context and txCtx the transaction's bounded one: reconciliation
// derives from ctx so an expired txCtx cannot starve it. An ambiguous store or commit failure
// is reconciled in a fresh transaction so a
// committed stamp is reported as such instead of being retried into authority_unknown.
func (s *Service) stopTerminalPublication(ctx, txCtx context.Context, tx pgx.Tx, qt *store.Queries, w store.Worker, id uuid.UUID,
	req PublicationContextRequest, params store.PublicationContextSnapshotParams, before []byte, reason string) PublicationContextResponse {
	var rows int64
	err := s.terminalFault(terminalStopFaultStamp, 0)
	if err == nil {
		rows, err = qt.StampTerminalTargetStop(txCtx, store.StampTerminalTargetStopParams{
			StopReason: reason, ID: id, WorkerID: pgconv.UUID(w.ID), ClaimGeneration: req.ClaimGeneration,
		})
	}
	if err != nil {
		_ = tx.Rollback(txCtx)
		return s.reconcileTerminalStop(ctx, w, id, req, params, before, reason)
	}
	if rows == 0 {
		return publicationUnknown("forge_mismatch")
	}
	if err := s.terminalFault(terminalStopFaultBeforeCommit, 0); err != nil {
		_ = tx.Rollback(txCtx)
		return s.reconcileTerminalStop(ctx, w, id, req, params, before, reason)
	}
	if err := tx.Commit(txCtx); err != nil {
		return s.reconcileTerminalStop(ctx, w, id, req, params, before, reason)
	}
	if err := s.terminalFault(terminalStopFaultAfterCommit, 0); err != nil {
		return s.reconcileTerminalStop(ctx, w, id, req, params, before, reason)
	}
	return publicationUnknown("target_terminal")
}

type reconcileOutcome int

const (
	reconcileRetry reconcileOutcome = iota
	reconcileDone
)

// reconcileTerminalStop resolves an ambiguous first attempt. Up to terminalStopReconcileAttempts
// fresh fenced transactions re-lock worker then run, re-run the claim checks and compare the
// authority snapshot SEMANTICALLY with the one the answer was based on: the only difference
// tolerated is this stop's own stamp (stop_kind cancelled with exactly the server reason on a run
// that had none). A stamp already present with the server reason answers target_terminal without a
// write; an absent stamp is applied; any other stop_kind or reason refuses without writing.
// Exhausting the attempts on store errors answers authority_unknown. The whole loop stays within
// completedPublicationDeadline.
func (s *Service) reconcileTerminalStop(ctx context.Context, w store.Worker, id uuid.UUID, req PublicationContextRequest,
	params store.PublicationContextSnapshotParams, before []byte, reason string) PublicationContextResponse {
	recCtx, cancel := context.WithTimeout(ctx, completedPublicationDeadline)
	defer cancel()
	for attempt := 1; attempt <= terminalStopReconcileAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-time.After(terminalStopReconcileBackoff[attempt-2]):
			case <-recCtx.Done():
				return publicationUnknown("authority_unknown")
			}
		}
		resp, outcome, err := s.reconcileTerminalStopOnce(recCtx, w, id, req, params, before, reason, attempt)
		if err == nil && outcome == reconcileDone {
			return resp
		}
		slog.Warn("publication context: terminal stop reconciliation attempt failed", "event", "publication_terminal_stop_reconcile",
			"run", id.String(), "attempt", attempt, "error", err)
	}
	return publicationUnknown("authority_unknown")
}

func (s *Service) reconcileTerminalStopOnce(ctx context.Context, w store.Worker, id uuid.UUID, req PublicationContextRequest,
	params store.PublicationContextSnapshotParams, before []byte, reason string, attempt int) (PublicationContextResponse, reconcileOutcome, error) {
	if err := s.terminalFault(terminalStopFaultReconcileBegin, attempt); err != nil {
		return PublicationContextResponse{}, reconcileRetry, err
	}
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return PublicationContextResponse{}, reconcileRetry, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qt := store.New(tx)
	worker, err := qt.GetWorkerForUpdate(ctx, w.ID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return PublicationContextResponse{}, reconcileRetry, err
		}
		return publicationUnknown("claim_changed"), reconcileDone, nil
	}
	if worker.UserID != w.UserID {
		return publicationUnknown("claim_changed"), reconcileDone, nil
	}
	run, err := qt.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: id, WorkerID: pgconv.UUID(w.ID)})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return PublicationContextResponse{}, reconcileRetry, err
		}
		return publicationUnknown("claim_changed"), reconcileDone, nil
	}
	if run.UserID != w.UserID || run.ClaimGeneration <= 0 || run.ClaimGeneration != req.ClaimGeneration ||
		run.ClaimReleasedAt.Valid || (run.Status != "claimed" && run.Status != "running") {
		return publicationUnknown("claim_changed"), reconcileDone, nil
	}
	after, err := readPublicationSnapshot(ctx, qt, params, "reconcile")
	if err != nil {
		return PublicationContextResponse{}, reconcileRetry, err
	}
	stamped := run.StopKind.Valid && run.StopKind.String == "cancelled" && run.StopReason.Valid && run.StopReason.String == reason
	if run.StopKind.Valid && !stamped {
		// An operator or earlier verdict owns the run: never overwritten, and answered before the
		// snapshot compare so a foreign stamp is forge_mismatch, not authority_changed.
		return publicationUnknown("forge_mismatch"), reconcileDone, nil
	}
	same, err := publicationSnapshotsEqualModuloStop(before, after, id, stamped)
	if err != nil {
		return publicationUnknown("authority_unknown"), reconcileDone, nil
	}
	if !same {
		return publicationUnknown("authority_changed"), reconcileDone, nil
	}
	if stamped {
		return publicationUnknown("target_terminal"), reconcileDone, nil
	}
	if err := s.terminalFault(terminalStopFaultStamp, attempt); err != nil {
		return PublicationContextResponse{}, reconcileRetry, err
	}
	rows, err := qt.StampTerminalTargetStop(ctx, store.StampTerminalTargetStopParams{
		StopReason: reason, ID: id, WorkerID: pgconv.UUID(w.ID), ClaimGeneration: req.ClaimGeneration,
	})
	if err != nil {
		return PublicationContextResponse{}, reconcileRetry, err
	}
	if rows == 0 {
		return publicationUnknown("forge_mismatch"), reconcileDone, nil
	}
	if err := s.terminalFault(terminalStopFaultBeforeCommit, attempt); err != nil {
		return PublicationContextResponse{}, reconcileRetry, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PublicationContextResponse{}, reconcileRetry, err
	}
	if err := s.terminalFault(terminalStopFaultAfterCommit, attempt); err != nil {
		// The commit landed; the next attempt reads it back as a stamp.
		return PublicationContextResponse{}, reconcileRetry, err
	}
	return publicationUnknown("target_terminal"), reconcileDone, nil
}

// publicationSnapshotsEqualModuloStop compares two authority snapshots semantically. When
// stamped is true, a stop_kind of cancelled in after (on the run id, as request or as a node)
// that before did not carry is this stop's own stamp and is normalized away; nothing else is.
func publicationSnapshotsEqualModuloStop(before, after []byte, id uuid.UUID, stamped bool) (bool, error) {
	var b, a publicationSnapshot
	if err := json.Unmarshal(before, &b); err != nil {
		return false, err
	}
	if err := json.Unmarshal(after, &a); err != nil {
		return false, err
	}
	normalize := func(was, now *publicationAuthority) {
		if stamped && was.StopKind == nil && now.StopKind != nil && *now.StopKind == "cancelled" {
			now.StopKind = nil
		}
	}
	normalize(&b.Request, &a.Request)
	if len(b.Nodes) == len(a.Nodes) {
		for i := range a.Nodes {
			if a.Nodes[i].Run.ID == id && b.Nodes[i].Run.ID == id {
				normalize(&b.Nodes[i].Run, &a.Nodes[i].Run)
			}
		}
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	ab, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	return bytes.Equal(bb, ab), nil
}
