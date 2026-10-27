package workersvc

import (
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Submit through the service, with its clock anchored to PostgreSQL rather than
// the test host. Queue dwell is part of the unchanged default 30-minute budget.
func admissionCheckerBase(t *testing.T, stage string) (crossCheckContentionFixture, store.Worker) {
	t.Helper()
	var f crossCheckContentionFixture
	if stage == "code" {
		f = codeFixture(t)
	} else {
		f = laneFixture(t)
		f.env.exec("DELETE FROM cross_checks WHERE lead_run_id=$1", f.lead)
		f.env.exec("DELETE FROM runs WHERE id=$1", f.runID)
		f.env.exec("UPDATE user_secrets SET is_default=true WHERE id=$1", f.aliasID)
		f.env.exec("UPDATE runs SET worker_id=$2 WHERE id=$1", f.lead, f.workerID)
	}
	return f, wkrRow(t, f.env, f.workerID)
}

func admissionCheckerFixture(t *testing.T, stage string, elapsed time.Duration) (crossCheckContentionFixture, store.Worker, store.CrossCheck) {
	t.Helper()
	f, w := admissionCheckerBase(t, stage)
	f, cc := admissionCheckerSubmit(t, f, w, stage, elapsed, nil)
	return f, w, cc
}

// A transaction binds both creation and claim to the same SQL now(). Outside
// boundary tests, use the actual database clock and backdate only the service
// clock BEFORE submission; the persisted snapshot is never rewritten.
func admissionCheckerSubmit(t *testing.T, f crossCheckContentionFixture, w store.Worker, stage string, elapsed time.Duration, tx pgx.Tx) (crossCheckContentionFixture, store.CrossCheck) {
	t.Helper()
	var dbNow time.Time
	q := f.env.q
	if tx != nil {
		q = q.WithTx(tx)
		f.svc.SetTxBeginner(tx)
		f.svc.q = q
		if err := tx.QueryRow(f.env.ctx, "SELECT now()").Scan(&dbNow); err != nil {
			t.Fatal(err)
		}
	} else if err := f.env.pool.QueryRow(f.env.ctx, "SELECT clock_timestamp()").Scan(&dbNow); err != nil {
		t.Fatal(err)
	}
	submittedAt := dbNow.Add(-elapsed)
	f.svc.SetClock(func() time.Time { return submittedAt })
	var cc store.CrossCheck
	var err error
	if stage == "code" {
		cc, err = f.svc.SubmitCodeCrossCheck(f.env.ctx, w, f.lead, 1, strings.Repeat("a", 40), strings.Repeat("b", 40))
	} else {
		cc, err = f.svc.SubmitPlanCrossCheck(f.env.ctx, w, f.lead, 1, f.candidate)
	}
	if err != nil || !cc.CheckerRunID.Valid {
		t.Fatalf("%s service submission: checker=%v err=%v", stage, cc.CheckerRunID, err)
	}
	f.runID = uuid.UUID(cc.CheckerRunID.Bytes)
	child, err := q.GetRunByID(f.env.ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if !cc.DeadlineAt.Valid || !cc.DeadlineAt.Time.Equal(submittedAt.Add(30*time.Minute)) ||
		child.BudgetWallSeconds.Int32 != 1800 {
		t.Fatalf("%s default timeout does not include queue dwell: deadline=%v clock=%v", stage, cc.DeadlineAt, dbNow)
	}
	// Restore a database-anchored present clock for status/settlement calls.
	f.svc.SetClock(func() time.Time { return dbNow })
	return f, cc
}

func admissionCheckerStatus(t *testing.T, f crossCheckContentionFixture, w store.Worker, stage string) store.CrossCheck {
	t.Helper()
	var cc store.CrossCheck
	var err error
	if stage == "code" {
		cc, err = f.svc.CodeCrossCheckStatus(f.env.ctx, w, f.lead, 1)
	} else {
		cc, _, err = f.svc.PlanCrossCheckStatus(f.env.ctx, w, f.lead, 1, 1)
	}
	if err != nil {
		t.Fatal(err)
	}
	return cc
}

func admissionCheckerLateVerdict(t *testing.T, f crossCheckContentionFixture, w store.Worker, stage string, generation int64) {
	t.Helper()
	var err error
	if stage == "code" {
		_, err = f.svc.DecideCodeCrossCheck(f.env.ctx, w, f.runID, generation, "completed", "", nil)
	} else {
		_, err = f.svc.DecidePlanCrossCheck(f.env.ctx, w, f.runID, generation, "approve", "approve", []byte(`{"summary":"late approval","items":[]}`))
	}
	if !errors.Is(err, ErrCrossCheckRefused) {
		t.Fatalf("late %s verdict: %v, want refusal", stage, err)
	}
}

func admissionCheckerTimedOut(t *testing.T, cc store.CrossCheck, stage string, deadline time.Time) {
	t.Helper()
	failed := cc.Verdict == "failed"
	if stage == "code" {
		failed = cc.Outcome.String == "failed"
	}
	if !failed || cc.ReasonClass.String != "timed_out" || !cc.DecidedAt.Valid ||
		!cc.DecidedAt.Time.Equal(deadline) || !cc.DeadlineAt.Time.Equal(deadline) {
		t.Fatalf("%s timeout changed settlement/deadline: verdict=%s outcome=%s reason=%s decided=%v deadline=%v",
			stage, cc.Verdict, cc.Outcome.String, cc.ReasonClass.String, cc.DecidedAt, cc.DeadlineAt)
	}
}

func TestCodexAdmissionCheckerShortCooldownLiveDB(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		t.Run(stage, func(t *testing.T) {
			f, w, created := admissionCheckerFixture(t, stage, 0)
			seedAdmissionCooldown(t, f.env, f.aliasID)
			// Ten minutes plus at most one minute of spread fits the deadline.
			f.env.exec(`UPDATE codex_account_admission_throttle SET
				effective_expiry=now()+interval '10 minutes',rate_expiry=now()+interval '10 minutes'
				WHERE user_id=$1`, f.userID)
			before := mustRun(t, f.env, f.runID)
			if p := laneClaim(t, f, "cross_check", nil); p != nil {
				t.Fatal("cooldown admitted checker")
			}
			f.unchanged(t, before)
			pending := admissionCheckerStatus(t, f, w, stage)
			if !pending.DeadlineAt.Time.Equal(created.DeadlineAt.Time) || pending.ReasonClass.Valid ||
				(stage == "plan" && pending.Verdict != "pending") || (stage == "code" && pending.Outcome.String != "pending") {
				t.Fatal("finite cooldown changed pending attempt or extended deadline")
			}
			// Expire the entire spread window; the same claim is the positive
			// control that rules out a hidden capability, affinity or slot gate.
			f.env.exec(`UPDATE codex_account_admission_throttle SET
				effective_expiry=now()-interval '2 minutes',rate_expiry=now()-interval '2 minutes'
				WHERE user_id=$1`, f.userID)
			p := laneClaim(t, f, "cross_check", nil)
			if p == nil || p.RunID != f.runID.String() || p.ClaimGeneration != before.ClaimGeneration+1 ||
				!mustRun(t, f.env, f.runID).CrossCheckLane {
				t.Fatal("checker did not claim after finite cooldown")
			}
			if cc := admissionCheckerStatus(t, f, w, stage); !cc.DeadlineAt.Time.Equal(created.DeadlineAt.Time) {
				t.Fatal("claim reset deadline after queue dwell")
			}
			// A separate service-produced expired attempt represents a checker
			// whose claim predates expiry. Seed only child custody, never cc.
			f, w, created = admissionCheckerFixture(t, stage, 31*time.Minute)
			f.env.exec(`UPDATE runs SET status='claimed',worker_id=$2,
				claim_generation=1,claimed_at=now()-interval '31 minutes',
				claim_released_at=NULL,cross_check_lane=true WHERE id=$1`, f.runID, f.workerID)
			claimed := mustRun(t, f.env, f.runID)
			if claimed.Status != "claimed" || claimed.WorkerID.Bytes != [16]byte(w.ID) ||
				claimed.ClaimReleasedAt.Valid || !claimed.CrossCheckLane || claimed.ClaimGeneration != 1 {
				t.Fatal("expired fixture lacks attributable live checker custody")
			}
			expired := created.DeadlineAt.Time
			admissionCheckerLateVerdict(t, f, w, stage, claimed.ClaimGeneration)
			settled := admissionCheckerStatus(t, f, w, stage)
			admissionCheckerTimedOut(t, settled, stage, expired)
			child := mustRun(t, f.env, f.runID)
			if child.Status != "cancelled" || !child.ClaimReleasedAt.Valid || child.ClaimGeneration != claimed.ClaimGeneration {
				t.Fatal("timeout did not cancel/release claimed checker")
			}
			admissionCheckerLateVerdict(t, f, w, stage, claimed.ClaimGeneration)
			repeated := admissionCheckerStatus(t, f, w, stage)
			if !reflect.DeepEqual(settled, repeated) {
				t.Fatal("late verdict or repeated status overwrote timeout")
			}
			f.unchanged(t, child)
		})
	}
}

func TestCodexAdmissionCheckerLongCooldownLiveDB(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		t.Run(stage, func(t *testing.T) {
			f, w, created := admissionCheckerFixture(t, stage, 0)
			seedAdmissionCooldown(t, f.env, f.aliasID)
			f.env.exec("UPDATE codex_account_admission_throttle SET rate_expiry=effective_expiry WHERE user_id=$1", f.userID)
			before := mustRun(t, f.env, f.runID)
			if p := laneClaim(t, f, "cross_check", nil); p != nil {
				t.Fatal("cooldown longer than deadline admitted checker")
			}
			f.unchanged(t, before)
			pending := admissionCheckerStatus(t, f, w, stage)
			if !pending.DeadlineAt.Time.Equal(created.DeadlineAt.Time) || pending.ReasonClass.Valid ||
				(stage == "plan" && pending.Verdict != "pending") || (stage == "code" && pending.Outcome.String != "pending") {
				t.Fatal("long cooldown changed pending attempt or extended deadline")
			}
			// Create another attempt with its full default budget already spent.
			// Keep admission active; timeout must not grant a claim exemption.
			f, w, created = admissionCheckerFixture(t, stage, 31*time.Minute)
			seedAdmissionCooldown(t, f.env, f.aliasID)
			f.env.exec("UPDATE codex_account_admission_throttle SET rate_expiry=effective_expiry WHERE user_id=$1", f.userID)
			before = mustRun(t, f.env, f.runID)
			if p := laneClaim(t, f, "cross_check", nil); p != nil {
				t.Fatal("expired checker bypassed long cooldown")
			}
			f.unchanged(t, before)
			deadline := created.DeadlineAt.Time
			settled := admissionCheckerStatus(t, f, w, stage)
			admissionCheckerTimedOut(t, settled, stage, deadline)
			child := mustRun(t, f.env, f.runID)
			if child.Status != "cancelled" || !child.ClaimReleasedAt.Valid || child.ClaimGeneration != before.ClaimGeneration {
				t.Fatal("timeout did not cancel/release queued checker without claiming it")
			}
			f.unchanged(t, child)
			if stage == "plan" {
				// Use the existing human gate interface: status alone does not
				// invent a lead lifecycle transition or clear auto approval.
				c, err := NormalizePlanCrossCheckCandidate(f.candidate)
				if err != nil {
					t.Fatal(err)
				}
				digest, err := c.Digest()
				if err != nil {
					t.Fatal(err)
				}
				generation := int64(1)
				milestones := []Milestone{}
				req := StateRequest{State: "running", ClaimGeneration: &generation, PlanMd: &c.PlanMd,
					Milestones: &milestones, RequiredCapabilities: &c.RequiredCapabilities,
					RequiredTools: &c.RequiredTools, SizeClass: &c.SizeClass, CandidateDigest: hex.EncodeToString(digest)}
				if _, applied, err := f.svc.SetState(f.env.ctx, w, f.lead, req); applied || !errors.Is(err, ErrInvalidState) {
					t.Fatalf("timeout entered implementation: applied=%v err=%v", applied, err)
				}
				presentation := uuid.New()
				req.State, req.CandidateDigest, req.PresentationID = "awaiting_approval", "", &presentation
				_, applied, err := f.svc.SetState(f.env.ctx, w, f.lead, req)
				lead := mustRun(t, f.env, f.lead)
				if err != nil || !applied || lead.Status != "awaiting_approval" || lead.AutoApprove ||
					lead.PlanCrossCheckGateReason.String != "timed_out" {
					t.Fatalf("human fallback: applied=%v status=%s auto=%v reason=%s err=%v",
						applied, lead.Status, lead.AutoApprove, lead.PlanCrossCheckGateReason.String, err)
				}
			} else if lead := mustRun(t, f.env, f.lead); lead.Status != "running" || lead.AutoApprove {
				t.Fatal("code timeout changed existing human approval policy")
			}
		})
	}
}

// Creation and claim share transaction-stable now(), with a separate service
// attempt for each boundary. No persisted deadline is changed for the control.
func TestCodexAdmissionCheckerDeadlineEqualityLiveDB(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		t.Run(stage, func(t *testing.T) {
			for _, delta := range []time.Duration{0, time.Microsecond} {
				t.Run(delta.String(), func(t *testing.T) {
					f, w := admissionCheckerBase(t, stage)
					seedAdmissionCooldown(t, f.env, f.aliasID)
					f.env.exec(`UPDATE codex_account_admission_throttle SET
						effective_expiry=now()-interval '2 minutes',rate_expiry=now()-interval '2 minutes'
						WHERE user_id=$1`, f.userID)
					tx, err := f.env.pool.Begin(f.env.ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = tx.Rollback(f.env.ctx) }()
					f, created := admissionCheckerSubmit(t, f, w, stage, 30*time.Minute-delta, tx)
					q := f.env.q.WithTx(tx)
					before, err := q.GetRunByID(f.env.ctx, f.runID)
					if err != nil {
						t.Fatal(err)
					}
					var now time.Time
					if err := tx.QueryRow(f.env.ctx, "SELECT now()").Scan(&now); err != nil {
						t.Fatal(err)
					}
					if !created.DeadlineAt.Time.Equal(now.Add(delta)) {
						t.Fatalf("service-created deadline=%v, want SQL now()+%v", created.DeadlineAt, delta)
					}
					params := f.env.codexClaimParams(f.userID, f.workerID, w.ProtocolCapabilities)
					params.Lane = "cross_check"
					params.CodexCuratedModels = CodexCuratedModels()
					params.CrossCheckEvaluatedAt = pgtype.Timestamptz{Time: now, Valid: true}
					params.CrossCheckAffinityCutoff = pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true}
					params.RecoveryCapable = true
					params.WorkerIdentity = "admission-checker-equality"
					claimed, err := q.ClaimRun(f.env.ctx, params)
					if delta == 0 {
						if !errors.Is(err, pgx.ErrNoRows) {
							t.Fatalf("claim at exact deadline: %v, want no rows", err)
						}
						after, err := q.GetRunByID(f.env.ctx, f.runID)
						if err != nil || !reflect.DeepEqual(before, after) {
							t.Fatalf("equality refusal changed checker: %v", err)
						}
						var holds int
						if err := tx.QueryRow(f.env.ctx, "SELECT count(*) FROM recovery_custody_holds WHERE run_id=$1", f.runID).Scan(&holds); err != nil || holds != 0 {
							t.Fatalf("equality refusal custody=%d err=%v", holds, err)
						}
					} else if err != nil || claimed.ID != f.runID || claimed.ClaimGeneration != before.ClaimGeneration+1 || !claimed.CrossCheckLane {
						t.Fatalf("just-before-deadline positive control: id=%s generation=%d lane=%v err=%v", claimed.ID, claimed.ClaimGeneration, claimed.CrossCheckLane, err)
					}
					if err := tx.Rollback(f.env.ctx); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}
