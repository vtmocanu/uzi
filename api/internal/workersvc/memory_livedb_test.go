package workersvc

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type memoryFixture struct {
	e    codexTestEnv
	s    *Service
	w    store.Worker
	b    MemoryBinding
	repo uuid.UUID
}

func newMemoryFixture(t *testing.T) memoryFixture {
	t.Helper()
	e := setupCodexLiveDB(t)
	user, _, repo := e.seedCodexInfra(t)
	wid := seedSnapshotWorker(t, e, user, "memory-nonce")
	e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", wid, []string{capability.WorkerMemoryPressureV1})
	run := seedOutageRun(t, e, user, repo, wid, "running", "issue", 2, 2)
	e.exec("UPDATE runs SET budget_wall_seconds=86400 WHERE id=$1", run)
	w, err := e.q.GetWorkerByID(e.ctx, wid)
	if err != nil {
		t.Fatal(err)
	}
	return memoryFixture{e: e, s: snapshotSvc(e, testParams()), w: w, repo: repo,
		b: MemoryBinding{RunID: run, WorkerID: wid, RegisterNonce: "memory-nonce",
			ClaimGeneration: 2, MemoryEpisode: 0, InterventionID: uuid.New()}}
}

func (f memoryFixture) reserve(t *testing.T, b MemoryBinding) MemoryReservation {
	t.Helper()
	r, err := f.s.ReserveMemoryIntervention(f.e.ctx, f.w, MemoryReservationRequest{MemoryBinding: b, Policy: MemoryPolicy{Version: 1, MaxInterventions: 3}})
	if err != nil {
		t.Fatal(err)
	}
	if r.MemoryBinding != b {
		t.Fatalf("binding echo=%+v want %+v", r.MemoryBinding, b)
	}
	return r
}

func (f memoryFixture) run(t *testing.T) store.Run {
	t.Helper()
	r, err := f.e.q.GetRunByID(f.e.ctx, f.b.RunID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f memoryFixture) hold(t *testing.T) store.Run {
	t.Helper()
	cause := recoveryCauseWorkerMemoryPressure
	r, applied, err := f.s.SetState(f.e.ctx, f.w, f.b.RunID, StateRequest{
		State: "recovery_wait", RecoveryCause: &cause, ClaimGeneration: &f.b.ClaimGeneration,
		RegisterNonce: f.b.RegisterNonce, MemoryEpisode: &f.b.MemoryEpisode})
	if err != nil || !applied {
		t.Fatalf("hold applied=%v err=%v", applied, err)
	}
	if r.RecoveryWaitCause.String != cause || r.RecoveryRetryNotBefore.Valid || !r.ClaimReleasedAt.Valid ||
		r.ReleasedWorkerID != pgconv.UUID(f.w.ID) || r.ReleasedWorkerNonce.String != f.b.RegisterNonce {
		t.Fatalf("hold fences: %+v", r)
	}
	return r
}

// PIDs identify the exact transactions started by the service. Tests observe
// actual PostgreSQL lock waits, bounded by a 10s context and 10000 probes.
type memoryBeginProbe struct {
	pool *pgxpool.Pool
	pids chan uint32
}

func (p memoryBeginProbe) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := p.pool.Begin(ctx)
	if err == nil {
		p.pids <- tx.Conn().PgConn().PID()
	}
	return tx, err
}

func memoryWaitBlocked(t *testing.T, e codexTestEnv, pids <-chan uint32, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
	defer cancel()
	for i := 0; i < n; i++ {
		var pid uint32
		select {
		case pid = <-pids:
		case <-ctx.Done():
			t.Fatal("transaction did not begin")
		}
		blocked := false
		for attempts := 0; attempts < 10000; attempts++ {
			if err := e.pool.QueryRow(ctx, "SELECT cardinality(pg_blocking_pids($1))>0", pid).Scan(&blocked); err != nil {
				t.Fatal(err)
			}
			if blocked {
				break
			}
			runtime.Gosched()
		}
		if !blocked {
			t.Fatalf("transaction %d never blocked on held row", pid)
		}
	}
}

type memoryResult struct {
	r   MemoryReservation
	err error
}

func memoryResults(t *testing.T, results <-chan memoryResult, n int) []MemoryReservation {
	t.Helper()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	out := make([]MemoryReservation, 0, n)
	for i := 0; i < n; i++ {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatal(result.err)
			}
			out = append(out, result.r)
		case <-timer.C:
			t.Fatal("reservation did not finish")
		}
	}
	return out
}

// All contenders are queued on a real held worker lock before release. A failure
// is collected from every sibling; the 15s contexts bound their lock waits.
func memoryConcurrent(t *testing.T, used int32, sameID bool, n int) (memoryFixture, []MemoryReservation) {
	t.Helper()
	f := newMemoryFixture(t)
	f.e.exec("UPDATE runs SET memory_policy=$2,memory_intervention_count=$3 WHERE id=$1", f.b.RunID,
		[]byte(`{"version":1,"max_interventions":3}`), used)
	blocker, err := f.e.pool.Begin(f.e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(f.e.ctx) }()
	if _, err = store.New(blocker).GetWorkerForUpdate(f.e.ctx, f.w.ID); err != nil {
		t.Fatal(err)
	}
	pids := make(chan uint32, n)
	f.s.SetTxBeginner(memoryBeginProbe{pool: f.e.pool, pids: pids})
	results := make(chan memoryResult, n)
	ctx, cancel := context.WithTimeout(f.e.ctx, 15*time.Second)
	defer cancel()
	for i := 0; i < n; i++ {
		b := f.b
		if !sameID {
			b.InterventionID = uuid.New()
		}
		go func() {
			r, err := f.s.ReserveMemoryIntervention(ctx, f.w, MemoryReservationRequest{MemoryBinding: b, Policy: MemoryPolicy{Version: 1, MaxInterventions: 3}})
			results <- memoryResult{r, err}
		}()
	}
	memoryWaitBlocked(t, f.e, pids, n)
	if err = blocker.Commit(f.e.ctx); err != nil {
		t.Fatal(err)
	}
	return f, memoryResults(t, results, n)
}

func TestMemoryReservationConcurrentLastSlotLiveDB(t *testing.T) {
	f, rs := memoryConcurrent(t, 2, false, 2)
	if rs[0].Admitted == rs[1].Admitted || f.run(t).MemoryInterventionCount != 3 {
		t.Fatalf("last slot admitted=%v/%v count=%d", rs[0].Admitted, rs[1].Admitted, f.run(t).MemoryInterventionCount)
	}
	for _, r := range rs {
		if r.Allowance.Used != 3 || r.Allowance.Remaining != 0 {
			t.Fatalf("allowance=%+v", r.Allowance)
		}
	}
}

func TestMemoryReservationConcurrentSameIDLiveDB(t *testing.T) {
	f, rs := memoryConcurrent(t, 0, true, 2)
	if rs[0] != rs[1] || !rs[0].Authorizing || rs[0].Allowance.Used != 1 || f.run(t).MemoryInterventionCount != 1 {
		t.Fatalf("duplicate responses=%+v count=%d", rs, f.run(t).MemoryInterventionCount)
	}
	var count int
	if err := f.e.pool.QueryRow(f.e.ctx, "SELECT count(*) FROM memory_interventions WHERE run_id=$1", f.b.RunID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("ledger count=%d", count)
	}
}

func TestMemoryReservationConcurrentDistinctIDsAtCapLiveDB(t *testing.T) {
	f, rs := memoryConcurrent(t, 3, false, 2)
	for _, r := range rs {
		if r.Admitted || r.Authorizing || r.Allowance.Used != 3 {
			t.Fatalf("cap response=%+v", r)
		}
	}
	if f.run(t).MemoryInterventionCount != 3 {
		t.Fatal("denial charged allowance")
	}
}

func TestMemoryReservationImmutableBindingLiveDB(t *testing.T) {
	f := newMemoryFixture(t)
	original := f.reserve(t, f.b)
	for _, change := range []func(*MemoryBinding){
		func(b *MemoryBinding) { b.RegisterNonce = "other" },
		func(b *MemoryBinding) { b.ClaimGeneration++ },
		func(b *MemoryBinding) { b.MemoryEpisode++ },
		func(b *MemoryBinding) {
			b.RunID = seedOutageRun(t, f.e, f.w.UserID, f.repo, f.w.ID, "running", "issue", 2, 0)
		},
	} {
		b := f.b
		change(&b)
		_, err := f.s.ReserveMemoryIntervention(f.e.ctx, f.w, MemoryReservationRequest{MemoryBinding: b, Policy: MemoryPolicy{Version: 1, MaxInterventions: 3}})
		if !errors.Is(err, ErrMemoryBinding) {
			t.Fatalf("changed binding=%+v error=%v", b, err)
		}
	}
	other := seedSnapshotWorker(t, f.e, f.w.UserID, "other-worker")
	f.e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", other, []string{capability.WorkerMemoryPressureV1})
	w, err := f.e.q.GetWorkerByID(f.e.ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	b := f.b
	b.WorkerID = other
	b.RegisterNonce = "other-worker"
	_, err = f.s.ReserveMemoryIntervention(f.e.ctx, w, MemoryReservationRequest{MemoryBinding: b, Policy: MemoryPolicy{Version: 1, MaxInterventions: 3}})
	if !errors.Is(err, ErrMemoryBinding) {
		t.Fatalf("worker rebinding=%v", err)
	}
	if again := f.reserve(t, f.b); again != original || f.run(t).MemoryInterventionCount != 1 {
		t.Fatal("binding changed or charged twice")
	}
}

func TestMemoryReservationOwnerResumeRaceLiveDB(t *testing.T) {
	f := newMemoryFixture(t)
	f.reserve(t, f.b)
	f.hold(t)
	owner, err := f.e.pool.Begin(f.e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Rollback(f.e.ctx) }()
	_, err = store.New(owner).ResumeMemoryEpisode(f.e.ctx, store.ResumeMemoryEpisodeParams{
		ID: f.b.RunID, UserID: f.w.UserID, GlobalTimeoutSeconds: 86400})
	if err != nil {
		t.Fatal(err)
	}
	pids := make(chan uint32, 1)
	f.s.SetTxBeginner(memoryBeginProbe{pool: f.e.pool, pids: pids})
	results := make(chan memoryResult, 1)
	ctx, cancel := context.WithTimeout(f.e.ctx, 15*time.Second)
	defer cancel()
	go func() {
		r, err := f.s.ReserveMemoryIntervention(ctx, f.w, MemoryReservationRequest{MemoryBinding: f.b, Policy: MemoryPolicy{Version: 1, MaxInterventions: 3}})
		results <- memoryResult{r, err}
	}()
	memoryWaitBlocked(t, f.e, pids, 1)
	if err = owner.Commit(f.e.ctx); err != nil {
		t.Fatal(err)
	}
	r := memoryResults(t, results, 1)[0]
	if r.Authorizing || !r.Admitted || r.MemoryEpisode != 0 || f.run(t).MemoryEpisode != 1 ||
		f.run(t).MemoryInterventionCount != 0 {
		t.Fatalf("old retry revived authority: %+v", r)
	}
}

func TestMemoryReservationNonceRotationRaceLiveDB(t *testing.T) {
	f := newMemoryFixture(t)
	f.reserve(t, f.b)
	rotation, err := f.e.pool.Begin(f.e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rotation.Rollback(f.e.ctx) }()
	if _, err = store.New(rotation).GetWorkerForUpdate(f.e.ctx, f.w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = rotation.Exec(f.e.ctx, "UPDATE workers SET snapshot_register_nonce='rotated' WHERE id=$1", f.w.ID); err != nil {
		t.Fatal(err)
	}
	pids := make(chan uint32, 1)
	f.s.SetTxBeginner(memoryBeginProbe{pool: f.e.pool, pids: pids})
	results := make(chan memoryResult, 1)
	ctx, cancel := context.WithTimeout(f.e.ctx, 15*time.Second)
	defer cancel()
	go func() {
		r, err := f.s.ReserveMemoryIntervention(ctx, f.w, MemoryReservationRequest{MemoryBinding: f.b, Policy: MemoryPolicy{Version: 1, MaxInterventions: 3}})
		results <- memoryResult{r, err}
	}()
	memoryWaitBlocked(t, f.e, pids, 1)
	if err = rotation.Commit(f.e.ctx); err != nil {
		t.Fatal(err)
	}
	r := memoryResults(t, results, 1)[0]
	if r.Authorizing || f.run(t).MemoryInterventionCount != 1 {
		t.Fatal("old nonce authorized or charged again")
	}
	f.s.SetTxBeginner(f.e.pool)
	b := f.b
	b.InterventionID = uuid.New()
	if _, err = f.s.ReserveMemoryIntervention(f.e.ctx, f.w, MemoryReservationRequest{MemoryBinding: b, Policy: MemoryPolicy{Version: 1, MaxInterventions: 3}}); !errors.Is(err, ErrMemoryStale) {
		t.Fatalf("new stale nonce reservation=%v", err)
	}
}

func TestMemoryReservationHistoricalRetryAfterNewClaimLiveDB(t *testing.T) {
	f := newMemoryFixture(t)
	before := f.reserve(t, f.b)
	next := seedSnapshotWorker(t, f.e, f.w.UserID, "next")
	f.hold(t)
	if _, err := f.e.q.ResumeMemoryEpisode(f.e.ctx, store.ResumeMemoryEpisodeParams{
		ID: f.b.RunID, UserID: f.w.UserID, GlobalTimeoutSeconds: 86400}); err != nil {
		t.Fatal(err)
	}
	w, err := f.e.q.GetWorkerByID(f.e.ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := f.e.q.ClaimRun(f.e.ctx, claimRunParams(w))
	if err != nil || claimed.ID != f.b.RunID || claimed.ClaimGeneration != 3 {
		t.Fatalf("new claim=%+v err=%v", claimed, err)
	}
	memoryAssertOrdinaryReportsRefused(t, f)
	r := f.reserve(t, f.b)
	if r.Authorizing || r.MemoryBinding != before.MemoryBinding || r.Allowance != before.Allowance ||
		f.run(t).MemoryInterventionCount != 0 {
		t.Fatalf("historical retry=%+v", r)
	}
	outcome, err := f.s.RecordMemoryInterventionOutcome(f.e.ctx, f.w, MemoryOutcomeRequest{MemoryBinding: f.b, Outcome: "confirmed_drained"})
	if err != nil || outcome.Authorizing || f.run(t).ClaimGeneration != 3 {
		t.Fatalf("historical outcome=%+v err=%v", outcome, err)
	}
}

func TestMemoryReservationNoSignalNoRefundLiveDB(t *testing.T) {
	for _, outcome := range []string{"no_signal", "unknown", "confirmed_drained"} {
		t.Run(outcome, func(t *testing.T) {
			f := newMemoryFixture(t)
			f.reserve(t, f.b)
			first, err := f.s.RecordMemoryInterventionOutcome(f.e.ctx, f.w, MemoryOutcomeRequest{MemoryBinding: f.b, Outcome: outcome})
			if err != nil {
				t.Fatal(err)
			}
			second, err := f.s.RecordMemoryInterventionOutcome(f.e.ctx, f.w, MemoryOutcomeRequest{MemoryBinding: f.b, Outcome: outcome})
			if err != nil || first != second || second.Authorizing {
				t.Fatalf("outcome retries=%+v/%+v err=%v", first, second, err)
			}
			if f.run(t).MemoryInterventionCount != 1 {
				t.Fatal("outcome refunded")
			}
			if retry := f.reserve(t, f.b); retry.Authorizing || retry.Outcome != outcome {
				t.Fatalf("settled retry revived: %+v", retry)
			}
			req := MemoryOutcomeRequest{MemoryBinding: f.b, Outcome: "unknown"}
			if outcome == "unknown" {
				req.Outcome = "confirmed_drained"
			}
			if _, err = f.s.RecordMemoryInterventionOutcome(f.e.ctx, f.w, req); !errors.Is(err, ErrMemoryBinding) {
				t.Fatalf("outcome changed: %v", err)
			}
		})
	}
}

func TestMemoryHoldOwnerResumePreservesUnknownDrainLiveDB(t *testing.T) {
	f := newMemoryFixture(t)
	f.reserve(t, f.b)
	if _, err := f.s.RecordMemoryInterventionOutcome(f.e.ctx, f.w, MemoryOutcomeRequest{MemoryBinding: f.b, Outcome: "unknown"}); err != nil {
		t.Fatal(err)
	}
	hold := uuid.New()
	f.e.exec(`INSERT INTO recovery_custody_holds
        (id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,inventory_guarded)
        VALUES($1,$2,$3,$4,2,'open',$5,'memory-fixture',true)`, hold, f.w.UserID, f.repo, f.b.RunID, f.w.ID)
	f.e.exec("UPDATE runs SET worker_recovery_episode=4,requeue_episode_baseline=1,stale_requeue_generation=2,finalize_resume_generation=1,iteration_count=7 WHERE id=$1", f.b.RunID)
	held := f.hold(t)
	_, err := f.e.q.ResumeMemoryEpisode(f.e.ctx, store.ResumeMemoryEpisodeParams{
		ID: f.b.RunID, UserID: f.w.UserID, GlobalTimeoutSeconds: 86400})
	if err != nil {
		t.Fatal(err)
	}
	r := f.run(t)
	if r.Status != "queued" || r.MemoryEpisode != 1 || r.MemoryInterventionCount != 0 || len(r.MemoryPolicy) != 0 ||
		r.WorkerRecoveryEpisode != held.WorkerRecoveryEpisode || r.RequeueCount != held.RequeueCount ||
		r.RequeueEpisodeBaseline != held.RequeueEpisodeBaseline || r.StaleRequeueGeneration != held.StaleRequeueGeneration ||
		r.FinalizeResumeGeneration != held.FinalizeResumeGeneration || r.IterationCount != held.IterationCount ||
		r.ClaimGeneration != held.ClaimGeneration || r.ClaimReleasedAt != held.ClaimReleasedAt ||
		r.ReleasedWorkerID != held.ReleasedWorkerID || r.ReleasedWorkerNonce != held.ReleasedWorkerNonce ||
		r.StartedAt != held.StartedAt || r.BudgetWallSeconds != held.BudgetWallSeconds ||
		r.BudgetPausedSeconds < held.BudgetPausedSeconds || r.WorkerID.Valid {
		t.Fatalf("memory Resume lost independent fences/counters: held=%+v after=%+v", held, r)
	}
	var state string
	if err = f.e.pool.QueryRow(f.e.ctx, "SELECT state FROM recovery_custody_holds WHERE id=$1", hold).Scan(&state); err != nil {
		t.Fatal(err)
	}
	row, err := f.e.q.GetMemoryIntervention(f.e.ctx, f.b.InterventionID)
	if err != nil || row.Outcome.String != "unknown" || state != "open" {
		t.Fatalf("drain/custody altered: %s/%s %v", row.Outcome.String, state, err)
	}
	if f.reserve(t, f.b).Authorizing {
		t.Fatal("Resume revived old intervention")
	}
}

func TestMemoryOutcomeAndFeedDeduplicationLiveDB(t *testing.T) {
	f := newMemoryFixture(t)
	f.reserve(t, f.b)
	frame := IncomingMessage{Seq: 1, Kind: "status", Payload: []byte(`{"event":"memory_intervention","outcome":"unknown"}`)}
	// Outcomes never allocate seq; the worker delivers feedback on the existing append seam.
	for i := 0; i < 2; i++ {
		if _, err := f.s.RecordMemoryInterventionOutcome(f.e.ctx, f.w, MemoryOutcomeRequest{MemoryBinding: f.b, Outcome: "unknown"}); err != nil {
			t.Fatal(err)
		}
		if err := f.s.AppendMessagesForClaim(f.e.ctx, f.w, f.b.RunID, []IncomingMessage{frame}, &f.b.ClaimGeneration); err != nil {
			t.Fatal(err)
		}
	}
	var frames, ledger int
	if err := f.e.pool.QueryRow(f.e.ctx, "SELECT count(*) FROM run_messages WHERE run_id=$1", f.b.RunID).Scan(&frames); err != nil {
		t.Fatal(err)
	}
	if err := f.e.pool.QueryRow(f.e.ctx, "SELECT count(*) FROM memory_interventions WHERE run_id=$1", f.b.RunID).Scan(&ledger); err != nil {
		t.Fatal(err)
	}
	if frames != 1 || ledger != 1 || f.run(t).LastSeq != 1 || f.run(t).MemoryInterventionCount != 1 {
		t.Fatalf("frames=%d ledger=%d seq=%d", frames, ledger, f.run(t).LastSeq)
	}
}
