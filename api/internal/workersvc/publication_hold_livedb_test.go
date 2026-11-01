package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #1324 D4: a worker-reported publication hold against a real Postgres. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

func (f *publicationParkFixture) hold1(gen int64, reason string) (store.Run, bool, error) {
	return f.svc.SetState(f.env.ctx, f.wkr, f.runID, pubHoldReport(gen, reason))
}

func TestPublicationWorkerHoldWritesOwnerHoldLiveDB(t *testing.T) {
	for _, reason := range publicationWorkerHoldReasons {
		t.Run(reason, func(t *testing.T) {
			f := newPublicationParkFixture(t, time.Hour)
			f.env.exec(`UPDATE runs SET forge_park_count=4 WHERE id=$1`, f.runID)
			run, applied, err := f.hold1(1, reason)
			if err != nil || !applied {
				t.Fatalf("hold applied=%v err=%v", applied, err)
			}
			if run.Status != "recovery_wait" || run.RecoveryWaitCause.String != "forge_unreachable" || run.RecoveryWaitPhase.String != "publication" ||
				run.RecoveryRetryNotBefore.Valid || run.ForgeParkCount != 4 || run.ClaimGeneration != 1 {
				t.Fatalf("returned run: %+v", run)
			}
			r := f.run(t)
			if r.Status != "recovery_wait" || r.RecoveryRetryNotBefore.Valid || r.RecoveryWaitPhase.String != "publication" ||
				r.ForgeParkCount != 4 || !r.WorkerID.Valid || r.ClaimReleasedAt.Valid {
				t.Fatalf("stored run: %+v", r)
			}
			s := f.side(t)
			if s.State != "active" || s.HoldReason.String != reason || !s.HoldReason.Valid || s.ParkTotal != 1 {
				t.Fatalf("side: %+v", s)
			}
			if st, ev := f.hold(t); st != "open" || ev.Valid {
				t.Fatalf("custody changed: %s/%v", st, ev)
			}
			var cooldowns int
			if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM forge_origin_cooldowns WHERE worker_id=$1`, f.workerID).Scan(&cooldowns); err != nil || cooldowns != 0 {
				t.Fatalf("a worker hold observed a forge origin: %d/%v", cooldowns, err)
			}
		})
	}
}

func TestPublicationWorkerHoldRefusalsLiveDB(t *testing.T) {
	t.Run("server-only reasons", func(t *testing.T) {
		f := newPublicationParkFixture(t, time.Hour)
		for _, reason := range []string{"publication_custodian_unavailable", "publication_retry_exhausted", "bogus"} {
			if _, applied, err := f.hold1(1, reason); !errors.Is(err, ErrInvalidState) || applied {
				t.Fatalf("%s: applied=%v err=%v", reason, applied, err)
			}
		}
		if r := f.run(t); r.Status != "running" || f.side(t).HoldReason.Valid {
			t.Fatalf("refused report mutated: %s %+v", r.Status, f.side(t))
		}
	})
	t.Run("no active intent", func(t *testing.T) {
		f := newPublicationParkFixture(t, time.Hour)
		plain := f.seedRun("running", 1)
		f.env.exec(`UPDATE runs SET worker_id=$2, started_at=now() WHERE id=$1`, plain, f.workerID)
		f.openHold(t, plain, 1)
		run, applied, err := f.svc.SetState(f.env.ctx, f.wkr, plain, pubHoldReport(1, "publication_model_required"))
		if !errors.Is(err, ErrPublicationNotEnrolled) || applied || run.Status != "running" {
			t.Fatalf("status=%s applied=%v err=%v", run.Status, applied, err)
		}
		var state string
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT state FROM recovery_custody_holds WHERE run_id=$1`, plain).Scan(&state); err != nil || state != "open" {
			t.Fatalf("custody %s/%v", state, err)
		}
	})
	t.Run("worker without the capability", func(t *testing.T) {
		f := newPublicationParkFixture(t, time.Hour)
		legacy := f.wkr
		legacy.ProtocolCapabilities = []string{capability.RecoveryArchiveV1}
		_, applied, err := f.svc.SetState(f.env.ctx, legacy, f.runID, pubHoldReport(1, "publication_model_required"))
		if !errors.Is(err, ErrInvalidState) || applied || f.run(t).Status != "running" {
			t.Fatalf("applied=%v err=%v status=%s", applied, err, f.run(t).Status)
		}
	})
	t.Run("stale generation", func(t *testing.T) {
		f := newPublicationParkFixture(t, time.Hour)
		run, applied, err := f.hold1(2, "publication_model_required")
		if !errors.Is(err, ErrForgeParkStaleClaim) || applied || run.Status != "running" {
			t.Fatalf("status=%s applied=%v err=%v", run.Status, applied, err)
		}
		if f.side(t).HoldReason.Valid || f.side(t).ParkTotal != 0 {
			t.Fatalf("stale report mutated: %+v", f.side(t))
		}
	})
}

func TestPublicationWorkerHoldDuplicateIsNoOpLiveDB(t *testing.T) {
	f := newPublicationParkFixture(t, time.Hour)
	if _, applied, err := f.hold1(1, "publication_source_unavailable"); err != nil || !applied {
		t.Fatalf("hold=%v/%v", applied, err)
	}
	before := f.side(t)
	f.clock = f.clock.Add(time.Minute)
	run, applied, err := f.hold1(1, "publication_source_unavailable")
	if err != nil || applied || run.Status != "recovery_wait" {
		t.Fatalf("duplicate status=%s applied=%v err=%v", run.Status, applied, err)
	}
	after := f.side(t)
	if after.ParkTotal != before.ParkTotal || after.HoldReason != before.HoldReason || f.run(t).RecoveryWaitCount != 1 {
		t.Fatalf("duplicate moved state: %+v -> %+v", before, after)
	}
}

func TestPublicationWorkerHoldHandoffRulesLiveDB(t *testing.T) {
	f := newPublicationParkFixture(t, time.Hour)
	f.env.exec(`UPDATE runs SET checkpoint_tip=$2 WHERE id=$1`, f.runID, pubHeadC)
	req := pubHoldReport(1, "publication_model_required")
	req.HandoffCheckpointTip = strPtr(pubHeadC)
	if _, applied, err := f.svc.SetState(f.env.ctx, f.wkr, f.runID, req); err != nil || !applied {
		t.Fatalf("hold=%v/%v", applied, err)
	}
	if h := f.side(t).Handoff; string(h) == "" {
		t.Fatal("a matching published tip recorded no handoff")
	}
}

func TestPublicationWorkerHoldIsInertToPromotersAndResumeLiveDB(t *testing.T) {
	f := newPublicationParkFixture(t, time.Hour)
	ctx := f.env.ctx
	held, applied, err := f.hold1(1, "publication_intent_mismatch")
	if err != nil || !applied {
		t.Fatalf("hold=%v/%v", applied, err)
	}
	if _, err := f.env.q.PromoteRecoveryWaitRuns(ctx, pgtype.Timestamptz{Time: f.clock.Add(1000 * time.Hour), Valid: true}); err != nil {
		t.Fatal(err)
	}
	if n, err := f.env.q.PromoteRecoveryWaitRunNow(ctx, store.PromoteRecoveryWaitRunNowParams{ID: f.runID, UserID: f.userID}); err != nil || n != 0 {
		t.Fatalf("early promote=%d/%v", n, err)
	}
	if r := f.run(t); r.Status != "recovery_wait" || r.RecoveryRetryNotBefore.Valid {
		t.Fatalf("a promoter moved the hold: %s", r.Status)
	}
	before := f.side(t)
	ok, err := f.svc.ResumePublicationForgeHold(ctx, f.userID, held)
	if err != nil || !ok {
		t.Fatalf("resume=%v/%v", ok, err)
	}
	r := f.run(t)
	if r.Status != "queued" || r.RecoveryWaitPhase.Valid || r.ForgeParkCount != 0 || r.ClaimGeneration != 1 {
		t.Fatalf("resumed run: %+v", r)
	}
	s := f.side(t)
	if s.HoldReason.Valid || s.Episode != before.Episode+1 || s.WaitStartedAt.Valid || s.State != "active" {
		t.Fatalf("side after resume: %+v", s)
	}
	if st, ev := f.hold(t); st != "open" || ev.Valid {
		t.Fatalf("resume touched custody: %s/%v", st, ev)
	}
}
