package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func memoryRevisionReceipt(t *testing.T, f memoryFixture) int64 {
	t.Helper()
	f.e.exec("UPDATE runs SET status='awaiting_approval', gate_revision=7 WHERE id=$1", f.b.RunID)
	var id int64
	err := f.e.pool.QueryRow(f.e.ctx, `INSERT INTO run_user_inputs
 (run_id,kind,body,consumed_at,consumed_claim_generation,consumed_worker_id,gate_binding,gate_revision)
 VALUES ($1,'revise_plan','revise',now(),$2,$3,'bound',7) RETURNING id`,
		f.b.RunID, f.b.ClaimGeneration, f.w.ID).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func memoryRevisionParkRefused(t *testing.T, f memoryFixture) {
	t.Helper()
	cause := recoveryCauseWorkerMemoryPressure
	_, applied, err := f.s.SetState(f.e.ctx, f.w, f.b.RunID, StateRequest{
		State: "recovery_wait", RecoveryCause: &cause, ClaimGeneration: &f.b.ClaimGeneration,
		RegisterNonce: f.b.RegisterNonce, MemoryEpisode: &f.b.MemoryEpisode})
	if applied || !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("park applied=%v err=%v", applied, err)
	}
}

func TestMemoryRevisionEligibilityLiveDB(t *testing.T) {
	// Every case uses fresh owner-scoped data; the fixture cleans it before pool close.
	for _, tc := range []struct {
		name, mutation string
		eligible       bool
	}{
		{"bound", "", true},
		{"unbound", "UPDATE run_user_inputs SET gate_binding='unbound',gate_revision=NULL WHERE id=$1", true},
		{"legacy", "UPDATE run_user_inputs SET gate_binding=NULL,gate_revision=NULL WHERE id=$1", true},
		{"running", "UPDATE runs SET status='running' WHERE id=$1", true},
		{"auto root", "UPDATE runs SET status='running',gate_revision=0 WHERE id=$1", true},
		{"human wait", "DELETE FROM run_user_inputs WHERE id=$1", false},
		{"unconsumed", "UPDATE run_user_inputs SET consumed_at=NULL WHERE id=$1", false},
		{"wrong generation", "UPDATE run_user_inputs SET consumed_claim_generation=1 WHERE id=$1", false},
		{"wrong worker", "UPDATE run_user_inputs SET consumed_worker_id=$2 WHERE id=$1", false},
		{"wrong run", "UPDATE run_user_inputs SET run_id=$2 WHERE id=$1", false},
		{"wrong kind", "UPDATE run_user_inputs SET kind='approve_plan' WHERE id=$1", false},
		{"wrong status", "UPDATE runs SET status='awaiting_input' WHERE id=$1", false},
		{"applied", "UPDATE run_user_inputs SET applied_at=now() WHERE id=$1", false},
		{"disposed", "UPDATE run_user_inputs SET disposition='superseded' WHERE id=$1", false},
		{"old gate", "UPDATE runs SET gate_revision=8 WHERE id=$1", false},
		{"pause", "UPDATE runs SET pause_requested_at=now() WHERE id=$1", false},
		{"switch", "UPDATE runs SET credential_switch_requested_at=now() WHERE id=$1", false},
		{"released", "UPDATE runs SET claim_released_at=now() WHERE id=$1", false},
		{"nonce", "UPDATE workers SET snapshot_register_nonce='rotated' WHERE id=$1", false},
		{"episode", "UPDATE runs SET memory_episode=1 WHERE id=$1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMemoryFixture(t)
			id := memoryRevisionReceipt(t, f)
			if tc.name == "running" || tc.name == "auto root" {
				f.e.exec("DELETE FROM run_user_inputs WHERE id=$1", id)
			}
			otherWorker := seedSnapshotWorker(t, f.e, f.w.UserID, "other-memory-nonce")
			otherRun := seedOutageRun(t, f.e, f.w.UserID, f.repo, otherWorker, "running", "issue", 2, 2)
			// Use only parameters referenced by each literal mutation (pgx rejects extras).
			if tc.mutation != "" {
				switch tc.name {
				case "wrong worker":
					f.e.exec(tc.mutation, id, otherWorker)
				case "wrong run":
					f.e.exec(tc.mutation, id, otherRun)
				case "nonce":
					f.e.exec(tc.mutation, f.w.ID)
				default:
					if tc.name == "running" || tc.name == "auto root" || tc.name == "wrong status" || tc.name == "old gate" || tc.name == "pause" || tc.name == "switch" || tc.name == "released" || tc.name == "episode" {
						// Rebind the sole run parameter without unused positional placeholders.
						f.e.exec(tc.mutation, f.b.RunID)
					} else {
						f.e.exec(tc.mutation, id)
					}
				}
			}
			got, err := f.s.ReserveMemoryIntervention(f.e.ctx, f.w, MemoryReservationRequest{MemoryBinding: f.b, Policy: MemoryPolicy{Version: 1, MaxInterventions: 3}})
			if tc.eligible {
				if err != nil || !got.Admitted || !got.Authorizing || got.Allowance.Used != 1 {
					t.Fatalf("reservation=%+v err=%v", got, err)
				}
				f.hold(t)
			} else {
				if !errors.Is(err, ErrMemoryStale) {
					t.Fatalf("reservation=%+v err=%v", got, err)
				}
				memoryRevisionParkRefused(t, f)
				if r := f.run(t); r.MemoryInterventionCount != 0 || len(r.MemoryPolicy) != 0 {
					t.Fatalf("refusal spent allowance: %+v", r)
				}
			}
		})
	}
}

func TestMemoryRevisionProofInvalidationLiveDB(t *testing.T) {
	for _, mutation := range []string{
		"UPDATE runs SET gate_revision=8 WHERE id=$1",
		"UPDATE run_user_inputs SET applied_at=now() WHERE run_id=$1",
	} {
		t.Run(mutation, func(t *testing.T) {
			f := newMemoryFixture(t)
			memoryRevisionReceipt(t, f)
			f.reserve(t, f.b)
			f.e.exec(mutation, f.b.RunID)
			retry := f.reserve(t, f.b)
			if !retry.Admitted || retry.Authorizing || retry.Allowance.Used != 1 {
				t.Fatalf("duplicate=%+v", retry)
			}
			memoryRevisionParkRefused(t, f)
			outcome, err := f.s.RecordMemoryInterventionOutcome(f.e.ctx, f.w, MemoryOutcomeRequest{MemoryBinding: f.b, Outcome: "no_signal"})
			if err != nil || outcome.Authorizing || outcome.Outcome != "no_signal" || f.run(t).MemoryInterventionCount != 1 {
				t.Fatalf("historical outcome=%+v err=%v", outcome, err)
			}
		})
	}
}

func TestMemoryRevisionHoldPreservesApprovalWaitBudgetLiveDB(t *testing.T) {
	f := newMemoryFixture(t)
	memoryRevisionReceipt(t, f)
	f.e.exec("UPDATE runs SET started_at=now()-interval '6 hours',status_since=now()-interval '6 hours',budget_wall_seconds=3600,budget_paused_seconds=0 WHERE id=$1", f.b.RunID)
	f.reserve(t, f.b)
	held := f.hold(t)
	if held.BudgetPausedSeconds < 21600 || held.BudgetWallSeconds.Int32 != 3600 {
		t.Fatalf("approval wait consumed execution budget: paused=%d wall=%+v", held.BudgetPausedSeconds, held.BudgetWallSeconds)
	}
	cause := recoveryCauseWorkerMemoryPressure
	replayed, applied, err := f.s.SetState(f.e.ctx, f.w, f.b.RunID, StateRequest{
		State: "recovery_wait", RecoveryCause: &cause, ClaimGeneration: &f.b.ClaimGeneration,
		RegisterNonce: f.b.RegisterNonce, MemoryEpisode: &f.b.MemoryEpisode})
	if err != nil || applied || replayed.BudgetPausedSeconds != held.BudgetPausedSeconds {
		t.Fatalf("memory hold replay banked twice: applied=%v paused=%d err=%v", applied, replayed.BudgetPausedSeconds, err)
	}
	if _, err := f.e.q.ResumeMemoryEpisode(f.e.ctx, store.ResumeMemoryEpisodeParams{
		ID: f.b.RunID, UserID: f.w.UserID, GlobalTimeoutSeconds: 86400}); err != nil {
		t.Fatal(err)
	}
	resumed := f.run(t)
	if resumed.Status != "queued" || resumed.MemoryEpisode != 1 ||
		resumed.BudgetWallSeconds != held.BudgetWallSeconds || resumed.BudgetPausedSeconds < held.BudgetPausedSeconds {
		t.Fatalf("owner could not resume with preserved wall allowance: run=%+v", resumed)
	}
}

func TestMemoryRevisionHoldResumeLiveDB(t *testing.T) {
	f := newMemoryFixture(t)
	id := memoryRevisionReceipt(t, f)
	f.reserve(t, f.b)
	f.hold(t)
	if _, err := f.e.q.ResumeMemoryEpisode(f.e.ctx, store.ResumeMemoryEpisodeParams{ID: f.b.RunID, UserID: f.w.UserID, GlobalTimeoutSeconds: 86400}); err != nil {
		t.Fatal(err)
	}
	resumed := f.run(t)
	if resumed.MemoryEpisode != 1 || resumed.MemoryInterventionCount != 0 || len(resumed.MemoryPolicy) != 0 {
		t.Fatalf("resumed=%+v", resumed)
	}
	var applied bool
	if err := f.e.pool.QueryRow(f.e.ctx, "SELECT applied_at IS NOT NULL FROM run_user_inputs WHERE id=$1", id).Scan(&applied); err != nil || applied {
		t.Fatalf("receipt applied=%v err=%v", applied, err)
	}
	w, nonce, err := f.s.Register(f.e.ctx, f.w, "fixture", "", nil, nil, f.w.ProtocolCapabilities, nil)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := f.e.q.ClaimRun(f.e.ctx, claimRunParams(w))
	if err != nil {
		t.Fatal(err)
	}
	f.e.exec("UPDATE runs SET status='awaiting_approval' WHERE id=$1", fresh.ID)
	if f.reserve(t, f.b).Authorizing {
		t.Fatal("old binding regained authority")
	}
	memoryRevisionParkRefused(t, f)
	successor := f
	successor.w = w
	successor.b.RegisterNonce = nonce
	successor.b.ClaimGeneration = fresh.ClaimGeneration
	successor.b.MemoryEpisode = fresh.MemoryEpisode
	successor.b.InterventionID = uuid.New()
	_, err = successor.s.ReserveMemoryIntervention(f.e.ctx, w, MemoryReservationRequest{MemoryBinding: successor.b, Policy: MemoryPolicy{Version: 1, MaxInterventions: 3}})
	if !errors.Is(err, ErrMemoryStale) {
		t.Fatalf("old receipt authorized successor: %v", err)
	}
	memoryRevisionParkRefused(t, successor)
	// Re-ACK transfers the preserved unapplied row to the new claim.
	if _, err := f.e.q.AckRunInputRows(f.e.ctx, store.AckRunInputRowsParams{RunID: f.b.RunID, Ids: []int64{id}, ClaimGeneration: pgconv.Int8Ptr(&fresh.ClaimGeneration), WorkerID: pgconv.UUID(w.ID)}); err != nil {
		t.Fatal(err)
	}
	if !successor.reserve(t, successor.b).Authorizing {
		t.Fatal("re-ACK did not authorize successor")
	}
}
