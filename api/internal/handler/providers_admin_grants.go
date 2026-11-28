package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Recipient grants of an organization-funded definition (PRD #4540 M2, R6). A definition's recipients
// are its named grants (model_provider_grants) plus the all-users switch (operator_all_users); both are
// written here and nowhere else. Who granted and when lives in the provider_audit log, never the table.
//
// The reader (GET) is an admin read-only route; every write is a cookie + CSRF admin write. Each write
// runs in one withAdminTx and takes the definition row first (LockSharedModelProviderForEdit), then the
// mode check (a per_user_key definition is a 409), so all grant, all-users, key, edit, retire and delete
// writes serialize on the one row. The caller-order pins are grantCallOrderPins in
// api/internal/store/model_provider_visibility_sql_test.go. A write is allowed on a retired definition:
// removing recipients must stay available after retire. Per write the order is: commit, then
// auditProvider, then the response, as the key routes do.
const (
	// maxProviderGrants is the cap on named grants per definition. A PUT adding a grant at the cap is a
	// 409; a PUT of a grant that already exists is a 204 at any count.
	maxProviderGrants = 10000

	// Grant page size: the default, and the largest a caller may ask for.
	defaultGrantsPageLimit = 100
	maxGrantsPageLimit     = 500

	// grantsUserFKConstraint is the grants table's foreign key to users: a user deleted between the
	// user lock and the insert reads as user_not_found, not a 500.
	grantsUserFKConstraint = "model_provider_grants_user_id_fkey"

	reasonUserNotFound        = "user_not_found"
	reasonProviderGrantCapHit = "provider_grant_cap_reached"
)

var (
	errGrantUserNotFound = errors.New("grantee not found")
	errGrantCapReached   = errors.New("provider grant limit reached")
)

// writeGrantError maps the grant routes' errors, then the shared definition ones.
func writeGrantError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, errGrantUserNotFound), isPGForeignKeyViolation(err, grantsUserFKConstraint):
		httpx.ErrorReason(w, http.StatusNotFound, "user not found", reasonUserNotFound)
	case errors.Is(err, errGrantCapReached):
		httpx.ErrorReason(w, http.StatusConflict,
			"the definition already has "+strconv.Itoa(maxProviderGrants)+" named recipients; remove one first or use all users",
			reasonProviderGrantCapHit)
	default:
		return writeSharedProviderError(w, err)
	}
	return true
}

// withAdminReadTx runs fn in one REPEATABLE READ, READ ONLY transaction, so a page's all-users flag,
// count and rows are one snapshot.
func (h *Handler) withAdminReadTx(ctx context.Context, fn func(q *store.Queries) error) error {
	var tx pgx.Tx
	var err error
	switch {
	case h.secretBeginTx != nil:
		tx, err = h.secretBeginTx(ctx)
	case h.pool != nil:
		tx, err = h.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	default:
		return store.ErrVaultTransactionRequired
	}
	if err != nil {
		return err
	}
	if tx == nil {
		return store.ErrVaultTransactionRequired
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY"); err != nil {
		return err
	}
	if err := fn(store.New(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// grantsPageParams reads ?cursor=<user_id>&limit=<n>. A bad cursor or a limit outside 1..500 is a 400
// (reported through w); no cursor starts from the beginning.
func grantsPageParams(w http.ResponseWriter, r *http.Request) (after uuid.UUID, limit int, ok bool) {
	limit = defaultGrantsPageLimit
	q := r.URL.Query()
	if raw := q.Get("cursor"); raw != "" {
		c, err := uuid.Parse(raw)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid cursor")
			return uuid.Nil, 0, false
		}
		after = c
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxGrantsPageLimit {
			httpx.Error(w, http.StatusBadRequest, "limit must be between 1 and "+strconv.Itoa(maxGrantsPageLimit))
			return uuid.Nil, 0, false
		}
		limit = n
	}
	return after, limit, true
}

// AdminListProviderGrants is GET /api/admin/providers/{id}/grants?cursor=<user_id>&limit=<n>: one
// keyset page of a definition's named recipients with the all-users flag and the total count, all read
// in one REPEATABLE READ READ ONLY transaction. An unknown definition is a 404 and a per_user_key one a
// 409 provider_not_operator_key. Items carry only the user id.
func (h *Handler) AdminListProviderGrants(w http.ResponseWriter, r *http.Request) {
	if _, ok := egressProfileAdmin(w, r); !ok {
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "provider")
	if !ok {
		return
	}
	after, limit, ok := grantsPageParams(w, r)
	if !ok {
		return
	}
	out := apitypes.AdminProviderGrantsDTO{Grants: []apitypes.AdminProviderGrantDTO{}}
	err := h.withAdminReadTx(r.Context(), func(q *store.Queries) error {
		row, err := q.GetSharedModelProviderAdmin(r.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) {
			return errProviderNotFound
		}
		if err != nil {
			return err
		}
		if row.CredentialMode.String != sharedProviderModeOperatorKey {
			return errNotOperatorProvider
		}
		out.OperatorAllUsers, out.NamedGrantCount = row.OperatorAllUsers, row.NamedGrantCount
		// One row beyond the page says whether another page follows.
		ids, err := q.ListModelProviderGrantsPage(r.Context(), store.ListModelProviderGrantsPageParams{
			ProviderID: id, AfterUserID: after, PageLimit: int32(limit + 1), //nolint:gosec // G115: limit is at most maxGrantsPageLimit
		})
		if err != nil {
			return err
		}
		if len(ids) > limit {
			ids = ids[:limit]
			next := ids[len(ids)-1].String()
			out.NextCursor = &next
		}
		for _, u := range ids {
			out.Grants = append(out.Grants, apitypes.AdminProviderGrantDTO{UserID: u.String()})
		}
		return nil
	})
	if err != nil {
		if writeGrantError(w, err) {
			return
		}
		internalProviderError(w, "list provider grants", err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// AdminPutProviderGrant is PUT /api/admin/providers/{id}/grants/{user_id}: names one recipient.
// Idempotent: an existing grant is a 204 with no audit line, even at the cap. An unknown or deleted user
// is a 404 user_not_found; a new grant at the cap is a 409 provider_grant_cap_reached.
func (h *Handler) AdminPutProviderGrant(w http.ResponseWriter, r *http.Request) {
	actor, ok := egressProfileAdmin(w, r)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "provider")
	if !ok {
		return
	}
	userID, ok := httpx.PathUUID(w, r, "user_id", "user")
	if !ok {
		return
	}
	added := false
	err := h.withAdminTx(r.Context(), func(q *store.Queries) error {
		cur, err := q.LockSharedModelProviderForEdit(r.Context(), id)
		if err := operatorDefinitionError(cur.CredentialMode.String, err); err != nil {
			return err
		}
		// The grantee's row, locked after the definition: a user delete waits for this transaction, and a
		// deleted user reads as no row.
		if _, err := q.LockUserForGrant(r.Context(), userID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errGrantUserNotFound
			}
			return err
		}
		n, err := q.CountModelProviderGrants(r.Context(), id)
		if err != nil {
			return err
		}
		if n >= maxProviderGrants {
			// At the cap only a grant that already exists is accepted (as a no-op).
			exists, err := q.HasModelProviderGrant(r.Context(), store.HasModelProviderGrantParams{ProviderID: id, UserID: userID})
			if err != nil {
				return err
			}
			if !exists {
				return errGrantCapReached
			}
			return nil
		}
		rows, err := q.InsertModelProviderGrant(r.Context(), store.InsertModelProviderGrantParams{ProviderID: id, UserID: userID})
		added = rows > 0
		return err
	})
	if err != nil {
		if writeGrantError(w, err) {
			return
		}
		internalProviderError(w, "add provider grant", err)
		return
	}
	if added {
		auditProvider("admin_grant_add", actor.ID, id, "user_id", userID.String())
	}
	w.WriteHeader(http.StatusNoContent)
}

// AdminDeleteProviderGrant is DELETE /api/admin/providers/{id}/grants/{user_id}: removes one recipient.
// Always a 204 for an existing definition, an absent grant and a deleted user included; the audit line is
// written only when a row was deleted.
func (h *Handler) AdminDeleteProviderGrant(w http.ResponseWriter, r *http.Request) {
	actor, ok := egressProfileAdmin(w, r)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "provider")
	if !ok {
		return
	}
	userID, ok := httpx.PathUUID(w, r, "user_id", "user")
	if !ok {
		return
	}
	removed := false
	err := h.withAdminTx(r.Context(), func(q *store.Queries) error {
		cur, err := q.LockSharedModelProviderForEdit(r.Context(), id)
		if err := operatorDefinitionError(cur.CredentialMode.String, err); err != nil {
			return err
		}
		n, err := q.DeleteModelProviderGrant(r.Context(), store.DeleteModelProviderGrantParams{ProviderID: id, UserID: userID})
		removed = n > 0
		return err
	})
	if err != nil {
		if writeGrantError(w, err) {
			return
		}
		internalProviderError(w, "remove provider grant", err)
		return
	}
	if removed {
		auditProvider("admin_grant_remove", actor.ID, id, "user_id", userID.String())
	}
	w.WriteHeader(http.StatusNoContent)
}

// AdminPutProviderAllUsers is PUT /api/admin/providers/{id}/all-users {enabled}: makes every user a
// recipient of the definition, or stops doing so. Idempotent, always a 204, and always one audit line
// with `enabled` and `changed` (whether the previous value differed).
func (h *Handler) AdminPutProviderAllUsers(w http.ResponseWriter, r *http.Request) {
	actor, ok := egressProfileAdmin(w, r)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "provider")
	if !ok {
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeProviderBody(w, r, &req) {
		return
	}
	if req.Enabled == nil {
		httpx.Error(w, http.StatusBadRequest, "enabled is required")
		return
	}
	enabled := *req.Enabled
	changed := false
	err := h.withAdminTx(r.Context(), func(q *store.Queries) error {
		cur, err := q.LockSharedModelProviderForEdit(r.Context(), id)
		if err := operatorDefinitionError(cur.CredentialMode.String, err); err != nil {
			return err
		}
		changed = cur.OperatorAllUsers != enabled
		n, err := q.SetOperatorAllUsers(r.Context(), store.SetOperatorAllUsersParams{ID: id, Enabled: enabled})
		if err != nil {
			return err
		}
		if n == 0 {
			return errProviderNotFound
		}
		return nil
	})
	if err != nil {
		if writeGrantError(w, err) {
			return
		}
		internalProviderError(w, "set all-users recipients", err)
		return
	}
	auditProvider("admin_all_users", actor.ID, id, "enabled", enabled, "changed", changed)
	w.WriteHeader(http.StatusNoContent)
}

// AdminRevokeProviderRecipients is POST /api/admin/providers/{id}/revoke-recipients: turns all-users off
// and deletes every named grant in one transaction. It answers 200 {named_grants_removed} and writes one
// aggregate audit line (admin_revoke_recipients), not one per grant.
func (h *Handler) AdminRevokeProviderRecipients(w http.ResponseWriter, r *http.Request) {
	actor, ok := egressProfileAdmin(w, r)
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "provider")
	if !ok {
		return
	}
	var removed int64
	var previousAllUsers bool
	err := h.withAdminTx(r.Context(), func(q *store.Queries) error {
		cur, err := q.LockSharedModelProviderForEdit(r.Context(), id)
		if err := operatorDefinitionError(cur.CredentialMode.String, err); err != nil {
			return err
		}
		previousAllUsers = cur.OperatorAllUsers
		n, err := q.SetOperatorAllUsers(r.Context(), store.SetOperatorAllUsersParams{ID: id, Enabled: false})
		if err != nil {
			return err
		}
		if n == 0 {
			return errProviderNotFound
		}
		removed, err = q.DeleteAllModelProviderGrants(r.Context(), id)
		return err
	})
	if err != nil {
		if writeGrantError(w, err) {
			return
		}
		internalProviderError(w, "revoke provider recipients", err)
		return
	}
	auditProvider("admin_revoke_recipients", actor.ID, id, "named_grants_removed", removed, "previous_all_users", previousAllUsers)
	httpx.JSON(w, http.StatusOK, apitypes.AdminRevokeRecipientsDTO{NamedGrantsRemoved: removed})
}
