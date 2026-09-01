package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/schedsvc"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Schedule CRUD (PRD #241 M4) end-to-end through the handlers, against a real Postgres.
//
// Handler.q is a concrete *store.Queries, so this exercises the real owner-scoping in
// the queries (a foreign id is a 404, not a leak) as well as the handler wiring.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.

type scheduleFixture struct {
	h        *Handler
	pool     *pgxpool.Pool
	owner    store.User
	stranger store.User
	repoID   uuid.UUID
}

func newScheduleFixture(ctx context.Context, t *testing.T) scheduleFixture {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)
	box := newHandlerTestBox(t)

	f := scheduleFixture{
		pool:     pool,
		owner:    store.User{ID: uuid.New(), Email: fmt.Sprintf("sched-owner-%s@e2e", uuid.NewString()[:8]), IsActive: true},
		stranger: store.User{ID: uuid.New(), Email: fmt.Sprintf("sched-other-%s@e2e", uuid.NewString()[:8]), IsActive: true},
	}
	svc := forgesvc.New(q, box, 5*time.Second, nil)
	wsvc := workersvc.New(q, box, workersvc.Params{})
	set := settings.New(&settingsStore{rows: []store.AppSetting{
		{Key: settings.KeyUziLabel, Value: "uzi"},
	}}, time.Minute)
	f.h = &Handler{
		pool:      pool,
		q:         q,
		box:       box,
		cfg:       config.Config{},
		settings:  set,
		svc:       svc,
		wsvc:      wsvc,
		scheduler: schedsvc.New(q, wsvc, svc, set, nil, nil, 0, slog.Default()),
	}

	sealed, err := box.Seal([]byte("glpat-dummy-token"))
	if err != nil {
		t.Fatalf("seal token: %v", err)
	}
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.owner.ID, f.owner.Email)
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.stranger.ID, f.stranger.Email)

	connID, repoID := uuid.New(), uuid.New()
	mustExecT(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.example', 'bot', 1, $3)`, connID, f.owner.ID, sealed)
	mustExecT(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/sched', 'https://forge.example/g/sched', 'main', true)`, repoID, connID)
	f.repoID = repoID
	return f
}

// insertRepo adds a forge connection + repo owned by `user` and returns the repo id.
// Mirrors newScheduleFixture's inserts (lines 87-93) so the repoint tests have a second
// owned repo and a stranger-owned repo to move to / be rejected from. forgeProjectID must
// not collide with the fixture's existing repo (project id 1), so callers pass a distinct
// one per repo.
func (f scheduleFixture) insertRepo(ctx context.Context, t *testing.T, user store.User, forgeProjectID int, path string) uuid.UUID {
	t.Helper()
	sealed, err := f.h.box.Seal([]byte("glpat-dummy-token"))
	if err != nil {
		t.Fatalf("seal token: %v", err)
	}
	connID, repoID := uuid.New(), uuid.New()
	// base_url is distinct per forgeProjectID: forge_connections has a UNIQUE(user_id,
	// forge_type, base_url), so a second gitlab connection for the SAME user (a second
	// owned repo) must not reuse newScheduleFixture's 'https://forge.example'. Build the
	// URL in Go rather than concatenating $3 in SQL (pgx cannot infer an int into a text
	// concat).
	baseURL := fmt.Sprintf("https://forge-%d.example", forgeProjectID)
	mustExecT(ctx, t, f.pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', $3, 'bot', 1, $4)`, connID, user.ID, baseURL, sealed)
	mustExecT(ctx, t, f.pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, $3, $4, 'https://forge.example/'||$4, 'main', true)`, repoID, connID, forgeProjectID, path)
	return repoID
}

// createSchedule POSTs to /api/repos/{id}/schedules and returns the decoded DTO.
func (f scheduleFixture) createSchedule(t *testing.T, user uuid.UUID, repoID uuid.UUID, body string) (apitypes.ScheduleDTO, int) {
	t.Helper()
	req := userReq(http.MethodPost, "/api/repos/"+repoID.String()+"/schedules", body, user, map[string]string{"id": repoID.String()})
	rec := httptest.NewRecorder()
	f.h.CreateSchedule(rec, req)
	var dto apitypes.ScheduleDTO
	if rec.Code == http.StatusCreated {
		if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
			t.Fatalf("decode create response: %v (body %s)", err, rec.Body.String())
		}
	}
	return dto, rec.Code
}

func TestScheduleCRUDRoundTripLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	// Create a recurring issue schedule.
	dto, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"issue","issue_iid":7,"timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	if dto.ID == "" || dto.Target != "issue" || dto.IssueIID == nil || *dto.IssueIID != 7 {
		t.Fatalf("create dto = %+v, want issue/7", dto)
	}
	// PRD #274 Decision 1a: wait_on_limit now defaults ON at create time (a schedule is
	// unattended, so a fired run parks on the usage limit rather than dying).
	if !dto.AutoApprove || !dto.WaitOnLimit || !dto.Enabled {
		t.Fatalf("create defaults = auto:%v wait:%v enabled:%v, want true/true/true", dto.AutoApprove, dto.WaitOnLimit, dto.Enabled)
	}
	if dto.NextFireAt == nil {
		t.Fatalf("recurring schedule should have a next_fire_at")
	}
	if len(dto.NextFires) == 0 {
		t.Fatalf("recurring schedule should carry a next-fires preview")
	}
	if dto.RepoPath != "g/sched" {
		t.Fatalf("repo_path = %q, want g/sched", dto.RepoPath)
	}
	id := dto.ID

	// Get.
	getReq := userReq(http.MethodGet, "/api/schedules/"+id, "", f.owner.ID, map[string]string{"id": id})
	getRec := httptest.NewRecorder()
	f.h.GetSchedule(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200", getRec.Code)
	}

	// List.
	listReq := userReq(http.MethodGet, "/api/me/schedules", "", f.owner.ID, nil)
	listRec := httptest.NewRecorder()
	f.h.ListMySchedules(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200", listRec.Code)
	}
	var list []apitypes.ScheduleDTO
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 || list[0].ID != id {
		t.Fatalf("list = %+v, want exactly the created schedule", list)
	}

	// Patch: toggle enabled off (only-enabled path).
	patchReq := userReq(http.MethodPatch, "/api/schedules/"+id, `{"enabled":false}`, f.owner.ID, map[string]string{"id": id})
	patchRec := httptest.NewRecorder()
	f.h.PatchSchedule(patchRec, patchReq)
	if patchRec.Code != http.StatusOK {
		t.Fatalf("patch status = %d, want 200", patchRec.Code)
	}
	var patched apitypes.ScheduleDTO
	if err := json.Unmarshal(patchRec.Body.Bytes(), &patched); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if patched.Enabled {
		t.Fatalf("after disable patch, enabled = true, want false")
	}

	// Patch: change the cron (config path), confirm next_fire recomputed and enabled preserved.
	cfgReq := userReq(http.MethodPatch, "/api/schedules/"+id, `{"cron_expr":"0 9 * * 1"}`, f.owner.ID, map[string]string{"id": id})
	cfgRec := httptest.NewRecorder()
	f.h.PatchSchedule(cfgRec, cfgReq)
	if cfgRec.Code != http.StatusOK {
		t.Fatalf("config patch status = %d, want 200", cfgRec.Code)
	}
	var reconf apitypes.ScheduleDTO
	if err := json.Unmarshal(cfgRec.Body.Bytes(), &reconf); err != nil {
		t.Fatalf("decode config patch: %v", err)
	}
	if reconf.CronExpr != "0 9 * * 1" {
		t.Fatalf("cron after patch = %q, want the new value", reconf.CronExpr)
	}
	if reconf.Enabled {
		t.Fatalf("a config-only patch must not re-enable a disabled schedule")
	}

	// Delete → 204, then Get → 404.
	delReq := userReq(http.MethodDelete, "/api/schedules/"+id, "", f.owner.ID, map[string]string{"id": id})
	delRec := httptest.NewRecorder()
	f.h.DeleteSchedule(delRec, delReq)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", delRec.Code)
	}
	gone := userReq(http.MethodGet, "/api/schedules/"+id, "", f.owner.ID, map[string]string{"id": id})
	goneRec := httptest.NewRecorder()
	f.h.GetSchedule(goneRec, gone)
	if goneRec.Code != http.StatusNotFound {
		t.Fatalf("get after delete status = %d, want 404", goneRec.Code)
	}
}

// TestScheduleMaxIssuesRoundTripLiveDB exercises the PRD #274 M2 sweep cap through the
// real store column (pgtype.Int4): a new sweep defaults to 10, a config PATCH that resends
// the cap persists it, and a config PATCH carrying an explicit null CLEARS it to unlimited
// (the deliberate replace-semantics — a seed-and-keep would make unlimited unreachable).
func TestScheduleMaxIssuesRoundTripLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	// Create a sweep with no explicit cap → server default 10.
	dto, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"sweep","labels":["bug"],"timing":"recurring","cron_expr":"0 9 * * 1"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	if dto.MaxIssues == nil || *dto.MaxIssues != 10 {
		t.Fatalf("new sweep max_issues = %v, want the default 10", dto.MaxIssues)
	}
	id := dto.ID

	patch := func(body string) apitypes.ScheduleDTO {
		req := userReq(http.MethodPatch, "/api/schedules/"+id, body, f.owner.ID, map[string]string{"id": id})
		rec := httptest.NewRecorder()
		f.h.PatchSchedule(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("patch %q status = %d, want 200 (body %s)", body, rec.Code, rec.Body.String())
		}
		var out apitypes.ScheduleDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode patch: %v", err)
		}
		return out
	}

	// A config PATCH resending the cap persists the new value.
	if got := patch(`{"target":"sweep","labels":["bug"],"timing":"recurring","cron_expr":"0 9 * * 1","max_issues":3}`); got.MaxIssues == nil || *got.MaxIssues != 3 {
		t.Fatalf("after set patch, max_issues = %v, want 3", got.MaxIssues)
	}

	// A config PATCH carrying an explicit null clears the cap to unlimited (NULL column).
	if got := patch(`{"target":"sweep","labels":["bug"],"timing":"recurring","cron_expr":"0 9 * * 1","max_issues":null}`); got.MaxIssues != nil {
		t.Fatalf("after clear patch, max_issues = %v, want nil (unlimited)", got.MaxIssues)
	}
}

// TestScheduleGuidanceRoundTripLiveDB exercises the PRD #274 M3 guidance field through the
// real store column (pgtype.Text): a create persists guidance, a config PATCH that resends
// it keeps it, a config PATCH omitting it CLEARS it to NULL (replace-semantics), and a blank
// value normalizes to NULL rather than storing whitespace.
func TestScheduleGuidanceRoundTripLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	// Create an issue schedule carrying guidance.
	dto, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"issue","issue_iid":7,"timing":"recurring","cron_expr":"0 9 * * 1","guidance":"add a failing test first"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	if dto.Guidance == nil || *dto.Guidance != "add a failing test first" {
		t.Fatalf("new issue guidance = %v, want the stored value", dto.Guidance)
	}
	id := dto.ID

	patch := func(body string) apitypes.ScheduleDTO {
		req := userReq(http.MethodPatch, "/api/schedules/"+id, body, f.owner.ID, map[string]string{"id": id})
		rec := httptest.NewRecorder()
		f.h.PatchSchedule(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("patch %q status = %d, want 200 (body %s)", body, rec.Code, rec.Body.String())
		}
		var out apitypes.ScheduleDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode patch: %v", err)
		}
		return out
	}

	// A config PATCH resending guidance persists the new value.
	if got := patch(`{"target":"issue","issue_iid":7,"timing":"recurring","cron_expr":"0 9 * * 1","guidance":"keep the diff small"}`); got.Guidance == nil || *got.Guidance != "keep the diff small" {
		t.Fatalf("after set patch, guidance = %v, want \"keep the diff small\"", got.Guidance)
	}

	// A config PATCH that omits guidance clears it (NULL column → nil DTO).
	if got := patch(`{"target":"issue","issue_iid":7,"timing":"recurring","cron_expr":"0 9 * * 1"}`); got.Guidance != nil {
		t.Fatalf("after omit patch, guidance = %v, want nil (cleared)", got.Guidance)
	}

	// A blank guidance value normalizes to NULL, not stored whitespace.
	if got := patch(`{"target":"issue","issue_iid":7,"timing":"recurring","cron_expr":"0 9 * * 1","guidance":"   "}`); got.Guidance != nil {
		t.Fatalf("blank guidance = %v, want nil (normalized to NULL)", got.Guidance)
	}
}

// TestScheduleModelRoundTripLiveDB exercises the PRD #300 per-schedule model override
// through the real store column (run_schedules.model, pgtype.Text). Unlike guidance
// (issue/sweep-only), model is an ALL-TARGETS field, so this uses a PROMPT schedule to
// prove the model persists exactly where guidance is rejected: a create persists the
// model, a config PATCH resending it replaces it, a config PATCH OMITTING it clears it to
// NULL (replace-semantics → inherit the owner default), and a malformed model is a 400
// from the real handler's agenttmpl.ValidateModel.
func TestScheduleModelRoundTripLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	// Create a prompt schedule carrying a model override.
	dto, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"do the thing","timing":"recurring","cron_expr":"0 9 * * 1","model":"fable"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	if dto.Model == nil || *dto.Model != "fable" {
		t.Fatalf("new prompt model = %v, want the stored value \"fable\"", dto.Model)
	}
	id := dto.ID

	// patch issues a config PATCH and returns the decoded DTO plus the HTTP code, so a
	// caller can assert either a successful replace or a rejection.
	patch := func(body string) (apitypes.ScheduleDTO, int) {
		req := userReq(http.MethodPatch, "/api/schedules/"+id, body, f.owner.ID, map[string]string{"id": id})
		rec := httptest.NewRecorder()
		f.h.PatchSchedule(rec, req)
		var out apitypes.ScheduleDTO
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode patch: %v", err)
			}
		}
		return out, rec.Code
	}

	// A config PATCH resending the full prompt config with a new model persists it.
	if got, gc := patch(`{"target":"prompt","prompt":"do the thing","timing":"recurring","cron_expr":"0 9 * * 1","model":"sonnet"}`); gc != http.StatusOK || got.Model == nil || *got.Model != "sonnet" {
		t.Fatalf("after set patch, code=%d model=%v, want 200 and \"sonnet\"", gc, got.Model)
	}

	// A config PATCH omitting model clears it to NULL (inherit — replace-semantics).
	if got, gc := patch(`{"target":"prompt","prompt":"do the thing","timing":"recurring","cron_expr":"0 9 * * 1"}`); gc != http.StatusOK || got.Model != nil {
		t.Fatalf("after omit patch, code=%d model=%v, want 200 and nil (cleared to inherit)", gc, got.Model)
	}

	// A malformed model is rejected by the real handler's ValidateModel (400), not stored.
	if _, gc := patch(`{"target":"prompt","prompt":"do the thing","timing":"recurring","cron_expr":"0 9 * * 1","model":"two words"}`); gc != http.StatusBadRequest {
		t.Fatalf("malformed model patch code = %d, want 400", gc)
	}
}

// TestScheduleMrReworkRoundTripLiveDB (PRD #841 M2) exercises the per-schedule mr_rework
// override through the real nullable store column (pgtype.Bool): a user schedule created
// with an explicit false persists it as a non-nil false, a config PATCH resending an
// explicit true replaces it, and a config PATCH omitting the field CLEARS it back to nil
// (inherit) — the tri-state replace-semantics (D5), where nil ≠ false. A fresh schedule
// created with no mr_rework field is nil (inherit the owner default live, D1).
func TestScheduleMrReworkRoundTripLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	// Create with no mr_rework field → nil (inherit).
	base, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"do the thing","timing":"recurring","cron_expr":"0 9 * * 1"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	if base.MrReworkEnabled != nil {
		t.Fatalf("fresh schedule mr_rework = %v, want nil (inherit)", base.MrReworkEnabled)
	}

	// Create with an explicit false override → persisted as a non-nil false.
	dto, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"do the thing","timing":"recurring","cron_expr":"0 9 * * 1","mr_rework_enabled":false}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	if dto.MrReworkEnabled == nil || *dto.MrReworkEnabled {
		t.Fatalf("new schedule mr_rework = %v, want a non-nil false", dto.MrReworkEnabled)
	}
	id := dto.ID

	patch := func(body string) (apitypes.ScheduleDTO, int) {
		req := userReq(http.MethodPatch, "/api/schedules/"+id, body, f.owner.ID, map[string]string{"id": id})
		rec := httptest.NewRecorder()
		f.h.PatchSchedule(rec, req)
		var out apitypes.ScheduleDTO
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode patch: %v", err)
			}
		}
		return out, rec.Code
	}

	// A config PATCH resending the full config with an explicit true replaces the override.
	if got, gc := patch(`{"target":"prompt","prompt":"do the thing","timing":"recurring","cron_expr":"0 9 * * 1","mr_rework_enabled":true}`); gc != http.StatusOK || got.MrReworkEnabled == nil || !*got.MrReworkEnabled {
		t.Fatalf("after set-true patch, code=%d mr_rework=%v, want 200 and non-nil true", gc, got.MrReworkEnabled)
	}

	// A config PATCH omitting mr_rework clears it to nil (inherit — replace-semantics, not
	// the false a plain bool would collapse to).
	if got, gc := patch(`{"target":"prompt","prompt":"do the thing","timing":"recurring","cron_expr":"0 9 * * 1"}`); gc != http.StatusOK || got.MrReworkEnabled != nil {
		t.Fatalf("after omit patch, code=%d mr_rework=%v, want 200 and nil (cleared to inherit)", gc, got.MrReworkEnabled)
	}
}

func TestScheduleOwnerIsolationLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	dto, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"sweep","labels":["bug"],"timing":"recurring","cron_expr":"0 9 * * 1"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	id := dto.ID

	// A second user must not see or mutate the owner's schedule.
	strangerGet := userReq(http.MethodGet, "/api/schedules/"+id, "", f.stranger.ID, map[string]string{"id": id})
	strangerRec := httptest.NewRecorder()
	f.h.GetSchedule(strangerRec, strangerGet)
	if strangerRec.Code != http.StatusNotFound {
		t.Fatalf("stranger get status = %d, want 404", strangerRec.Code)
	}

	strangerPatch := userReq(http.MethodPatch, "/api/schedules/"+id, `{"enabled":false}`, f.stranger.ID, map[string]string{"id": id})
	strangerPatchRec := httptest.NewRecorder()
	f.h.PatchSchedule(strangerPatchRec, strangerPatch)
	if strangerPatchRec.Code != http.StatusNotFound {
		t.Fatalf("stranger patch status = %d, want 404", strangerPatchRec.Code)
	}

	strangerDel := userReq(http.MethodDelete, "/api/schedules/"+id, "", f.stranger.ID, map[string]string{"id": id})
	strangerDelRec := httptest.NewRecorder()
	f.h.DeleteSchedule(strangerDelRec, strangerDel)
	if strangerDelRec.Code != http.StatusNotFound {
		t.Fatalf("stranger delete status = %d, want 404", strangerDelRec.Code)
	}

	// The stranger also cannot create a schedule on the owner's repo (repoForRequest 404).
	_, foreignCode := f.createSchedule(t, f.stranger.ID, f.repoID,
		`{"target":"issue","issue_iid":3,"timing":"recurring","cron_expr":"0 2 * * *"}`)
	if foreignCode != http.StatusNotFound {
		t.Fatalf("stranger create on owner repo status = %d, want 404", foreignCode)
	}
}

// patchSchedule PATCHes /api/schedules/{id} as `user` and returns the decoded DTO (only when
// the code is 200) plus the HTTP code, so a caller asserts either a successful repoint or a
// rejection. Shared by the Feature A (PRD #344) repoint tests below.
func (f scheduleFixture) patchSchedule(t *testing.T, user uuid.UUID, id, body string) (apitypes.ScheduleDTO, int) {
	t.Helper()
	req := userReq(http.MethodPatch, "/api/schedules/"+id, body, user, map[string]string{"id": id})
	rec := httptest.NewRecorder()
	f.h.PatchSchedule(rec, req)
	var out apitypes.ScheduleDTO
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode patch: %v (body %s)", err, rec.Body.String())
		}
	}
	return out, rec.Code
}

// getSchedule GETs /api/schedules/{id} as `user` and returns the decoded DTO (only when the
// code is 200) plus the HTTP code.
func (f scheduleFixture) getSchedule(t *testing.T, user uuid.UUID, id string) (apitypes.ScheduleDTO, int) {
	t.Helper()
	req := userReq(http.MethodGet, "/api/schedules/"+id, "", user, map[string]string{"id": id})
	rec := httptest.NewRecorder()
	f.h.GetSchedule(rec, req)
	var out apitypes.ScheduleDTO
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode get: %v (body %s)", err, rec.Body.String())
		}
	}
	return out, rec.Code
}

// TestScheduleRepointRoundTripLiveDB exercises Feature A (PRD #344 M3) end-to-end against a
// real Postgres: a BARE {"repo_id":...} PATCH repoints a sweep schedule IN PLACE — the same
// schedule id, its non-repo config carried forward by the keep-on-empty merge seed — and a
// follow-up GET proves the row was UPDATED, not delete+recreated (stable id, new repo_path).
func TestScheduleRepointRoundTripLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	// A recurring sweep on the fixture repo (g/sched), with labels + a specific cron so the
	// keep-on-empty carry-through of the non-repo config is observable after the repoint.
	dto, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"sweep","labels":["bug","urgent"],"timing":"recurring","cron_expr":"0 9 * * 1"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	if dto.RepoPath != "g/sched" {
		t.Fatalf("create repo_path = %q, want g/sched", dto.RepoPath)
	}
	id := dto.ID

	repo2 := f.insertRepo(ctx, t, f.owner, 2, "g/sched2")

	// A BARE repo_id PATCH: no other config resent. The keep-on-empty seed must carry the
	// target/cron/labels from the current row while only the repo moves.
	moved, mc := f.patchSchedule(t, f.owner.ID, id, `{"repo_id":"`+repo2.String()+`"}`)
	if mc != http.StatusOK {
		t.Fatalf("repoint patch status = %d, want 200", mc)
	}
	if moved.ID != id {
		t.Fatalf("repoint changed the schedule id: got %q, want %q (repoint is an UPDATE, not delete+recreate)", moved.ID, id)
	}
	if moved.RepoID != repo2.String() {
		t.Fatalf("repoint repo_id = %q, want %q", moved.RepoID, repo2.String())
	}
	if moved.RepoPath != "g/sched2" {
		t.Fatalf("repoint repo_path = %q, want g/sched2", moved.RepoPath)
	}
	// The keep-on-empty seed carried the rest of the row: target, cron, labels intact.
	if moved.Target != "sweep" {
		t.Fatalf("repoint target = %q, want sweep (config must survive a bare repo_id PATCH)", moved.Target)
	}
	if moved.CronExpr != "0 9 * * 1" {
		t.Fatalf("repoint cron_expr = %q, want the original 0 9 * * 1 (config must survive)", moved.CronExpr)
	}
	if len(moved.Labels) != 2 || moved.Labels[0] != "bug" || moved.Labels[1] != "urgent" {
		t.Fatalf("repoint labels = %v, want [bug urgent] (config must survive)", moved.Labels)
	}

	// A follow-up GET proves the row was UPDATED, not delete+recreated: same id resolves to
	// 200, now pointing at the new repo.
	got, gc := f.getSchedule(t, f.owner.ID, id)
	if gc != http.StatusOK {
		t.Fatalf("get after repoint status = %d, want 200 (id must be preserved)", gc)
	}
	if got.ID != id || got.RepoPath != "g/sched2" {
		t.Fatalf("get after repoint = id %q repo_path %q, want id %q repo_path g/sched2", got.ID, got.RepoPath, id)
	}
}

// TestScheduleRepointOwnerIsolationLiveDB pins the D3 ownership mirror against the real
// GetRepoForUser query: an owner cannot repoint onto a repo they do not own (404), nor onto
// an absent repo (404), and a rejected repoint does not mutate the row.
func TestScheduleRepointOwnerIsolationLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	dto, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"sweep","labels":["bug"],"timing":"recurring","cron_expr":"0 9 * * 1"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	id := dto.ID

	// A repo owned by the stranger, not the owner: repoint must 404 (ownership mirror).
	strangerRepo := f.insertRepo(ctx, t, f.stranger, 2, "s/repo")
	if _, sc := f.patchSchedule(t, f.owner.ID, id, `{"repo_id":"`+strangerRepo.String()+`"}`); sc != http.StatusNotFound {
		t.Fatalf("repoint onto a foreign repo status = %d, want 404", sc)
	}

	// An absent repo id: also a 404 (GetRepoForUser finds nothing).
	if _, ac := f.patchSchedule(t, f.owner.ID, id, `{"repo_id":"`+uuid.New().String()+`"}`); ac != http.StatusNotFound {
		t.Fatalf("repoint onto an absent repo status = %d, want 404", ac)
	}

	// The schedule's repo is UNCHANGED after both rejected repoints.
	got, gc := f.getSchedule(t, f.owner.ID, id)
	if gc != http.StatusOK || got.RepoPath != "g/sched" {
		t.Fatalf("after rejected repoints, get = code %d repo_path %q, want 200 and g/sched (row must not mutate)", gc, got.RepoPath)
	}
}

// TestScheduleRepointMalformedLiveDB pins the 400 ground: a non-UUID repo_id is rejected
// before any store call, and the row is unchanged.
func TestScheduleRepointMalformedLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	dto, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"sweep","labels":["bug"],"timing":"recurring","cron_expr":"0 9 * * 1"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	id := dto.ID

	if _, mc := f.patchSchedule(t, f.owner.ID, id, `{"repo_id":"not-a-uuid"}`); mc != http.StatusBadRequest {
		t.Fatalf("malformed repo_id status = %d, want 400", mc)
	}

	got, gc := f.getSchedule(t, f.owner.ID, id)
	if gc != http.StatusOK || got.RepoPath != "g/sched" {
		t.Fatalf("after malformed repoint, get = code %d repo_path %q, want 200 and g/sched (row must not mutate)", gc, got.RepoPath)
	}
}

// TestScheduleResumeRearmLiveDB pins issue #396: an enabled-only resume of a RECURRING
// schedule re-arms next_fire_at to the next FUTURE cron occurrence in the same write that
// flips enabled, so a schedule paused past one or more fire windows does not immediately
// replay the missed window on resume — and it does so ONLY for the recurring/enabled-only
// path, leaving status, `once` schedules, and combined config+enable PATCHes untouched.
//
// Runs against a real Postgres so it exercises the real ResumeRecurringSchedule query
// (its RETURNING and its deliberate refusal to touch status) rather than a stub.
func TestScheduleResumeRearmLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	// forceOverdue pushes a row's next_fire_at into the past so a resume that fails to
	// re-arm would leave an overdue fire time behind (the exact bug of issue #396).
	forceOverdue := func(t *testing.T, id string, interval string) {
		t.Helper()
		mustExecT(ctx, t, f.pool,
			`UPDATE run_schedules SET next_fire_at = now() - `+interval+` WHERE id = $1`, uuid.MustParse(id))
	}
	// runCount reports how many runs point back at a schedule via schedule_id.
	runCount := func(t *testing.T, id string) int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE schedule_id = $1`, uuid.MustParse(id)).Scan(&n); err != nil {
			t.Fatalf("count runs: %v", err)
		}
		return n
	}

	t.Run("recurring-overdue-resume", func(t *testing.T) {
		dto, code := f.createSchedule(t, f.owner.ID, f.repoID,
			`{"target":"issue","issue_iid":7,"timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC"}`)
		if code != http.StatusCreated {
			t.Fatalf("create status = %d, want 201", code)
		}
		id := dto.ID

		// Pause, then force the row overdue while paused.
		if _, pc := f.patchSchedule(t, f.owner.ID, id, `{"enabled":false}`); pc != http.StatusOK {
			t.Fatalf("pause status = %d, want 200", pc)
		}
		forceOverdue(t, id, "interval '3 hours'")

		// Resume via an enabled-only PATCH: the row must re-arm to a FUTURE fire time.
		resumed, rc := f.patchSchedule(t, f.owner.ID, id, `{"enabled":true}`)
		if rc != http.StatusOK {
			t.Fatalf("resume status = %d, want 200", rc)
		}
		if !resumed.Enabled {
			t.Fatalf("after resume enabled = false, want true")
		}
		if resumed.NextFireAt == nil {
			t.Fatalf("resumed schedule has no next_fire_at, want a future one")
		}
		if !resumed.NextFireAt.After(time.Now()) {
			t.Fatalf("resumed next_fire_at = %s is not in the future (issue #396: resume must re-arm past the missed window)", resumed.NextFireAt)
		}
		// The overdue window must NOT have been replayed into an actual run.
		if n := runCount(t, id); n != 0 {
			t.Fatalf("resume created %d run(s), want 0 (resume must re-arm, not fire the missed window)", n)
		}
	})

	t.Run("recurring-future-resume-idempotent", func(t *testing.T) {
		dto, code := f.createSchedule(t, f.owner.ID, f.repoID,
			`{"target":"issue","issue_iid":7,"timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC"}`)
		if code != http.StatusCreated {
			t.Fatalf("create status = %d, want 201", code)
		}
		id := dto.ID
		if dto.NextFireAt == nil {
			t.Fatalf("created recurring schedule has no next_fire_at")
		}
		created := *dto.NextFireAt // already a future occurrence; do NOT force it.

		if _, pc := f.patchSchedule(t, f.owner.ID, id, `{"enabled":false}`); pc != http.StatusOK {
			t.Fatalf("pause status = %d, want 200", pc)
		}
		resumed, rc := f.patchSchedule(t, f.owner.ID, id, `{"enabled":true}`)
		if rc != http.StatusOK {
			t.Fatalf("resume status = %d, want 200", rc)
		}
		if resumed.NextFireAt == nil {
			t.Fatalf("resumed schedule has no next_fire_at")
		}
		// A natural future occurrence is the same first-strictly-after-now cron instant on
		// both sides, so a resume that re-arms must land on the SAME instant it created.
		if d := resumed.NextFireAt.Sub(created); d < -time.Second || d > time.Second {
			t.Fatalf("resumed next_fire_at = %s, want the originally-created occurrence %s (delta %s)", resumed.NextFireAt, created, d)
		}
		if !resumed.NextFireAt.After(time.Now()) {
			t.Fatalf("resumed next_fire_at = %s is not in the future", resumed.NextFireAt)
		}
	})

	t.Run("combined-config-and-enable", func(t *testing.T) {
		dto, code := f.createSchedule(t, f.owner.ID, f.repoID,
			`{"target":"issue","issue_iid":7,"timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC"}`)
		if code != http.StatusCreated {
			t.Fatalf("create status = %d, want 201", code)
		}
		id := dto.ID
		if _, pc := f.patchSchedule(t, f.owner.ID, id, `{"enabled":false}`); pc != http.StatusOK {
			t.Fatalf("pause status = %d, want 200", pc)
		}
		forceOverdue(t, id, "interval '3 hours'")

		// A single PATCH carrying BOTH a new cron and enabled:true is NOT the enabled-only
		// path — it flows through the config UPDATE, which recomputes next_fire from the NEW
		// cron. This proves the rearm branch did not re-derive from the old 0 2 cron.
		now := time.Now()
		combined, cc := f.patchSchedule(t, f.owner.ID, id, `{"cron_expr":"0 14 * * *","enabled":true}`)
		if cc != http.StatusOK {
			t.Fatalf("combined patch status = %d, want 200", cc)
		}
		if combined.CronExpr != "0 14 * * *" {
			t.Fatalf("combined patch cron_expr = %q, want 0 14 * * *", combined.CronExpr)
		}
		if !combined.Enabled {
			t.Fatalf("combined patch enabled = false, want true")
		}
		if combined.NextFireAt == nil {
			t.Fatalf("combined patch has no next_fire_at")
		}
		want, err := schedsvc.NextFire("0 14 * * *", "UTC", now)
		if err != nil {
			t.Fatalf("compute expected next fire: %v", err)
		}
		if d := combined.NextFireAt.Sub(want); d < -2*time.Second || d > 2*time.Second {
			t.Fatalf("combined next_fire_at = %s, want the NEW cron's occurrence %s (delta %s)", combined.NextFireAt, want, d)
		}
		if h := combined.NextFireAt.UTC().Hour(); h != 14 {
			t.Fatalf("combined next_fire_at hour (UTC) = %d, want 14 (must derive from the NEW cron, not the old 0 2)", h)
		}
	})

	t.Run("parked-recurring-resume-keeps-status", func(t *testing.T) {
		dto, code := f.createSchedule(t, f.owner.ID, f.repoID,
			`{"target":"issue","issue_iid":7,"timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC"}`)
		if code != http.StatusCreated {
			t.Fatalf("create status = %d, want 201", code)
		}
		id := dto.ID
		// Park it (status='error') AND force it overdue, both while it is enabled.
		mustExecT(ctx, t, f.pool,
			`UPDATE run_schedules SET status = 'error', next_fire_at = now() - interval '3 hours' WHERE id = $1`, uuid.MustParse(id))
		// Also pause it so the resume is a genuine enabled:false→true transition.
		if _, pc := f.patchSchedule(t, f.owner.ID, id, `{"enabled":false}`); pc != http.StatusOK {
			t.Fatalf("pause status = %d, want 200", pc)
		}

		resumed, rc := f.patchSchedule(t, f.owner.ID, id, `{"enabled":true}`)
		if rc != http.StatusOK {
			t.Fatalf("resume status = %d, want 200", rc)
		}
		// status is pause/resume-orthogonal: resume must NOT revive a parked schedule.
		if resumed.Status != "error" {
			t.Fatalf("resumed status = %q, want error (resume must not un-park a status='error' schedule)", resumed.Status)
		}
		// But the rearm still happened.
		if resumed.NextFireAt == nil || !resumed.NextFireAt.After(time.Now()) {
			t.Fatalf("resumed next_fire_at = %v, want a future instant (rearm must happen even for a parked schedule)", resumed.NextFireAt)
		}
	})

	t.Run("once-overdue-resume-not-rearmed", func(t *testing.T) {
		runAt := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
		dto, code := f.createSchedule(t, f.owner.ID, f.repoID,
			fmt.Sprintf(`{"target":"issue","issue_iid":7,"timing":"once","run_at":%q,"timezone":"UTC"}`, runAt))
		if code != http.StatusCreated {
			t.Fatalf("create once status = %d, want 201 (body path checked run_at future)", code)
		}
		id := dto.ID
		if _, pc := f.patchSchedule(t, f.owner.ID, id, `{"enabled":false}`); pc != http.StatusOK {
			t.Fatalf("pause status = %d, want 200", pc)
		}
		// Force the once row overdue while paused.
		forceOverdue(t, id, "interval '1 hour'")

		resumed, rc := f.patchSchedule(t, f.owner.ID, id, `{"enabled":true}`)
		if rc != http.StatusOK {
			t.Fatalf("resume status = %d, want 200", rc)
		}
		if !resumed.Enabled {
			t.Fatalf("after once resume enabled = false, want true")
		}
		if resumed.NextFireAt == nil {
			t.Fatalf("resumed once schedule has no next_fire_at")
		}
		// The rearm branch must NOT run for a `once` schedule: next_fire_at stays the forced
		// overdue instant (still due), leaving the fire itself to the scheduler.
		if resumed.NextFireAt.After(time.Now()) {
			t.Fatalf("resumed once next_fire_at = %s is in the future; a `once` resume must NOT re-arm (rearm is recurring-only)", resumed.NextFireAt)
		}
		if d := time.Since(*resumed.NextFireAt); d < 30*time.Minute || d > 90*time.Minute {
			t.Fatalf("resumed once next_fire_at is %s in the past, want ~1 hour (the forced overdue value, unchanged)", d)
		}
	})

	t.Run("pause-does-not-touch-in-flight-run", func(t *testing.T) {
		dto, code := f.createSchedule(t, f.owner.ID, f.repoID,
			`{"target":"prompt","prompt":"do the thing","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC"}`)
		if code != http.StatusCreated {
			t.Fatalf("create status = %d, want 201", code)
		}
		id := dto.ID

		// Insert a prompt run for this schedule (the scheduler's own insert path).
		run, err := f.h.q.CreatePromptRun(ctx, store.CreatePromptRunParams{
			UserID:      f.owner.ID,
			RepoID:      f.repoID,
			IssueTitle:  "scheduled prompt",
			ScheduleID:  uuid.MustParse(id),
			AutoApprove: true,
			WaitOnLimit: true,
		})
		if err != nil {
			t.Fatalf("insert prompt run: %v", err)
		}
		before := run.Status // default 'queued'

		// Pause the schedule.
		if _, pc := f.patchSchedule(t, f.owner.ID, id, `{"enabled":false}`); pc != http.StatusOK {
			t.Fatalf("pause status = %d, want 200", pc)
		}

		// The in-flight run must still exist with its status UNCHANGED (pause is not cancel).
		var after string
		if err := f.pool.QueryRow(ctx, `SELECT status FROM runs WHERE schedule_id = $1`, uuid.MustParse(id)).Scan(&after); err != nil {
			t.Fatalf("read run status after pause: %v (run must still exist)", err)
		}
		if after != before {
			t.Fatalf("run status after pause = %q, want %q unchanged (pause must not cancel an in-flight run)", after, before)
		}
	})
}

// TestScheduleRepointIssueTargetRestrictedLiveDB pins D4 restrict against a real row: an
// issue-target schedule cannot be repointed (422, because issue_iid is repo-relative), and
// the rejected repoint does not mutate the row.
func TestScheduleRepointIssueTargetRestrictedLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	dto, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"issue","issue_iid":7,"timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	id := dto.ID

	repo2 := f.insertRepo(ctx, t, f.owner, 2, "g/sched3")
	if _, rc := f.patchSchedule(t, f.owner.ID, id, `{"repo_id":"`+repo2.String()+`"}`); rc != http.StatusUnprocessableEntity {
		t.Fatalf("issue-target repoint status = %d, want 422 (D4 restrict)", rc)
	}

	got, gc := f.getSchedule(t, f.owner.ID, id)
	if gc != http.StatusOK || got.RepoPath != "g/sched" {
		t.Fatalf("after rejected issue repoint, get = code %d repo_path %q, want 200 and g/sched (row must not mutate)", gc, got.RepoPath)
	}
}

// TestCreateScheduleOversizeBody413LiveDB is the schedules half of PRD #954 M3 (S3):
// an over-cap body on an OWNED repo's create-schedule route (repoForRequest passes, so
// DecodeJSONLimited is what fails) answers a truthful 413 with uzi's own prose — not the
// 400 the site used to return, and not net/http's "http: request body too large" literal.
func TestCreateScheduleOversizeBody413LiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	// A well-formed JSON body strictly larger than the 1 MiB cap, so it crosses on SIZE
	// rather than on any malformation.
	body := `{"target":"issue","cron_expr":"` + strings.Repeat("a", 1<<20) + `"}`
	if len(body) <= 1<<20 {
		t.Fatalf("oversize fixture is %d bytes, not over the 1 MiB cap it exists to cross", len(body))
	}

	req := userReq(http.MethodPost, "/api/repos/"+f.repoID.String()+"/schedules", body,
		f.owner.ID, map[string]string{"id": f.repoID.String()})
	rec := httptest.NewRecorder()
	f.h.CreateSchedule(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize body status = %d, want 413 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "too large") {
		t.Fatalf("413 body %q does not carry uzi's oversize prose", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "http:") {
		t.Fatalf("413 body leaked net/http's stdlib literal: %q", rec.Body.String())
	}
}
