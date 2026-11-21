package store_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #3252 m4: the phase-switch hold. SetRunPhaseSwitchHold parks a Claude run's own live claim
// (running | awaiting_approval -> paused, hold_reason 'phase_switch_unavailable'); the owner's
// resume re-queues it keeping worker, generation and marker, and every worker-report writer must
// refuse the marker in both the paused and the queued interval. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

const phaseSwitchReason = "phase_switch_unavailable"

// A hold taken at the human gate must bank that wait before replacing status_since. Resume only
// banks the later hold interval, so it cannot recover a gate interval the hold discarded.
func TestPhaseSwitchHoldPreservesApprovalWaitBudgetLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	worker := fx.worker("phase-budget", wpWorker{})
	budget := int32(3600)
	const priorBank = int32(120)
	for _, status := range []string{"running", "awaiting_approval"} {
		t.Run(status, func(t *testing.T) {
			startedAgo := 10*time.Second + time.Duration(priorBank)*time.Second
			sinceAgo := 5 * time.Second
			if status == "awaiting_approval" {
				startedAgo = 3*time.Hour + time.Duration(priorBank)*time.Second
				sinceAgo = 3*time.Hour - 10*time.Second
			}
			id := fx.run(wpRun{status: status, worker: &worker, claimGen: 3,
				startedAgo: startedAgo, sinceAgo: sinceAgo, budgetWall: &budget, budgetPaused: priorBank})
			before := fx.mustRun(id)
			request := store.SetRunPhaseSwitchHoldParams{
				ID: id, WorkerID: pgtype.UUID{Bytes: worker, Valid: true}, ClaimGeneration: 3}
			held, err := fx.q.SetRunPhaseSwitchHold(fx.ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			// The service's lost-ACK re-read is reached via this no-op: no repeat UPDATE or bank.
			if _, err := fx.q.SetRunPhaseSwitchHold(fx.ctx, request); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("repeat statement: %v, want no rows", err)
			}
			again := fx.mustRun(id)
			if again.BudgetPausedSeconds != held.BudgetPausedSeconds || !again.StatusSince.Time.Equal(held.StatusSince.Time) {
				t.Fatal("repeat changed the hold's pause bank or status clock")
			}
			if _, err := fx.q.ResumePausedRun(fx.ctx, store.ResumePausedRunParams{
				ID: id, UserID: fx.userID, GlobalTimeoutSeconds: 3600}); err != nil {
				t.Fatal(err)
			}
			var active int
			if err := fx.pool.QueryRow(fx.ctx, `SELECT GREATEST(0, EXTRACT(EPOCH FROM (now()-started_at))::int)-budget_paused_seconds FROM runs WHERE id=$1`, id).Scan(&active); err != nil {
				t.Fatal(err)
			}
			t.Logf("source=%s banked_before_resume=%d active_seconds_after_resume=%d wall_budget=%d", status, held.BudgetPausedSeconds, active, budget)
			if active < 0 || active > 30 {
				t.Fatalf("human gate wait became active time: got %d active seconds, expected about 10", active)
			}
			wantBank := priorBank
			if status == "awaiting_approval" {
				wantBank += int32(held.StatusSince.Time.Sub(before.StatusSince.Time).Seconds())
				// PostgreSQL rounds an epoch-to-int cast, whereas Duration.Seconds truncates here.
				if held.BudgetPausedSeconds < wantBank || held.BudgetPausedSeconds > wantBank+1 {
					t.Fatalf("bank=%d, want %d (+1 for rounding)", held.BudgetPausedSeconds, wantBank)
				}
			} else if held.BudgetPausedSeconds != priorBank {
				t.Fatalf("running hold changed bank: got %d, want %d", held.BudgetPausedSeconds, priorBank)
			}
		})
	}
}

// TestSetRunPhaseSwitchHoldStatementLiveDB executes the setter's fence directly: the six eligible
// kinds, interactive or not, in running or awaiting_approval, hold; every other shape matches 0 rows.
func TestSetRunPhaseSwitchHoldStatementLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	worker := fx.worker("w-3252s", wpWorker{})
	wid := pgtype.UUID{Bytes: worker, Valid: true}
	// The fixture inserts an issue-shaped row; runs_kind_shape needs each other kind's own columns.
	kindShape := map[string]string{
		"ci_fix":       "kind = 'ci_fix', pipeline_id = PIPE, pipeline_ref = 'ref-PIPE'",
		"prompt":       "kind = 'prompt', issue_iid = NULL",
		"task":         "kind = 'task', issue_iid = NULL, branch = 'uzi/task/x'",
		"self_improve": "kind = 'self_improve'",
		"mr_rework":    "kind = 'mr_rework', pipeline_ref = 'r-PIPE', mr_iid = PIPE, target_run_id = 'TARGET'",
	}
	seed := func(o wpRun, mut string) uuid.UUID {
		w := worker
		o.worker = &w
		o.claimGen = 3
		kind := o.kind
		o.kind = ""
		id := fx.run(o)
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET health = 'stalled', health_reason = 'x', health_since = now() WHERE id = $1`, id)
		if shape := kindShape[kind]; shape != "" {
			shape = strings.ReplaceAll(shape, "PIPE", fmt.Sprint(fx.iid))
			if kind == "mr_rework" {
				target := fx.run(wpRun{})
				shape = strings.ReplaceAll(shape, "TARGET", target.String())
			}
			mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET `+shape+` WHERE id = $1`, id)
		}
		if mut != "" {
			mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET `+mut+` WHERE id = $1`, id)
		}
		return id
	}
	hold := func(id uuid.UUID, w pgtype.UUID, gen int64) (store.Run, error) {
		return fx.q.SetRunPhaseSwitchHold(fx.ctx, store.SetRunPhaseSwitchHoldParams{
			HoldCapturedHead: pgtype.Text{String: "h", Valid: true}, ID: id, WorkerID: w, ClaimGeneration: gen})
	}

	for _, harness := range []string{"claude", "codex"} {
		for _, kind := range []string{"issue", "ci_fix", "self_improve", "prompt", "mr_rework", "task"} {
			for _, interactive := range []bool{false, true} {
				for _, status := range []string{"running", "awaiting_approval"} {
					t.Run(fmt.Sprintf("%s/%s/interactive=%v/%s", harness, kind, interactive, status), func(t *testing.T) {
						id := seed(wpRun{kind: kind, harness: harness, interactive: interactive, status: status}, "")
						// self_improve and ci_fix allow one active run each: free the slot for the next case.
						t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM runs WHERE id = $1`, id) })
						run, err := hold(id, wid, 3)
						if err != nil {
							t.Fatalf("hold: %v", err)
						}
						if run.Status != "paused" || run.HoldReason.String != phaseSwitchReason || run.HoldCapturedHead.String != "h" {
							t.Fatalf("held = (%q, %q, %q)", run.Status, run.HoldReason.String, run.HoldCapturedHead.String)
						}
						if run.OpenQuestionID.Valid || run.Health != "ok" || run.HealthReason.Valid || run.HealthSince.Valid {
							t.Fatalf("question/health not reset: %v %q", run.OpenQuestionID, run.Health)
						}
						if run.WorkerID.Bytes != worker || run.ClaimGeneration != 3 || run.ClaimReleasedAt.Valid {
							t.Fatal("the hold keeps the worker and the live claim")
						}
					})
				}
			}
		}

	}

	refused := map[string]func() uuid.UUID{
		"wrong generation": func() uuid.UUID { return seed(wpRun{}, "claim_generation = 4") },
		"released claim":   func() uuid.UUID { return seed(wpRun{}, "claim_released_at = now()") },
		"awaiting_input":   func() uuid.UUID { return seed(wpRun{status: "awaiting_input"}, "") },
		"queued":           func() uuid.UUID { return seed(wpRun{status: "queued"}, "") },
		// Kind is the setter's allowlist: a chat or cross_check run is never parked into this hold.
		"chat kind": func() uuid.UUID {
			return seed(wpRun{}, "kind = 'chat', repo_id = NULL, issue_iid = NULL, branch = NULL")
		},
		"cross_check kind": func() uuid.UUID {
			target := fx.run(wpRun{})
			return seed(wpRun{}, "kind = 'cross_check', issue_iid = NULL, target_run_id = '"+target.String()+
				"', report_only = true, budget_wall_seconds = 3600")
		},
		// runs.egress_profile_id is immutable after insert, so the profiled run is inserted whole.
		"egress profile": func() uuid.UUID {
			profile := uuid.New()
			mustExec(fx.ctx, t, fx.pool, `INSERT INTO egress_profiles (id, name, hosts) VALUES ($1, $2, '{docs.example.com}')`,
				profile, "ps-"+profile.String()[:20])
			id := uuid.New()
			fx.iid++
			mustExec(fx.ctx, t, fx.pool, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description,
				status, harness, worker_id, started_at, status_since, claim_generation, egress_profile_id)
				VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'running', 'claude', $5, now(), now(), 3, $6)`,
				id, fx.userID, fx.repoID, fx.iid, worker, profile)
			// Registered after the fixture's cleanup, so it runs first: the profile is RESTRICTed while a run uses it.
			t.Cleanup(func() {
				_, _ = fx.pool.Exec(fx.ctx, `DELETE FROM runs WHERE egress_profile_id = $1`, profile)
				_, _ = fx.pool.Exec(fx.ctx, `DELETE FROM egress_profiles WHERE id = $1`, profile)
			})
			return id
		},
		"review target": func() uuid.UUID {
			target := seed(wpRun{}, "")
			return seed(wpRun{}, "review_target_run_id = '"+target.String()+"'")
		},
	}
	for name, mk := range refused {
		if _, err := hold(mk(), wid, 3); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("%s: err = %v, want ErrNoRows", name, err)
		}
	}
	if _, err := hold(seed(wpRun{}, ""), pgtype.UUID{Bytes: uuid.New(), Valid: true}, 3); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("other worker: err = %v, want ErrNoRows", err)
	}
	id := seed(wpRun{}, "")
	if _, err := hold(id, wid, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := hold(id, wid, 3); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("the statement itself must be a 0-row no-op on an already-held row; err = %v", err)
	}
}

// TestPhaseSwitchHoldResumeAndCancelLiveDB: the owner's resume admits the hold and re-queues the run
// KEEPING worker, generation and marker; the owner's cancel still applies.
func TestPhaseSwitchHoldResumeAndCancelLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	worker := fx.worker("w-3252r", wpWorker{})
	w := worker
	id := fx.run(wpRun{status: "paused", worker: &w, claimGen: 3, holdReason: phaseSwitchReason, holdCapturedHead: "h"})
	if _, err := fx.q.ResumePausedRun(fx.ctx, store.ResumePausedRunParams{ID: id, UserID: fx.userID, GlobalTimeoutSeconds: 3600}); err != nil {
		t.Fatalf("ResumePausedRun on the phase-switch hold: %v", err)
	}
	resumed := fx.mustRun(id)
	if resumed.Status != "queued" || resumed.WorkerID.Bytes != worker || resumed.ClaimGeneration != 3 ||
		resumed.ClaimReleasedAt.Valid || resumed.HoldReason.String != phaseSwitchReason {
		t.Fatalf("resumed = (%q, worker %v, gen %d, released %v, hold %q), want queued keeping worker, generation and marker",
			resumed.Status, resumed.WorkerID.Bytes, resumed.ClaimGeneration, resumed.ClaimReleasedAt.Valid, resumed.HoldReason.String)
	}
}

// TestPhaseSwitchHoldRefusesLateWorkerReportsLiveDB: a late report from the parking flight must not
// move the held row in either interval (paused, and queued after the owner's resume). It covers the
// worker-report writers SetRunAwaitingInput, SetRunAwaitingFollowup, SetRunAwaitingApproval,
// SetRunCompleted, SetRunFailed, SetRunFailedPlanRejected and SupersedeRunByWorker. SetRunRunning,
// the limit/recovery/provider parks and the pause writers require status 'running', so they never
// match a held row. Each report also runs against an unmarked queued row, the control showing the
// marker is what refuses it.
func TestPhaseSwitchHoldRefusesLateWorkerReportsLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	worker := fx.worker("w-3252l", wpWorker{})
	wid := pgtype.UUID{Bytes: worker, Valid: true}
	reports := map[string]func(id uuid.UUID) (int64, error){
		"SetRunAwaitingInput": func(id uuid.UUID) (int64, error) {
			return fx.q.SetRunAwaitingInput(fx.ctx, store.SetRunAwaitingInputParams{
				OpenQuestionID: pgtype.Text{String: "q1", Valid: true}, ID: id, WorkerID: wid})
		},
		"SetRunAwaitingFollowup": func(id uuid.UUID) (int64, error) {
			return fx.q.SetRunAwaitingFollowup(fx.ctx, store.SetRunAwaitingFollowupParams{ID: id, WorkerID: wid})
		},
		"SupersedeRunByWorker": func(id uuid.UUID) (int64, error) {
			return fx.q.SupersedeRunByWorker(fx.ctx, store.SupersedeRunByWorkerParams{StopReason: "branch moved", ID: id, WorkerID: wid})
		},
		"SetRunFailed": func(id uuid.UUID) (int64, error) {
			return fx.q.SetRunFailed(fx.ctx, store.SetRunFailedParams{ID: id, WorkerID: wid, FailureReason: pgtype.Text{String: "boom", Valid: true}})
		},
		"SetRunFailedPlanRejected": func(id uuid.UUID) (int64, error) {
			return fx.q.SetRunFailedPlanRejected(fx.ctx, store.SetRunFailedPlanRejectedParams{
				ID: id, WorkerID: wid, FailureReason: pgtype.Text{String: "rejected", Valid: true}})
		},
		"SetRunAwaitingApproval": func(id uuid.UUID) (int64, error) {
			return fx.q.SetRunAwaitingApproval(fx.ctx, store.SetRunAwaitingApprovalParams{
				PlanMd: pgtype.Text{String: "plan", Valid: true}, ID: id, WorkerID: wid})
		},
		"SetRunCompleted": func(id uuid.UUID) (int64, error) {
			return fx.q.SetRunCompleted(fx.ctx, store.SetRunCompletedParams{ID: id, WorkerID: wid})
		},
	}
	for name, report := range reports {
		for _, status := range []string{"paused", "queued"} {
			t.Run(name+"/"+status, func(t *testing.T) {
				held := fx.run(wpRun{status: status, worker: &worker, claimGen: 3, holdReason: phaseSwitchReason, holdCapturedHead: "h"})
				n, err := report(held)
				if err != nil || n != 0 {
					t.Fatalf("%s on a %s phase-switch hold = (%d, %v), want (0, nil)", name, status, n, err)
				}
				run := fx.mustRun(held)
				if run.Status != status || run.HoldReason.String != phaseSwitchReason || run.HoldCapturedHead.String != "h" || run.WorkerID.Bytes != worker {
					t.Fatalf("late report changed the held row: %q %q %q", run.Status, run.HoldReason.String, run.HoldCapturedHead.String)
				}
				if status == "queued" {
					// Control: the same queued row without the marker is moved by the report.
					plain := fx.run(wpRun{status: status, worker: &worker, claimGen: 3})
					if n, err := report(plain); err != nil || n != 1 {
						t.Fatalf("control: %s on an unmarked queued run = (%d, %v), want (1, nil)", name, n, err)
					}
				}
			})
		}
	}
}
