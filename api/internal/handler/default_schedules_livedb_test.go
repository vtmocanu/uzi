package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/schedsvc"
	"github.com/vtmocanu/uzi/api/internal/schedtmpl"
)

// Handler-level end-to-end coverage for the PRD #589 M2 default-schedule endpoints against
// a real Postgres (owner-scoping and idempotency live in the SQL). Skipped unless
// UZI_TEST_DATABASE_URL is set; ./e2e/run-store-it.sh provides one.

// enableCatalog POSTs to /api/repos/{id}/schedule-catalog/{slug} and returns the decoded DTO.
func (f scheduleFixture) enableCatalog(t *testing.T, user, repoID uuid.UUID, slug string) (apitypes.ScheduleDTO, int) {
	t.Helper()
	req := userReq(http.MethodPost, "/api/repos/"+repoID.String()+"/schedule-catalog/"+slug, "",
		user, map[string]string{"id": repoID.String(), "slug": slug})
	rec := httptest.NewRecorder()
	f.h.EnableCatalogSchedule(rec, req)
	var dto apitypes.ScheduleDTO
	if rec.Code == http.StatusCreated || rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
			t.Fatalf("decode enable response: %v (body %s)", err, rec.Body.String())
		}
	}
	return dto, rec.Code
}

// enableCatalogBody is enableCatalog with a caller-supplied request body (issue #660): it
// POSTs `body` (e.g. a `{"timezone":...}` override) instead of the empty body enableCatalog
// sends, and returns the decoded DTO plus the status code.
func (f scheduleFixture) enableCatalogBody(t *testing.T, user, repoID uuid.UUID, slug, body string) (apitypes.ScheduleDTO, int) {
	t.Helper()
	req := userReq(http.MethodPost, "/api/repos/"+repoID.String()+"/schedule-catalog/"+slug, body,
		user, map[string]string{"id": repoID.String(), "slug": slug})
	rec := httptest.NewRecorder()
	f.h.EnableCatalogSchedule(rec, req)
	var dto apitypes.ScheduleDTO
	if rec.Code == http.StatusCreated || rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
			t.Fatalf("decode enable response: %v (body %s)", err, rec.Body.String())
		}
	}
	return dto, rec.Code
}

func TestScheduleCatalogLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	req := userReq(http.MethodGet, "/api/schedule-catalog", "", f.owner.ID, nil)
	rec := httptest.NewRecorder()
	f.h.ScheduleCatalog(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var resp apitypes.ScheduleCatalogResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	if len(resp.Entries) != len(schedtmpl.Catalog()) {
		t.Fatalf("catalog entries = %d, want %d", len(resp.Entries), len(schedtmpl.Catalog()))
	}
	if len(resp.Enablements) != 0 {
		t.Fatalf("enablements = %d, want 0 before any enable", len(resp.Enablements))
	}

	// After enabling one, its (repo, slug) appears in the enablement state.
	dto, code := f.enableCatalog(t, f.owner.ID, f.repoID, "docs-hygiene")
	if code != http.StatusCreated {
		t.Fatalf("enable status = %d, want 201", code)
	}
	rec2 := httptest.NewRecorder()
	f.h.ScheduleCatalog(rec2, userReq(http.MethodGet, "/api/schedule-catalog", "", f.owner.ID, nil))
	var resp2 apitypes.ScheduleCatalogResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode catalog 2: %v", err)
	}
	if len(resp2.Enablements) != 1 {
		t.Fatalf("enablements = %d, want 1 after enable", len(resp2.Enablements))
	}
	en := resp2.Enablements[0]
	if en.Slug != "docs-hygiene" || en.RepoID != f.repoID.String() || en.ScheduleID != dto.ID {
		t.Fatalf("enablement = %+v, want docs-hygiene on this repo/schedule", en)
	}
}

func TestEnableCatalogScheduleIdempotentLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	want, _ := schedtmpl.BySlug("docs-hygiene")
	first, code := f.enableCatalog(t, f.owner.ID, f.repoID, "docs-hygiene")
	if code != http.StatusCreated {
		t.Fatalf("first enable status = %d, want 201", code)
	}
	if first.Origin != "default" || first.CatalogSlug == nil || *first.CatalogSlug != "docs-hygiene" {
		t.Fatalf("enabled dto origin/slug = %q/%v, want default/docs-hygiene", first.Origin, first.CatalogSlug)
	}
	// The DTO surfaces the RESOLVED catalog prompt (never stored on the row).
	if first.Prompt != want.Prompt {
		t.Fatalf("dto prompt = %q, want the resolved catalog prompt", first.Prompt)
	}
	if first.Customized {
		t.Fatal("newly enabled default is customized, want false")
	}

	// A repeat enable is idempotent: 200 with the SAME schedule id.
	second, code := f.enableCatalog(t, f.owner.ID, f.repoID, "docs-hygiene")
	if code != http.StatusOK {
		t.Fatalf("second enable status = %d, want 200 (idempotent)", code)
	}
	if second.ID != first.ID {
		t.Fatalf("idempotent enable returned a different schedule: %s vs %s", second.ID, first.ID)
	}

	// An unknown slug is a 404.
	_, code = f.enableCatalog(t, f.owner.ID, f.repoID, "no-such-slug")
	if code != http.StatusNotFound {
		t.Fatalf("unknown slug enable status = %d, want 404", code)
	}
}

// TestEnableCatalogScheduleTimezoneOverrideLiveDB (issue #660) pins the optional timezone
// override on enable: a `{"timezone":...}` body makes the enabled default fire in that zone
// from the first fire (both the stored Timezone and the computed next_fire_at), an
// empty/absent body keeps the catalog zone (UTC), and an invalid IANA name is a 400.
func TestEnableCatalogScheduleTimezoneOverrideLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	// A fixed clock so the handler's NextFire and the test's NextFire compute from the same
	// instant (the fixture leaves h.now nil → time.Now, which two calls could straddle).
	clock := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	f.h.now = func() time.Time { return clock }

	job, _ := schedtmpl.BySlug("docs-hygiene") // cron "0 3 * * 1": a fixed wall-clock hour, so UTC vs Bucharest differ.

	// A valid override zone is stored on the row and drives next_fire_at.
	dto, code := f.enableCatalogBody(t, f.owner.ID, f.repoID, "docs-hygiene", `{"timezone":"Europe/Bucharest"}`)
	if code != http.StatusCreated {
		t.Fatalf("enable with tz override status = %d, want 201", code)
	}
	if dto.Timezone != "Europe/Bucharest" {
		t.Fatalf("override dto timezone = %q, want Europe/Bucharest", dto.Timezone)
	}
	wantNext, err := schedsvc.NextFire(job.Cron, "Europe/Bucharest", clock)
	if err != nil {
		t.Fatalf("compute expected Bucharest next fire: %v", err)
	}
	utcNext, err := schedsvc.NextFire(job.Cron, "UTC", clock)
	if err != nil {
		t.Fatalf("compute UTC next fire: %v", err)
	}
	if wantNext.Equal(utcNext) {
		t.Fatal("Bucharest and UTC next fires are the same instant; pick a cron whose wall-clock hour differs per zone")
	}
	if dto.NextFireAt == nil || !dto.NextFireAt.Equal(wantNext) {
		t.Fatalf("override next_fire_at = %v, want the Bucharest instant %v", dto.NextFireAt, wantNext)
	}

	// Idempotent re-enable of the SAME repo with a DIFFERENT tz must not clobber the stored
	// zone (issue #660 AC: no regression to idempotent re-enable). The override feeds only the
	// initial insert; ON CONFLICT DO NOTHING returns the existing row untouched with 200.
	reenabled, code := f.enableCatalogBody(t, f.owner.ID, f.repoID, "docs-hygiene", `{"timezone":"America/New_York"}`)
	if code != http.StatusOK {
		t.Fatalf("re-enable status = %d, want 200 (idempotent)", code)
	}
	if reenabled.ID != dto.ID {
		t.Fatalf("re-enable returned a different schedule: %s vs %s", reenabled.ID, dto.ID)
	}
	if reenabled.Timezone != "Europe/Bucharest" {
		t.Fatalf("re-enable clobbered the stored tz = %q, want the original Europe/Bucharest", reenabled.Timezone)
	}

	// An empty body keeps the catalog zone (UTC) on a second repo (the first is now enabled).
	repoB := f.insertRepo(ctx, t, f.owner, 2, "g/sched-tz-b")
	base, code := f.enableCatalogBody(t, f.owner.ID, repoB, "docs-hygiene", "")
	if code != http.StatusCreated {
		t.Fatalf("enable with empty body status = %d, want 201", code)
	}
	if base.Timezone != schedtmpl.DefaultTimezone {
		t.Fatalf("empty-body dto timezone = %q, want the catalog default %q", base.Timezone, schedtmpl.DefaultTimezone)
	}
	if base.NextFireAt == nil || !base.NextFireAt.Equal(utcNext) {
		t.Fatalf("empty-body next_fire_at = %v, want the UTC instant %v", base.NextFireAt, utcNext)
	}

	// An invalid IANA name is a 400 (on a third repo so the check is independent of state).
	repoC := f.insertRepo(ctx, t, f.owner, 3, "g/sched-tz-c")
	if _, code := f.enableCatalogBody(t, f.owner.ID, repoC, "docs-hygiene", `{"timezone":"Not/AZone"}`); code != http.StatusBadRequest {
		t.Fatalf("invalid timezone enable status = %d, want 400", code)
	}
}

func TestResetScheduleLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	dto, code := f.enableCatalog(t, f.owner.ID, f.repoID, "docs-hygiene")
	if code != http.StatusCreated {
		t.Fatalf("enable status = %d, want 201", code)
	}
	job, _ := schedtmpl.BySlug("docs-hygiene")

	// Diverge the cron via a config PATCH → the row becomes customized.
	patched := f.patchDefault(t, f.owner.ID, dto.ID, `{"cron_expr":"15 3 * * 4"}`)
	if !patched.Customized {
		t.Fatalf("after a divergent patch, customized = false, want true")
	}

	// Reset restores the catalog cron and clears customized.
	resetReq := userReq(http.MethodPost, "/api/schedules/"+dto.ID+"/reset", "", f.owner.ID, map[string]string{"id": dto.ID})
	resetRec := httptest.NewRecorder()
	f.h.ResetSchedule(resetRec, resetReq)
	if resetRec.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want 200 (body %s)", resetRec.Code, resetRec.Body.String())
	}
	var reset apitypes.ScheduleDTO
	if err := json.Unmarshal(resetRec.Body.Bytes(), &reset); err != nil {
		t.Fatalf("decode reset: %v", err)
	}
	if reset.Customized {
		t.Fatal("reset row customized = true, want false")
	}
	if reset.CronExpr != job.Cron {
		t.Fatalf("reset cron = %q, want the catalog default %q", reset.CronExpr, job.Cron)
	}

	// Resetting a user-origin schedule is a 409.
	userDTO, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"my own prompt","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC"}`)
	if code != http.StatusCreated {
		t.Fatalf("create user schedule status = %d, want 201", code)
	}
	rReq := userReq(http.MethodPost, "/api/schedules/"+userDTO.ID+"/reset", "", f.owner.ID, map[string]string{"id": userDTO.ID})
	rRec := httptest.NewRecorder()
	f.h.ResetSchedule(rRec, rReq)
	if rRec.Code != http.StatusConflict {
		t.Fatalf("reset user schedule status = %d, want 409", rRec.Code)
	}
}

func TestPatchDefaultScheduleCustomizedLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	dto, code := f.enableCatalog(t, f.owner.ID, f.repoID, "docs-hygiene")
	if code != http.StatusCreated {
		t.Fatalf("enable status = %d, want 201", code)
	}
	job, _ := schedtmpl.BySlug("docs-hygiene")

	// A divergent editable field sets customized.
	diverged := f.patchDefault(t, f.owner.ID, dto.ID, `{"cron_expr":"15 3 * * 4"}`)
	if !diverged.Customized {
		t.Fatal("divergent patch: customized = false, want true")
	}
	// Its prompt stays the RESOLVED catalog prompt (never editable).
	if diverged.Prompt != job.Prompt {
		t.Fatalf("patched default prompt = %q, want catalog prompt unchanged", diverged.Prompt)
	}

	// Patching every editable field back to the catalog values clears customized.
	restored := f.patchDefault(t, f.owner.ID, dto.ID, `{"cron_expr":"`+job.Cron+`"}`)
	if restored.Customized {
		t.Fatal("catalog-matching patch: customized = true, want false")
	}

	// A default row's prompt is catalog-owned and cannot be patched: 400.
	badReq := userReq(http.MethodPatch, "/api/schedules/"+dto.ID, `{"prompt":"hijack the baked prompt"}`, f.owner.ID, map[string]string{"id": dto.ID})
	badRec := httptest.NewRecorder()
	f.h.PatchSchedule(badRec, badReq)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("patch default prompt status = %d, want 400", badRec.Code)
	}
}

// TestPatchDefaultOverrideSubagentModelLiveDB (issue #691): override_subagent_model is a run
// option owner-editable on a default. A config PATCH toggling it persists AND flips
// customized (its catalog baseline is always false, so any toggled-on value diverges); an
// exact-restore back to false un-customizes; and Reset restores the catalog baseline (false)
// and clears customized.
func TestPatchDefaultOverrideSubagentModelLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	dto, code := f.enableCatalog(t, f.owner.ID, f.repoID, "docs-hygiene")
	if code != http.StatusCreated {
		t.Fatalf("enable status = %d, want 201", code)
	}
	// Fresh default: the override is at the catalog baseline (false) and the row is not customized.
	if dto.OverrideSubagentModel == nil || *dto.OverrideSubagentModel {
		t.Fatalf("fresh default override = %v, want a non-nil false (catalog baseline)", dto.OverrideSubagentModel)
	}
	if dto.Customized {
		t.Fatal("fresh default customized = true, want false")
	}

	// Toggling override on persists and flips customized.
	on := f.patchDefault(t, f.owner.ID, dto.ID, `{"override_subagent_model":true}`)
	if on.OverrideSubagentModel == nil || !*on.OverrideSubagentModel {
		t.Fatalf("patched override = %v, want a non-nil true (persisted)", on.OverrideSubagentModel)
	}
	if !on.Customized {
		t.Fatal("override-on divergence: customized = false, want true")
	}

	// It really persisted: re-read via GET surfaces the toggled-on override.
	got, gcode := f.getSchedule(t, f.owner.ID, dto.ID)
	if gcode != http.StatusOK {
		t.Fatalf("re-read status = %d, want 200", gcode)
	}
	if got.OverrideSubagentModel == nil || !*got.OverrideSubagentModel {
		t.Fatalf("re-read override = %v, want the persisted true", got.OverrideSubagentModel)
	}

	// Toggling it back off is an exact-restore: override false AND customized cleared.
	off := f.patchDefault(t, f.owner.ID, dto.ID, `{"override_subagent_model":false}`)
	if off.OverrideSubagentModel == nil || *off.OverrideSubagentModel {
		t.Fatalf("restored override = %v, want a non-nil false", off.OverrideSubagentModel)
	}
	if off.Customized {
		t.Fatal("exact-restore: customized = true, want false (un-customizes)")
	}

	// Toggle on again, then Reset drops it back to the catalog baseline and un-customizes.
	reon := f.patchDefault(t, f.owner.ID, dto.ID, `{"override_subagent_model":true}`)
	if reon.OverrideSubagentModel == nil || !*reon.OverrideSubagentModel {
		t.Fatalf("re-toggled override = %v, want a non-nil true", reon.OverrideSubagentModel)
	}
	resetReq := userReq(http.MethodPost, "/api/schedules/"+dto.ID+"/reset", "", f.owner.ID, map[string]string{"id": dto.ID})
	resetRec := httptest.NewRecorder()
	f.h.ResetSchedule(resetRec, resetReq)
	if resetRec.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want 200 (body %s)", resetRec.Code, resetRec.Body.String())
	}
	var reset apitypes.ScheduleDTO
	if err := json.Unmarshal(resetRec.Body.Bytes(), &reset); err != nil {
		t.Fatalf("decode reset: %v", err)
	}
	if reset.OverrideSubagentModel == nil || *reset.OverrideSubagentModel {
		t.Fatalf("reset override = %v, want the catalog baseline false restored", reset.OverrideSubagentModel)
	}
	if reset.Customized {
		t.Fatal("reset customized = true, want false")
	}
}

// cloneSchedule POSTs to /api/schedules/{id}/clone with the given (possibly empty) body and
// returns the decoded DTO plus the status code.
func (f scheduleFixture) cloneSchedule(t *testing.T, user uuid.UUID, id, body string) (apitypes.ScheduleDTO, int) {
	t.Helper()
	req := userReq(http.MethodPost, "/api/schedules/"+id+"/clone", body, user, map[string]string{"id": id})
	rec := httptest.NewRecorder()
	f.h.CloneSchedule(rec, req)
	var dto apitypes.ScheduleDTO
	if rec.Code == http.StatusCreated {
		if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
			t.Fatalf("decode clone response: %v (body %s)", err, rec.Body.String())
		}
	}
	return dto, rec.Code
}

// TestCloneDefaultPromptUnlocksLiveDB is the M3 flagship: cloning a default PROMPT schedule
// produces a fully-editable user row whose prompt column equals the catalog's baked prompt
// (the lock is lifted), with catalog_slug NULL and customized false.
func TestCloneDefaultPromptUnlocksLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	src, code := f.enableCatalog(t, f.owner.ID, f.repoID, "docs-hygiene")
	if code != http.StatusCreated {
		t.Fatalf("enable status = %d, want 201", code)
	}
	job, _ := schedtmpl.BySlug("docs-hygiene")

	clone, code := f.cloneSchedule(t, f.owner.ID, src.ID, "")
	if code != http.StatusCreated {
		t.Fatalf("clone status = %d, want 201", code)
	}
	if clone.ID == src.ID {
		t.Fatal("clone returned the same schedule id as the source")
	}
	if clone.Origin != "user" {
		t.Fatalf("clone origin = %q, want user", clone.Origin)
	}
	if clone.CatalogSlug != nil {
		t.Fatalf("clone catalog_slug = %v, want nil (lock lifted)", clone.CatalogSlug)
	}
	if clone.Customized {
		t.Fatal("clone customized = true, want false")
	}
	// The lock-lift: the baked catalog prompt is now stored on the user row (the DTO of a
	// user row surfaces the stored column, not a resolved catalog value).
	if clone.Prompt != job.Prompt {
		t.Fatalf("clone prompt = %q, want the baked catalog prompt", clone.Prompt)
	}
	// It landed in the source's own repo (no repo_id given).
	if clone.RepoID != f.repoID.String() {
		t.Fatalf("clone repo = %q, want the source repo %q", clone.RepoID, f.repoID.String())
	}

	// The clone is now freely editable: a prompt PATCH that a default row rejects (400)
	// succeeds on the user clone.
	patched := f.patchDefault(t, f.owner.ID, clone.ID, `{"prompt":"my own edited prompt"}`)
	if patched.Prompt != "my own edited prompt" {
		t.Fatalf("edited clone prompt = %q, want the new text (clone is editable)", patched.Prompt)
	}
}

// TestCloneDefaultSweepCopiesSelectorLiveDB: cloning a default SWEEP copies the catalog's
// labels and guidance into the new user row.
func TestCloneDefaultSweepCopiesSelectorLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	src, code := f.enableCatalog(t, f.owner.ID, f.repoID, "bug-triage")
	if code != http.StatusCreated {
		t.Fatalf("enable status = %d, want 201", code)
	}
	job, _ := schedtmpl.BySlug("bug-triage")

	clone, code := f.cloneSchedule(t, f.owner.ID, src.ID, "")
	if code != http.StatusCreated {
		t.Fatalf("clone status = %d, want 201", code)
	}
	if clone.Origin != "user" || clone.CatalogSlug != nil {
		t.Fatalf("clone origin/slug = %q/%v, want user/nil", clone.Origin, clone.CatalogSlug)
	}
	if clone.Target != "sweep" {
		t.Fatalf("clone target = %q, want sweep", clone.Target)
	}
	if len(clone.Labels) != len(job.Labels) {
		t.Fatalf("clone labels = %v, want the catalog labels %v", clone.Labels, job.Labels)
	}
	for i := range job.Labels {
		if clone.Labels[i] != job.Labels[i] {
			t.Fatalf("clone labels = %v, want %v", clone.Labels, job.Labels)
		}
	}
	if job.Guidance != "" {
		if clone.Guidance == nil || *clone.Guidance != job.Guidance {
			t.Fatalf("clone guidance = %v, want the catalog guidance", clone.Guidance)
		}
	}
	if clone.MaxIssues == nil || *clone.MaxIssues != job.MaxIssues {
		t.Fatalf("clone max_issues = %v, want the catalog cap %d", clone.MaxIssues, job.MaxIssues)
	}
}

// TestCloneUserScheduleCopiesFieldsLiveDB: cloning a user-origin schedule copies its stored
// fields as-is.
func TestCloneUserScheduleCopiesFieldsLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	src, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"the original text","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}

	clone, code := f.cloneSchedule(t, f.owner.ID, src.ID, "")
	if code != http.StatusCreated {
		t.Fatalf("clone status = %d, want 201", code)
	}
	if clone.ID == src.ID {
		t.Fatal("clone returned the same id as the source user schedule")
	}
	if clone.Origin != "user" || clone.Prompt != "the original text" || clone.CronExpr != "0 2 * * *" {
		t.Fatalf("clone = %+v, want a copy of the user schedule fields", clone)
	}
}

// TestSelfImproveScheduleReconfigureLiveDB (PRD #590 follow-up, items 1 & 2) pins the
// self_improve schedule lifecycle end to end against a real DB:
//   - a DIRECT POST /schedules with target=self_improve stays a 400 (catalog-enable-only);
//   - a user-origin CLONE of an enabled self_improve default is a valid row whose config
//     PATCH (cron) is accepted (item 1);
//   - auto_approve is force-true on BOTH the user-origin clone and the default row, so a
//     PATCH setting it false is ignored and it stays true (item 2).
func TestSelfImproveScheduleReconfigureLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	// Item 1 invariant: a direct create of a self_improve schedule is rejected with the
	// unchanged target message (self_improve is not a directly-creatable target).
	req := userReq(http.MethodPost, "/api/repos/"+f.repoID.String()+"/schedules",
		`{"target":"self_improve","timing":"recurring","cron_expr":"0 4 */2 * *","timezone":"UTC"}`,
		f.owner.ID, map[string]string{"id": f.repoID.String()})
	rec := httptest.NewRecorder()
	f.h.CreateSchedule(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("direct self_improve create status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "target must be one of: issue, sweep, prompt") {
		t.Fatalf("direct self_improve create body = %q, want the unchanged target message", rec.Body.String())
	}

	// Enable the self_improve default, then clone it into a second owned repo → a user-origin
	// self_improve row the owner may reconfigure.
	def, code := f.enableCatalog(t, f.owner.ID, f.repoID, "self-improve")
	if code != http.StatusCreated {
		t.Fatalf("enable self-improve status = %d, want 201", code)
	}
	if def.Target != "self_improve" || def.Origin != "default" {
		t.Fatalf("enabled self_improve dto = target %q origin %q, want self_improve/default", def.Target, def.Origin)
	}
	if !def.AutoApprove {
		t.Fatalf("enabled self_improve default auto_approve = false, want true (catalog default)")
	}

	targetRepo := f.insertRepo(ctx, t, f.owner, 2, "g/sched-si")
	clone, code := f.cloneSchedule(t, f.owner.ID, def.ID, `{"repo_id":"`+targetRepo.String()+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("clone self_improve status = %d, want 201", code)
	}
	if clone.Target != "self_improve" || clone.Origin != "user" || clone.CatalogSlug != nil {
		t.Fatalf("clone = target %q origin %q slug %v, want self_improve/user/nil", clone.Target, clone.Origin, clone.CatalogSlug)
	}

	// Item 1: a config PATCH (new cron) on the user-origin clone is ACCEPTED (200) and persists.
	const newCron = "30 5 */3 * *"
	patched := f.patchDefault(t, f.owner.ID, clone.ID, `{"cron_expr":"`+newCron+`"}`)
	if patched.CronExpr != newCron {
		t.Fatalf("patched clone cron = %q, want %q", patched.CronExpr, newCron)
	}
	// It really persisted.
	got, gcode := f.getSchedule(t, f.owner.ID, clone.ID)
	if gcode != http.StatusOK {
		t.Fatalf("re-read clone status = %d, want 200", gcode)
	}
	if got.CronExpr != newCron {
		t.Fatalf("re-read clone cron = %q, want the persisted %q", got.CronExpr, newCron)
	}

	// Item 2 (user-origin path): a PATCH trying to set auto_approve=false is ignored — a
	// self_improve run is always auto-approved, so the stored flag stays true.
	userFalse := f.patchDefault(t, f.owner.ID, clone.ID, `{"auto_approve":false}`)
	if !userFalse.AutoApprove {
		t.Fatalf("user-origin self_improve auto_approve after false patch = %v, want true (forced)", userFalse.AutoApprove)
	}

	// Item 2 (default-origin path): the same on the enabled default row stays true.
	defFalse := f.patchDefault(t, f.owner.ID, def.ID, `{"auto_approve":false}`)
	if !defFalse.AutoApprove {
		t.Fatalf("default-origin self_improve auto_approve after false patch = %v, want true (forced)", defFalse.AutoApprove)
	}

	// Item 1 defense-in-depth: a PATCH must not CONVERT a non-self_improve row into a
	// self_improve one (that would be a create-by-patch the direct POST path blocks). Create a
	// plain sweep schedule, then try to repoint its target to self_improve → 400.
	sweep, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"sweep","timing":"recurring","cron_expr":"0 3 * * *","timezone":"UTC"}`)
	if code != http.StatusCreated {
		t.Fatalf("create sweep status = %d, want 201", code)
	}
	if _, cc := f.patchSchedule(t, f.owner.ID, sweep.ID, `{"target":"self_improve"}`); cc != http.StatusBadRequest {
		t.Fatalf("convert sweep→self_improve via PATCH status = %d, want 400 (conversion blocked)", cc)
	}
}

// TestCloneToDifferentRepoLiveDB: a {"repo_id"} body clones into a second owned repo; a
// foreign/absent target repo is a 404.
func TestCloneToDifferentRepoLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	src, code := f.enableCatalog(t, f.owner.ID, f.repoID, "docs-hygiene")
	if code != http.StatusCreated {
		t.Fatalf("enable status = %d, want 201", code)
	}
	targetRepo := f.insertRepo(ctx, t, f.owner, 2, "g/sched-two")

	clone, code := f.cloneSchedule(t, f.owner.ID, src.ID, `{"repo_id":"`+targetRepo.String()+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("clone-to-repo status = %d, want 201", code)
	}
	if clone.RepoID != targetRepo.String() {
		t.Fatalf("clone repo = %q, want the target repo %q", clone.RepoID, targetRepo.String())
	}

	// A repo the caller does not own is a 404.
	strangerRepo := f.insertRepo(ctx, t, f.stranger, 3, "g/sched-stranger")
	req := userReq(http.MethodPost, "/api/schedules/"+src.ID+"/clone", `{"repo_id":"`+strangerRepo.String()+`"}`,
		f.owner.ID, map[string]string{"id": src.ID})
	rec := httptest.NewRecorder()
	f.h.CloneSchedule(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("clone to a foreign repo status = %d, want 404", rec.Code)
	}
}

// TestEnableCatalogTwoReposLiveDB pins the SERVER invariant the CLI's client-side multi-repo
// fan-out relies on: enabling the same slug on two different repos yields two independent
// default rows.
func TestEnableCatalogTwoReposLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	repoB := f.insertRepo(ctx, t, f.owner, 2, "g/sched-b")

	a, code := f.enableCatalog(t, f.owner.ID, f.repoID, "docs-hygiene")
	if code != http.StatusCreated {
		t.Fatalf("enable on repoA status = %d, want 201", code)
	}
	b, code := f.enableCatalog(t, f.owner.ID, repoB, "docs-hygiene")
	if code != http.StatusCreated {
		t.Fatalf("enable on repoB status = %d, want 201", code)
	}
	if a.ID == b.ID {
		t.Fatal("enabling the same slug on two repos returned the same schedule id")
	}
	if a.RepoID != f.repoID.String() || b.RepoID != repoB.String() {
		t.Fatalf("rows landed on the wrong repos: a=%q b=%q", a.RepoID, b.RepoID)
	}
}

// guidanceVal dereferences a DTO's guidance pointer to a plain string ("" when nil). A
// default row's DTO surfaces the catalog guidance (a non-nil pointer, even when the catalog
// guidance is empty), so tests compare the effective value, not the pointer.
func guidanceVal(dto apitypes.ScheduleDTO) string {
	if dto.Guidance == nil {
		return ""
	}
	return *dto.Guidance
}

// jsonString JSON-encodes s to a quoted, escaped string literal for embedding in a request
// body (so arbitrary content, e.g. an oversized filler, is a valid JSON value).
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// patchStatus PATCHes a schedule and returns only the HTTP status code (for the rejection
// paths where the body is an error, not a DTO).
func (f scheduleFixture) patchStatus(t *testing.T, user uuid.UUID, id, body string) int {
	t.Helper()
	req := userReq(http.MethodPatch, "/api/schedules/"+id, body, user, map[string]string{"id": id})
	rec := httptest.NewRecorder()
	f.h.PatchSchedule(rec, req)
	return rec.Code
}

// TestPatchDefaultPromptGuidanceLiveDB (issue #662) is the flagship for the owner-guidance
// overlay: a PROMPT default accepts an owner guidance PATCH, persists it (round-trips through
// the DTO over the catalog's empty guidance), sets customized, and an exact-restore (clearing
// guidance back to empty) un-customizes.
func TestPatchDefaultPromptGuidanceLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	dto, code := f.enableCatalog(t, f.owner.ID, f.repoID, "docs-hygiene")
	if code != http.StatusCreated {
		t.Fatalf("enable status = %d, want 201", code)
	}
	job, _ := schedtmpl.BySlug("docs-hygiene")
	// A freshly enabled default surfaces the catalog baseline guidance (empty for a prompt
	// catalog job), never any owner guidance yet.
	if got := guidanceVal(dto); got != job.Guidance {
		t.Fatalf("fresh prompt default guidance = %q, want the catalog baseline %q", got, job.Guidance)
	}

	const guidance = "Prefer small, reviewable diffs and skip generated files."
	patched := f.patchDefault(t, f.owner.ID, dto.ID, `{"guidance":`+jsonString(guidance)+`}`)
	if patched.Guidance == nil || *patched.Guidance != guidance {
		t.Fatalf("patched guidance = %v, want the owner guidance persisted", patched.Guidance)
	}
	if !patched.Customized {
		t.Fatal("guidance divergence: customized = false, want true")
	}
	// The prompt stays the RESOLVED catalog prompt (guidance does not touch it).
	if patched.Prompt != job.Prompt {
		t.Fatalf("patched prompt = %q, want catalog prompt unchanged", patched.Prompt)
	}

	// It really persisted: re-read via GET surfaces the stored guidance.
	got, gcode := f.getSchedule(t, f.owner.ID, dto.ID)
	if gcode != http.StatusOK {
		t.Fatalf("re-read status = %d, want 200", gcode)
	}
	if got.Guidance == nil || *got.Guidance != guidance {
		t.Fatalf("re-read guidance = %v, want the persisted owner guidance", got.Guidance)
	}

	// Clearing guidance back to empty un-customizes (exact-restore to the catalog baseline).
	restored := f.patchDefault(t, f.owner.ID, dto.ID, `{"guidance":""}`)
	if got := guidanceVal(restored); got != job.Guidance {
		t.Fatalf("cleared guidance = %q, want the catalog baseline %q", got, job.Guidance)
	}
	if restored.Customized {
		t.Fatal("cleared guidance: customized = true, want false (exact-restore un-customizes)")
	}
}

// TestPatchDefaultSweepGuidanceOverlayLiveDB (issue #675) is the sweep analogue of the prompt
// overlay flagship: a SWEEP default surfaces the catalog guidance as read-only BakedGuidance
// with a NULL overlay (Guidance nil, not customized); accepts an owner guidance OVERLAY PATCH
// that persists (round-trips through the DTO's Guidance while BakedGuidance stays the catalog
// value), sets customized; rejects an oversized overlay with a 422; and a Reset clears the
// overlay back to NULL and un-customizes, leaving BakedGuidance intact.
func TestPatchDefaultSweepGuidanceOverlayLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	dto, code := f.enableCatalog(t, f.owner.ID, f.repoID, "bug-triage")
	if code != http.StatusCreated {
		t.Fatalf("enable status = %d, want 201", code)
	}
	job, _ := schedtmpl.BySlug("bug-triage")
	if job.Guidance == "" {
		t.Fatal("bug-triage catalog entry has no guidance to discriminate on")
	}
	// A freshly enabled sweep default: the catalog guidance is the read-only BAKED value; the
	// owner overlay column is NULL (Guidance nil), and the row is not customized.
	if dto.BakedGuidance == nil || *dto.BakedGuidance != job.Guidance {
		t.Fatalf("fresh sweep default baked_guidance = %v, want the catalog guidance %q", dto.BakedGuidance, job.Guidance)
	}
	if dto.Guidance != nil {
		t.Fatalf("fresh sweep default overlay guidance = %v, want nil (NULL overlay)", dto.Guidance)
	}
	if dto.Customized {
		t.Fatal("fresh sweep default with a NULL overlay is customized, want false")
	}

	const overlay = "prefer table-driven tests"
	patched := f.patchDefault(t, f.owner.ID, dto.ID, `{"guidance":`+jsonString(overlay)+`}`)
	if patched.Guidance == nil || *patched.Guidance != overlay {
		t.Fatalf("patched overlay = %v, want the owner overlay persisted", patched.Guidance)
	}
	if patched.BakedGuidance == nil || *patched.BakedGuidance != job.Guidance {
		t.Fatalf("patched baked_guidance = %v, want the catalog guidance unchanged", patched.BakedGuidance)
	}
	if !patched.Customized {
		t.Fatal("overlay divergence: customized = false, want true")
	}

	// It really persisted: re-read via GET surfaces the stored overlay and the baked value.
	got, gcode := f.getSchedule(t, f.owner.ID, dto.ID)
	if gcode != http.StatusOK {
		t.Fatalf("re-read status = %d, want 200", gcode)
	}
	if got.Guidance == nil || *got.Guidance != overlay {
		t.Fatalf("re-read overlay = %v, want the persisted owner overlay", got.Guidance)
	}
	if got.BakedGuidance == nil || *got.BakedGuidance != job.Guidance {
		t.Fatalf("re-read baked_guidance = %v, want the catalog guidance", got.BakedGuidance)
	}

	// Oversized overlay → 422.
	huge := strings.Repeat("x", MaxGuidanceBytes+1)
	if code := f.patchStatus(t, f.owner.ID, dto.ID, `{"guidance":`+jsonString(huge)+`}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("oversized overlay patch status = %d, want 422", code)
	}

	// Reset clears the overlay back to NULL and un-customizes; the baked value survives.
	resetReq := userReq(http.MethodPost, "/api/schedules/"+dto.ID+"/reset", "", f.owner.ID, map[string]string{"id": dto.ID})
	resetRec := httptest.NewRecorder()
	f.h.ResetSchedule(resetRec, resetReq)
	if resetRec.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want 200 (body %s)", resetRec.Code, resetRec.Body.String())
	}
	var reset apitypes.ScheduleDTO
	if err := json.Unmarshal(resetRec.Body.Bytes(), &reset); err != nil {
		t.Fatalf("decode reset: %v", err)
	}
	if reset.Guidance != nil {
		t.Fatalf("reset overlay = %v, want nil (cleared back to NULL)", reset.Guidance)
	}
	if reset.BakedGuidance == nil || *reset.BakedGuidance != job.Guidance {
		t.Fatalf("reset baked_guidance = %v, want the catalog guidance intact", reset.BakedGuidance)
	}
	if reset.Customized {
		t.Fatal("reset customized = true, want false")
	}
}

// TestPatchDefaultPromptGuidanceTooLargeLiveDB (issue #662): a prompt default's guidance is
// still capped at MaxGuidanceBytes — over the cap is a 422.
func TestPatchDefaultPromptGuidanceTooLargeLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	dto, code := f.enableCatalog(t, f.owner.ID, f.repoID, "docs-hygiene")
	if code != http.StatusCreated {
		t.Fatalf("enable status = %d, want 201", code)
	}
	huge := strings.Repeat("x", MaxGuidanceBytes+1)
	if got := f.patchStatus(t, f.owner.ID, dto.ID, `{"guidance":`+jsonString(huge)+`}`); got != http.StatusUnprocessableEntity {
		t.Fatalf("oversized guidance patch status = %d, want 422", got)
	}
}

// TestPatchDefaultPromptLocksNonGuidanceLiveDB (issue #662): opening guidance on a prompt
// default does not unlock the other catalog-owned fields — prompt/labels/target still 400.
func TestPatchDefaultPromptLocksNonGuidanceLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	dto, code := f.enableCatalog(t, f.owner.ID, f.repoID, "docs-hygiene")
	if code != http.StatusCreated {
		t.Fatalf("enable status = %d, want 201", code)
	}
	for _, body := range []string{
		`{"prompt":"hijack the baked prompt"}`,
		`{"labels":["bug"]}`,
		`{"target":"sweep"}`,
	} {
		if got := f.patchStatus(t, f.owner.ID, dto.ID, body); got != http.StatusBadRequest {
			t.Fatalf("patch %s status = %d, want 400", body, got)
		}
	}
}

// TestResetPromptDefaultClearsGuidanceLiveDB (issue #662): Reset of a prompt default with
// stored owner guidance drops it back to the catalog baseline (NULL).
func TestResetPromptDefaultClearsGuidanceLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	dto, code := f.enableCatalog(t, f.owner.ID, f.repoID, "docs-hygiene")
	if code != http.StatusCreated {
		t.Fatalf("enable status = %d, want 201", code)
	}
	patched := f.patchDefault(t, f.owner.ID, dto.ID, `{"guidance":"owner guidance to be reset"}`)
	if patched.Guidance == nil {
		t.Fatal("precondition: guidance did not persist before reset")
	}

	resetReq := userReq(http.MethodPost, "/api/schedules/"+dto.ID+"/reset", "", f.owner.ID, map[string]string{"id": dto.ID})
	resetRec := httptest.NewRecorder()
	f.h.ResetSchedule(resetRec, resetReq)
	if resetRec.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want 200 (body %s)", resetRec.Code, resetRec.Body.String())
	}
	var reset apitypes.ScheduleDTO
	if err := json.Unmarshal(resetRec.Body.Bytes(), &reset); err != nil {
		t.Fatalf("decode reset: %v", err)
	}
	job, _ := schedtmpl.BySlug("docs-hygiene")
	// Reset drops the stored owner guidance, so the DTO falls back to the catalog baseline.
	if got := guidanceVal(reset); got != job.Guidance {
		t.Fatalf("reset guidance = %q, want the catalog baseline %q", got, job.Guidance)
	}
	if reset.Customized {
		t.Fatal("reset customized = true, want false")
	}

	// And a fresh GET confirms the stored column is truly cleared (not merely a resolved DTO).
	got, gcode := f.getSchedule(t, f.owner.ID, dto.ID)
	if gcode != http.StatusOK {
		t.Fatalf("re-read status = %d, want 200", gcode)
	}
	if v := guidanceVal(got); v != job.Guidance {
		t.Fatalf("re-read guidance = %q, want the catalog baseline %q", v, job.Guidance)
	}
}

// patchDefault PATCHes a schedule and returns the decoded DTO, failing on a non-200.
func (f scheduleFixture) patchDefault(t *testing.T, user uuid.UUID, id, body string) apitypes.ScheduleDTO {
	t.Helper()
	req := userReq(http.MethodPatch, "/api/schedules/"+id, body, user, map[string]string{"id": id})
	rec := httptest.NewRecorder()
	f.h.PatchSchedule(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var dto apitypes.ScheduleDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	return dto
}
