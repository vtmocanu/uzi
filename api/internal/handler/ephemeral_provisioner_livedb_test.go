package handler

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/hostedsvc"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// The end-to-end live-DB proof of PRD #529 M2's provisioner: the real
// EphemeralProvisioner running its real transaction against a real Postgres. It lives in
// the handler package (not hostedsvc) because ./e2e/run-store-it.sh only sweeps the store
// and handler packages for the LiveDB suffix, and the provisioner needs a pool + box +
// settings that a fake cannot supply — the advisory lock, the cap TOCTOU, the SealJoinToken
// co-write, and the 23505 one-per-run skip are all invisible to a fake store.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

type ephemeralFixture struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	q      *store.Queries
	box    *secretbox.Box
	userID uuid.UUID
	repoID uuid.UUID
	iid    int64
}

// newEphemeralFixture spins a migrated pool and seeds one user (opted into ephemeral
// auto-provisioning unless optIn=false) + a forge connection + repo.
func newEphemeralFixture(t *testing.T, optIn bool) *ephemeralFixture {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
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

	fx := &ephemeralFixture{t: t, ctx: ctx, pool: pool, q: store.New(pool), box: newHandlerTestBox(t), userID: uuid.New(), repoID: uuid.New()}
	connID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, ephemeral_workers_enabled) VALUES ($1, $2, 'x', $3)`,
		fx.userID, fmt.Sprintf("eph-%s@e2e", fx.userID), optIn); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, fx.userID, []byte{0x1}); err != nil {
		t.Fatalf("seed connection: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, fx.repoID, connID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	return fx
}

func (fx *ephemeralFixture) nextIID() int64 { fx.iid++; return fx.iid }

// onlineWorker inserts an online worker under the fixture user; docker toggles
// docker_enabled.
func (fx *ephemeralFixture) onlineWorker(name string, docker bool) uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	a, b := uuid.New(), uuid.New()
	if _, err := fx.pool.Exec(fx.ctx,
		`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, docker_enabled)
		 VALUES ($1, $2, $3, $4, 'online', now(), $5)`,
		id, fx.userID, name, append(a[:], b[:]...), docker); err != nil {
		fx.t.Fatalf("seed worker: %v", err)
	}
	return id
}

// queuedRun inserts a fresh queued, unowned run carrying required_capabilities.
func (fx *ephemeralFixture) queuedRun(caps []string) uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	if _, err := fx.pool.Exec(fx.ctx,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id, required_capabilities)
		 VALUES ($1, $2, $3, $4, 't', 'd', 'queued', NULL, $5)`,
		id, fx.userID, fx.repoID, fx.nextIID(), caps); err != nil {
		fx.t.Fatalf("seed queued run: %v", err)
	}
	return id
}

// provisioner builds the real provisioner with a settings cache reflecting the instance
// kill-switch and the given cap/default size. It delegates to provisionerWithDelay with
// delay 0 (past by construction), so the existing capability-gap tests are unchanged.
func (fx *ephemeralFixture) provisioner(instanceEnabled bool, maxPerUser int) *hostedsvc.EphemeralProvisioner {
	return fx.provisionerWithDelay(instanceEnabled, maxPerUser, 0)
}

// provisionerWithDelay is provisioner plus the saturation debounce knob. provisioner
// delegates with delay 0 (past by construction) so the existing capability-gap tests are
// unchanged; the saturation tests flip this to gate the debounce deterministically without
// any wall-clock sleep.
func (fx *ephemeralFixture) provisionerWithDelay(instanceEnabled bool, maxPerUser int, saturationDelay time.Duration) *hostedsvc.EphemeralProvisioner {
	sc := settings.New(&settingsStore{rows: []store.AppSetting{
		{Key: settings.KeyEphemeralWorkersEnabled, Value: fmt.Sprintf("%t", instanceEnabled)},
	}}, time.Minute)
	return hostedsvc.NewEphemeralProvisioner(fx.pool, fx.q, fx.box, sc, hostedsvc.EphemeralConfig{
		MaxPerUser:      maxPerUser,
		DefaultSize:     "m",
		SaturationDelay: saturationDelay,
	})
}

// onlineWorkerWithCap inserts an online worker with an explicit max_concurrent_runs cap
// (the sibling onlineWorker leaves it NULL = unbounded). docker toggles docker_enabled.
func (fx *ephemeralFixture) onlineWorkerWithCap(name string, docker bool, maxConcurrent int) uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	a, b := uuid.New(), uuid.New()
	if _, err := fx.pool.Exec(fx.ctx,
		`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, docker_enabled, max_concurrent_runs)
		 VALUES ($1, $2, $3, $4, 'online', now(), $5, $6)`,
		id, fx.userID, name, append(a[:], b[:]...), docker, maxConcurrent); err != nil {
		fx.t.Fatalf("seed capped worker: %v", err)
	}
	return id
}

// activeRunOn seeds a running (slot-occupying) run bound to workerID, so the worker's
// free-slot count drops by one. kind defaults to 'issue' (non-chat), which the free-slot
// subquery counts.
func (fx *ephemeralFixture) activeRunOn(workerID uuid.UUID) uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	if _, err := fx.pool.Exec(fx.ctx,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id)
		 VALUES ($1, $2, $3, $4, 't', 'd', 'running', $5)`,
		id, fx.userID, fx.repoID, fx.nextIID(), workerID); err != nil {
		fx.t.Fatalf("seed active run: %v", err)
	}
	return id
}

// ephemeralRows reads the fixture user's ephemeral worker rows as (runID -> row).
type ephRow struct {
	id       uuid.UUID
	runID    uuid.UUID
	docker   bool
	template string
	size     string
}

func (fx *ephemeralFixture) ephemeralRows() []ephRow {
	fx.t.Helper()
	rows, err := fx.pool.Query(fx.ctx,
		`SELECT id, ephemeral_run_id, docker_enabled, template_declared, hosted_size
		   FROM workers WHERE user_id = $1 AND ephemeral ORDER BY created_at`, fx.userID)
	if err != nil {
		fx.t.Fatalf("read ephemeral rows: %v", err)
	}
	defer rows.Close()
	var out []ephRow
	for rows.Next() {
		var r ephRow
		if err := rows.Scan(&r.id, &r.runID, &r.docker, &r.template, &r.size); err != nil {
			fx.t.Fatalf("scan ephemeral row: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// TestEphemeralProvisionPassSuccessLiveDB: the headline M2 success criterion.
func TestEphemeralProvisionPassSuccessLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, true)
	fx.onlineWorker("base-only", false) // base fleet cannot satisfy docker
	runID := fx.queuedRun([]string{"docker"})

	prov := fx.provisioner(true, 2)
	// NOTE: ProvisionPass returns a GLOBAL created count (it sweeps ALL users), and the
	// store-IT harness shares one Postgres across the store+handler packages, so sibling
	// tests' leftover opted-in unplaceable runs inflate that int. Assert on USER-SCOPED
	// effects (fx.ephemeralRows() is WHERE user_id = fx.userID) instead — robust to the
	// shared DB while still failing on a real regression for this fixture's own user.
	if _, err := prov.ProvisionPass(fx.ctx); err != nil {
		t.Fatalf("ProvisionPass: %v", err)
	}
	rows := fx.ephemeralRows()
	if len(rows) != 1 {
		t.Fatalf("ephemeral rows for fixture user = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.runID != runID {
		t.Errorf("ephemeral_run_id = %s, want %s", r.runID, runID)
	}
	if !r.docker {
		t.Errorf("docker_enabled = false, want true (the run needed docker)")
	}
	if r.template != "base" {
		t.Errorf("template = %q, want base", r.template)
	}
	if r.size != "m" {
		t.Errorf("hosted_size = %q, want m (the configured default)", r.size)
	}

	// The join token was sealed in the provision tx (co-write): a pending token row must
	// exist for the worker, or the worker could never be delivered one.
	var tokens int
	if err := fx.pool.QueryRow(fx.ctx, `SELECT count(*) FROM hosted_worker_tokens WHERE worker_id = $1`, r.id).Scan(&tokens); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if tokens != 1 {
		t.Errorf("pending token rows = %d, want 1 — SealJoinToken did not land in the provision tx", tokens)
	}

	// A second pass provisions nothing new for THIS user: the run is already bound (trigger
	// skip) and the unique index would reject a duplicate anyway. Assert the user-scoped row
	// count is unchanged (still exactly one, no duplicate), not the global created int.
	if _, err := prov.ProvisionPass(fx.ctx); err != nil {
		t.Fatalf("second ProvisionPass: %v", err)
	}
	if n := len(fx.ephemeralRows()); n != 1 {
		t.Errorf("ephemeral rows for fixture user after second pass = %d, want 1 (no duplicate)", n)
	}
}

// TestEphemeralProvisionPassFlagOffLiveDB: instance kill-switch off provisions nothing.
func TestEphemeralProvisionPassFlagOffLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, true) // user opted in, but instance flag off
	fx.onlineWorker("base-only", false)
	fx.queuedRun([]string{"docker"})

	prov := fx.provisioner(false, 2)
	created, err := prov.ProvisionPass(fx.ctx)
	if err != nil {
		t.Fatalf("ProvisionPass: %v", err)
	}
	if created != 0 {
		t.Errorf("created = %d, want 0 (instance kill-switch off)", created)
	}
	if n := len(fx.ephemeralRows()); n != 0 {
		t.Errorf("ephemeral rows = %d, want 0", n)
	}
}

// TestEphemeralProvisionPassUserNotOptedInLiveDB: per-user opt-in off provisions nothing
// even with the instance flag on.
func TestEphemeralProvisionPassUserNotOptedInLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, false) // NOT opted in
	fx.onlineWorker("base-only", false)
	fx.queuedRun([]string{"docker"})

	prov := fx.provisioner(true, 2)
	// Global created is contaminated by sibling tests on the shared DB; assert the
	// user-scoped effect: this not-opted-in user gets ZERO ephemeral rows.
	if _, err := prov.ProvisionPass(fx.ctx); err != nil {
		t.Fatalf("ProvisionPass: %v", err)
	}
	if n := len(fx.ephemeralRows()); n != 0 {
		t.Errorf("ephemeral rows for fixture user = %d, want 0 (user did not opt in)", n)
	}
}

// TestEphemeralProvisionPassOverCapLiveDB: a user already at the concurrent cap gets no
// new ephemeral worker.
func TestEphemeralProvisionPassOverCapLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, true)
	fx.onlineWorker("base-only", false)

	// Pre-seed one ephemeral worker (bound to an unrelated run) so the user sits at a
	// cap of 1.
	otherRun := fx.queuedRun([]string{"docker"})
	a, b := uuid.New(), uuid.New()
	if _, err := fx.pool.Exec(fx.ctx,
		`INSERT INTO workers (user_id, name, token_hash, template_declared, kind, hosted_size, docker_enabled, ephemeral, ephemeral_run_id)
		 VALUES ($1, 'pre-ephemeral', $2, 'base', 'hosted', 'm', true, true, $3)`,
		fx.userID, append(a[:], b[:]...), otherRun); err != nil {
		t.Fatalf("seed pre-existing ephemeral worker: %v", err)
	}

	// A fresh unplaceable run for the same user.
	fx.queuedRun([]string{"docker"})

	prov := fx.provisioner(true, 1) // cap = 1, already at 1
	// Global created sweeps all users and is contaminated on the shared DB; assert the
	// user-scoped effect: an at-cap user gains NO new ephemeral row (still exactly the one
	// pre-seeded). This holds whether the run is filtered by the trigger query's fairness
	// cap or rejected by provisionOne's advisory-locked cap check — both leave one row.
	if _, err := prov.ProvisionPass(fx.ctx); err != nil {
		t.Fatalf("ProvisionPass: %v", err)
	}
	if n := len(fx.ephemeralRows()); n != 1 {
		t.Errorf("ephemeral rows for fixture user = %d, want 1 (only the pre-seeded one; user at cap)", n)
	}
}

// TestEphemeralProvisionPassPlaceableRunLiveDB: a run an online docker worker can claim
// is not provisioned for.
func TestEphemeralProvisionPassPlaceableRunLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, true)
	fx.onlineWorker("docker-capable", true) // satisfies docker
	fx.queuedRun([]string{"docker"})

	prov := fx.provisioner(true, 2)
	// Global created is contaminated by sibling tests on the shared DB; assert the
	// user-scoped effect: this user's placeable run yields ZERO ephemeral rows.
	if _, err := prov.ProvisionPass(fx.ctx); err != nil {
		t.Fatalf("ProvisionPass: %v", err)
	}
	if n := len(fx.ephemeralRows()); n != 0 {
		t.Errorf("ephemeral rows for fixture user = %d, want 0 (an online docker worker can claim the run)", n)
	}
}

// TestEphemeralProvisionPassSaturationBurstLiveDB: a capability-placeable but slot-blocked
// run past the debounce provisions exactly one run-bound worker; the SAME run under the
// debounce provisions nothing (the flip proves the zero is the debounce). (M4 a+b)
func TestEphemeralProvisionPassSaturationBurstLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, true)
	w := fx.onlineWorkerWithCap("docker-full", true, 1) // capable of docker, cap 1
	fx.activeRunOn(w)                                   // 1 active -> 0 free slots -> saturated
	runID := fx.queuedRun([]string{"docker"})           // placeable by w, but w is full

	// Under the debounce (delay 1h, run is fresh): provisions nothing.
	blocked := fx.provisionerWithDelay(true, 2, time.Hour)
	if _, err := blocked.ProvisionPass(fx.ctx); err != nil {
		t.Fatalf("ProvisionPass (blocked): %v", err)
	}
	if n := len(fx.ephemeralRows()); n != 0 {
		t.Fatalf("under debounce: ephemeral rows = %d, want 0", n)
	}

	// Flip ONE variable — delay 0 (past by construction), same run + fleet: provisions one.
	burst := fx.provisionerWithDelay(true, 2, 0)
	if _, err := burst.ProvisionPass(fx.ctx); err != nil {
		t.Fatalf("ProvisionPass (burst): %v", err)
	}
	rows := fx.ephemeralRows()
	if len(rows) != 1 {
		t.Fatalf("past debounce: ephemeral rows = %d, want 1", len(rows))
	}
	if rows[0].runID != runID {
		t.Errorf("ephemeral_run_id = %s, want %s", rows[0].runID, runID)
	}
	if !rows[0].docker {
		t.Errorf("docker_enabled = false, want true (the run needed docker)")
	}
	if rows[0].template != "base" {
		t.Errorf("template = %q, want base", rows[0].template)
	}
	if rows[0].size != "m" {
		t.Errorf("hosted_size = %q, want m (configured default)", rows[0].size)
	}
}

// TestEphemeralProvisionPassSaturationFreeSlotLiveDB: a capable worker with a FREE slot
// makes the run placeable -> nothing; filling that slot (flip one variable) makes it
// slot-blocked -> provisions. (M4 c)
func TestEphemeralProvisionPassSaturationFreeSlotLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, true)
	w := fx.onlineWorkerWithCap("docker-cap2", true, 2) // capable, cap 2
	fx.activeRunOn(w)                                   // 1 active -> 1 free slot remains
	fx.queuedRun([]string{"docker"})

	// One free slot: not slot-blocked -> nothing (delay 0 so only the free slot gates it).
	prov := fx.provisionerWithDelay(true, 2, 0)
	if _, err := prov.ProvisionPass(fx.ctx); err != nil {
		t.Fatalf("ProvisionPass (free slot): %v", err)
	}
	if n := len(fx.ephemeralRows()); n != 0 {
		t.Fatalf("with a free slot: ephemeral rows = %d, want 0 (run is placeable)", n)
	}

	// Flip ONE variable — occupy the second slot so the worker is full -> provisions.
	fx.activeRunOn(w) // 2 active -> 0 free
	if _, err := prov.ProvisionPass(fx.ctx); err != nil {
		t.Fatalf("ProvisionPass (now full): %v", err)
	}
	if n := len(fx.ephemeralRows()); n != 1 {
		t.Fatalf("with no free slot: ephemeral rows = %d, want 1", n)
	}
}

// TestEphemeralProvisionPassSaturationKillSwitchLiveDB: instance kill-switch off gates the
// saturation path too; flipping it on (one variable) provisions. (M4 d)
func TestEphemeralProvisionPassSaturationKillSwitchLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, true)
	w := fx.onlineWorkerWithCap("docker-full", true, 1)
	fx.activeRunOn(w)
	fx.queuedRun([]string{"docker"})

	off := fx.provisionerWithDelay(false, 2, 0) // kill-switch OFF
	if _, err := off.ProvisionPass(fx.ctx); err != nil {
		t.Fatalf("ProvisionPass (kill-switch off): %v", err)
	}
	if n := len(fx.ephemeralRows()); n != 0 {
		t.Fatalf("kill-switch off: ephemeral rows = %d, want 0", n)
	}

	on := fx.provisionerWithDelay(true, 2, 0) // flip ON
	if _, err := on.ProvisionPass(fx.ctx); err != nil {
		t.Fatalf("ProvisionPass (kill-switch on): %v", err)
	}
	if n := len(fx.ephemeralRows()); n != 1 {
		t.Fatalf("kill-switch on: ephemeral rows = %d, want 1", n)
	}
}

// TestEphemeralProvisionPassSaturationNotOptedInLiveDB: a not-opted-in user gets no
// saturation burst; opting in (one variable) provisions. (M4 e)
func TestEphemeralProvisionPassSaturationNotOptedInLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, false) // NOT opted in
	w := fx.onlineWorkerWithCap("docker-full", true, 1)
	fx.activeRunOn(w)
	fx.queuedRun([]string{"docker"})

	prov := fx.provisionerWithDelay(true, 2, 0)
	if _, err := prov.ProvisionPass(fx.ctx); err != nil {
		t.Fatalf("ProvisionPass (not opted in): %v", err)
	}
	if n := len(fx.ephemeralRows()); n != 0 {
		t.Fatalf("not opted in: ephemeral rows = %d, want 0", n)
	}

	// Flip ONE variable — opt the user in -> provisions.
	if _, err := fx.pool.Exec(fx.ctx, `UPDATE users SET ephemeral_workers_enabled = true WHERE id = $1`, fx.userID); err != nil {
		t.Fatalf("opt user in: %v", err)
	}
	if _, err := prov.ProvisionPass(fx.ctx); err != nil {
		t.Fatalf("ProvisionPass (opted in): %v", err)
	}
	if n := len(fx.ephemeralRows()); n != 1 {
		t.Fatalf("opted in: ephemeral rows = %d, want 1", n)
	}
}

// TestEphemeralProvisionPassSaturationOverCapLiveDB: a user already at the per-user
// ephemeral cap gets no saturation burst; raising the cap (one variable) provisions. (M4 f)
func TestEphemeralProvisionPassSaturationOverCapLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, true)
	w := fx.onlineWorkerWithCap("docker-full", true, 1)
	fx.activeRunOn(w)

	// Pre-seed one ephemeral worker bound to an unrelated run so the user sits at cap 1.
	otherRun := fx.queuedRun([]string{"docker"})
	a, b := uuid.New(), uuid.New()
	if _, err := fx.pool.Exec(fx.ctx,
		`INSERT INTO workers (user_id, name, token_hash, template_declared, kind, hosted_size, docker_enabled, ephemeral, ephemeral_run_id)
		 VALUES ($1, 'pre-ephemeral', $2, 'base', 'hosted', 'm', true, true, $3)`,
		fx.userID, append(a[:], b[:]...), otherRun); err != nil {
		t.Fatalf("seed pre-existing ephemeral worker: %v", err)
	}
	fx.queuedRun([]string{"docker"}) // a fresh saturation-eligible run

	atCap := fx.provisionerWithDelay(true, 1, 0) // cap 1, already at 1
	if _, err := atCap.ProvisionPass(fx.ctx); err != nil {
		t.Fatalf("ProvisionPass (at cap): %v", err)
	}
	if n := len(fx.ephemeralRows()); n != 1 {
		t.Fatalf("at cap: ephemeral rows = %d, want 1 (only the pre-seeded)", n)
	}

	// Flip ONE variable — raise the cap to 2 -> the fresh run provisions.
	raised := fx.provisionerWithDelay(true, 2, 0)
	if _, err := raised.ProvisionPass(fx.ctx); err != nil {
		t.Fatalf("ProvisionPass (cap raised): %v", err)
	}
	if n := len(fx.ephemeralRows()); n != 2 {
		t.Fatalf("cap raised: ephemeral rows = %d, want 2", n)
	}
}

// TestEphemeralProvisionPassSaturationPlainRunLiveDB: a PLAIN run (zero required
// capabilities) under saturation provisions (Decision 2 — no cardinality>0 guard); the
// same run under the debounce provisions nothing. (M4 g)
func TestEphemeralProvisionPassSaturationPlainRunLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, true)
	w := fx.onlineWorkerWithCap("base-full", false, 1) // plain base worker, cap 1
	fx.activeRunOn(w)                                  // saturated
	runID := fx.queuedRun([]string{})                  // zero required capabilities (NOT NULL '{}')

	blocked := fx.provisionerWithDelay(true, 2, time.Hour)
	if _, err := blocked.ProvisionPass(fx.ctx); err != nil {
		t.Fatalf("ProvisionPass (blocked): %v", err)
	}
	if n := len(fx.ephemeralRows()); n != 0 {
		t.Fatalf("plain run under debounce: ephemeral rows = %d, want 0", n)
	}

	burst := fx.provisionerWithDelay(true, 2, 0)
	if _, err := burst.ProvisionPass(fx.ctx); err != nil {
		t.Fatalf("ProvisionPass (burst): %v", err)
	}
	rows := fx.ephemeralRows()
	if len(rows) != 1 {
		t.Fatalf("plain run past debounce: ephemeral rows = %d, want 1", len(rows))
	}
	if rows[0].runID != runID {
		t.Errorf("ephemeral_run_id = %s, want %s", rows[0].runID, runID)
	}
	if rows[0].docker {
		t.Errorf("docker_enabled = true, want false (plain run)")
	}
	if rows[0].template != "base" {
		t.Errorf("template = %q, want base (plain run)", rows[0].template)
	}
}

// TestEphemeralProvisionBindModeLiveDB pins issue #804 in the REAL provisioner path
// (ProvisionPass → provisionOne against real Postgres): an auto-provisioned burst worker
// defaults to anthropic_bind_mode 'auto' ONLY when its owner has a non-empty auto-select
// pool (≥1 auto_eligible anthropic_token), and 'default' otherwise — so an auto worker
// never parks its run in pool_wait on an empty pool. This is the CI-gating home for the
// decision (run-store-it.sh sweeps `-run 'LiveDB$'` over the handler package); the
// store-package LiveDB tests separately pin the UserHasAutoEligibleAnthropicToken read and
// the CreateEphemeralHostedWorker persistence it composes.
func TestEphemeralProvisionBindModeLiveDB(t *testing.T) {
	insertToken := func(fx *ephemeralFixture, label string, first bool) uuid.UUID {
		fx.t.Helper()
		row, err := fx.q.InsertUserSecret(fx.ctx, store.InsertUserSecretParams{
			UserID: fx.userID, Kind: store.KindAnthropicToken, Label: label, WantDefault: first,
			Ciphertext: []byte("ct-" + label), SealedWith: store.SealedWithMaster,
		})
		if err != nil {
			fx.t.Fatalf("insert token %s: %v", label, err)
		}
		return row.ID
	}
	// provisionAndReadMode drives a capability-gap provision for the fixture user (a docker
	// run its base-only fleet cannot serve) and returns the created worker's persisted
	// anthropic_bind_mode.
	provisionAndReadMode := func(fx *ephemeralFixture) string {
		fx.t.Helper()
		fx.onlineWorker("base-only", false) // base fleet cannot satisfy docker
		runID := fx.queuedRun([]string{"docker"})
		if _, err := fx.provisioner(true, 2).ProvisionPass(fx.ctx); err != nil {
			fx.t.Fatalf("ProvisionPass: %v", err)
		}
		var mode string
		if err := fx.pool.QueryRow(fx.ctx,
			`SELECT anthropic_bind_mode FROM workers WHERE ephemeral_run_id = $1 AND ephemeral`, runID).Scan(&mode); err != nil {
			fx.t.Fatalf("read bind mode: %v", err)
		}
		return mode
	}

	// An owner with ≥1 auto_eligible token (here: a born-eligible first token plus a second
	// opted-out token) → the pool is non-empty → auto.
	t.Run("eligible_token_owner_auto", func(t *testing.T) {
		fx := newEphemeralFixture(t, true)
		insertToken(fx, "default", true)      // first token, born auto_eligible (issue #804)
		insertToken(fx, "console-key", false) // a second, non-eligible token; pool still non-empty
		if got := provisionAndReadMode(fx); got != workersvc.BindModeAuto {
			t.Errorf("anthropic_bind_mode = %q, want %q (owner has ≥1 auto_eligible token)", got, workersvc.BindModeAuto)
		}
	})

	// An owner whose ONLY token is not eligible (born eligible, then opted out) → empty pool
	// → default, so the run is never parked in pool_wait.
	t.Run("opted_out_only_owner_default", func(t *testing.T) {
		fx := newEphemeralFixture(t, true)
		tok := insertToken(fx, "default", true)
		if _, err := fx.q.SetUserSecretAutoEligible(fx.ctx, store.SetUserSecretAutoEligibleParams{
			ID: tok, UserID: fx.userID, Kind: store.KindAnthropicToken, AutoEligible: false,
		}); err != nil {
			t.Fatalf("opt token out: %v", err)
		}
		if got := provisionAndReadMode(fx); got != workersvc.BindModeDefault {
			t.Errorf("anthropic_bind_mode = %q, want %q (empty auto-select pool must not select auto)", got, workersvc.BindModeDefault)
		}
	})

	// A single-token owner, eligible purely via the born-eligible first-token insert (no
	// toggle) → auto. This is the headline #804 case: single-token users get auto for free.
	t.Run("single_token_owner_auto", func(t *testing.T) {
		fx := newEphemeralFixture(t, true)
		insertToken(fx, "default", true) // sole token, born auto_eligible via insert
		if got := provisionAndReadMode(fx); got != workersvc.BindModeAuto {
			t.Errorf("anthropic_bind_mode = %q, want %q (single born-eligible token)", got, workersvc.BindModeAuto)
		}
	})
}
