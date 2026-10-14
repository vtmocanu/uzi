package workersvc

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/outcomeloss"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #2984: the owner-confirmed outcome discard against a REAL Postgres. The service is
// driven directly with an injected clock; the router, auth and CSRF halves live in
// handler/outcome_discard_livedb_test.go. Skipped unless UZI_TEST_DATABASE_URL points at a
// throwaway Postgres (./e2e/run-store-it.sh).

type discardClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *discardClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *discardClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type discardFix struct {
	t      *testing.T
	e      interlockLiveDB
	svc    *Service
	clk    *discardClock
	worker store.Worker
	run    uuid.UUID
}

func newDiscardFix(t *testing.T) *discardFix {
	t.Helper()
	e := setupInterlockLiveDB(t)
	svc := New(e.q, newBox(t), testParams())
	svc.SetTxBeginner(e.pool)
	clk := &discardClock{t: time.Now().UTC().Truncate(time.Millisecond)}
	svc.SetClock(clk.now)
	wid := e.seedWorker(t, []string{"outcome_discard_v1"})
	w, err := e.q.GetWorkerByID(e.ctx, wid)
	if err != nil {
		t.Fatal(err)
	}
	f := &discardFix{t: t, e: e, svc: svc, clk: clk, worker: w}
	f.run = f.seedRun(e.userID)
	return f
}

// seedRun inserts a failed run owned by user and attributed to NO worker (the run_not_owned
// shape: the api no longer attributes it to the holding worker).
func (f *discardFix) seedRun(user uuid.UUID) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	iid := *f.e.nextIID
	*f.e.nextIID++
	f.e.exec(f.t, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, kind, status)
	               VALUES ($1, $2, $3, $4, 't', 'd', 'issue', 'failed')`, id, user, f.e.repoID, iid)
	return id
}

func discardJournal(n byte) string { return strings.Repeat(string("0123456789abcdef"[n%16]), 64) }

// discardReport builds a complete, valid report for (worker, run, gen). variant changes the
// manifest content (so its digest) without changing identity.
func discardReport(worker, run uuid.UUID, gen int64, journal, reason string, variant byte) apitypes.BlockedOutcomeReport {
	m := &apitypes.OutcomeLossManifest{
		Version: 1, WorkerID: worker.String(), RunID: run.String(), RunDir: run.String(), ClaimGeneration: gen,
		JournalSHA256: journal, BlockedReason: reason,
		Outcome:  apitypes.OutcomeLossOutcome{Status: "failed", Phase: "run"},
		Siblings: []apitypes.OutcomeLossSibling{},
		Clones: []apitypes.OutcomeLossClone{{
			Path: "/data/runner/x/issue-1", Branch: "agent/issue-1", CommitBasis: outcomeloss.BasisWorkerBare,
			UnpublishedCommits: []string{strings.Repeat("a", 40)},
			Paths: []apitypes.OutcomeLossPathItem{{
				Path: "notes.txt", Status: "??", Kind: "file", Size: 3, SHA256: discardJournal(variant),
			}},
		}},
	}
	_, sum, err := outcomeloss.Sum(m)
	if err != nil {
		panic(err)
	}
	return apitypes.BlockedOutcomeReport{
		RunID: run.String(), ClaimGeneration: gen, JournalSHA256: journal, BlockedReason: reason,
		LossComplete: true, LossSHA256: sum, Loss: m,
	}
}

func (f *discardFix) report(gen int64, variant byte) apitypes.BlockedOutcomeReport {
	return discardReport(f.worker.ID, f.run, gen, discardJournal(1), outcomeloss.ReasonRunNotOwned, variant)
}

func (f *discardFix) put(reports ...apitypes.BlockedOutcomeReport) {
	f.t.Helper()
	if reports == nil {
		reports = []apitypes.BlockedOutcomeReport{}
	}
	if err := f.svc.ReplaceBlockedOutcomes(f.e.ctx, f.worker, reports); err != nil {
		f.t.Fatalf("put: %v", err)
	}
}

func discardReq(r apitypes.BlockedOutcomeReport) apitypes.OutcomeDiscardRequest {
	return apitypes.OutcomeDiscardRequest{
		RunID: r.RunID, ClaimGeneration: r.ClaimGeneration, JournalSHA256: r.JournalSHA256,
		LossSHA256: r.LossSHA256, Evidence: apitypes.OutcomeDiscardEvidenceInformed,
	}
}

func (f *discardFix) authorize(r apitypes.BlockedOutcomeReport) (apitypes.OutcomeDiscard, error) {
	return f.svc.AuthorizeOutcomeDiscard(f.e.ctx, f.e.userID, f.worker.ID, discardReq(r))
}

func (f *discardFix) mustAuthorize(r apitypes.BlockedOutcomeReport) apitypes.OutcomeDiscard {
	f.t.Helper()
	d, err := f.authorize(r)
	if err != nil {
		f.t.Fatalf("authorize: %v", err)
	}
	return d
}

func wantRefusal(t *testing.T, err error, code string) {
	t.Helper()
	var ref *OutcomeDiscardRefusal
	if !errors.As(err, &ref) || ref.Code != code {
		t.Fatalf("err = %v, want refusal %q", err, code)
	}
}

func (f *discardFix) begin(id string) apitypes.OutcomeDiscard {
	f.t.Helper()
	d, err := f.svc.BeginOutcomeDiscard(f.e.ctx, f.worker, uuid.MustParse(id))
	if err != nil {
		f.t.Fatalf("begin: %v", err)
	}
	return d
}

func (f *discardFix) status(id string) apitypes.OutcomeDiscard {
	f.t.Helper()
	d, err := f.svc.GetOutcomeDiscardForOwner(f.e.ctx, f.e.userID, f.worker.ID, uuid.MustParse(id))
	if err != nil {
		f.t.Fatalf("status: %v", err)
	}
	return d
}

func TestOutcomeDiscardLifecycleLiveDB(t *testing.T) {
	f := newDiscardFix(t)
	rep := f.report(3, 1)
	f.put(rep)

	owner, err := f.svc.ListBlockedOutcomesForOwner(f.e.ctx, f.e.userID, f.worker.ID)
	if err != nil || len(owner) != 1 || owner[0].LossSHA256 != rep.LossSHA256 || !owner[0].LossComplete || owner[0].Loss == nil {
		t.Fatalf("owner list = %+v, %v", owner, err)
	}
	d := f.mustAuthorize(rep)
	if d.State != "authorized" || d.Reason != "" || d.LeaseUntil != nil || d.RemainingMS != 0 {
		t.Fatalf("authorized view = %+v", d)
	}
	if want := f.clk.now().Add(OutcomeDiscardAuthorizationTTL); !d.ExpiresAt.Equal(want) {
		t.Fatalf("expires_at = %v, want %v", d.ExpiresAt, want)
	}
	// Re-confirming the same tuple is idempotent and creates no second directive.
	if again := f.mustAuthorize(rep); again.ID != d.ID {
		t.Fatalf("second authorize minted %s, want %s", again.ID, d.ID)
	}
	list, err := f.svc.ListWorkerOutcomeDiscards(f.e.ctx, f.worker)
	if err != nil || len(list) != 1 || list[0].ID != d.ID {
		t.Fatalf("worker list = %+v, %v", list, err)
	}

	b1 := f.begin(d.ID)
	if b1.State != "executing" || b1.RemainingMS != OutcomeDiscardLease.Milliseconds() || b1.LeaseUntil == nil {
		t.Fatalf("begin = %+v", b1)
	}
	f.clk.advance(10 * time.Second)
	b2 := f.begin(d.ID)
	if b2.State != "executing" || !b2.LeaseUntil.Equal(*b1.LeaseUntil) || b2.RemainingMS != b1.RemainingMS-10000 {
		t.Fatalf("repeated begin extended or changed the lease: first %+v second %+v", b1, b2)
	}
	// An owner cancel of an executing directive is refused.
	if _, err := f.svc.CancelOutcomeDiscard(f.e.ctx, f.e.userID, f.worker.ID, uuid.MustParse(d.ID)); !errors.Is(err, ErrOutcomeDiscardConflict) {
		t.Fatalf("cancel executing: %v", err)
	}
	// A second authorize while executing is execution_unresolved.
	_, err = f.authorize(rep)
	wantRefusal(t, err, DiscardRefusalExecutionUnresolved)

	done, err := f.svc.CompleteOutcomeDiscard(f.e.ctx, f.worker, uuid.MustParse(d.ID), apitypes.OutcomeDiscardCompleteRequest{Result: "deleted"})
	if err != nil || done.State != "completed" || done.Reason != "deleted" || done.LeaseUntil != nil {
		t.Fatalf("complete = %+v, %v", done, err)
	}
	if again, err := f.svc.CompleteOutcomeDiscard(f.e.ctx, f.worker, uuid.MustParse(d.ID), apitypes.OutcomeDiscardCompleteRequest{Result: "deleted"}); err != nil || again.State != "completed" {
		t.Fatalf("idempotent complete = %+v, %v", again, err)
	}
	if _, err := f.svc.CompleteOutcomeDiscard(f.e.ctx, f.worker, uuid.MustParse(d.ID), apitypes.OutcomeDiscardCompleteRequest{Result: "refused", Reason: "x"}); !errors.Is(err, ErrOutcomeDiscardConflict) {
		t.Fatalf("different result: %v", err)
	}
	if got := f.status(d.ID); got.State != "completed" {
		t.Fatalf("owner status = %+v", got)
	}
	list, _ = f.svc.ListWorkerOutcomeDiscards(f.e.ctx, f.worker)
	if len(list) != 0 {
		t.Fatalf("a final directive is still listed to the worker: %+v", list)
	}
}

func TestOutcomeDiscardCompleteMappingLiveDB(t *testing.T) {
	for _, tc := range []struct {
		req       apitypes.OutcomeDiscardCompleteRequest
		wantState string
		wantErr   error
	}{
		{apitypes.OutcomeDiscardCompleteRequest{Result: "refused", Reason: "loss_changed"}, "refused", nil},
		{apitypes.OutcomeDiscardCompleteRequest{Result: "unsynced", Reason: "fsync_failed"}, "completed_unknown", nil},
		{apitypes.OutcomeDiscardCompleteRequest{Result: "deleted", Reason: "x"}, "", ErrInvalidOutcomeDiscardRequest},
		{apitypes.OutcomeDiscardCompleteRequest{Result: "refused"}, "", ErrInvalidOutcomeDiscardRequest},
		{apitypes.OutcomeDiscardCompleteRequest{Result: "refused", Reason: "Bad Reason"}, "", ErrInvalidOutcomeDiscardRequest},
		{apitypes.OutcomeDiscardCompleteRequest{Result: "wat", Reason: "x"}, "", ErrInvalidOutcomeDiscardRequest},
	} {
		f := newDiscardFix(t)
		rep := f.report(1, 1)
		f.put(rep)
		d := f.mustAuthorize(rep)
		// /complete before /begin is a state conflict, never a transition.
		if _, err := f.svc.CompleteOutcomeDiscard(f.e.ctx, f.worker, uuid.MustParse(d.ID), tc.req); tc.wantErr == nil && !errors.Is(err, ErrOutcomeDiscardConflict) {
			t.Fatalf("%+v before begin: %v", tc.req, err)
		}
		f.begin(d.ID)
		got, err := f.svc.CompleteOutcomeDiscard(f.e.ctx, f.worker, uuid.MustParse(d.ID), tc.req)
		if !errors.Is(err, tc.wantErr) || got.State != tc.wantState {
			t.Fatalf("%+v: state %q err %v, want %q / %v", tc.req, got.State, err, tc.wantState, tc.wantErr)
		}
		if tc.wantErr == nil && got.Reason != tc.req.Reason {
			t.Fatalf("%+v: reason %q", tc.req, got.Reason)
		}
	}
}

// Every refusal of the owner POST, in the spec's order: each case sets up the named condition
// AND every later one, so the earliest must win.
func TestOutcomeDiscardRefusalOrderLiveDB(t *testing.T) {
	f := newDiscardFix(t)
	ctx := f.e.ctx
	rep := f.report(2, 1)
	req := discardReq(rep)
	other := f.e.userID // placeholder to keep the shape obvious below

	// 400s come first and touch no row.
	bad := req
	bad.Evidence = "recovery_capture"
	if _, err := f.svc.AuthorizeOutcomeDiscard(ctx, other, f.worker.ID, bad); !errors.Is(err, ErrInvalidOutcomeDiscardRequest) {
		t.Fatalf("evidence: %v", err)
	}
	bad = req
	bad.JournalSHA256 = strings.ToUpper(bad.JournalSHA256) + "x"
	if _, err := f.svc.AuthorizeOutcomeDiscard(ctx, other, f.worker.ID, bad); !errors.Is(err, ErrInvalidOutcomeDiscardRequest) {
		t.Fatalf("grammar: %v", err)
	}
	// 404: a run the caller does not own, then a worker it does not own.
	stranger := uuid.New()
	f.e.exec(t, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, stranger, "stranger-"+stranger.String()+"@e2e")
	if _, err := f.svc.AuthorizeOutcomeDiscard(ctx, stranger, f.worker.ID, req); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("foreign run: %v", err)
	}
	foreignWorkerRun := f.seedRun(stranger)
	r2 := req
	r2.RunID = foreignWorkerRun.String()
	if _, err := f.svc.AuthorizeOutcomeDiscard(ctx, stranger, f.worker.ID, r2); !errors.Is(err, ErrWorkerNotFound) {
		t.Fatalf("run owner who does not own the worker: %v", err)
	}

	// Nothing reported yet.
	_, err := f.authorize(rep)
	wantRefusal(t, err, DiscardRefusalNotBlocked)

	// Reason: a non-run_not_owned report is not discardable, even with a wrong sha and an open hold.
	notOwned := discardReport(f.worker.ID, f.run, 2, discardJournal(1), "reserve_exhausted", 1)
	f.put(notOwned)
	f.e.exec(t, `UPDATE runs SET worker_id = $2 WHERE id = $1`, f.run, f.worker.ID) // also attributed
	_, err = f.svc.AuthorizeOutcomeDiscard(ctx, f.e.userID, f.worker.ID, discardReq(notOwned))
	wantRefusal(t, err, DiscardRefusalReasonNotDiscard)

	// Attributed to the holding worker: refused before the sha comparisons.
	f.put(rep)
	wrongJournal := req
	wrongJournal.JournalSHA256 = discardJournal(9)
	_, err = f.svc.AuthorizeOutcomeDiscard(ctx, f.e.userID, f.worker.ID, wrongJournal)
	wantRefusal(t, err, DiscardRefusalAttributed)
	f.e.exec(t, `UPDATE runs SET worker_id = NULL WHERE id = $1`, f.run)

	_, err = f.svc.AuthorizeOutcomeDiscard(ctx, f.e.userID, f.worker.ID, wrongJournal)
	wantRefusal(t, err, DiscardRefusalJournalChanged)

	// An incomplete manifest outranks a digest mismatch (the incomplete row has no digest).
	incomplete := rep
	incomplete.LossComplete, incomplete.Loss, incomplete.LossSHA256, incomplete.IncompleteReason = false, nil, "", "clone_unreadable"
	f.put(incomplete)
	_, err = f.svc.AuthorizeOutcomeDiscard(ctx, f.e.userID, f.worker.ID, req)
	wantRefusal(t, err, DiscardRefusalManifestIncomplete)

	f.put(rep)
	staleLoss := req
	staleLoss.LossSHA256 = discardJournal(7)
	f.e.exec(t, `INSERT INTO recovery_custody_holds
		(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id)
		VALUES ($1,$2,$3,$4,1,'open',$5,'ident',$5,$4)`, uuid.New(), f.e.userID, f.e.repoID, f.run, f.worker.ID)
	_, err = f.svc.AuthorizeOutcomeDiscard(ctx, f.e.userID, f.worker.ID, staleLoss)
	wantRefusal(t, err, DiscardRefusalLossChanged)

	// Open custody hold with a matching tuple.
	_, err = f.authorize(rep)
	wantRefusal(t, err, DiscardRefusalCustodyHoldOpen)
	f.e.exec(t, `UPDATE recovery_custody_holds SET state='discarded', live_worker_id=NULL, live_run_id=NULL WHERE run_id=$1`, f.run)

	// Executing directive, then success path is blocked until it resolves.
	d := f.mustAuthorize(rep)
	f.begin(d.ID)
	_, err = f.authorize(rep)
	wantRefusal(t, err, DiscardRefusalExecutionUnresolved)
}

// /begin re-checks everything the confirmation depended on, and a refusal is terminal.
func TestOutcomeDiscardBeginRechecksLiveDB(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(f *discardFix, rep apitypes.BlockedOutcomeReport)
		want   string
	}{
		{"stale authorization after the report changes its digest", func(f *discardFix, rep apitypes.BlockedOutcomeReport) {
			f.put(f.report(rep.ClaimGeneration, 2))
		}, DiscardRefusalLossChanged},
		{"report cleared", func(f *discardFix, _ apitypes.BlockedOutcomeReport) { f.put() }, DiscardRefusalNotBlocked},
		{"journal changed", func(f *discardFix, rep apitypes.BlockedOutcomeReport) {
			f.put(discardReport(f.worker.ID, f.run, rep.ClaimGeneration, discardJournal(4), outcomeloss.ReasonRunNotOwned, 1))
		}, DiscardRefusalJournalChanged},
		{"reason changed", func(f *discardFix, rep apitypes.BlockedOutcomeReport) {
			f.put(discardReport(f.worker.ID, f.run, rep.ClaimGeneration, rep.JournalSHA256, "gap_unrecoverable", 1))
		}, DiscardRefusalReasonNotDiscard},
		{"report became incomplete", func(f *discardFix, rep apitypes.BlockedOutcomeReport) {
			rep.LossComplete, rep.Loss, rep.LossSHA256, rep.IncompleteReason = false, nil, "", "path_cap_exceeded"
			f.put(rep)
		}, DiscardRefusalManifestIncomplete},
		{"run reattributed to the holding worker", func(f *discardFix, _ apitypes.BlockedOutcomeReport) {
			f.e.exec(f.t, `UPDATE runs SET worker_id = $2 WHERE id = $1`, f.run, f.worker.ID)
		}, DiscardRefusalAttributed},
		{"custody hold opened", func(f *discardFix, _ apitypes.BlockedOutcomeReport) {
			f.e.exec(f.t, `INSERT INTO recovery_custody_holds
				(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id)
				VALUES ($1,$2,$3,$4,1,'open',$5,'ident',$5,$4)`, uuid.New(), f.e.userID, f.e.repoID, f.run, f.worker.ID)
		}, DiscardRefusalCustodyHoldOpen},
		{"run deleted", func(f *discardFix, _ apitypes.BlockedOutcomeReport) {
			f.e.exec(f.t, `DELETE FROM runs WHERE id = $1`, f.run)
		}, "run_not_owned_by_worker_owner"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDiscardFix(t)
			rep := f.report(5, 1)
			f.put(rep)
			d := f.mustAuthorize(rep)
			tc.mutate(f, rep)
			got := f.begin(d.ID)
			if got.State != "refused" || got.Reason != tc.want || got.LeaseUntil != nil {
				t.Fatalf("begin = %+v, want refused/%s", got, tc.want)
			}
			// The refusal is final: a later begin cannot resurrect the directive.
			if again := f.begin(d.ID); again.State != "refused" {
				t.Fatalf("begin after refusal = %+v", again)
			}
		})
	}
}

func TestOutcomeDiscardExpiryOnlyAfterLeaseAndGraceLiveDB(t *testing.T) {
	f := newDiscardFix(t)
	rep := f.report(1, 1)
	f.put(rep)
	d := f.mustAuthorize(rep)

	f.clk.advance(OutcomeDiscardAuthorizationTTL - time.Second)
	if got := f.status(d.ID); got.State != "authorized" {
		t.Fatalf("expired early: %+v", got)
	}
	f.clk.advance(time.Second)
	if got := f.status(d.ID); got.State != "expired" || got.Reason != "authorization_expired" {
		t.Fatalf("authorized past expires_at = %+v", got)
	}
	if got := f.begin(d.ID); got.State != "expired" {
		t.Fatalf("begin on an expired directive = %+v", got)
	}

	// Executing: stays executing through lease_until and the 30s grace, expires after.
	d2 := f.mustAuthorize(rep)
	f.begin(d2.ID)
	f.clk.advance(OutcomeDiscardLease)
	if got := f.status(d2.ID); got.State != "executing" || got.RemainingMS != 0 {
		t.Fatalf("at lease_until the directive must still be executing with 0 remaining: %+v", got)
	}
	f.clk.advance(OutcomeDiscardGrace - time.Second)
	if got := f.status(d2.ID); got.State != "executing" {
		t.Fatalf("expired before lease+grace: %+v", got)
	}
	f.clk.advance(time.Second)
	if got := f.status(d2.ID); got.State != "expired" || got.Reason != "lease_expired" {
		t.Fatalf("executing past lease+grace = %+v", got)
	}
	if _, err := f.svc.CompleteOutcomeDiscard(f.e.ctx, f.worker, uuid.MustParse(d2.ID), apitypes.OutcomeDiscardCompleteRequest{Result: "deleted"}); !errors.Is(err, ErrOutcomeDiscardConflict) {
		t.Fatalf("complete after expiry: %v", err)
	}

	// Authorize expires a stale executing row before inserting, so a fresh directive is minted.
	d3 := f.mustAuthorize(rep)
	f.begin(d3.ID)
	f.clk.advance(OutcomeDiscardLease + OutcomeDiscardGrace)
	d4 := f.mustAuthorize(rep)
	if d4.ID == d3.ID || d4.State != "authorized" {
		t.Fatalf("authorize over a stale executing row = %+v", d4)
	}
	if got := f.status(d3.ID); got.State != "expired" {
		t.Fatalf("stale executing row after authorize = %+v", got)
	}
}

func TestOutcomeDiscardWorkerReadExpiresLiveDB(t *testing.T) {
	f := newDiscardFix(t)
	rep := f.report(1, 1)
	f.put(rep)
	d := f.mustAuthorize(rep)
	f.begin(d.ID)
	f.clk.advance(OutcomeDiscardLease + OutcomeDiscardGrace)
	list, err := f.svc.ListWorkerOutcomeDiscards(f.e.ctx, f.worker)
	if err != nil || len(list) != 0 {
		t.Fatalf("worker list = %+v, %v", list, err)
	}
	if got := f.status(d.ID); got.State != "expired" {
		t.Fatalf("status = %+v", got)
	}
}

func TestOutcomeDiscardReconcileLiveDB(t *testing.T) {
	absent := apitypes.OutcomeDiscardReconcileRequest{Observation: "absent"}
	unverifiable := apitypes.OutcomeDiscardReconcileRequest{Observation: "present_unverifiable"}
	reconcile := func(f *discardFix, id string, req apitypes.OutcomeDiscardReconcileRequest) (apitypes.OutcomeDiscard, error) {
		return f.svc.ReconcileOutcomeDiscard(f.e.ctx, f.worker, uuid.MustParse(id), req)
	}
	t.Run("authorized and absent is refused journal_absent, idempotently", func(t *testing.T) {
		f := newDiscardFix(t)
		rep := f.report(1, 1)
		f.put(rep)
		d := f.mustAuthorize(rep)
		f.put() // the journal is gone: the worker's report no longer lists it (no reported row)
		for i := 0; i < 2; i++ {
			got, err := reconcile(f, d.ID, absent)
			if err != nil || got.State != "refused" || got.Reason != "journal_absent" {
				t.Fatalf("reconcile %d = %+v, %v", i, got, err)
			}
		}
		if _, err := reconcile(f, d.ID, unverifiable); !errors.Is(err, ErrOutcomeDiscardConflict) {
			t.Fatalf("a different observation after the final state: %v", err)
		}
	})
	t.Run("executing and absent is completed_unknown even with no reported row and after the lease", func(t *testing.T) {
		f := newDiscardFix(t)
		rep := f.report(1, 1)
		f.put(rep)
		d := f.mustAuthorize(rep)
		f.begin(d.ID)
		f.put() // post-restart order: register -> PUT (row removed) -> GET -> reconcile
		list, _ := f.svc.ListWorkerOutcomeDiscards(f.e.ctx, f.worker)
		if len(list) != 1 || list[0].State != "executing" {
			t.Fatalf("an executing directive must still be returned after the report is removed: %+v", list)
		}
		f.clk.advance(OutcomeDiscardLease + time.Second) // lease over, still inside the grace
		got, err := reconcile(f, d.ID, absent)
		if err != nil || got.State != "completed_unknown" || got.Reason != "journal_absent_after_begin" {
			t.Fatalf("reconcile = %+v, %v", got, err)
		}
		if again, err := reconcile(f, d.ID, absent); err != nil || again.State != "completed_unknown" {
			t.Fatalf("idempotent reconcile = %+v, %v", again, err)
		}
	})
	t.Run("present_unverifiable is refused journal_unverifiable and never deletes", func(t *testing.T) {
		f := newDiscardFix(t)
		rep := f.report(1, 1)
		f.put(rep)
		d := f.mustAuthorize(rep)
		f.begin(d.ID)
		got, err := reconcile(f, d.ID, unverifiable)
		if err != nil || got.State != "refused" || got.Reason != "journal_unverifiable" {
			t.Fatalf("reconcile = %+v, %v", got, err)
		}
	})
	t.Run("a bad observation is a 400-class error and another worker's directive is not found", func(t *testing.T) {
		f := newDiscardFix(t)
		rep := f.report(1, 1)
		f.put(rep)
		d := f.mustAuthorize(rep)
		if _, err := reconcile(f, d.ID, apitypes.OutcomeDiscardReconcileRequest{Observation: "gone"}); !errors.Is(err, ErrInvalidOutcomeDiscardRequest) {
			t.Fatalf("bad observation: %v", err)
		}
		other, err := f.e.q.GetWorkerByID(f.e.ctx, f.e.seedWorker(t, nil))
		if err != nil {
			t.Fatal(err)
		}
		id := uuid.MustParse(d.ID)
		if _, err := f.svc.BeginOutcomeDiscard(f.e.ctx, other, id); !errors.Is(err, ErrOutcomeDiscardNotFound) {
			t.Fatalf("foreign begin: %v", err)
		}
		if _, err := f.svc.CompleteOutcomeDiscard(f.e.ctx, other, id, apitypes.OutcomeDiscardCompleteRequest{Result: "deleted"}); !errors.Is(err, ErrOutcomeDiscardNotFound) {
			t.Fatalf("foreign complete: %v", err)
		}
		if _, err := f.svc.ReconcileOutcomeDiscard(f.e.ctx, other, id, absent); !errors.Is(err, ErrOutcomeDiscardNotFound) {
			t.Fatalf("foreign reconcile: %v", err)
		}
		if l, _ := f.svc.ListWorkerOutcomeDiscards(f.e.ctx, other); len(l) != 0 {
			t.Fatalf("foreign list leaked: %+v", l)
		}
	})
}

func TestOutcomeDiscardCancelLiveDB(t *testing.T) {
	f := newDiscardFix(t)
	rep := f.report(1, 1)
	f.put(rep)
	d := f.mustAuthorize(rep)
	got, err := f.svc.CancelOutcomeDiscard(f.e.ctx, f.e.userID, f.worker.ID, uuid.MustParse(d.ID))
	if err != nil || got.State != "cancelled" || got.Reason != "owner_cancelled" {
		t.Fatalf("cancel = %+v, %v", got, err)
	}
	if again, err := f.svc.CancelOutcomeDiscard(f.e.ctx, f.e.userID, f.worker.ID, uuid.MustParse(d.ID)); err != nil || again.State != "cancelled" {
		t.Fatalf("idempotent cancel = %+v, %v", again, err)
	}
	if b := f.begin(d.ID); b.State != "cancelled" {
		t.Fatalf("begin after cancel = %+v", b)
	}
	// Another principal cannot cancel or read it.
	stranger := uuid.New()
	f.e.exec(t, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, stranger, "stranger-"+stranger.String()+"@e2e")
	if _, err := f.svc.CancelOutcomeDiscard(f.e.ctx, stranger, f.worker.ID, uuid.MustParse(d.ID)); !errors.Is(err, ErrOutcomeDiscardNotFound) {
		t.Fatalf("stranger cancel: %v", err)
	}
	if _, err := f.svc.GetOutcomeDiscardForOwner(f.e.ctx, stranger, f.worker.ID, uuid.MustParse(d.ID)); !errors.Is(err, ErrOutcomeDiscardNotFound) {
		t.Fatalf("stranger status: %v", err)
	}
}

func TestOutcomeDiscardReportValidationLiveDB(t *testing.T) {
	f := newDiscardFix(t)
	good := f.report(1, 1)
	f.put(good)
	bad := f.report(2, 1)
	bad.LossSHA256 = discardJournal(3)
	if err := f.svc.ReplaceBlockedOutcomes(f.e.ctx, f.worker, []apitypes.BlockedOutcomeReport{good, bad}); !errors.Is(err, ErrInvalidBlockedOutcome) {
		t.Fatalf("digest mismatch: %v", err)
	}
	if err := f.svc.ReplaceBlockedOutcomes(f.e.ctx, f.worker, []apitypes.BlockedOutcomeReport{good, good}); !errors.Is(err, ErrInvalidBlockedOutcome) {
		t.Fatalf("duplicate: %v", err)
	}
	nine := make([]apitypes.BlockedOutcomeReport, 9)
	for i := range nine {
		nine[i] = f.report(int64(i), 1)
	}
	if err := f.svc.ReplaceBlockedOutcomes(f.e.ctx, f.worker, nine); !errors.Is(err, ErrInvalidBlockedOutcome) {
		t.Fatalf("nine outcomes: %v", err)
	}
	// A rejected PUT left the stored set untouched (validation precedes the transaction).
	owner, err := f.svc.ListBlockedOutcomesForOwner(f.e.ctx, f.e.userID, f.worker.ID)
	if err != nil || len(owner) != 1 {
		t.Fatalf("stored set after rejected PUTs = %+v, %v", owner, err)
	}
	// A report for a run the owner does not own is stored but never shown to them.
	stranger := uuid.New()
	f.e.exec(t, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, stranger, "stranger-"+stranger.String()+"@e2e")
	foreign := f.seedRun(stranger)
	f.put(good, discardReport(f.worker.ID, foreign, 1, discardJournal(2), outcomeloss.ReasonRunNotOwned, 1))
	owner, _ = f.svc.ListBlockedOutcomesForOwner(f.e.ctx, f.e.userID, f.worker.ID)
	if len(owner) != 1 || owner[0].RunID != f.run.String() {
		t.Fatalf("owner list leaked a foreign run's outcome: %+v", owner)
	}
	if _, err := f.svc.ListBlockedOutcomesForOwner(f.e.ctx, stranger, f.worker.ID); !errors.Is(err, ErrWorkerNotFound) {
		t.Fatalf("stranger list on a worker they do not own: %v", err)
	}
}

// Schema guards the service relies on: the partial unique index, the result_reason pairing and
// the cascade from the worker.
func TestOutcomeDiscardSchemaLiveDB(t *testing.T) {
	f := newDiscardFix(t)
	rep := f.report(1, 1)
	f.put(rep)
	d := f.mustAuthorize(rep)
	insert := func(state string, reason any) error {
		_, err := f.e.pool.Exec(f.e.ctx, `INSERT INTO worker_outcome_discards
			(worker_id, run_id, claim_generation, journal_sha256, loss_sha256, confirmed_by, evidence, state, lease_until, expires_at, result_reason)
			VALUES ($1,$2,1,$3,$4,$5,'informed_decision',$6,now(),now(),$7)`,
			f.worker.ID, f.run, rep.JournalSHA256, rep.LossSHA256, f.e.userID, state, reason)
		return err
	}
	if err := insert("executing", nil); err == nil {
		t.Fatal("a second live directive for one journal was accepted")
	}
	if err := insert("refused", nil); err == nil {
		t.Fatal("a final state without result_reason was accepted")
	}
	if err := insert("authorized", "x"); err == nil {
		t.Fatal("a live state with a result_reason was accepted")
	}
	if err := insert("refused", "x"); err != nil {
		t.Fatalf("final rows must not collide with the live one: %v", err)
	}
	f.e.exec(t, `DELETE FROM workers WHERE id = $1`, f.worker.ID)
	var n int
	if err := f.e.pool.QueryRow(f.e.ctx, `SELECT (SELECT count(*) FROM worker_outcome_discards WHERE worker_id=$1)
	      + (SELECT count(*) FROM worker_blocked_outcomes WHERE worker_id=$1)`, f.worker.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows survived the worker delete: %d, %v (directive %s)", n, err, d.ID)
	}
}

// Retention: the sweep hook expires stale directives and deletes final directives and unreported
// outcomes older than 30 days, and leaves younger rows alone.
func TestOutcomeDiscardSweepRetentionLiveDB(t *testing.T) {
	f := newDiscardFix(t)
	rep := f.report(1, 1)
	f.put(rep)
	live := f.mustAuthorize(rep)
	old := f.seedRun(f.e.userID)
	oldRep := discardReport(f.worker.ID, old, 1, discardJournal(2), outcomeloss.ReasonRunNotOwned, 1)
	f.put(rep, oldRep)
	f.e.exec(t, `UPDATE worker_blocked_outcomes SET reported_at = $2 WHERE worker_id=$1 AND run_id=$3`,
		f.worker.ID, f.clk.now().Add(-OutcomeDiscardRetention-time.Hour), old)
	// A final directive 31 days old, and a final one 1 day old.
	for _, age := range []time.Duration{31 * 24 * time.Hour, 24 * time.Hour} {
		f.e.exec(t, `INSERT INTO worker_outcome_discards
			(worker_id, run_id, claim_generation, journal_sha256, loss_sha256, confirmed_by, evidence, state, lease_until, expires_at, result_reason, updated_at)
			VALUES ($1,$2,9,$3,$4,$5,'informed_decision','refused',$6,$6,'x',$6)`,
			f.worker.ID, f.run, rep.JournalSHA256, rep.LossSHA256, f.e.userID, f.clk.now().Add(-age))
	}
	f.clk.advance(OutcomeDiscardAuthorizationTTL + time.Minute) // the live directive is now stale
	f.svc.sweepOutcomeDiscards(f.e.ctx, f.clk.now())

	count := func(q string, args ...any) int {
		var n int
		if err := f.e.pool.QueryRow(f.e.ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM worker_outcome_discards WHERE worker_id=$1 AND claim_generation=9`, f.worker.ID); n != 1 {
		t.Fatalf("final directives left = %d, want only the 1-day-old one", n)
	}
	if n := count(`SELECT count(*) FROM worker_blocked_outcomes WHERE worker_id=$1`, f.worker.ID); n != 1 {
		t.Fatalf("outcome rows left = %d, want only the freshly reported one", n)
	}
	if got := f.status(live.ID); got.State != "expired" {
		t.Fatalf("the stale live directive after the sweep = %+v", got)
	}
	var wid uuid.UUID
	if err := f.e.pool.QueryRow(f.e.ctx, `SELECT worker_id FROM worker_outcome_discards WHERE id=$1`, live.ID).Scan(&wid); err != nil || wid != f.worker.ID {
		t.Fatalf("worker_id after sweep = %v, %v", wid, err)
	}
}

// Two sessions hammer PUT, authorize and begin of one worker at once: no deadlock (every
// transaction takes L0, L1, L2, L3 in order), and a stale authorization is refused once a PUT
// has changed the digest.
func TestOutcomeDiscardConcurrentWritersLiveDB(t *testing.T) {
	f := newDiscardFix(t)
	ctx, cancel := context.WithTimeout(f.e.ctx, 60*time.Second)
	defer cancel()
	reps := []apitypes.BlockedOutcomeReport{f.report(1, 1), f.report(1, 2)}
	f.put(reps[0])
	var wg sync.WaitGroup
	errc := make(chan error, 256)
	run := func(fn func(i int) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				if err := fn(i); err != nil {
					errc <- err
					return
				}
			}
		}()
	}
	okRefusal := func(err error) error {
		var ref *OutcomeDiscardRefusal
		if err == nil || errors.As(err, &ref) || errors.Is(err, ErrOutcomeDiscardConflict) || errors.Is(err, ErrOutcomeDiscardNotFound) {
			return nil
		}
		return err
	}
	run(func(i int) error { // the worker's reporter flips the digest back and forth
		return f.svc.ReplaceBlockedOutcomes(ctx, f.worker, []apitypes.BlockedOutcomeReport{reps[i%2]})
	})
	run(func(i int) error { // the owner confirms whatever it last saw
		d, err := f.svc.AuthorizeOutcomeDiscard(ctx, f.e.userID, f.worker.ID, discardReq(reps[i%2]))
		_ = d
		return okRefusal(err)
	})
	run(func(i int) error { // the worker polls and begins
		list, err := f.svc.ListWorkerOutcomeDiscards(ctx, f.worker)
		if err != nil {
			return err
		}
		for _, d := range list {
			if _, err := f.svc.BeginOutcomeDiscard(ctx, f.worker, uuid.MustParse(d.ID)); okRefusal(err) != nil {
				return err
			}
		}
		return nil
	})
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("concurrent writers did not finish: probable deadlock")
	}
	close(errc)
	for err := range errc {
		t.Errorf("concurrent writer: %v", err)
	}
	// Live invariant: never more than one live directive for the journal.
	var n int
	if err := f.e.pool.QueryRow(f.e.ctx, `SELECT count(*) FROM worker_outcome_discards WHERE worker_id=$1 AND state IN ('authorized','executing')`, f.worker.ID).Scan(&n); err != nil || n > 1 {
		t.Fatalf("live directives = %d, %v", n, err)
	}

	// An unknown id is not found, whatever the concurrent traffic did.
	if _, err := f.svc.BeginOutcomeDiscard(ctx, f.worker, uuid.New()); !errors.Is(err, ErrOutcomeDiscardNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}

// A holder of the L0 lock (the workers row FOR KEY SHARE) must make a worker delete and a user
// delete WAIT, not deadlock, and both must complete once it releases.
func TestOutcomeDiscardL0HolderBlocksDeletesLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	for _, tc := range []string{"worker", "user"} {
		t.Run(tc, func(t *testing.T) {
			user := uuid.New()
			e.exec(t, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, user, "l0-"+user.String()+"@e2e")
			wid := uuid.New()
			e.exec(t, `INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1,$2,$3,$4,'online')`, wid, user, "l0-"+wid.String()[:8], wid[:])
			tx, err := e.pool.Begin(e.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(e.ctx) }()
			q := store.New(tx)
			if _, err := q.LockWorkerForOutcomeDiscard(e.ctx, wid); err != nil {
				t.Fatal(err)
			}
			if err := q.LockWorkerOutcomeDiscards(e.ctx, wid); err != nil {
				t.Fatal(err)
			}
			finished := make(chan error, 1)
			go func() {
				var err error
				if tc == "worker" {
					_, err = e.q.DeleteWorkerForUser(e.ctx, store.DeleteWorkerForUserParams{ID: wid, UserID: user})
				} else {
					_, err = e.pool.Exec(e.ctx, `DELETE FROM users WHERE id = $1`, user)
				}
				finished <- err
			}()
			select {
			case err := <-finished:
				t.Fatalf("the delete finished while L0 was held: %v", err)
			case <-time.After(400 * time.Millisecond):
			}
			if err := tx.Commit(e.ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-finished:
				if err != nil {
					t.Fatalf("delete after release: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the delete never completed after L0 was released")
			}
			if _, err := e.q.GetWorkerByID(e.ctx, wid); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("worker survived: %v", err)
			}
		})
	}
}

// A PUT in flight for a worker that is deleted afterwards fails cleanly, never with a held lock.
func TestOutcomeDiscardWorkerGoneLiveDB(t *testing.T) {
	f := newDiscardFix(t)
	gone := f.worker
	f.e.exec(t, `DELETE FROM workers WHERE id = $1`, gone.ID)
	err := f.svc.ReplaceBlockedOutcomes(f.e.ctx, gone, []apitypes.BlockedOutcomeReport{})
	if !errors.Is(err, ErrOutcomeWorkerGone) {
		t.Fatalf("PUT for a deleted worker: %v", err)
	}
}
