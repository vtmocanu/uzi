package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// End-to-end coverage for the PRD #636 M1 sibling-group work against a real Postgres: the
// create-time sibling_group_id, the transactional /add-repo endpoint (coalesce + copy), the
// partial-unique-index 409, delete hygiene, and the repoint rejection. Owner-scoping and the
// COALESCE race-safety live in the SQL, so these run through the real *store.Queries.
//
// Skipped unless UZI_TEST_DATABASE_URL is set; ./e2e/run-store-it.sh provides one.

// addRepo POSTs to /api/schedules/{id}/add-repo and returns the decoded sibling DTO + status.
func (f scheduleFixture) addRepo(t *testing.T, user uuid.UUID, id, repoID string) (apitypes.ScheduleDTO, int) {
	t.Helper()
	req := userReq(http.MethodPost, "/api/schedules/"+id+"/add-repo", `{"repo_id":"`+repoID+`"}`,
		user, map[string]string{"id": id})
	rec := httptest.NewRecorder()
	f.h.AddScheduleRepo(rec, req)
	var dto apitypes.ScheduleDTO
	if rec.Code == http.StatusCreated {
		if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
			t.Fatalf("decode add-repo response: %v (body %s)", err, rec.Body.String())
		}
	}
	return dto, rec.Code
}

// countReposSchedules returns how many run_schedules the owner has on a given repo — the
// discriminating check for the duplicate-add-repo 409 (no second row created).
func (f scheduleFixture) countReposSchedules(ctx context.Context, t *testing.T, user, repoID uuid.UUID) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM run_schedules WHERE user_id = $1 AND repo_id = $2`, user, repoID).Scan(&n); err != nil {
		t.Fatalf("count schedules: %v", err)
	}
	return n
}

// A plain create stores NO group id (a standalone single-repo row); a create carrying a
// sibling_group_id stores it verbatim (PRD #636 Decision 4).
func TestCreateScheduleSiblingGroupLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	plain, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"standalone","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	if plain.SiblingGroupID != nil {
		t.Fatalf("plain create sibling_group_id = %v, want nil (standalone)", plain.SiblingGroupID)
	}

	group := uuid.NewString()
	grouped, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"grouped","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC","sibling_group_id":"`+group+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("grouped create status = %d, want 201", code)
	}
	if grouped.SiblingGroupID == nil || *grouped.SiblingGroupID != group {
		t.Fatalf("grouped create sibling_group_id = %v, want %s", grouped.SiblingGroupID, group)
	}

	// A malformed sibling_group_id is a 400, not a 500 from the insert.
	bad, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"bad","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC","sibling_group_id":"not-a-uuid"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("malformed group create status = %d, want 400 (dto %+v)", code, bad)
	}
}

// add-repo on a NULL-group source coalesces: the source gets a fresh group id and the new
// sibling on the target repo shares it (PRD #636 Decision 5).
func TestAddScheduleRepoCoalescesNullGroupLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	src, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"my job","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	if src.SiblingGroupID != nil {
		t.Fatal("source started grouped, want standalone")
	}
	repoB := f.insertRepo(ctx, t, f.owner, 2, "g/sib-b")

	sib, code := f.addRepo(t, f.owner.ID, src.ID, repoB.String())
	if code != http.StatusCreated {
		t.Fatalf("add-repo status = %d, want 201", code)
	}
	if sib.ID == src.ID {
		t.Fatal("add-repo returned the source id, want a new sibling")
	}
	if sib.RepoID != repoB.String() {
		t.Fatalf("sibling repo = %q, want the target %q", sib.RepoID, repoB.String())
	}
	if sib.SiblingGroupID == nil {
		t.Fatal("sibling has no group id, want the coalesced group")
	}
	// The source now carries the SAME group id (coalesced under the row lock).
	refetched, code := f.getSchedule(t, f.owner.ID, src.ID)
	if code != http.StatusOK {
		t.Fatalf("get source status = %d, want 200", code)
	}
	if refetched.SiblingGroupID == nil || *refetched.SiblingGroupID != *sib.SiblingGroupID {
		t.Fatalf("source group = %v, sibling group = %v, want equal", refetched.SiblingGroupID, sib.SiblingGroupID)
	}
	// The copied config carries over (prompt, cadence).
	if sib.Prompt != "my job" || sib.CronExpr != "0 2 * * *" {
		t.Fatalf("sibling config = %+v, want a copy of the source", sib)
	}
}

// add-repo on an already-grouped source makes the new row JOIN the existing group rather than
// mint a fresh one (the COALESCE keeps the source's id).
func TestAddScheduleRepoJoinsExistingGroupLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	group := uuid.NewString()
	src, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"grouped","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC","sibling_group_id":"`+group+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	repoB := f.insertRepo(ctx, t, f.owner, 2, "g/sib-b")

	sib, code := f.addRepo(t, f.owner.ID, src.ID, repoB.String())
	if code != http.StatusCreated {
		t.Fatalf("add-repo status = %d, want 201", code)
	}
	if sib.SiblingGroupID == nil || *sib.SiblingGroupID != group {
		t.Fatalf("sibling group = %v, want the source's existing group %s", sib.SiblingGroupID, group)
	}
}

// A duplicate add-repo (the same target repo already a sibling in the group) hits the partial
// unique index → 409, and NO second row is created (idempotent-safe).
func TestAddScheduleRepoDuplicate409LiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	src, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"dup","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	repoB := f.insertRepo(ctx, t, f.owner, 2, "g/sib-b")

	if _, code := f.addRepo(t, f.owner.ID, src.ID, repoB.String()); code != http.StatusCreated {
		t.Fatalf("first add-repo status = %d, want 201", code)
	}
	req := userReq(http.MethodPost, "/api/schedules/"+src.ID+"/add-repo", `{"repo_id":"`+repoB.String()+`"}`,
		f.owner.ID, map[string]string{"id": src.ID})
	rec := httptest.NewRecorder()
	f.h.AddScheduleRepo(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate add-repo status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	if n := f.countReposSchedules(ctx, t, f.owner.ID, repoB); n != 1 {
		t.Fatalf("schedules on target repo = %d, want 1 (no second row from the 409)", n)
	}
}

// A foreign source (owned by the stranger) and a foreign target repo both 404.
func TestAddScheduleRepoForeign404LiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	src, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"owned","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	repoB := f.insertRepo(ctx, t, f.owner, 2, "g/sib-b")

	// Foreign source: the stranger cannot add a repo to the owner's schedule.
	req := userReq(http.MethodPost, "/api/schedules/"+src.ID+"/add-repo", `{"repo_id":"`+repoB.String()+`"}`,
		f.stranger.ID, map[string]string{"id": src.ID})
	rec := httptest.NewRecorder()
	f.h.AddScheduleRepo(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign-source add-repo status = %d, want 404", rec.Code)
	}

	// Foreign target repo: a repo the owner does not own is a 404.
	strangerRepo := f.insertRepo(ctx, t, f.stranger, 3, "g/sib-stranger")
	if _, code := f.addRepo(t, f.owner.ID, src.ID, strangerRepo.String()); code != http.StatusNotFound {
		t.Fatalf("foreign-target add-repo status = %d, want 404", code)
	}
}

// Deleting one sibling of a two-member group clears the group id off the sole survivor so it
// renders as a standalone row (PRD #636 Decision 3 delete hygiene).
func TestDeleteScheduleClearsSingletonGroupLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	src, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"grp","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	repoB := f.insertRepo(ctx, t, f.owner, 2, "g/sib-b")
	sib, code := f.addRepo(t, f.owner.ID, src.ID, repoB.String())
	if code != http.StatusCreated {
		t.Fatalf("add-repo status = %d, want 201", code)
	}

	// Delete the sibling; the source drops to the sole live member of the group.
	delReq := userReq(http.MethodDelete, "/api/schedules/"+sib.ID, "", f.owner.ID, map[string]string{"id": sib.ID})
	delRec := httptest.NewRecorder()
	f.h.DeleteSchedule(delRec, delReq)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204 (body %s)", delRec.Code, delRec.Body.String())
	}

	survivor, code := f.getSchedule(t, f.owner.ID, src.ID)
	if code != http.StatusOK {
		t.Fatalf("get survivor status = %d, want 200", code)
	}
	if survivor.SiblingGroupID != nil {
		t.Fatalf("survivor group = %v, want nil (cleared to standalone)", survivor.SiblingGroupID)
	}
}

// A repoint (UpdateRunSchedule) of one sibling onto a repo already occupied by another sibling
// in the same group is rejected by the partial unique index. Exercised at the store level (the
// M1 handler PATCH does not yet special-case the 23505); this proves the index does its job.
func TestSiblingRepointRejectedByIndexLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	src, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"grp","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	repoB := f.insertRepo(ctx, t, f.owner, 2, "g/sib-b")
	sib, code := f.addRepo(t, f.owner.ID, src.ID, repoB.String())
	if code != http.StatusCreated {
		t.Fatalf("add-repo status = %d, want 201", code)
	}

	// Fetch the sibling row and repoint it onto repoA (already occupied by the source in this
	// group) via the real UpdateRunSchedule — the unique index must reject it (23505).
	sibID := uuid.MustParse(sib.ID)
	row, err := f.h.q.GetRunScheduleForUser(ctx, store.GetRunScheduleForUserParams{ID: sibID, UserID: f.owner.ID})
	if err != nil {
		t.Fatalf("fetch sibling row: %v", err)
	}
	_, err = f.h.q.UpdateRunSchedule(ctx, store.UpdateRunScheduleParams{
		Target:                row.Target,
		RepoID:                f.repoID, // repoA, where the source sibling already lives in this group
		IssueIid:              row.IssueIid,
		Labels:                row.Labels,
		Prompt:                row.Prompt,
		Timing:                row.Timing,
		CronExpr:              row.CronExpr,
		RunAt:                 row.RunAt,
		Timezone:              row.Timezone,
		NextFireAt:            row.NextFireAt,
		AutoApprove:           row.AutoApprove,
		WaitOnLimit:           row.WaitOnLimit,
		MaxIssues:             row.MaxIssues,
		Guidance:              row.Guidance,
		Model:                 row.Model,
		OverrideSubagentModel: row.OverrideSubagentModel,
		Customized:            row.Customized,
		ID:                    sibID,
		UserID:                f.owner.ID,
	})
	if err == nil {
		t.Fatal("repoint onto an occupied sibling repo succeeded, want a unique-index rejection")
	}
	if !isUniqueViolation(err) {
		t.Fatalf("repoint error = %v, want a 23505 unique violation", err)
	}
}

// A repoint through the REAL handler (PATCH /api/schedules/{id}, UpdateSchedule) of one sibling
// onto a repo already occupied by another sibling in the same group is rejected as a clean 409,
// not a generic 500 (PRD #636 Decision 10). Complements the store-level
// TestSiblingRepointRejectedByIndexLiveDB above, which proves the index; this proves the handler
// maps the 23505 to StatusConflict and leaves no duplicate/corruption behind.
func TestSiblingRepointRejectedByHandler409LiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	// Group spanning repoA (the fixture repo) + repoB.
	src, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"grp","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	repoB := f.insertRepo(ctx, t, f.owner, 2, "g/sib-b")
	sib, code := f.addRepo(t, f.owner.ID, src.ID, repoB.String())
	if code != http.StatusCreated {
		t.Fatalf("add-repo status = %d, want 201", code)
	}

	// Repoint the repoB sibling onto repoA (already occupied by the source in this group) via a
	// real PATCH through UpdateSchedule — the handler must return 409, not 500.
	_, code = f.patchSchedule(t, f.owner.ID, sib.ID, `{"repo_id":"`+f.repoID.String()+`"}`)
	if code != http.StatusConflict {
		t.Fatalf("handler repoint status = %d, want 409", code)
	}

	// No corruption: the sibling still lives on repoB (the rejected repoint did not partially
	// apply), and repoA still carries exactly the one source row (no duplicate created).
	survivor, code := f.getSchedule(t, f.owner.ID, sib.ID)
	if code != http.StatusOK {
		t.Fatalf("get sibling status = %d, want 200", code)
	}
	if survivor.RepoID != repoB.String() {
		t.Fatalf("sibling repo after rejected repoint = %q, want unchanged %q", survivor.RepoID, repoB.String())
	}
	if n := f.countReposSchedules(ctx, t, f.owner.ID, f.repoID); n != 1 {
		t.Fatalf("schedules on repoA = %d, want 1 (rejected repoint created no duplicate)", n)
	}
}

// Delete hygiene must NOT clear a group that still has ≥2 live members: the count(*)=1 gate in
// ClearSingletonSiblingGroup only collapses a group down to a lone survivor (PRD #636 Decision 3).
// Build a 3-member group (repoA/B/C), delete ONE, and assert the two survivors still share their
// non-null group id.
func TestDeleteKeepsMultiMemberGroupLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	src, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"grp","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	repoB := f.insertRepo(ctx, t, f.owner, 2, "g/sib-b")
	repoC := f.insertRepo(ctx, t, f.owner, 3, "g/sib-c")
	sibB, code := f.addRepo(t, f.owner.ID, src.ID, repoB.String())
	if code != http.StatusCreated {
		t.Fatalf("add-repo B status = %d, want 201", code)
	}
	sibC, code := f.addRepo(t, f.owner.ID, src.ID, repoC.String())
	if code != http.StatusCreated {
		t.Fatalf("add-repo C status = %d, want 201", code)
	}

	// Delete one member (the repoC sibling); the group still has 2 live members (source + sibB).
	delReq := userReq(http.MethodDelete, "/api/schedules/"+sibC.ID, "", f.owner.ID, map[string]string{"id": sibC.ID})
	delRec := httptest.NewRecorder()
	f.h.DeleteSchedule(delRec, delReq)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204 (body %s)", delRec.Code, delRec.Body.String())
	}

	// Both survivors STILL carry the same non-null group id (the ≥2-member group was left intact).
	survivorA, code := f.getSchedule(t, f.owner.ID, src.ID)
	if code != http.StatusOK {
		t.Fatalf("get survivor A status = %d, want 200", code)
	}
	survivorB, code := f.getSchedule(t, f.owner.ID, sibB.ID)
	if code != http.StatusOK {
		t.Fatalf("get survivor B status = %d, want 200", code)
	}
	if survivorA.SiblingGroupID == nil || survivorB.SiblingGroupID == nil {
		t.Fatalf("survivor groups = %v / %v, want both non-nil (multi-member group untouched)",
			survivorA.SiblingGroupID, survivorB.SiblingGroupID)
	}
	if *survivorA.SiblingGroupID != *survivorB.SiblingGroupID {
		t.Fatalf("survivor groups = %v / %v, want equal", survivorA.SiblingGroupID, survivorB.SiblingGroupID)
	}
}

// Regression canary (PRD #636 M1): a pre-existing ungrouped schedule still scans/loads fine
// after the additive column + index. Not a discriminating test (schedsvc reads none of the
// column) — it only proves the SELECT */RETURNING * struct scan did not break.
func TestUngroupedScheduleStillLoadsLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	src, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"legacy","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	got, code := f.getSchedule(t, f.owner.ID, src.ID)
	if code != http.StatusOK {
		t.Fatalf("get status = %d, want 200", code)
	}
	if got.ID != src.ID || got.SiblingGroupID != nil {
		t.Fatalf("ungrouped reload = %+v, want the same row with a nil group", got)
	}
	// It also appears in the owner's list without error (the ListRunSchedulesForUser scan).
	listReq := userReq(http.MethodGet, "/api/me/schedules", "", f.owner.ID, nil)
	listRec := httptest.NewRecorder()
	f.h.ListMySchedules(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200 (body %s)", listRec.Code, listRec.Body.String())
	}
}

// add-repo on an issue-target source is rejected 422 (issue #638 P1a): issue_iid is
// repo-relative, so copying it onto a sibling repo would point at a different, unrelated
// issue. The rejection fires BEFORE any row is created, so the target repo stays empty.
func TestAddScheduleRepoIssueTargetRejectedLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	src, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"issue","issue_iid":7,"timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	repoB := f.insertRepo(ctx, t, f.owner, 2, "g/iss-b")

	if _, code := f.addRepo(t, f.owner.ID, src.ID, repoB.String()); code != http.StatusUnprocessableEntity {
		t.Fatalf("add-repo status = %d, want 422 (issue-target rejection)", code)
	}
	if n := f.countReposSchedules(ctx, t, f.owner.ID, repoB); n != 0 {
		t.Fatalf("schedules on target repo = %d, want 0 (rejection created no sibling)", n)
	}
}

// add-repo onto an ALREADY-grouped source must not bump the source's updated_at (issue #638
// P3): the coalesce is a no-op once the source carries a group id, so the CTE UPDATE does not
// fire and updated_at stays put. Adding the FIRST sibling legitimately moves it (the source
// gains its group id there); adding a later sibling must not.
func TestAddScheduleRepoKeepsSourceUpdatedAtLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	src, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"grp","timing":"recurring","cron_expr":"0 2 * * *","timezone":"UTC"}`)
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	srcID, err := uuid.Parse(src.ID)
	if err != nil {
		t.Fatalf("parse source id: %v", err)
	}

	// Adding the first sibling groups the source: its updated_at legitimately moves here.
	repoB := f.insertRepo(ctx, t, f.owner, 2, "g/upd-b")
	if _, code := f.addRepo(t, f.owner.ID, src.ID, repoB.String()); code != http.StatusCreated {
		t.Fatalf("add-repo B status = %d, want 201", code)
	}
	grouped, code := f.getSchedule(t, f.owner.ID, src.ID)
	if code != http.StatusOK {
		t.Fatalf("get grouped source status = %d, want 200", code)
	}
	if grouped.SiblingGroupID == nil {
		t.Fatal("source has no group id after adding the first sibling, want it grouped")
	}

	var before time.Time
	if err := f.pool.QueryRow(ctx,
		`SELECT updated_at FROM run_schedules WHERE id = $1`, srcID).Scan(&before); err != nil {
		t.Fatalf("read source updated_at: %v", err)
	}

	// Adding a SECOND sibling: the coalesce is a no-op (source already grouped) and must not
	// bump updated_at.
	repoC := f.insertRepo(ctx, t, f.owner, 3, "g/upd-c")
	if _, code := f.addRepo(t, f.owner.ID, src.ID, repoC.String()); code != http.StatusCreated {
		t.Fatalf("add-repo C status = %d, want 201", code)
	}

	var after time.Time
	if err := f.pool.QueryRow(ctx,
		`SELECT updated_at FROM run_schedules WHERE id = $1`, srcID).Scan(&after); err != nil {
		t.Fatalf("read source updated_at after 3rd repo: %v", err)
	}
	if !after.Equal(before) {
		t.Fatalf("source updated_at moved from %v to %v after adding the 3rd repo, want unchanged (coalesce no-op)", before, after)
	}

	// The source still carries the same group id it had after adding the first sibling — the
	// coalesce still RETURNS the effective id even though it wrote nothing.
	still, code := f.getSchedule(t, f.owner.ID, src.ID)
	if code != http.StatusOK {
		t.Fatalf("get source status = %d, want 200", code)
	}
	if still.SiblingGroupID == nil || *still.SiblingGroupID != *grouped.SiblingGroupID {
		t.Fatalf("source group = %v, want unchanged %v", still.SiblingGroupID, grouped.SiblingGroupID)
	}
}
