package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
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
		{Key: settings.KeyPRDLabel, Value: "PRD"},
	}}, time.Minute)
	f.h = &Handler{
		pool:      pool,
		q:         q,
		box:       box,
		cfg:       config.Config{},
		settings:  set,
		svc:       svc,
		wsvc:      wsvc,
		scheduler: schedsvc.New(q, wsvc, svc, set, nil, 0, slog.Default()),
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
