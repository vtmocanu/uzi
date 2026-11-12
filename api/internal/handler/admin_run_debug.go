package handler

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// adminRunDebugMessage is the message of the single audit record written per
// newly authorized cross-user run debug read.
const adminRunDebugMessage = "admin run debug read"

// adminRunDebugRoute names the debug read an audit record is for.
type adminRunDebugRoute string

const (
	adminRunDebugRouteInputs   adminRunDebugRoute = "runs.inputs"
	adminRunDebugRouteArchives adminRunDebugRoute = "runs.archives"
	adminRunDebugRouteFetches  adminRunDebugRoute = "runs.fetches"
	adminRunDebugRouteHolds    adminRunDebugRoute = "recovery.holds"
	adminRunDebugRouteRuns     adminRunDebugRoute = "admin.runs"
)

// adminRunDebugLogger returns the logger the audit records go to. A variable so
// tests can capture the records without touching the process-wide default.
var adminRunDebugLogger = func() *slog.Logger { return slog.Default() }

// adminRunDebugActor reports the acting admin and the CLI token id when this
// request may take the cross-user debug path. It fails closed: the deployment flag
// must be on, the request must carry a Bearer CLI credential whose STORED scope is
// admin_ro (never a cookie, never a uzc_ token), and the user, loaded live per
// request, must still be an admin.
func (h *Handler) adminRunDebugActor(r *http.Request) (store.User, uuid.UUID, bool) {
	if !h.cfg.AdminRunDebug {
		return store.User{}, uuid.Nil, false
	}
	cred, ok := mw.CLICredentialFromContext(r.Context())
	if !ok || cred.Scope != clitoken.ScopeAdminRO {
		return store.User{}, uuid.Nil, false
	}
	user, ok := mw.UserFromContext(r.Context())
	if !ok || !user.IsAdmin {
		return store.User{}, uuid.Nil, false
	}
	return user, cred.TokenID, true
}

func adminRunDebugBaseAttrs(actor store.User, tokenID uuid.UUID, route adminRunDebugRoute) []any {
	return []any{
		"actor_user_id", actor.ID.String(),
		"token_id", tokenID.String(),
		"route", string(route),
	}
}

// auditRunDebugRead records a cross-user read of one run's debug data.
func auditRunDebugRead(r *http.Request, actor store.User, tokenID uuid.UUID, route adminRunDebugRoute, runID, ownerID uuid.UUID) {
	attrs := append(adminRunDebugBaseAttrs(actor, tokenID, route),
		"run_id", runID.String(), "owner_id", ownerID.String())
	adminRunDebugLogger().InfoContext(r.Context(), adminRunDebugMessage, attrs...)
}

// auditHoldsDebugRead records a cross-user read of another owner's recovery holds.
func auditHoldsDebugRead(r *http.Request, actor store.User, tokenID uuid.UUID, route adminRunDebugRoute, targetOwnerID uuid.UUID) {
	attrs := append(adminRunDebugBaseAttrs(actor, tokenID, route),
		"target_owner_id", targetOwnerID.String())
	adminRunDebugLogger().InfoContext(r.Context(), adminRunDebugMessage, attrs...)
}

// auditRunsListDebugRead records an all-states admin run listing. ownerFilter nil
// means all owners; since is omitted from the record when nil.
func auditRunsListDebugRead(r *http.Request, actor store.User, tokenID uuid.UUID, route adminRunDebugRoute, ownerFilter *uuid.UUID, since *time.Time, returned int) {
	owner := "all"
	if ownerFilter != nil {
		owner = ownerFilter.String()
	}
	attrs := append(adminRunDebugBaseAttrs(actor, tokenID, route), "owner_filter", owner)
	if since != nil {
		attrs = append(attrs, "since", since.UTC().Format(time.RFC3339Nano))
	}
	attrs = append(attrs, "returned", returned)
	adminRunDebugLogger().InfoContext(r.Context(), adminRunDebugMessage, attrs...)
}
