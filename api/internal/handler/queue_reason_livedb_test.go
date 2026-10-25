package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #3454 (Visibility): queue_reason is derived in SQL at read time, so the run read, the run
// list, the board's latest run and the /api/v1 job all agree with the pause. Live-DB because the
// derivation is fn_instance_intake_paused / fn_run_admitted_during_intake_pause. The pause is
// instance-global; every test lifts it on cleanup.

func setQueueReasonPause(t *testing.T, pool *pgxpool.Pool, active bool) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.SetInstanceIntakePause(ctx, store.New(tx), active, "queue reason test", pgtype.UUID{}, "test"); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("SetInstanceIntakePause(%v): %v", active, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// queueReasonOf extracts the queue_reason key of a JSON object: "" when absent or null.
func queueReasonOf(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("not an object: %s", raw)
	}
	v, ok := m["queue_reason"]
	if !ok || string(v) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		t.Fatalf("queue_reason %s: %v", v, err)
	}
	return s
}

func TestQueueReasonLiveDB(t *testing.T) {
	e := newV1JobsEnv(t, 0)
	owner, uzc := e.user()
	repoID := cliSeedOwnedRepo(t, e.pool, owner)
	setQueueReasonPause(t, e.pool, false) // explicit baseline: an absent singleton row reads as paused
	t.Cleanup(func() { setQueueReasonPause(t, e.pool, false) })

	seedRun := func(iid int, status string, claimGeneration int) uuid.UUID {
		id := uuid.New()
		cliMustExec(t, e.pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, claim_generation)
			 VALUES ($1, $2, $3, $4, 't', 'd', $5, 'issue', $6)`, id, owner, repoID, iid, status, claimGeneration)
		return id
	}
	held := seedRun(1, "queued", 0)     // a new root: held back while paused
	admitted := seedRun(2, "queued", 1) // already claimed once: still admitted
	running := seedRun(3, "running", 1)
	job := e.create(uzc, v1MinimalJob)
	jobID := uuid.MustParse(job.ID)
	if job.QueueReason != nil {
		t.Fatalf("job created before the pause carries queue_reason %q", *job.QueueReason)
	}

	runGet := func(id uuid.UUID) string {
		r := e.call(http.MethodGet, "/api/runs/"+id.String(), uzc, "")
		if r.status != http.StatusOK {
			t.Fatalf("GET run %s = %d %s", id, r.status, r.body)
		}
		var env struct {
			Run json.RawMessage `json:"run"`
		}
		if err := json.Unmarshal(r.body, &env); err != nil || len(env.Run) == 0 {
			t.Fatalf("GET run %s body has no run object: %s", id, r.body)
		}
		return queueReasonOf(t, env.Run)
	}
	runList := func() map[uuid.UUID]string {
		r := e.call(http.MethodGet, "/api/runs/", uzc, "")
		if r.status != http.StatusOK {
			t.Fatalf("GET runs = %d %s", r.status, r.body)
		}
		var resp struct {
			Runs []json.RawMessage `json:"runs"`
		}
		if err := json.Unmarshal(r.body, &resp); err != nil {
			t.Fatal(err)
		}
		out := map[uuid.UUID]string{}
		for _, raw := range resp.Runs {
			var id struct {
				ID uuid.UUID `json:"id"`
			}
			if err := json.Unmarshal(raw, &id); err != nil {
				t.Fatal(err)
			}
			out[id.ID] = queueReasonOf(t, raw)
		}
		return out
	}
	v1Get := func(id uuid.UUID) apitypes.V1JobDTO {
		r := e.call(http.MethodGet, "/api/v1/jobs/"+id.String(), uzc, "")
		if r.status != http.StatusOK {
			t.Fatalf("GET job = %d %s", r.status, r.body)
		}
		var dto apitypes.V1JobDTO
		r.decode(t, &dto)
		return dto
	}
	v1List := func() map[string]*string {
		r := e.call(http.MethodGet, "/api/v1/jobs", uzc, "")
		if r.status != http.StatusOK {
			t.Fatalf("GET jobs = %d %s", r.status, r.body)
		}
		var dto apitypes.V1JobListDTO
		r.decode(t, &dto)
		out := map[string]*string{}
		for _, j := range dto.Jobs {
			out[j.ID] = j.QueueReason
		}
		return out
	}
	adminTok := cliMintToken(t, e.pool, cliSeedUser(t, e.pool, true), clitoken.ScopeAdminRO)
	adminSummary := func() map[uuid.UUID]string {
		r := e.call(http.MethodGet, "/api/admin/runs?view=summary", adminTok, "")
		if r.status != http.StatusOK {
			t.Fatalf("GET admin runs summary = %d %s", r.status, r.body)
		}
		var resp struct {
			Runs []json.RawMessage `json:"runs"`
		}
		if err := json.Unmarshal(r.body, &resp); err != nil {
			t.Fatal(err)
		}
		out := map[uuid.UUID]string{}
		for _, raw := range resp.Runs {
			var id struct {
				ID uuid.UUID `json:"id"`
			}
			if err := json.Unmarshal(raw, &id); err != nil {
				t.Fatal(err)
			}
			out[id.ID] = queueReasonOf(t, raw)
		}
		return out
	}
	check := func(phase string, pausedHeld bool) {
		t.Helper()
		want := ""
		if pausedHeld {
			want = "maintenance"
		}
		for name, c := range map[string]struct {
			id   uuid.UUID
			want string
		}{"held": {held, want}, "admitted": {admitted, ""}, "running": {running, ""}, "job": {jobID, want}} {
			if got := runGet(c.id); got != c.want {
				t.Errorf("%s: GET run %s queue_reason = %q, want %q", phase, name, got, c.want)
			}
		}
		list := runList()
		for name, c := range map[string]struct {
			id   uuid.UUID
			want string
		}{"held": {held, want}, "admitted": {admitted, ""}, "running": {running, ""}} {
			got, ok := list[c.id]
			if !ok {
				t.Errorf("%s: run list lacks %s run %s", phase, name, c.id)
			} else if got != c.want {
				t.Errorf("%s: list %s queue_reason = %q, want %q", phase, name, got, c.want)
			}
		}
		admin := adminSummary()
		for name, c := range map[string]struct {
			id   uuid.UUID
			want string
		}{"held": {held, want}, "admitted": {admitted, ""}} {
			got, ok := admin[c.id]
			if !ok {
				t.Errorf("%s: admin summary lacks %s run %s", phase, name, c.id)
			} else if got != c.want {
				t.Errorf("%s: admin summary %s queue_reason = %q, want %q", phase, name, got, c.want)
			}
		}
		dto := v1Get(jobID)
		if dto.Status != "queued" {
			t.Errorf("%s: v1 job status = %q, want queued (status enum is unchanged)", phase, dto.Status)
		}
		got := ""
		if dto.QueueReason != nil {
			got = *dto.QueueReason
		}
		if got != want {
			t.Errorf("%s: v1 job queue_reason = %q, want %q", phase, got, want)
		}
		lj := v1List()[job.ID]
		got = ""
		if lj != nil {
			got = *lj
		}
		if got != want {
			t.Errorf("%s: v1 job list queue_reason = %q, want %q", phase, got, want)
		}
	}

	check("before the pause", false)
	setQueueReasonPause(t, e.pool, true)
	check("paused", true)

	// Example 3: a job created during the pause is accepted, queued, and says why.
	created := e.create(uzc, v1MinimalJob)
	if created.Status != "queued" || created.QueueReason == nil || *created.QueueReason != "maintenance" {
		t.Errorf("job created during the pause = status %q queue_reason %v, want queued / maintenance", created.Status, created.QueueReason)
	}

	setQueueReasonPause(t, e.pool, false)
	check("after the lift", false)
	if got := v1Get(uuid.MustParse(created.ID)); got.QueueReason != nil {
		t.Errorf("job created during the pause still carries queue_reason %q after the lift", *got.QueueReason)
	}
}

// The board's latest run (list projection) takes queue_reason from the same SQL derivation as the
// single-card projection, which TestQueueReasonMoveIssueLiveDB covers.
func TestQueueReasonBoardLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newBoardOrderFixture(ctx, t, 1, 2, 3)
	setQueueReasonPause(t, f.pool, false) // explicit baseline: an absent singleton row reads as paused
	t.Cleanup(func() { setQueueReasonPause(t, f.pool, false) })
	for iid, c := range map[int64]struct {
		status string
		gen    int
	}{1: {"queued", 0}, 2: {"queued", 1}, 3: {"running", 1}} {
		mustExecT(ctx, t, f.pool,
			`INSERT INTO runs (user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, claim_generation)
			 VALUES ($1, $2, 'issue', $3, 't', 'd', $4, $5)`, f.owner.ID, f.repoID, iid, c.status, c.gen)
	}
	reasons := func() map[int64]string {
		t.Helper()
		w := f.call(t, f.owner, f.repoID, `{"iids":[1,2,3]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("board = %d %s", w.Code, w.Body.String())
		}
		var resp struct {
			Board struct {
				Cards []struct {
					IID       int64           `json:"iid"`
					LatestRun json.RawMessage `json:"latest_run"`
				} `json:"cards"`
			} `json:"board"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		out := map[int64]string{}
		for _, c := range resp.Board.Cards {
			out[c.IID] = queueReasonOf(t, c.LatestRun)
		}
		return out
	}
	want := func(phase string, got map[int64]string, w1 string) {
		t.Helper()
		if len(got) != 3 || got[1] != w1 || got[2] != "" || got[3] != "" {
			t.Errorf("%s: board queue_reason by iid = %v, want 1:%q 2:\"\" 3:\"\"", phase, got, w1)
		}
	}
	want("before the pause", reasons(), "")
	setQueueReasonPause(t, f.pool, true)
	want("paused", reasons(), "maintenance")
	setQueueReasonPause(t, f.pool, false)
	want("after the lift", reasons(), "")
}

// The single-card response (MoveIssue) carries the latest run's queue_reason too, so a drag never
// blanks the badge the board is showing.
func TestQueueReasonMoveIssueLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newMoveFixture(ctx, t, &moveStub{})
	setQueueReasonPause(t, f.pool, false) // explicit baseline: an absent singleton row reads as paused
	t.Cleanup(func() { setQueueReasonPause(t, f.pool, false) })
	mustExecT(ctx, t, f.pool,
		`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
		 VALUES ($1, 320, 't', 'opened', '[]'::jsonb, 'https://x', false, now(), now())`, f.repoID)
	mustExecT(ctx, t, f.pool,
		`INSERT INTO runs (user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, claim_generation)
		 VALUES ($1, $2, 'issue', 320, 't', 'd', 'queued', 0)`, f.user.ID, f.repoID)

	move := func() string {
		t.Helper()
		rr := httptest.NewRecorder()
		f.h.MoveIssue(rr, boardWriterReq(f.user, f.repoID, "320", `{"to_column":"Planned"}`))
		if rr.Code != http.StatusOK {
			t.Fatalf("move status = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
		var resp struct {
			Card struct {
				LatestRun json.RawMessage `json:"latest_run"`
			} `json:"card"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil || len(resp.Card.LatestRun) == 0 {
			t.Fatalf("move body has no card.latest_run: %v %s", err, rr.Body.String())
		}
		return queueReasonOf(t, resp.Card.LatestRun)
	}
	if got := move(); got != "" {
		t.Errorf("before the pause: single-card queue_reason = %q, want none", got)
	}
	setQueueReasonPause(t, f.pool, true)
	if got := move(); got != "maintenance" {
		t.Errorf("paused: single-card queue_reason = %q, want maintenance", got)
	}
}
