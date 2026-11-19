package workersvc

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4488 (ADR-4007 T3): the retirement decision (tx-R1) and a consumer admission are not
// serialized, so a consumer committed between tx-R1's blocker recheck and its COMMIT is missed. These
// LiveDB tests pin the invariant from both sides through the REAL admission paths (createRunResolved
// -> createRunAtomic, and SetState for the ReconcileRunMR write), same owner and repo as the preserved
// source run:
//
//   - retirement first: while tx-R1 is open (paused by terminalHooks.beforeRetiringCommit) no consumer
//     may commit;
//   - consumer first: while an admission is in flight (paused by Service.createBeforeCommitHook) a
//     retire pass must retire nothing, and once the consumer committed the next pass keeps the ref
//     with the family's own blocker.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

const (
	// raceHold is a failure fallback longer than the observation window. Interleavings
	// that require retirement to commit use retireTestBounds; timeout tests keep their short bounds.
	raceHold = 10 * time.Second
	// racePoll bounds the wait for the admission to show up in pg_locks as a queued waiter on the gate.
	racePoll = 5 * time.Second
	// raceWaitBound bounds every wait of the test goroutine.
	raceWaitBound = 15 * time.Second
)

// raceFamily is one consumer admission family: arm prepares the (already live and terminal) source
// run, admit runs the family's real admission path against it, reason is the blocker a keep records
// once that consumer is committed.
type raceFamily struct {
	name   string
	reason string
	arm    func(t *testing.T, f *sweepFix)
	admit  func(f *sweepFix, svc *Service, iid int64) error
}

func raceSourceIID(t *testing.T, f *sweepFix) int64 {
	t.Helper()
	var iid int64
	if err := f.e.pool.QueryRow(f.e.ctx, `SELECT issue_iid FROM runs WHERE id=$1`, f.run).Scan(&iid); err != nil {
		t.Fatal(err)
	}
	return iid
}

// raceBranch gives the source run a branch of its own (and returns it).
func raceBranch(t *testing.T, f *sweepFix) string {
	t.Helper()
	branch := "uzi-race/" + uuid.NewString()
	f.e.exec(t, `UPDATE runs SET branch=$2 WHERE id=$1`, f.run, branch)
	return branch
}

func raceFamilies() []raceFamily {
	var branch string
	return []raceFamily{
		{
			name: "issue run", reason: "cross_run_consumer",
			arm: func(t *testing.T, f *sweepFix) {
				f.e.exec(t, `INSERT INTO issues (id, repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
					VALUES ($1, $2, $3, 'race issue', 'opened', '["uzi"]', 'https://forge.e2e/i', false, now(), now())`,
					uuid.New(), f.e.repoID, raceSourceIID(t, f))
			},
			admit: func(f *sweepFix, svc *Service, iid int64) error {
				_, err := svc.CreateRun(f.e.ctx, f.e.userID, f.e.repoID, iid, "desc", nil, nil, false, nil, nil, nil, nil, nil, nil)
				return err
			},
		},
		{
			name: "self-improve run", reason: "cross_run_consumer",
			admit: func(f *sweepFix, svc *Service, iid int64) error {
				_, err := svc.CreateSelfImproveRun(f.e.ctx, f.e.userID, f.e.repoID, iid, "t", "d", nil, nil, false, nil)
				return err
			},
		},
		{
			name: "continued-issue task", reason: "cross_run_consumer",
			admit: func(f *sweepFix, svc *Service, iid int64) error {
				_, err := svc.CreateTaskRun(f.e.ctx, f.e.userID, f.e.repoID, "continue the issue", "", false, false, false, false, &iid)
				return err
			},
		},
		{
			name: "then-fix child", reason: "dependent_run_active",
			admit: func(f *sweepFix, svc *Service, _ int64) error {
				_, err := svc.CreateThenFixRun(f.e.ctx, f.e.userID, f.e.repoID, f.run, "uzi/task/"+uuid.NewString(), "main", "fix findings", pgtype.Int4{}, pgtype.Int4{}, HarnessClaude)
				return err
			},
		},
		{
			name: "task review", reason: "dependent_run_active",
			admit: func(f *sweepFix, svc *Service, _ int64) error {
				_, err := svc.CreateTaskReviewRun(f.e.ctx, f.e.userID, f.e.repoID, f.run, "uzi/task/"+uuid.NewString(), "main", HarnessClaude)
				return err
			},
		},
		{
			name: "mr rework", reason: "dependent_run_active",
			arm: func(t *testing.T, f *sweepFix) {
				branch = raceBranch(t, f)
				f.e.exec(t, `UPDATE runs SET mr_iid=777, mr_state='opened' WHERE id=$1`, f.run)
			},
			admit: func(f *sweepFix, svc *Service, _ int64) error {
				_, err := svc.StartMRReworkForRun(f.e.ctx, f.e.userID, f.run, "please rework the merge request", nil)
				return err
			},
		},
		{
			name: "ci fix", reason: "mr_run_active",
			arm: func(t *testing.T, f *sweepFix) { branch = raceBranch(t, f) },
			admit: func(f *sweepFix, svc *Service, _ int64) error {
				_, err := svc.CreateCIFixRun(f.e.ctx, f.e.userID, f.e.repoID, branch, "Fix CI", "desc", sampleSnapshot(), nil)
				return err
			},
		},
		{
			name: "judge (auto)", reason: "dependent_run_active",
			arm: func(t *testing.T, f *sweepFix) {
				f.e.exec(t, `UPDATE users SET judge_enabled = true WHERE id = $1`, f.e.userID)
			},
			admit: func(f *sweepFix, svc *Service, _ int64) error {
				run, err := f.e.q.GetRunByID(f.e.ctx, f.run)
				if err != nil {
					return err
				}
				svc.maybeEnqueueJudge(f.e.ctx, run)
				return nil
			},
		},
		{
			name: "judge (rerun)", reason: "dependent_run_active",
			admit: func(f *sweepFix, svc *Service, _ int64) error {
				_, err := svc.RerunJudge(f.e.ctx, f.e.userID, false, f.run)
				return err
			},
		},
	}
}

// raceFixture builds a retire-enabled sweep fixture whose source run is live, terminal and proven
// closed, armed for fam, plus the service the consumer admission goes through.
func raceFixture(t *testing.T, fam raceFamily) (*sweepFix, *Service) {
	t.Helper()
	f := newSweepFix(t)
	f.enableRetire()
	f.e.exec(t, `INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
		VALUES ($1, $2, 'anthropic_token', 'race-token', true, $3, 'master')`, uuid.New(), f.e.userID, []byte("ct"))
	f.live()
	f.forges.issue = forge.Issue{State: "closed"}
	if fam.arm != nil {
		fam.arm(t, f)
	}
	svc := New(f.e.q, f.svc.box, testParams())
	svc.SetTxBeginner(f.e.pool)
	svc.SetForges(originDriftForges{f: &originDriftForge{}})
	svc.SetSettings(fakeSettings{enabled: true})
	return f, svc
}

// consumerRows is the number of runs of the fixture's owner besides the source run.
func (f *sweepFix) consumerRows() int {
	f.t.Helper()
	var n int
	if err := f.e.pool.QueryRow(f.e.ctx, `SELECT count(*) FROM runs WHERE user_id=$1 AND id<>$2`, f.e.userID, f.run).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// raceGate is a one-shot pause: the paused side signals reached and then waits for release (bounded
// by raceHold, so a failed test cannot wedge its goroutine). release is idempotent.
type raceGate struct {
	reached chan struct{}
	freed   chan struct{}
	once    sync.Once
	rel     sync.Once
}

func newRaceGate(t *testing.T) *raceGate {
	g := &raceGate{reached: make(chan struct{}), freed: make(chan struct{})}
	t.Cleanup(g.release)
	return g
}

func (g *raceGate) pause() {
	g.once.Do(func() { close(g.reached) })
	select {
	case <-g.freed:
	case <-time.After(raceHold):
	}
}

func (g *raceGate) release() { g.rel.Do(func() { close(g.freed) }) }

func raceAwait[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(raceWaitBound):
		t.Fatalf("timed out waiting for %s", what)
	}
	panic("unreachable")
}

func raceReached(t *testing.T, g *raceGate, what string) {
	t.Helper()
	select {
	case <-g.reached:
	case <-time.After(raceWaitBound):
		g.release()
		t.Fatalf("timed out waiting for %s", what)
	}
}

// raceRetirementFirst runs a retire pass paused inside tx-R1 (after the retiring write, before its
// COMMIT), starts the admission meanwhile and asserts no consumer commits inside that window; it then
// releases both sides and asserts the admission succeeded and the row never went back to live.
func raceRetirementFirst(t *testing.T, f *sweepFix, probe func() bool, admit func() error) {
	t.Helper()
	bounds := retireTestBounds
	f.svc.terminal.retireBounds = &bounds
	g := newRaceGate(t)
	f.svc.terminal.beforeRetiringCommit = g.pause
	sweepDone := make(chan error, 1)
	go func() {
		_, err := f.svc.SweepTerminalPreservations(f.e.ctx)
		sweepDone <- err
	}()
	raceReached(t, g, "the retirement decision window")

	admDone := make(chan error, 1)
	go func() { admDone <- admit() }()
	// Wait until the admission is queued behind the retirement on the gate key (a not-granted
	// ShareLock waiter, so it cannot have committed), or until its consumer shows up as committed
	// inside the window, which is the failure.
	committed, queued := false, false
	for deadline := time.Now().Add(racePoll); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if probe() {
			committed = true
			break
		}
		if gateLocks(t, f, f.e.repoID, "ShareLock", false) > 0 {
			queued = true
			break
		}
	}
	g.release()
	sweepErr := raceAwait(t, sweepDone, "the retire pass")
	admErr := raceAwait(t, admDone, "the admission")
	if committed {
		t.Errorf("consumer committed inside the retirement decision window")
	} else if !queued {
		t.Errorf("the admission never queued on the consumer gate within %v", racePoll)
	}
	if sweepErr != nil {
		t.Errorf("retire pass: %v", sweepErr)
	}
	if admErr != nil {
		t.Fatalf("admission after the window: %v", admErr)
	}
	if !probe() {
		t.Fatal("the admission returned without committing its consumer")
	}
	if p, ok := f.pres(); !ok || p.State == "live" {
		t.Fatalf("preservation = %+v (found %v), want retiring or later", p, ok)
	}
}

func TestTerminalConsumerRaceRetirementFirstLiveDB(t *testing.T) {
	for _, fam := range raceFamilies() {
		t.Run(fam.name, func(t *testing.T) {
			f, svc := raceFixture(t, fam)
			iid := raceSourceIID(t, f)
			raceRetirementFirst(t, f,
				func() bool { return f.consumerRows() > 0 },
				func() error { return fam.admit(f, svc, iid) })
		})
	}
}

func TestTerminalConsumerRaceConsumerFirstLiveDB(t *testing.T) {
	for _, fam := range raceFamilies() {
		t.Run(fam.name, func(t *testing.T) {
			f, svc := raceFixture(t, fam)
			iid := raceSourceIID(t, f)
			g := newRaceGate(t)
			svc.createBeforeCommitHook = func(store.Run) { g.pause() }
			admDone := make(chan error, 1)
			go func() { admDone <- fam.admit(f, svc, iid) }()
			raceReached(t, g, "the admission to reach its commit")

			// The admission has inserted its consumer but not committed: a retire pass must not retire.
			f.sweep()
			p, _ := f.pres()
			retired := f.deleteCount() != 0 || p.State != "live"
			gateBusy := p.LastError.String == "retire kept: consumer_gate_busy"
			g.release()
			if err := raceAwait(t, admDone, "the admission"); err != nil {
				t.Fatalf("admission: %v", err)
			}
			if retired {
				t.Fatalf("retired while a consumer admission was in flight (deletes=%d state=%s)", f.deleteCount(), p.State)
			}
			if !gateBusy {
				t.Fatalf("first pass last_error = %q, want the gate-busy keep while the admission held the gate", p.LastError.String)
			}
			if f.consumerRows() == 0 {
				t.Fatal("the admission returned without committing its consumer")
			}

			// Once the consumer is committed the next pass keeps the ref with the family's blocker.
			f.due()
			f.sweep()
			p = f.mustPres("live")
			if want := "retire kept: " + fam.reason; p.LastError.String != want || f.deleteCount() != 0 {
				t.Fatalf("last_error = %q deletes=%d, want %q and no delete", p.LastError.String, f.deleteCount(), want)
			}
		})
	}
}

// ReconcileRunMR is not a create: a nonterminal ci_fix consumer of the same repo with no merge
// request yet gains the source's mr_iid when its worker reports it (SetState, the real report path),
// which makes it a mr_run_active consumer of the source.
func TestTerminalConsumerRaceReconcileRunMRLiveDB(t *testing.T) {
	const mr = int64(777)
	f, svc := raceFixture(t, raceFamily{arm: func(t *testing.T, f *sweepFix) {
		raceBranch(t, f)
		f.e.exec(t, `UPDATE runs SET mr_iid=$2 WHERE id=$1`, f.run, mr)
	}})
	consumer := uuid.New()
	f.e.exec(t, `INSERT INTO runs (id, user_id, repo_id, kind, status, worker_id, issue_title, issue_description, pipeline_id, pipeline_ref)
		VALUES ($1, $2, $3, 'ci_fix', 'running', $4, 't', 'd', 1, $5)`, consumer, f.e.userID, f.e.repoID, f.worker.ID, "refs/heads/race-"+uuid.NewString())
	probe := func() bool {
		var got pgtype.Int8
		if err := f.e.pool.QueryRow(f.e.ctx, `SELECT mr_iid FROM runs WHERE id=$1`, consumer).Scan(&got); err != nil {
			t.Error(err)
			return false
		}
		return got.Valid && got.Int64 == mr
	}
	raceRetirementFirst(t, f, probe, func() error {
		m := mr
		_, _, err := svc.SetState(context.Background(), f.worker, consumer, StateRequest{State: "running", MrIID: &m})
		return err
	})
}
