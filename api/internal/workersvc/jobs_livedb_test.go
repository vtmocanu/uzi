package workersvc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// jobs_livedb_test.go covers the PRD #1908 M2 job service against a live Postgres. Skipped
// unless UZI_TEST_DATABASE_URL points at a throwaway database; run via ./e2e/run-store-it.sh.

// jobCapSettings fake: only JobMaxActivePerUser is ever called by the create path.
type jobCapFake struct {
	Settings
	cap int
}

func (f jobCapFake) JobMaxActivePerUser(context.Context) (int, error) { return f.cap, nil }

type jobEnv struct {
	codexTestEnv
	svc *Service
}

func setupJobLiveDB(t *testing.T, cap int) jobEnv {
	t.Helper()
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool)
	if cap > 0 {
		svc.SetHealthSettings(jobCapFake{cap: cap})
	}
	return jobEnv{codexTestEnv: env, svc: svc}
}

// seedJobUser is a user with a usable Anthropic credential.
func (e jobEnv) seedJobUser(t *testing.T) uuid.UUID {
	t.Helper()
	u := e.seedUser(t)
	e.seedAnthropicToken(t, u, "job-"+uuid.NewString())
	return u
}

// seedProduct inserts a product allowing the given job types plus a token for the user.
func (e jobEnv) seedProduct(t *testing.T, userID uuid.UUID, allowed []string) (productID, tokenID uuid.UUID) {
	t.Helper()
	productID, tokenID = uuid.New(), uuid.New()
	if allowed == nil {
		allowed = []string{} // a nil slice binds NULL, which the NOT NULL column refuses
	}
	e.exec(`INSERT INTO products (id, name, allowed_job_types) VALUES ($1, $2, $3)`,
		productID, "prod-"+productID.String(), allowed)
	e.exec(`INSERT INTO product_tokens (id, user_id, product_id, name, token_hash, token_prefix, scopes)
	        VALUES ($1, $2, $3, 'tok', $4, 'uzp_test', ARRAY['jobs:run','jobs:read'])`,
		tokenID, userID, productID, []byte(tokenID.String()))
	return productID, tokenID
}

func cliCaller(u uuid.UUID) JobCaller { return JobCaller{UserID: u} }

func productCaller(u, product, token uuid.UUID) JobCaller {
	return JobCaller{UserID: u, ProductID: &product, ProductTokenID: &token}
}

func jobReq(c JobCaller) CreateJobParams {
	return CreateJobParams{Caller: c, JobType: runkind.JobTypeResearch, Title: "Summarize", Prompt: "read the inputs and summarize"}
}

func (e jobEnv) jobCount(t *testing.T, userID uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM runs WHERE user_id = $1 AND kind = 'job'`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateJobRunHappyLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	u := e.seedJobUser(t)
	label := "end-user-42"
	wall := 600
	p := jobReq(cliCaller(u))
	p.Inputs = []JobInput{{Name: "notes.md", Content: "alpha\x00beta"}, {Name: "data.csv", Content: "a,b"}}
	p.RequestedByLabel = &label
	p.WallSeconds = &wall

	v, err := e.svc.CreateJobRun(e.ctx, p)
	if err != nil {
		t.Fatalf("CreateJobRun: %v", err)
	}
	if v.Status != "queued" || v.JobType != "research" || v.Title != "Summarize" || v.ProductID != nil || v.ProductTokenID != nil {
		t.Fatalf("view = %+v", v)
	}
	run := mustRun(t, e.codexTestEnv, v.ID)
	if run.Kind != runkind.Job || !run.JobType.Valid || run.JobType.String != "research" || run.RepoID.Valid || run.IssueIid.Valid || run.Branch.Valid {
		t.Errorf("row shape: kind=%s job_type=%+v repo=%+v iid=%+v branch=%+v", run.Kind, run.JobType, run.RepoID, run.IssueIid, run.Branch)
	}
	if run.Harness != string(HarnessClaude) {
		t.Errorf("harness = %q, want claude", run.Harness)
	}
	if run.Status != "queued" || len(run.RequiredCapabilities) != 0 || !run.AutoApprove || run.CompletionContractVersion.Valid {
		t.Errorf("run = status %q caps %v auto_approve %v contract %+v", run.Status, run.RequiredCapabilities, run.AutoApprove, run.CompletionContractVersion)
	}
	if run.IssueTitle != "Summarize" || run.IssueDescription != p.Prompt {
		t.Errorf("title/prompt = %q / %q", run.IssueTitle, run.IssueDescription)
	}
	if !run.BudgetWallSeconds.Valid || run.BudgetWallSeconds.Int32 != 600 {
		t.Errorf("budget_wall_seconds = %+v, want 600", run.BudgetWallSeconds)
	}

	rows, err := e.pool.Query(e.ctx, `SELECT ordinal, name, content_md FROM job_inputs WHERE run_id = $1 ORDER BY ordinal`, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var ord int
		var name, content string
		if err := rows.Scan(&ord, &name, &content); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%d:%s:%s", ord, name, content))
	}
	if fmt.Sprint(got) != "[0:notes.md:alphabeta 1:data.csv:a,b]" {
		t.Errorf("inputs = %v", got)
	}

	var pid, tid *uuid.UUID
	var lbl *string
	if err := e.pool.QueryRow(e.ctx, `SELECT product_id, product_token_id, requested_by_label FROM job_origins WHERE run_id = $1`, v.ID).Scan(&pid, &tid, &lbl); err != nil {
		t.Fatal(err)
	}
	if pid != nil || tid != nil || lbl == nil || *lbl != label {
		t.Errorf("origin = product %v token %v label %v; a uzc_ caller must record neither product column", pid, tid, lbl)
	}
}

func TestCreateJobRunRefusalsLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 1)
	u := e.seedJobUser(t)

	// Over the cap: one active job fills a cap of 1.
	first, err := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u))); !errors.Is(err, ErrJobOverCap) {
		t.Fatalf("over cap err = %v, want ErrJobOverCap", err)
	}
	if n := e.jobCount(t, u); n != 1 {
		t.Fatalf("job rows after an over-cap refusal = %d, want 1", n)
	}
	// Another user is unaffected, and a finished job frees the slot.
	if _, err := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(e.seedJobUser(t)))); err != nil {
		t.Fatalf("other user create: %v", err)
	}
	if _, err := e.svc.CancelJob(e.ctx, cliCaller(u), first.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u))); err != nil {
		t.Fatalf("create after cancel: %v", err)
	}

	e2 := setupJobLiveDB(t, 0)
	u2 := e2.seedJobUser(t)
	before := e2.jobCount(t, u2)

	// Invalid and oversize.
	bad := jobReq(cliCaller(u2))
	bad.Inputs = []JobInput{{Name: "../etc/passwd", Content: "x"}}
	if _, err := e2.svc.CreateJobRun(e2.ctx, bad); !errors.Is(err, ErrJobInvalid) {
		t.Errorf("bad input name err = %v", err)
	}
	big := jobReq(cliCaller(u2))
	big.Prompt = string(make([]byte, MaxIssueDescriptionBytes+1))
	if _, err := e2.svc.CreateJobRun(e2.ctx, big); !errors.Is(err, ErrJobInvalid) {
		t.Errorf("oversize prompt err = %v", err)
	}
	// Unknown type, egress flag.
	unk := jobReq(cliCaller(u2))
	unk.JobType = "translate"
	if _, err := e2.svc.CreateJobRun(e2.ctx, unk); !errors.Is(err, ErrJobTypeUnknown) {
		t.Errorf("unknown type err = %v", err)
	}
	eg := jobReq(cliCaller(u2))
	eg.EgressProfile = true
	if _, err := e2.svc.CreateJobRun(e2.ctx, eg); !errors.Is(err, ErrJobNotSupported) {
		t.Errorf("egress err = %v", err)
	}

	// Product allow-list: a type outside it, and an empty list, both refuse.
	pAllowed, tAllowed := e2.seedProduct(t, u2, []string{"research"})
	pEmpty, tEmpty := e2.seedProduct(t, u2, nil)
	if _, err := e2.svc.CreateJobRun(e2.ctx, jobReq(productCaller(u2, pEmpty, tEmpty))); !errors.Is(err, ErrJobTypeNotAllowed) {
		t.Errorf("empty allow-list err = %v, want ErrJobTypeNotAllowed", err)
	}
	e2.exec(`UPDATE products SET allowed_job_types = '{}' WHERE id = $1`, pAllowed)
	if _, err := e2.svc.CreateJobRun(e2.ctx, jobReq(productCaller(u2, pAllowed, tAllowed))); !errors.Is(err, ErrJobTypeNotAllowed) {
		t.Errorf("emptied allow-list err = %v, want ErrJobTypeNotAllowed", err)
	}
	// A disabled product yields nothing either.
	pOff, tOff := e2.seedProduct(t, u2, []string{"research"})
	e2.exec(`UPDATE products SET enabled = false WHERE id = $1`, pOff)
	if _, err := e2.svc.CreateJobRun(e2.ctx, jobReq(productCaller(u2, pOff, tOff))); !errors.Is(err, ErrJobTypeNotAllowed) {
		t.Errorf("disabled product err = %v, want ErrJobTypeNotAllowed", err)
	}
	if n := e2.jobCount(t, u2); n != before {
		t.Fatalf("a refused create left %d job rows", n-before)
	}

	// No Anthropic credential: absent, then all disabled.
	noCred := e2.seedUser(t)
	_, err = e2.svc.CreateJobRun(e2.ctx, jobReq(cliCaller(noCred)))
	if !IsNoModelCredential(err) {
		t.Fatalf("no credential err = %v, want IsNoModelCredential", err)
	}
	tok := e2.seedAnthropicToken(t, noCred, "will-disable")
	disableSecret(e2.codexTestEnv, tok, true)
	_, err = e2.svc.CreateJobRun(e2.ctx, jobReq(cliCaller(noCred)))
	if !IsNoModelCredential(err) || !errors.Is(err, ErrHarnessCredentialDisabled) {
		t.Fatalf("disabled credential err = %v, want the disabled variant", err)
	}
	if n := e2.jobCount(t, noCred); n != 0 {
		t.Fatalf("no-credential refusal left %d job rows", n)
	}

	// A user holding only a Codex credential still gets no job: the harness pin is Claude.
	codexOnly := e2.seedUser(t)
	alias := e2.seedStaticAPIKey(t, codexOnly, "codex-key", "sk-fixture-"+uuid.NewString())
	makeCodexDefault(t, e2.codexTestEnv, alias)
	if _, err := e2.svc.CreateJobRun(e2.ctx, jobReq(cliCaller(codexOnly))); !IsNoModelCredential(err) {
		t.Fatalf("codex-only user err = %v, want IsNoModelCredential (a job never runs on Codex)", err)
	}
}

func TestCreateJobRunCallerKindsLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	u := e.seedJobUser(t)
	prod, tok := e.seedProduct(t, u, []string{"research"})

	pv, err := e.svc.CreateJobRun(e.ctx, jobReq(productCaller(u, prod, tok)))
	if err != nil {
		t.Fatalf("product create: %v", err)
	}
	if pv.ProductID == nil || *pv.ProductID != prod || pv.ProductTokenID == nil || *pv.ProductTokenID != tok {
		t.Errorf("view origin = %v/%v", pv.ProductID, pv.ProductTokenID)
	}
	var pid, tid uuid.UUID
	if err := e.pool.QueryRow(e.ctx, `SELECT product_id, product_token_id FROM job_origins WHERE run_id = $1`, pv.ID).Scan(&pid, &tid); err != nil || pid != prod || tid != tok {
		t.Fatalf("origin row = %v/%v err=%v", pid, tid, err)
	}

	// A uzc_ caller (no product) may create every known type regardless of any product's list.
	for _, jt := range runkind.JobTypes() {
		p := jobReq(cliCaller(u))
		p.JobType = jt
		if _, err := e.svc.CreateJobRun(e.ctx, p); err != nil {
			t.Errorf("uzc_ create of %q: %v", jt, err)
		}
	}
}

func TestJobReadVisibilityLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	owner := e.seedJobUser(t)
	other := e.seedJobUser(t)
	pA, tA := e.seedProduct(t, owner, []string{"research"})
	pB, tB := e.seedProduct(t, owner, []string{"research"})

	cliJob, err := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(owner)))
	if err != nil {
		t.Fatal(err)
	}
	aJob, err := e.svc.CreateJobRun(e.ctx, jobReq(productCaller(owner, pA, tA)))
	if err != nil {
		t.Fatal(err)
	}
	bJob, err := e.svc.CreateJobRun(e.ctx, jobReq(productCaller(owner, pB, tB)))
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`INSERT INTO run_messages (run_id, seq, kind, payload) VALUES ($1, 1, 'text', '{"text":"hi"}')`, aJob.ID)

	notFound := func(name string, _ any, err error) {
		t.Helper()
		if !errors.Is(err, ErrJobNotFound) {
			t.Errorf("%s err = %v, want ErrJobNotFound", name, err)
		}
	}
	// Another user sees nothing, through every read and cancel.
	oc := cliCaller(other)
	_, err = e.svc.GetJobForCaller(e.ctx, oc, cliJob.ID)
	notFound("other user get", nil, err)
	_, err = e.svc.GetJobResult(e.ctx, oc, cliJob.ID)
	notFound("other user result", nil, err)
	_, err = e.svc.ListJobMessages(e.ctx, oc, aJob.ID, 0, 10)
	notFound("other user messages", nil, err)
	_, err = e.svc.CancelJob(e.ctx, oc, cliJob.ID)
	notFound("other user cancel", nil, err)
	// Even a product token of the same product but a DIFFERENT user is refused.
	xp := productCaller(other, pA, uuid.New())
	_, err = e.svc.GetJobForCaller(e.ctx, xp, aJob.ID)
	notFound("other user same product get", nil, err)

	// A product-token caller sees only its own product's jobs.
	ca := productCaller(owner, pA, tA)
	if _, err := e.svc.GetJobForCaller(e.ctx, ca, aJob.ID); err != nil {
		t.Errorf("product A reads its job: %v", err)
	}
	_, err = e.svc.GetJobForCaller(e.ctx, ca, bJob.ID)
	notFound("product A reads B", nil, err)
	_, err = e.svc.GetJobForCaller(e.ctx, ca, cliJob.ID)
	notFound("product A reads a uzc_ job", nil, err)
	_, err = e.svc.GetJobResult(e.ctx, ca, bJob.ID)
	notFound("product A result of B", nil, err)
	_, err = e.svc.ListJobMessages(e.ctx, ca, bJob.ID, 0, 10)
	notFound("product A messages of B", nil, err)
	_, err = e.svc.CancelJob(e.ctx, ca, bJob.ID)
	notFound("product A cancels B", nil, err)
	if st := e.jobStatusRow(t, bJob.ID); st != "queued" {
		t.Errorf("a refused cross-product cancel changed B to %q", st)
	}
	jobs, _, err := e.svc.ListJobsForCaller(e.ctx, ca, nil, 50)
	if err != nil || len(jobs) != 1 || jobs[0].ID != aJob.ID {
		t.Errorf("product A list = %+v err=%v, want only its own job", jobs, err)
	}

	// The uzc_ owner sees all three.
	all, _, err := e.svc.ListJobsForCaller(e.ctx, cliCaller(owner), nil, 50)
	if err != nil || len(all) != 3 {
		t.Fatalf("owner list = %d err=%v, want 3", len(all), err)
	}
	if none, _, _ := e.svc.ListJobsForCaller(e.ctx, oc, nil, 50); len(none) != 0 {
		t.Errorf("other user list = %d, want 0", len(none))
	}
}

func (e jobEnv) jobStatusRow(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := e.pool.QueryRow(e.ctx, `SELECT status FROM runs WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCreateJobRunCapRaceLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 3)
	// Several rounds, each on a fresh user, so a lost race shows up as more than one success.
	for round := 0; round < 8; round++ {
		u := e.seedJobUser(t)
		for i := 0; i < 2; i++ { // cap-1 = 2 active
			if _, err := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u))); err != nil {
				t.Fatal(err)
			}
		}
		const racers = 8
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, racers)
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, errs[i] = e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u)))
			}(i)
		}
		close(start)
		wg.Wait()
		ok, over := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrJobOverCap):
				over++
			default:
				t.Errorf("unexpected err: %v", err)
			}
		}
		if ok != 1 || over != racers-1 {
			t.Fatalf("round %d: racing creates at cap-1: %d succeeded, %d over cap; want exactly 1 and %d", round, ok, over, racers-1)
		}
		if n := e.jobCount(t, u); n != 3 {
			t.Fatalf("round %d: job rows = %d, want exactly the cap (3)", round, n)
		}
	}
}

// TestCreateJobRunWaitsOnTheUserLockLiveDB is the deterministic half of the race proof: a
// transaction holding the per-user job-create lock blocks a create until it commits, and the
// create then counts the row that transaction inserted. Without LockJobCreate in the create
// transaction the create returns immediately and succeeds past the cap.
func TestCreateJobRunWaitsOnTheUserLockLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 1)
	u := e.seedJobUser(t)

	holder, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(e.ctx) }()
	hq := store.New(holder)
	if err := hq.LockJobCreate(e.ctx, u); err != nil {
		t.Fatal(err)
	}

	type result struct {
		v   JobView
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u)))
		done <- result{v, err}
	}()

	select {
	case r := <-done:
		t.Fatalf("CreateJobRun finished (%+v, %v) while another transaction held the user's job-create lock", r.v, r.err)
	case <-time.After(500 * time.Millisecond):
	}

	// The holder fills the cap and commits; the waiting create must now see it.
	if _, err := hq.CreateJobRun(e.ctx, store.CreateJobRunParams{
		RunID: uuid.New(), UserID: u, JobType: pgconv.TextOrNull("research"), IssueTitle: "held", IssueDescription: "p", Harness: "claude",
	}); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if !errors.Is(r.err, ErrJobOverCap) {
			t.Fatalf("waiting create = (%+v, %v), want ErrJobOverCap", r.v, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("CreateJobRun never resumed after the lock holder committed")
	}
}

func TestListJobsKeysetLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	u := e.seedJobUser(t)
	var ids []uuid.UUID
	for i := 0; i < 5; i++ {
		v, err := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u)))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, v.ID)
	}
	// Give three jobs the SAME created_at so the id tiebreak is exercised, and order the rest.
	base := time.Now().UTC().Truncate(time.Second)
	for i, id := range ids {
		at := base.Add(time.Duration(i/2) * time.Minute)
		e.exec(`UPDATE runs SET created_at = $2 WHERE id = $1`, id, at)
	}

	var seen []uuid.UUID
	var cursor *JobCursor
	pages := 0
	for {
		page, next, err := e.svc.ListJobsForCaller(e.ctx, cliCaller(u), cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, j := range page {
			seen = append(seen, j.ID)
		}
		if next == nil {
			break
		}
		cursor = next
		if pages > 10 {
			t.Fatal("cursor never ends")
		}
	}
	if len(seen) != 5 || pages != 3 {
		t.Fatalf("paged %d jobs in %d pages, want 5 in 3", len(seen), pages)
	}
	uniq := map[uuid.UUID]bool{}
	for _, id := range seen {
		uniq[id] = true
	}
	if len(uniq) != 5 {
		t.Fatalf("keyset repeated or skipped rows: %v", seen)
	}
	all, next, _ := e.svc.ListJobsForCaller(e.ctx, cliCaller(u), nil, 100)
	if next != nil || len(all) != 5 {
		t.Fatalf("single page = %d next=%v", len(all), next)
	}
	for i := 1; i < len(all); i++ {
		a, b := all[i-1], all[i]
		if a.CreatedAt.Before(b.CreatedAt) || (a.CreatedAt.Equal(b.CreatedAt) && a.ID.String() < b.ID.String()) {
			t.Fatalf("not ordered (created_at, id) descending at %d: %v then %v", i, a, b)
		}
	}
}

func TestCancelJobLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	u := e.seedJobUser(t)
	c := cliCaller(u)
	v, err := e.svc.CreateJobRun(e.ctx, jobReq(c))
	if err != nil {
		t.Fatal(err)
	}

	// Extending a job is refused, whatever the cap.
	if _, err := e.svc.SubmitInput(e.ctx, u, v.ID, "extend", "7200", nil); !errors.Is(err, ErrExtendNotTimed) {
		t.Errorf("extend of a job err = %v, want ErrExtendNotTimed", err)
	}
	if _, err := e.svc.SubmitInput(e.ctx, u, v.ID, "pause", "now", nil); !errors.Is(err, ErrPauseNotRunning) && !errors.Is(err, ErrPauseNotSupported) {
		t.Errorf("pause of a queued job err = %v, want a pause refusal", err)
	}

	got, err := e.svc.CancelJob(e.ctx, c, v.ID)
	if err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	if got.Status != "cancelled" || got.FinishedAt == nil {
		t.Errorf("cancelled view = %+v", got)
	}
	if st := e.jobStatusRow(t, v.ID); st != "cancelled" {
		t.Errorf("runs.status = %q", st)
	}
	if _, err := e.svc.CancelJob(e.ctx, c, v.ID); !errors.Is(err, ErrJobTerminal) {
		t.Errorf("second cancel err = %v, want ErrJobTerminal", err)
	}
}

func TestGetJobResultLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	u := e.seedJobUser(t)
	c := cliCaller(u)
	v, err := e.svc.CreateJobRun(e.ctx, jobReq(c))
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.GetJobResult(e.ctx, c, v.ID)
	if err != nil || res.Ready || res.JobStatus != "queued" {
		t.Fatalf("no-result-yet = %+v err=%v, want a not-ready value and no error", res, err)
	}
	e.exec(`INSERT INTO job_results (run_id, status, report_md) VALUES ($1, 'ok', '# report')`, v.ID)
	e.exec(`INSERT INTO job_findings (run_id, ordinal, severity, message_md, file, line) VALUES ($1, 1, 'warning', 'second', 'a.go', 7)`, v.ID)
	e.exec(`INSERT INTO job_findings (run_id, ordinal, severity, message_md, url) VALUES ($1, 0, 'info', 'first', 'https://example.test/x')`, v.ID)
	res, err = e.svc.GetJobResult(e.ctx, c, v.ID)
	if err != nil || !res.Ready || res.Status != "ok" || res.ReportMD != "# report" || len(res.Findings) != 2 {
		t.Fatalf("result = %+v err=%v", res, err)
	}
	if f := res.Findings[0]; f.Ordinal != 0 || f.Severity != "info" || f.URL == nil || *f.URL != "https://example.test/x" || f.File != nil {
		t.Errorf("finding 0 = %+v", f)
	}
	if f := res.Findings[1]; f.Line == nil || *f.Line != 7 || f.File == nil || *f.File != "a.go" {
		t.Errorf("finding 1 = %+v", f)
	}
}

func TestListJobMessagesProjectionLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	u := e.seedJobUser(t)
	c := cliCaller(u)
	v, err := e.svc.CreateJobRun(e.ctx, jobReq(c))
	if err != nil {
		t.Fatal(err)
	}
	// A secret-shaped value assembled at runtime so no complete token literal sits in source.
	secret := "sk-ant-" + "api03-" + "FIXTUREFIXTUREFIXTURE1234"
	msgs := []struct {
		seq     int
		kind    string
		payload string
	}{
		{1, "text", `{"text":"working on it"}`},
		{2, "tool_use", `{"name":"Bash","input":{"command":"curl -H 'x-api-key: ` + secret + `'"}}`},
		{3, "tool_result", `{"content":"` + secret + `"}`},
		{4, "thinking", `{"text":"secret plan"}`},
		{5, "status", `{"text":"leaked ` + secret + ` here"}`},
		{6, "error", `{"text":"boom"}`},
		{7, "user_message", `{"text":"hello"}`},
		{8, "plan", `{"plan_md":"p"}`},
	}
	for _, m := range msgs {
		e.exec(`INSERT INTO run_messages (run_id, seq, kind, payload) VALUES ($1, $2, $3, $4::jsonb)`, v.ID, m.seq, m.kind, m.payload)
	}
	out, err := e.svc.ListJobMessages(e.ctx, c, v.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var seqs []int
	for _, m := range out {
		seqs = append(seqs, m.Seq)
		if got := m.Text; got != "" && containsSecret(got, secret) {
			t.Errorf("message %d leaked the secret: %q", m.Seq, got)
		}
	}
	if fmt.Sprint(seqs) != "[1 5 6]" {
		t.Fatalf("projected seqs = %v, want only text/status/error [1 5 6]", seqs)
	}
	after, _ := e.svc.ListJobMessages(e.ctx, c, v.ID, 5, 100)
	if len(after) != 1 || after[0].Seq != 6 {
		t.Errorf("after_seq=5 = %+v", after)
	}
	one, _ := e.svc.ListJobMessages(e.ctx, c, v.ID, 0, 1)
	if len(one) != 1 {
		t.Errorf("limit 1 = %d rows", len(one))
	}
}

func containsSecret(s, secret string) bool {
	for i := 0; i+len(secret) <= len(s); i++ {
		if s[i:i+len(secret)] == secret {
			return true
		}
	}
	return false
}

// TestJobStatusCoversStatusCatalogLiveDB reads runs_status_check from pg_constraint and requires
// every status it allows to be mapped EXPLICITLY (the default arm of JobStatus is not evidence),
// so a status added by a later migration cannot silently read as running.
func TestJobStatusCoversStatusCatalogLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	var def string
	if err := e.pool.QueryRow(e.ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'runs_status_check'`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	rows, err := e.pool.Query(e.ctx, `SELECT unnest(regexp_matches($1, '''([a-z_]+)''', 'g'))`, def)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var statuses []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, s)
	}
	if len(statuses) != 13 {
		t.Fatalf("runs_status_check has %d values (%v), want 13; extend JobStatus and this test deliberately", len(statuses), statuses)
	}
	public := map[string]bool{"queued": true, "running": true, "waiting": true, "completed": true, "failed": true, "cancelled": true}
	for _, s := range statuses {
		if !explicitJobStatus[s] {
			t.Errorf("status %q is allowed by runs_status_check but has no explicit JobStatus case", s)
		}
		if !public[JobStatus(s)] {
			t.Errorf("JobStatus(%q) = %q, not a public status", s, JobStatus(s))
		}
	}
}

// explicitJobStatus lists the statuses JobStatus names in a case (kept beside the switch's test so a
// new catalog value fails here until it is decided).
var explicitJobStatus = map[string]bool{
	"queued": true, "claimed": true, "running": true, "awaiting_approval": true, "awaiting_input": true,
	"awaiting_followup": true, "pool_wait": true, "paused": true, "limit_wait": true, "recovery_wait": true,
	"completed": true, "failed": true, "cancelled": true,
}
