package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// ci_watch schedule surface (PRD #2865) end-to-end through the handlers against a real
// Postgres: create/patch/reset/clone, the episode listing, and the schedule row lock that
// orders config edits against fire transitions. Skipped unless UZI_TEST_DATABASE_URL is set.

func (f scheduleFixture) createCIWatch(t *testing.T, extra string) apitypes.ScheduleDTO {
	t.Helper()
	body := `{"target":"ci_watch","timing":"recurring","cron_expr":"0 * * * *","timezone":"UTC"` + extra + `}`
	dto, code := f.createSchedule(t, f.owner.ID, f.repoID, body)
	if code != http.StatusCreated {
		t.Fatalf("create ci_watch status = %d, want 201", code)
	}
	return dto
}

func (f scheduleFixture) seedEpisodes(ctx context.Context, t *testing.T, id string, keys ...string) {
	t.Helper()
	for _, k := range keys {
		mustExecT(ctx, t, f.pool, `INSERT INTO ci_watch_episodes (schedule_id, workflow_key) VALUES ($1, $2)`, id, k)
	}
}

func (f scheduleFixture) episodePage(t *testing.T, user uuid.UUID, id, query string) (apitypes.CIEpisodePage, int) {
	t.Helper()
	req := userReq(http.MethodGet, "/api/schedules/"+id+"/ci-episodes"+query, "", user, map[string]string{"id": id})
	rec := httptest.NewRecorder()
	f.h.ListScheduleCIEpisodes(rec, req)
	var page apitypes.CIEpisodePage
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode page: %v (%s)", err, rec.Body.String())
		}
	}
	return page, rec.Code
}

func TestCIWatchCreateAndSelectorsLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	dto := f.createCIWatch(t, `,"ci_workflows":["nightly"," 123 "]`)
	if dto.AutoApprove {
		t.Fatal("ci_watch create must default auto_approve to false")
	}
	if dto.Prompt == "" {
		t.Fatal("blank prompt must default to the ci-repair catalog body")
	}
	if len(dto.CIWorkflows) != 2 || dto.CIWorkflows[0] != "nightly" || dto.CIWorkflows[1] != "123" {
		t.Fatalf("ci_workflows = %v", dto.CIWorkflows)
	}
	on := f.createCIWatch(t, `,"auto_approve":true`)
	if !on.AutoApprove {
		t.Fatal("explicit auto_approve=true must be honored")
	}

	// Presence: an unrelated edit keeps the list; [] clears; a list replaces; null clears.
	kept, code := f.patchSchedule(t, f.owner.ID, dto.ID, `{"cron_expr":"5 * * * *"}`)
	if code != http.StatusOK || len(kept.CIWorkflows) != 2 {
		t.Fatalf("absent ci_workflows: status %d list %v, want kept", code, kept.CIWorkflows)
	}
	repl, code := f.patchSchedule(t, f.owner.ID, dto.ID, `{"ci_workflows":["only"]}`)
	if code != http.StatusOK || len(repl.CIWorkflows) != 1 || repl.CIWorkflows[0] != "only" {
		t.Fatalf("replace: status %d list %v", code, repl.CIWorkflows)
	}
	cleared, code := f.patchSchedule(t, f.owner.ID, dto.ID, `{"ci_workflows":[]}`)
	if code != http.StatusOK || len(cleared.CIWorkflows) != 0 {
		t.Fatalf("[]: status %d list %v, want cleared", code, cleared.CIWorkflows)
	}
	f.patchSchedule(t, f.owner.ID, dto.ID, `{"ci_workflows":["again"]}`)
	nulled, code := f.patchSchedule(t, f.owner.ID, dto.ID, `{"ci_workflows":null}`)
	if code != http.StatusOK || len(nulled.CIWorkflows) != 0 {
		t.Fatalf("null: status %d list %v, want cleared", code, nulled.CIWorkflows)
	}
	if st := f.patchStatus(t, f.owner.ID, dto.ID, `{"ci_workflows":["a","a"]}`); st != http.StatusBadRequest {
		t.Fatalf("duplicate selectors status = %d, want 400", st)
	}

	// Rejected on other targets.
	if _, code := f.createSchedule(t, f.owner.ID, f.repoID,
		`{"target":"prompt","prompt":"p","timing":"recurring","cron_expr":"0 2 * * *","ci_workflows":["x"]}`); code != http.StatusBadRequest {
		t.Fatalf("ci_workflows on prompt create = %d, want 400", code)
	}
}

func TestCIWatchEpisodesEndpointLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)
	dto := f.createCIWatch(t, "")

	// Empty history: the endpoint emits [] and the detail carries an empty first page.
	page, code := f.episodePage(t, f.owner.ID, dto.ID, "")
	if code != http.StatusOK || page.Episodes == nil || len(page.Episodes) != 0 || page.NextCursor != nil {
		t.Fatalf("empty page = %+v status %d", page, code)
	}

	f.seedEpisodes(ctx, t, dto.ID, "c", "a", "b", "d")
	var seen []string
	query := "?limit=3"
	for i := 0; i < 5; i++ {
		page, code = f.episodePage(t, f.owner.ID, dto.ID, query)
		if code != http.StatusOK {
			t.Fatalf("page status %d", code)
		}
		for _, e := range page.Episodes {
			seen = append(seen, e.WorkflowKey)
		}
		if page.NextCursor == nil {
			break
		}
		query = "?limit=3&cursor=" + string(*page.NextCursor)
	}
	if fmt.Sprint(seen) != "[a b c d]" {
		t.Fatalf("paged keys = %v, want [a b c d] byte-ordered with no repeats", seen)
	}
	// A page that exactly fills the limit with nothing after it has no next cursor.
	page, _ = f.episodePage(t, f.owner.ID, dto.ID, "?limit=4")
	if len(page.Episodes) != 4 || page.NextCursor != nil {
		t.Fatalf("exact-fit page = %d entries cursor %v, want 4 and none", len(page.Episodes), page.NextCursor)
	}

	// Detail embeds the first page.
	got, code := f.getSchedule(t, f.owner.ID, dto.ID)
	if code != http.StatusOK || got.CIEpisodes == nil || len(got.CIEpisodes.Episodes) != 4 {
		t.Fatalf("detail ci_episodes = %+v", got.CIEpisodes)
	}
	// Other targets carry none.
	other, _ := f.createSchedule(t, f.owner.ID, f.repoID, `{"target":"prompt","prompt":"p","timing":"recurring","cron_expr":"0 2 * * *"}`)
	if g, _ := f.getSchedule(t, f.owner.ID, other.ID); g.CIEpisodes != nil {
		t.Fatalf("prompt schedule carries ci_episodes: %+v", g.CIEpisodes)
	}

	// Foreign schedule is a 404; bad cursor / limit are 400.
	if _, code = f.episodePage(t, f.stranger.ID, dto.ID, ""); code != http.StatusNotFound {
		t.Fatalf("foreign status = %d, want 404", code)
	}
	for _, q := range []string{"?cursor=%21%21", "?limit=0", "?limit=101", "?limit=x", "?limit=-1"} {
		if _, code = f.episodePage(t, f.owner.ID, dto.ID, q); code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", q, code)
		}
	}
}

func TestCIWatchRepointRefusedWithEpisodesLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)
	repo2 := f.insertRepo(ctx, t, f.owner, 2, "g/sched2")
	dto := f.createCIWatch(t, "")

	// No history yet: a repoint is allowed.
	moved, code := f.patchSchedule(t, f.owner.ID, dto.ID, `{"repo_id":"`+repo2.String()+`"}`)
	if code != http.StatusOK || moved.RepoID != repo2.String() {
		t.Fatalf("repoint without episodes: status %d repo %s", code, moved.RepoID)
	}
	f.seedEpisodes(ctx, t, dto.ID, "nightly")
	if st := f.patchStatus(t, f.owner.ID, dto.ID, `{"repo_id":"`+f.repoID.String()+`"}`); st != http.StatusConflict {
		t.Fatalf("repoint with episodes = %d, want 409", st)
	}
	if got, _ := f.getSchedule(t, f.owner.ID, dto.ID); got.RepoID != repo2.String() {
		t.Fatalf("refused repoint mutated the row: repo %s", got.RepoID)
	}
	// A target change keeps the episodes and is not a repoint.
	conv, code := f.patchSchedule(t, f.owner.ID, dto.ID, `{"target":"prompt","prompt":"p"}`)
	if code != http.StatusOK || conv.Target != "prompt" {
		t.Fatalf("convert away: status %d target %s", code, conv.Target)
	}
	var n int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM ci_watch_episodes WHERE schedule_id = $1`, dto.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("episodes after target change = %d (%v), want 1 kept", n, err)
	}
	// Repointing it stays refused: the history belongs to the recorded repo.
	if st := f.patchStatus(t, f.owner.ID, dto.ID, `{"repo_id":"`+f.repoID.String()+`"}`); st != http.StatusConflict {
		t.Fatalf("repoint after conversion = %d, want 409", st)
	}
}

// waitBlocked polls until some backend is blocked by the holder's pid, read from pg_locks
// (an ungranted lock) and pg_blocking_pids, so the test proves the PATCH is queued behind the
// schedule row lock rather than assuming it.
func waitBlocked(ctx context.Context, t *testing.T, f scheduleFixture, holderPID int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var blocked, ungranted int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, holderPID).Scan(&blocked); err != nil {
			t.Fatalf("pg_blocking_pids: %v", err)
		}
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
			WHERE NOT l.granted AND $1 = ANY(pg_blocking_pids(a.pid))`, holderPID).Scan(&ungranted); err != nil {
			t.Fatalf("pg_locks: %v", err)
		}
		if blocked > 0 && ungranted > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("PATCH never queued behind the held schedule row lock")
}

func TestCIWatchPatchWaitsForScheduleLockLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)
	repo2 := f.insertRepo(ctx, t, f.owner, 2, "g/sched2")
	dto := f.createCIWatch(t, "")
	id := uuid.MustParse(dto.ID)

	// Stand in for a fire transition: hold the owner-scoped row lock.
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := store.New(f.pool).WithTx(tx)
	if _, err := qtx.LockRunScheduleForUser(ctx, store.LockRunScheduleForUserParams{ID: id, UserID: f.owner.ID}); err != nil {
		t.Fatal(err)
	}
	var holderPID int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatal(err)
	}

	type result struct {
		code int
		dto  apitypes.ScheduleDTO
	}
	done := make(chan result, 1)
	go func() {
		d, c := f.patchSchedule(t, f.owner.ID, dto.ID, `{"repo_id":"`+repo2.String()+`"}`)
		done <- result{c, d}
	}()
	waitBlocked(ctx, t, f, holderPID)
	select {
	case r := <-done:
		t.Fatalf("PATCH returned %d while the schedule lock was held", r.code)
	default:
	}

	// The "fire" records its first episode under the lock, then commits. The PATCH must
	// observe it: the repoint check runs under the lock, after the fire's transition.
	if _, err := tx.Exec(ctx, `INSERT INTO ci_watch_episodes (schedule_id, workflow_key) VALUES ($1, 'nightly')`, id); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.code != http.StatusConflict {
			t.Fatalf("PATCH after fire = %d, want 409 (repoint refused under the lock)", r.code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("PATCH did not complete after the lock was released")
	}
	if got, _ := f.getSchedule(t, f.owner.ID, dto.ID); got.RepoID != f.repoID.String() {
		t.Fatalf("repo changed to %s despite the 409", got.RepoID)
	}
}

func TestCIWatchResetWaitsForScheduleLockLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)
	def, code := f.enableCatalog(t, f.owner.ID, f.repoID, "ci-repair")
	if code != http.StatusCreated {
		t.Fatalf("enable ci-repair = %d, want 201", code)
	}
	f.patchDefault(t, f.owner.ID, def.ID, `{"ci_workflows":["nightly"]}`)
	id := uuid.MustParse(def.ID)

	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := store.New(f.pool).WithTx(tx).LockRunScheduleForUser(ctx, store.LockRunScheduleForUserParams{ID: id, UserID: f.owner.ID}); err != nil {
		t.Fatal(err)
	}
	var holderPID int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		req := userReq(http.MethodPost, "/api/schedules/"+def.ID+"/reset", "", f.owner.ID, map[string]string{"id": def.ID})
		rec := httptest.NewRecorder()
		f.h.ResetSchedule(rec, req)
		done <- rec.Code
	}()
	waitBlocked(ctx, t, f, holderPID)
	select {
	case c := <-done:
		t.Fatalf("reset returned %d while the schedule lock was held", c)
	default:
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-done:
		if c != http.StatusOK {
			t.Fatalf("reset = %d, want 200", c)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reset did not complete after the lock was released")
	}
	got, _ := f.getSchedule(t, f.owner.ID, def.ID)
	if len(got.CIWorkflows) != 0 || got.AutoApprove || got.Customized {
		t.Fatalf("reset row = workflows %v auto %v customized %v, want none/false/false", got.CIWorkflows, got.AutoApprove, got.Customized)
	}
}

func TestCIRepairCatalogAutoApproveOffLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	// Every other default still enables with auto-approve on.
	other, code := f.enableCatalog(t, f.owner.ID, f.repoID, "docs-hygiene")
	if code != http.StatusCreated || !other.AutoApprove {
		t.Fatalf("docs-hygiene enable = %d auto %v, want 201/true", code, other.AutoApprove)
	}

	def, code := f.enableCatalog(t, f.owner.ID, f.repoID, "ci-repair")
	if code != http.StatusCreated {
		t.Fatalf("enable ci-repair = %d, want 201", code)
	}
	if def.AutoApprove || def.Target != "ci_watch" || def.Customized {
		t.Fatalf("enabled ci-repair = auto %v target %s customized %v, want false/ci_watch/false", def.AutoApprove, def.Target, def.Customized)
	}
	if def.Prompt == "" {
		t.Fatal("default ci-repair DTO must surface the resolved catalog prompt")
	}

	// Owner guidance and selectors are editable and customize the row; auto-approve on does too.
	g := f.patchDefault(t, f.owner.ID, def.ID, `{"guidance":"be careful","ci_workflows":["nightly"]}`)
	if !g.Customized || len(g.CIWorkflows) != 1 || guidanceVal(g) != "be careful" {
		t.Fatalf("patched default = %+v", g)
	}
	if st := f.patchStatus(t, f.owner.ID, def.ID, `{"ci_workflows":["a","a"]}`); st != http.StatusBadRequest {
		t.Fatalf("default duplicate selectors = %d, want 400", st)
	}

	// A clone keeps auto-approve off, the selectors, and bakes the catalog prompt.
	clone, code := f.cloneSchedule(t, f.owner.ID, def.ID, "")
	if code != http.StatusCreated {
		t.Fatalf("clone = %d, want 201", code)
	}
	if clone.AutoApprove || clone.Target != "ci_watch" || clone.Origin != "user" || clone.Prompt == "" || len(clone.CIWorkflows) != 1 {
		t.Fatalf("clone = auto %v target %s origin %s prompt %q workflows %v", clone.AutoApprove, clone.Target, clone.Origin, clone.Prompt, clone.CIWorkflows)
	}

	// Reset restores auto-approve off and drops selectors/overlay.
	req := userReq(http.MethodPost, "/api/schedules/"+def.ID+"/reset", "", f.owner.ID, map[string]string{"id": def.ID})
	rec := httptest.NewRecorder()
	f.h.ResetSchedule(rec, req)
	var reset apitypes.ScheduleDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &reset)
	if rec.Code != http.StatusOK || reset.AutoApprove || reset.Customized || len(reset.CIWorkflows) != 0 {
		t.Fatalf("reset = %d auto %v customized %v workflows %v", rec.Code, reset.AutoApprove, reset.Customized, reset.CIWorkflows)
	}
}
