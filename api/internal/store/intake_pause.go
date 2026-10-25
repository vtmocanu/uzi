package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// IntakePauseReasonMax bounds the admin-supplied pause reason, which is rendered to every
// user (PRD #3454). It matches the instance_intake_pause.reason CHECK.
const IntakePauseReasonMax = 280

// IntakePauseReasonText sanitizes an admin pause reason exactly as the manual cordon
// reason is: terminal sequences stripped, flattened, capped. Content is never rejected.
func IntakePauseReasonText(s string) string { return ManualCordonText(s, IntakePauseReasonMax) }

// IntakePauseState is the uncached intake-pause row. RowMissing reports a deleted
// singleton, which fn_instance_intake_paused treats as paused (fail closed); Active is
// then true and the other fields are zero.
type IntakePauseState struct {
	InstanceIntakePause
	RowMissing bool
}

// ReadInstanceIntakePause reads the singleton (named so to avoid the sqlc model type
// InstanceIntakePause) uncached, inside the caller's transaction when q
// is tx-bound. It serves routes, readouts and notices only: claims gate in SQL through
// fn_instance_intake_paused and Go never derives admission itself.
func ReadInstanceIntakePause(ctx context.Context, q *Queries) (IntakePauseState, error) {
	row, err := q.GetInstanceIntakePause(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return IntakePauseState{InstanceIntakePause: InstanceIntakePause{ID: true, Active: true}, RowMissing: true}, nil
	}
	if err != nil {
		return IntakePauseState{}, err
	}
	return IntakePauseState{InstanceIntakePause: row}, nil
}

// SetInstanceIntakePause applies a transition under the singleton's row lock. q MUST be
// bound to the caller's open transaction (store.New(tx)), never the pool: the row change
// and its audit event are two statements that only commit or roll back together inside
// that transaction, and the caller commits. changed is false for a no-op (off to off, or
// on to on with an identical reason), which writes no event. A deleted singleton is
// re-seeded inactive first, so a PUT can lift the fail-closed pause a missing row implies.
//
//	off to on: since = now, attribution = the caller, event "on"
//	on to on, different reason: reason only (since and attribution kept), event "reason"
//	  by the caller
//	on to off: reason and since cleared, event "off"
func SetInstanceIntakePause(ctx context.Context, q *Queries, active bool, reason string, byUserID pgtype.UUID, byLabel string) (state InstanceIntakePause, changed bool, err error) {
	if err := q.EnsureInstanceIntakePauseRow(ctx); err != nil {
		return InstanceIntakePause{}, false, err
	}
	cur, err := q.LockInstanceIntakePause(ctx)
	if err != nil {
		return InstanceIntakePause{}, false, err
	}
	label := pgtype.Text{String: ManualCordonText(byLabel, 200)}
	label.Valid = label.String != ""
	var newReason pgtype.Text
	if active {
		newReason.String = IntakePauseReasonText(reason)
		newReason.Valid = newReason.String != ""
	}
	if !cur.Active && !active {
		return cur, false, nil
	}
	action := "on"
	keepAttribution := false
	switch {
	case active && cur.Active:
		if cur.Reason == newReason {
			return cur, false, nil
		}
		action, keepAttribution = "reason", true
	case active:
	default:
		action = "off"
	}
	updated, err := q.UpdateInstanceIntakePause(ctx, UpdateInstanceIntakePauseParams{
		Active: active, KeepAttribution: keepAttribution, ByUserID: byUserID, ByLabel: label, Reason: newReason,
	})
	if err != nil {
		return InstanceIntakePause{}, false, err
	}
	if err := q.InsertInstanceIntakePauseEvent(ctx, InsertInstanceIntakePauseEventParams{
		ByUserID: byUserID, ByLabel: label, Action: action, Reason: newReason,
	}); err != nil {
		return InstanceIntakePause{}, false, err
	}
	return updated, true, nil
}
