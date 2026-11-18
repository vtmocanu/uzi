package hostedsvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Issue #4395 M2 controls: a warm wake must leave the existing demand-wake and ephemeral
// provisioning behaviour unchanged (PRD D7). The warm wake itself is written by
// workersvc.HibernationAutoPass; the demand waker and the provisioner run as in production.
// Skipped without UZI_TEST_DATABASE_URL (run via ./e2e/run-store-it.sh).

// surplusAutoSettings turns hibernation, automatic mode and surplus mode on.
type surplusAutoSettings struct{}

func (surplusAutoSettings) WorkerHibernationEnabled(context.Context) (bool, error) { return true, nil }
func (surplusAutoSettings) WorkerHibernationAuto(context.Context) (bool, error)    { return true, nil }
func (surplusAutoSettings) WorkerHibernationIdleAfter(context.Context) (time.Duration, error) {
	return 336 * time.Hour, nil
}
func (surplusAutoSettings) WorkerHibernationSurplus(context.Context) (bool, error) { return true, nil }
func (surplusAutoSettings) WorkerHibernationSurplusIdleAfter(context.Context) (time.Duration, error) {
	return 2 * time.Hour, nil
}

// poolMember seeds a registered persistent hosted worker of the owner's one pool (equal key:
// template, size, docker, no capabilities, one slot).
func (e *wakeEnv) poolMember(status string, heartbeatAgo time.Duration) uuid.UUID {
	e.t.Helper()
	id, pod := uuid.New(), uuid.New()
	e.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, kind, hosted_size, template_declared, docker_enabled,
	                             max_concurrent_runs, registered_pod_uid, created_at, last_heartbeat_at)
	        VALUES ($1, $2, $3, $4, $5, 'hosted', 'm', 'base', false, 1, $6, now() - interval '10 hours', now() - $7::interval)`,
		id, e.user, "member-"+id.String(), append(id[:], id[:]...), status, pod, heartbeatAgo.String())
	return id
}

// autoSleeper seeds a hibernated pool member whose op the automatic pass admitted.
func (e *wakeEnv) autoSleeper() uuid.UUID {
	e.t.Helper()
	id := e.poolMember("offline", time.Hour)
	op := uuid.New()
	e.exec(`UPDATE workers SET hibernation_state = 'hibernated', hibernation_op_id = $2, hibernation_origin_op_id = $2,
	        hibernation_admit_origin = 'auto_surplus', hibernation_requested_at = now() - interval '1 hour',
	        hibernated_at = now() - interval '1 hour' WHERE id = $1`, id, op)
	return id
}

func (e *wakeEnv) warmPass() {
	e.t.Helper()
	e.svc.SetHibernationSettings(surplusAutoSettings{})
	e.svc.SetHibernationAutoSettings(surplusAutoSettings{})
	if _, err := e.svc.HibernationAutoPass(e.ctx); err != nil {
		e.t.Fatalf("HibernationAutoPass: %v", err)
	}
}

func (e *wakeEnv) wakeOrigin(w uuid.UUID) string {
	e.t.Helper()
	var o string
	if err := e.pool.QueryRow(e.ctx, `SELECT COALESCE(hibernation_wake_origin, '') FROM workers WHERE id = $1`, w).Scan(&o); err != nil {
		e.t.Fatalf("wake origin: %v", err)
	}
	return o
}

// A queued run that independently qualifies for a demand wake (no online capable worker) attaches
// to the warm-waking member: it gets its own receipt, the member's origin is upgraded to demand,
// and no second worker is woken.
func TestWarmWakeAttachesAQueuedRunThatQualifiesLiveDB(t *testing.T) {
	e := newWakeEnv(t, false)
	e.poolMember("offline", time.Hour) // the keeper cannot serve: offline
	sleeper := e.autoSleeper()
	other := e.autoSleeper()
	run := e.queuedRun()

	e.warmPass()
	warmed := sleeper
	if e.state(sleeper) != "waking" {
		warmed = other
	}
	if e.state(warmed) != "waking" || e.wakeOrigin(warmed) != "warm" {
		t.Fatalf("no warm wake: %q origin %q", e.state(warmed), e.wakeOrigin(warmed))
	}
	if e.wakeRows() != 0 {
		t.Fatal("the warm wake wrote a demand receipt")
	}
	e.wake()
	if got := e.count(`SELECT count(*) FROM hibernation_wakes WHERE run_id = $1 AND worker_id = $2`, run, warmed); got != 1 {
		t.Fatalf("the queued run did not attach to the warm-waking member (rows %d)", got)
	}
	if e.wakeOrigin(warmed) != "demand" {
		t.Fatalf("origin after attach = %q, want demand", e.wakeOrigin(warmed))
	}
	cold := other
	if warmed == other {
		cold = sleeper
	}
	if e.state(cold) != "hibernated" {
		t.Fatalf("a second sleeper was woken: %q", e.state(cold))
	}
}

// With an online but full keeper a queued run does not attach to the warm-waking member (an
// online capable worker is coverage even when it is full), and the saturation-driven ephemeral
// provisioning is not suppressed by the warm wake: only the run's own receipt suppresses it.
func TestWarmWakeDoesNotSuppressProvisioningForAFullKeeperLiveDB(t *testing.T) {
	e := newWakeEnv(t, true)
	e.prov.cfg.SaturationDelay = time.Nanosecond
	keeper := e.poolMember("online", 0)
	e.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, claim_generation)
	        VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'running', $5, 1)`, uuid.New(), e.user, e.repo, wakeIID.Add(1), keeper)
	sleeper := e.autoSleeper()
	run := e.queuedRun()

	e.warmPass()
	if e.state(sleeper) != "waking" || e.wakeOrigin(sleeper) != "warm" {
		t.Fatalf("no warm wake: %q origin %q", e.state(sleeper), e.wakeOrigin(sleeper))
	}
	e.wake()
	if got := e.count(`SELECT count(*) FROM hibernation_wakes WHERE run_id = $1`, run); got != 0 {
		t.Fatalf("the run attached to the warm-waking member although a full online worker covers it (rows %d)", got)
	}
	if e.wakeOrigin(sleeper) != "warm" {
		t.Fatalf("origin = %q, want warm: nothing joined", e.wakeOrigin(sleeper))
	}
	// ProvisionPass counts every owner's provisioning in the database (a saturation delay of one
	// nanosecond makes queued runs left by other tests eligible too), so only this owner's ephemeral
	// workers are asserted.
	e.provision()
	if got := e.ephemeralWorkers(); got != 1 {
		t.Fatalf("ephemeral workers of this owner = %d, want 1: a warm wake alone must not suppress provisioning", got)
	}
}
