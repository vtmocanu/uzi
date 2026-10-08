package workersvc

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestMemoryFreshAdmissionRefusedLiveDB(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"pending pause", "UPDATE runs SET pause_requested_at=now() WHERE id=$1"},
		{"pending credential switch", "UPDATE runs SET credential_switch_requested_at=now() WHERE id=$1"},
		{"released", "UPDATE runs SET claim_released_at=now() WHERE id=$1"},
		{"queued", "UPDATE runs SET status='queued' WHERE id=$1"},
		{"paused", "UPDATE runs SET status='paused' WHERE id=$1"},
		{"approval", "UPDATE runs SET status='awaiting_approval' WHERE id=$1"},
		{"input", "UPDATE runs SET status='awaiting_input' WHERE id=$1"},
		{"followup", "UPDATE runs SET status='awaiting_followup' WHERE id=$1"},
		{"limit wait", "UPDATE runs SET status='limit_wait' WHERE id=$1"},
		{"recovery wait", "UPDATE runs SET status='recovery_wait' WHERE id=$1"},
		{"cancelled", "UPDATE runs SET status='cancelled' WHERE id=$1"},
		{"completed", "UPDATE runs SET status='completed' WHERE id=$1"},
		{"failed", "UPDATE runs SET status='failed' WHERE id=$1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMemoryFixture(t)
			f.e.exec(tc.sql, f.b.RunID)
			before := f.run(t)
			_, err := f.s.ReserveMemoryIntervention(f.e.ctx, f.w, MemoryReservationRequest{
				MemoryBinding: f.b, Policy: MemoryPolicy{Version: 1, MaxInterventions: 3}})
			if !errors.Is(err, ErrMemoryStale) {
				t.Fatalf("fresh admission=%v", err)
			}
			if !reflect.DeepEqual(before, f.run(t)) {
				t.Fatal("refused admission changed run")
			}
			if _, err := f.e.q.GetMemoryIntervention(f.e.ctx, f.b.InterventionID); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("refused admission created ledger entry: %v", err)
			}
		})
	}
}

func memoryAssertOrdinaryReportsRefused(t *testing.T, f memoryFixture) {
	t.Helper()
	before := f.run(t)
	refusal := ErrStaleClaim
	if before.WorkerID != pgconv.UUID(f.w.ID) {
		refusal = ErrRunNotOwned
	}
	for _, state := range []string{"running", "completed", "failed"} {
		_, applied, err := f.s.SetState(f.e.ctx, f.w, f.b.RunID,
			StateRequest{State: state, ClaimGeneration: &f.b.ClaimGeneration})
		if applied || !errors.Is(err, refusal) {
			t.Fatalf("old %s report applied=%v err=%v", state, applied, err)
		}
	}
	err := f.s.AppendMessagesForClaim(f.e.ctx, f.w, f.b.RunID, []IncomingMessage{
		{Seq: 1, Kind: "status", Payload: []byte(`{"event":"late_memory_feedback"}`)},
	}, &f.b.ClaimGeneration)
	if !errors.Is(err, refusal) {
		t.Fatalf("old feed report=%v", err)
	}
	if !reflect.DeepEqual(before, f.run(t)) {
		t.Fatal("stale ordinary report changed run")
	}
	var frames int
	if err := f.e.pool.QueryRow(f.e.ctx, "SELECT count(*) FROM run_messages WHERE run_id=$1", f.b.RunID).Scan(&frames); err != nil {
		t.Fatal(err)
	}
	if frames != 0 {
		t.Fatalf("stale feed persisted %d frames", frames)
	}
}

// Both lanes use real claim writes. Register renews the same worker's incarnation;
// it does not renew the memory episode or any worker-death retry allowance.
func TestMemoryHoldResumeClaimIncarnationLiveDB(t *testing.T) {
	for _, chat := range []bool{false, true} {
		name := "issue"
		if chat {
			name = "chat"
		}
		t.Run(name, func(t *testing.T) {
			f := newMemoryFixture(t)
			if chat {
				// Remove the issue fixture from this worker's live claim set.
				f.e.exec("UPDATE runs SET status='completed' WHERE id=$1", f.b.RunID)
				f.b.RunID = seedOutageChatRun(t, f.e, f.w.UserID, f.w.ID, 2, 2)
			}
			claim := func(w store.Worker) (store.Run, error) {
				if chat {
					return f.e.q.ClaimChatRun(f.e.ctx, store.ClaimChatRunParams{
						WorkerID: pgconv.UUID(w.ID), UserID: w.UserID,
						AffinityCutoff: pgconv.Time(time.Now().Add(-time.Hour))})
				}
				return f.e.q.ClaimRun(f.e.ctx, claimRunParams(w))
			}
			f.reserve(t, f.b)
			held := f.hold(t)
			if _, err := claim(f.w); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("claim memory hold=%v", err)
			}
			memoryAssertOrdinaryReportsRefused(t, f)
			if _, err := f.e.q.ResumeMemoryEpisode(f.e.ctx, store.ResumeMemoryEpisodeParams{
				ID: f.b.RunID, UserID: f.w.UserID, GlobalTimeoutSeconds: 86400}); err != nil {
				t.Fatal(err)
			}
			resumed := f.run(t)
			if _, err := claim(f.w); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("released incarnation reclaimed after Resume: %v", err)
			}
			if !reflect.DeepEqual(resumed, f.run(t)) {
				t.Fatal("refused claim changed resumed row")
			}
			w, nonce, err := f.s.Register(f.e.ctx, f.w, "fixture", "", nil, nil,
				[]string{capability.WorkerMemoryPressureV1}, nil)
			if err != nil || nonce == "" || nonce == f.b.RegisterNonce {
				t.Fatalf("registration nonce=%q err=%v", nonce, err)
			}
			fresh, err := claim(w)
			if err != nil || fresh.ID != f.b.RunID || fresh.ClaimGeneration != 3 ||
				fresh.ClaimReleasedAt.Valid || fresh.ReleasedWorkerID.Valid || fresh.ReleasedWorkerNonce.Valid ||
				fresh.MemoryEpisode != 1 || fresh.MemoryInterventionCount != 0 ||
				fresh.WorkerRecoveryEpisode != held.WorkerRecoveryEpisode || fresh.RequeueCount != held.RequeueCount {
				t.Fatalf("fresh incarnation claim=%+v err=%v", fresh, err)
			}
			memoryAssertOrdinaryReportsRefused(t, f)
			if f.reserve(t, f.b).Authorizing {
				t.Fatal("old reservation regained authority")
			}
			b := f.b
			b.RegisterNonce, b.ClaimGeneration, b.MemoryEpisode, b.InterventionID = nonce, fresh.ClaimGeneration, fresh.MemoryEpisode, uuid.New()
			newFlight := f
			newFlight.w = w
			if got := newFlight.reserve(t, b); !got.Admitted || !got.Authorizing || got.Allowance.Used != 1 {
				t.Fatalf("fresh incarnation admission=%+v", got)
			}
		})
	}
}

func TestMemoryBindingCollisionAndOutcomeLiveDB(t *testing.T) {
	f := newMemoryFixture(t)
	f.reserve(t, f.b)
	original, err := f.e.q.GetMemoryIntervention(f.e.ctx, f.b.InterventionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, foreignTenant := range []bool{false, true} {
		user, repo := f.w.UserID, f.repo
		if foreignTenant {
			user, _, repo = f.e.seedCodexInfra(t)
		}
		wid := seedSnapshotWorker(t, f.e, user, "collision")
		f.e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", wid, []string{capability.WorkerMemoryPressureV1})
		w, err := f.e.q.GetWorkerByID(f.e.ctx, wid)
		if err != nil {
			t.Fatal(err)
		}
		b := f.b
		b.WorkerID, b.RegisterNonce = wid, "collision"
		b.RunID = seedOutageRun(t, f.e, user, repo, wid, "running", "issue", 2, 0)
		for _, outcome := range []bool{false, true} {
			if outcome {
				_, err = f.s.RecordMemoryInterventionOutcome(f.e.ctx, w,
					MemoryOutcomeRequest{MemoryBinding: b, Outcome: "confirmed_drained"})
			} else {
				_, err = f.s.ReserveMemoryIntervention(f.e.ctx, w,
					MemoryReservationRequest{MemoryBinding: b, Policy: MemoryPolicy{Version: 1, MaxInterventions: 3}})
			}
			if !errors.Is(err, ErrMemoryBinding) {
				t.Fatalf("tenant=%v outcome=%v rebinding=%v", foreignTenant, outcome, err)
			}
		}
		run, err := f.e.q.GetRunByID(f.e.ctx, b.RunID)
		if err != nil || run.MemoryInterventionCount != 0 || len(run.MemoryPolicy) != 0 {
			t.Fatalf("losing run charged: %+v err=%v", run, err)
		}
	}
	for _, change := range []func(*MemoryBinding){
		func(b *MemoryBinding) { b.RegisterNonce = "wrong" },
		func(b *MemoryBinding) { b.ClaimGeneration++ },
		func(b *MemoryBinding) { b.MemoryEpisode++ },
		func(b *MemoryBinding) {
			b.RunID = seedOutageRun(t, f.e, f.w.UserID, f.repo, f.w.ID, "running", "issue", 2, 0)
		},
	} {
		b := f.b
		change(&b)
		if _, err := f.s.RecordMemoryInterventionOutcome(f.e.ctx, f.w,
			MemoryOutcomeRequest{MemoryBinding: b, Outcome: "unknown"}); !errors.Is(err, ErrMemoryBinding) {
			t.Fatalf("outcome binding=%+v err=%v", b, err)
		}
	}
	after, err := f.e.q.GetMemoryIntervention(f.e.ctx, f.b.InterventionID)
	if err != nil || !reflect.DeepEqual(original, after) || f.run(t).MemoryInterventionCount != 1 {
		t.Fatalf("winner changed: %+v err=%v", after, err)
	}
}

func TestMemoryConcurrentDistinctRunsLiveDB(t *testing.T) {
	for _, sameID := range []bool{false, true} {
		name := "distinct IDs"
		if sameID {
			name = "same ID collision"
		}
		t.Run(name, func(t *testing.T) {
			f := newMemoryFixture(t)
			wid := seedSnapshotWorker(t, f.e, f.w.UserID, "second")
			f.e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", wid, []string{capability.WorkerMemoryPressureV1})
			w, err := f.e.q.GetWorkerByID(f.e.ctx, wid)
			if err != nil {
				t.Fatal(err)
			}
			b := f.b
			b.WorkerID, b.RegisterNonce = wid, "second"
			b.RunID = seedOutageRun(t, f.e, w.UserID, f.repo, wid, "running", "issue", 2, 0)
			if !sameID {
				b.InterventionID = uuid.New()
			}
			blocker, err := f.e.pool.Begin(f.e.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = blocker.Rollback(f.e.ctx) }()
			q := store.New(blocker)
			for _, id := range []uuid.UUID{f.w.ID, wid} {
				if _, err := q.GetWorkerForUpdate(f.e.ctx, id); err != nil {
					t.Fatal(err)
				}
			}
			pids := make(chan uint32, 2)
			results := make(chan memoryResult, 2)
			f.s.SetTxBeginner(memoryBeginProbe{pool: f.e.pool, pids: pids})
			ctx, cancel := context.WithTimeout(f.e.ctx, 15*time.Second)
			defer cancel()
			// Two attempts, each bounded by ctx; neither failure stops its sibling.
			for _, input := range []struct {
				w store.Worker
				b MemoryBinding
			}{{f.w, f.b}, {w, b}} {
				go func() {
					r, err := f.s.ReserveMemoryIntervention(ctx, input.w,
						MemoryReservationRequest{MemoryBinding: input.b, Policy: MemoryPolicy{Version: 1, MaxInterventions: 3}})
					results <- memoryResult{r, err}
				}()
			}
			memoryWaitBlocked(t, f.e, pids, 2)
			if err := blocker.Commit(f.e.ctx); err != nil {
				t.Fatal(err)
			}
			admitted, conflicts := 0, 0
			winners := make(map[uuid.UUID]int32)
			// Collect both results before assertions, with the same bounded context.
			for i := 0; i < 2; i++ {
				select {
				case result := <-results:
					if errors.Is(result.err, ErrMemoryBinding) {
						conflicts++
					} else if result.err != nil {
						t.Errorf("reservation=%v", result.err)
					} else if result.r.Admitted && result.r.Authorizing && result.r.Allowance.Used == 1 {
						admitted++
						winners[result.r.RunID]++
					} else {
						t.Errorf("reservation=%+v", result.r)
					}
				case <-ctx.Done():
					t.Fatal("distinct run reservations did not finish")
				}
			}
			wantAdmitted, wantConflicts := 2, 0
			if sameID {
				wantAdmitted, wantConflicts = 1, 1
			}
			second, err := f.e.q.GetRunByID(f.e.ctx, b.RunID)
			if err != nil || admitted != wantAdmitted || conflicts != wantConflicts ||
				f.run(t).MemoryInterventionCount != winners[f.b.RunID] || second.MemoryInterventionCount != winners[b.RunID] {
				t.Fatalf("admitted=%d conflicts=%d second=%+v err=%v", admitted, conflicts, second, err)
			}
		})
	}
}

func memoryCheckerFixture(t *testing.T) (memoryFixture, uuid.UUID) {
	t.Helper()
	f := newMemoryFixture(t)
	lead := seedOutageRun(t, f.e, f.w.UserID, f.repo, f.w.ID, "running", "issue", 2, 0)
	f.e.exec("UPDATE runs SET kind='cross_check',issue_iid=NULL,target_run_id=$2,report_only=true WHERE id=$1", f.b.RunID, lead)
	f.e.exec(`INSERT INTO cross_checks
		(lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,size_class,base_commit,candidate_digest,checker_run_id,checker_harness,deadline_at)
		VALUES($1,'plan',1,2,'plan','[]','s',$2,$3,$4,'claude',now()+interval '30 minutes')`,
		lead, strings.Repeat("a", 40), []byte("memory-candidate"), f.b.RunID)
	return f, lead
}

func TestMemoryCrossCheckParentScopeLiveDB(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"eligible plan round1", ""},
		{"round2", "UPDATE cross_checks SET round=2 WHERE lead_run_id=$1"},
		{"deadline", "UPDATE cross_checks SET deadline_at=now()-interval '1 second' WHERE lead_run_id=$1"},
		{"generation", "UPDATE runs SET claim_generation=3 WHERE id=$1"},
		{"released parent", "UPDATE runs SET claim_released_at=now() WHERE id=$1"},
		{"inactive parent", "UPDATE runs SET status='awaiting_approval' WHERE id=$1"},
		{"decided", "UPDATE cross_checks SET verdict='approve' WHERE lead_run_id=$1"},
		{"target ownership", ""},
		{"tenant ownership", "UPDATE runs SET user_id=$2 WHERE id=$1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, lead := memoryCheckerFixture(t)
			// Hold while eligible, then exercise the same boundary at reservation,
			// parking and owner Resume. Each mutation is independent.
			f.reserve(t, f.b)
			f.hold(t)
			apply := func() {
				switch tc.name {
				case "target ownership":
					other := seedOutageRun(t, f.e, f.w.UserID, f.repo, f.w.ID, "running", "issue", 2, 0)
					f.e.exec("UPDATE runs SET target_run_id=$2 WHERE id=$1", f.b.RunID, other)
				case "tenant ownership":
					f.e.exec(tc.sql, lead, f.e.seedUser(t))
				default:
					if tc.sql != "" {
						f.e.exec(tc.sql, lead)
					}
				}
			}
			apply()
			_, err := f.e.q.ResumeMemoryEpisode(f.e.ctx, store.ResumeMemoryEpisodeParams{
				ID: f.b.RunID, UserID: f.w.UserID, GlobalTimeoutSeconds: 86400})
			if tc.name == "eligible plan round1" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("ineligible checker Resume=%v", err)
			}
			// A separately active checker proves admission/hold eligibility, even
			// when a parent-exit trigger already terminated the previous checker.
			f, lead = memoryCheckerFixture(t)
			apply()
			_, err = f.s.ReserveMemoryIntervention(f.e.ctx, f.w,
				MemoryReservationRequest{MemoryBinding: f.b, Policy: MemoryPolicy{Version: 1, MaxInterventions: 3}})
			if tc.name == "eligible plan round1" {
				if err != nil {
					t.Fatal(err)
				}
				f.hold(t)
			} else {
				if !errors.Is(err, ErrMemoryStale) {
					t.Fatalf("ineligible checker admission=%v", err)
				}
				cause := recoveryCauseWorkerMemoryPressure
				_, applied, err := f.s.SetState(f.e.ctx, f.w, f.b.RunID, StateRequest{
					State: "recovery_wait", RecoveryCause: &cause, ClaimGeneration: &f.b.ClaimGeneration,
					MemoryEpisode: &f.b.MemoryEpisode, RegisterNonce: f.b.RegisterNonce})
				if applied || !errors.Is(err, ErrStaleClaim) {
					t.Fatalf("ineligible checker hold=%v err=%v", applied, err)
				}
				if f.run(t).MemoryInterventionCount != 0 {
					t.Fatal("ineligible checker charged")
				}
			}
		})
	}
	t.Run("unsupported stage rejected by schema", func(t *testing.T) {
		f, lead := memoryCheckerFixture(t)
		_, err := f.e.pool.Exec(f.e.ctx, "UPDATE cross_checks SET stage='implementation' WHERE lead_run_id=$1", lead)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("unsupported stage error=%v", err)
		}
	})
}
