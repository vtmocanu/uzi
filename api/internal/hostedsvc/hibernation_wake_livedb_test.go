package hostedsvc

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #4205 M5: the demand-wake pass and its suppression of ephemeral provisioning, over a real
// database. Skipped without UZI_TEST_DATABASE_URL; run via ./e2e/run-store-it.sh.

type wakeSettings struct{ ephemeralOn bool }

func (s wakeSettings) EphemeralWorkersEnabled(context.Context) (bool, error) {
	return s.ephemeralOn, nil
}
func (wakeSettings) DockerRepoAllowlist(context.Context) ([]uuid.UUID, error) {
	return []uuid.UUID{}, nil
}

type hibSettingsOff struct{}

func (hibSettingsOff) WorkerHibernationEnabled(context.Context) (bool, error) { return false, nil }

type wakeEnv struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	q     *store.Queries
	svc   *workersvc.Service
	waker *HibernationWaker
	prov  *EphemeralProvisioner
	user  uuid.UUID
	repo  uuid.UUID
}

var wakeIID atomic.Int64

func newWakeEnv(t *testing.T, ephemeralOn bool) *wakeEnv {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("secretbox: %v", err)
	}
	q := store.New(pool)
	e := &wakeEnv{t: t, ctx: ctx, pool: pool, q: q, user: uuid.New(), repo: uuid.New()}
	conn := uuid.New()
	e.exec(`INSERT INTO users (id, email, password_hash, vault_mode, ephemeral_workers_enabled) VALUES ($1, $2, 'x', 'unprotected', true)`,
		e.user, fmt.Sprintf("wake-%s@e2e", e.user))
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, e.user) })
	e.exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	        VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, conn, e.user, []byte{1})
	e.exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	        VALUES ($1, $2, 1, $3, $4, 'main', true)`, e.repo, conn, "g/"+e.repo.String(), "https://forge.e2e/g/"+e.repo.String())

	e.svc = workersvc.New(q, box, workersvc.Params{WorkerHeartbeatStale: 45 * time.Second})
	e.svc.SetTxBeginner(pool)
	// Both settings are OFF in every test: the wake pass must not depend on either (D7).
	e.svc.SetHibernationSettings(hibSettingsOff{})
	e.waker = NewHibernationWaker(q, e.svc, wakeSettings{ephemeralOn: ephemeralOn}, HibernationWakerConfig{
		WorkerAffinityGrace:   2 * time.Minute,
		WorkerAffinityCeiling: 2 * time.Hour,
		WorkerHeartbeatStale:  45 * time.Second,
		BackgroundGrace:       15 * time.Minute,
	})
	e.prov = NewEphemeralProvisioner(pool, q, box, wakeSettings{ephemeralOn: ephemeralOn}, EphemeralConfig{
		WorkerAffinityGrace:   2 * time.Minute,
		WorkerAffinityCeiling: 2 * time.Hour,
		BackgroundGrace:       15 * time.Minute,
		MaxPerUser:            5,
		DefaultSize:           "m",
		ProvisionDeadline:     10 * time.Minute,
	})
	return e
}

func (e *wakeEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("exec %q: %v", sql, err)
	}
}

// sleeper seeds a hibernated persistent hosted worker.
func (e *wakeEnv) sleeper(caps, protocol []string, hibernatedAgo time.Duration) uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	if caps == nil {
		caps = []string{}
	}
	if protocol == nil {
		protocol = []string{}
	}
	e.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, kind, hosted_size, template_declared, capabilities, protocol_capabilities,
	                             hibernation_state, hibernation_op_id, hibernated_at, last_heartbeat_at)
	        VALUES ($1, $2, $3, $4, 'offline', 'hosted', 'm', 'base', $5, $6, 'hibernated', $7, now() - $8::interval, now() - interval '1 hour')`,
		id, e.user, "sleeper-"+id.String(), append(id[:], id[:]...), caps, protocol, uuid.New(), fmt.Sprintf("%d seconds", int(hibernatedAgo.Seconds())))
	return id
}

// queuedRun seeds a queued run old enough that debounces and affinity never decide.
func (e *wakeEnv) queuedRun(caps ...string) uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	if caps == nil {
		caps = []string{}
	}
	e.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, required_capabilities, status_since, updated_at)
	        VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'queued', $5, now() - interval '1 hour', now() - interval '1 hour')`,
		id, e.user, e.repo, wakeIID.Add(1), caps)
	return id
}

func (e *wakeEnv) state(w uuid.UUID) string {
	e.t.Helper()
	var s string
	if err := e.pool.QueryRow(e.ctx, `SELECT hibernation_state FROM workers WHERE id = $1`, w).Scan(&s); err != nil {
		e.t.Fatalf("state: %v", err)
	}
	return s
}

func (e *wakeEnv) op(w uuid.UUID) uuid.UUID {
	e.t.Helper()
	var op uuid.UUID
	if err := e.pool.QueryRow(e.ctx, `SELECT hibernation_op_id FROM workers WHERE id = $1`, w).Scan(&op); err != nil {
		e.t.Fatalf("op: %v", err)
	}
	return op
}

func (e *wakeEnv) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

func (e *wakeEnv) wakeRows() int {
	return e.count(`SELECT count(*) FROM hibernation_wakes hw JOIN runs r ON r.id = hw.run_id WHERE r.user_id = $1`, e.user)
}

func (e *wakeEnv) ephemeralWorkers() int {
	return e.count(`SELECT count(*) FROM workers WHERE user_id = $1 AND ephemeral`, e.user)
}

func (e *wakeEnv) wake() int64 {
	e.t.Helper()
	n, err := e.waker.WakePass(e.ctx)
	if err != nil {
		e.t.Fatalf("WakePass: %v", err)
	}
	return n
}

func (e *wakeEnv) provision() int64 {
	e.t.Helper()
	n, err := e.prov.ProvisionPass(e.ctx)
	if err != nil {
		e.t.Fatalf("ProvisionPass: %v", err)
	}
	return n
}

// checker seeds a queued plan-stage Claude cross-check child whose lead runs on a separate worker.
func (e *wakeEnv) checker() uuid.UUID {
	e.t.Helper()
	leadHost := uuid.New()
	e.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at) VALUES ($1, $2, $3, $4, 'online', now())`,
		leadHost, e.user, "lead-host-"+leadHost.String(), append(leadHost[:], leadHost[:]...))
	lead, child := e.queuedRun(), e.queuedRun()
	e.exec(`UPDATE runs SET status='running', worker_id=$2, claim_generation=1 WHERE id=$1`, lead, leadHost)
	e.exec(`UPDATE runs SET kind='cross_check', issue_iid=NULL, target_run_id=$2, harness='claude', report_only=true,
	        budget_wall_seconds=1800, required_capabilities='{}' WHERE id=$1`, child, lead)
	e.exec(`INSERT INTO cross_checks
		(lead_run_id, checker_run_id, stage, round, lead_claim_generation, plan_md, milestones, size_class, base_commit,
		 candidate_digest, checker_harness, deadline_at, automatic_rounds_enabled, automatic_revision_limit)
		VALUES ($1, $2, 'plan', 1, 1, 'plan', '[]', 's', repeat('a', 40), $3, 'claude', now() + interval '5 minutes', true, 2)`,
		lead, child, []byte("digest"))
	return child
}

func TestHibernationWakePassKindsLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name     string
		protocol []string
		seed     func(e *wakeEnv) uuid.UUID
	}{
		{"plain run", nil, func(e *wakeEnv) uuid.UUID { return e.queuedRun() }},
		{"chat run", nil, func(e *wakeEnv) uuid.UUID {
			var id uuid.UUID
			if err := e.pool.QueryRow(e.ctx, `INSERT INTO runs (user_id, issue_title, issue_description, kind) VALUES ($1, 'c', 'd', 'chat') RETURNING id`, e.user).Scan(&id); err != nil {
				t.Fatalf("chat: %v", err)
			}
			return id
		}},
		{"judge run", nil, func(e *wakeEnv) uuid.UUID {
			target := e.queuedRun()
			e.exec(`UPDATE runs SET status = 'cancelled' WHERE id = $1`, target)
			var id uuid.UUID
			if err := e.pool.QueryRow(e.ctx, `INSERT INTO runs (user_id, issue_title, issue_description, status, kind, target_run_id) VALUES ($1, 't', 'd', 'queued', 'judge', $2) RETURNING id`, e.user, target).Scan(&id); err != nil {
				t.Fatalf("judge: %v", err)
			}
			return id
		}},
		{"cross-check child", []string{"cross_check_v1", "cross_check_codex_lead_v1"}, func(e *wakeEnv) uuid.UUID { return e.checker() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Ephemeral provisioning is OFF: the pass must run whatever that setting says.
			e := newWakeEnv(t, false)
			w := e.sleeper(nil, tc.protocol, time.Hour)
			run := tc.seed(e)
			if got := e.wake(); got != 1 {
				t.Fatalf("WakePass woke %d, want 1", got)
			}
			if e.state(w) != "waking" {
				t.Fatalf("worker state = %q, want waking", e.state(w))
			}
			if e.wakeRows() != 1 {
				t.Fatalf("want one wake row for run %s", run)
			}
		})
	}
}

func TestHibernationWakePassIsNotPacedAndOverflowsToTheNextPassLiveDB(t *testing.T) {
	t.Run("12 runs on 12 workers are all woken in one pass", func(t *testing.T) {
		e := newWakeEnv(t, false)
		var workers []uuid.UUID
		for i := 0; i < 12; i++ {
			c := fmt.Sprintf("cap-%d", i)
			workers = append(workers, e.sleeper([]string{c}, nil, time.Hour))
			e.queuedRun(c)
		}
		if got := e.wake(); got != 12 {
			t.Fatalf("WakePass woke %d, want 12 (demand wakes are not paced at the 10 per pass of the setting-disable wake)", got)
		}
		for _, w := range workers {
			if e.state(w) != "waking" {
				t.Fatalf("worker %s state = %q", w, e.state(w))
			}
		}
	})

	t.Run("more demand than the scan LIMIT: the remainder is handled on the next pass", func(t *testing.T) {
		e := newWakeEnv(t, false)
		w := e.sleeper(nil, nil, time.Hour)
		total := int(ephemeralProvisionBatch) + 3
		for i := 0; i < total; i++ {
			e.queuedRun()
		}
		// One worker moves to waking; the other runs of the scan attach to it (not counted as wakes).
		if got := e.wake(); got != 1 {
			t.Fatalf("first pass woke %d workers, want 1", got)
		}
		if e.wakeRows() != int(ephemeralProvisionBatch) {
			t.Fatalf("rows after pass 1 = %d", e.wakeRows())
		}
		op := e.op(w)
		if got := e.wake(); got != 0 {
			t.Fatalf("second pass moved %d workers, want 0 (the remainder attaches to the waking worker)", got)
		}
		if e.wakeRows() != total {
			t.Fatalf("rows after pass 2 = %d, want %d", e.wakeRows(), total)
		}
		if e.op(w) != op {
			t.Fatal("the already-waking worker's op id changed across passes")
		}
	})
}

func TestHibernationWakePassNeverWakesASiblingLiveDB(t *testing.T) {
	e := newWakeEnv(t, false)
	older := e.sleeper(nil, nil, 2*time.Hour)
	newer := e.sleeper(nil, nil, time.Hour)
	e.queuedRun()
	e.wake()
	op := e.op(older)
	if e.state(older) != "waking" || e.state(newer) != "hibernated" {
		t.Fatalf("states older=%s newer=%s, want the oldest hibernated_at to be woken", e.state(older), e.state(newer))
	}
	for i := 0; i < 3; i++ {
		if got := e.wake(); got != 0 {
			t.Fatalf("repeat pass %d woke %d", i, got)
		}
	}
	if e.state(newer) != "hibernated" || e.op(older) != op || e.wakeRows() != 1 {
		t.Fatal("repeated passes woke a sibling or re-minted the op")
	}
	// A second run that arrives mid-wake attaches to the waking worker: one row more, no sibling,
	// and no worker transition to count.
	e.queuedRun()
	if got := e.wake(); got != 0 {
		t.Fatalf("late run woke %d workers, want 0", got)
	}
	if e.state(newer) != "hibernated" || e.op(older) != op || e.wakeRows() != 2 {
		t.Fatal("a run arriving mid-wake woke a sibling or re-minted the op")
	}
}

func TestHibernationWakePassKeepsRowsUntilTheRunLeavesTheQueueLiveDB(t *testing.T) {
	e := newWakeEnv(t, false)
	w := e.sleeper(nil, nil, time.Hour)
	gone, kept := e.queuedRun(), e.queuedRun()
	if got := e.wake(); got != 1 {
		t.Fatalf("woke %d workers, want 1", got)
	}
	if e.wakeRows() != 2 {
		t.Fatalf("rows = %d, want 2", e.wakeRows())
	}
	var firstWoken time.Time
	if err := e.pool.QueryRow(e.ctx, `SELECT first_woken_at FROM hibernation_wakes WHERE run_id = $1`, kept).Scan(&firstWoken); err != nil {
		t.Fatal(err)
	}
	// One run leaves the queue while the worker settles awake with the other run's row still naming
	// it. (Re-hibernating it here is the stranding sequence HibernateWorker now refuses with
	// pending_wake_demand; see TestHibernateRefusedWhileAQueuedRunWokeTheWorkerLiveDB.)
	e.exec(`UPDATE runs SET status = 'cancelled' WHERE id = $1`, gone)
	e.exec(`UPDATE workers SET hibernation_state = 'none', hibernation_op_id = NULL, hibernated_at = NULL, wake_requested_at = NULL WHERE id = $1`, w)
	for i := 0; i < 2; i++ {
		if got := e.wake(); got != 0 {
			t.Fatalf("pass %d woke %d workers; a run that already woke once must not wake again", i, got)
		}
	}
	if e.count(`SELECT count(*) FROM hibernation_wakes WHERE run_id = $1`, gone) != 0 {
		t.Fatal("the row of a run that left the queue was not released")
	}
	var after time.Time
	if err := e.pool.QueryRow(e.ctx, `SELECT first_woken_at FROM hibernation_wakes WHERE run_id = $1`, kept).Scan(&after); err != nil {
		t.Fatalf("the row of the still-queued run %s was deleted: %v", kept, err)
	}
	if !after.Equal(firstWoken) || e.state(w) != "none" {
		t.Fatalf("the still-queued run's row changed: first_woken_at %v -> %v, state=%s", firstWoken, after, e.state(w))
	}
	// A run claimed away releases its row on the next pass.
	e.exec(`UPDATE runs SET status = 'running' WHERE id = $1`, kept)
	e.wake()
	if e.wakeRows() != 0 {
		t.Fatalf("rows = %d after the run left the queue, want 0", e.wakeRows())
	}
}

func TestHibernationWakeSuppressesEphemeralProvisioningLiveDB(t *testing.T) {
	t.Run("a wake in the last 15 minutes suppresses provisioning for its run; expiry releases it", func(t *testing.T) {
		e := newWakeEnv(t, true)
		e.sleeper([]string{"jvm"}, nil, time.Hour)
		run := e.queuedRun("jvm")
		if e.wake() != 1 {
			t.Fatal("no wake")
		}
		if got := e.provision(); got != 0 || e.ephemeralWorkers() != 0 {
			t.Fatalf("provisioned %d workers although a wake covers run %s", got, run)
		}
		// A later tick inside the window is suppressed too.
		if got := e.provision(); got != 0 {
			t.Fatalf("second tick provisioned %d", got)
		}
		e.exec(`UPDATE hibernation_wakes SET first_woken_at = now() - interval '16 minutes' WHERE run_id = $1`, run)
		if got := e.provision(); got != 1 || e.ephemeralWorkers() != 1 {
			t.Fatalf("after the timeout provisioning must proceed: created %d", got)
		}
	})

	t.Run("control: without a wake the same run is provisioned (the pass order matters)", func(t *testing.T) {
		e := newWakeEnv(t, true)
		e.sleeper([]string{"jvm"}, nil, time.Hour)
		e.queuedRun("jvm")
		if got := e.provision(); got != 1 {
			t.Fatalf("provisioned %d, want 1: the run is real provisioning demand", got)
		}
	})

	t.Run("a waking worker that no longer accepts intake does not suppress provisioning", func(t *testing.T) {
		e := newWakeEnv(t, true)
		sleeper := e.sleeper([]string{"jvm"}, nil, time.Hour)
		e.queuedRun("jvm")
		if e.wake() != 1 {
			t.Fatal("no wake")
		}
		if got := e.provision(); got != 0 {
			t.Fatalf("control: provisioned %d although a healthy wake covers the run", got)
		}
		e.exec(`UPDATE workers SET manual_cordoned_at = now(), manual_cordoned_by = user_id, manual_cordoned_by_label = 'op' WHERE id = $1`, sleeper)
		if got := e.provision(); got != 1 || e.ephemeralWorkers() != 1 {
			t.Fatalf("a cordoned waking worker can never serve the run; provisioning must proceed, created %d", got)
		}
	})

	t.Run("a run needing another capability is not suppressed", func(t *testing.T) {
		e := newWakeEnv(t, true)
		e.sleeper([]string{"jvm"}, nil, time.Hour)
		covered := e.queuedRun("jvm")
		other := e.queuedRun("docker")
		e.wake()
		if got := e.provision(); got != 1 {
			t.Fatalf("provisioned %d, want exactly the run the sleeper cannot serve", got)
		}
		var bound uuid.UUID
		if err := e.pool.QueryRow(e.ctx, `SELECT ephemeral_run_id FROM workers WHERE user_id = $1 AND ephemeral`, e.user).Scan(&bound); err != nil {
			t.Fatalf("bound run: %v", err)
		}
		if bound == covered || bound != other {
			t.Fatalf("ephemeral worker bound to %s, want the uncovered run %s (covered run %s)", bound, other, covered)
		}
	})

	t.Run("a capacity change during the wake (a capable online worker that is full) stays suppressed", func(t *testing.T) {
		e := newWakeEnv(t, true)
		e.prov.cfg.SaturationDelay = time.Nanosecond
		e.sleeper([]string{"jvm"}, nil, time.Hour)
		run := e.queuedRun("jvm")
		e.wake()
		full := uuid.New()
		e.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, capabilities, max_concurrent_runs)
		        VALUES ($1, $2, $3, $4, 'online', now(), '{jvm}', 1)`, full, e.user, "full-"+full.String(), append(full[:], full[:]...))
		e.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
		        VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'running', $5)`, uuid.New(), e.user, e.repo, wakeIID.Add(1), full)
		if got := e.provision(); got != 0 || e.ephemeralWorkers() != 0 {
			t.Fatalf("the saturation trigger provisioned %d for a run a waking worker covers", got)
		}
		// Control: dropping the wake lets the saturation trigger fire, so it really was the wake.
		e.exec(`DELETE FROM hibernation_wakes WHERE run_id = $1`, run)
		if got := e.provision(); got != 1 {
			t.Fatalf("control: saturation provisioned %d, want 1", got)
		}
	})
}

// leasedEphemeral seeds an online ephemeral worker holding a live lease for run's repository and
// branch, so it can take the run through its lease.
func (e *wakeEnv) leasedEphemeral(run uuid.UUID) uuid.UUID {
	e.t.Helper()
	var iid int64
	if err := e.pool.QueryRow(e.ctx, `SELECT issue_iid FROM runs WHERE id = $1`, run).Scan(&iid); err != nil {
		e.t.Fatalf("issue_iid: %v", err)
	}
	id := uuid.New()
	e.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, kind, hosted_size, template_declared, ephemeral,
	                             last_heartbeat_at, lease_since, lease_repo_id, lease_branch)
	        VALUES ($1, $2, $3, $4, 'online', 'hosted', 'm', 'base', true, now(), now(), $5, $6)`,
		id, e.user, "leased-"+id.String(), append(id[:], id[:]...), e.repo, fmt.Sprintf("agent/issue-%d", iid))
	return id
}

func TestHibernationWakePassHonoursTheConfiguredLeaseLiveDB(t *testing.T) {
	t.Run("a leased ephemeral worker that can take the run through its lease: no sleeper is woken", func(t *testing.T) {
		e := newWakeEnv(t, false)
		e.waker.cfg.Lease = time.Hour
		sleeper := e.sleeper(nil, nil, time.Hour)
		run := e.queuedRun()
		e.leasedEphemeral(run)
		if got := e.wake(); got != 0 {
			t.Fatalf("woke %d workers although a leased worker can take the run", got)
		}
		if e.state(sleeper) != "hibernated" || e.wakeRows() != 0 {
			t.Fatalf("sleeper state=%s rows=%d, want hibernated and no wake row", e.state(sleeper), e.wakeRows())
		}
	})

	t.Run("control: with no lease configured the same run wakes the sleeper", func(t *testing.T) {
		e := newWakeEnv(t, false)
		sleeper := e.sleeper(nil, nil, time.Hour)
		run := e.queuedRun()
		e.leasedEphemeral(run)
		if got := e.wake(); got != 1 {
			t.Fatalf("woke %d workers, want 1", got)
		}
		if e.state(sleeper) != "waking" {
			t.Fatalf("sleeper state = %s, want waking", e.state(sleeper))
		}
	})
}
