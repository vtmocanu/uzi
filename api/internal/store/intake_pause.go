package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
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
// re-seeded inactive first, so a PUT can lift the fail-closed pause a missing row implies;
// lifting a re-seeded row is a change (paused to unpaused) and writes an "off" event.
//
//	off to on: since = now, attribution = the caller, event "on"
//	on to on, different reason: reason only (since and attribution kept), event "reason"
//	  by the caller
//	on to off: reason and since cleared, event "off"
func SetInstanceIntakePause(ctx context.Context, q *Queries, active bool, reason string, byUserID pgtype.UUID, byLabel string) (state InstanceIntakePause, changed bool, err error) {
	reseeded, err := q.EnsureInstanceIntakePauseRow(ctx)
	if err != nil {
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
		if reseeded == 0 {
			return cur, false, nil
		}
		// The singleton was missing (effectively paused) and this lifts it: the effective
		// state changed, so audit it as an "off" by the caller.
		if err := q.InsertInstanceIntakePauseEvent(ctx, InsertInstanceIntakePauseEventParams{
			ByUserID: byUserID, ByLabel: label, Action: "off",
		}); err != nil {
			return InstanceIntakePause{}, false, err
		}
		return cur, true, nil
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

// MaintenanceItem is one run row of the maintenance readout. Tag is "progressing" or
// "needs_owner" for held and custody rows, nil otherwise; HoldID is set for custody rows only.
type MaintenanceItem struct {
	RunID      uuid.UUID
	Kind       string
	Status     string
	OwnerLabel string
	Repo       *string
	Since      *time.Time
	Tag        *string
	HoldID     *uuid.UUID
}

// MaintenanceSection is an exact Count plus the oldest (at most maintenanceListLimit) Items.
type MaintenanceSection struct {
	Count int64
	Items []MaintenanceItem
}

// MaintenanceWorker is an ephemeral worker bound to a queued run the pause will not admit.
type MaintenanceWorker struct {
	WorkerID   uuid.UUID
	OwnerLabel string
	RunID      uuid.UUID
	Since      *time.Time
}

// MaintenanceWorkers is the idle-bound-worker section.
type MaintenanceWorkers struct {
	Count int64
	Items []MaintenanceWorker
}

// MaintenanceReadout is the snapshot-consistent drain readout behind GET /api/admin/maintenance.
type MaintenanceReadout struct {
	AsOf             time.Time
	Active           MaintenanceSection
	Held             MaintenanceSection
	Custody          MaintenanceSection
	Queued           MaintenanceSection
	IdleBoundWorkers MaintenanceWorkers
}

// Clear reports that nothing is in flight, parked or held in custody: queued work and idle
// bound workers are informational and do not count.
func (m MaintenanceReadout) Clear() bool {
	return m.Active.Count == 0 && m.Held.Count == 0 && m.Custody.Count == 0
}

func tsPtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

func textPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	v := t.String
	return &v
}

// ReadMaintenanceReadout reads every section in one REPEATABLE READ READ ONLY transaction so
// counts and lists share a snapshot; AsOf is that transaction's now().
func ReadMaintenanceReadout(ctx context.Context, db cordonTxBeginner) (MaintenanceReadout, error) {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return MaintenanceReadout{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := New(tx)
	var out MaintenanceReadout
	asOf, err := q.MaintenanceSnapshotNow(ctx)
	if err != nil {
		return MaintenanceReadout{}, err
	}
	out.AsOf = asOf.Time
	out.Active.Items = []MaintenanceItem{}
	out.Held.Items = []MaintenanceItem{}
	out.Custody.Items = []MaintenanceItem{}
	out.Queued.Items = []MaintenanceItem{}
	out.IdleBoundWorkers.Items = []MaintenanceWorker{}

	active, err := q.ListMaintenanceActiveRuns(ctx)
	if err != nil {
		return MaintenanceReadout{}, err
	}
	for _, r := range active {
		out.Active.Count = r.Total
		out.Active.Items = append(out.Active.Items, MaintenanceItem{RunID: r.RunID, Kind: r.Kind, Status: r.Status,
			OwnerLabel: r.OwnerLabel, Repo: textPtr(r.Repo), Since: tsPtr(r.Since)})
	}
	held, err := q.ListMaintenanceHeldRuns(ctx)
	if err != nil {
		return MaintenanceReadout{}, err
	}
	for _, r := range held {
		out.Held.Count = r.Total
		tag := r.Tag
		out.Held.Items = append(out.Held.Items, MaintenanceItem{RunID: r.RunID, Kind: r.Kind, Status: r.Status,
			OwnerLabel: r.OwnerLabel, Repo: textPtr(r.Repo), Since: tsPtr(r.Since), Tag: &tag})
	}
	custody, err := q.ListMaintenanceCustodyRuns(ctx)
	if err != nil {
		return MaintenanceReadout{}, err
	}
	for _, r := range custody {
		out.Custody.Count = r.Total
		tag, hold := r.Tag, r.HoldID
		out.Custody.Items = append(out.Custody.Items, MaintenanceItem{RunID: r.RunID, Kind: r.Kind, Status: r.Status,
			OwnerLabel: r.OwnerLabel, Repo: textPtr(r.Repo), Since: tsPtr(r.Since), Tag: &tag, HoldID: &hold})
	}
	queued, err := q.ListMaintenanceQueuedRuns(ctx)
	if err != nil {
		return MaintenanceReadout{}, err
	}
	for _, r := range queued {
		out.Queued.Count = r.Total
		out.Queued.Items = append(out.Queued.Items, MaintenanceItem{RunID: r.RunID, Kind: r.Kind, Status: r.Status,
			OwnerLabel: r.OwnerLabel, Repo: textPtr(r.Repo), Since: tsPtr(r.Since)})
	}
	workers, err := q.ListMaintenanceIdleBoundWorkers(ctx)
	if err != nil {
		return MaintenanceReadout{}, err
	}
	for _, r := range workers {
		out.IdleBoundWorkers.Count = r.Total
		out.IdleBoundWorkers.Items = append(out.IdleBoundWorkers.Items, MaintenanceWorker{WorkerID: r.WorkerID,
			OwnerLabel: r.OwnerLabel, RunID: r.RunID, Since: tsPtr(r.Since)})
	}
	return out, nil
}
