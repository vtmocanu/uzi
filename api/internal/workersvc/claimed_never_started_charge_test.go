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
	charged, exhausted := uuid.New(), uuid.New()
	fs := &fakeStore{chargedClaimed: []store.ChargeClaimedNeverStartedContinuationsRow{
		{ID: charged, UserID: uuid.New(), Status: "queued"},
		{ID: exhausted, UserID: uuid.New(), Status: "recovery_wait"},
	}}
	svc := New(fs, newBox(t), testParams())
	spy := &stateSpy{}
	svc.SetBroadcaster(spy)
	svc.now = func() time.Time { return time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC) }

	res, err := svc.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.ClaimedCharged != 1 || res.ClaimedExhausted != 1 || res.ClaimedReset != 0 {
		t.Fatalf("result = %+v, want one charge, one exhaustion and no uncharged reset", res)
	}
	if fs.chargeArg == nil || int(fs.chargeArg.MaxRequeues) != testParams().RunMaxRequeues {
		t.Fatalf("charge args = %+v, want MaxRequeues %d", fs.chargeArg, testParams().RunMaxRequeues)
	}
	// The SAME cutoff as the uncharged sweep, so the two statements partition one candidate set.
	if !fs.chargeArg.Cutoff.Time.Equal(fs.claimCutoff.Time) || !fs.claimCutoff.Valid {
		t.Fatalf("charge cutoff %v differs from the sweep cutoff %v", fs.chargeArg.Cutoff, fs.claimCutoff)
	}
	for id, want := range map[uuid.UUID]string{charged: "queued", exhausted: "recovery_wait"} {
		if got, ok := spy.statusFor(id); !ok || got != want {
			t.Fatalf("publish for %s = (%q, %v), want (%q, true)", id, got, ok, want)
		}
	}
}

func TestSweepChargeFailureStopsBeforeTheUnchargedReset(t *testing.T) {
	fs := &fakeStore{chargeErr: errors.New("boom")}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.Sweep(context.Background()); err == nil {
		t.Fatal("Sweep succeeded although the charge pass failed")
	}
	for _, call := range fs.callOrder {
		if call == "claimed_never_started" {
			t.Fatalf("the uncharged reset ran after a failed charge pass: %v", fs.callOrder)
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
