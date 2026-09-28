package recovery

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestCustodySettledHookAfterCommitLiveDB pins PRD #1810 D3's recovery half against a REAL
// Postgres: every release and discard path of the Service calls the custody-settlement hook with
// the run id AFTER its transaction committed (the hook, reading on a different connection, already
// sees the settled hold) and only when the statement moved a row. The end-to-end delete of the
// retained checkpoint ref is proven in workersvc (checkpoint_settlement_livedb_test.go).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).
func TestCustodySettledHookAfterCommitLiveDB(t *testing.T) {
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

	userID, connID, repoID, workerID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("hook-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/hook', 'https://forge.e2e/g/hook', 'main', true)`, repoID, connID)
	exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'online')`,
		workerID, userID, "w-"+workerID.String(), workerID[:])

	var iid int64
	newRun := func() uuid.UUID {
		iid++
		id := uuid.New()
		exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
		      VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'failed')`, id, userID, repoID, iid)
		return id
	}
	openHold := func(runID uuid.UUID, gen int64) uuid.UUID {
		id := uuid.New()
		exec(`INSERT INTO recovery_custody_holds
		        (id, user_id, repo_id, run_id, generation, state,
		         original_worker_id, original_worker_identity, live_worker_id, live_run_id)
		      VALUES ($1, $2, $3, $4, $5, 'open', $6, 'ident', $6, $4)`,
			id, userID, repoID, runID, gen, workerID)
		return id
	}
	openHolds := func(runID uuid.UUID) int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM recovery_custody_holds WHERE run_id = $1 AND state = 'open'`, runID).Scan(&n); err != nil {
			t.Fatalf("count open holds: %v", err)
		}
		return n
	}

	// The hook records each call and, on its own pool connection, how many holds of the run were
	// still open at that moment: a call made before the commit would still see the hold open.
	type call struct {
		run  uuid.UUID
		open int
	}
	var calls []call
	svc := New(store.New(pool), pool, nil, Limits{}, nil)
	svc.SetCustodySettledHook(func(runID uuid.UUID) { calls = append(calls, call{runID, openHolds(runID)}) })
	seen := 0
	expect := func(step string, runID uuid.UUID, want int) {
		t.Helper()
		if len(calls) != want {
			t.Fatalf("%s: hook calls = %d, want %d", step, len(calls), want)
		}
		grew := want > seen
		seen = want
		if grew {
			if c := calls[len(calls)-1]; c.run != runID || c.open != 0 {
				t.Fatalf("%s: hook call = {run %s open %d}, want {run %s open 0} (after the commit)", step, c.run, c.open, runID)
			}
		}
	}
	v1 := store.Worker{ID: workerID, UserID: userID, ProtocolCapabilities: []string{capability.RecoveryArchiveV1}}
	v2 := store.Worker{ID: workerID, UserID: userID, ProtocolCapabilities: []string{capability.RecoveryArchiveV1, capability.RecoveryArchiveV2}}
	gen := func(g int64) *int64 { return &g }

	// v2 exact-generation release, then its idempotent repeat (0 rows: no call).
	r1 := newRun()
	openHold(r1, 1)
	if res, err := svc.Release(ctx, v2, r1, apitypes.RecoveryReleaseRequest{Generation: gen(1)}); err != nil || !res.Released {
		t.Fatalf("v2 Release = %+v, %v", res, err)
	}
	expect("v2 release", r1, 1)
	if _, err := svc.Release(ctx, v2, r1, apitypes.RecoveryReleaseRequest{Generation: gen(1)}); err != nil {
		t.Fatalf("v2 Release repeat: %v", err)
	}
	expect("v2 release repeat", r1, 1)

	// v1 sole-hold release (the transactional path).
	r2 := newRun()
	openHold(r2, 1)
	if res, err := svc.Release(ctx, v1, r2, apitypes.RecoveryReleaseRequest{}); err != nil || !res.Released {
		t.Fatalf("v1 Release = %+v, %v", res, err)
	}
	expect("v1 release", r2, 2)

	// v1 with two open holds retains: no release, no call.
	r3 := newRun()
	openHold(r3, 1)
	h3 := openHold(r3, 2)
	if res, err := svc.Release(ctx, v1, r3, apitypes.RecoveryReleaseRequest{}); err != nil || !res.Retained {
		t.Fatalf("v1 ambiguous Release = %+v, %v; want retained", res, err)
	}
	expect("v1 ambiguous release", r3, 2)

	// Owner discard, then its no-op repeat.
	r4 := newRun()
	h4 := openHold(r4, 1)
	if ok, err := svc.DiscardHold(ctx, userID, r4, h4); err != nil || !ok {
		t.Fatalf("DiscardHold = %v, %v", ok, err)
	}
	expect("discard", r4, 3)
	if ok, err := svc.DiscardHold(ctx, userID, r4, h4); err != nil || ok {
		t.Fatalf("DiscardHold repeat = %v, %v; want a no-op", ok, err)
	}
	expect("discard repeat", r4, 3)

	// A discard that leaves a sibling hold open still fires (the settle itself keeps the ref).
	if ok, err := svc.DiscardHold(ctx, userID, r3, h3); err != nil || !ok {
		t.Fatalf("DiscardHold(sibling) = %v, %v", ok, err)
	}
	if len(calls) != 4 || calls[3].run != r3 || calls[3].open != 1 {
		t.Fatalf("sibling discard hook calls = %+v, want a 4th call for %s seeing 1 hold still open", calls, r3)
	}

	// No hook wired: every path is a no-op for it.
	bare := New(store.New(pool), pool, nil, Limits{}, nil)
	r5 := newRun()
	h5 := openHold(r5, 1)
	if ok, err := bare.DiscardHold(ctx, userID, r5, h5); err != nil || !ok {
		t.Fatalf("DiscardHold without a hook = %v, %v", ok, err)
	}
}
