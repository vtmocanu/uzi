package workersvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func newAutomaticRoundsFixture(t *testing.T, limit int32) (codexTestEnv, *Service, store.Worker, uuid.UUID, PlanCrossCheckCandidate) {
	t.Helper()
	env := setupCodexLiveDB(t)
	user, worker, repo := env.seedCodexInfra(t)
	secret := env.seedStaticAPIKey(t, user, "rounds", "fixture-"+uuid.NewString())
	env.exec("UPDATE user_secrets SET is_default=true WHERE id=$1", secret)
	env.exec("UPDATE workers SET protocol_capabilities=ARRAY['cross_check_v1','cross_check_rounds_v1'] WHERE id=$1", worker)
	lead := uuid.New()
	env.exec(`INSERT INTO runs(id,user_id,repo_id,worker_id,issue_iid,issue_title,issue_description,status,harness,auto_approve,plan_cross_check_required,claim_generation)
 VALUES($1,$2,$3,$4,2150,'round issue','body','running','claude',true,true,1)`, lead, user, repo, worker)
	p := testParams()
	p.PlanCrossCheckMaxRevisions = limit
	p.PlanCrossCheckMaxRevisionsSet = true
	svc := New(env.q, env.box, p)
	svc.SetTxBeginner(env.pool)
	c := PlanCrossCheckCandidate{PlanMd: "plan", Milestones: []byte("[]"), RequiredCapabilities: []string{}, RequiredTools: []string{}, SizeClass: "s", BaseCommit: strings.Repeat("a", 40), PlanningDiff: "diff"}
	return env, svc, store.Worker{ID: worker, UserID: user}, lead, c
}

func TestPlanCrossCheckAutomaticRoundsBudgetLiveDB(t *testing.T) {
	for limit := int32(0); limit <= 4; limit++ {
		t.Run(fmt.Sprintf("budget%d", limit), func(t *testing.T) {
			env, svc, w, lead, c := newAutomaticRoundsFixture(t, limit)
			for round := int32(1); round <= limit+1; round++ {
				cc, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, c, round)
				if err != nil {
					t.Fatal(err)
				}
				if !cc.AutomaticRoundsEnabled || cc.AutomaticRevisionLimit != limit || cc.Round != round {
					t.Fatalf("snapshot: %+v", cc)
				}
				// A later config increase must never widen this execution's snapshot.
				svc.p.PlanCrossCheckMaxRevisions = 4
				retry, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, c, round)
				if err != nil || retry.ID != cc.ID {
					t.Fatalf("retry: %v", err)
				}
				changed := c
				changed.PlanMd = "different"
				if _, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, changed, round); !errors.Is(err, ErrCrossCheckInterrupted) {
					t.Fatalf("changed candidate: %v", err)
				}
				child := uuid.UUID(cc.CheckerRunID.Bytes)
				env.exec("UPDATE runs SET worker_id=$2,status='running',claim_generation=1 WHERE id=$1", child, w.ID)
				if _, err := svc.DecidePlanCrossCheck(env.ctx, w, child, 1, "revise", "revise", []byte(`{"summary":"revise","items":[]}`)); err != nil {
					t.Fatal(err)
				}
			}
			wantError := ErrCrossCheckRevisionsExhausted
			if limit == 4 {
				// Round 6 exceeds the protocol's maximum candidate number.
				wantError = ErrCrossCheckRefused
			}
			if _, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, c, limit+2); !errors.Is(err, wantError) {
				t.Fatalf("budget exceeded: %v, want %v", err, wantError)
			}
			var count, revise int
			if err := env.pool.QueryRow(env.ctx, "SELECT (SELECT count(*) FROM cross_checks WHERE lead_run_id=$1),revise_count FROM runs WHERE id=$1", lead).Scan(&count, &revise); err != nil {
				t.Fatal(err)
			}
			if count != int(limit+1) || revise != 0 {
				t.Fatalf("count=%d human revisions=%d", count, revise)
			}
		})
	}
}

func TestPlanCrossCheckAutomaticRoundsRecoveryPolicyLiveDB(t *testing.T) {
	cases := []struct {
		name, verdict, reason  string
		expired, gate, allowed bool
	}{
		{"pending before", "pending", "", false, false, true},
		{"pending expired", "pending", "", true, false, false},
		{"revise", "revise", "revise", false, false, true},
		{"unstored approval", "approve", "approve", false, false, true},
		{"block", "block", "block", false, false, false},
		{"timeout", "failed", "timed_out", true, false, false},
		{"model error", "failed", "model_error", false, false, false},
		{"model timeout", "failed", "model_timeout", false, false, false},
		{"malformed", "failed", "malformed", false, false, false},
		{"checker unavailable", "failed", "checker_unavailable", false, false, false},
		{"confinement failed", "failed", "confinement_failed", false, false, false},
		{"human gate", "revise", "revise", false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, svc, w, lead, c := newAutomaticRoundsFixture(t, 2)
			cc, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, c, 1)
			if err != nil {
				t.Fatal(err)
			}
			if tc.verdict != "pending" {
				env.exec("UPDATE cross_checks SET verdict=$2,reason_class=$3,decided_at=now(),wait_credited=true WHERE id=$1", cc.ID, tc.verdict, tc.reason)
			}
			if tc.expired {
				env.exec("UPDATE cross_checks SET deadline_at=now()-interval '1 second' WHERE id=$1", cc.ID)
			}
			if tc.gate {
				env.exec("UPDATE runs SET status='awaiting_approval',auto_approve=false,plan_md='human plan',gate_revision=1 WHERE id=$1", lead)
			}
			env.exec("UPDATE runs SET claim_generation=2 WHERE id=$1", lead)
			next, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 2, c, 2)
			if tc.allowed {
				if err != nil || next.Round != 2 || next.CheckerRunID == cc.CheckerRunID {
					t.Fatalf("eligible recovery: %v", err)
				}
				status, _, statusErr := svc.PlanCrossCheckStatus(env.ctx, w, lead, 2, 2)
				if statusErr != nil || status.ID != next.ID || status.Round != 2 {
					t.Fatalf("round 2 status: round=%d err=%v", status.Round, statusErr)
				}
				retry, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 2, c, 2)
				if err != nil || retry.ID != next.ID {
					t.Fatalf("recovery retry: %v", err)
				}
			} else {
				if !errors.Is(err, ErrCrossCheckRefused) {
					t.Fatalf("forbidden recovery: %v", err)
				}
				var children, rounds int
				if err := env.pool.QueryRow(env.ctx, "SELECT (SELECT count(*) FROM runs WHERE target_run_id=$1 AND kind='cross_check'),(SELECT count(*) FROM cross_checks WHERE lead_run_id=$1)", lead).Scan(&children, &rounds); err != nil || children != 1 || rounds != 1 {
					t.Fatalf("forbidden children=%d rounds=%d err=%v", children, rounds, err)
				}
				if tc.verdict == "block" || tc.verdict == "failed" {
					status, _, statusErr := svc.PlanCrossCheckStatus(env.ctx, w, lead, 2, 1)
					if statusErr != nil || status.Verdict != tc.verdict || status.ReasonClass.String != tc.reason {
						t.Fatalf("decided fallback changed on status read: verdict=%s reason=%s err=%v", status.Verdict, status.ReasonClass.String, statusErr)
					}
				}
			}
		})
	}
}

func TestPlanCrossCheckAutomaticRoundsCurrentWorkerDowngradeLiveDB(t *testing.T) {
	env, svc, w, lead, c := newAutomaticRoundsFixture(t, 2)
	cc, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, c, 1)
	if err != nil {
		t.Fatal(err)
	}
	env.exec("UPDATE cross_checks SET verdict='revise',reason_class='revise',decided_at=now(),wait_credited=true WHERE id=$1", cc.ID)
	env.exec("UPDATE workers SET protocol_capabilities=ARRAY['cross_check_v1'] WHERE id=$1", w.ID)
	if _, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, c, 2); !errors.Is(err, ErrCrossCheckInterrupted) {
		t.Fatalf("downgraded worker: %v", err)
	}
	if retry, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, c, 1); err != nil || retry.ID != cc.ID {
		t.Fatalf("downgrade lost immutable retry: %v", err)
	}
}

func TestPlanCrossCheckAutomaticRoundsConcurrentRequestedRoundLiveDB(t *testing.T) {
	env, svc, w, lead, c := newAutomaticRoundsFixture(t, 2)
	for round := int32(1); round <= 2; round++ {
		// Two requests start together; each has one attempt and a shared ten-second
		// deadline. Collect both results before asserting so failure cannot strand a sibling.
		ctx, cancel := context.WithTimeout(env.ctx, 10*time.Second)
		start := make(chan struct{})
		type result struct {
			cc  store.CrossCheck
			err error
		}
		results := make(chan result, 2)
		for i := 0; i < 2; i++ {
			go func() {
				<-start
				cc, err := svc.SubmitPlanCrossCheck(ctx, w, lead, 1, c, round)
				results <- result{cc, err}
			}()
		}
		close(start)
		first, second := <-results, <-results
		cancel()
		if first.err != nil || second.err != nil || first.cc.ID != second.cc.ID ||
			first.cc.Round != round || first.cc.CheckerRunID != second.cc.CheckerRunID {
			t.Fatalf("round %d concurrent requests: first=%+v second=%+v", round, first, second)
		}
		var attempts, children, humanRevisions int
		if err := env.pool.QueryRow(env.ctx, `SELECT
			(SELECT count(*) FROM cross_checks WHERE lead_run_id=$1),
			(SELECT count(*) FROM runs WHERE target_run_id=$1 AND kind='cross_check'),
			revise_count FROM runs WHERE id=$1`, lead).Scan(&attempts, &children, &humanRevisions); err != nil {
			t.Fatal(err)
		}
		if attempts != int(round) || children != int(round) || humanRevisions != 0 {
			t.Fatalf("round %d attempts=%d children=%d human revisions=%d", round, attempts, children, humanRevisions)
		}
		child := uuid.UUID(first.cc.CheckerRunID.Bytes)
		env.exec("UPDATE runs SET worker_id=$2,status='running',claim_generation=1 WHERE id=$1", child, w.ID)
		if _, err := svc.DecidePlanCrossCheck(env.ctx, w, child, 1, "revise", "revise", []byte(`{"summary":"revise","items":[]}`)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPlanCrossCheckAutomaticRoundsConflictingCandidatesLiveDB(t *testing.T) {
	env, svc, w, lead, c := newAutomaticRoundsFixture(t, 2)
	other := c
	other.PlanMd += "\nconflicting proposal"
	for round := int32(1); round <= 2; round++ {
		ctx, cancel := context.WithTimeout(env.ctx, 10*time.Second)
		start := make(chan struct{})
		type result struct {
			cc        store.CrossCheck
			candidate PlanCrossCheckCandidate
			err       error
		}
		results := make(chan result, 2)
		for _, candidate := range []PlanCrossCheckCandidate{c, other} {
			go func() {
				<-start
				cc, err := svc.SubmitPlanCrossCheck(ctx, w, lead, 1, candidate, round)
				results <- result{cc, candidate, err}
			}()
		}
		close(start)
		winner, loser := <-results, <-results
		cancel()
		if winner.err != nil {
			winner, loser = loser, winner
		}
		if winner.err != nil || !errors.Is(loser.err, ErrCrossCheckRefused) || winner.cc.Round != round {
			t.Fatalf("conflicting round %d: winner err=%v loser err=%v", round, winner.err, loser.err)
		}
		stored, err := env.q.GetExactPlanCrossCheck(env.ctx, store.GetExactPlanCrossCheckParams{LeadRunID: lead, Round: round})
		if err != nil || stored.ID != winner.cc.ID || !stored.PlanMd.Valid || stored.PlanMd.String != winner.candidate.PlanMd {
			t.Fatalf("winning candidate not immutable: err=%v", err)
		}
		retry, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, winner.candidate, round)
		if err != nil || retry.ID != stored.ID {
			t.Fatalf("winning candidate retry: %v", err)
		}
		if _, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, loser.candidate, round); !errors.Is(err, ErrCrossCheckRefused) {
			t.Fatalf("conflicting candidate retry: %v", err)
		}
		var attempts, children, humanRevisions int
		if err := env.pool.QueryRow(env.ctx, `SELECT
			(SELECT count(*) FROM cross_checks WHERE lead_run_id=$1),
			(SELECT count(*) FROM runs WHERE target_run_id=$1 AND kind='cross_check'),
			revise_count FROM runs WHERE id=$1`, lead).Scan(&attempts, &children, &humanRevisions); err != nil {
			t.Fatal(err)
		}
		if attempts != int(round) || children != int(round) || humanRevisions != 0 {
			t.Fatalf("conflict created extra rows: attempts=%d children=%d human revisions=%d", attempts, children, humanRevisions)
		}
		child := uuid.UUID(stored.CheckerRunID.Bytes)
		env.exec("UPDATE runs SET worker_id=$2,status='running',claim_generation=1 WHERE id=$1", child, w.ID)
		if _, err := svc.DecidePlanCrossCheck(env.ctx, w, child, 1, "revise", "revise", []byte(`{"summary":"revise","items":[]}`)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPlanCrossCheckAutomaticRoundsRecoveryConsumesBudgetLiveDB(t *testing.T) {
	for _, approval := range []bool{false, true} {
		for limit := int32(0); limit <= 4; limit++ {
			t.Run(fmt.Sprintf("approval%v/budget%d", approval, limit), func(t *testing.T) {
				env, svc, w, lead, c := newAutomaticRoundsFixture(t, limit)
				for round := int32(1); round <= limit+1; round++ {
					cc, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, int64(round), c, round)
					if err != nil {
						t.Fatal(err)
					}
					child := uuid.UUID(cc.CheckerRunID.Bytes)
					env.exec("UPDATE runs SET worker_id=$2,status='running',claim_generation=1 WHERE id=$1", child, w.ID)
					want := "superseded"
					if approval {
						if _, err := svc.DecidePlanCrossCheck(env.ctx, w, child, 1, "approve", "approve", []byte(`{"summary":"approved","items":[]}`)); err != nil {
							t.Fatal(err)
						}
						want = "approved_not_stored"
					}
					env.exec("UPDATE runs SET claim_generation=$2 WHERE id=$1", lead, int64(round+1))
					settled, err := env.q.GetExactPlanCrossCheck(env.ctx, store.GetExactPlanCrossCheckParams{LeadRunID: lead, Round: round})
					if err != nil || settled.Verdict != "failed" || settled.ReasonClass.String != want || !settled.InterruptedAt.Valid {
						t.Fatalf("recovered round %d: %+v err=%v", round, settled, err)
					}
					if _, err := svc.DecidePlanCrossCheck(env.ctx, w, child, 1, "approve", "approve", []byte(`{"summary":"late","items":[]}`)); !errors.Is(err, ErrCrossCheckRefused) {
						t.Fatalf("old child approval after reclaim: %v", err)
					}
				}
				want := ErrCrossCheckRevisionsExhausted
				if limit == 4 {
					want = ErrCrossCheckRefused
				}
				if _, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, int64(limit+2), c, limit+2); !errors.Is(err, want) {
					t.Fatalf("reclaim granted extra budget: %v want %v", err, want)
				}
				var attempts, children, humanRevisions int
				if err := env.pool.QueryRow(env.ctx, `SELECT
					(SELECT count(*) FROM cross_checks WHERE lead_run_id=$1),
					(SELECT count(*) FROM runs WHERE target_run_id=$1 AND kind='cross_check'),
					revise_count FROM runs WHERE id=$1`, lead).Scan(&attempts, &children, &humanRevisions); err != nil {
					t.Fatal(err)
				}
				if attempts != int(limit+1) || children != int(limit+1) || humanRevisions != 0 {
					t.Fatalf("attempts=%d children=%d human revisions=%d", attempts, children, humanRevisions)
				}
			})
		}
	}
}
