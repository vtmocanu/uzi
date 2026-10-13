package workersvc

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func memoryCodeFixture(t *testing.T, expired bool) (memoryFixture, uuid.UUID) {
	t.Helper()
	f := newMemoryFixture(t)
	lead := seedOutageRun(t, f.e, f.w.UserID, f.repo, f.w.ID, "running", "issue", 2, 0)
	f.e.exec("UPDATE runs SET code_cross_check_required=true WHERE id=$1", lead)
	f.e.exec("UPDATE runs SET kind='cross_check',harness='codex',issue_iid=NULL,target_run_id=$2,report_only=true WHERE id=$1", f.b.RunID, lead)
	// Deadline and code evidence are immutable; choose expiry at insertion.
	f.e.exec(`INSERT INTO cross_checks
 (lead_run_id,stage,round,lead_claim_generation,verdict,outcome,base_commit,head_commit,candidate_digest,
  checker_run_id,checker_harness,deadline_at,code_context,guidance_snapshot,guidance_text,guidance_digest,
  repo_instructions_text,repo_instructions_digest)
 VALUES ($1,'code',1,2,'failed','pending',$2,$3,$4,$5,'codex',
  CASE WHEN $6::boolean THEN now()-interval '1 minute' ELSE now()+interval '30 minutes' END,
  '{}','', '',sha256(convert_to('', 'UTF8')), '',sha256(convert_to('', 'UTF8')))`,
		lead, strings.Repeat("a", 40), strings.Repeat("b", 40), []byte("memory-code-candidate"), f.b.RunID, expired)
	return f, lead
}

func TestMemoryCodeCheckerAuthorityLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name              string
		eligible, expired bool
		mutate            func(memoryFixture, uuid.UUID)
	}{
		{"running parent", true, false, nil},
		{"interactive task parent", true, false, func(f memoryFixture, lead uuid.UUID) {
			f.e.exec("UPDATE runs SET kind='task',issue_iid=NULL,branch='uzi/task/'||id::text,interactive=true,status='awaiting_followup' WHERE id=$1", lead)
		}},
		{"stale generation", false, false, func(f memoryFixture, lead uuid.UUID) {
			f.e.exec("UPDATE runs SET claim_generation=3 WHERE id=$1", lead)
		}},
		{"released parent", false, false, func(f memoryFixture, lead uuid.UUID) {
			f.e.exec("UPDATE runs SET claim_released_at=now() WHERE id=$1", lead)
		}},
		{"interrupted pending check", false, false, func(f memoryFixture, lead uuid.UUID) {
			f.e.exec("UPDATE cross_checks SET interrupted_at=now() WHERE lead_run_id=$1", lead)
		}},
		{"decided code check", false, false, func(f memoryFixture, lead uuid.UUID) {
			f.e.exec("UPDATE cross_checks SET outcome='completed',decided_at=now() WHERE lead_run_id=$1", lead)
		}},
		{"deadline", false, true, nil},
		{"unsupported waiting issue", false, false, func(f memoryFixture, lead uuid.UUID) {
			f.e.exec("UPDATE runs SET status='awaiting_followup' WHERE id=$1", lead)
		}},
		{"noninteractive waiting task", false, false, func(f memoryFixture, lead uuid.UUID) {
			f.e.exec("UPDATE runs SET kind='task',issue_iid=NULL,branch='uzi/task/'||id::text,interactive=false,status='awaiting_followup' WHERE id=$1", lead)
		}},
		{"target mismatch", false, false, func(f memoryFixture, lead uuid.UUID) {
			other := seedOutageRun(t, f.e, f.w.UserID, f.repo, f.w.ID, "running", "issue", 2, 0)
			f.e.exec("UPDATE runs SET target_run_id=$2 WHERE id=$1", f.b.RunID, other)
		}},
		{"foreign parent", false, false, func(f memoryFixture, lead uuid.UUID) {
			f.e.exec("UPDATE runs SET user_id=$2 WHERE id=$1", lead, f.e.seedUser(t))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("admission", func(t *testing.T) {
				f, lead := memoryCodeFixture(t, tc.expired)
				if tc.mutate != nil {
					tc.mutate(f, lead)
				}
				got, err := f.s.ReserveMemoryIntervention(f.e.ctx, f.w, MemoryReservationRequest{
					MemoryBinding: f.b, Policy: MemoryPolicy{Version: 1, MaxInterventions: 3}})
				if tc.eligible {
					if err != nil || !got.Admitted || !got.Authorizing || got.Allowance.Used != 1 {
						t.Fatalf("code admission=%+v err=%v", got, err)
					}
					f.hold(t)
				} else {
					if !errors.Is(err, ErrMemoryStale) {
						t.Fatalf("ineligible code admission=%+v err=%v", got, err)
					}
					if f.run(t).MemoryInterventionCount != 0 {
						t.Fatal("stale code checker charged")
					}
					cause := recoveryCauseWorkerMemoryPressure
					_, applied, err := f.s.SetState(f.e.ctx, f.w, f.b.RunID, StateRequest{
						State: "recovery_wait", RecoveryCause: &cause, ClaimGeneration: &f.b.ClaimGeneration,
						MemoryEpisode: &f.b.MemoryEpisode, RegisterNonce: f.b.RegisterNonce})
					if applied || !errors.Is(err, ErrStaleClaim) {
						t.Fatalf("ineligible code hold=%v err=%v", applied, err)
					}
				}
			})
			if !tc.expired {
				t.Run("historical replay", func(t *testing.T) {
					f, lead := memoryCodeFixture(t, false)
					f.reserve(t, f.b)
					if tc.mutate != nil {
						tc.mutate(f, lead)
					}
					got, err := f.s.ReserveMemoryIntervention(f.e.ctx, f.w, MemoryReservationRequest{
						MemoryBinding: f.b, Policy: MemoryPolicy{Version: 1, MaxInterventions: 3}})
					if err != nil || !got.Admitted || got.Authorizing != tc.eligible || got.Allowance.Used != 1 ||
						f.run(t).MemoryInterventionCount != 1 {
						t.Fatalf("code replay=%+v err=%v", got, err)
					}
				})
			}
			t.Run("resume", func(t *testing.T) {
				f, lead := memoryCodeFixture(t, tc.expired)
				// Seed an existing owner hold independently of admission so the resume guard
				// is exercised even for an already expired immutable check snapshot.
				f.e.exec(`UPDATE runs SET status='recovery_wait',recovery_wait_cause='worker_memory_pressure',
     claim_released_at=now(),released_worker_id=worker_id,released_worker_nonce='memory-nonce',
     memory_policy='{"version":1,"max_interventions":3}',memory_intervention_count=3 WHERE id=$1`, f.b.RunID)
				if tc.mutate != nil {
					tc.mutate(f, lead)
				}
				before := f.run(t)
				_, err := f.e.q.ResumeMemoryEpisode(f.e.ctx, store.ResumeMemoryEpisodeParams{
					ID: f.b.RunID, UserID: f.w.UserID, GlobalTimeoutSeconds: 86400})
				after := f.run(t)
				if tc.eligible {
					if err != nil || after.Status != "queued" || after.MemoryEpisode != 1 ||
						after.MemoryInterventionCount != 0 || after.MemoryPolicy != nil ||
						after.WorkerRecoveryEpisode != before.WorkerRecoveryEpisode || after.RequeueCount != before.RequeueCount ||
						after.ReleasedWorkerID != pgconv.UUID(f.w.ID) || after.ReleasedWorkerNonce.String != "memory-nonce" {
						t.Fatalf("code resume=%+v err=%v", after, err)
					}
				} else if !errors.Is(err, pgx.ErrNoRows) || !reflect.DeepEqual(before, after) {
					t.Fatalf("ineligible code resume changed row: before=%+v after=%+v err=%v", before, after, err)
				}
			})
		})
	}
}
