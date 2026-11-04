package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #3859, fake-store half: the sweep wiring, the registration evidence ACK and its
// validation. The SQL itself is proved by claimed_never_started_charge_livedb_test.go.

func TestSweepChargesClaimedNeverStartedContinuations(t *testing.T) {
	charged, exhausted, unwitnessed := uuid.New(), uuid.New(), uuid.New()
	fs := &fakeStore{
		chargedClaimed: []store.ChargeClaimedNeverStartedContinuationsRow{
			{ID: charged, UserID: uuid.New(), Status: "queued"},
			{ID: exhausted, UserID: uuid.New(), Status: "recovery_wait"},
		},
		unwitnessedClaimed: []store.ParkUnwitnessedContinuationsRow{{ID: unwitnessed, UserID: uuid.New(), Status: "recovery_wait"}},
	}
	svc := New(fs, newBox(t), testParams())
	spy := &stateSpy{}
	svc.SetBroadcaster(spy)
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }

	res, err := svc.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.ClaimedCharged != 1 || res.ClaimedExhausted != 1 || res.ClaimedUnwitnessed != 1 || res.ClaimedReset != 0 {
		t.Fatalf("result = %+v, want one charge, one exhaustion, one unwitnessed park and no uncharged reset", res)
	}
	if fs.cohortArg == nil || int(fs.cohortArg.MaxRequeues) != testParams().RunMaxRequeues {
		t.Fatalf("cohort args = %+v, want MaxRequeues %d", fs.cohortArg, testParams().RunMaxRequeues)
	}
	// One cohort, one grace cutoff; the witness window is twice the grace, no new knob.
	grace := testParams().ClaimGrace
	if !fs.cohortArg.Cutoff.Time.Equal(now.Add(-grace)) || !fs.cohortArg.WitnessCutoff.Time.Equal(now.Add(-2*grace)) ||
		fs.cohortArg.WitnessWindowSeconds != (2*grace).Seconds() {
		t.Fatalf("cohort cutoffs = %+v, want grace %v and a 2x witness window", fs.cohortArg, grace)
	}
	for id, want := range map[uuid.UUID]string{charged: "queued", exhausted: "recovery_wait", unwitnessed: "recovery_wait"} {
		if got, ok := spy.statusFor(id); !ok || got != want {
			t.Fatalf("publish for %s = (%q, %v), want (%q, true)", id, got, ok, want)
		}
	}
}

func TestSweepClaimedNeverStartedCohortFailureStopsTheSweep(t *testing.T) {
	fs := &fakeStore{chargeErr: errors.New("boom")}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.Sweep(context.Background()); err == nil {
		t.Fatal("Sweep succeeded although the claimed-never-started cohort failed")
	}
	for _, call := range fs.callOrder {
		if call == "task_never_dispatched" {
			t.Fatalf("a later pass ran after the failed cohort: %v", fs.callOrder)
		}
	}
}

func evidenceFixture(t *testing.T, rows int64) (*fakeStore, *Service, store.Worker, uuid.UUID) {
	t.Helper()
	fs, svc, wkr, runID := runningStateFixture(t)
	fs.evidenceRows = rows
	return fs, svc, wkr, runID
}

func TestRegistrationEvidenceAckValues(t *testing.T) {
	epoch := int64(7)
	for rows, want := range map[int64]string{1: RegistrationEvidenceRecorded, 0: RegistrationEvidenceRefused} {
		fs, svc, wkr, runID := evidenceFixture(t, rows)
		res, err := svc.SetStateReportWithReconciliation(context.Background(), wkr, runID, StateRequest{
			State:                "running",
			RegistrationEvidence: &RegistrationEvidence{RegisterNonce: "n", ClaimGeneration: 3, RegistrationEpoch: &epoch},
		})
		if err != nil || res.RegistrationEvidence != want {
			t.Fatalf("rows=%d: evidence=%q err=%v, want %q", rows, res.RegistrationEvidence, err, want)
		}
		if len(fs.evidenceArgs) != 1 {
			t.Fatalf("evidence writes = %d, want 1", len(fs.evidenceArgs))
		}
		arg := fs.evidenceArgs[0]
		if arg.ID != runID || arg.WorkerID != pgconv.UUID(wkr.ID) || arg.Generation != (pgtype.Int8{Int64: 3, Valid: true}) ||
			arg.RegisterNonce.String != "n" || arg.RegistrationEpoch.Int64 != 7 {
			t.Fatalf("evidence write = %+v", arg)
		}
	}
}

// The evidence is written even when the status write is refused: the two are independent.
func TestRegistrationEvidenceSurvivesARefusedStatusWrite(t *testing.T) {
	epoch := int64(0)
	fs, svc, wkr, runID := evidenceFixture(t, 1)
	fs.setRunningRows = 0
	res, err := svc.SetStateReportWithReconciliation(context.Background(), wkr, runID, StateRequest{
		State:                "running",
		RegistrationEvidence: &RegistrationEvidence{RegisterNonce: "n", ClaimGeneration: 1, RegistrationEpoch: &epoch},
	})
	if err != nil || res.Applied || res.RegistrationEvidence != RegistrationEvidenceRecorded || len(fs.evidenceArgs) != 1 {
		t.Fatalf("applied=%v evidence=%q writes=%d err=%v, want a refused write with recorded evidence", res.Applied, res.RegistrationEvidence, len(fs.evidenceArgs), err)
	}
}

func TestRegistrationEvidenceValidation(t *testing.T) {
	zero, neg := int64(0), int64(-1)
	for name, req := range map[string]StateRequest{
		"empty nonce":     {State: "running", RegistrationEvidence: &RegistrationEvidence{ClaimGeneration: 1, RegistrationEpoch: &zero}},
		"zero generation": {State: "running", RegistrationEvidence: &RegistrationEvidence{RegisterNonce: "n", RegistrationEpoch: &zero}},
		"negative epoch":  {State: "running", RegistrationEvidence: &RegistrationEvidence{RegisterNonce: "n", ClaimGeneration: 1, RegistrationEpoch: &neg}},
		"missing epoch":   {State: "running", RegistrationEvidence: &RegistrationEvidence{RegisterNonce: "n", ClaimGeneration: 1}},
		"wrong status":    {State: "completed", RegistrationEvidence: &RegistrationEvidence{RegisterNonce: "n", ClaimGeneration: 1, RegistrationEpoch: &zero}},
	} {
		fs, svc, wkr, runID := evidenceFixture(t, 1)
		if _, err := svc.SetStateReportWithReconciliation(context.Background(), wkr, runID, req); !errors.Is(err, ErrInvalidRegistrationEvidence) {
			t.Fatalf("%s: err = %v, want ErrInvalidRegistrationEvidence", name, err)
		}
		if len(fs.evidenceArgs) != 0 {
			t.Fatalf("%s: an invalid object reached the store", name)
		}
	}
}
