package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1247 M6: the schedule per-run credential override, end-to-end through the handlers
// against a REAL Postgres. These exercise what the fake tier cannot: the override columns'
// persistence, the presence-aware seed-and-keep vs validate/write split (blocker 2), the
// kind-scoped owner-scoped secret lookup, the typed-error → HTTP-status mapping, and the
// clone/add-repo/reset column plumbing. They reuse newScheduleFixture (schedules_livedb_test.go).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix. A package printing `ok` with
// PASS=0 is INVALID, not green.

// seedOwnedAnthropicTokenSched inserts one anthropic_token user_secret owned by userID and
// returns its id. Only the metadata (kind + label) is read by the validator and the label
// enrichment; the ciphertext is a placeholder (never opened here).
func (f scheduleFixture) seedOwnedAnthropicToken(ctx context.Context, t *testing.T, userID uuid.UUID, label string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	mustExecT(ctx, t, f.pool,
		`INSERT INTO user_secrets (id, user_id, kind, label, ciphertext, sealed_with)
		 VALUES ($1, $2, 'anthropic_token', $3, $4, 'master')`,
		id, userID, label, []byte("x"))
	return id
}

// seedUsableCodexDefault gives userID a REAL, usable Codex credential (a static
// openai_api_key default) so a raw default_harness='codex' column actually resolves Codex
// under D11 (PRD #1429 M2, D5) — merely setting the column is no longer sufficient once
// scheduleEffectiveHarness reads a genuine D11 resolution instead of the retired raw guesser.
func (f scheduleFixture) seedUsableCodexDefault(ctx context.Context, t *testing.T, userID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	mustExecT(ctx, t, f.pool,
		`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
		 VALUES ($1, $2, 'openai_api_key', $3, true, $4, 'master')`,
		id, userID, "codex-key-"+uuid.NewString(), []byte("x"))
	if _, err := f.h.q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{
		UserSecretID: id,
		UserID:       userID,
		Status:       "static",
	}); err != nil {
		t.Fatalf("insert codex credential state: %v", err)
	}
	return id
}

// patchScheduleRaw PATCHes /api/schedules/{id} with a raw body and returns the recorder.
func (f scheduleFixture) patchScheduleRaw(t *testing.T, user, id uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := userReq(http.MethodPatch, "/api/schedules/"+id.String(), body, user, map[string]string{"id": id.String()})
	rec := httptest.NewRecorder()
	f.h.PatchSchedule(rec, req)
	return rec
}

// scheduleOverrideColumns reads the two override columns off a run_schedules row.
func (f scheduleFixture) scheduleOverrideColumns(ctx context.Context, t *testing.T, id string) (mode *string, secret *uuid.UUID) {
	t.Helper()
	if err := f.pool.QueryRow(ctx,
		`SELECT credential_override_mode, credential_override_secret_id FROM run_schedules WHERE id = $1`,
		id).Scan(&mode, &secret); err != nil {
		t.Fatalf("read override columns: %v", err)
	}
	return mode, secret
}

// TestCreateScheduleCredentialOverridePersistsLiveDB: a create carrying a pinned override
// persists the two columns and the response DTO carries mode + the resolved label.
func TestCreateScheduleCredentialOverridePersistsLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)
	secretID := f.seedOwnedAnthropicToken(ctx, t, f.owner.ID, "prod-pin")

	dto, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"weekly","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC","credential_override":{"mode":"pinned","secret_id":"`+secretID.String()+`"}}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	if dto.CredentialOverride == nil || dto.CredentialOverride.Mode != "pinned" {
		t.Fatalf("dto override = %+v, want mode pinned", dto.CredentialOverride)
	}
	if dto.CredentialOverride.Label == nil || *dto.CredentialOverride.Label != "prod-pin" {
		t.Fatalf("dto override label = %v, want prod-pin (the resolved token label)", dto.CredentialOverride.Label)
	}
	mode, secret := f.scheduleOverrideColumns(ctx, t, dto.ID)
	if mode == nil || *mode != "pinned" || secret == nil || *secret != secretID {
		t.Fatalf("persisted columns = (%v,%v), want (pinned,%s)", mode, secret, secretID)
	}
}

// TestCreateScheduleCredentialOverrideAbsentIsNullLiveDB: a create with no credential_override
// leaves both columns NULL (inherit), byte-identical to a pre-#1247 schedule.
func TestCreateScheduleCredentialOverrideAbsentIsNullLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)
	dto, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"weekly","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	if dto.CredentialOverride != nil {
		t.Fatalf("dto override = %+v, want nil (inherit)", dto.CredentialOverride)
	}
	mode, secret := f.scheduleOverrideColumns(ctx, t, dto.ID)
	if mode != nil || secret != nil {
		t.Fatalf("persisted columns = (%v,%v), want both NULL", mode, secret)
	}
}

// TestCreateScheduleCredentialOverride404LiveDB: a pinned override naming a non-owned/absent
// secret id is a 404 (the kind-scoped owner-scoped lookup misses).
func TestCreateScheduleCredentialOverride404LiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)
	foreign := uuid.New()
	_, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"x","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC","credential_override":{"mode":"pinned","secret_id":"`+foreign.String()+`"}}`)
	if code != http.StatusNotFound {
		t.Fatalf("create with foreign secret status = %d, want 404", code)
	}
}

// TestCreateScheduleCredentialOverride400LiveDB: a malformed secret_id uuid, and a mode outside
// the closed set, are each a 400.
func TestCreateScheduleCredentialOverride400LiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)
	base := `{"target":"prompt","prompt":"x","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC",`
	if _, code := f.createSchedule(t, f.owner.ID, f.repoID, base+`"credential_override":{"mode":"pinned","secret_id":"not-a-uuid"}}`); code != http.StatusBadRequest {
		t.Fatalf("malformed secret_id status = %d, want 400", code)
	}
	if _, code := f.createSchedule(t, f.owner.ID, f.repoID, base+`"credential_override":{"mode":"bogus"}}`); code != http.StatusBadRequest {
		t.Fatalf("bad mode status = %d, want 400", code)
	}
	// A pinned mode with no secret id is a 400.
	if _, code := f.createSchedule(t, f.owner.ID, f.repoID, base+`"credential_override":{"mode":"pinned"}}`); code != http.StatusBadRequest {
		t.Fatalf("pinned with no secret status = %d, want 400", code)
	}
}

// TestCreateScheduleCredentialOverride422CodexLiveDB: when the owner's default_harness is codex,
// a create carrying any override is a 422 (D9 — an Anthropic override cannot ride a codex run).
func TestCreateScheduleCredentialOverride422CodexLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)
	mustExecT(ctx, t, f.pool, `UPDATE users SET default_harness = 'codex' WHERE id = $1`, f.owner.ID)
	// PRD #1429 M2 (D5): scheduleEffectiveHarness now resolves a REAL D11 harness rather than
	// reading the raw default_harness column, so the pin must actually be usable to reach 422
	// (an unusable Codex default falls through to Claude and accepts the override instead).
	f.seedUsableCodexDefault(ctx, t, f.owner.ID)
	_, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"x","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC","credential_override":{"mode":"auto"}}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("create with codex default harness status = %d, want 422", code)
	}
}

// TestScheduleOmittedRetimeSelfImproveSucceedsLiveDB is blocker 2 (a): an absent-override
// retime of a default self_improve schedule SUCCEEDS — the validator is skipped (which would
// 409 the self_improve lane), and the stored override columns are untouched.
func TestScheduleOmittedRetimeSelfImproveSucceedsLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)
	si, code := f.enableCatalog(t, f.owner.ID, f.repoID, "self-improve")
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("enable self-improve status = %d, want 201/200", code)
	}
	id, _ := uuid.Parse(si.ID)
	rec := f.patchScheduleRaw(t, f.owner.ID, id, `{"cron_expr":"0 5 */2 * *"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("omitted-override retime of a self_improve schedule status = %d, want 200 (validator must be skipped) — body %s", rec.Code, rec.Body.String())
	}
	mode, secret := f.scheduleOverrideColumns(ctx, t, si.ID)
	if mode != nil || secret != nil {
		t.Fatalf("columns after retime = (%v,%v), want both NULL (untouched)", mode, secret)
	}
}

// TestScheduleExplicitOverrideSelfImprove409LiveDB is blocker 2 (b): every EXPLICIT override
// mode — inherit, auto, default, and pinned — on a self_improve schedule is a 409, because the
// validator refuses the self_improve lane BEFORE the mode switch (D10). Critically, even an
// explicit inherit 409s (it is present, so it is validated).
func TestScheduleExplicitOverrideSelfImprove409LiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)
	si, code := f.enableCatalog(t, f.owner.ID, f.repoID, "self-improve")
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("enable self-improve status = %d, want 201/200", code)
	}
	id, _ := uuid.Parse(si.ID)
	secretID := f.seedOwnedAnthropicToken(ctx, t, f.owner.ID, "si-pin")
	bodies := map[string]string{
		"inherit": `{"credential_override":{"mode":"inherit"}}`,
		"auto":    `{"credential_override":{"mode":"auto"}}`,
		"default": `{"credential_override":{"mode":"default"}}`,
		"pinned":  `{"credential_override":{"mode":"pinned","secret_id":"` + secretID.String() + `"}}`,
	}
	for mode, body := range bodies {
		rec := f.patchScheduleRaw(t, f.owner.ID, id, body)
		if rec.Code != http.StatusConflict {
			t.Fatalf("explicit %s override on self_improve status = %d, want 409 — body %s", mode, rec.Code, rec.Body.String())
		}
	}
}

// TestScheduleOmittedRetimeKeepsStoredOverrideLiveDB: a retime that omits credential_override on
// a NON-self_improve user schedule leaves the stored override columns unchanged (seed-and-keep).
func TestScheduleOmittedRetimeKeepsStoredOverrideLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)
	secretID := f.seedOwnedAnthropicToken(ctx, t, f.owner.ID, "keep-pin")
	dto, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"x","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC","credential_override":{"mode":"pinned","secret_id":"`+secretID.String()+`"}}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	id, _ := uuid.Parse(dto.ID)
	rec := f.patchScheduleRaw(t, f.owner.ID, id, `{"cron_expr":"0 6 * * *"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("retime status = %d, want 200 — body %s", rec.Code, rec.Body.String())
	}
	mode, secret := f.scheduleOverrideColumns(ctx, t, dto.ID)
	if mode == nil || *mode != "pinned" || secret == nil || *secret != secretID {
		t.Fatalf("columns after retime = (%v,%v), want the stored (pinned,%s) preserved", mode, secret, secretID)
	}
	// A PRESENT explicit inherit, by contrast, clears it (switchable lane).
	rec = f.patchScheduleRaw(t, f.owner.ID, id, `{"credential_override":{"mode":"inherit"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("explicit inherit clear status = %d, want 200 — body %s", rec.Code, rec.Body.String())
	}
	mode, secret = f.scheduleOverrideColumns(ctx, t, dto.ID)
	if mode != nil || secret != nil {
		t.Fatalf("columns after explicit inherit = (%v,%v), want both NULL (cleared)", mode, secret)
	}
}

// TestDefaultScheduleCredentialOverridePersistAndResetLiveDB: a default-origin PATCH persists
// the override and flips customized; reset clears it back to inherit and un-customizes.
func TestDefaultScheduleCredentialOverridePersistAndResetLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)
	def, code := f.enableCatalog(t, f.owner.ID, f.repoID, "docs-hygiene")
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("enable docs-hygiene status = %d, want 201/200", code)
	}
	id, _ := uuid.Parse(def.ID)
	rec := f.patchScheduleRaw(t, f.owner.ID, id, `{"credential_override":{"mode":"auto"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("default override PATCH status = %d, want 200 — body %s", rec.Code, rec.Body.String())
	}
	var patched apitypes.ScheduleDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &patched); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if patched.CredentialOverride == nil || patched.CredentialOverride.Mode != "auto" {
		t.Fatalf("patched override = %+v, want mode auto", patched.CredentialOverride)
	}
	if !patched.Customized {
		t.Fatalf("a default carrying an override must read customized=true")
	}
	mode, _ := f.scheduleOverrideColumns(ctx, t, def.ID)
	if mode == nil || *mode != "auto" {
		t.Fatalf("persisted mode = %v, want auto", mode)
	}
	// Reset clears the override and un-customizes.
	resetReq := userReq(http.MethodPost, "/api/schedules/"+def.ID+"/reset", "", f.owner.ID, map[string]string{"id": def.ID})
	resetRec := httptest.NewRecorder()
	f.h.ResetSchedule(resetRec, resetReq)
	if resetRec.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want 200 — body %s", resetRec.Code, resetRec.Body.String())
	}
	var reset apitypes.ScheduleDTO
	if err := json.Unmarshal(resetRec.Body.Bytes(), &reset); err != nil {
		t.Fatalf("decode reset: %v", err)
	}
	if reset.CredentialOverride != nil {
		t.Fatalf("after reset override = %+v, want nil (inherit)", reset.CredentialOverride)
	}
	if reset.Customized {
		t.Fatalf("after reset customized = true, want false")
	}
	mode, secret := f.scheduleOverrideColumns(ctx, t, def.ID)
	if mode != nil || secret != nil {
		t.Fatalf("columns after reset = (%v,%v), want both NULL", mode, secret)
	}
}

// TestScheduleCloneCredentialOverridePreservedLiveDB: cloning a schedule carrying an override
// copies both columns onto the clone (a clone never silently drops the override).
func TestScheduleCloneCredentialOverridePreservedLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)
	secretID := f.seedOwnedAnthropicToken(ctx, t, f.owner.ID, "clone-pin")
	src, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"x","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC","credential_override":{"mode":"pinned","secret_id":"`+secretID.String()+`"}}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	cloneReq := userReq(http.MethodPost, "/api/schedules/"+src.ID+"/clone", "", f.owner.ID, map[string]string{"id": src.ID})
	cloneRec := httptest.NewRecorder()
	f.h.CloneSchedule(cloneRec, cloneReq)
	if cloneRec.Code != http.StatusCreated {
		t.Fatalf("clone status = %d, want 201 — body %s", cloneRec.Code, cloneRec.Body.String())
	}
	var clone apitypes.ScheduleDTO
	if err := json.Unmarshal(cloneRec.Body.Bytes(), &clone); err != nil {
		t.Fatalf("decode clone: %v", err)
	}
	if clone.CredentialOverride == nil || clone.CredentialOverride.Mode != "pinned" {
		t.Fatalf("clone override = %+v, want mode pinned", clone.CredentialOverride)
	}
	if clone.CredentialOverride.Label == nil || *clone.CredentialOverride.Label != "clone-pin" {
		t.Fatalf("clone override label = %v, want the resolved clone-pin", clone.CredentialOverride.Label)
	}
	mode, secret := f.scheduleOverrideColumns(ctx, t, clone.ID)
	if mode == nil || *mode != "pinned" || secret == nil || *secret != secretID {
		t.Fatalf("clone columns = (%v,%v), want the source (pinned,%s)", mode, secret, secretID)
	}
}

// TestScheduleAddRepoCredentialOverridePreservedLiveDB: replicating a schedule onto another repo
// (add-repo) copies the override onto the new sibling.
func TestScheduleAddRepoCredentialOverridePreservedLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)
	secretID := f.seedOwnedAnthropicToken(ctx, t, f.owner.ID, "sibling-pin")
	src, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"x","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC","credential_override":{"mode":"pinned","secret_id":"`+secretID.String()+`"}}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	repoB := f.insertRepo(ctx, t, f.owner, 2, "g/sched-b")
	body := `{"repo_id":"` + repoB.String() + `"}`
	addReq := userReq(http.MethodPost, "/api/schedules/"+src.ID+"/add-repo", body, f.owner.ID, map[string]string{"id": src.ID})
	addRec := httptest.NewRecorder()
	f.h.AddScheduleRepo(addRec, addReq)
	if addRec.Code != http.StatusCreated {
		t.Fatalf("add-repo status = %d, want 201 — body %s", addRec.Code, addRec.Body.String())
	}
	var sibling apitypes.ScheduleDTO
	if err := json.Unmarshal(addRec.Body.Bytes(), &sibling); err != nil {
		t.Fatalf("decode sibling: %v", err)
	}
	if sibling.CredentialOverride == nil || sibling.CredentialOverride.Mode != "pinned" {
		t.Fatalf("sibling override = %+v, want mode pinned", sibling.CredentialOverride)
	}
	mode, secret := f.scheduleOverrideColumns(ctx, t, sibling.ID)
	if mode == nil || *mode != "pinned" || secret == nil || *secret != secretID {
		t.Fatalf("sibling columns = (%v,%v), want the source (pinned,%s)", mode, secret, secretID)
	}
}

// TestScheduleCredentialOverrideRoundTripStoreLiveDB proves the store layer round-trips the two
// columns through CreateRunSchedule and UpdateRunSchedule directly (the query-level contract the
// handler tests above build on).
func TestScheduleCredentialOverrideRoundTripStoreLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)
	secretID := f.seedOwnedAnthropicToken(ctx, t, f.owner.ID, "store-pin")

	future := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
	created, err := f.h.q.CreateRunSchedule(ctx, store.CreateRunScheduleParams{
		UserID:                     f.owner.ID,
		RepoID:                     f.repoID,
		Target:                     "prompt",
		Prompt:                     pgtype.Text{String: "weekly", Valid: true},
		Timing:                     "recurring",
		CronExpr:                   pgtype.Text{String: "0 2 * * *", Valid: true},
		Timezone:                   "UTC",
		NextFireAt:                 future,
		AutoApprove:                true,
		WaitOnLimit:                true,
		Enabled:                    true,
		CredentialOverrideMode:     pgtype.Text{String: "pinned", Valid: true},
		CredentialOverrideSecretID: pgtype.UUID{Bytes: secretID, Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateRunSchedule: %v", err)
	}
	if !created.CredentialOverrideMode.Valid || created.CredentialOverrideMode.String != "pinned" ||
		!created.CredentialOverrideSecretID.Valid || uuid.UUID(created.CredentialOverrideSecretID.Bytes) != secretID {
		t.Fatalf("created columns = (%+v,%+v), want (pinned,%s)", created.CredentialOverrideMode, created.CredentialOverrideSecretID, secretID)
	}

	// UpdateRunSchedule clearing to inherit (both NULL).
	updated, err := f.h.q.UpdateRunSchedule(ctx, store.UpdateRunScheduleParams{
		Target:      "prompt",
		RepoID:      f.repoID,
		Prompt:      pgtype.Text{String: "weekly", Valid: true},
		Timing:      "recurring",
		CronExpr:    pgtype.Text{String: "0 2 * * *", Valid: true},
		Timezone:    "UTC",
		NextFireAt:  future,
		AutoApprove: true,
		WaitOnLimit: true,
		ID:          created.ID,
		UserID:      f.owner.ID,
		// CredentialOverrideMode / SecretID left zero-value → NULL (inherit).
	})
	if err != nil {
		t.Fatalf("UpdateRunSchedule: %v", err)
	}
	if updated.CredentialOverrideMode.Valid || updated.CredentialOverrideSecretID.Valid {
		t.Fatalf("updated columns = (%+v,%+v), want both NULL after a clear", updated.CredentialOverrideMode, updated.CredentialOverrideSecretID)
	}
}
