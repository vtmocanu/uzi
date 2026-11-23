package handler

// run_env_vars.go: per-repo environment variables for runs (PRD #4416 M1). Variables
// are stored and managed here; nothing injects them into a run yet (M2).
//
// Every value, plain or secret, is sealed through the vault seam with the AAD that
// runenv.AADKind builds. A secret's value is never opened for a response. Writes run
// in one transaction that takes the owner's secret-mutation advisory lock and then
// the users-row fence, like every other secret writer (withSecretLock).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/runenv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// Machine-readable codes of the 409 answers a client branches on.
const (
	runEnvCodeLimitReached = "env_limit_reached"
	runEnvCodeHistoryFull  = "env_history_full"
	runEnvCodeChanged      = "env_changed"
)

// runEnvMaxBodyBytes caps a PUT body, the same 1 MiB as httpx.DecodeJSONLimited.
const runEnvMaxBodyBytes = 1 << 20

// errRunEnvChanged: the live row moved between the read that planned the write and the
// locked re-read inside the transaction.
var errRunEnvChanged = errors.New("run env: the variable changed concurrently")

// errRunEnvRepoGone: the repo was deleted between the ownership check and the write.
var errRunEnvRepoGone = errors.New("run env: repo not found")

// lockRunEnvRepo pins the repo row (FOR KEY SHARE) right after the users-row fence and
// before any run_env_vars statement, so a concurrent repo delete (whose cascade takes the
// env rows) cannot interleave with this write. The lock order of every env writer is:
// secret-mutation advisory lock, users row, repos row, env rows. The full vault conversion
// lock order (lockLifecycleState, lockVaultRows, convertMaterial) is:
//  1. the settings admission lock (LockSettingsAdmission), taken when the lifecycle takes it;
//  2. the secret-mutation advisory lock;
//  3. the users row, then the user_vaults row (lockVaultRows);
//  4. connections, repos, accounts, secrets, env rows (convertMaterial).
//
// An env writer's order is a subsequence of this one, so the two cannot deadlock.
func lockRunEnvRepo(ctx context.Context, q *store.Queries, repoID uuid.UUID) error {
	if _, err := q.LockRepoKeyShare(ctx, repoID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errRunEnvRepoGone
		}
		return err
	}
	return nil
}

// runEnvMasterSealed: an env row sealed under the nil-AAD legacy marker. The table never
// admits one (its CHECK), so seeing one means the row was tampered with; it is refused
// rather than opened without its name/repo binding.
func runEnvMasterSealed(sealedWith string) bool { return sealedWith == store.SealedWithMaster }

// runEnvConflictError is a 409 raised inside a write transaction.
type runEnvConflictError struct{ code, message string }

func (e *runEnvConflictError) Error() string { return e.code + ": " + e.message }

// runEnvHeadroomConflict turns a failed vault headroom check into the 409 a run-env save
// answers with. The vault's own message talks about enabling or disabling the vault, which
// is not what the caller is doing; any other error is returned unchanged (a 500).
func runEnvHeadroomConflict(code string, err error) error {
	var limit *vault.ConversionLimitError
	if !errors.As(err, &limit) {
		return err
	}
	return &runEnvConflictError{code, fmt.Sprintf("saving this would bring your stored credentials and variables to %d records and %d bytes, over the vault's storage limit; disable or delete a variable, or remove a credential, first", limit.Rows, limit.Bytes)}
}

// runEnvRow is the shape every run_env_vars read returns, whichever generated query
// produced it.
type runEnvRow = store.ListRunEnvVarsForRepoRow

// runEnvOwner resolves the caller and the repo they own, answering 401/400/404 itself
// (the same preflight as GetRepoToolProfile: a foreign or unknown repo is a 404).
func (h *Handler) runEnvOwner(w http.ResponseWriter, r *http.Request) (userID, repoID uuid.UUID, ok bool) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return uuid.Nil, uuid.Nil, false
	}
	id, ok := httpx.PathUUID(w, r, "id", "repo")
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}
	if _, err := h.q.GetRepoForUser(r.Context(), store.GetRepoForUserParams{ID: id, UserID: user.ID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "repo not found")
			return uuid.Nil, uuid.Nil, false
		}
		slog.Error("get repo for run env", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return uuid.Nil, uuid.Nil, false
	}
	return user.ID, id, true
}

func repoPG(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

// writeRunEnvRule answers a broken field rule with 422, naming the field and the rule.
func writeRunEnvRule(w http.ResponseWriter, err error) {
	var rule *runenv.RuleError
	if !errors.As(err, &rule) {
		httpx.Error(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	httpx.JSON(w, http.StatusUnprocessableEntity, map[string]string{
		"error": rule.Field + ": " + rule.Rule,
		"field": rule.Field,
		"rule":  rule.Rule,
	})
}

func writeRunEnvConflict(w http.ResponseWriter, code, message string) {
	httpx.JSON(w, http.StatusConflict, map[string]string{"error": message, "code": code})
}

// writeRunEnvTxError maps an error out of a write transaction.
func (h *Handler) writeRunEnvTxError(w http.ResponseWriter, err error, op string) {
	var conflict *runEnvConflictError
	var pgErr *pgconn.PgError
	switch {
	case h.writeVaultStale(w, err):
	case h.writeVaultLocked(w, err):
	case errors.As(err, &conflict):
		writeRunEnvConflict(w, conflict.code, conflict.message)
	case errors.Is(err, errRunEnvRepoGone), errors.As(err, &pgErr) && pgErr.Code == "23503":
		httpx.Error(w, http.StatusNotFound, "repo not found")
	case errors.Is(err, errRunEnvChanged), errors.Is(err, pgx.ErrNoRows), errors.As(err, &pgErr) && pgErr.Code == "23505":
		writeRunEnvConflict(w, runEnvCodeChanged, "the variable changed while you were saving; reload and try again")
	default:
		slog.Error(op, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
}

func runEnvState(r runEnvRow) string {
	switch {
	case r.PendingDeleteAt.Valid:
		return apitypes.RunEnvStatePendingDeletion
	case r.DisabledAt.Valid:
		return apitypes.RunEnvStateDisabled
	}
	return apitypes.RunEnvStateEnabled
}

// runEnvDTO builds the response view of one row. A secret's value is never opened. A
// plain value is opened through the vault; a locked vault (or a value that cannot be
// opened) withholds it and reports locked=true only for the locked case. vaultLocked
// withholds every plain value without opening it: a row read before the vault was
// protected and locked may still carry master_aad material the master key opens.
func (h *Handler) runEnvDTO(userID, repoID uuid.UUID, row runEnvRow, vaultLocked bool) (dto apitypes.RunEnvVarDTO, locked bool) {
	dto = apitypes.RunEnvVarDTO{
		VarID:       row.VarID,
		Name:        row.Name,
		Secret:      row.Secret,
		Description: row.Description,
		State:       runEnvState(row),
		Rev:         row.Rev,
		CreatedAt:   row.CreatedAt.Time,
		UpdatedAt:   row.UpdatedAt.Time,
	}
	if row.DisabledAt.Valid {
		t := row.DisabledAt.Time
		dto.DisabledAt = &t
	}
	if row.Secret {
		return dto, false
	}
	if h.vault == nil {
		dto.ValueWithheld = true
		return dto, false
	}
	if vaultLocked {
		dto.ValueWithheld = true
		return dto, true
	}
	if runEnvMasterSealed(row.SealedWith) {
		slog.Error("run env value sealed without AAD", "name", row.Name)
		dto.ValueWithheld = true
		return dto, false
	}
	kind := runenv.AADKind(runenv.ScopeRepo, &repoID, row.Secret, row.Name)
	plain, err := h.vault.Open(userID, kind, row.SealedWith, row.Ciphertext)
	if err != nil {
		dto.ValueWithheld = true
		if errors.Is(err, vault.ErrLocked) {
			return dto, true
		}
		slog.Error("open plain run env value", "error", err)
		return dto, false
	}
	v := string(plain)
	wipeBytes(plain)
	dto.Value = &v
	return dto, false
}

// runEnvWriteDTO builds a write response's view of the saved row. It asks the vault for
// its lock state after the write committed, so an enable+lock that lands between the
// commit and this response still withholds the plain value (the row may carry master_aad
// material the master key opens).
func (h *Handler) runEnvWriteDTO(ctx context.Context, userID, repoID uuid.UUID, row runEnvRow) apitypes.RunEnvVarDTO {
	vaultLocked := h.vault != nil && h.vault.RequiresUnlock(ctx, userID)
	dto, _ := h.runEnvDTO(userID, repoID, row, vaultLocked)
	return dto
}

func wipeBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// ListRepoEnvVars is GET /api/repos/{id}/env: names, kinds, descriptions, states and
// plain values. Accepts the session cookie or a CLI token (RequireUser). A secret's
// value is in no response; a locked vault withholds plain values and says so.
func (h *Handler) ListRepoEnvVars(w http.ResponseWriter, r *http.Request) {
	userID, repoID, ok := h.runEnvOwner(w, r)
	if !ok {
		return
	}
	rows, err := h.q.ListRunEnvVarsForRepo(r.Context(), store.ListRunEnvVarsForRepoParams{UserID: userID, RepoID: repoPG(repoID)})
	if err != nil {
		slog.Error("list run env vars", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := apitypes.RunEnvListDTO{
		Variables: make([]apitypes.RunEnvVarDTO, 0, len(rows)),
		Counts:    apitypes.RunEnvCountsDTO{Limit: runenv.MaxVarsPerScope},
	}
	// Secret rows never open here, so a secret-only list cannot tell a locked vault from
	// an unlocked one by its rows: ask the vault itself (fail-closed on a lookup error).
	// Asked after the rows are read, so a lock that lands in between still withholds them.
	if h.vault != nil {
		out.VaultLocked = h.vault.RequiresUnlock(r.Context(), userID)
	}
	vaultLocked := out.VaultLocked
	for _, row := range rows {
		dto, locked := h.runEnvDTO(userID, repoID, row, vaultLocked)
		out.VaultLocked = out.VaultLocked || locked
		out.Variables = append(out.Variables, dto)
		out.Counts.Total++
		switch dto.State {
		case apitypes.RunEnvStateDisabled:
			out.Counts.Disabled++
		case apitypes.RunEnvStatePendingDeletion:
			out.Counts.PendingDeletion++
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, out)
}

type putRunEnvRequest struct {
	Value       *string `json:"value"`
	Secret      *bool   `json:"secret"`
	Description *string `json:"description"`
}

// receiptHeld asks whether any run holds a delivery receipt for one version. M1 has
// no receipts (store.RunEnvVersionHasReceipt answers false); tests replace the seam to
// reach the retire and pending-deletion paths.
func (h *Handler) receiptHeld(ctx context.Context, q *store.Queries, varID uuid.UUID, rev int32) (bool, error) {
	if h.runEnvHasReceipt != nil {
		return h.runEnvHasReceipt(ctx, varID, rev)
	}
	return q.RunEnvVersionHasReceipt(ctx, varID, rev)
}

// PutRepoEnvVar is PUT /api/repos/{id}/env/{name}: create a variable, change its
// description in place (no value), or write a new version (a value, or a change of the
// secret flag). Cookie and CSRF only.
func (h *Handler) PutRepoEnvVar(w http.ResponseWriter, r *http.Request) {
	userID, repoID, ok := h.runEnvOwner(w, r)
	if !ok {
		return
	}
	name := chi.URLParam(r, "name")
	if err := runenv.ValidateName(name); err != nil {
		writeRunEnvRule(w, err)
		return
	}
	// encoding/json replaces invalid UTF-8 with U+FFFD, which would store a different
	// value than the caller sent, so the raw body is checked before it is decoded.
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, runEnvMaxBodyBytes))
	if err != nil {
		httpx.RespondDecodeError(w, err, "invalid request body")
		return
	}
	if !utf8.Valid(raw) {
		httpx.Error(w, http.StatusBadRequest, "the request body is not valid UTF-8")
		return
	}
	var req putRunEnvRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		httpx.RespondDecodeError(w, err, "invalid request body")
		return
	}
	// An escaped unpaired UTF-16 surrogate also decodes to U+FFFD; refuse it for the same
	// reason. The walk is valid only on a body that already decoded as JSON.
	if hasLoneSurrogateEscape(raw) {
		httpx.Error(w, http.StatusBadRequest, "the request body contains an unpaired UTF-16 surrogate escape (\\uD800-\\uDFFF)")
		return
	}
	if req.Description != nil {
		if err := runenv.ValidateDescription(*req.Description); err != nil {
			writeRunEnvRule(w, err)
			return
		}
	}
	ctx := r.Context()
	repo := repoPG(repoID)

	cur, err := h.q.GetLiveRunEnvVarByName(ctx, store.GetLiveRunEnvVarByNameParams{UserID: userID, RepoID: repo, Name: name})
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("get run env var", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	secret := true // a new variable is secret unless the caller says otherwise
	description := ""
	if exists {
		secret, description = cur.Secret, cur.Description
	}
	if req.Secret != nil {
		secret = *req.Secret
	}
	if req.Description != nil {
		description = *req.Description
	}
	if !exists && req.Value == nil {
		writeRunEnvRule(w, &runenv.RuleError{Field: "value", Rule: "a value is required to create a variable"})
		return
	}
	if exists && cur.Secret && !secret && req.Value == nil {
		writeRunEnvRule(w, &runenv.RuleError{Field: "value", Rule: "changing a secret to a plain value needs a new value in the same save: a stored secret is never read back"})
		return
	}

	newVersion := req.Value != nil || (exists && secret != cur.Secret)
	if exists && !newVersion {
		h.putRunEnvDescription(ctx, w, userID, repoID, cur, description)
		return
	}

	// A new version: obtain the plaintext, validate it for the TARGET secret flag, seal it
	// before the transaction. A plain-to-secret change reseals the stored value.
	var value []byte
	if req.Value != nil {
		value = []byte(*req.Value)
	} else {
		if h.vault == nil {
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		if runEnvMasterSealed(cur.SealedWith) {
			slog.Error("run env value sealed without AAD", "name", name)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		old, openErr := h.vault.Open(userID, runenv.AADKind(runenv.ScopeRepo, &repoID, cur.Secret, name), cur.SealedWith, cur.Ciphertext)
		if openErr != nil {
			if h.writeVaultLocked(w, openErr) {
				return
			}
			slog.Error("open stored run env value", "error", openErr)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		value = old
	}
	defer wipeBytes(value)
	if err := runenv.ValidateValue(secret, string(value)); err != nil {
		writeRunEnvRule(w, err)
		return
	}
	if h.vault == nil {
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	material, err := h.vault.Seal(ctx, userID, runenv.AADKind(runenv.ScopeRepo, &repoID, secret, name), value, vault.SealForUse)
	if err != nil {
		if h.writeVaultLocked(w, err) {
			return
		}
		slog.Error("seal run env value", "error", err) // carries no plaintext
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	var saved runEnvRow
	err = h.withSecretLock(ctx, userID, &material, func(q *store.Queries) error {
		if err := lockRunEnvRepo(ctx, q, repoID); err != nil {
			return err
		}
		live, getErr := q.GetLiveRunEnvVarByNameForUpdate(ctx, store.GetLiveRunEnvVarByNameForUpdateParams{UserID: userID, RepoID: repo, Name: name})
		switch {
		case exists && getErr != nil, exists && (live.ID != cur.ID || live.Rev != cur.Rev):
			if getErr != nil && !errors.Is(getErr, pgx.ErrNoRows) {
				return getErr
			}
			return errRunEnvChanged
		case !exists && getErr == nil:
			return errRunEnvChanged
		case !exists && !errors.Is(getErr, pgx.ErrNoRows):
			return getErr
		}
		usage, err := q.GetRunEnvScopeUsage(ctx, store.GetRunEnvScopeUsageParams{UserID: userID, RepoID: repo})
		if err != nil {
			return err
		}
		totals, err := q.GetVaultMaterialTotals(ctx, userID)
		if err != nil {
			return err
		}
		newBytes := int64(len(value))
		// A description-only save does not bump rev, so one committed between the planning
		// read and this transaction passes the id/rev check: an omitted description keeps
		// the locked row's, not the planning read's.
		description := description
		if exists && req.Description == nil {
			description = live.Description
		}
		params := store.InsertRunEnvVarParams{
			UserID: userID, RepoID: repo, Name: name, Secret: secret, Description: description,
			Ciphertext: material.Ciphertext, SealedWith: material.SealedWith, ValueBytes: int32(newBytes), //nolint:gosec // G115: bounded to 65536 by ValidateValue.
		}
		if !exists {
			if usage.LiveCount+usage.PendingCount >= runenv.MaxVarsPerScope {
				return &runEnvConflictError{runEnvCodeLimitReached, fmt.Sprintf("this repo already has %d variables (disabled ones and ones pending deletion count); delete one first", runenv.MaxVarsPerScope)}
			}
			if usage.ValueBytes+newBytes > runenv.MaxScopeValueBytes {
				return &runEnvConflictError{runEnvCodeLimitReached, fmt.Sprintf("the variables of this repo may hold at most %d bytes of values", runenv.MaxScopeValueBytes)}
			}
			if err := vault.CheckMaterialHeadroom(totals, 1, int64(len(material.Ciphertext)), int64(len(material.Ciphertext))); err != nil {
				return runEnvHeadroomConflict(runEnvCodeLimitReached, err)
			}
			params.VarID, params.Rev = uuid.New(), 1
		} else {
			if usage.ValueBytes-int64(live.ValueBytes)+newBytes > runenv.MaxScopeValueBytes {
				return &runEnvConflictError{runEnvCodeLimitReached, fmt.Sprintf("the variables of this repo may hold at most %d bytes of values", runenv.MaxScopeValueBytes)}
			}
			retain := false
			if live.Secret {
				if retain, err = h.receiptHeld(ctx, q, live.VarID, live.Rev); err != nil {
					return err
				}
			}
			if retain {
				// The previous secret version stays sealed for the runs that hold a receipt
				// for it, inside the per-user history budget and the vault material totals.
				retired, err := q.GetRunEnvRetiredUsage(ctx, userID)
				if err != nil {
					return err
				}
				if retired.RetiredCount+1 > runenv.MaxRetiredVersions || retired.ValueBytes+int64(live.ValueBytes) > runenv.MaxRetiredValueBytes {
					return &runEnvConflictError{runEnvCodeHistoryFull, fmt.Sprintf("this change keeps the previous secret version for runs that still hold it, and the history budget is full (%d of %d versions, %d of %d bytes); disable or delete a variable instead", retired.RetiredCount, runenv.MaxRetiredVersions, retired.ValueBytes, runenv.MaxRetiredValueBytes)}
				}
				if err := vault.CheckMaterialHeadroom(totals, 1, int64(len(material.Ciphertext)), int64(len(material.Ciphertext))); err != nil {
					return runEnvHeadroomConflict(runEnvCodeHistoryFull, err)
				}
				if n, err := q.RetireRunEnvVarVersion(ctx, store.RetireRunEnvVarVersionParams{ID: live.ID, UserID: userID}); err != nil || n != 1 {
					if err == nil {
						err = errRunEnvChanged
					}
					return err
				}
			} else {
				// The old ciphertext is replaced, so only growth counts against the vault totals.
				if err := vault.CheckMaterialHeadroom(totals, 0, int64(len(material.Ciphertext))-int64(len(live.Ciphertext)), int64(len(material.Ciphertext))); err != nil {
					return runEnvHeadroomConflict(runEnvCodeHistoryFull, err)
				}
				if n, err := q.DeleteRunEnvVarVersion(ctx, store.DeleteRunEnvVarVersionParams{ID: live.ID, UserID: userID}); err != nil || n != 1 {
					if err == nil {
						err = errRunEnvChanged
					}
					return err
				}
			}
			params.VarID, params.Rev, params.DisabledAt = live.VarID, live.Rev+1, live.DisabledAt
		}
		inserted, err := q.InsertRunEnvVar(ctx, params)
		if err != nil {
			return err
		}
		saved = runEnvRow(inserted)
		return nil
	})
	if err != nil {
		h.writeRunEnvTxError(w, err, "write run env var")
		return
	}
	dto := h.runEnvWriteDTO(r.Context(), userID, repoID, saved)
	status := http.StatusOK
	if !exists {
		status = http.StatusCreated
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, status, apitypes.RunEnvWriteDTO{Variable: dto})
}

// putRunEnvDescription is the description-only save: the live row changes in place and
// no new version is written. It needs no vault capacity, so it works while locked.
func (h *Handler) putRunEnvDescription(ctx context.Context, w http.ResponseWriter, userID, repoID uuid.UUID, cur store.RunEnvVar, description string) {
	row := runEnvRow{
		ID: cur.ID, VarID: cur.VarID, Name: cur.Name, Secret: cur.Secret, Description: cur.Description,
		Ciphertext: cur.Ciphertext, SealedWith: cur.SealedWith, ValueBytes: cur.ValueBytes, Rev: cur.Rev,
		DisabledAt: cur.DisabledAt, PendingDeleteAt: cur.PendingDeleteAt, CreatedAt: cur.CreatedAt, UpdatedAt: cur.UpdatedAt,
	}
	if description != cur.Description {
		var err error
		row, err = h.mutateLiveRunEnv(ctx, userID, repoID, cur.Name, cur, func(q *store.Queries, live store.RunEnvVar) (runEnvRow, error) {
			updated, err := q.UpdateRunEnvVarDescription(ctx, store.UpdateRunEnvVarDescriptionParams{ID: live.ID, UserID: userID, Description: description})
			return runEnvRow(updated), err
		})
		if err != nil {
			h.writeRunEnvTxError(w, err, "update run env description")
			return
		}
	}
	dto := h.runEnvWriteDTO(ctx, userID, repoID, row)
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, apitypes.RunEnvWriteDTO{Variable: dto})
}

// mutateLiveRunEnv runs a metadata-only change on the live row of a name in a locked
// transaction: the advisory lock, then the users-row fence (without a seal snapshot,
// since no ciphertext is written). expect, when set, is the row the caller planned
// against; a moved row is errRunEnvChanged. A missing row is pgx.ErrNoRows.
func (h *Handler) mutateLiveRunEnv(ctx context.Context, userID, repoID uuid.UUID, name string, expect store.RunEnvVar, fn func(*store.Queries, store.RunEnvVar) (runEnvRow, error)) (runEnvRow, error) {
	var out runEnvRow
	err := h.withSecretLock(ctx, userID, nil, func(q *store.Queries) error {
		if _, err := q.LockUserVaultState(ctx, userID); err != nil {
			return err
		}
		if err := lockRunEnvRepo(ctx, q, repoID); err != nil {
			return err
		}
		live, err := q.GetLiveRunEnvVarByNameForUpdate(ctx, store.GetLiveRunEnvVarByNameForUpdateParams{UserID: userID, RepoID: repoPG(repoID), Name: name})
		if err != nil {
			return err
		}
		if expect.ID != uuid.Nil && (live.ID != expect.ID || live.Rev != expect.Rev) {
			return errRunEnvChanged
		}
		out, err = fn(q, live)
		return err
	})
	return out, err
}

// SetRepoEnvVarDisabled is POST /api/repos/{id}/env/{name}/disable and .../enable:
// metadata-only on the live version (no new version, no history or vault capacity, no
// unlock needed). Idempotent.
func (h *Handler) setRepoEnvVarDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	userID, repoID, ok := h.runEnvOwner(w, r)
	if !ok {
		return
	}
	name := chi.URLParam(r, "name")
	// PRD 4416: disabling must always cut a compromised or newly denied variable
	// off, so it checks only the grammar; enabling keeps the full denylist.
	validate := runenv.ValidateName
	if disabled {
		validate = runenv.ValidateNameShape
	}
	if err := validate(name); err != nil {
		writeRunEnvRule(w, err)
		return
	}
	row, err := h.mutateLiveRunEnv(r.Context(), userID, repoID, name, store.RunEnvVar{}, func(q *store.Queries, live store.RunEnvVar) (runEnvRow, error) {
		updated, err := q.SetRunEnvVarDisabled(r.Context(), store.SetRunEnvVarDisabledParams{ID: live.ID, UserID: userID, Disabled: disabled})
		return runEnvRow(updated), err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, http.StatusNotFound, "variable not found")
		return
	}
	if err != nil {
		h.writeRunEnvTxError(w, err, "disable run env var")
		return
	}
	dto := h.runEnvWriteDTO(r.Context(), userID, repoID, row)
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, apitypes.RunEnvWriteDTO{Variable: dto})
}

// DisableRepoEnvVar is POST /api/repos/{id}/env/{name}/disable.
func (h *Handler) DisableRepoEnvVar(w http.ResponseWriter, r *http.Request) {
	h.setRepoEnvVarDisabled(w, r, true)
}

// EnableRepoEnvVar is POST /api/repos/{id}/env/{name}/enable.
func (h *Handler) EnableRepoEnvVar(w http.ResponseWriter, r *http.Request) {
	h.setRepoEnvVarDisabled(w, r, false)
}

// DeleteRepoEnvVar is DELETE /api/repos/{id}/env/{name}. A variable no run holds a
// receipt for is dropped at once (200). A secret some run holds a receipt for is kept
// sealed as pending deletion so that run can still mask it (202); its name is free
// again, it still counts toward the per-scope limit, and it is never refused for a
// budget reason. Neither answer revokes the credential at its provider.
func (h *Handler) DeleteRepoEnvVar(w http.ResponseWriter, r *http.Request) {
	userID, repoID, ok := h.runEnvOwner(w, r)
	if !ok {
		return
	}
	name := chi.URLParam(r, "name")
	// PRD 4416: delete, like disable, must work for a name denied after it was saved.
	if err := runenv.ValidateNameShape(name); err != nil {
		writeRunEnvRule(w, err)
		return
	}
	state := apitypes.RunEnvStateDisabled // overwritten below
	_, err := h.mutateLiveRunEnv(r.Context(), userID, repoID, name, store.RunEnvVar{}, func(q *store.Queries, live store.RunEnvVar) (runEnvRow, error) {
		retain := false
		if live.Secret {
			var err error
			if retain, err = h.receiptHeld(r.Context(), q, live.VarID, live.Rev); err != nil {
				return runEnvRow{}, err
			}
		}
		if retain {
			if n, err := q.MarkRunEnvVarPendingDeletion(r.Context(), store.MarkRunEnvVarPendingDeletionParams{ID: live.ID, UserID: userID}); err != nil || n != 1 {
				if err == nil {
					err = errRunEnvChanged
				}
				return runEnvRow{}, err
			}
			state = apitypes.RunEnvStatePendingDeletion
			return runEnvRow{}, nil
		}
		if n, err := q.DeleteRunEnvVarVersion(r.Context(), store.DeleteRunEnvVarVersionParams{ID: live.ID, UserID: userID}); err != nil || n != 1 {
			if err == nil {
				err = errRunEnvChanged
			}
			return runEnvRow{}, err
		}
		state = "deleted"
		return runEnvRow{}, nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, http.StatusNotFound, "variable not found")
		return
	}
	if err != nil {
		h.writeRunEnvTxError(w, err, "delete run env var")
		return
	}
	status := http.StatusOK
	if state == apitypes.RunEnvStatePendingDeletion {
		status = http.StatusAccepted
	}
	httpx.JSON(w, status, apitypes.RunEnvDeleteDTO{State: state})
}

// envVarCountsForUser maps each of the user's repos to its live variable count for the
// repo DTO. Enrichment: a store failure logs and leaves the counts at zero.
func (h *Handler) envVarCountsForUser(ctx context.Context, userID uuid.UUID, logPrefix string) map[uuid.UUID]int {
	out := map[uuid.UUID]int{}
	rows, err := h.q.ListRunEnvVarCountsForUser(ctx, userID)
	if err != nil {
		slog.Warn(logPrefix+": env var counts", "error", err)
		return out
	}
	for _, r := range rows {
		out[r.RepoID] = int(r.VarCount)
	}
	return out
}

// envVarCountForRepo is the single-repo form for responses that carry one RepoDTO.
func (h *Handler) envVarCountForRepo(ctx context.Context, userID, repoID uuid.UUID) int {
	n, err := h.q.CountRunEnvVarsForRepo(ctx, store.CountRunEnvVarsForRepoParams{UserID: userID, RepoID: repoPG(repoID)})
	if err != nil {
		slog.Warn("repo response: env var count", "error", err)
		return 0
	}
	return int(n)
}
