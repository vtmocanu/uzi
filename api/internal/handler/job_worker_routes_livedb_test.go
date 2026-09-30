package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// job_worker_routes_livedb_test.go covers PRD #1908's worker route surface for job runs through
// the real worker router: the not_for_job refusals and the job-result ingest. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway database; run via ./e2e/run-store-it.sh.

type jobRouteEnv struct {
	pool   *pgxpool.Pool
	router http.Handler
	user   uuid.UUID
	worker uuid.UUID
	tokens map[uuid.UUID]string
}

func setupJobRouteLiveDB(t *testing.T) *jobRouteEnv {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)
	svc := workersvc.New(q, newHandlerTestBox(t), workersvc.Params{})
	svc.SetTxBeginner(pool)
	h := &Handler{q: q, wsvc: svc}
	e := &jobRouteEnv{pool: pool, router: h.WorkerRoutes(mw.NewLimiter(1000, time.Minute, nil)), tokens: map[uuid.UUID]string{}}
	e.user = e.seedUser(t)
	e.worker = e.seedWorker(t, e.user)
	return e
}

func (e *jobRouteEnv) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func (e *jobRouteEnv) seedUser(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	e.exec(t, `INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x')`, id, fmt.Sprintf("jobroute-%s@e2e", id))
	return id
}

func (e *jobRouteEnv) seedWorker(t *testing.T, user uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	token, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	e.tokens[id] = token
	e.exec(t, `INSERT INTO workers (id,user_id,name,token_hash,protocol_capabilities) VALUES ($1,$2,$3,$4,'{}')`, id, user, "jobroute-"+id.String(), hash)
	return id
}

// seedJob inserts a repo-less running job held by worker at claim generation 1.
func (e *jobRouteEnv) seedJob(t *testing.T, user, worker uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	e.exec(t, `INSERT INTO runs (id, user_id, kind, job_type, issue_title, issue_description, auto_approve, required_capabilities,
	                             status, worker_id, budget_wall_seconds, started_at, claimed_at, claim_generation)
	           VALUES ($1, $2, 'job', 'research', 'job', 'prompt', true, '{}', 'running', $3, 600, now(), now(), 1)`, id, user, worker)
	return id
}

// seedChat inserts a running chat run held by worker: the non-job control (a chat run needs no
// repo, so it is the cheapest run kind that is not a job).
func (e *jobRouteEnv) seedChat(t *testing.T, user, worker uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	e.exec(t, `INSERT INTO runs (id,user_id,kind,issue_title,issue_description,status,worker_id,claim_generation) VALUES ($1,$2,'chat','t','d','running',$3,1)`, id, user, worker)
	return id
}

// do sends one request as worker and returns the status (0 when the handler panicked after the
// refusal middleware let it through, which the control cases read as "reached the handler": the
// bare Handler under test has no forge service) and the decoded JSON body.
func (e *jobRouteEnv) do(worker uuid.UUID, method, path, body string) (code int, out map[string]any) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token, ok := e.tokens[worker]; ok {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	func() {
		defer func() {
			if recover() != nil {
				code = 0
			}
		}()
		e.router.ServeHTTP(rec, req)
		code = rec.Code
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}()
	return code, out
}

// jobRefusedRoutes are the worker routes PRD #1908 refuses for a job run. reviewed marks the routes
// whose {id} is the REVIEWED run rather than the run the worker holds.
var jobRefusedRoutes = []struct {
	name, method, path string
	reviewed           bool
}{
	{"publish", "POST", "/publish", false},
	{"memory save", "POST", "/memory", false},
	{"memory list", "GET", "/memory", false},
	{"forge issue", "GET", "/forge/issues/1", false},
	{"forge issues", "GET", "/forge/issues", false},
	{"forge label events", "GET", "/forge/issues/1/label-events", false},
	{"forge merge request", "GET", "/forge/merge-requests/1", false},
	{"forge pipeline jobs", "GET", "/forge/pipelines/1/jobs", false},
	{"forge latest pipeline", "GET", "/forge/latest-pipeline", false},
	{"mr thread reply", "POST", "/forge/mr-threads/reply", false},
	{"mr thread resolve", "POST", "/forge/mr-threads/resolve", false},
	{"trace", "GET", "/trace", true},
	{"review", "POST", "/review", true},
	{"task review", "POST", "/task-review", true},
}

// TestWorkerJobRouteRefusalsLiveDB: every worker route that reaches third-party content or a forge
// answers 403 not_for_job when the run is a job, and answers as before (not 403) for another kind.
// For trace, review and task-review the run in the path is the REVIEWED run.
//
// MUTATION CHECK: deleting the refuseJobRuns / refuseJobReviewTargets mounts in handler.go
// (or the guard's body) reddens the job cases with a non-403 status.
func TestWorkerJobRouteRefusalsLiveDB(t *testing.T) {
	e := setupJobRouteLiveDB(t)
	held := e.seedJob(t, e.user, e.worker)
	heldChat := e.seedChat(t, e.user, e.worker)
	// A job of ANOTHER user, held by that user's worker: this worker must learn nothing about it.
	otherUser := e.seedUser(t)
	otherWorker := e.seedWorker(t, otherUser)
	foreignJob := e.seedJob(t, otherUser, otherWorker)
	// A job of this user that another worker of the same user holds: a reviewed-run route still
	// refuses it, an owned-run route does not (this worker does not hold it).
	sibling := e.seedWorker(t, e.user)
	siblingJob := e.seedJob(t, e.user, sibling)

	for _, rt := range jobRefusedRoutes {
		t.Run(rt.name, func(t *testing.T) {
			path := func(id uuid.UUID) string { return "/api/worker/runs/" + id.String() + rt.path }
			code, out := e.do(e.worker, rt.method, path(held), `{}`)
			if code != http.StatusForbidden || out["reason"] != "not_for_job" {
				t.Fatalf("job run: status %d body %v, want 403 not_for_job", code, out)
			}
			if code, _ := e.do(e.worker, rt.method, path(heldChat), `{}`); code == http.StatusForbidden {
				t.Fatalf("non-job run: status 403, want the route's own answer")
			}
			if code, _ := e.do(e.worker, rt.method, path(foreignJob), `{}`); code == http.StatusForbidden {
				t.Fatalf("another user's job: status 403, the refusal must not reveal a foreign run's kind")
			}
			code, _ = e.do(e.worker, rt.method, path(siblingJob), `{}`)
			if rt.reviewed && code != http.StatusForbidden {
				t.Fatalf("reviewed job run held elsewhere: status %d, want 403", code)
			}
			if !rt.reviewed && code == http.StatusForbidden {
				t.Fatalf("a job another worker holds: status 403, want the route's own answer")
			}
		})
	}
}

func (e *jobRouteEnv) jobResultCounts(t *testing.T, runID uuid.UUID) (results, findings int) {
	t.Helper()
	ctx := context.Background()
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM job_results WHERE run_id = $1`, runID).Scan(&results); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM job_findings WHERE run_id = $1`, runID).Scan(&findings); err != nil {
		t.Fatal(err)
	}
	return results, findings
}

// TestWorkerJobResultIngestLiveDB drives POST /api/worker/runs/{id}/job-result: validation
// refusals write nothing, the fences (claim generation, run kind, ownership) hold, and a repeat
// POST replaces the earlier findings.
func TestWorkerJobResultIngestLiveDB(t *testing.T) {
	e := setupJobRouteLiveDB(t)
	run := e.seedJob(t, e.user, e.worker)
	url := "/api/worker/runs/" + run.String() + "/job-result"
	post := func(body string) (int, map[string]any) { return e.do(e.worker, "POST", url, body) }
	finding := func(extra string) string {
		return `{"claim_generation":1,"status":"completed","report_md":"r","findings":[{"severity":"info","message_md":"m"` + extra + `}]}`
	}

	invalid := []struct{ name, body string }{
		{"bad severity", strings.Replace(finding(""), `"info"`, `"fatal"`, 1)},
		{"url and file", finding(`,"url":"https://e.com","file":"f.go"`)},
		{"line without file", finding(`,"line":4`)},
		{"javascript url", finding(`,"url":"javascript:alert(1)"`)},
		{"unknown field", `{"claim_generation":1,"status":"completed","report_md":"r","extra":1}`},
		{"unknown finding field", finding(`,"nope":1`)},
		{"missing generation", `{"status":"completed","report_md":"r"}`},
		{"bad status", `{"claim_generation":1,"status":"","report_md":"r"}`},
		{"oversize report", `{"claim_generation":1,"status":"completed","report_md":"` + strings.Repeat("r", workersvc.JobResultReportMaxBytes+1) + `"}`},
		{"oversize message", `{"claim_generation":1,"status":"completed","findings":[{"severity":"info","message_md":"` + strings.Repeat("m", workersvc.JobFindingMessageMaxBytes+1) + `"}]}`},
		{"trailing value", finding("") + ` {}`},
		{"not json", `nope`},
	}
	for _, tc := range invalid {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			if code, out := post(tc.body); code != http.StatusBadRequest {
				t.Fatalf("status %d body %v, want 400", code, out)
			}
			if r, f := e.jobResultCounts(t, run); r != 0 || f != 0 {
				t.Fatalf("a refused body wrote %d results, %d findings", r, f)
			}
		})
	}
	t.Run("a body over the request cap is 413", func(t *testing.T) {
		big := `{"claim_generation":1,"status":"completed","report_md":"` + strings.Repeat("r", jobResultMaxBodyBytes) + `"}`
		if code, _ := post(big); code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status %d, want 413", code)
		}
	})
	t.Run("stale claim generation is a 409 stale_claim", func(t *testing.T) {
		code, out := post(strings.Replace(finding(""), `"claim_generation":1`, `"claim_generation":7`, 1))
		if code != http.StatusConflict || out["disposition"] != "stale_claim" {
			t.Fatalf("status %d body %v, want 409 stale_claim", code, out)
		}
		if r, _ := e.jobResultCounts(t, run); r != 0 {
			t.Fatal("a stale post wrote a result")
		}
	})
	t.Run("a non-job run is refused not_for_job", func(t *testing.T) {
		chat := e.seedChat(t, e.user, e.worker)
		code, out := e.do(e.worker, "POST", "/api/worker/runs/"+chat.String()+"/job-result", finding(""))
		if code != http.StatusForbidden || out["reason"] != "not_for_job" {
			t.Fatalf("status %d body %v, want 403 not_for_job", code, out)
		}
		if r, _ := e.jobResultCounts(t, chat); r != 0 {
			t.Fatal("a refused non-job post wrote a result")
		}
	})
	t.Run("a worker not holding the run is 404, unauthenticated is 401", func(t *testing.T) {
		other := e.seedWorker(t, e.user)
		if code, _ := e.do(other, "POST", url, finding("")); code != http.StatusNotFound {
			t.Fatalf("status %d, want 404", code)
		}
		if code, _ := e.do(uuid.Nil, "POST", url, finding("")); code != http.StatusUnauthorized {
			t.Fatalf("status %d, want 401", code)
		}
	})
	t.Run("ownership and kind are checked before the body is read", func(t *testing.T) {
		// An over-cap body would be a 413 if the api read it first. A worker that does not hold the
		// run must get 404, and a non-job run 403 not_for_job, without the body being consulted.
		huge := `{"claim_generation":1,"status":"completed","report_md":"` + strings.Repeat("r", jobResultMaxBodyBytes+1024) + `"}`
		if code, _ := post(huge); code != http.StatusRequestEntityTooLarge {
			t.Fatalf("control: the holder's over-cap body: status %d, want 413", code)
		}
		other := e.seedWorker(t, e.user)
		if code, _ := e.do(other, "POST", url, huge); code != http.StatusNotFound {
			t.Fatalf("a worker not holding the run, over-cap body: status %d, want 404 (ownership before the body)", code)
		}
		chat := e.seedChat(t, e.user, e.worker)
		code, out := e.do(e.worker, "POST", "/api/worker/runs/"+chat.String()+"/job-result", huge)
		if code != http.StatusForbidden || out["reason"] != "not_for_job" {
			t.Fatalf("a non-job run, over-cap body: status %d body %v, want 403 not_for_job (kind before the body)", code, out)
		}
	})
	t.Run("a repeated findings key is refused", func(t *testing.T) {
		body := `{"claim_generation":1,"status":"completed","findings":[{"severity":"info","message_md":"a"}],"findings":[{"severity":"info","message_md":"b"}]}`
		if code, out := post(body); code != http.StatusBadRequest {
			t.Fatalf("status %d body %v, want 400", code, out)
		}
		if r, f := e.jobResultCounts(t, run); r != 0 || f != 0 {
			t.Fatalf("a refused body wrote %d results, %d findings", r, f)
		}
	})
	t.Run("a valid post persists and a repeat replaces the findings", func(t *testing.T) {
		first := `{"claim_generation":1,"status":"completed","report_md":"first","findings":[` +
			`{"severity":"info","message_md":"a"},{"severity":"error","message_md":"b","file":"x.go","line":3},` +
			`{"severity":"warning","message_md":"c","url":"https://example.com/p"}]}`
		if code, out := post(first); code != http.StatusOK {
			t.Fatalf("first post: status %d body %v", code, out)
		}
		if r, f := e.jobResultCounts(t, run); r != 1 || f != 3 {
			t.Fatalf("after first post: %d results, %d findings; want 1, 3", r, f)
		}
		second := `{"claim_generation":1,"status":"partial","report_md":"second","findings":[{"severity":"warning","message_md":"only"}]}`
		if code, out := post(second); code != http.StatusOK {
			t.Fatalf("second post: status %d body %v", code, out)
		}
		if r, f := e.jobResultCounts(t, run); r != 1 || f != 1 {
			t.Fatalf("after second post: %d results, %d findings; want 1, 1 (replaced)", r, f)
		}
		var status, report, msg string
		if err := e.pool.QueryRow(context.Background(), `SELECT status, report_md FROM job_results WHERE run_id=$1`, run).Scan(&status, &report); err != nil {
			t.Fatal(err)
		}
		if err := e.pool.QueryRow(context.Background(), `SELECT message_md FROM job_findings WHERE run_id=$1`, run).Scan(&msg); err != nil {
			t.Fatal(err)
		}
		if status != "partial" || report != "second" || msg != "only" {
			t.Fatalf("stored %q/%q/%q, want partial/second/only", status, report, msg)
		}
	})
}
