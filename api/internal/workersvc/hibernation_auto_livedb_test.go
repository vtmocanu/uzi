package workersvc

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4207: automatic hibernation of idle workers over a real database. Skipped without
// UZI_TEST_DATABASE_URL (run via ./e2e/run-store-it.sh).
//
// The service clock is injected and fixed, so every worker and run timestamp below is written
// relative to it; the controller advertisement is re-stamped whenever the clock moves.

type autoSettings struct {
	enabled, auto bool
	idle          time.Duration
	err           error
}

func (a *autoSettings) WorkerHibernationEnabled(context.Context) (bool, error) {
	return a.enabled, a.err
}
func (a *autoSettings) WorkerHibernationAuto(context.Context) (bool, error) { return a.auto, a.err }
func (a *autoSettings) WorkerHibernationIdleAfter(context.Context) (time.Duration, error) {
	return a.idle, a.err
}

func (a *autoSettings) WorkerHibernationSurplus(context.Context) (bool, error) { return false, nil }
func (a *autoSettings) WorkerHibernationSurplusIdleAfter(context.Context) (time.Duration, error) {
	return 2 * time.Hour, nil
}

const autoIdleAfter = 336 * time.Hour

type autoFx struct {
	*hibFx
	set   *autoSettings
	clock time.Time
}

func newAutoFx(t *testing.T) *autoFx {
	t.Helper()
	f := newHibFx(t)
	a := &autoFx{hibFx: f, set: &autoSettings{enabled: true, auto: true, idle: autoIdleAfter},
		clock: time.Now().UTC().Truncate(time.Second)}
	f.svc.SetHibernationSettings(a.set)
	f.svc.SetHibernationAutoSettings(a.set)
	f.svc.SetClock(func() time.Time { return a.clock })
	a.advertiseNow()
	return a
}

func (a *autoFx) advertiseNow() {
	a.t.Helper()
	err := a.env.q.UpsertControllerHibernationAdvertisement(a.env.ctx, store.UpsertControllerHibernationAdvertisementParams{
		AdvertisedAt:        pgtype.Timestamptz{Time: a.clock, Valid: true},
		HibernationVersion:  pgtype.Int4{Int32: 1, Valid: true},
		PollIntervalSeconds: pgtype.Int4{Int32: 300, Valid: true},
	})
	if err != nil {
		a.t.Fatalf("advertise: %v", err)
	}
}

// advance moves the clock and keeps the controller advertisement fresh.
func (a *autoFx) advance(d time.Duration) {
	a.clock = a.clock.Add(d)
	a.advertiseNow()
}

// idle seeds a persistent worker whose only activity anchor (created_at) is `ago` before the clock.
func (a *autoFx) idle(ago time.Duration) (uuid.UUID, uuid.UUID) {
	a.t.Helper()
	id, pod := a.worker()
	a.env.exec(`UPDATE workers SET created_at = $2, last_heartbeat_at = $3 WHERE id = $1`, id, a.clock.Add(-ago), a.clock)
	return id, pod
}

func (a *autoFx) runPass() (int64, error) { return a.svc.HibernationAutoPass(a.env.ctx) }

// next waits out the pacing gap and runs a pass.
func (a *autoFx) next() (int64, error) {
	a.advance(2 * time.Minute)
	return a.runPass()
}

func (a *autoFx) mustPass(want int64) {
	a.t.Helper()
	got, err := a.runPass()
	if err != nil || got != want {
		a.t.Fatalf("pass = %d, %v; want %d, nil", got, err, want)
	}
}

func (a *autoFx) mustNext(want int64) {
	a.t.Helper()
	got, err := a.next()
	if err != nil || got != want {
		a.t.Fatalf("pass = %d, %v; want %d, nil", got, err, want)
	}
}

// wantUntouched fails if the row differs from before: a skipped worker must produce no write.
func (a *autoFx) wantUntouched(id uuid.UUID, before store.Worker) {
	a.t.Helper()
	if after := a.row(id); !reflect.DeepEqual(before, after) {
		a.t.Fatalf("a skipped worker was written:\n before %+v\n after  %+v", before, after)
	}
}

// terminalTarget seeds an unassigned completed issue run, the target some kinds require.
func (a *autoFx) terminalTarget() uuid.UUID {
	a.t.Helper()
	id := uuid.New()
	a.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, finished_at)
	            VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'completed', $5)`, id, a.user, a.repo, nextOutageIID(), a.clock.Add(-1000*time.Hour))
	return id
}

// runOf inserts one run of the given kind and status assigned to worker (nil = unassigned). The
// extra columns each kind's runs_kind_shape constraint requires are supplied here.
func (a *autoFx) runOf(kind, status string, worker *uuid.UUID) uuid.UUID {
	a.t.Helper()
	id := uuid.New()
	cols := []string{"id", "user_id", "kind", "issue_title", "issue_description", "status", "worker_id"}
	var wid any
	if worker != nil {
		wid = *worker
	}
	args := []any{id, a.user, kind, "t", "d", status, wid}
	add := func(col string, v any) {
		cols = append(cols, col)
		args = append(args, v)
	}
	repo := func() { add("repo_id", a.repo) }
	switch kind {
	case runkind.Issue, runkind.SelfImprove:
		repo()
		add("issue_iid", nextOutageIID())
	case runkind.CIFix:
		repo()
		add("pipeline_id", nextOutageIID())
		add("pipeline_ref", "main")
	case runkind.Prompt:
		repo()
	case runkind.Task:
		repo()
		add("branch", "uzi/task/"+id.String())
	case runkind.MRRework:
		repo()
		add("pipeline_ref", "agent/issue-1")
		add("mr_iid", nextOutageIID())
		add("target_run_id", a.terminalTarget())
	case runkind.Judge:
		add("target_run_id", a.terminalTarget())
	case runkind.Job:
		add("job_type", "research")
	case runkind.CrossCheck:
		repo()
		add("target_run_id", a.terminalTarget())
		add("harness", "codex")
		add("report_only", true)
		add("budget_wall_seconds", 3600)
	}
	ph := make([]string, len(cols))
	for i := range ph {
		ph[i] = fmt.Sprintf("$%d", i+1)
	}
	a.env.exec(`INSERT INTO runs (`+strings.Join(cols, ",")+`) VALUES (`+strings.Join(ph, ",")+`)`, args...)
	return id
}

// finish ends a run at `at` (terminal status, finished_at set).
func (a *autoFx) finish(run uuid.UUID, status string, at time.Time) {
	a.t.Helper()
	a.env.exec(`UPDATE runs SET status = $2, finished_at = $3, status_since = $3 WHERE id = $1`, run, status, at)
}

func (a *autoFx) idleness(id uuid.UUID) store.GetWorkerIdlenessRow {
	a.t.Helper()
	row, err := a.env.q.GetWorkerIdleness(a.env.ctx, id)
	if err != nil {
		a.t.Fatalf("GetWorkerIdleness: %v", err)
	}
	return row
}

func wantNotBlocked(t *testing.T, w store.Worker) {
	t.Helper()
	if w.HibernationBlockedReason.Valid || w.HibernationBlockedAt.Valid || w.HibernationBlockedSince.Valid {
		t.Fatalf("blocked columns set: %+v", w)
	}
}

// L1, L2, L4, L11: every run kind and every non-terminal status keeps a worker busy; a run that
// ended while another is still assigned does not make it idle; a requeued run counts against
// its new worker; a run that finished just now is activity.
func TestHibernationAutoIdlenessLiveDB(t *testing.T) {
	a := newAutoFx(t)
	// Keep threshold jumps local to this fixture instead of aging shared worker history.
	a.clock = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	a.advertiseNow()
	t.Run("recent foreign worker history does not displace the idle target", func(t *testing.T) {
		foreign := newHibFx(t)
		foreignAnchor := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
		// Fill a candidate batch with older anchors if the fixture uses wall time.
		for i := 0; i < HibernationAutoAdmissionsPerPass; i++ {
			id, _ := foreign.worker()
			foreign.env.exec(`UPDATE workers SET created_at = $2, manual_cordoned_at = $2,
				manual_cordoned_by = $3, manual_cordoned_by_label = 'fixture' WHERE id = $1`,
				id, foreignAnchor, foreign.user)
		}
		id, _ := a.idle(400 * 24 * time.Hour)
		run := a.runOf(runkind.Issue, "running", &id)
		a.finish(run, "completed", a.clock)
		a.advance(autoIdleAfter)
		a.mustPass(1)
		a.want(id, "preparing")
	})
	t.Run("every run kind keeps the worker busy", func(t *testing.T) {
		id, _ := a.idle(400 * 24 * time.Hour)
		for _, kind := range runkind.All() {
			run := a.runOf(kind, "running", &id)
			if got := a.idleness(id); got.Nonterminal != 1 {
				t.Fatalf("%s: nonterminal = %d, want 1", kind, got.Nonterminal)
			}
			before := a.row(id)
			a.mustNext(0)
			a.wantUntouched(id, before)
			a.env.exec(`DELETE FROM runs WHERE id = $1`, run)
		}
		for _, status := range []string{"queued", "claimed", "running", "awaiting_approval", "awaiting_input",
			"limit_wait", "awaiting_followup", "pool_wait", "paused"} {
			run := a.runOf(runkind.Issue, status, &id)
			before := a.row(id)
			a.mustNext(0)
			a.wantUntouched(id, before)
			a.env.exec(`DELETE FROM runs WHERE id = $1`, run)
		}
		// Control: with no run left, the same worker is admitted.
		a.mustNext(1)
		a.want(id, "preparing")
	})

	t.Run("one run ends while another is still assigned", func(t *testing.T) {
		id, _ := a.idle(400 * 24 * time.Hour)
		ended := a.runOf(runkind.Issue, "running", &id)
		a.runOf(runkind.Chat, "awaiting_input", &id)
		a.finish(ended, "completed", a.clock.Add(-400*24*time.Hour))
		if got := a.idleness(id); got.Nonterminal != 1 {
			t.Fatalf("nonterminal = %d, want 1", got.Nonterminal)
		}
		before := a.row(id)
		a.mustNext(0)
		a.wantUntouched(id, before)
	})

	t.Run("a requeued run counts against its new worker", func(t *testing.T) {
		oldW, _ := a.idle(400 * 24 * time.Hour)
		newW, _ := a.idle(400 * 24 * time.Hour)
		run := a.runOf(runkind.Issue, "running", &oldW)
		a.env.exec(`UPDATE runs SET worker_id = $2 WHERE id = $1`, run, newW)
		if a.idleness(oldW).Nonterminal != 0 || a.idleness(newW).Nonterminal != 1 {
			t.Fatal("the run still counts against the old worker")
		}
		a.mustNext(1)
		a.want(oldW, "preparing")
		a.want(newW, "none")
	})

	t.Run("a run that finished just now is activity", func(t *testing.T) {
		for _, c := range []struct {
			name   string
			finish func(id uuid.UUID, run uuid.UUID)
		}{
			{"finished_at", func(_, run uuid.UUID) { a.finish(run, "completed", a.clock) }},
			{"no finished_at falls back to status_since", func(_, run uuid.UUID) {
				a.env.exec(`UPDATE runs SET status = 'failed', finished_at = NULL, status_since = $2 WHERE id = $1`, run, a.clock)
			}},
		} {
			id, _ := a.idle(400 * 24 * time.Hour)
			run := a.runOf(runkind.Issue, "running", &id)
			c.finish(id, run)
			got := a.idleness(id)
			if got.Nonterminal != 0 || !got.Anchor.Valid || !got.Anchor.Time.Equal(a.clock) {
				t.Fatalf("%s: idleness = %+v, want 0 and anchor %v", c.name, got, a.clock)
			}
			before := a.row(id)
			a.mustNext(0)
			a.wantUntouched(id, before)
			// Idle again once the threshold has passed since that finish.
			a.advance(autoIdleAfter)
			a.mustPass(1)
			a.want(id, "preparing")
		}
	})
}

// L3: a settled wake restarts the idle clock. Without the grace stamp the worker, whose own
// anchor is 400 days old, would be re-hibernated by the very next pass.
func TestHibernationAutoSettledWakeResetsClockLiveDB(t *testing.T) {
	a := newAutoFx(t)
	id, pod := a.idle(400 * 24 * time.Hour)
	a.fullCycle(id, pod) // manual hibernate, wake and settle at the fixed clock
	row := a.row(id)
	if row.HibernationState != "none" || !row.LastWakeSettledAt.Valid || !row.LastWakeSettledAt.Time.Equal(a.clock) {
		t.Fatalf("settle did not stamp the grace anchor: state=%q settled=%v", row.HibernationState, row.LastWakeSettledAt)
	}
	a.mustNext(0)
	a.want(id, "none")
	a.advance(autoIdleAfter - 10*time.Minute)
	a.mustPass(0)
	a.advance(11 * time.Minute)
	a.mustPass(1)
	a.want(id, "preparing")
}

// L5, L9: blocked workers sort last and cannot starve a clean one; one pass admits at most ten.
func TestHibernationAutoCandidateOrderingLiveDB(t *testing.T) {
	t.Run("twelve recently blocked workers do not starve a clean one", func(t *testing.T) {
		a := newAutoFx(t)
		var blocked []uuid.UUID
		for i := 0; i < 12; i++ {
			id, _ := a.idle(500 * 24 * time.Hour)
			a.env.exec(`UPDATE workers SET manual_cordoned_at = $2, manual_cordoned_by = $3, manual_cordoned_by_label = 'a',
				hibernation_blocked_reason = 'manual_cordon', hibernation_blocked_at = $4, hibernation_blocked_since = $4 WHERE id = $1`,
				id, a.clock.Add(-time.Hour), a.user, a.clock.Add(-time.Duration(i+1)*time.Minute))
			blocked = append(blocked, id)
		}
		clean, _ := a.idle(100 * 24 * time.Hour) // the youngest anchor: only the ordering puts it in the first ten
		a.mustPass(1)
		a.want(clean, "preparing")
		recorded := 0
		for _, id := range blocked {
			if a.row(id).HibernationBlockedAt.Time.Equal(a.clock) {
				recorded++
			}
		}
		if recorded != HibernationAutoAdmissionsPerPass-1 {
			t.Fatalf("%d blocked workers were retried, want %d", recorded, HibernationAutoAdmissionsPerPass-1)
		}
	})

	t.Run("fifteen idle workers: ten per pass", func(t *testing.T) {
		a := newAutoFx(t)
		var ids []uuid.UUID
		for i := 0; i < 15; i++ {
			id, _ := a.idle(time.Duration(400+i) * 24 * time.Hour)
			ids = append(ids, id)
		}
		a.mustPass(HibernationAutoAdmissionsPerPass)
		a.mustNext(5)
		prep := 0
		for _, id := range ids {
			if a.state(id) == "preparing" {
				prep++
			}
		}
		if prep != 15 {
			t.Fatalf("%d of 15 admitted", prep)
		}
	})

	t.Run("threshold plus one minute is admitted, minus one minute is not", func(t *testing.T) {
		a := newAutoFx(t)
		over, _ := a.idle(autoIdleAfter + time.Minute)
		under, _ := a.idle(autoIdleAfter - time.Minute)
		a.mustPass(1)
		a.want(over, "preparing")
		a.want(under, "none")
	})
}

// L7: the pass needs both settings, reports a settings read error without admitting, and runs at
// most once a minute.
func TestHibernationAutoSettingsAndPacingLiveDB(t *testing.T) {
	a := newAutoFx(t)
	w1, _ := a.idle(400 * 24 * time.Hour)
	for name, set := range map[string]autoSettings{
		"enabled off": {enabled: false, auto: true, idle: autoIdleAfter},
		"auto off":    {enabled: true, auto: false, idle: autoIdleAfter},
	} {
		*a.set = set
		before := a.row(w1)
		if n, err := a.next(); err != nil || n != 0 {
			t.Fatalf("%s: pass = %d, %v", name, n, err)
		}
		a.wantUntouched(w1, before)
	}
	*a.set = autoSettings{enabled: true, auto: true, idle: autoIdleAfter, err: errors.New("settings unavailable")}
	if n, err := a.next(); err == nil || n != 0 {
		t.Fatalf("read error: pass = %d, %v; want 0 and an error", n, err)
	}
	a.want(w1, "none")
	*a.set = autoSettings{enabled: true, auto: true, idle: autoIdleAfter}

	// A pass that did nothing because a setting was off did not use up the minute: this one runs.
	a.mustPass(1)
	a.want(w1, "preparing")
	// Within a minute of the last pass that ran, another is refused outright.
	w2, _ := a.idle(400 * 24 * time.Hour)
	a.advance(59 * time.Second)
	a.mustPass(0)
	a.want(w2, "none")
	a.advance(time.Second)
	a.mustPass(1)
	a.want(w2, "preparing")

	// Without a wired settings reader the pass is inert.
	svc := New(a.env.q, a.env.box, testParams())
	svc.SetTxBeginner(a.env.pool)
	if n, err := svc.HibernationAutoPass(a.env.ctx); n != 0 || err != nil {
		t.Fatalf("unwired pass = %d, %v", n, err)
	}
}

// L8: a refusal is recorded with its refined reason and committed, the since time restarts only
// when the reason changes, and the markers are cleared when the worker is finally admitted.
func TestHibernationAutoRefusalRecordedLiveDB(t *testing.T) {
	a := newAutoFx(t)
	cases := []struct {
		name, want string
		prep, undo string
	}{
		{"manual cordon", BlockedManualCordon,
			`UPDATE workers SET manual_cordoned_at = now(), manual_cordoned_by = $2, manual_cordoned_by_label = 'a' WHERE id = $1`,
			`UPDATE workers SET manual_cordoned_at = NULL, manual_cordoned_by = NULL, manual_cordoned_by_label = NULL WHERE id = $1`},
		{"maintenance fence", BlockedMaintenance,
			`UPDATE workers SET maintenance_fenced = true WHERE id = $1 AND $2::uuid IS NOT NULL`,
			`UPDATE workers SET maintenance_fenced = false WHERE id = $1`},
		{"image roll", BlockedRoll,
			`UPDATE workers SET draining_since = now(), draining_reason = 'image_roll' WHERE id = $1 AND $2::uuid IS NOT NULL`,
			`UPDATE workers SET draining_since = NULL, draining_reason = NULL WHERE id = $1`},
		{"draining", BlockedDraining,
			`UPDATE workers SET draining_since = now() WHERE id = $1 AND $2::uuid IS NOT NULL`,
			`UPDATE workers SET draining_since = NULL, draining_reason = NULL WHERE id = $1`},
		{"no pod identity", BlockedNoPodIdentity,
			`UPDATE workers SET registered_pod_uid = NULL WHERE id = $1 AND $2::uuid IS NOT NULL`,
			`UPDATE workers SET registered_pod_uid = gen_random_uuid() WHERE id = $1`},
	}
	closed := map[string]bool{}
	for _, r := range HibernationBlockedReasons() {
		closed[r] = true
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, _ := a.idle(400 * 24 * time.Hour)
			a.env.exec(c.prep, id, a.user)
			a.mustNext(0)
			row := a.row(id)
			if row.HibernationState != "none" || row.HibernationBlockedReason.String != c.want ||
				!row.HibernationBlockedAt.Time.Equal(a.clock) || !row.HibernationBlockedSince.Time.Equal(a.clock) {
				t.Fatalf("after refusal: state=%q reason=%q at=%v since=%v", row.HibernationState,
					row.HibernationBlockedReason.String, row.HibernationBlockedAt.Time, row.HibernationBlockedSince.Time)
			}
			if !closed[row.HibernationBlockedReason.String] {
				t.Fatalf("reason %q is outside the closed set", row.HibernationBlockedReason.String)
			}
			first := a.clock
			// The same reason again moves the sighting, not the since time.
			a.mustNext(0)
			row = a.row(id)
			if !row.HibernationBlockedAt.Time.Equal(a.clock) || !row.HibernationBlockedSince.Time.Equal(first) {
				t.Fatalf("same reason: at=%v since=%v, want at=%v since=%v", row.HibernationBlockedAt.Time, row.HibernationBlockedSince.Time, a.clock, first)
			}
			// Cleared: admitted with the automatic reason, and the markers are gone.
			a.env.exec(c.undo, id)
			a.mustNext(1)
			row = a.row(id)
			wantNotBlocked(t, row)
			if row.HibernationState != "preparing" || row.HibernationReason.String != "automatic: no run activity for 14d" {
				t.Fatalf("admitted row: state=%q reason=%q", row.HibernationState, row.HibernationReason.String)
			}
		})
	}

	t.Run("custody hold, then a changed reason restarts since", func(t *testing.T) {
		id, _ := a.idle(400 * 24 * time.Hour)
		run := uuid.New()
		a.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, claim_generation, finished_at)
		            VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'completed', 1, $5)`, run, a.user, a.repo, nextOutageIID(), a.clock.Add(-1000*time.Hour))
		hold := uuid.New()
		a.env.exec(`INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
		            VALUES($1,$2,$3,$4,1,'open',$5,'ident',$5,$4,true)`, hold, a.user, a.repo, run, id)
		a.mustNext(0)
		row := a.row(id)
		if row.HibernationBlockedReason.String != BlockedCustodyHold {
			t.Fatalf("reason = %q, want custody_hold", row.HibernationBlockedReason.String)
		}
		a.env.exec(`DELETE FROM recovery_custody_holds WHERE id = $1`, hold)
		a.env.exec(`UPDATE workers SET manual_cordoned_at = now(), manual_cordoned_by = $2, manual_cordoned_by_label = 'a' WHERE id = $1`, id, a.user)
		a.mustNext(0)
		row = a.row(id)
		if row.HibernationBlockedReason.String != BlockedManualCordon || !row.HibernationBlockedSince.Time.Equal(a.clock) {
			t.Fatalf("changed reason: %q since=%v, want manual_cordon since=%v", row.HibernationBlockedReason.String, row.HibernationBlockedSince.Time, a.clock)
		}
	})

	t.Run("controller not advertising", func(t *testing.T) {
		id, _ := a.idle(400 * 24 * time.Hour)
		a.advance(2 * time.Minute)
		a.env.exec(`UPDATE controller_report_status SET hibernation_version = 0`)
		a.mustPass(0)
		if got := a.row(id).HibernationBlockedReason.String; got != BlockedControllerNotAdvertise {
			t.Fatalf("reason = %q", got)
		}
		a.advertiseNow()
		a.mustNext(1)
		wantNotBlocked(t, a.row(id))
	})
}

// L10: a controller refusal before any scale-down restores none, records controller_refused and
// keeps the worker out of the candidate list for the cooldown; a refusal after the scale-down
// wakes the worker and its settle stamps the grace anchor.
func TestHibernationAutoControllerRefusalLiveDB(t *testing.T) {
	t.Run("refusal before scale-down", func(t *testing.T) {
		a := newAutoFx(t)
		id, pod := a.idle(400 * 24 * time.Hour)
		a.mustPass(1)
		a.want(id, "preparing")
		refusedAt := a.clock
		a.mustApply(id, TransitionRefused, pod)
		row := a.row(id)
		if row.HibernationState != "none" || row.HibernationBlockedReason.String != BlockedControllerRefused ||
			!row.HibernationBlockedAt.Time.Equal(refusedAt) || !row.HibernationBlockedSince.Time.Equal(refusedAt) ||
			row.LastWakeSettledAt.Valid {
			t.Fatalf("after refusal: %+v", row)
		}
		a.advance(59*time.Minute - time.Second)
		a.mustPass(0)
		wantUntouchedRow := a.row(id)
		if !wantUntouchedRow.HibernationBlockedAt.Time.Equal(refusedAt) {
			t.Fatal("an excluded worker was written")
		}
		a.advance(2 * time.Second) // 59m01s since the refusal: still inside the cooldown
		a.mustPass(0)
		a.advance(2 * time.Minute) // 61m01s
		a.mustPass(1)
		a.want(id, "preparing")
		wantNotBlocked(t, a.row(id))
	})

	t.Run("refusal after scale-down wakes the worker and the settle starts the grace period", func(t *testing.T) {
		a := newAutoFx(t)
		id, pod := a.idle(400 * 24 * time.Hour)
		a.mustPass(1)
		a.mustApply(id, TransitionScalingDown, pod)
		a.mustApply(id, TransitionRefused, pod)
		row := a.row(id)
		if row.HibernationState != "waking" || row.HibernationBlockedReason.Valid || row.LastWakeSettledAt.Valid {
			t.Fatalf("after late refusal: %+v", row)
		}
		next := uuid.New()
		a.mustApply(id, TransitionReady, next)
		a.env.exec(`UPDATE workers SET registered_pod_uid = $2, last_heartbeat_at = $3 WHERE id = $1`, id, next, a.clock)
		a.register(id, &next)
		row = a.row(id)
		if row.HibernationState != "none" || !row.LastWakeSettledAt.Time.Equal(a.clock) {
			t.Fatalf("after settle: state=%q settled=%v", row.HibernationState, row.LastWakeSettledAt)
		}
		a.mustNext(0)
		a.want(id, "none")
	})
}

// L13: an automatic admission writes exactly what the manual one does, except the reason, and the
// next pass leaves both alone.
func TestHibernationAutoMatchesManualAdmissionLiveDB(t *testing.T) {
	a := newAutoFx(t)
	mk := func() uuid.UUID {
		id, _ := a.dockerWorker()
		a.env.exec(`UPDATE workers SET created_at = $2, last_heartbeat_at = $3 WHERE id = $1`, id, a.clock.Add(-400*24*time.Hour), a.clock)
		return id
	}
	manual, auto := mk(), mk()
	// The manual path also clears stale blocked markers.
	a.env.exec(`UPDATE workers SET hibernation_blocked_reason = 'custody_hold', hibernation_blocked_at = $2, hibernation_blocked_since = $2 WHERE id = $1`, manual, a.clock.Add(-time.Hour))
	a.hibernate(manual)
	a.mustPass(1)
	m, u := a.row(manual), a.row(auto)
	wantNotBlocked(t, m)
	wantNotBlocked(t, u)
	if m.HibernationReason.Valid || u.HibernationReason.String != "automatic: no run activity for 14d" {
		t.Fatalf("reasons: manual=%v auto=%q", m.HibernationReason, u.HibernationReason.String)
	}
	norm := func(w store.Worker) store.Worker {
		w.ID, w.Name, w.TokenHash = uuid.Nil, "", nil
		w.HibernationOpID, w.HibernationAdmissionPodUid, w.RegisteredPodUid = pgtype.UUID{Bytes: [16]byte{1}, Valid: w.HibernationOpID.Valid}, pgtype.UUID{Bytes: [16]byte{1}, Valid: w.HibernationAdmissionPodUid.Valid}, pgtype.UUID{}
		w.CleanupOpID = pgtype.UUID{Bytes: [16]byte{1}, Valid: w.CleanupOpID.Valid}
		// Origin markers are op-bound; the admit origin differs by design (asserted below).
		w.HibernationOriginOpID = pgtype.UUID{Bytes: [16]byte{1}, Valid: w.HibernationOriginOpID.Valid}
		w.HibernationAdmitOrigin = pgtype.Text{}
		w.CleanupRegisterNonce, w.SnapshotRegisterNonce = pgtype.Text{}, pgtype.Text{}
		w.HibernationReason = pgtype.Text{}
		return w
	}
	if m.HibernationAdmitOrigin.String != AdmitOriginManual || u.HibernationAdmitOrigin.String != AdmitOriginAutoLong ||
		!originMarkersValid(&m) || !originMarkersValid(&u) {
		t.Fatalf("admit origins: manual=%v auto=%v", m.HibernationAdmitOrigin, u.HibernationAdmitOrigin)
	}
	mn, un := norm(m), norm(u)
	mv, uv := reflect.ValueOf(mn), reflect.ValueOf(un)
	for i := 0; i < mv.NumField(); i++ {
		name := mv.Type().Field(i).Name
		if !strings.HasPrefix(name, "Hibernat") && !strings.HasPrefix(name, "Wake") && !strings.HasPrefix(name, "Cleanup") &&
			!strings.HasPrefix(name, "LastWake") {
			continue // identity, heartbeat and defaulted-clock columns differ by construction
		}
		if !reflect.DeepEqual(mv.Field(i).Interface(), uv.Field(i).Interface()) {
			t.Fatalf("admissions differ beyond the reason in %s: manual %+v, auto %+v", name, mv.Field(i).Interface(), uv.Field(i).Interface())
		}
	}
	if m.CleanupPhase != CleanupPhaseRequested || u.CleanupPhase != CleanupPhaseRequested {
		t.Fatalf("cleanup phases %q / %q, want requested", m.CleanupPhase, u.CleanupPhase)
	}
	if a.events(auto, "hibernate") != 0 || a.events(manual, "hibernate") != 1 {
		t.Fatalf("audit events: auto=%d manual=%d", a.events(auto, "hibernate"), a.events(manual, "hibernate"))
	}
	a.mustNext(0)
	a.wantUntouched(manual, m)
	a.wantUntouched(auto, u)
}

// L13b: an automatically admitted worker rides the same cleanup/withdraw/settle machinery: an
// unknown prune attempt is not withdrawn by the deadline, a matching stopped report settles it,
// the deadline then withdraws the worker, and the settle stamps the grace anchor.
func TestHibernationAutoCleanupDeadlineAndSettleLiveDB(t *testing.T) {
	a := newAutoFx(t)
	id, pod := a.dockerWorker()
	a.env.exec(`UPDATE workers SET created_at = $2, last_heartbeat_at = $3 WHERE id = $1`, id, a.clock.Add(-400*24*time.Hour), a.clock)
	a.mustPass(1)
	a.wantPhase(id, CleanupPhaseRequested)
	a.advance(time.Second) // the agent's evidence must be newer than the request and not in the future
	rep := a.readyRep(id)
	checked := a.clock
	rep.CustodyCheckedAt = &checked
	a.mustReport(id, rep)
	a.wantPhase(id, CleanupPhaseStarted)
	settle := func() {
		t.Helper()
		if _, err := a.svc.HibernationSettlePass(a.env.ctx); err != nil {
			t.Fatalf("settle pass: %v", err)
		}
	}
	a.advance(11 * time.Minute)
	settle()
	a.wantPhase(id, CleanupPhaseUnknown)
	a.want(id, "preparing") // an unsettled attempt suspends the preparation deadline
	a.advance(30 * time.Minute)
	settle()
	a.want(id, "preparing")

	if _, err := a.apply(id, a.cleanupStopped(id, pod)); err != nil {
		t.Fatalf("cleanup_stopped: %v", err)
	}
	a.wantPhase(id, CleanupPhaseSettled)
	a.want(id, "preparing")
	a.advance(11 * time.Minute)
	settle()
	a.want(id, "waking")
	if a.row(id).LastWakeSettledAt.Valid {
		t.Fatal("the withdrawal stamped the grace anchor before the wake settled")
	}
	a.heartbeatAt(id, a.clock)
	a.mustApply(id, TransitionReady, pod) // the admission pod never stopped: rule (a)
	row := a.row(id)
	if row.HibernationState != "none" || !row.LastWakeSettledAt.Time.Equal(a.clock) {
		t.Fatalf("after settle: state=%q settled=%v", row.HibernationState, row.LastWakeSettledAt)
	}
}

func (a *autoFx) heartbeatAt(id uuid.UUID, at time.Time) {
	a.env.exec(`UPDATE workers SET last_heartbeat_at = $2 WHERE id = $1`, id, at)
}

// L14a: an assignment that holds the worker lock wins. The pass lists the worker, blocks on the
// lock, and after the claim commits sees a non-terminal run and writes nothing. This proves the
// row lock serializes the pass behind the claim (the pass is seen blocked by the holder's
// backend); the zero-admission outcome is also guaranteed by the busy refusal, so it does not
// by itself isolate the post-lock re-check.
func TestHibernationAutoAssignmentWinsLiveDB(t *testing.T) {
	a := newAutoFx(t)
	id, _ := a.idle(400 * 24 * time.Hour)
	before := a.row(id)

	tx, err := a.env.pool.Begin(a.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(a.env.ctx) }()
	var holder int
	if err := tx.QueryRow(a.env.ctx, `SELECT pg_backend_pid()`).Scan(&holder); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(a.env.ctx, `SELECT id FROM workers WHERE id = $1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(a.env.ctx, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, claim_generation)
		VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'claimed', $5, 1)`, uuid.New(), a.user, a.repo, nextOutageIID(), id); err != nil {
		t.Fatal(err)
	}

	type result struct {
		n   int64
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := a.runPass()
		done <- result{n, err}
	}()

	deadline := time.Now().Add(15 * time.Second)
	for {
		var waiting int
		if err := a.env.pool.QueryRow(a.env.ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE $1 = ANY(pg_blocking_pids(pid))`, holder).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case r := <-done:
			t.Fatalf("the pass finished without waiting for the worker lock: %+v", r)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the pass never blocked on the worker lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := tx.Commit(a.env.ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || r.n != 0 {
		t.Fatalf("pass = %d, %v; want 0, nil", r.n, r.err)
	}
	a.wantUntouched(id, before)
}

// L14b: an admission that wins fences the claim and the chat-continue affinity.
func TestHibernationAutoAdmissionWinsLiveDB(t *testing.T) {
	a := newAutoFx(t)
	id, _ := a.idle(400 * 24 * time.Hour)
	other, _ := a.idle(1 * time.Hour) // awake and not idle enough: the control claimant
	a.mustPass(1)
	a.want(id, "preparing")

	a.assertNoClaim(id) // Claim returns nothing and the seeded queued run is left alone
	queued := a.queuedRun()
	defer a.env.exec(`DELETE FROM runs WHERE id = $1`, queued)
	if payload, err := a.svc.Claim(a.env.ctx, a.row(id), nil); err != nil || payload != nil {
		t.Fatalf("Claim on a preparing worker = %v, %v", payload, err)
	}
	var status string
	var assigned pgtype.UUID
	if err := a.env.pool.QueryRow(a.env.ctx, `SELECT status, worker_id FROM runs WHERE id = $1`, queued).Scan(&status, &assigned); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || assigned.Valid {
		t.Fatalf("the run left the queue: status=%q worker=%v", status, assigned)
	}

	// A queued run PINNED to the hibernating worker (the resume-pinned case that bypasses the
	// intake check) is fenced by ClaimRun's own hibernation_state guard: the unassigned run
	// above is already excluded by the intake check, so only this case isolates that guard.
	pinned := a.queuedRun()
	defer a.env.exec(`DELETE FROM runs WHERE id = $1`, pinned)
	a.env.exec(`UPDATE runs SET worker_id = $2 WHERE id = $1`, pinned, id)
	if payload, err := a.svc.Claim(a.env.ctx, a.row(id), nil); err != nil || payload != nil {
		t.Fatalf("Claim of a pinned run on a preparing worker = %v, %v", payload, err)
	}
	if err := a.env.pool.QueryRow(a.env.ctx, `SELECT status, worker_id FROM runs WHERE id = $1`, pinned).Scan(&status, &assigned); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || !assigned.Valid || assigned.Bytes != id {
		t.Fatalf("the pinned run was touched: status=%q worker=%v", status, assigned)
	}
	// Control: the same pinned run on an awake worker is claimed.
	a.env.exec(`DELETE FROM runs WHERE id = $1`, queued)
	a.env.exec(`UPDATE runs SET worker_id = $2 WHERE id = $1`, pinned, other)
	if payload, err := a.svc.Claim(a.env.ctx, a.row(other), nil); err != nil || payload == nil {
		t.Fatalf("control: an awake worker did not claim its pinned run: %v, %v", payload, err)
	}

	// Chat continue with the hibernating worker as affinity gets no worker.
	ended := a.runOf(runkind.Chat, "running", nil)
	a.finish(ended, "completed", a.clock.Add(-time.Minute))
	run, err := a.env.q.CreateChatContinueRun(a.env.ctx, store.CreateChatContinueRunParams{
		UserID: a.user, IssueTitle: "t", Title: pgtype.Text{String: "t", Valid: true},
		ResumeOfRunID: pgtype.UUID{Bytes: ended, Valid: true}, WorkerID: pgtype.UUID{Bytes: id, Valid: true}})
	if err != nil {
		t.Fatalf("CreateChatContinueRun: %v", err)
	}
	if run.WorkerID.Valid {
		t.Fatalf("a chat continue was pinned to a hibernating worker: %v", run.WorkerID)
	}
	// Control: the same call against an awake worker keeps the affinity.
	ok, err := a.env.q.CreateChatContinueRun(a.env.ctx, store.CreateChatContinueRunParams{
		UserID: a.user, IssueTitle: "t", Title: pgtype.Text{String: "t", Valid: true},
		ResumeOfRunID: pgtype.UUID{Bytes: ended, Valid: true}, WorkerID: pgtype.UUID{Bytes: other, Valid: true}})
	if err != nil || !ok.WorkerID.Valid {
		t.Fatalf("control chat continue: %v, worker=%v", err, ok.WorkerID)
	}
}

// L15: the worker is re-checked on its locked row. A run that appears or finishes between the
// candidate list and the lock stops the admission, with no write.
func TestHibernationAutoRecheckAfterListLiveDB(t *testing.T) {
	cases := []struct {
		name   string
		inject func(a *autoFx, id uuid.UUID)
	}{
		{"a run finished just now", func(a *autoFx, id uuid.UUID) {
			run := a.runOf(runkind.Issue, "completed", &id)
			a.finish(run, "completed", a.clock)
		}},
		// Guarded by both the post-lock re-check and hibernateRefusal's busy check, so it
		// does not isolate the re-check; only "a run finished just now" does (the busy check
		// ignores terminal runs).
		{"a run was assigned", func(a *autoFx, id uuid.UUID) { a.runOf(runkind.Issue, "queued", &id) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := newAutoFx(t)
			id, _ := a.idle(400 * 24 * time.Hour)
			before := a.row(id)
			listed := false
			a.svc.autoPassAfterList = func() {
				listed = true
				c.inject(a, id)
			}
			a.mustPass(0)
			if !listed {
				t.Fatal("the worker was not listed, so the re-check was not exercised")
			}
			a.wantUntouched(id, before)
		})
	}
}

// L12: an error that fails the commit leaves the worker as it was, with no blocked markers, and
// does not stop its siblings. The seam is a deferred constraint trigger that rejects the commit
// of an admission of one named worker.
func TestHibernationAutoCommitFailureLeavesNoTraceLiveDB(t *testing.T) {
	a := newAutoFx(t)
	bad, _ := a.idle(400 * 24 * time.Hour)
	good, _ := a.idle(399 * 24 * time.Hour)
	fn := "hib_auto_fail_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	a.env.exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.id = '%s'::uuid AND NEW.hibernation_state = 'preparing' THEN
				RAISE EXCEPTION 'injected commit failure';
			END IF;
			RETURN NULL;
		END $$`, fn, bad))
	a.env.exec(fmt.Sprintf(`CREATE CONSTRAINT TRIGGER %s AFTER UPDATE ON workers DEFERRABLE INITIALLY DEFERRED
		FOR EACH ROW EXECUTE FUNCTION %s()`, fn, fn))
	t.Cleanup(func() {
		_, _ = a.env.pool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON workers`, fn))
		_, _ = a.env.pool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))
	})
	before := a.row(bad)
	n, err := a.runPass()
	if err == nil || !strings.Contains(err.Error(), "injected commit failure") {
		t.Fatalf("pass error = %v, want the injected failure", err)
	}
	if n != 1 {
		t.Fatalf("admitted = %d, want 1 (the sibling)", n)
	}
	a.want(good, "preparing")
	a.wantUntouched(bad, before)
	wantNotBlocked(t, a.row(bad))
}

// The automatic setting can change after candidate enumeration or while a row lock is pending.
// Count reads to ensure the pass checks a locked candidate and stops, even if residue means the
// first candidate belongs to another fixture.
type autoModeReadCounter struct {
	*autoSettings
	reads int
}

func (s *autoModeReadCounter) WorkerHibernationAuto(ctx context.Context) (bool, error) {
	s.reads++
	return s.autoSettings.WorkerHibernationAuto(ctx)
}
func TestHibernationAutoOffAfterListStopsPassLiveDB(t *testing.T) {
	a := newAutoFx(t)
	id, _ := a.idle(400 * 24 * time.Hour)
	before := a.row(id)
	reader := &autoModeReadCounter{autoSettings: a.set}
	a.svc.SetHibernationAutoSettings(reader)
	listed := false
	a.svc.autoPassAfterList = func() { listed = true; a.set.auto = false }
	n, err := a.runPass()
	if err != nil || n != 0 {
		t.Fatalf("auto-off pass=%d, %v, want no admissions", n, err)
	}
	if !listed || reader.reads != 2 {
		t.Fatalf("listed=%v automatic-mode reads=%d; want pass gate plus first locked candidate", listed, reader.reads)
	}
	a.wantUntouched(id, before)
}
