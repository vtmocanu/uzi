package store

import (
	"context"
	"errors"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

var ErrManualCordonEphemeral = errors.New("manual cordon is only supported for persistent workers")

// WorkerAcceptsNewIntake mirrors fn_worker_accepts_new_intake for locked service reads.
// Ephemeral binding and continuation exceptions belong to each intake caller.
func WorkerAcceptsNewIntake(w Worker) bool {
	return !w.ManualCordonedAt.Valid && !w.DrainingSince.Valid && !w.MaintenanceFenced &&
		w.MaintenancePhase != "requested" && w.MaintenancePhase != "ready" &&
		w.MaintenancePhase != "stopping" && w.MaintenancePhase != "recycling"
}

type cordonTxBeginner interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}

// ChangeWorkerManualCordon serializes with intake using the worker-first lock order.
// Actor is the authenticated owner; callers must never source it from request JSON.
// One transition and its audit event commit together. A commit error is an error,
// including when the server cannot establish whether the commit was acknowledged.
func ChangeWorkerManualCordon(ctx context.Context, db cordonTxBeginner, ownerID, workerID uuid.UUID, displayName string, reason pgtype.Text, cordon bool) (Worker, error) {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Worker{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := New(tx)
	locked, err := q.GetWorkerForOwnerForUpdate(ctx, GetWorkerForOwnerForUpdateParams{WorkerID: workerID, OwnerID: ownerID})
	if err != nil {
		return Worker{}, err
	}
	// Ownership precedes kind validation so a foreign ephemeral worker is a 404.
	if locked.Ephemeral {
		return Worker{}, ErrManualCordonEphemeral
	}
	if locked.Kind != "hosted" && locked.Kind != "external" {
		return Worker{}, pgx.ErrNoRows
	}
	current, err := q.GetWorkerByIDForUser(ctx, GetWorkerByIDForUserParams{ID: workerID, UserID: ownerID})
	if err != nil {
		return Worker{}, err
	}
	if current.ManualCordonedAt.Valid == cordon {
		if err := tx.Commit(ctx); err != nil {
			return Worker{}, err
		}
		return current, nil
	}
	label := ManualCordonText(displayName, 200)
	if label == "" {
		label = ownerID.String()
	}
	reason.String = ManualCordonText(reason.String, 500)
	reason.Valid = reason.Valid && reason.String != ""
	action := "uncordon"
	if cordon {
		action = "cordon"
		current, err = q.CordonWorkerForOwner(ctx, CordonWorkerForOwnerParams{
			WorkerID: workerID, OwnerID: ownerID, ActorID: ownerID, ActorLabel: label, Reason: reason,
		})
	} else {
		reason = current.ManualCordonReason
		current, err = q.UncordonWorkerForOwner(ctx, UncordonWorkerForOwnerParams{WorkerID: workerID, OwnerID: ownerID})
	}
	if err != nil {
		return Worker{}, err
	}
	if err := q.InsertWorkerCordonEvent(ctx, InsertWorkerCordonEventParams{
		OwnerID: ownerID, WorkerID: workerID, ActorID: ownerID, ActorLabel: label, Action: action, Reason: reason,
	}); err != nil {
		return Worker{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Worker{}, err
	}
	return current, nil
}

// ManualCordonText strips terminal sequences before flattening metadata and caps codepoints.
// This is specific to manual metadata; general terminal cell semantics stay unchanged.
func ManualCordonText(value string, limit int) string {
	value = strings.TrimSpace(termsafe.CellText(ansi.Strip(value)))
	runes := []rune(value)
	return strings.TrimSpace(string(runes[:min(len(runes), limit)]))
}
