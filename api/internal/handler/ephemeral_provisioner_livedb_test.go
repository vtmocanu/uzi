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
// kill-switch and the given cap/default size.
func (fx *ephemeralFixture) provisioner(instanceEnabled bool, maxPerUser int) *hostedsvc.EphemeralProvisioner {
	sc := settings.New(&settingsStore{rows: []store.AppSetting{
		{Key: settings.KeyEphemeralWorkersEnabled, Value: fmt.Sprintf("%t", instanceEnabled)},
	}}, time.Minute)
	return hostedsvc.NewEphemeralProvisioner(fx.pool, fx.q, fx.box, sc, hostedsvc.EphemeralConfig{
		MaxPerUser:  maxPerUser,
		DefaultSize: "m",
	})
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
