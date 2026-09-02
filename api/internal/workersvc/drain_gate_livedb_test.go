package workersvc

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestDrainGateLiveDB is the Decision-7 invariant test against a REAL Postgres: the
// sharpest risk in PRD #422 M3 is that a draining worker's cordon is CLOBBERED by its
// own heartbeat (which is why draining lives in its own column, not workers.status).
// Only a live DB running the actual RegisterWorker/HeartbeatWorker statements can prove
// the column survives a heartbeat and is cleared on the post-roll re-register, so this
// exercises the full lifecycle:
//
//	register → cordon (UPDATE draining_since = now()) → heartbeat → claim → re-register → claim
//
// and asserts:
//   - after HeartbeatWorker the worker is STILL draining (draining_since not cleared) AND
//     status='online' (a heartbeat keeps it live without un-cordoning it);
//   - svc.Claim reports idle while the worker is draining, and the queued run STAYS queued;
//   - a subsequent RegisterWorker (the post-roll re-register) CLEARS draining_since, and
//     svc.Claim then proceeds — the run leaves 'queued' (it is claimed; here it then fails
//     on the missing Anthropic token, which is idle-with-a-failed-run, the well-pinned
//     credential path — either way it is no longer queued, proving the gate reopened).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (named OUTSIDE the
// uzi- namespace, per the store live-DB harness). A package that prints `ok` with PASS=0
// is INVALID, not green.
func TestDrainGateLiveDB(t *testing.T) {
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
	defer pool.Close()

	box := newBox(t)
	q := store.New(pool)
	svc := New(q, box, testParams())

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	// A real box-sealed bot PAT so the claim gets past PAT decryption to the (missing)
	// Anthropic token — the controlled fail-the-run path pinned by
	// TestClaimFailsRunWhenAnthropicTokenMissing.
	sealedPAT, err := box.Seal([]byte("bot-pat-DRAINGATE-abcdef1234567890"))
	if err != nil {
		t.Fatalf("seal PAT: %v", err)
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("drain-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, sealedPAT)
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/drain', 'https://forge.e2e/g/drain', 'main', true)`, repoID, connID)

	// A worker under the user; RegisterWorker is an UPDATE, so the row must exist first.
	// token_hash is unique per run (workers_token_hash_key) — derived from a fresh uuid so
	// re-runs against a persistent throwaway DB never collide.
	workerID := uuid.New()
	tokenHash := workerID[:]
	exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, 'w-drain', $3, 'offline')`,
		workerID, userID, tokenHash)

	// register: brings the worker online; draining_since must be NULL.
	if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{ID: workerID}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	reg, err := q.GetWorkerByID(ctx, workerID)
	if err != nil {
		t.Fatalf("GetWorkerByID after register: %v", err)
	}
	if reg.DrainingSince.Valid {
		t.Fatal("a freshly registered worker must not be draining")
	}

	// A queued run for the user to claim.
	runID := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id)
	      VALUES ($1, $2, $3, 1, 't', 'd', 'queued', NULL)`, runID, userID, repoID)

	runStatus := func() string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, runID).Scan(&s); err != nil {
			t.Fatalf("read run status: %v", err)
		}
		return s
	}

	// cordon: the controller's future cordon-write (M4), simulated here as the UPDATE it
	// will perform.
	exec(`UPDATE workers SET draining_since = now() WHERE id = $1`, workerID)

	// heartbeat: a draining worker MUST keep heartbeating. The Decision-7 assertion —
	// the heartbeat must NOT clobber the cordon back to online-and-claimable.
	if _, err := q.HeartbeatWorker(ctx, store.HeartbeatWorkerParams{ID: workerID}); err != nil {
		t.Fatalf("HeartbeatWorker: %v", err)
	}
	beat, err := q.GetWorkerByID(ctx, workerID)
	if err != nil {
		t.Fatalf("GetWorkerByID after heartbeat: %v", err)
	}
	if !beat.DrainingSince.Valid {
		t.Fatal("HeartbeatWorker CLOBBERED the cordon: draining_since must survive a heartbeat (Decision 7)")
	}
	if beat.Status != "online" {
		t.Fatalf("a draining worker must stay online after a heartbeat; status = %q", beat.Status)
	}

	// claim while draining: the gate reports idle and the run stays queued.
	payload, err := svc.Claim(ctx, beat)
	if err != nil {
		t.Fatalf("Claim while draining: %v", err)
	}
	if payload != nil {
		t.Fatal("a draining worker must claim nothing (nil payload)")
	}
	if s := runStatus(); s != "queued" {
		t.Fatalf("the run must stay queued while the worker is draining; status = %q", s)
	}

	// re-register (the post-roll re-register): clears draining_since, re-enabling claims.
	if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{ID: workerID}); err != nil {
		t.Fatalf("RegisterWorker (post-roll): %v", err)
	}
	rolled, err := q.GetWorkerByID(ctx, workerID)
	if err != nil {
		t.Fatalf("GetWorkerByID after re-register: %v", err)
	}
	if rolled.DrainingSince.Valid {
		t.Fatal("RegisterWorker must CLEAR draining_since (clear-on-roll), else the worker is cordoned forever")
	}

	// claim after the roll: the gate is reopened, so ClaimRun fires. The run leaves
	// 'queued' (claimed, then failed on the absent Anthropic token) — either terminal is
	// proof the gate no longer blocks.
	if _, err := svc.Claim(ctx, rolled); err != nil {
		t.Fatalf("Claim after roll: %v", err)
	}
	if s := runStatus(); s == "queued" {
		t.Fatal("after the roll the drain gate must reopen: the run must no longer be queued")
	}
}
