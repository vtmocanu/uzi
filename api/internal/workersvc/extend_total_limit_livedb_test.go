package workersvc

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #2735: extension admission reserves room under the extension-admission limit
// MaxRunWallSeconds, so a total built from admitted extensions (Config.RunTimeoutSeconds =
// coalesce(wall, RUN_TIMEOUT) + extension + finalize) stays at or under it whichever order the owner
// extends, approves the plan (which scales the wall up to the ceiling) and Stops. The limit is not an
// absolute cap on served totals: since #2761 the worker re-arms its wall timer in chunks, so a
// configuration-driven total above it is honored. These tests drive the real service paths and read
// the served value out of the real claim assembly.

// sealClaimSecrets reseals the fixture's forge connection and adds a default Anthropic token so the
// real claim assembly can run (seedCodexInfra stores an undecryptable placeholder).
func (f *wallParkExtendFixture) sealClaimSecrets(t *testing.T) {
	t.Helper()
	botPAT, err := f.env.box.Seal([]byte(codexToken("bot-pat")))
	if err != nil {
		t.Fatalf("seal bot PAT: %v", err)
	}
	f.env.exec(`UPDATE forge_connections SET token_ciphertext = $1 WHERE user_id = $2`, botPAT, f.userID)
	anthropic, err := f.env.box.Seal([]byte(codexToken("anthropic")))
	if err != nil {
		t.Fatalf("seal anthropic token: %v", err)
	}
	f.env.exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	            VALUES ($1, $2, 'anthropic_token', $3, true, $4, 'master')`, uuid.New(), f.userID, "anthropic-"+uuid.NewString(), anthropic)
}

// servedWallSeconds returns Config.RunTimeoutSeconds from the real claim assembly of the run's
// current row: the value the worker arms its wall timer with.
func (f *wallParkExtendFixture) servedWallSeconds(t *testing.T, id uuid.UUID) int {
	t.Helper()
	payload, err := f.svc.assembleClaim(f.env.ctx, store.Worker{ID: f.worker, UserID: f.userID}, mustRun(t, f.env, id))
	if err != nil {
		t.Fatalf("assembleClaim: %v", err)
	}
	return payload.Config.RunTimeoutSeconds
}

// assertTimerInRange fails when the served wall exceeds the extension-admission limit
// MaxRunWallSeconds (2^31-1 ms in whole seconds).
func assertTimerInRange(t *testing.T, served int) {
	t.Helper()
	if int64(served)*1000 > math.MaxInt32 {
		t.Fatalf("served RunTimeoutSeconds = %d: %d ms exceeds MaxInt32 (the worker timer would fire at once)", served, int64(served)*1000)
	}
}

// repark returns an extended (queued) run to a budget_exhausted wall park, as the sweep would.
func (f *wallParkExtendFixture) repark(t *testing.T, id uuid.UUID) {
	t.Helper()
	f.env.exec(`UPDATE runs SET status='paused', hold_reason='budget_exhausted', status_since=now()-interval '30 minutes',
		started_at=now()-interval '90 days' WHERE id=$1`, id)
	f.env.exec(`INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1,'pause','wall')`, id)
}

// milestoneWallPark turns a wall-park run into a milestone issue run with one completed milestone so
// the owner can Stop it.
func (f *wallParkExtendFixture) milestoneWallPark(t *testing.T, id uuid.UUID) {
	t.Helper()
	f.env.exec(`UPDATE runs SET milestones_frozen='[{"id":"m1","title":"one"},{"id":"m2","title":"two"}]'::jsonb,
		milestones_completed='["m1"]'::jsonb WHERE id=$1`, id)
}

func (f *wallParkExtendFixture) mustExtend(t *testing.T, id uuid.UUID, secs string) {
	t.Helper()
	if _, err := f.svc.SubmitInput(f.env.ctx, f.userID, id, "extend", secs, nil); err != nil {
		t.Fatalf("extend %s: %v", secs, err)
	}
}

// stopAndServe Stops a wall-parked milestone run through the service and returns the served wall
// total from the real claim assembly.
func (f *wallParkExtendFixture) stopAndServe(t *testing.T, id uuid.UUID) int {
	t.Helper()
	f.milestoneWallPark(t, id)
	if _, err := f.svc.SubmitInput(f.env.ctx, f.userID, id, "stop", "", nil); err != nil {
		t.Fatalf("stop: %v", err)
	}
	run := mustRun(t, f.env, id)
	if run.BudgetFinalizeSeconds != WallParkFinalizeSeconds {
		t.Fatalf("budget_finalize_seconds after Stop = %d, want %d", run.BudgetFinalizeSeconds, WallParkFinalizeSeconds)
	}
	return f.servedWallSeconds(t, id)
}

// (a) A NULL-wall run at the plan gate is extended to fill its room, then approved with a
// multi-milestone plan: the approve scales the wall up to the ceiling. The reserve for that scale-up
// made the extension stop short, so the served total stays within the extension-admission limit.
func TestExtendThenApproveScaledWallStaysInTimerRangeLiveDB(t *testing.T) {
	f := newWallParkExtendFixture(t, 604800)
	f.sealClaimSecrets(t)
	f.svc.SetTxBeginner(f.env.pool)
	f.svc.SetBackground(func(func()) {})
	id := uuid.New()
	f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id,
		milestones_candidate, claim_generation)
		VALUES ($1,$2,$3,'issue',1,'t','d','awaiting_approval',$4,
		'[{"id":"a","title":"A"},{"id":"b","title":"B"},{"id":"c","title":"C"},{"id":"d","title":"D"},{"id":"e","title":"E"},{"id":"f","title":"F"}]'::jsonb,1)`,
		id, f.userID, f.repoID, f.worker)

	// testParams: RunTimeout 2h, RunWallCeiling 8h. The room reserves the 72h maximum ceiling:
	// 2147483 - 259200 - 1800 = 1886483 = 3 x 604800 + 72083. The first extend of the full cap
	// leaves the run NULL-walled.
	f.mustExtend(t, id, "604800")
	f.mustExtend(t, id, "604800")
	f.mustExtend(t, id, "604800")
	f.mustExtend(t, id, "72083")
	if _, err := f.svc.SubmitInput(f.env.ctx, f.userID, id, "extend", "60", nil); !errors.Is(err, ErrExtensionLimitReached) {
		t.Fatalf("extend past the room err = %v, want ErrExtensionLimitReached", err)
	}
	if run := mustRun(t, f.env, id); run.BudgetWallSeconds.Valid {
		t.Fatalf("wall = %v before approve, want NULL (the case under test)", run.BudgetWallSeconds)
	}

	if _, err := f.svc.SubmitInputWithOptions(f.env.ctx, f.userID, id, "approve_plan", "", &AgentSelection{Source: AgentSourceOwn}, SubmitInputOptions{}); err != nil {
		t.Fatalf("approve_plan: %v", err)
	}
	run := mustRun(t, f.env, id)
	if !run.BudgetWallSeconds.Valid || run.BudgetWallSeconds.Int32 != 28800 {
		t.Fatalf("wall after approve = %v, want the 28800s ceiling (a multi-milestone plan scales to it)", run.BudgetWallSeconds)
	}
	served := f.servedWallSeconds(t, id)
	assertTimerInRange(t, served)
	if want := 28800 + 1886483; served != want {
		t.Fatalf("served = %d, want %d (scaled wall + extension)", served, want)
	}
}

// (b) A run extended to fill its room, wall-parked and Stopped: the one-time finalize allowance Stop
// adds was reserved up front, so the served total stays within the extension-admission limit.
func TestExtendFillThenWallParkStopStaysInTimerRangeLiveDB(t *testing.T) {
	f := newWallParkExtendFixture(t, 604800)
	f.sealClaimSecrets(t)
	// Wall 3600 (fixture), RunTimeout 2h / ceiling 8h: room = 1886483. Seed 3 x 604800 already granted.
	id := f.wallPark(t, 3*604800)
	f.mustExtend(t, id, "72083") // exactly fills the room; resumes the run
	f.repark(t, id)
	if _, err := f.svc.SubmitInput(f.env.ctx, f.userID, id, "extend", "60", nil); !errors.Is(err, ErrExtensionLimitReached) {
		t.Fatalf("extend past the room err = %v, want ErrExtensionLimitReached", err)
	}

	served := f.stopAndServe(t, id)
	assertTimerInRange(t, served)
	if want := 3600 + 1886483 + 1800; served != want {
		t.Fatalf("served = %d, want %d (wall + extension + finalize)", served, want)
	}
}

// (c) A persisted wall (72h, e.g. from a larger ceiling at freeze time) above the current service
// ceiling (24h): the room is computed from the persisted 72h, +60 beyond it is refused, and after a
// wall park and Stop the served total is still within the extension-admission limit.
func TestPersistedWallAboveCeilingRoomAndStopLiveDB(t *testing.T) {
	f := newWallParkExtendFixture(t, 604800)
	f.sealClaimSecrets(t)
	f.svc.p.RunTimeout = 6 * time.Hour
	f.svc.p.RunWallCeiling = 24 * time.Hour
	id := f.wallPark(t, 3*604800)
	f.env.exec(`UPDATE runs SET budget_wall_seconds = 259200 WHERE id=$1`, id)

	// Room = 2147483 - 259200 (the persisted 72h, not the 24h ceiling) - 1800 - 1814400 = 72083.
	if _, err := f.svc.SubmitInput(f.env.ctx, f.userID, id, "extend", "72143", nil); !errors.Is(err, ErrExtensionLimitReached) {
		t.Fatalf("extend 60s beyond the room err = %v, want ErrExtensionLimitReached", err)
	}
	if run := mustRun(t, f.env, id); run.Status != "paused" || run.BudgetExtensionSeconds != 3*604800 {
		t.Fatalf("refused extend changed the run: %s ext=%d", run.Status, run.BudgetExtensionSeconds)
	}
	f.mustExtend(t, id, "72083")
	f.repark(t, id)
	if _, err := f.svc.SubmitInput(f.env.ctx, f.userID, id, "extend", "60", nil); !errors.Is(err, ErrExtensionLimitReached) {
		t.Fatalf("extend after filling err = %v, want ErrExtensionLimitReached", err)
	}

	served := f.stopAndServe(t, id)
	assertTimerInRange(t, served)
	if want := 259200 + 1886483 + 1800; served != want || want != MaxRunWallSeconds {
		t.Fatalf("served = %d, want %d == MaxRunWallSeconds %d", served, want, MaxRunWallSeconds)
	}
}

// Issue #2735 review finding: a NULL-wall run is extended to fill its room under a 8h ceiling,
// then the operator raises RUN_WALL_CEILING (to the 72h maximum) before the plan freezes its wall.
// The room reserved MaxRunWallCeiling up front, so the larger scaled wall stays within the
// extension-admission limit whichever approval path freezes it: the owner's approve_plan input, or an autopilot run's own
// self-contained running report.
func TestExtendThenCeilingRaisedThenApproveStaysInTimerRangeLiveDB(t *testing.T) {
	const sixMilestones = `[{"id":"a","title":"A"},{"id":"b","title":"B"},{"id":"c","title":"C"},{"id":"d","title":"D"},{"id":"e","title":"E"},{"id":"f","title":"F"}]`
	for _, path := range []string{"approve_plan", "autopilot_running_report"} {
		t.Run(path, func(t *testing.T) {
			f := newWallParkExtendFixture(t, 604800)
			f.sealClaimSecrets(t)
			f.svc.SetTxBeginner(f.env.pool)
			f.svc.SetBackground(func(func()) {})
			id := uuid.New()
			if path == "approve_plan" {
				f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id,
					milestones_candidate, claim_generation)
					VALUES ($1,$2,$3,'issue',1,'t','d','awaiting_approval',$4,$5::jsonb,1)`, id, f.userID, f.repoID, f.worker, sixMilestones)
			} else {
				f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id,
					auto_approve, plan_source, milestones_candidate, claim_generation, started_at)
					VALUES ($1,$2,$3,'issue',1,'t','d','running',$4,true,'agent',$5::jsonb,1,now())`, id, f.userID, f.repoID, f.worker, sixMilestones)
			}

			// testParams: RunTimeout 2h, ceiling 8h; the reserved room is 1886483 s.
			f.mustExtend(t, id, "604800")
			f.mustExtend(t, id, "604800")
			f.mustExtend(t, id, "604800")
			f.mustExtend(t, id, "72083")
			if _, err := f.svc.SubmitInput(f.env.ctx, f.userID, id, "extend", "60", nil); !errors.Is(err, ErrExtensionLimitReached) {
				t.Fatalf("extend past the room err = %v, want ErrExtensionLimitReached", err)
			}
			if run := mustRun(t, f.env, id); run.BudgetWallSeconds.Valid {
				t.Fatalf("wall = %v before the freeze, want NULL (the case under test)", run.BudgetWallSeconds)
			}

			// The operator raises only RUN_WALL_CEILING (72h is its validated maximum) and restarts.
			f.svc.p.RunWallCeiling = 72 * time.Hour
			if _, err := f.svc.SubmitInput(f.env.ctx, f.userID, id, "extend", "60", nil); !errors.Is(err, ErrExtensionLimitReached) {
				t.Fatalf("extend after the ceiling raise err = %v, want ErrExtensionLimitReached (room already reserved 72h)", err)
			}

			if path == "approve_plan" {
				if _, err := f.svc.SubmitInputWithOptions(f.env.ctx, f.userID, id, "approve_plan", "", &AgentSelection{Source: AgentSourceOwn}, SubmitInputOptions{}); err != nil {
					t.Fatalf("approve_plan: %v", err)
				}
			} else {
				gen := int64(1)
				if _, applied, err := f.svc.SetState(f.env.ctx, store.Worker{ID: f.worker, UserID: f.userID}, id, StateRequest{State: "running", ClaimGeneration: &gen}); err != nil || !applied {
					t.Fatalf("running report applied=%v err=%v", applied, err)
				}
			}
			run := mustRun(t, f.env, id)
			if !run.BudgetWallSeconds.Valid || run.BudgetWallSeconds.Int32 != 43200 {
				t.Fatalf("wall after the freeze = %v, want 43200 (2h x 6 milestones, under the raised 72h ceiling)", run.BudgetWallSeconds)
			}
			served := f.servedWallSeconds(t, id)
			assertTimerInRange(t, served)
			if want := 43200 + 1886483; served != want {
				t.Fatalf("served = %d, want %d (scaled wall + extension)", served, want)
			}
		})
	}
}
