package workersvc

import (
	"errors"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1795 M2 live-DB coverage: every plan-gate verdict insertion seam stamps the gate binding
// from the locked run row (bound while gated, bound(N) during a revision turn, unbound before the
// first gate, legacy at revision 0, always NULL for chat), a verdict racing a publication binds to
// exactly one revision in commit order, and an expected_gate_revision mismatch writes nothing
// (no selection, milestone freeze, revise_count, stop_kind, capability clear, server-side reject).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh. A package that prints `ok` with PASS=0 is INVALID, not green.

type verdictRow struct {
	kind     string
	binding  *string
	revision *int64
}

// The assertion helpers below take the SUBTEST's t: a failure must fail the subtest that
// observed it, never the fixture's parent t from a subtest goroutine.
func (f *gateFix) verdicts(t *testing.T, runID uuid.UUID) []verdictRow {
	t.Helper()
	rows, err := f.pool.Query(f.ctx, `SELECT kind, gate_binding, gate_revision FROM run_user_inputs WHERE run_id = $1 ORDER BY id`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []verdictRow
	for rows.Next() {
		var v verdictRow
		if err := rows.Scan(&v.kind, &v.binding, &v.revision); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

// submit posts a steering input as the run's owner.
func (f *gateFix) submit(runID uuid.UUID, kind string, sel *AgentSelection, opts SubmitInputOptions) (SubmitInputResult, error) {
	f.t.Helper()
	return f.svc.SubmitInputWithOptions(f.ctx, f.userID, runID, kind, "because", sel, opts)
}

// liveWorker makes the fixture worker a live poller, so a reject is enqueued (CreateStopVerdictInput)
// rather than applied server-side.
func (f *gateFix) liveWorker() {
	f.t.Helper()
	f.exec(`UPDATE workers SET last_heartbeat_at = now(), status = 'online' WHERE id = $1`, f.wkr.ID)
}

// staleWorker makes the fixture worker a dead poller, so a reject is applied server-side.
func (f *gateFix) staleWorker() {
	f.t.Helper()
	f.exec(`UPDATE workers SET last_heartbeat_at = NULL, status = 'offline' WHERE id = $1`, f.wkr.ID)
}

// gatedRun returns a run published at revision 1 (awaiting_approval, docker required, a
// one-milestone candidate).
func (f *gateFix) gatedRun() uuid.UUID {
	f.t.Helper()
	run := f.newRun("issue")
	f.mustPublish(run, gateReq(i64(1), uid()), 1)
	return run
}

func bindingString(v verdictRow) string {
	b := "legacy"
	if v.binding != nil {
		b = *v.binding
	}
	if v.revision != nil {
		return b + "(" + strconv.FormatInt(*v.revision, 10) + ")"
	}
	return b
}

// assertVerdicts compares the run's input rows (kind + binding) in insertion order.
func (f *gateFix) assertVerdicts(t *testing.T, runID uuid.UUID, want ...string) {
	t.Helper()
	var got []string
	for _, v := range f.verdicts(t, runID) {
		got = append(got, v.kind+"="+bindingString(v))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("verdict rows = %v, want %v", got, want)
	}
}

func TestGateVerdictBindingLiveDB(t *testing.T) {
	f := newGateFix(t, 3)
	own := &AgentSelection{Source: AgentSourceOwn}

	t.Run("bound while gated, on every verdict seam; cancel and follow_up stay legacy", func(t *testing.T) {
		f.liveWorker()
		run := f.gatedRun()
		for _, c := range []struct {
			kind string
			sel  *AgentSelection
		}{
			{"approve_plan", nil}, {"approve_plan", own}, {"revise_plan", nil}, {"reject_plan", nil}, {"follow_up", nil}, {"cancel", nil},
		} {
			if _, err := f.submit(run, c.kind, c.sel, SubmitInputOptions{}); err != nil {
				t.Fatalf("%s: %v", c.kind, err)
			}
		}
		f.assertVerdicts(t, run, "approve_plan=bound(1)", "approve_plan=bound(1)", "revise_plan=bound(1)",
			"reject_plan=bound(1)", "follow_up=legacy", "cancel=legacy")
	})

	t.Run("bound(N) during a revision turn: the status stays awaiting_approval", func(t *testing.T) {
		f.liveWorker()
		run := f.gatedRun()
		f.mustPublish(run, gateReq(i64(1), uid()), 2)
		if _, err := f.submit(run, "revise_plan", nil, SubmitInputOptions{}); err != nil {
			t.Fatal(err)
		}
		// The worker is re-planning; the run still shows the old gate at revision 2.
		if r := f.row(run); r.status != "awaiting_approval" || r.revision != 2 {
			t.Fatalf("revision turn: %+v", r)
		}
		for _, k := range []string{"approve_plan", "reject_plan"} {
			if _, err := f.submit(run, k, nil, SubmitInputOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		f.assertVerdicts(t, run, "revise_plan=bound(2)", "approve_plan=bound(2)", "reject_plan=bound(2)")
	})

	t.Run("unbound while running before the first gate", func(t *testing.T) {
		f.liveWorker()
		run := f.newRun("issue") // running, gate_revision 0
		for _, c := range []struct {
			kind string
			sel  *AgentSelection
		}{{"approve_plan", nil}, {"approve_plan", own}, {"revise_plan", nil}, {"reject_plan", nil}} {
			if _, err := f.submit(run, c.kind, c.sel, SubmitInputOptions{}); err != nil {
				t.Fatalf("%s: %v", c.kind, err)
			}
		}
		f.assertVerdicts(t, run, "approve_plan=unbound", "approve_plan=unbound", "revise_plan=unbound", "reject_plan=unbound")
	})

	t.Run("unbound while queued, and after a later gate is published at N", func(t *testing.T) {
		run := f.gatedRun()
		f.mustPublish(run, gateReq(i64(1), uid()), 2)
		// Requeued (e.g. a stale worker): no gate is visible. It keeps its revision counter. The
		// requirement is cleared so the (worker-less, fail-closed) capability gate lets it through.
		f.exec(`UPDATE runs SET status = 'queued', worker_id = NULL, required_capabilities = '{}' WHERE id = $1`, run)
		for _, k := range []string{"approve_plan", "revise_plan"} {
			if _, err := f.submit(run, k, nil, SubmitInputOptions{}); err != nil {
				t.Fatalf("%s: %v", k, err)
			}
		}
		f.assertVerdicts(t, run, "approve_plan=unbound", "revise_plan=unbound")
	})

	t.Run("legacy at revision 0 (a gate published before the migration)", func(t *testing.T) {
		f.liveWorker()
		run := f.newRun("issue")
		f.exec(`UPDATE runs SET status = 'awaiting_approval', plan_md = 'old plan' WHERE id = $1`, run)
		for _, c := range []struct {
			kind string
			sel  *AgentSelection
		}{{"approve_plan", nil}, {"approve_plan", own}, {"revise_plan", nil}, {"reject_plan", nil}} {
			if _, err := f.submit(run, c.kind, c.sel, SubmitInputOptions{}); err != nil {
				t.Fatalf("%s: %v", c.kind, err)
			}
		}
		f.assertVerdicts(t, run, "approve_plan=legacy", "approve_plan=legacy", "revise_plan=legacy", "reject_plan=legacy")
	})

	t.Run("a chat run's verdicts are always legacy", func(t *testing.T) {
		f.liveWorker()
		run := f.newRun("chat")
		if _, err := f.submit(run, "approve_plan", nil, SubmitInputOptions{}); err != nil {
			t.Fatal(err)
		}
		// Even a (never-allocated) nonzero revision on an awaiting_approval chat row stays legacy:
		// the kind, not the counter, decides.
		f.exec(`UPDATE runs SET status = 'awaiting_approval', gate_revision = 3 WHERE id = $1`, run)
		for _, k := range []string{"approve_plan", "revise_plan", "reject_plan"} {
			if _, err := f.submit(run, k, nil, SubmitInputOptions{}); err != nil {
				t.Fatalf("%s: %v", k, err)
			}
		}
		f.assertVerdicts(t, run, "approve_plan=legacy", "approve_plan=legacy", "revise_plan=legacy", "reject_plan=legacy")
	})

	t.Run("an expected revision that matches is accepted and bound", func(t *testing.T) {
		f.liveWorker()
		run := f.gatedRun()
		for _, c := range []struct {
			kind string
			sel  *AgentSelection
		}{{"approve_plan", nil}, {"approve_plan", own}, {"revise_plan", nil}, {"reject_plan", nil}} {
			if _, err := f.submit(run, c.kind, c.sel, SubmitInputOptions{ExpectedGateRevision: i64(1)}); err != nil {
				t.Fatalf("%s: %v", c.kind, err)
			}
		}
		f.assertVerdicts(t, run, "approve_plan=bound(1)", "approve_plan=bound(1)", "revise_plan=bound(1)", "reject_plan=bound(1)")
	})

	t.Run("an expected revision on a non-verdict kind is refused", func(t *testing.T) {
		run := f.gatedRun()
		for _, k := range []string{"follow_up", "cancel"} {
			if _, err := f.submit(run, k, nil, SubmitInputOptions{ExpectedGateRevision: i64(1)}); !errors.Is(err, ErrExpectedGateRevisionNotApplicable) {
				t.Fatalf("%s err = %v, want ErrExpectedGateRevisionNotApplicable", k, err)
			}
		}
		f.assertVerdicts(t, run)
	})
}

// TestGateVerdictRacesPublicationLiveDB: a verdict insert racing a gate publication (the M1
// SetStateReport path, which allocates under the same run-row lock) binds to exactly one
// revision, consistent with commit order.
func TestGateVerdictRacesPublicationLiveDB(t *testing.T) {
	f := newGateFix(t, 3)
	f.liveWorker()

	type contender func(run uuid.UUID) error
	// publish is the M1 publication path: a new presentation under the run's current claim
	// (gatedRun leaves it at generation 1).
	publish := func(run uuid.UUID) error {
		_, applied, rev, err := f.svc.SetStateReport(f.ctx, f.wkr, run, gateReq(i64(1), uid()))
		if err == nil && (!applied || rev != 2) {
			return errors.New("publication not applied at revision 2")
		}
		return err
	}
	approve := func(run uuid.UUID) error {
		_, err := f.submit(run, "approve_plan", nil, SubmitInputOptions{})
		return err
	}
	revise := func(run uuid.UUID) error {
		_, err := f.submit(run, "revise_plan", nil, SubmitInputOptions{})
		return err
	}
	reject := func(run uuid.UUID) error {
		_, err := f.submit(run, "reject_plan", nil, SubmitInputOptions{})
		return err
	}

	// runOrdered holds the run row lock, queues the two contenders in a known order behind it,
	// then releases it: Postgres grants a contended row lock to its waiters in arrival order, so
	// the first contender commits first.
	runOrdered := func(t *testing.T, first, second contender) uuid.UUID {
		t.Helper()
		run := f.gatedRun()
		tx, err := f.pool.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(f.ctx) }()
		if _, err := tx.Exec(f.ctx, `SELECT 1 FROM runs WHERE id = $1 FOR UPDATE`, run); err != nil {
			t.Fatal(err)
		}
		errs := make(chan error, 2)
		go func() { errs <- first(run) }()
		waitForLockWaiters(t, f, 1)
		go func() { errs <- second(run) }()
		waitForLockWaiters(t, f, 2)
		if err := tx.Commit(f.ctx); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if err := <-errs; err != nil {
				t.Fatalf("contender: %v", err)
			}
		}
		return run
	}

	for _, v := range []struct {
		name string
		fn   contender
		kind string
	}{{"approve", approve, "approve_plan"}, {"revise", revise, "revise_plan"}, {"reject", reject, "reject_plan"}} {
		t.Run(v.name+": publication commits first, the verdict binds to the new revision", func(t *testing.T) {
			run := runOrdered(t, publish, v.fn)
			f.assertVerdicts(t, run, v.kind+"=bound(2)")
		})
		t.Run(v.name+": the verdict commits first, it binds to the old revision", func(t *testing.T) {
			run := runOrdered(t, v.fn, publish)
			f.assertVerdicts(t, run, v.kind+"=bound(1)")
			if r := f.row(run); r.revision != 2 {
				t.Fatalf("publication after the verdict: revision %d, want 2", r.revision)
			}
		})
	}

	t.Run("unordered goroutines behind a barrier always bind to exactly one revision", func(t *testing.T) {
		for i := 0; i < 20; i++ {
			run := f.gatedRun()
			start := make(chan struct{})
			var wg sync.WaitGroup
			errs := make([]error, 2)
			for j, c := range []contender{publish, approve} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					errs[j] = c(run)
				}()
			}
			close(start)
			wg.Wait()
			if err := errors.Join(errs...); err != nil {
				t.Fatal(err)
			}
			got := f.verdicts(t, run)
			if len(got) != 1 || got[0].binding == nil || *got[0].binding != "bound" || got[0].revision == nil ||
				(*got[0].revision != 1 && *got[0].revision != 2) {
				t.Fatalf("iteration %d: verdict rows %+v, want exactly one bound(1|2)", i, got)
			}
		}
	})
}

// waitForLockWaiters blocks until at least n backends wait on a lock in this database.
func waitForLockWaiters(t *testing.T, f *gateFix, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var got int
		if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM pg_stat_activity
		      WHERE wait_event_type = 'Lock' AND datname = current_database() AND pid <> pg_backend_pid()`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fewer than %d contenders ever blocked on the run row lock", n)
}

// sideEffects is every column a refused verdict must leave untouched.
type sideEffects struct {
	status         string
	agentSource    *string
	frozen         []byte
	budgetIters    *int32
	reviseCount    int32
	stopKind       *string
	caps           []string
	failureReason  *string
	inputRows      int
	contractFrozen bool
}

func (f *gateFix) sideEffects(t *testing.T, runID uuid.UUID) sideEffects {
	t.Helper()
	var s sideEffects
	if err := f.pool.QueryRow(f.ctx, `SELECT status, agent_source, milestones_frozen, budget_max_iterations, revise_count,
	        stop_kind, required_capabilities, failure_reason, completion_contract IS NOT NULL,
	        (SELECT count(*) FROM run_user_inputs WHERE run_id = runs.id)
	      FROM runs WHERE id = $1`, runID).Scan(&s.status, &s.agentSource, &s.frozen, &s.budgetIters, &s.reviseCount,
		&s.stopKind, &s.caps, &s.failureReason, &s.contractFrozen, &s.inputRows); err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *gateFix) assertUntouched(t *testing.T, runID uuid.UUID, before sideEffects) {
	t.Helper()
	after := f.sideEffects(t, runID)
	if after.status != before.status || after.agentSource != nil || after.frozen != nil || after.budgetIters != nil ||
		after.reviseCount != before.reviseCount || after.stopKind != nil || !slices.Equal(after.caps, before.caps) ||
		after.failureReason != nil || after.inputRows != before.inputRows || after.contractFrozen {
		t.Fatalf("a refused verdict wrote: before %+v after %+v", before, after)
	}
}

func assertMismatch(t *testing.T, err error, current int64) {
	t.Helper()
	var mm *GateRevisionMismatchError
	if !errors.As(err, &mm) || mm.Current != current {
		t.Fatalf("err = %v, want *GateRevisionMismatchError{Current: %d}", err, current)
	}
}

func TestGateVerdictMismatchHasNoSideEffectsLiveDB(t *testing.T) {
	f := newGateFix(t, 3)
	own := &AgentSelection{Source: AgentSourceOwn}

	// Every verdict shape, including the capability-override approve; reject runs both with a
	// live poller (CreateStopVerdictInput) and without one (RejectRunServerSide).
	type verdict struct {
		name string
		kind string
		sel  *AgentSelection
		opt  SubmitInputOptions
		live bool
	}
	verdicts := []verdict{
		{"approve (no selection)", "approve_plan", nil, SubmitInputOptions{}, true},
		{"approve (selection)", "approve_plan", own, SubmitInputOptions{}, true},
		{"approve (capability override, selection)", "approve_plan", own, SubmitInputOptions{OverrideCapabilities: true}, true},
		{"approve (capability override, no selection)", "approve_plan", nil, SubmitInputOptions{OverrideCapabilities: true}, true},
		{"revise", "revise_plan", nil, SubmitInputOptions{}, true},
		{"reject (live poller)", "reject_plan", nil, SubmitInputOptions{}, true},
		{"reject (server-side)", "reject_plan", nil, SubmitInputOptions{}, false},
	}
	prepare := func(v verdict) uuid.UUID {
		if v.live {
			f.liveWorker()
		} else {
			f.staleWorker()
		}
		run := f.gatedRun()
		f.mustPublish(run, gateReq(i64(1), uid()), 2)
		return run
	}

	for _, v := range verdicts {
		t.Run(v.name+": refused up front against an older revision", func(t *testing.T) {
			run := prepare(v)
			before := f.sideEffects(t, run)
			v.opt.ExpectedGateRevision = i64(1)
			_, err := f.submit(run, v.kind, v.sel, v.opt)
			assertMismatch(t, err, 2)
			f.assertUntouched(t, run, before)
		})
		// The load-bearing case: the Go pre-check reads revision 2 and passes, and a publication
		// commits before the verdict's statement takes the row lock. Only the SQL predicate,
		// evaluated against the locked row, can refuse it.
		t.Run(v.name+": refused atomically when a publication lands after the pre-check", func(t *testing.T) {
			run := prepare(v)
			before := f.sideEffects(t, run)
			tx, err := f.pool.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(f.ctx) }()
			if _, err := tx.Exec(f.ctx, `SELECT 1 FROM runs WHERE id = $1 FOR UPDATE`, run); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			opt := v.opt
			opt.ExpectedGateRevision = i64(2)
			go func() {
				_, err := f.submit(run, v.kind, v.sel, opt)
				done <- err
			}()
			waitForLockWaiters(t, f, 1)
			if _, err := tx.Exec(f.ctx, `UPDATE runs SET gate_revision = 3 WHERE id = $1`, run); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(f.ctx); err != nil {
				t.Fatal(err)
			}
			assertMismatch(t, <-done, 3)
			f.assertUntouched(t, run, before)
		})
	}

	t.Run("an expected revision on a run with no visible gate is refused and does not fail it", func(t *testing.T) {
		f.staleWorker()
		run := f.newRun("issue") // running, never gated
		before := f.sideEffects(t, run)
		for _, k := range []string{"approve_plan", "revise_plan", "reject_plan"} {
			_, err := f.submit(run, k, nil, SubmitInputOptions{ExpectedGateRevision: i64(0)})
			assertMismatch(t, err, 0)
		}
		f.assertUntouched(t, run, before)
	})

	t.Run("without an expected revision the revise cap keeps its own error", func(t *testing.T) {
		run := f.gatedRun()
		f.exec(`UPDATE runs SET revise_count = 3 WHERE id = $1`, run)
		if _, err := f.submit(run, "revise_plan", nil, SubmitInputOptions{}); !errors.Is(err, ErrReviseCapReached) {
			t.Fatalf("err = %v, want ErrReviseCapReached", err)
		}
		if _, err := f.submit(run, "revise_plan", nil, SubmitInputOptions{ExpectedGateRevision: i64(1)}); !errors.Is(err, ErrReviseCapReached) {
			t.Fatalf("matching expected revision at the cap: err = %v, want ErrReviseCapReached", err)
		}
	})
}

// TestGateVerdictStoreQueriesLiveDB drives each stamped insert and the scoped clear directly, so
// the SQL predicates are pinned independently of the service's Go pre-check.
func TestGateVerdictStoreQueriesLiveDB(t *testing.T) {
	f := newGateFix(t, 3)
	run := f.gatedRun()
	f.mustPublish(run, gateReq(i64(1), uid()), 2)
	before := f.sideEffects(t, run)
	stale := pgconv.Int8Ptr(i64(1))

	if _, err := f.q.CreateGateVerdictInput(f.ctx, store.CreateGateVerdictInputParams{RunID: run, ExpectedGateRevision: stale}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("CreateGateVerdictInput err = %v, want no rows", err)
	}
	if _, err := f.q.CreateApprovePlanInput(f.ctx, store.CreateApprovePlanInputParams{
		RunID: run, AgentSource: pgconv.TextOrNull("own"), AgentExclusions: []byte("[]"), ExpectedGateRevision: stale,
		RunMaxIterations: 5, RunTimeoutSeconds: 3600, MilestoneBudgetCap: 5, SizeBudgetFactorL: 2, BudgetWallCeilingSeconds: 7200,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("CreateApprovePlanInput err = %v, want no rows", err)
	}
	if _, err := f.q.CreateRunReviseInputIfUnderCap(f.ctx, store.CreateRunReviseInputIfUnderCapParams{RunID: run, MaxRevisions: 3, ExpectedGateRevision: stale}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("CreateRunReviseInputIfUnderCap err = %v, want no rows", err)
	}
	if _, err := f.q.CreateStopVerdictInput(f.ctx, store.CreateStopVerdictInputParams{
		RunID: run, Kind: "reject_plan", StopKind: pgconv.TextOrNull("plan_rejected"), ExpectedGateRevision: stale,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("CreateStopVerdictInput err = %v, want no rows", err)
	}
	if n, err := f.q.RejectRunServerSide(f.ctx, store.RejectRunServerSideParams{ID: run, UserID: f.userID, FailureReason: pgconv.TextOrNull("no"), ExpectedGateRevision: stale}); err != nil || n != 0 {
		t.Fatalf("RejectRunServerSide = (%d, %v), want 0 rows", n, err)
	}
	// The capability clear is scoped to the approve's bound revision.
	if n, err := f.q.ClearRunRequiredCapabilities(f.ctx, store.ClearRunRequiredCapabilitiesParams{ID: run, UserID: f.userID, GateRevision: 1}); err != nil || n != 0 {
		t.Fatalf("ClearRunRequiredCapabilities at an old revision = (%d, %v), want 0 rows", n, err)
	}
	f.assertUntouched(t, run, before)
	if !slices.Contains(f.row(run).caps, capability.Docker) {
		t.Fatal("the stale clear removed the requirement")
	}
	if n, err := f.q.ClearRunRequiredCapabilities(f.ctx, store.ClearRunRequiredCapabilitiesParams{ID: run, UserID: f.userID, GateRevision: 2}); err != nil || n != 1 {
		t.Fatalf("ClearRunRequiredCapabilities at the bound revision = (%d, %v), want 1 row", n, err)
	}
}
