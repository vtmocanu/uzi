package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/schedsvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// pauseForTest flips the instance-global pause through the same store seam the admin route
// uses and lifts it on cleanup, so a failing test cannot leave the instance paused.
func pauseForTest(ctx context.Context, t *testing.T, pool *pgxpool.Pool) func(active bool, reason string) {
	t.Helper()
	set := func(active bool, reason string) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.SetInstanceIntakePause(ctx, store.New(tx), active, reason, pgtype.UUID{}, "test"); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	set(false, "")
	t.Cleanup(func() { set(false, "") })
	return set
}

// PRD #3454: POST /api/repos/{id}/runs (CreateRun) carries the additive notice only while
// maintenance is active, driven through the real handler against Postgres.
func TestCreateRunRouteNoticeLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newStartRunGuardFixture(ctx, t)
	pause := pauseForTest(ctx, t, f.pool)

	decode := func(w *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body %s)", w.Code, w.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	repoOff := f.seedEnabledRepoWithIssue(ctx, t, 5301, 7, protClean)
	if body := decode(f.createRun(t, repoOff, 7)); body["notice"] != nil {
		t.Errorf("create while lifted carries notice %v", body["notice"])
	}

	pause(true, "node drain")
	repoOn := f.seedEnabledRepoWithIssue(ctx, t, 5302, 7, protClean)
	body := decode(f.createRun(t, repoOn, 7))
	n, _ := body["notice"].(string)
	if !strings.Contains(n, "maintenance mode") || !strings.Contains(n, "Reason: node drain") {
		t.Errorf("create while paused notice = %q", n)
	}
	if body["run"] == nil {
		t.Errorf("response lost the run: %v", body)
	}
}

// PRD #3454: run-now sets the notice only when it actually started runs.
func TestRunScheduleNowNoticeOnlyWhenStartedLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)
	pause := pauseForTest(ctx, t, f.pool)

	pause(true, "node drain")
	d, code := f.enableCatalog(t, f.owner.ID, f.repoID, "ondeck-sweep")
	if code != http.StatusCreated {
		t.Fatalf("enable catalog = %d", code)
	}
	mustExecT(ctx, t, f.pool, `INSERT INTO issues(repo_id,forge_issue_iid,title,state,labels,web_url,forge_updated_at,synced_at) VALUES($1,1,'next','opened','["on-deck","uzi"]','https://forge.example/1',now(),now())`, f.repoID)
	ff := &ondeckForge{labels: []string{"on-deck", "uzi"}}
	f.h.scheduler = schedsvc.New(store.New(f.pool), ondeckRuns{Service: f.h.wsvc, f: f}, ondeckBuilder{Service: f.h.svc, f: ff}, f.h.settings, nil, nil, time.Minute, nil)
	runNow := func() apitypes.RunNowResponse {
		t.Helper()
		w := httptest.NewRecorder()
		f.h.RunScheduleNow(w, userReq(http.MethodPost, "/api/schedules/"+d.ID+"/run-now", "", f.owner.ID, map[string]string{"id": d.ID}))
		if w.Code != http.StatusAccepted {
			t.Fatalf("run-now = %d %s", w.Code, w.Body.String())
		}
		var out apitypes.RunNowResponse
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	started := runNow()
	if len(started.Started) != 1 || started.Notice == nil || !strings.Contains(*started.Notice, "Reason: node drain") {
		t.Fatalf("run-now that started a run: %+v", started)
	}
	drained := runNow() // the sweep label was consumed: nothing starts
	if len(drained.Started) != 0 || drained.Notice != nil {
		t.Fatalf("run-now that started nothing carries a notice: %+v", drained)
	}
}
