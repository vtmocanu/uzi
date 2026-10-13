package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #1733: Stop on a provider_unavailable hold is the existing two-step (POST /inputs kind=stop
// writes the scope ceiling, then the owner's resume re-queues the run), driven here through the
// real handlers against a REAL Postgres. A hold with no completed milestone refuses the stop with
// a 409 naming the remaining choices, and the refusal changes nothing. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// providerHold seeds a paused issue run holding on provider_unavailable with the frozen
// milestones m1..m3, the first `completed` of them complete, and an open provider-park streak.
func (e *wallParkExtendHTTPEnv) providerHold(completed int) uuid.UUID {
	e.t.Helper()
	e.iid++
	id := uuid.New()
	frozen := `[{"id":"m1","title":"m1"},{"id":"m2","title":"m2"},{"id":"m3","title":"m3"}]`
	done := make([]string, 0, completed)
	for _, m := range []string{"m1", "m2", "m3"}[:completed] {
		done = append(done, fmt.Sprintf("%q", m))
	}
	e.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id,
		hold_reason, milestones_frozen, milestones_completed, provider_park_count, provider_park_streak_started_at,
		started_at, status_since, claim_generation)
		VALUES ($1,$2,$3,'issue',$4,'t','d','paused',$5,'provider_unavailable',$6::jsonb,$7::jsonb,3,now()-interval '25 hours',
		now()-interval '26 hours',now()-interval '1 minute',1)`,
		id, e.owner.ID, e.repoID, e.iid, e.worker, frozen, "["+strings.Join(done, ",")+"]")
	return id
}

func (e *wallParkExtendHTTPEnv) post(handler func(http.ResponseWriter, *http.Request), id uuid.UUID, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/runs/"+id.String(), strings.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id.String())
	req = req.WithContext(context.WithValue(mw.ContextWithUser(req.Context(), e.owner), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

type providerHoldRow struct {
	status, hold string
	ceiling      *int32
	count        int32
	startValid   bool
}

func (e *wallParkExtendHTTPEnv) providerHoldRow(id uuid.UUID) providerHoldRow {
	e.t.Helper()
	var r providerHoldRow
	var hold *string
	if err := e.query(`SELECT status, hold_reason, scope_ceiling, provider_park_count, provider_park_streak_started_at IS NOT NULL FROM runs WHERE id=$1`, id).
		Scan(&r.status, &hold, &r.ceiling, &r.count, &r.startValid); err != nil {
		e.t.Fatal(err)
	}
	if hold != nil {
		r.hold = *hold
	}
	return r
}

// TestProviderHoldStopWithoutCompletedMilestone409LiveDB: Stop on a hold with nothing complete is
// a 409 carrying the sentence that names resume and cancel; the hold, its streak and the scope
// ceiling are unchanged.
func TestProviderHoldStopWithoutCompletedMilestone409LiveDB(t *testing.T) {
	e := newWallParkExtendHTTPEnv(t, 57600)
	id := e.providerHold(0)

	rec := e.post(e.h.CreateRunInput, id, `{"kind":"stop"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if msg, _ := body["error"].(string); msg != workersvc.ErrProviderHoldStopUnavailable.Error() {
		t.Fatalf("error = %q, want %q", msg, workersvc.ErrProviderHoldStopUnavailable.Error())
	}
	if r := e.providerHoldRow(id); r.status != "paused" || r.hold != "provider_unavailable" || r.ceiling != nil || r.count != 3 || !r.startValid {
		t.Fatalf("a refused stop changed the hold: %+v", r)
	}
}

// TestProviderHoldStopThenResumeLiveDB: with two milestones complete, Stop caps the scope at two
// and leaves the hold paused; the owner's resume then re-queues the run with the ceiling kept and
// the streak cleared.
func TestProviderHoldStopThenResumeLiveDB(t *testing.T) {
	e := newWallParkExtendHTTPEnv(t, 57600)
	id := e.providerHold(2)

	rec := e.post(e.h.CreateRunInput, id, `{"kind":"stop"}`)
	if rec.Code >= 300 {
		t.Fatalf("stop: code = %d; body=%s", rec.Code, rec.Body.String())
	}
	r := e.providerHoldRow(id)
	if r.status != "paused" || r.hold != "provider_unavailable" || r.ceiling == nil || *r.ceiling != 2 {
		t.Fatalf("after stop: %+v, want the hold kept with scope_ceiling 2", r)
	}

	rec = e.post(e.h.ResumeRunNow, id, ``)
	if rec.Code != http.StatusOK {
		t.Fatalf("resume: code = %d; body=%s", rec.Code, rec.Body.String())
	}
	r = e.providerHoldRow(id)
	if r.status != "queued" || r.ceiling == nil || *r.ceiling != 2 || r.count != 0 || r.startValid {
		t.Fatalf("after resume: %+v, want queued with scope_ceiling 2 and the streak cleared", r)
	}
}

// TestWorkerRunOwnershipReportsProviderHoldLiveDB (PRD #1733): the ownership probe carries
// hold_reason, additively, only for a paused run with a reason, so a worker that lost the ack of a
// provider park can recognise the hold. A running run omits the key.
func TestWorkerRunOwnershipReportsProviderHoldLiveDB(t *testing.T) {
	e := newWallParkExtendHTTPEnv(t, 57600)
	probe := func(id uuid.UUID) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/worker/runs/"+id.String()+"/ownership", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", id.String())
		ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
		req = req.WithContext(mw.ContextWithWorker(ctx, store.Worker{ID: e.worker, UserID: e.owner.ID}))
		rec := httptest.NewRecorder()
		e.h.WorkerRunOwnership(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("ownership: code = %d; body=%s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	hold := probe(e.providerHold(0))
	if hold["status"] != "paused" || hold["hold_reason"] != "provider_unavailable" {
		t.Fatalf("provider hold probe = %v, want paused with hold_reason provider_unavailable", hold)
	}

	e.iid++
	running := uuid.New()
	e.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, claim_generation)
		VALUES ($1,$2,$3,'issue',$4,'t','d','running',$5,1)`, running, e.owner.ID, e.repoID, e.iid, e.worker)
	if body := probe(running); body["status"] != "running" {
		t.Fatalf("running probe = %v", body)
	} else if body["hold_reason"] != nil {
		// main's ownership body always carries hold_reason (null without a hold).
		t.Fatalf("a running run's probe carries a hold_reason: %v", body)
	}
}
