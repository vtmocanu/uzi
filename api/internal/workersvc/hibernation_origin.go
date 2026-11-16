package workersvc

import (
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Op-bound origin markers (issue #4395, D11). The hibernation op lives in columns on workers;
// hibernation_origin_op_id binds the two origin columns to the op they describe. A marker is
// valid only while hibernation_origin_op_id equals hibernation_op_id, so a new op minted by an
// older api replica (which never writes the markers) reads as legacy.

// Admit origins: who admitted the op (none -> preparing).
const (
	AdmitOriginManual      = "manual"
	AdmitOriginAutoLong    = "auto_long"
	AdmitOriginAutoSurplus = "auto_surplus"
)

// Wake origins: who moved the op to waking.
const (
	WakeOriginManual = "manual"
	WakeOriginDemand = "demand"
	WakeOriginSystem = "system"
	WakeOriginWarm   = "warm"
)

// originMarkersValid reports whether w's origin markers describe its current op.
func originMarkersValid(w *store.Worker) bool {
	return w.HibernationOriginOpID.Valid && w.HibernationOpID.Valid &&
		w.HibernationOriginOpID.Bytes == w.HibernationOpID.Bytes
}

// validWakeOrigin is the wake origin of w's current op, or "" when the markers are not valid
// (legacy) or the op has no wake origin yet.
func validWakeOrigin(w *store.Worker) string {
	if !originMarkersValid(w) || !w.HibernationWakeOrigin.Valid {
		return ""
	}
	return w.HibernationWakeOrigin.String
}

// joinWakeOrigin is the wake origin an already-waking op carries after a request with origin req
// joins it. A manual or demand request upgrades a warm op; nothing ever downgrades to warm, and
// every other pair keeps cur (a system request is not stronger than any recorded origin, and a
// legacy cur of "" stays legacy: it already reads as manual-grade).
func joinWakeOrigin(cur, req string) string {
	if cur == WakeOriginWarm && (req == WakeOriginManual || req == WakeOriginDemand) {
		return req
	}
	return cur
}

// carryManualGrace raises last_manual_wake_settled_at to last_wake_settled_at when the current
// op's settle counts as manual-grade, so a later op's origin never reclassifies an earlier
// settlement (D15). It must run before any write that replaces the op's markers or sets the wake
// origin to warm or system.
//
// The settle is manual-grade unless the markers are valid and the wake origin is warm or system.
// A valid op with an admit origin and no wake origin has not woken: last_wake_settled_at was
// already classified when it was admitted, so it carries nothing (reading it by the legacy rule
// would reclassify an earlier warm or system settle as manual-grade).
func carryManualGrace(w *store.Worker) {
	if !w.LastWakeSettledAt.Valid {
		return
	}
	if originMarkersValid(w) {
		switch w.HibernationWakeOrigin.String {
		case WakeOriginWarm, WakeOriginSystem:
			return
		}
		if !w.HibernationWakeOrigin.Valid && w.HibernationAdmitOrigin.Valid {
			return
		}
	}
	if !w.LastManualWakeSettledAt.Valid || w.LastManualWakeSettledAt.Time.Before(w.LastWakeSettledAt.Time) {
		w.LastManualWakeSettledAt = w.LastWakeSettledAt
	}
}

// stampAdmitOrigin binds a fresh admission op to its admit origin; the wake origin starts unset.
func stampAdmitOrigin(w *store.Worker, op uuid.UUID, origin string) {
	w.HibernationOriginOpID = hibernationUUID(op)
	w.HibernationAdmitOrigin = pgconv.TextOrNull(origin)
	w.HibernationWakeOrigin = pgtype.Text{}
}

// stampWakeOrigin binds a fresh wake op to its wake origin; a wake op carries no admit origin.
func stampWakeOrigin(w *store.Worker, op uuid.UUID, origin string) {
	w.HibernationOriginOpID = hibernationUUID(op)
	w.HibernationAdmitOrigin = pgtype.Text{}
	w.HibernationWakeOrigin = pgconv.TextOrNull(origin)
}
