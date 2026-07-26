package store_test

// PRD #113 M4 — the parts of roll health that ONLY a real Postgres can answer, because
// they live in SQL rather than in Go:
//
//   * the upsert's confinement to EXISTING, HOSTED rows (an INSERT ... SELECT, so the
//     guard is the query, not a caller's check);
//   * the SET-IF-NULL on upgrading_since, including that a `settled` report does NOT
//     clear it;
//   * that RegisterWorker clears the anchor ONLY when the version actually MOVES — the
//     INV-5 invariant, expressed as a CTE so the move and the clear are one round trip;
//   * cross-tenancy: the per-user summary cannot see another user's workers, which is
//     structural here because the table has no user_id at all.
//
// The classifier's behaviour GIVEN this persistence is pinned without a database in
// workersvc/upgrade_ceiling_test.go. The split is deliberate: these tests judge the SQL,
// those judge the decision table, and neither can paper over the other.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// e2e/run-store-it.sh. A package that prints `ok` with PASS=0 is INVALID, not green.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// rollReport builds an upsert param set for one worker at one instant.
func rollReport(workerID uuid.UUID, phase string, observedAt time.Time) store.UpsertWorkerRollHealthParams {
	return store.UpsertWorkerRollHealthParams{
		WorkerID:             workerID,
		Phase:                phase,
		ObservedAt:           ts(observedAt),
		ControllerReportedAt: ts(observedAt),
		RestartCount:         0,
		WorkerImageTag:       pgtype.Text{String: "0.11.7", Valid: true},
	}
}

func TestWorkerRollHealthPersistenceLiveDB(t *testing.T) {
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
	defer pool.Close()
	q := store.New(pool)

	userID := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("roll-%s@e2e", userID))

	// ck_workers_hosted_metadata (migration 00066) requires hosted_size AND
	// template_declared on a hosted row, and forbids hosted_size on an external one.
	seedWorker := func(kind, version string) uuid.UUID {
		id := uuid.New()
		var size any
		if kind == "hosted" {
			size = "m"
		}
		mustExec(ctx, t, pool,
			`INSERT INTO workers (id, user_id, name, token_hash, status, kind, version, hosted_size, template_declared)
			 VALUES ($1, $2, $3, $4, 'offline', $5, $6, $7, 'base')`,
			id, userID, "w-"+id.String()[:8], []byte(id.String()), kind, version, size)
		return id
	}

	hosted := seedWorker("hosted", "0.11.0")
	external := seedWorker("external", "0.11.0")

	t0 := time.Now().UTC().Truncate(time.Second)

	// ---- Confinement: hosted accepts, external and unknown do not. ----
	n, err := q.UpsertWorkerRollHealth(ctx, rollReport(hosted, "rolling", t0))
	if err != nil {
		t.Fatalf("upsert hosted: %v", err)
	}
	if n != 1 {
		t.Fatalf("upsert for a hosted worker affected %d rows, want 1", n)
	}

	n, err = q.UpsertWorkerRollHealth(ctx, rollReport(external, "stuck", t0))
	if err != nil {
		t.Fatalf("upsert external: %v", err)
	}
	if n != 0 {
		t.Errorf("upsert for an EXTERNAL worker affected %d rows, want 0. The controller has no "+
			"jurisdiction over a worker its owner runs by hand; without the kind='hosted' guard it "+
			"could assert upgrade_failed with attacker-authored text against one.", n)
	}

	n, err = q.UpsertWorkerRollHealth(ctx, rollReport(uuid.New(), "stuck", t0))
	if err != nil {
		t.Fatalf("upsert unknown: %v", err)
	}
	if n != 0 {
		t.Errorf("upsert for an UNKNOWN worker id affected %d rows, want 0 — a report must never be "+
			"able to create rows, or a hostile controller grows this table without bound", n)
	}

	// ---- Liveness must be untouched. ----
	var status string
	var lastHeartbeat *time.Time
	if err := pool.QueryRow(ctx, `SELECT status, last_heartbeat_at FROM workers WHERE id = $1`, hosted).
		Scan(&status, &lastHeartbeat); err != nil {
		t.Fatalf("read worker: %v", err)
	}
	if status != "offline" || lastHeartbeat != nil {
		t.Errorf("after a roll report the worker reads status=%q last_heartbeat_at=%v; liveness is "+
			"heartbeat-owned and a report that can reach it lets a lying controller hold a dead worker "+
			"online, which is run-scheduling state", status, lastHeartbeat)
	}

	anchorAfter := func(what string) *time.Time {
		var got *time.Time
		if err := pool.QueryRow(ctx, `SELECT upgrading_since FROM worker_upgrade_reports WHERE worker_id = $1`, hosted).
			Scan(&got); err != nil {
			t.Fatalf("read upgrading_since (%s): %v", what, err)
		}
		return got
	}

	first := anchorAfter("first report")
	if first == nil {
		t.Fatalf("the first non-terminal report did not stamp upgrading_since; the ceiling has no anchor")
	}

	// ---- SET-IF-NULL: a later report keeps the ORIGINAL anchor. ----
	if _, err := q.UpsertWorkerRollHealth(ctx, rollReport(hosted, "rolling", t0.Add(10*time.Minute))); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	second := anchorAfter("second report")
	if second == nil || !second.Equal(*first) {
		t.Errorf("upgrading_since moved from %v to %v on the second report. Re-stamping it does not "+
			"weaken the ceiling, it DELETES it: a controller posting every 10s would satisfy any window "+
			"forever.", first, second)
	}

	// ---- A `settled` report must NOT clear the anchor. ----
	if _, err := q.UpsertWorkerRollHealth(ctx, rollReport(hosted, "settled", t0.Add(20*time.Minute))); err != nil {
		t.Fatalf("settled upsert: %v", err)
	}
	if got := anchorAfter("settled report"); got == nil {
		t.Errorf("a `settled` report cleared upgrading_since. That hands the reset back to the " +
			"controller — report settled once, then resume lying, and the clock restarts — which is " +
			"exactly the forgeability the anchor exists to prevent. Only a register may clear it.")
	}

	// ---- The clear: register with the SAME version must NOT clear. ----
	if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{
		ID: hosted, Version: pgtype.Text{String: "0.11.0", Valid: true},
	}); err != nil {
		t.Fatalf("register same version: %v", err)
	}
	if got := anchorAfter("register, unchanged version"); got == nil {
		t.Errorf("registering with an UNCHANGED version cleared the anchor. A register is evidence the " +
			"pod came back; only a version MOVE is evidence the roll completed. Clearing on any register " +
			"opens an unbounded re-arm path, and it is most available exactly where a stuck worker lives: " +
			"a crash-looping agent re-registers on every start.")
	}

	// ---- The clear: register with a MOVED version clears. ----
	if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{
		ID: hosted, Version: pgtype.Text{String: "0.11.7", Valid: true},
	}); err != nil {
		t.Fatalf("register moved version: %v", err)
	}
	if got := anchorAfter("register, moved version"); got != nil {
		t.Errorf("upgrading_since = %v after the worker re-registered on a NEW version; the roll "+
			"completed, so the anchor must clear or the next roll gets only the remainder of this "+
			"window", got)
	}

	// A fresh roll re-arms with a NEW anchor — the ceiling is not a one-shot latch.
	if _, err := q.UpsertWorkerRollHealth(ctx, rollReport(hosted, "rolling", t0.Add(30*time.Minute))); err != nil {
		t.Fatalf("re-arm upsert: %v", err)
	}
	rearmed := anchorAfter("re-arm")
	if rearmed == nil {
		t.Fatalf("after the clear, a new roll did not stamp a new anchor")
	}
	if rearmed.Equal(*first) {
		t.Errorf("the re-armed anchor equals the original (%v); a second roll must get a full fresh "+
			"window, not the remainder of the first", first)
	}
}

// Cross-tenancy. The roll-health table has no user_id, so the ONLY way to read a row is
// through `workers` — which is what makes per-user scoping unavoidable rather than
// remembered. Two users with distinct coordinates, because a single-user fixture passes
// against a query with no WHERE at all.
func TestWorkerUpgradeSummaryIsPerUserLiveDB(t *testing.T) {
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
	defer pool.Close()
	q := store.New(pool)

	mkUser := func(tag string) uuid.UUID {
		id := uuid.New()
		mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			id, fmt.Sprintf("%s-%s@e2e", tag, id))
		return id
	}
	mkWorker := func(user uuid.UUID, version string) uuid.UUID {
		id := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO workers (id, user_id, name, token_hash, status, kind, version, hosted_size, template_declared)
			 VALUES ($1, $2, $3, $4, 'online', 'hosted', $5, 'm', 'base')`,
			id, user, "w-"+id.String()[:8], []byte(id.String()), version)
		return id
	}

	alice, bob := mkUser("alice"), mkUser("bob")
	aliceWorker := mkWorker(alice, "0.11.7")
	bobWorker := mkWorker(bob, "0.11.0")

	now := time.Now().UTC()
	// Bob's worker is the STUCK one. If the query leaked, Alice would see a failing
	// worker she does not own — a nav badge counting someone else's problem.
	if _, err := q.UpsertWorkerRollHealth(ctx, rollReport(bobWorker, "stuck", now)); err != nil {
		t.Fatalf("upsert bob: %v", err)
	}
	if _, err := q.UpsertWorkerRollHealth(ctx, rollReport(aliceWorker, "settled", now)); err != nil {
		t.Fatalf("upsert alice: %v", err)
	}

	rows, err := q.GetWorkerUpgradeSummaryForUser(ctx, alice)
	if err != nil {
		t.Fatalf("summary for alice: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("alice sees %d workers, want exactly her own 1", len(rows))
	}
	if rows[0].WorkerID != aliceWorker {
		t.Errorf("alice's summary returned worker %s, want %s", rows[0].WorkerID, aliceWorker)
	}
	if rows[0].Phase.Valid && rows[0].Phase.String == "stuck" {
		t.Errorf("alice's summary carries the STUCK phase that belongs to bob's worker — the join " +
			"through workers is not scoping by owner")
	}

	// And the mute is per-user too: Alice muting her worker must not mute Bob's.
	if err := q.MuteWorkerUpgrade(ctx, store.MuteWorkerUpgradeParams{
		UserID: alice, WorkerID: aliceWorker, Release: "0.11.7",
	}); err != nil {
		t.Fatalf("mute alice: %v", err)
	}
	// Alice attempting to mute BOB's worker writes nothing: the (worker, user) match in
	// the INSERT ... SELECT is the authorization, exactly as notifications does it.
	if err := q.MuteWorkerUpgrade(ctx, store.MuteWorkerUpgradeParams{
		UserID: alice, WorkerID: bobWorker, Release: "0.11.7",
	}); err != nil {
		t.Fatalf("cross-user mute returned an error; it should simply write nothing: %v", err)
	}
	var mutes int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM worker_upgrade_mutes WHERE worker_id = $1`, bobWorker).
		Scan(&mutes); err != nil {
		t.Fatalf("count bob mutes: %v", err)
	}
	if mutes != 0 {
		t.Errorf("alice muted bob's worker (%d rows); the (user_id, worker_id) match IS the "+
			"authorization and it did not hold", mutes)
	}
}
