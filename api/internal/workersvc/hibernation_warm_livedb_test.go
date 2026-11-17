package workersvc

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4395 M2: the warm reserve, the owner-clock long anchor and their budgets over a real
// database. Skipped without UZI_TEST_DATABASE_URL (run via ./e2e/run-store-it.sh). Outcomes are
// asserted per seeded worker; the pass count is asserted only where the count is the property
// under test (the budgets).

const (
	warmLongIdle = 400 * 24 * time.Hour
	oldRunAge    = 20 * 24 * time.Hour // older than the 14d long threshold
)

// sleeper admits id through the real admission writer with the given admit origin (preparing)
// and, when hibernated is set, drives it through the controller transitions to hibernated.
func (p *poolFx) sleeper(id uuid.UUID, origin string, hibernated bool) {
	p.t.Helper()
	if err := p.svc.hibernationTx(p.env.ctx, id, func(q *store.Queries, w store.Worker) error {
		admitHibernation(&w, p.clock, "test", origin)
		_, err := writeHibernation(p.env.ctx, q, w)
		return err
	}); err != nil {
		p.t.Fatalf("admit %s: %v", id, err)
	}
	if !hibernated {
		return
	}
	pod := p.row(id).RegisteredPodUid.Bytes
	p.mustApply(id, TransitionScalingDown, uuid.UUID(pod))
	p.mustApply(id, TransitionScaledDown, uuid.Nil)
	p.want(id, "hibernated")
}

// busy gives worker a running owner run whose created_at is age before the clock, so the owner
// is active now without the run entering the owner anchor near the clock.
func (p *poolFx) busy(worker uuid.UUID, age time.Duration) uuid.UUID {
	p.t.Helper()
	run := p.runOf(runkind.Issue, "running", &worker)
	p.env.exec(`UPDATE runs SET created_at = $2 WHERE id = $1`, run, p.clock.Add(-age))
	return run
}

// ownerRun seeds a worker-less run of the owner (user_id attribution only).
func (p *poolFx) ownerRun(kind, status string, created time.Time, finished *time.Time) uuid.UUID {
	p.t.Helper()
	run := p.runOf(kind, status, nil)
	if finished != nil {
		p.env.exec(`UPDATE runs SET created_at = $2, finished_at = $3, status_since = $3 WHERE id = $1`, run, created, *finished)
	} else {
		p.env.exec(`UPDATE runs SET created_at = $2 WHERE id = $1`, run, created)
	}
	return run
}

// finishedOwnerRun seeds a completed worker-less owner run that started and finished at ago.
func (p *poolFx) finishedOwnerRun(ago time.Duration) uuid.UUID {
	at := p.clock.Add(-ago)
	return p.ownerRun(runkind.Job, "completed", at, &at)
}

func (p *poolFx) wakeRows(worker uuid.UUID) int {
	p.t.Helper()
	var n int
	if err := p.env.pool.QueryRow(p.env.ctx, `SELECT count(*) FROM hibernation_wakes WHERE worker_id = $1`, worker).Scan(&n); err != nil {
		p.t.Fatal(err)
	}
	return n
}

func (p *poolFx) longIdleness(id uuid.UUID) store.GetWorkerLongIdlenessRow {
	p.t.Helper()
	row, err := p.env.q.GetWorkerLongIdleness(p.env.ctx, id)
	if err != nil {
		p.t.Fatalf("GetWorkerLongIdleness: %v", err)
	}
	return row
}

// wantWarm asserts id was warm woken: waking, the reserve reason, a valid warm wake origin, no
// demand receipt and no event row.
func (p *poolFx) wantWarm(id uuid.UUID) {
	p.t.Helper()
	p.want(id, "waking")
	w := p.wantOrigins(id, "", WakeOriginWarm)
	if w.HibernationReason.String != "automatic: warm reserve" {
		p.t.Fatalf("reason = %q", w.HibernationReason.String)
	}
	if p.wakeRows(id) != 0 {
		p.t.Fatal("a warm wake must write no hibernation_wakes row")
	}
	if p.events(id, "wake") != 0 || p.events(id, "hibernate") != 0 {
		p.t.Fatal("a warm wake must insert no hibernation event row")
	}
}

// settleSamePod settles a withdrawn (never scaled down) waking worker through the controller's
// ready report for its admission pod, at the fixture clock.
func (p *poolFx) settleSamePod(id uuid.UUID) store.Worker {
	p.t.Helper()
	w := p.row(id)
	p.beat(id)
	p.mustApply(id, TransitionReady, uuid.UUID(w.HibernationAdmissionPodUid.Bytes))
	p.want(id, "none")
	return p.row(id)
}

// Example 2 and the warm-wake decision, per condition.
func TestWarmWakeExample2LiveDB(t *testing.T) {
	t.Run("a busy keeper and an automatically hibernated sibling: the next pass warm wakes it", func(t *testing.T) {
		p := newPoolFx(t)
		keeper, sleeper := p.member(5*time.Hour), p.member(6*time.Hour)
		p.sleeper(sleeper, AdmitOriginAutoSurplus, true)
		before := p.row(sleeper)
		p.busy(keeper, time.Hour)
		if n := p.pass(keeper); n != 1 {
			t.Fatalf("pass = %d, want 1", n)
		}
		p.wantWarm(sleeper)
		after := p.row(sleeper)
		if after.HibernationOpID == before.HibernationOpID {
			t.Fatal("a warm wake mints a new op")
		}
		p.wantStates(map[uuid.UUID]string{keeper: "none"})
		// A healthy waking member counts toward the reserve: a further sleeper stays asleep.
		extra := p.member(7 * time.Hour)
		p.sleeper(extra, AdmitOriginAutoLong, true)
		if n := p.pass(keeper); n != 0 {
			t.Fatalf("pass with a healthy waking member = %d, want 0", n)
		}
		p.want(extra, "hibernated")
		p.wantWarm(sleeper)
	})

	t.Run("a preparing member is withdrawn before a hibernated one is woken", func(t *testing.T) {
		p := newPoolFx(t)
		keeper := p.member(5 * time.Hour)
		hib := p.member(4 * time.Hour) // the more recently used sleeper
		prep := p.member(9 * time.Hour)
		p.sleeper(hib, AdmitOriginAutoSurplus, true)
		p.sleeper(prep, AdmitOriginAutoLong, false)
		p.busy(keeper, time.Hour)
		p.pass(keeper)
		p.wantWarm(prep)
		p.want(hib, "hibernated")
	})

	t.Run("among hibernated sleepers the most recently used wakes first", func(t *testing.T) {
		p := newPoolFx(t)
		keeper, older, newer := p.member(5*time.Hour), p.member(9*time.Hour), p.member(4*time.Hour)
		p.sleeper(older, AdmitOriginAutoSurplus, true)
		p.sleeper(newer, AdmitOriginAutoSurplus, true)
		p.busy(keeper, time.Hour)
		p.pass(keeper)
		p.wantWarm(newer)
		p.want(older, "hibernated")
	})

	t.Run("a waking member with a wake_error does not count: another sleeper is chosen", func(t *testing.T) {
		p := newPoolFx(t)
		keeper, failed, next := p.member(5*time.Hour), p.member(9*time.Hour), p.member(6*time.Hour)
		p.sleeper(failed, AdmitOriginAutoSurplus, true)
		p.sleeper(next, AdmitOriginAutoSurplus, true)
		p.busy(keeper, time.Hour)
		if _, err := p.svc.WakeWorker(p.env.ctx, nil, failed, "system"); err != nil {
			t.Fatal(err)
		}
		p.env.exec(`UPDATE workers SET wake_error = 'controller could not scale up' WHERE id = $1`, failed)
		p.pass(keeper)
		p.wantWarm(next)
		if got := p.row(failed); got.HibernationState != "waking" || !got.WakeError.Valid {
			t.Fatalf("the failed member must be left as it is: %q %v", got.HibernationState, got.WakeError)
		}
	})

	t.Run("a pending demand wake on a member suppresses the warm wake", func(t *testing.T) {
		p := newPoolFx(t)
		keeper, sleeper, other := p.member(5*time.Hour), p.member(6*time.Hour), p.member(7*time.Hour)
		p.sleeper(sleeper, AdmitOriginAutoSurplus, true)
		p.sleeper(other, AdmitOriginAutoSurplus, true)
		p.busy(keeper, time.Hour)
		run := p.queuedRun()
		p.env.exec(`INSERT INTO hibernation_wakes (run_id, worker_id, first_woken_at) VALUES ($1, $2, $3)`, run, other, p.clock)
		if n := p.pass(keeper); n != 0 {
			t.Fatalf("pass = %d, want 0", n)
		}
		p.want(sleeper, "hibernated")
		p.want(other, "hibernated")
		// Control: once the run leaves the queue the demand is gone and the pool is re-warmed.
		p.env.exec(`UPDATE runs SET status = 'cancelled', finished_at = $2, status_since = $2 WHERE id = $1`, run, p.clock)
		p.pass(keeper)
		if p.state(sleeper) != "waking" && p.state(other) != "waking" {
			t.Fatal("control: neither sleeper was warm woken once the demand was gone")
		}
	})

	t.Run("an awake ready idle member is the reserve", func(t *testing.T) {
		p := newPoolFx(t)
		// Both awake members are young: no surplus, and the idle one is the reserve.
		a, b, sleeper := p.member(time.Hour), p.member(30*time.Minute), p.member(9*time.Hour)
		p.sleeper(sleeper, AdmitOriginAutoSurplus, true)
		p.busy(a, time.Hour)
		if n := p.pass(a, b); n != 0 {
			t.Fatalf("pass = %d, want 0", n)
		}
		p.want(sleeper, "hibernated")
	})

	t.Run("a keeper that is not ready is no reserve", func(t *testing.T) {
		p := newPoolFx(t)
		keeper, sleeper := p.member(5*time.Hour), p.member(6*time.Hour)
		p.sleeper(sleeper, AdmitOriginAutoSurplus, true)
		p.runOf(runkind.Chat, "running", nil)
		p.env.exec(`UPDATE workers SET memory_authority_lost_at = $2 WHERE id = $1`, keeper, p.clock.Add(-time.Hour))
		p.pass(keeper)
		p.wantWarm(sleeper)
	})
}

// Example 6 and the wake-target eligibility: only automatically admitted, uncordoned,
// unfenced, error-free sleepers are woken.
func TestWarmWakeTargetEligibilityLiveDB(t *testing.T) {
	cases := []struct {
		name string
		mut  func(p *poolFx, sleeper uuid.UUID)
	}{
		{"manually hibernated (example 6)", func(p *poolFx, s uuid.UUID) {
			p.env.exec(`UPDATE workers SET hibernation_admit_origin = 'manual' WHERE id = $1`, s)
		}},
		{"op without markers reads as legacy", func(p *poolFx, s uuid.UUID) {
			p.env.exec(`UPDATE workers SET hibernation_origin_op_id = NULL, hibernation_admit_origin = NULL WHERE id = $1`, s)
		}},
		{"markers bound to another op", func(p *poolFx, s uuid.UUID) {
			p.env.exec(`UPDATE workers SET hibernation_origin_op_id = gen_random_uuid() WHERE id = $1`, s)
		}},
		{"manually cordoned", func(p *poolFx, s uuid.UUID) {
			p.env.exec(`UPDATE workers SET manual_cordoned_at = $2, manual_cordoned_by = $3, manual_cordoned_by_label = 'a' WHERE id = $1`, s, p.clock, p.user)
		}},
		{"maintenance fenced", func(p *poolFx, s uuid.UUID) {
			p.env.exec(`UPDATE workers SET maintenance_fenced = true WHERE id = $1`, s)
		}},
		{"maintenance phase", func(p *poolFx, s uuid.UUID) {
			p.env.exec(`UPDATE workers SET maintenance_phase = 'requested' WHERE id = $1`, s)
		}},
		{"wake error", func(p *poolFx, s uuid.UUID) {
			p.env.exec(`UPDATE workers SET wake_error = 'x' WHERE id = $1`, s)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newPoolFx(t)
			keeper, sleeper := p.member(5*time.Hour), p.member(6*time.Hour)
			p.sleeper(sleeper, AdmitOriginAutoSurplus, true)
			p.busy(keeper, time.Hour)
			c.mut(p, sleeper)
			before := p.row(sleeper)
			if n := p.pass(keeper); n != 0 {
				t.Fatalf("pass = %d, want 0", n)
			}
			after := p.row(sleeper)
			if after.HibernationState != "hibernated" || after.HibernationOpID != before.HibernationOpID {
				t.Fatalf("the sleeper was touched: %q", after.HibernationState)
			}
		})
	}
	t.Run("a manual hibernation of an actual admin is never undone", func(t *testing.T) {
		p := newPoolFx(t)
		keeper, b := p.member(5*time.Hour), p.member(6*time.Hour)
		p.hibernate(b)
		p.busy(keeper, time.Hour)
		p.pass(keeper)
		p.wantOrigins(b, AdmitOriginManual, "")
		p.want(b, "preparing")
	})
}

// Shared gates: an established gate suppresses the proactive warm wake only.
func TestWarmWakeSharedGatesLiveDB(t *testing.T) {
	pause := func(t *testing.T, p *poolFx) {
		t.Helper()
		p.env.exec(`INSERT INTO instance_intake_pause (id) VALUES (true) ON CONFLICT (id) DO UPDATE SET active = true, since = now()`)
		t.Cleanup(func() {
			_, _ = p.env.pool.Exec(context.Background(), `UPDATE instance_intake_pause SET active = false, since = NULL, reason = NULL WHERE id`)
		})
	}
	for _, gate := range []struct {
		name string
		set  func(t *testing.T, p *poolFx)
	}{
		{"paused instance intake", pause},
	} {
		t.Run(gate.name+": busy keeper and a sleeper get no warm wake", func(t *testing.T) {
			p := newPoolFx(t)
			keeper, sleeper := p.member(5*time.Hour), p.member(6*time.Hour)
			p.sleeper(sleeper, AdmitOriginAutoSurplus, true)
			p.busy(keeper, time.Hour)
			gate.set(t, p)
			if n := p.pass(keeper); n != 0 {
				t.Fatalf("pass = %d, want 0", n)
			}
			p.want(sleeper, "hibernated")
		})
		t.Run(gate.name+": an idle keeper still admits the surplus", func(t *testing.T) {
			p := newPoolFx(t)
			cand, keeper := p.member(6*time.Hour), p.member(5*time.Hour)
			gate.set(t, p)
			p.pass(cand, keeper)
			p.wantStates(map[uuid.UUID]string{cand: "preparing", keeper: "none"})
		})
	}
	t.Run("control: the same pool without a gate is warm woken", func(t *testing.T) {
		p := newPoolFx(t)
		keeper, sleeper := p.member(5*time.Hour), p.member(6*time.Hour)
		p.sleeper(sleeper, AdmitOriginAutoSurplus, true)
		p.busy(keeper, time.Hour)
		p.pass(keeper)
		p.wantWarm(sleeper)
	})
	t.Run("the owner at the custody admission limit is still warm woken", func(t *testing.T) {
		// Cross-check and continuation-exempt claims and chat claims ignore the limit, so a warm
		// worker stays useful: the custody limit is not a warm-wake gate.
		p := newPoolFx(t)
		p.svc.p.CustodyHoldLimit = &fakeCustodyLimit{n: 1}
		keeper, sleeper := p.member(5*time.Hour), p.member(6*time.Hour)
		p.seedOpenCustodyHold(keeper) // one admission unit: the owner is at the limit of 1
		p.sleeper(sleeper, AdmitOriginAutoSurplus, true)
		p.busy(keeper, time.Hour)
		p.pass(keeper)
		p.wantWarm(sleeper)
	})
}

// Example 4, first half: warm wakes only while the owner is active within idle_after.
func TestWarmWakeOwnerActivityLiveDB(t *testing.T) {
	setup := func(p *poolFx) (keeper, sleeper uuid.UUID) {
		keeper, sleeper = p.member(warmLongIdle), p.member(warmLongIdle+time.Hour)
		p.sleeper(sleeper, AdmitOriginAutoSurplus, true)
		// The keeper is awake but not ready (offline), so the pool has no reserve.
		p.env.exec(`UPDATE workers SET status = 'offline' WHERE id = $1`, keeper)
		return keeper, sleeper
	}
	t.Run("last activity inside idle_after: warm wake", func(t *testing.T) {
		p := newPoolFx(t)
		_, sleeper := setup(p)
		p.finishedOwnerRun(14*24*time.Hour - 10*time.Minute)
		p.pass()
		p.wantWarm(sleeper)
	})
	t.Run("last activity past idle_after: no warm wake", func(t *testing.T) {
		p := newPoolFx(t)
		keeper, sleeper := setup(p)
		p.finishedOwnerRun(14*24*time.Hour + 10*time.Minute)
		p.pass()
		p.want(sleeper, "hibernated")
		// The long pass is what acts on an inactive owner: the idle pooled keeper sleeps.
		p.want(keeper, "preparing")
		p.wantOrigins(keeper, AdmitOriginAutoLong, "")
	})
	t.Run("activity that ages out between the list and the lock is re-judged under the lock", func(t *testing.T) {
		p := newPoolFx(t)
		_, sleeper := setup(p)
		run := p.finishedOwnerRun(time.Hour) // the list sees an active owner
		p.svc.autoPassAfterList = func() {
			aged := p.clock.Add(-15 * 24 * time.Hour)
			p.env.exec(`UPDATE runs SET created_at = $2, finished_at = $2, status_since = $2 WHERE id = $1`, run, aged)
		}
		p.pass()
		p.svc.autoPassAfterList = nil
		p.want(sleeper, "hibernated")
	})
	t.Run("a non-terminal run makes the owner active now however old it is", func(t *testing.T) {
		p := newPoolFx(t)
		_, sleeper := setup(p)
		p.ownerRun(runkind.Issue, "awaiting_input", p.clock.Add(-400*24*time.Hour), nil)
		p.pass()
		p.wantWarm(sleeper)
	})
	t.Run("an owner with no run at all is not active", func(t *testing.T) {
		p := newPoolFx(t)
		_, sleeper := setup(p)
		p.pass()
		p.want(sleeper, "hibernated")
	})
	t.Run("a warm wake is not owner activity", func(t *testing.T) {
		p := newPoolFx(t)
		keeper, sleeper := setup(p)
		p.finishedOwnerRun(14*24*time.Hour - 10*time.Minute)
		p.pass()
		p.wantWarm(sleeper)
		// Settle the warm wake (no run involved), let the owner's last activity age past idle_after:
		// nothing in the warm wake extended the owner clock.
		_ = keeper
		p.advance(15 * time.Minute)
		if got := p.longIdleness(sleeper); !got.Anchor.Valid || got.Anchor.Time.After(p.clock.Add(-14*24*time.Hour+5*time.Minute)) {
			t.Fatalf("long anchor %v moved with a warm wake; want the owner's last activity", got.Anchor.Time)
		}
	})
}

// Example 4: the owner-clock long rule for pooled workers.
func TestPooledLongRuleOwnerClockLiveDB(t *testing.T) {
	type tc struct {
		name       string
		seed       func(p *poolFx)
		wantLong   bool // both members are long-idle: auto_long admissions, one per pass
		wantActive bool // the owner is active: only the surplus rule acts
	}
	finishedNow := func(p *poolFx) {
		p.ownerRun(runkind.Job, "completed", p.clock.Add(-oldRunAge), &p.clock)
	}
	cases := []tc{
		{name: "no owner run at all", seed: func(*poolFx) {}, wantLong: true},
		{name: "last run finished past the threshold", seed: func(p *poolFx) { p.finishedOwnerRun(15 * 24 * time.Hour) }, wantLong: true},
		{name: "last run finished inside the threshold", seed: func(p *poolFx) { p.finishedOwnerRun(13 * 24 * time.Hour) }, wantActive: true},
		{name: "non-terminal run older than the threshold", seed: func(p *poolFx) {
			p.ownerRun(runkind.Issue, "running", p.clock.Add(-oldRunAge), nil)
		}, wantActive: true},
		{name: "only a recent scheduled run", seed: func(p *poolFx) {
			run := p.finishedOwnerRun(time.Hour)
			p.env.exec(`UPDATE runs SET trigger_source = 'schedule' WHERE id = $1`, run)
		}, wantActive: true},
		{name: "run created before the threshold with no worker, finishing now", seed: finishedNow, wantActive: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newPoolFx(t)
			a, b := p.member(warmLongIdle), p.member(warmLongIdle-time.Hour)
			c.seed(p)
			if n := p.pass(a, b); n != 1 {
				t.Fatalf("pass 1 = %d, want 1 (one change per owner)", n)
			}
			w := p.row(a)
			switch {
			case c.wantLong:
				if w.HibernationState != "preparing" || w.HibernationReason.String != "automatic: no run activity for 14d" {
					t.Fatalf("a: %q %q, want an auto_long admission", w.HibernationState, w.HibernationReason.String)
				}
				p.wantOrigins(a, AdmitOriginAutoLong, "")
				p.want(b, "none")
				// Every idle pooled worker of an inactive owner sleeps, one per pass.
				if n := p.pass(b); n != 1 {
					t.Fatalf("pass 2 = %d, want 1", n)
				}
				p.want(b, "preparing")
				p.wantOrigins(b, AdmitOriginAutoLong, "")
			case c.wantActive:
				// The owner is active: the keeper is never long-idle, and only the surplus rule acts.
				if w.HibernationState != "preparing" || !strings.HasPrefix(w.HibernationReason.String, "automatic: surplus") {
					t.Fatalf("a: %q %q, want a surplus admission", w.HibernationState, w.HibernationReason.String)
				}
				p.wantOrigins(a, AdmitOriginAutoSurplus, "")
				if n := p.pass(b); n != 0 {
					t.Fatalf("pass 2 = %d, want 0: the keeper is not long-idle while its owner is active", n)
				}
				p.want(b, "none")
			}
		})
	}
}

// Example 4: warm and system wakes never extend the long anchor; a manual wake does; a settled
// warm wake does not keep an inactive owner's worker awake.
func TestLongAnchorWakeOriginsLiveDB(t *testing.T) {
	for _, c := range []struct {
		name     string
		wake     string
		manual   bool // last_manual_wake_settled_at is set too
		wantKept bool
	}{
		{"warm wake", WakeOriginWarm, false, false},
		{"system wake", WakeOriginSystem, false, false},
		{"manual wake", WakeOriginManual, true, true},
		{"demand wake settled", WakeOriginDemand, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := newPoolFx(t)
			old, woken := p.member(warmLongIdle), p.member(warmLongIdle)
			p.finishedOwnerRun(15 * 24 * time.Hour) // the owner has been inactive for 15d
			settled := p.clock.Add(-5 * 24 * time.Hour)
			op := uuid.New()
			var manual any
			if c.manual {
				manual = settled
			}
			p.env.exec(`UPDATE workers SET hibernation_op_id = $2, hibernation_origin_op_id = $2, hibernation_wake_origin = $3,
			            last_wake_settled_at = $4, last_manual_wake_settled_at = $5 WHERE id = $1`, woken, op, c.wake, settled, manual)
			p.pass(old, woken)
			p.pass(old, woken)
			want := "preparing"
			if c.wantKept {
				want = "none"
			}
			p.wantStates(map[uuid.UUID]string{old: "preparing", woken: want})
			if !c.wantKept {
				p.wantOrigins(woken, AdmitOriginAutoLong, "")
			}
		})
	}
}

// The origin of a wake that joins a waking warm op decides, after settlement, whether the settle
// extends the long anchor. A warm request never downgrades the origin.
func TestOriginJoinsExtendLongAnchorLiveDB(t *testing.T) {
	var ownerAnchor time.Time // the owner's only activity: the keeper's run, created oldRunAge before the clock
	prep := func(t *testing.T) (p *poolFx, keeper, sleeper uuid.UUID) {
		p = newPoolFx(t)
		keeper, sleeper = p.member(warmLongIdle), p.member(warmLongIdle)
		p.sleeper(sleeper, AdmitOriginAutoSurplus, false) // preparing: withdrawn, never scaled down
		ownerAnchor = p.clock.Add(-oldRunAge)
		p.busy(keeper, oldRunAge)
		p.pass(keeper)
		p.wantWarm(sleeper)
		return p, keeper, sleeper
	}
	t.Run("warm settle does not extend", func(t *testing.T) {
		p, _, sleeper := prep(t)
		row := p.settleSamePod(sleeper)
		if !row.LastWakeSettledAt.Valid || row.LastManualWakeSettledAt.Valid {
			t.Fatalf("warm settle: last_wake=%v last_manual=%v", row.LastWakeSettledAt, row.LastManualWakeSettledAt)
		}
		if got := p.longIdleness(sleeper); !got.Anchor.Time.Equal(ownerAnchor) {
			t.Fatalf("long anchor = %v, want only the owner's activity %v", got.Anchor.Time, ownerAnchor)
		}
	})
	t.Run("manual join extends", func(t *testing.T) {
		p, _, sleeper := prep(t)
		op := p.op(sleeper)
		if _, err := p.svc.WakeWorker(p.env.ctx, &p.actor, sleeper, ""); err != nil {
			t.Fatal(err)
		}
		if p.op(sleeper) != op {
			t.Fatal("the join replaced the op")
		}
		p.wantOrigins(sleeper, "", WakeOriginManual)
		row := p.settleSamePod(sleeper)
		if !row.LastManualWakeSettledAt.Valid {
			t.Fatal("a warm op upgraded to manual must settle as manual-grade")
		}
		if got := p.longIdleness(sleeper); !got.Anchor.Time.Equal(p.clock) {
			t.Fatalf("long anchor = %v, want the settle %v", got.Anchor.Time, p.clock)
		}
	})
	t.Run("demand join extends", func(t *testing.T) {
		p, _, sleeper := prep(t)
		run := p.queuedRun()
		p.env.exec(`UPDATE runs SET created_at = $2 WHERE id = $1`, run, p.clock.Add(-oldRunAge))
		if res, err := p.svc.DemandWake(p.env.ctx, run, sleeper); err != nil || res != DemandWakeAttached {
			t.Fatalf("DemandWake = %v, %v; want attached", res, err)
		}
		p.wantOrigins(sleeper, "", WakeOriginDemand)
		p.settleSamePod(sleeper)
		if got := p.longIdleness(sleeper); !got.Anchor.Time.Equal(p.clock) {
			t.Fatalf("long anchor = %v, want the settle %v", got.Anchor.Time, p.clock)
		}
	})
	t.Run("a warm request never downgrades", func(t *testing.T) {
		p, _, sleeper := prep(t)
		if _, err := p.svc.WakeWorker(p.env.ctx, &p.actor, sleeper, ""); err != nil {
			t.Fatal(err)
		}
		if err := p.svc.hibernationTx(p.env.ctx, sleeper, func(q *store.Queries, w store.Worker) error {
			moveToWaking(&w, p.clock, warmReserveReason, WakeOriginWarm)
			_, err := writeHibernation(p.env.ctx, q, w)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		p.wantOrigins(sleeper, "", WakeOriginManual)
	})
}

// Go and SQL use the same conservative wake term: for every marker combination the long anchor
// the query computes equals GREATEST(created_at, owner anchor, conservativeWakeTerm of the row).
func TestConservativeWakeTermParityLiveDB(t *testing.T) {
	type seed struct {
		name         string
		validMarkers bool
		admit, wake  string
		settledAgo   time.Duration
		manualAgo    time.Duration // 0 = NULL
	}
	cases := []seed{
		{"no markers at all (pre-migration row)", false, "", "", 2 * 24 * time.Hour, 0},
		{"markers bound to another op", false, AdmitOriginAutoLong, WakeOriginWarm, 2 * 24 * time.Hour, 0},
		{"valid warm wake", true, "", WakeOriginWarm, 2 * 24 * time.Hour, 0},
		{"valid system wake", true, "", WakeOriginSystem, 2 * 24 * time.Hour, 0},
		{"valid warm wake with an older manual grace", true, "", WakeOriginWarm, 2 * 24 * time.Hour, 6 * 24 * time.Hour},
		{"valid manual wake, no last_manual (old writer)", true, "", WakeOriginManual, 2 * 24 * time.Hour, 0},
		{"valid manual wake with last_manual", true, "", WakeOriginManual, 2 * 24 * time.Hour, 2 * 24 * time.Hour},
		{"valid demand wake", true, "", WakeOriginDemand, 2 * 24 * time.Hour, 0},
		{"valid admitted op that has not woken", true, AdmitOriginAutoSurplus, "", 2 * 24 * time.Hour, 0},
		{"valid admitted op that has not woken, older manual grace", true, AdmitOriginAutoLong, "", 2 * 24 * time.Hour, 6 * 24 * time.Hour},
		{"valid op with neither origin", true, "", "", 2 * 24 * time.Hour, 0},
		{"never settled", true, "", WakeOriginManual, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newPoolFx(t)
			w, _ := p.member(warmLongIdle), p.member(warmLongIdle)
			op, other := uuid.New(), uuid.New()
			originOp := op
			if !c.validMarkers {
				originOp = other
			}
			var settled, manual, admit, wake any
			if c.settledAgo > 0 {
				settled = p.clock.Add(-c.settledAgo)
			}
			if c.manualAgo > 0 {
				manual = p.clock.Add(-c.manualAgo)
			}
			if c.admit != "" {
				admit = c.admit
			}
			if c.wake != "" {
				wake = c.wake
			}
			p.env.exec(`UPDATE workers SET hibernation_op_id = $2, hibernation_origin_op_id = $3, hibernation_admit_origin = $4,
			            hibernation_wake_origin = $5, last_wake_settled_at = $6, last_manual_wake_settled_at = $7 WHERE id = $1`,
				w, op, originOp, admit, wake, settled, manual)
			row := p.row(w)
			want := row.CreatedAt.Time
			if term := conservativeWakeTerm(&row); term.Valid && term.Time.After(want) {
				want = term.Time
			}
			got := p.longIdleness(w)
			if !got.Pooled || !got.Anchor.Valid || !got.Anchor.Time.Equal(want) {
				t.Fatalf("SQL long anchor = %v (pooled %t), Go expects %v", got.Anchor, got.Pooled, want)
			}
		})
	}
}

// Example 5, second half: legacy and old-writer settles keep their grace when surplus mode is on;
// only a known warm or system settle is excluded from the owner clock.
func TestSurplusModeKeepsLegacyGraceLiveDB(t *testing.T) {
	for _, c := range []struct {
		name  string
		seed  string // SQL fragment over the target worker; $1 = id, $2 = settle time, $3 = op
		kept  bool
		extra string
	}{
		{"legacy row: NULL markers and a recent pre-migration wake", `UPDATE workers SET last_wake_settled_at = $2 WHERE id = $1 AND $3::uuid IS NOT NULL`, true, ""},
		{"op minted without markers", `UPDATE workers SET hibernation_op_id = $3, hibernation_origin_op_id = gen_random_uuid(),
		   hibernation_wake_origin = 'warm', last_wake_settled_at = $2 WHERE id = $1`, true, ""},
		{"manual op settled by an old writer", `UPDATE workers SET hibernation_op_id = $3, hibernation_origin_op_id = $3,
		   hibernation_wake_origin = 'manual', last_wake_settled_at = $2 WHERE id = $1`, true, ""},
		{"known warm settle", `UPDATE workers SET hibernation_op_id = $3, hibernation_origin_op_id = $3,
		   hibernation_wake_origin = 'warm', last_wake_settled_at = $2 WHERE id = $1`, false, ""},
		{"known system settle", `UPDATE workers SET hibernation_op_id = $3, hibernation_origin_op_id = $3,
		   hibernation_wake_origin = 'system', last_wake_settled_at = $2 WHERE id = $1`, false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := newPoolFx(t)
			target, sibling := p.member(warmLongIdle), p.member(warmLongIdle)
			p.hibernate(sibling) // a pooled but sleeping, manually admitted sibling: no keeper, no warm target
			p.env.exec(c.seed, target, p.clock.Add(-2*24*time.Hour), uuid.New())
			p.pass(target)
			want := "preparing"
			if c.kept {
				want = "none"
			}
			if got := p.state(target); got != want {
				t.Fatalf("state = %q, want %q", got, want)
			}
			if !c.kept {
				p.wantOrigins(target, AdmitOriginAutoLong, "")
			}
		})
	}
}

// Example 5, first half of the toggle case: setting off right after a warm wake keeps the worker
// awake for the full threshold, because every settle (warm included) writes last_wake_settled_at.
func TestSettingOffAfterWarmWakeKeepsWorkerAwakeLiveDB(t *testing.T) {
	p := newPoolFx(t)
	keeper, sleeper := p.member(warmLongIdle), p.member(warmLongIdle)
	p.sleeper(sleeper, AdmitOriginAutoSurplus, false)
	p.busy(keeper, time.Hour)
	p.pass(keeper)
	p.wantWarm(sleeper)
	row := p.settleSamePod(sleeper)
	settled := row.LastWakeSettledAt.Time
	if !settled.Equal(p.clock) {
		t.Fatalf("settle stamp %v, want %v", settled, p.clock)
	}
	p.ps.setSurplus(false)
	// Today's anchor is the settle: the worker is awake until a full idle_after has passed.
	p.advance(14*24*time.Hour - 10*time.Minute)
	p.pass()
	if got := p.state(sleeper); got != "none" {
		t.Fatalf("state with the setting off just inside the threshold = %q, want none", got)
	}
	p.advance(20 * time.Minute)
	// The pass is global and capped at 10 changes, and a jump of 14 days makes every persistent
	// hosted worker another test left in the database long idle too; oldest anchors go first, so
	// allow a bounded number of passes for the backlog to drain before the sleeper's turn.
	for i := 0; i < 20 && p.state(sleeper) != "preparing"; i++ {
		p.pass()
	}
	if got := p.state(sleeper); got != "preparing" {
		t.Fatalf("state with the setting off past the threshold = %q, want preparing", got)
	}
	if got := p.row(sleeper).HibernationReason.String; got != "automatic: no run activity for 14d" {
		t.Fatalf("reason = %q", got)
	}
}

// flipSurplus reports surplus mode on for its first read (the pass's list read) and off for every
// later one, as an admin turning the setting off between the list and the locked decision.
type flipSurplus struct {
	*poolSettings
	reads atomic.Int32
}

func (f *flipSurplus) WorkerHibernationSurplus(ctx context.Context) (bool, error) {
	if f.reads.Add(1) == 1 {
		return f.poolSettings.WorkerHibernationSurplus(ctx)
	}
	return false, nil
}

// Example 5, the race form: surplus mode is switched off after the pass listed a pooled worker as
// long-idle by the owner clock but before the locked decision. The worker's last settle is a known
// warm one (excluded from the owner clock) and recent, so today's per-worker anchor keeps it awake:
// it must not be admitted on the owner clock. The control keeps the setting on and does admit.
func TestSettingOffBetweenListAndDecideKeepsPooledWorkerAwakeLiveDB(t *testing.T) {
	for _, c := range []struct {
		name string
		flip bool
		want string
	}{
		{"control: setting stays on", false, "preparing"},
		{"setting switched off before the locked decision", true, "none"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := newPoolFx(t)
			target, sibling := p.member(warmLongIdle), p.member(warmLongIdle)
			p.hibernate(sibling) // pooled but sleeping, manually admitted: no keeper, no warm target
			p.env.exec(`UPDATE workers SET hibernation_op_id = $3, hibernation_origin_op_id = $3,
			   hibernation_wake_origin = 'warm', last_wake_settled_at = $2 WHERE id = $1`,
				target, p.clock.Add(-time.Hour), uuid.New())
			if c.flip {
				p.svc.SetHibernationAutoSettings(&flipSurplus{poolSettings: p.ps})
			}
			p.pass(target)
			if got := p.state(target); got != c.want {
				t.Fatalf("state = %q, want %q", got, c.want)
			}
		})
	}
}

// The carry-forward regression: a recent legacy wake, older but still-active owner history, an
// automatic surplus sleep, then warm wakes from keeper unavailability with no new activity. The
// original grace survives op replacement and settlement; repeated warm wakes never extend it.
func TestCarryForwardGraceSurvivesWarmWakesLiveDB(t *testing.T) {
	p := newPoolFx(t)
	w, keeper := p.member(warmLongIdle), p.member(time.Hour)
	legacy := p.clock.Add(-3 * 24 * time.Hour)
	p.env.exec(`UPDATE workers SET last_wake_settled_at = $2 WHERE id = $1`, w, legacy)
	p.finishedOwnerRun(5 * 24 * time.Hour) // older than the legacy wake, inside the long window
	anchorIs := func(want time.Time, when string) {
		t.Helper()
		if got := p.longIdleness(w); !got.Anchor.Time.Equal(want) {
			t.Fatalf("%s: long anchor = %v, want %v", when, got.Anchor.Time, want)
		}
	}
	anchorIs(legacy, "before any surplus sleep")

	// Automatic surplus sleep of w (its surplus anchor is the legacy wake, older than the keeper's).
	p.pass(w, keeper)
	p.want(w, "preparing")
	p.wantOrigins(w, AdmitOriginAutoSurplus, "")
	anchorIs(legacy, "after the surplus admission")
	if got := p.row(w).LastManualWakeSettledAt; !got.Valid || !got.Time.Equal(legacy) {
		t.Fatalf("grace not carried at admission: %v", got)
	}

	for cycle := 1; cycle <= 2; cycle++ {
		// The keeper becomes unavailable; the pool is re-warmed from the automatically admitted sleeper.
		p.env.exec(`UPDATE workers SET status = 'offline' WHERE id = $1`, keeper)
		p.pass()
		p.wantWarm(w)
		anchorIs(legacy, fmt.Sprintf("cycle %d: after the warm wake", cycle))
		p.env.exec(`UPDATE workers SET status = 'online' WHERE id = $1`, keeper)
		settle := p.settleSamePod(w)
		if !settle.LastWakeSettledAt.Time.Equal(p.clock) {
			t.Fatalf("cycle %d: settle stamp = %v", cycle, settle.LastWakeSettledAt)
		}
		anchorIs(legacy, fmt.Sprintf("cycle %d: after the warm settle", cycle))
		if cycle == 2 {
			break
		}
		// w is surplus again once the keeper is the more recently used member and the threshold passed.
		p.advance(poolSurplusIdle + time.Hour)
		p.beat(w, keeper)
		p.env.exec(`UPDATE workers SET last_wake_settled_at = $2 WHERE id = $1`, keeper, p.clock)
		p.pass(w, keeper)
		p.want(w, "preparing")
		anchorIs(legacy, fmt.Sprintf("cycle %d: after the second surplus admission", cycle))
	}
	// The grace expires on its own: legacy + idle_after, never later.
	p.advance(14*24*time.Hour - 3*24*time.Hour - 5*time.Minute)
	if got := p.longIdleness(w); got.Anchor.Time.After(p.clock.Add(-14*24*time.Hour + 10*time.Minute)) {
		t.Fatalf("grace extended by warm wakes: anchor %v", got.Anchor.Time)
	}
}

// Budgets: one change per owner per pass across warm wake, surplus and long; the shared cap of
// ten; owners served oldest-first.
func TestWarmWakeBudgetsLiveDB(t *testing.T) {
	t.Run("one change per owner per pass: the warm wake first, then the surplus", func(t *testing.T) {
		p := newPoolFx(t)
		// Pool X: busy keeper plus an automatically hibernated sleeper. Pool Y (size l): two idle members.
		keeper, sleeper := p.member(5*time.Hour), p.member(6*time.Hour)
		y1, y2 := p.member(9*time.Hour), p.member(8*time.Hour)
		p.env.exec(`UPDATE workers SET hosted_size = 'l' WHERE id = ANY($1)`, []uuid.UUID{y1, y2})
		p.sleeper(sleeper, AdmitOriginAutoSurplus, true)
		p.busy(keeper, time.Hour)
		if n := p.pass(keeper, y1, y2); n != 1 {
			t.Fatalf("pass 1 = %d, want 1", n)
		}
		p.wantWarm(sleeper)
		p.wantStates(map[uuid.UUID]string{y1: "none", y2: "none"})
		if n := p.pass(keeper, y1, y2); n != 1 {
			t.Fatalf("pass 2 = %d, want 1", n)
		}
		p.wantStates(map[uuid.UUID]string{y1: "preparing", y2: "none"})
		p.wantWarm(sleeper)
	})

	t.Run("shared cap of ten across owners, oldest first", func(t *testing.T) {
		p := newPoolFx(t)
		const owners = HibernationAutoAdmissionsPerPass + 2
		keepers := make([]uuid.UUID, owners)
		sleepers := make([]uuid.UUID, owners)
		for i := range owners {
			o := p.owner()
			keepers[i] = p.memberOf(o, 5*time.Hour)
			// Owner 0's sleeper is the oldest: it has waited longest.
			sleepers[i] = p.memberOf(o, 30*time.Hour+time.Duration(owners-i)*time.Minute)
			p.sleeper(sleepers[i], AdmitOriginAutoSurplus, false)
			run := p.runOfOwner(o, "running", &keepers[i])
			p.env.exec(`UPDATE runs SET created_at = $2 WHERE id = $1`, run, p.clock.Add(-time.Hour))
		}
		if n := p.pass(keepers...); n != HibernationAutoAdmissionsPerPass {
			t.Fatalf("pass 1 = %d, want the cap %d", n, HibernationAutoAdmissionsPerPass)
		}
		for i := range owners {
			want := "waking"
			if i >= HibernationAutoAdmissionsPerPass {
				want = "preparing" // the two youngest owners wait a pass
			}
			p.wantStates(map[uuid.UUID]string{sleepers[i]: want})
		}
		if n := p.pass(keepers...); n != 2 {
			t.Fatalf("pass 2 = %d, want the remaining 2", n)
		}
		for i := range owners {
			p.wantWarm(sleepers[i])
		}
	})
}

// runOfOwner inserts a repo-less chat run of owner assigned to worker.
func (p *poolFx) runOfOwner(owner uuid.UUID, status string, worker *uuid.UUID) uuid.UUID {
	p.t.Helper()
	id := uuid.New()
	var wid any
	if worker != nil {
		wid = *worker
	}
	p.env.exec(`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, worker_id)
	            VALUES ($1, $2, 'chat', 't', 'd', $3, $4)`, id, owner, status, wid)
	return id
}

// The SQL form of pooledness equals the Go pool key: the surplus-mode long listing's pooled flag
// agrees with poolKey over the owner's rows for every key dimension.
func TestPooledSQLMatchesGoPoolKeyLiveDB(t *testing.T) {
	p := newPoolFx(t)
	mk := func(sql string, args ...any) uuid.UUID {
		id := p.member(10 * time.Hour)
		if sql != "" {
			p.env.exec(`UPDATE workers SET `+sql+` WHERE id = $1`, append([]any{id}, args...)...)
		}
		return id
	}
	ids := map[string]uuid.UUID{
		"a1":       mk(""),
		"a2":       mk(""),
		"size":     mk(`hosted_size = 'l'`),
		"template": mk(`template_declared = 'other'`),
		"docker":   mk(`docker_enabled = true`),
		"docker2":  mk(`docker_enabled = true`),
		"caps":     mk(`capabilities = '{jvm}'`),
		"proto1":   mk(`protocol_capabilities = '{x,y}'`),
		"proto2":   mk(`protocol_capabilities = '{y,x,x}'`),
		"proto3":   mk(`protocol_capabilities = '{x}'`),
		"slots":    mk(`max_concurrent_runs = 3`),
		"nullrun1": mk(`max_concurrent_runs = NULL`),
		"nullrun2": mk(`max_concurrent_runs = NULL`),
		"cross1":   mk(`max_cross_check_slots = 2`),
		"cross2":   mk(`max_cross_check_slots = 2`),
		"cross3":   mk(`max_cross_check_slots = 1`),
		"unreg":    mk(`docker_enabled = NULL`),
		"lone":     mk(`hosted_size = 'xl'`),
	}
	eph := uuid.New()
	p.env.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, kind, hosted_size, template_declared, docker_enabled, ephemeral, max_concurrent_runs, created_at, last_heartbeat_at)
	            VALUES ($1, $2, 'eph', $3, 'online', 'hosted', 'm', 'base', false, true, 2, $4, $5)`, eph, p.user, eph[:], p.clock.Add(-10*time.Hour), p.clock)
	ids["ephemeral"] = eph

	goPooled := map[uuid.UUID]bool{}
	rows, err := p.env.q.GetOwnerHibernationPoolInput(p.env.ctx, p.user)
	if err != nil {
		t.Fatal(err)
	}
	size := map[string]int{}
	for _, r := range rows {
		if k, ok := poolKey(r); ok {
			size[k]++
		}
	}
	for _, r := range rows {
		k, ok := poolKey(r)
		goPooled[r.ID] = ok && size[k] >= 2
	}
	cands, err := p.env.q.ListAutoHibernationCandidatesSurplusMode(p.env.ctx, store.ListAutoHibernationCandidatesSurplusModeParams{
		Cutoff: pgconv.Time(p.clock.Add(24 * time.Hour)), RecentCutoff: pgconv.Time(p.clock), MaxRows: 512,
	})
	if err != nil {
		t.Fatal(err)
	}
	sqlPooled := map[uuid.UUID]bool{}
	for _, c := range cands {
		if c.UserID == p.user {
			sqlPooled[c.ID] = c.Pooled
		}
	}
	for name, id := range ids {
		if name == "ephemeral" {
			if _, listed := sqlPooled[id]; listed {
				t.Fatalf("the ephemeral worker must not be listed")
			}
			continue
		}
		got, listed := sqlPooled[id]
		if !listed {
			t.Fatalf("%s not listed", name)
		}
		if got != goPooled[id] {
			t.Errorf("%s: SQL pooled = %t, Go poolKey pooled = %t", name, got, goPooled[id])
		}
	}
	for _, name := range []string{"a1", "a2", "docker", "docker2", "proto1", "proto2", "nullrun1", "nullrun2", "cross1", "cross2"} {
		if !sqlPooled[ids[name]] {
			t.Errorf("%s should be pooled", name)
		}
	}
	for _, name := range []string{"size", "template", "caps", "proto3", "slots", "cross3", "unreg", "lone"} {
		if sqlPooled[ids[name]] {
			t.Errorf("%s should not be pooled", name)
		}
	}
}

// The locked recheck: activity that lands after the list and before the lock wins. The pooled
// long admission is refused by GetWorkerLongIdleness, so the worker is admitted (if at all) by
// the surplus rule, never as auto_long. Two arms: a run that finishes after the list moves the
// owner anchor; a non-terminal run older than the threshold leaves the anchor old and is caught
// only by the owner-active-now arm.
func TestPooledLongAdmissionRechecksUnderLockLiveDB(t *testing.T) {
	for _, c := range []struct {
		name string
		hook func(p *poolFx)
	}{
		{"a run finishes after the list", func(p *poolFx) { p.finishedOwnerRun(time.Minute) }},
		{"an old non-terminal run appears after the list", func(p *poolFx) {
			p.ownerRun(runkind.Issue, "running", p.clock.Add(-oldRunAge), nil)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := newPoolFx(t)
			a, b := p.member(warmLongIdle), p.member(warmLongIdle-time.Hour)
			p.svc.autoPassAfterList = func() { c.hook(p) }
			p.pass(a, b)
			p.svc.autoPassAfterList = nil
			w := p.row(a)
			if w.HibernationState != "preparing" || w.HibernationAdmitOrigin.String != AdmitOriginAutoSurplus {
				t.Fatalf("a: %q admit %v; the long recheck must refuse and the surplus rule admit", w.HibernationState, w.HibernationAdmitOrigin)
			}
			p.want(b, "none")
		})
	}
}

// Without surplus mode nothing changes: no warm wake, the long rule is the per-worker rule.
func TestWarmWakeNeedsSurplusModeLiveDB(t *testing.T) {
	p := newPoolFx(t)
	keeper, sleeper := p.member(5*time.Hour), p.member(6*time.Hour)
	p.sleeper(sleeper, AdmitOriginAutoSurplus, true)
	p.busy(keeper, time.Hour)
	p.ps.setSurplus(false)
	if n := p.pass(keeper); n != 0 {
		t.Fatalf("pass = %d, want 0", n)
	}
	p.want(sleeper, "hibernated")
	// The setting switched off between the list and the write also withholds the warm wake.
	p.ps.setSurplus(true)
	p.svc.autoPassAfterList = func() { p.ps.setSurplus(false) }
	if n := p.pass(keeper); n != 0 {
		t.Fatalf("pass with the setting switched off after the list = %d, want 0", n)
	}
	p.want(sleeper, "hibernated")
	p.svc.autoPassAfterList = nil
	p.ps.setSurplus(true)
	p.pass(keeper)
	p.wantWarm(sleeper)
}

// Issue #4442: a draining (non-maintenance) sleeper stays a warm-wake target. A replacement pod's
// Register clears the drain (clear-on-roll, PRD #422) so the worker becomes a ready keeper; an
// original pod retained settles without a Register, so the drain and the not-ready state stay.

// drain marks id draining outside maintenance, as an image roll does.
func (p *poolFx) drain(id uuid.UUID) {
	p.t.Helper()
	p.env.exec(`UPDATE workers SET draining_since = $2, draining_reason = 'image_roll' WHERE id = $1`, id, p.clock.Add(-time.Minute))
}

// registerPod registers id from pod through the real Register path with the seeded slot count
// (so the pool key keeps max_concurrent_runs) and the given protocol capabilities; it returns
// the new registration nonce.
func (p *poolFx) registerPod(id, pod uuid.UUID, protocolCaps []string) string {
	p.t.Helper()
	slots := 2
	_, nonce, err := p.svc.RegisterWithCrossCheckSlots(p.env.ctx, p.row(id), "v1", "", &slots, nil, nil, protocolCaps, nil, pod)
	if err != nil {
		p.t.Fatalf("register: %v", err)
	}
	return nonce
}

// readyReplacement sends the controller's ready report for a fresh pod of the waking id: ready
// alone never settles a replacement pod.
func (p *poolFx) readyReplacement(id uuid.UUID) uuid.UUID {
	p.t.Helper()
	next := uuid.New()
	p.env.exec(`UPDATE workers SET registered_pod_uid = $2 WHERE id = $1`, id, next)
	p.mustApply(id, TransitionReady, next)
	p.want(id, "waking")
	return next
}

// keeperOf is the deterministic keeper of the owner's pool input, if any.
func (p *poolFx) keeperOf() (uuid.UUID, bool) {
	p.t.Helper()
	rows, err := p.env.q.GetOwnerHibernationPoolInput(p.env.ctx, p.user)
	if err != nil {
		p.t.Fatal(err)
	}
	return pickKeeper(rows, p.clock, p.svc.p.WorkerHeartbeatStale)
}

func TestWarmWakeDrainingSleeperLiveDB(t *testing.T) {
	// warmDraining warm wakes a draining, automatically admitted sleeper beside a busy keeper.
	warmDraining := func(p *poolFx, keeper, sleeper uuid.UUID, hibernated bool) {
		p.t.Helper()
		p.sleeper(sleeper, AdmitOriginAutoSurplus, hibernated)
		p.drain(sleeper)
		p.busy(keeper, time.Hour)
		if n := p.pass(keeper); n != 1 {
			p.t.Fatalf("pass = %d, want 1", n)
		}
		p.wantWarm(sleeper)
		if !p.row(sleeper).DrainingSince.Valid {
			p.t.Fatal("the warm wake itself must not clear the drain")
		}
	}

	t.Run("eligibility: a draining hibernated sleeper is the warm-wake target", func(t *testing.T) {
		p := newPoolFx(t)
		keeper, sleeper := p.member(5*time.Hour), p.member(6*time.Hour)
		warmDraining(p, keeper, sleeper, true)
	})

	t.Run("replacement pod: ready plus Register settles and clears the drain", func(t *testing.T) {
		p := newPoolFx(t)
		keeper, sleeper := p.member(5*time.Hour), p.member(6*time.Hour)
		warmDraining(p, keeper, sleeper, true)
		pod := p.readyReplacement(sleeper)
		p.registerPod(sleeper, pod, nil)
		p.want(sleeper, "none")
		row := p.row(sleeper)
		if row.DrainingSince.Valid {
			t.Fatal("Register's clear-on-roll must clear a non-maintenance drain")
		}
		if !row.WakeRegisterOpID.Valid {
			t.Fatal("the replacement settle must record the Register")
		}
		p.beatOnly(sleeper) // Register stamped the database now(); pass moved the fixture clock ahead
		if !p.ready(sleeper) {
			t.Fatal("the woken worker must be a ready keeper")
		}
		if k, ok := p.keeperOf(); !ok || k != sleeper {
			t.Fatalf("keeper = %v, %v; want the woken worker", k, ok)
		}
	})

	t.Run("replacement pod, memory-capable: ready only after a fresh fenced held report", func(t *testing.T) {
		p := newPoolFx(t)
		keeper, sleeper := p.member(5*time.Hour), p.member(6*time.Hour)
		p.makeCapable(keeper)
		m := p.makeCapable(sleeper)
		if a, b := keyOf(t, p.poolRowOf(sleeper)), keyOf(t, p.poolRowOf(keeper)); a != b {
			t.Fatalf("setup: sleeper and keeper are in different pools: %s vs %s", a, b)
		}
		warmDraining(p, keeper, sleeper, true)
		pod := p.readyReplacement(sleeper)
		m.nonce, m.epoch = p.registerPod(sleeper, pod, capableCaps()), 0
		p.want(sleeper, "none")
		if p.row(sleeper).DrainingSince.Valid {
			t.Fatal("Register's clear-on-roll must clear a non-maintenance drain")
		}
		p.beatOnly(sleeper)
		if _, ok := p.heldAt(sleeper); ok {
			t.Fatal("Register must leave no held receipt")
		}
		if p.ready(sleeper) {
			t.Fatal("a memory-capable worker without a fresh held report must not be ready")
		}
		m.claim("held")
		if _, ok := p.heldAt(sleeper); !ok {
			t.Fatal("a held report on the new registration must stamp the receipt")
		}
		p.beat(sleeper) // refreshes the heartbeat and the existing receipt to the fixture clock
		if !p.ready(sleeper) {
			t.Fatal("after a fresh fenced held report the woken worker must be a ready keeper")
		}
		if k, ok := p.keeperOf(); !ok || k != sleeper {
			t.Fatalf("keeper = %v, %v; want the woken worker", k, ok)
		}
	})

	t.Run("original pod retained: settles without Register and keeps the drain", func(t *testing.T) {
		p := newPoolFx(t)
		keeper, sleeper := p.member(5*time.Hour), p.member(6*time.Hour)
		warmDraining(p, keeper, sleeper, false) // preparing: withdrawn, never scaled down
		row := p.settleSamePod(sleeper)
		if row.WakeRegisterOpID.Valid {
			t.Fatal("the same-pod settle must not need a Register")
		}
		if !row.DrainingSince.Valid {
			t.Fatal("without a Register the drain must stay")
		}
		p.beatOnly(sleeper)
		if p.ready(sleeper) {
			t.Fatal("a draining worker must not be a ready keeper")
		}
		if k, ok := p.keeperOf(); ok {
			t.Fatalf("keeper = %v; want none", k)
		}
	})
}
