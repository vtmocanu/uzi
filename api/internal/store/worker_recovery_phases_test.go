package store_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Exercise generated public methods; main regenerates these types before the
// live-DB run. Every caller passes the captured server ledger and actual locks.
func frozenRecoveryPhase(ctx context.Context, q *store.Queries, phase string, worker uuid.UUID, ids []uuid.UUID, frozen []byte, parents []uuid.UUID) ([]uuid.UUID, error) {
	switch phase {
	case "readopt":
		rows, err := q.FrozenReadoptRunsFromSnapshot(ctx, store.FrozenReadoptRunsFromSnapshotParams{
			WorkerID: worker, FrozenTargets: frozen, LockedParentIds: parents,
		})
		var result []uuid.UUID
		for _, row := range rows {
			result = append(result, row.ID)
		}
		return result, err
	case "missing fail":
		rows, err := q.FrozenFailRunsMissingFromSnapshot(ctx, store.FrozenFailRunsMissingFromSnapshotParams{
			WorkerID: pgU(worker), MaxRequeues: 5, FailureReason: pgT("lost"),
			MissingCutoff: planCrossCheckTime(time.Now().Add(-time.Minute)),
			Now:           planCrossCheckTime(time.Now()), GlobalTimeoutSeconds: 3600,
			FrozenTargets: frozen, LockedParentIds: parents,
		})
		var result []uuid.UUID
		for _, row := range rows {
			result = append(result, row.ID)
		}
		return result, err
	case "missing requeue":
		rows, err := q.FrozenRequeueRunsMissingFromSnapshot(ctx, store.FrozenRequeueRunsMissingFromSnapshotParams{
			WorkerID: pgU(worker), MaxRequeues: 5,
			MissingCutoff: planCrossCheckTime(time.Now().Add(-time.Minute)),
			Now:           planCrossCheckTime(time.Now()), GlobalTimeoutSeconds: 3600,
			FrozenTargets: frozen, LockedParentIds: parents,
		})
		var result []uuid.UUID
		for _, row := range rows {
			result = append(result, row.ID)
		}
		return result, err
	case "worker fail":
		rows, err := q.FrozenFailWorkerRunsOverCap(ctx, store.FrozenFailWorkerRunsOverCapParams{
			WorkerID: pgU(worker), MaxRequeues: 5, FailureReason: pgT("lost"),
			FrozenTargets: frozen, LockedParentIds: parents,
		})
		var result []uuid.UUID
		for _, row := range rows {
			result = append(result, row.ID)
		}
		return result, err
	case "worker requeue":
		return q.FrozenRequeueWorkerRuns(ctx, store.FrozenRequeueWorkerRunsParams{
			WorkerID: pgU(worker), MaxRequeues: 5, FrozenTargets: frozen, LockedParentIds: parents,
		})
	case "attested fail":
		generations := make([]int64, len(ids))
		for i := range generations {
			generations[i] = 1
		}
		rows, err := q.FrozenFailAttestedFinalizeRunsOverCap(ctx, store.FrozenFailAttestedFinalizeRunsOverCapParams{
			WorkerID: pgU(worker), MaxRequeues: 5, FailureReason: pgT("lost"),
			RunIds: ids, ClaimGenerations: generations, FrozenTargets: frozen, LockedParentIds: parents,
		})
		var result []uuid.UUID
		for _, row := range rows {
			result = append(result, row.ID)
		}
		return result, err
	case "attested requeue":
		generations := make([]int64, len(ids))
		for i := range generations {
			generations[i] = 1
		}
		rows, err := q.FrozenRequeueAttestedFinalizeRuns(ctx, store.FrozenRequeueAttestedFinalizeRunsParams{
			WorkerID: pgU(worker), MaxRequeues: 5, RunIds: ids, ClaimGenerations: generations,
			FrozenTargets: frozen, LockedParentIds: parents,
		})
		var result []uuid.UUID
		for _, row := range rows {
			result = append(result, row.ID)
		}
		return result, err
	default:
		panic("unknown recovery phase")
	}
}

func prepareFrozenRecoveryChild(ctx context.Context, t *testing.T, fx *planCrossCheckDeletionFixture, phase string) uuid.UUID {
	t.Helper()
	f := fx.f
	worker := uuid.New()
	mustExec(ctx, t, f.pool, `INSERT INTO workers(id,user_id,name,token_hash)
		VALUES($1,$2,'recovery child',$3)`, worker, f.userID, "hash-"+worker.String())
	count := 0
	if phase == "worker fail" || phase == "missing fail" || phase == "attested fail" {
		count = 5
	}
	status := "running"
	if phase == "readopt" {
		status = "queued"
	}
	mustExec(ctx, t, f.pool, `UPDATE runs SET worker_id=$2,status=$3,claim_generation=1,
		claim_released_at=NULL,requeue_count=$4,status_since=now()-interval '2 minutes',
		started_at=now(),stale_requeue_generation=1,
		finalize_resume_generation=CASE WHEN $5 THEN 1 ELSE NULL END WHERE id=$1`,
		fx.checkerID, worker, status, count, phase == "attested fail")
	if phase == "readopt" {
		mustExec(ctx, t, f.pool, `INSERT INTO worker_active_runs(worker_id,run_id,claim_generation,phase,terminal_pending,snapshot_epoch,reported_at)
			VALUES($1,$2,1,'running',false,1,now())`, worker, fx.checkerID)
	}
	return worker
}

func TestFrozenRecoveryPhasesHealthyLiveDB(t *testing.T) {
	for _, phase := range []string{"readopt", "missing fail", "missing requeue", "worker fail", "worker requeue", "attested fail", "attested requeue"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			fx := setupPlanCrossCheckDeletion(ctx, t)
			worker := prepareFrozenRecoveryChild(ctx, t, fx, phase)
			tx, err := fx.f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackWorkerRecoveryTx(t, tx)
			frozen, parents := freezeWorkerSnapshot(ctx, t, tx, worker, []uuid.UUID{fx.checkerID})
			rows, err := frozenRecoveryPhase(ctx, store.New(tx), phase, worker, []uuid.UUID{fx.checkerID}, frozen, parents)
			if err != nil || !reflect.DeepEqual(rows, []uuid.UUID{fx.checkerID}) {
				t.Fatalf("healthy %s: rows=%v err=%v", phase, rows, err)
			}
			want := "queued"
			switch phase {
			case "readopt":
				want = "running"
			case "missing fail", "worker fail", "attested fail":
				want = "failed"
			}
			var status, verdict, leadStatus string
			var credit int32
			if err := tx.QueryRow(ctx, `SELECT child.status,cc.verdict,lead.status,lead.budget_paused_seconds
				FROM runs child JOIN cross_checks cc ON cc.checker_run_id=child.id
				JOIN runs lead ON lead.id=cc.lead_run_id WHERE child.id=$1`, fx.checkerID).
				Scan(&status, &verdict, &leadStatus, &credit); err != nil {
				t.Fatal(err)
			}
			if status != want || verdict != "pending" || leadStatus != "running" || credit != 0 {
				t.Fatalf("child=%s want=%s verdict=%s lead=%s bank=%d", status, want, verdict, leadStatus, credit)
			}
		})
	}
}

func TestFrozenRecoveryPhasesIdentityAndHeldParentLiveDB(t *testing.T) {
	for _, drift := range []string{"generation", "ownership", "target", "disappearance", "replacement held", "replacement outside", "appearance"} {
		t.Run(drift, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			fx := setupPlanCrossCheckDeletion(ctx, t)
			f := fx.f
			worker := prepareFrozenRecoveryChild(ctx, t, fx, "worker fail")
			newParent := uuid.New()
			mustExec(ctx, t, f.pool, `INSERT INTO runs(id,user_id,repo_id,kind,status,issue_title,issue_description,harness)
				VALUES($1,$2,$3,'prompt','running','other parent','other parent','claude')`, newParent, f.userID, f.repoID)
			if drift == "appearance" {
				mustExec(ctx, t, f.pool, "DELETE FROM cross_checks WHERE id=$1", fx.crossCheckID)
			}
			tx, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackWorkerRecoveryTx(t, tx)
			snapshotIDs := []uuid.UUID{fx.checkerID}
			if drift == "replacement held" {
				snapshotIDs = append(snapshotIDs, newParent)
				// A second owned target makes this a genuinely already-held parent.
				mustExec(ctx, t, f.pool, "UPDATE runs SET worker_id=$2 WHERE id=$1", newParent, worker)
			}
			frozen, parents := freezeWorkerSnapshot(ctx, t, tx, worker, snapshotIDs)
			switch drift {
			case "generation":
				execFrozenPhase(ctx, t, tx, "UPDATE runs SET claim_generation=2 WHERE id=$1", fx.checkerID)
			case "ownership":
				execFrozenPhase(ctx, t, tx, "UPDATE runs SET worker_id=$2 WHERE id=$1", fx.checkerID, f.workerID)
			case "target":
				execFrozenPhase(ctx, t, tx, "UPDATE runs SET target_run_id=$2 WHERE id=$1", fx.checkerID, newParent)
			case "disappearance":
				execFrozenPhase(ctx, t, tx, "DELETE FROM cross_checks WHERE id=$1", fx.crossCheckID)
			case "replacement held":
				execFrozenPhase(ctx, t, tx, "UPDATE cross_checks SET lead_run_id=$2 WHERE id=$1", fx.crossCheckID, newParent)
			case "replacement outside":
				mustExec(ctx, t, f.pool, "UPDATE cross_checks SET lead_run_id=$2 WHERE id=$1", fx.crossCheckID, newParent)
			case "appearance":
				execFrozenPhase(ctx, t, tx, `INSERT INTO cross_checks(id,lead_run_id,checker_run_id,stage,round,lead_claim_generation,
					plan_md,milestones,size_class,base_commit,candidate_digest,checker_harness,deadline_at)
					VALUES($1,$2,$3,'plan',1,1,'plan','[]','s',repeat('a',40),$4,'codex',now()+interval '5 minutes')`,
					fx.crossCheckID, f.runID, fx.checkerID, []byte("new-digest"))
			}
			if drift == "replacement outside" {
				holder, err := f.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer rollbackWorkerRecoveryTx(t, holder)
				execFrozenPhase(ctx, t, holder, "SELECT id FROM runs WHERE id=$1 FOR UPDATE", newParent)
			}
			execFrozenPhase(ctx, t, tx, "SET LOCAL lock_timeout='500ms'")
			rows, err := frozenRecoveryPhase(ctx, store.New(tx), "worker fail", worker, []uuid.UUID{fx.checkerID}, frozen, parents)
			if err != nil || len(rows) != 0 {
				t.Fatalf("drift %s: rows=%v err=%v", drift, rows, err)
			}
			var status string
			if err := tx.QueryRow(ctx, "SELECT status FROM runs WHERE id=$1", fx.checkerID).Scan(&status); err != nil || status != "running" {
				t.Fatalf("drift child status=%s err=%v", status, err)
			}
		})
	}
}

// All terminal families share the same final parent-exit ownership rule. The
// parked/released cases create the check AFTER custody ended, so setup itself
// cannot settle it through the lead-exit trigger.
func TestRecoveryTerminalParentOwnershipLiveDB(t *testing.T) {
	for _, path := range []string{"worker", "stale", "missing", "attested", "frozen worker", "frozen missing", "frozen attested"} {
		states := map[string][]string{
			"worker": {"approve"}, "stale": {"failed"}, "missing": {"parent fence"}, "attested": {"pending"},
			"frozen worker":  {"pending", "approve", "failed", "parked", "released", "parent fence", "frozen parent fence"},
			"frozen missing": {"pending"}, "frozen attested": {"parent fence"},
		}[path]
		for _, state := range states {
			t.Run(path+"/"+state, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				fx := setupPlanCrossCheckDeletion(ctx, t)
				f := fx.f
				mustExec(ctx, t, f.pool, "DELETE FROM cross_checks WHERE id=$1", fx.crossCheckID)
				mustExec(ctx, t, f.pool, `UPDATE runs SET status='running',worker_id=$3,claim_generation=1,
					claim_released_at=NULL,requeue_count=5,finalize_resume_generation=1,
					status_since=now()-interval '2 minutes',started_at=now(),budget_wall_seconds=3600
					WHERE id IN($1,$2)`, f.runID, fx.checkerID, f.workerID)
				if state == "parked" {
					mustExec(ctx, t, f.pool, "UPDATE runs SET status='awaiting_approval' WHERE id=$1", f.runID)
				}
				if state == "released" {
					mustExec(ctx, t, f.pool, "UPDATE runs SET claim_released_at=now() WHERE id=$1", f.runID)
				}
				mustExec(ctx, t, f.pool, `INSERT INTO cross_checks(id,lead_run_id,checker_run_id,stage,round,lead_claim_generation,
					plan_md,milestones,size_class,base_commit,candidate_digest,checker_harness,deadline_at)
					VALUES($1,$2,$3,'plan',1,1,'plan','[]','s',repeat('a',40),$4,'codex',now()+interval '5 minutes')`,
					fx.crossCheckID, f.runID, fx.checkerID, []byte("terminal-digest"))
				if state == "approve" || state == "failed" {
					mustExec(ctx, t, f.pool, "UPDATE cross_checks SET verdict=$2,reason_class=CASE WHEN $2='failed' THEN 'model_error' ELSE $2 END,decided_at=now() WHERE id=$1",
						fx.crossCheckID, state)
				}
				mustExec(ctx, t, f.pool, `UPDATE cross_checks SET created_at='2020-01-01 00:00:00+00',
					deadline_at='2020-01-01 00:00:17.25+00' WHERE id=$1`, fx.crossCheckID)
				mustExec(ctx, t, f.pool, "UPDATE runs SET budget_paused_seconds=7 WHERE id=$1", f.runID)
				mustExec(ctx, t, f.pool, "UPDATE workers SET last_heartbeat_at=now()-interval '10 minutes' WHERE id=$1", f.workerID)
				tx, err := f.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer rollbackWorkerRecoveryTx(t, tx)
				ids := []uuid.UUID{f.runID, fx.checkerID}
				frozen, parents := freezeWorkerSnapshot(ctx, t, tx, f.workerID, ids)
				if state == "parent fence" {
					// Keep the independently eligible child, rejecting only its lead.
					// An exact-generation lease rejects the lead in every fail family.
					execFrozenPhase(ctx, t, tx, `INSERT INTO worker_active_runs(worker_id,run_id,claim_generation,phase,
						terminal_pending,terminal_pending_until,snapshot_epoch,reported_at)
						VALUES($1,$2,1,'running',true,now()+interval '1 minute',1,now())`, f.workerID, f.runID)
				}
				if state == "frozen parent fence" {
					// The prelock selected this lead, but its exact frozen kind no
					// longer matches. The unchanged child must fail independently.
					execFrozenPhase(ctx, t, tx, "UPDATE runs SET kind='prompt',issue_iid=NULL WHERE id=$1", f.runID)
				}
				q := store.New(tx)
				run := func() error {
					switch path {
					case "worker":
						_, err := q.FailWorkerRunsOverCap(ctx, store.FailWorkerRunsOverCapParams{WorkerID: pgU(f.workerID), MaxRequeues: 5, FailureReason: pgT("lost")})
						return err
					case "stale":
						_, err := q.FailRunsOfStaleWorkersOverCap(ctx, store.FailRunsOfStaleWorkersOverCapParams{
							MaxRequeues: 5, FailureReason: pgT("lost"), FailCutoff: planCrossCheckTime(time.Now().Add(-time.Minute)),
						})
						return err
					case "missing":
						_, err := q.FailRunsMissingFromSnapshot(ctx, store.FailRunsMissingFromSnapshotParams{
							WorkerID: pgU(f.workerID), MaxRequeues: 5, FailureReason: pgT("lost"),
							MissingCutoff: planCrossCheckTime(time.Now().Add(-time.Minute)),
							Now:           planCrossCheckTime(time.Now()), GlobalTimeoutSeconds: 3600,
						})
						return err
					case "attested":
						_, err := q.FailAttestedFinalizeRunsOverCap(ctx, store.FailAttestedFinalizeRunsOverCapParams{
							WorkerID: pgU(f.workerID), MaxRequeues: 5, FailureReason: pgT("lost"), RunIds: ids, ClaimGenerations: []int64{1, 1},
						})
						return err
					default:
						phase := map[string]string{"frozen worker": "worker fail", "frozen missing": "missing fail", "frozen attested": "attested fail"}[path]
						_, err := frozenRecoveryPhase(ctx, q, phase, f.workerID, ids, frozen, parents)
						return err
					}
				}
				if err := run(); err != nil {
					t.Fatal(err)
				}
				var child, verdict, leadStatus string
				var bank int32
				read := func() {
					t.Helper()
					if err := tx.QueryRow(ctx, `SELECT child.status,cc.verdict,lead.status,lead.budget_paused_seconds
						FROM cross_checks cc JOIN runs child ON child.id=cc.checker_run_id
						JOIN runs lead ON lead.id=cc.lead_run_id WHERE cc.id=$1`, fx.crossCheckID).
						Scan(&child, &verdict, &leadStatus, &bank); err != nil {
						t.Fatal(err)
					}
				}
				read()
				wantChild, wantVerdict, wantBank := "failed", "pending", int32(7)
				wantLeadStatus := "failed"
				switch state {
				case "pending":
					wantChild, wantVerdict, wantBank = "cancelled", "failed", 25
				case "approve", "failed":
					wantVerdict = state
				case "released", "parent fence", "frozen parent fence":
					wantLeadStatus = "running"
				}
				if child != wantChild || verdict != wantVerdict || leadStatus != wantLeadStatus || bank != wantBank {
					t.Fatalf("child=%s verdict=%s lead=%s bank=%d want=%s/%s/%s/%d", child, verdict, leadStatus, bank, wantChild, wantVerdict, wantLeadStatus, wantBank)
				}
				if err := run(); err != nil {
					t.Fatal(err)
				}
				read()
				if child != wantChild || verdict != wantVerdict || leadStatus != wantLeadStatus || bank != wantBank {
					t.Fatalf("repeat child=%s verdict=%s lead=%s bank=%d", child, verdict, leadStatus, bank)
				}
				if err := tx.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
				var original string
				var originalBank int32
				if err := f.pool.QueryRow(ctx, "SELECT status,budget_paused_seconds FROM runs WHERE id=$1", fx.checkerID).
					Scan(&original, &originalBank); err != nil || original != "running" || originalBank != 0 {
					t.Fatalf("rollback child=%s bank=%d err=%v", original, originalBank, err)
				}
				wantOriginalLeadStatus := "running"
				if state == "parked" {
					wantOriginalLeadStatus = "awaiting_approval"
				}
				if err := f.pool.QueryRow(ctx, "SELECT status,budget_paused_seconds FROM runs WHERE id=$1", f.runID).
					Scan(&original, &originalBank); err != nil || original != wantOriginalLeadStatus || originalBank != 7 {
					t.Fatalf("rollback lead=%s bank=%d want=%s/7 err=%v", original, originalBank, wantOriginalLeadStatus, err)
				}
			})
		}
	}
}

func execFrozenPhase(ctx context.Context, t *testing.T, tx pgx.Tx, query string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

// Small discriminating cases retain the original lease/overflow/allowance
// boundaries, and distinguish terminal orphan cleanup from re-admission.
func TestFrozenRecoveryPhaseGuardsLiveDB(t *testing.T) {
	cases := []struct {
		name, phase string
		want        bool
	}{
		{"lease", "worker fail", false},
		{"overflow", "worker fail", false},
		{"stale lease", "worker fail", true},
		{"orphan fail", "worker fail", true},
		{"orphan requeue", "worker requeue", false},
		{"orphan readopt", "readopt", false},
		{"allowance", "attested requeue", true},
		{"spent allowance", "attested requeue", false},
		{"attested overflow", "attested fail", true},
		{"expired check", "missing requeue", false},
		{"stale lead", "readopt", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			fx := setupPlanCrossCheckDeletion(ctx, t)
			f := fx.f
			worker := prepareFrozenRecoveryChild(ctx, t, fx, tc.phase)
			if tc.name == "orphan fail" || tc.name == "orphan requeue" || tc.name == "orphan readopt" {
				mustExec(ctx, t, f.pool, "DELETE FROM cross_checks WHERE id=$1", fx.crossCheckID)
			}
			tx, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackWorkerRecoveryTx(t, tx)
			frozen, parents := freezeWorkerSnapshot(ctx, t, tx, worker, []uuid.UUID{fx.checkerID})
			switch tc.name {
			case "lease", "stale lease":
				generation := int64(1)
				if tc.name == "stale lease" {
					generation = 0
				}
				execFrozenPhase(ctx, t, tx, `INSERT INTO worker_active_runs(worker_id,run_id,claim_generation,phase,
					terminal_pending,terminal_pending_until,snapshot_epoch,reported_at)
					VALUES($1,$2,$3,'running',true,now()+interval '1 minute',1,now())`, worker, fx.checkerID, generation)
			case "overflow", "attested overflow":
				execFrozenPhase(ctx, t, tx, "UPDATE workers SET pending_overflow_until=now()+interval '1 minute' WHERE id=$1", worker)
			case "allowance", "spent allowance":
				execFrozenPhase(ctx, t, tx, "UPDATE runs SET requeue_count=5,finalize_resume_generation=CASE WHEN $2 THEN 1 ELSE NULL END WHERE id=$1",
					fx.checkerID, tc.name == "spent allowance")
			case "expired check":
				execFrozenPhase(ctx, t, tx, "UPDATE cross_checks SET deadline_at=now()-interval '1 second' WHERE id=$1", fx.crossCheckID)
			case "stale lead":
				execFrozenPhase(ctx, t, tx, "UPDATE cross_checks SET lead_claim_generation=0 WHERE id=$1", fx.crossCheckID)
			}
			rows, err := frozenRecoveryPhase(ctx, store.New(tx), tc.phase, worker, []uuid.UUID{fx.checkerID}, frozen, parents)
			if err != nil || (len(rows) == 1) != tc.want || len(rows) > 1 {
				t.Fatalf("guard %s: rows=%v want mutation=%t err=%v", tc.name, rows, tc.want, err)
			}
			if tc.name == "allowance" {
				var charged bool
				if err := tx.QueryRow(ctx, "SELECT requeue_count=6 AND finalize_resume_generation=1 AND status='queued' FROM runs WHERE id=$1",
					fx.checkerID).Scan(&charged); err != nil || !charged {
					t.Fatalf("one-shot allowance charged=%t err=%v", charged, err)
				}
			}
		})
	}
}
