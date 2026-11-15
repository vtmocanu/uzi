package hostedsvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// autoSettings backs the automatic-hibernation pass (issue #4207): hibernation and its
// automatic pass on, idle threshold 14 days.
type autoSettings struct{}

func (autoSettings) WorkerHibernationEnabled(context.Context) (bool, error) { return true, nil }
func (autoSettings) WorkerHibernationAuto(context.Context) (bool, error)    { return true, nil }
func (autoSettings) WorkerHibernationIdleAfter(context.Context) (time.Duration, error) {
	return 14 * 24 * time.Hour, nil
}

// TestHibernationAutoThenScheduleWakeLiveDB is PRD #4207 examples 1 and 2 at the api level: the
// automatic pass admits the worker idle 15 days and not its sibling idle 13 days; the
// controller's transitions take it to hibernated; a queued schedule-triggered run of the owner
// is woken by the real HibernationWaker; the replacement pod's registration settles the wake;
// the run is claimed and finished on the worker; the next pass does not admit it (the clock
// restarted at the run's finished_at) and a pass at finished_at+14d+1m does.
//
// Substitution: the run's claim and completion are UPDATEs to the run row (status running with
// worker_id, then completed with finished_at), not the claim endpoint. The worker has no prune
// capability, so no hibernation cache cleanup attempt is issued.
func TestHibernationAutoThenScheduleWakeLiveDB(t *testing.T) {
	e := newWakeEnv(t, false)
	clock := time.Now().UTC().Truncate(time.Second)
	e.svc.SetHibernationSettings(autoSettings{})
	e.svc.SetHibernationAutoSettings(autoSettings{})
	e.svc.SetClock(func() time.Time { return clock })
	advertise := func() {
		t.Helper()
		if err := e.q.UpsertControllerHibernationAdvertisement(e.ctx, store.UpsertControllerHibernationAdvertisementParams{
			AdvertisedAt:        pgtype.Timestamptz{Time: clock, Valid: true},
			HibernationVersion:  pgtype.Int4{Int32: 1, Valid: true},
			PollIntervalSeconds: pgtype.Int4{Int32: 300, Valid: true},
		}); err != nil {
			t.Fatalf("advertise: %v", err)
		}
	}
	advance := func(d time.Duration) { clock = clock.Add(d); advertise() }
	advertise()

	seed := func(name string, idleFor time.Duration) (uuid.UUID, uuid.UUID) {
		id, pod := uuid.New(), uuid.New()
		e.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, kind, hosted_size, template_declared, registered_pod_uid, created_at, last_heartbeat_at)
		        VALUES ($1, $2, $3, $4, 'online', 'hosted', 'm', 'base', $5, $6, now())`,
			id, e.user, name+"-"+id.String(), append(id[:], id[:]...), pod, clock.Add(-idleFor))
		return id, pod
	}
	// The database is shared across packages and never truncated, so residue workers from other
	// tests may also be candidates: every assertion below is on these two workers' own state,
	// never on the pass's admitted count.
	idle15, pod15 := seed("idle15", 15*24*time.Hour)
	idle13, _ := seed("idle13", 13*24*time.Hour)

	pass := func() int64 {
		t.Helper()
		n, err := e.svc.HibernationAutoPass(e.ctx)
		if err != nil {
			t.Fatalf("HibernationAutoPass: %v", err)
		}
		return n
	}

	// Example 1: only the worker past the threshold is admitted.
	if n := pass(); n < 1 {
		t.Fatalf("pass admitted %d, want at least the 15d worker", n)
	}
	if got := e.state(idle15); got != "preparing" {
		t.Fatalf("15d worker state = %q, want preparing", got)
	}
	if got := e.state(idle13); got != "none" {
		t.Fatalf("13d worker state = %q, want none", got)
	}

	// The controller scales it down and reports the pod gone.
	deployUID := uuid.NewString()
	transition := func(name string, pod uuid.UUID) {
		t.Helper()
		tr := workersvc.HibernationTransition{OpID: e.op(idle15).String(), Transition: name, DeploymentUID: deployUID}
		if name != workersvc.TransitionScaledDown {
			tr.ResourceVersion = "rv-" + uuid.NewString()[:8]
		}
		if name == workersvc.TransitionScalingDown || name == workersvc.TransitionReady {
			tr.PodUID = pod.String()
		}
		if _, err := e.svc.ApplyTransition(e.ctx, idle15, tr); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	transition(workersvc.TransitionScalingDown, pod15)
	transition(workersvc.TransitionScaledDown, uuid.Nil)
	if got := e.state(idle15); got != "hibernated" {
		t.Fatalf("state after scaled_down = %q, want hibernated", got)
	}

	// The sibling cannot serve the run (offline), so the schedule-triggered run wakes the
	// hibernated worker. Example 2.
	e.exec(`UPDATE workers SET status = 'offline', last_heartbeat_at = now() - interval '1 hour' WHERE id = $1`, idle13)
	run := e.queuedRun()
	e.exec(`UPDATE runs SET trigger_source = 'schedule' WHERE id = $1`, run)
	if n := e.wake(); n != 1 {
		t.Fatalf("WakePass moved %d workers, want 1", n)
	}
	if got := e.state(idle15); got != "waking" {
		t.Fatalf("state after WakePass = %q, want waking", got)
	}

	// The replacement pod is ready and registers: the wake settles and stamps the grace anchor.
	next := uuid.New()
	transition(workersvc.TransitionReady, next)
	e.exec(`UPDATE workers SET registered_pod_uid = $2, status = 'online', last_heartbeat_at = now() WHERE id = $1`, idle15, next)
	row, err := e.q.GetWorkerByID(e.ctx, idle15)
	if err != nil {
		t.Fatalf("GetWorkerByID: %v", err)
	}
	if _, _, err := e.svc.RegisterWithCrossCheckSlots(e.ctx, row, "v1", "", nil, nil, nil, nil, nil, next); err != nil {
		t.Fatalf("register: %v", err)
	}
	var state string
	var settled pgtype.Timestamptz
	if err := e.pool.QueryRow(e.ctx, `SELECT hibernation_state, last_wake_settled_at FROM workers WHERE id = $1`, idle15).Scan(&state, &settled); err != nil {
		t.Fatal(err)
	}
	if state != "none" || !settled.Valid || !settled.Time.Equal(clock) {
		t.Fatalf("after settle: state=%q last_wake_settled_at=%v, want none at %v", state, settled, clock)
	}

	// The run is claimed on the woken worker, then finishes an hour later.
	e.exec(`UPDATE runs SET status = 'running', worker_id = $2, claim_generation = 1 WHERE id = $1`, run, idle15)
	advance(2 * time.Minute)
	pass()
	if got := e.state(idle15); got != "none" {
		t.Fatalf("a worker with a running run was admitted: state %q", got)
	}
	advance(time.Hour)
	finished := clock
	e.exec(`UPDATE runs SET status = 'completed', finished_at = $2, status_since = $2 WHERE id = $1`, run, finished)

	// Right after the run finishes, the 15-day-old creation no longer counts: not admitted.
	advance(2 * time.Minute)
	pass()
	if got := e.state(idle15); got != "none" {
		t.Fatalf("right after the run finished: state %q, want none", got)
	}
	// One minute short of the threshold from finished_at: still not admitted.
	clock = finished.Add(14*24*time.Hour - time.Minute)
	advertise()
	pass()
	if got := e.state(idle15); got != "none" {
		t.Fatalf("at finished_at+14d-1m: state %q, want none", got)
	}
	// finished_at + 14d + 1m: admitted.
	clock = finished.Add(14*24*time.Hour + time.Minute)
	advertise()
	pass()
	if got := e.state(idle15); got != "preparing" {
		t.Fatalf("at finished_at+14d+1m: state %q, want preparing", got)
	}
}
