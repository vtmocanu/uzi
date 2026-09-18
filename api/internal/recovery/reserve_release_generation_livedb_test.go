package recovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestRecoveryReserveReleaseGenerationSafeLiveDB is the PRD #1349 M4 (D1/D2) live-DB gate for the
// generation-safe worker Reserve/Release RPCs, against a REAL Postgres — behaviour a fake store
// cannot exhibit (generation-exact hold selection, the FOR UPDATE ambiguity resolution, and that
// an ambiguous reserve binds NOTHING while an ambiguous release RETAINS every hold).
//
// It exercises both capability paths off the SAME worker id, varying only the passed
// store.Worker.ProtocolCapabilities (v2 names its generation; v1 must resolve an unambiguous
// single hold or refuse/retain):
//   - v2 reserve binds to the EXACT generation; a wrong generation is fail-closed.
//   - v1 reserve binds under a SINGLE open hold; MORE than one is ErrAmbiguous and binds nothing.
//   - v2 release settles ONLY the named generation; a sibling generation stays open.
//   - v1 release settles a SINGLE open hold; MORE than one RETAINS (Retained=true, nothing
//     released, holds stay open); zero open holds is an idempotent no-op.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (run via
// ./e2e/run-store-it.sh). A package that prints `ok` with PASS=0 is INVALID, not green.
func TestRecoveryReserveReleaseGenerationSafeLiveDB(t *testing.T) {
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
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("rgen-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/rgen', 'https://forge.e2e/g/rgen', 'main', true)`, repoID, connID)
	workerName := "w-" + workerID.String()
	exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'online')`,
		workerID, userID, workerName, workerID[:])

	var iid int64
	newRun := func() uuid.UUID {
		iid++
		id := uuid.New()
		exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
		      VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'queued')`, id, userID, repoID, iid)
		return id
	}
	// openHold opens an OPEN hold at generation gen, with original_worker_id == live_worker_id ==
	// workerID (the real invariant: a hold is opened with both equal and never reassigned).
	openHold := func(runID uuid.UUID, gen int64) uuid.UUID {
		id := uuid.New()
		exec(`INSERT INTO recovery_custody_holds
		        (id, user_id, repo_id, run_id, generation, state,
		         original_worker_id, original_worker_identity, live_worker_id, live_run_id)
		      VALUES ($1, $2, $3, $4, $5, 'open', $6, 'ident', $6, $4)`,
			id, userID, repoID, runID, gen, workerID)
		return id
	}
	holdState := func(id uuid.UUID) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `SELECT state FROM recovery_custody_holds WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatalf("read hold state: %v", err)
		}
		return s
	}
	captureCount := func(runID uuid.UUID) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM recovery_captures WHERE run_id = $1`, runID).Scan(&n); err != nil {
			t.Fatalf("count captures: %v", err)
		}
		return n
	}

	svc := New(store.New(pool), pool, nil, Limits{}, nil)
	v1 := store.Worker{ID: workerID, UserID: userID, Name: workerName, ProtocolCapabilities: []string{capability.RecoveryArchiveV1}}
	v2 := store.Worker{ID: workerID, UserID: userID, Name: workerName, ProtocolCapabilities: []string{capability.RecoveryArchiveV1, capability.RecoveryArchiveV2}}
	gen := func(g int64) *int64 { return &g }

	// ── (a) v2 reserve binds to the EXACT generation; a wrong generation is fail-closed. ──
	r1 := newRun()
	h1a := openHold(r1, 1)
	h1b := openHold(r1, 2)
	res, err := svc.Reserve(ctx, v2, r1, apitypes.RecoveryReserveRequest{IdempotencyKey: "k1", SourceSha: "aaaa1111", Generation: gen(2)})
	if err != nil {
		t.Fatalf("v2 Reserve(gen 2): %v", err)
	}
	var boundHold uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT hold_id FROM recovery_captures WHERE id = $1`, uuid.MustParse(res.CaptureID)).Scan(&boundHold); err != nil {
		t.Fatalf("read bound hold: %v", err)
	}
	if boundHold != h1b {
		t.Fatalf("v2 reserve bound to hold %s, want the gen-2 hold %s (never the gen-1 hold %s)", boundHold, h1b, h1a)
	}
	// A generation naming no open hold this worker took → fail closed.
	if _, err := svc.Reserve(ctx, v2, r1, apitypes.RecoveryReserveRequest{IdempotencyKey: "k1x", SourceSha: "aaaa1111", Generation: gen(9)}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("v2 Reserve(gen 9, no such hold) = %v, want ErrNotAuthorized (fail closed)", err)
	}

	// ── (b) v1 reserve binds under a SINGLE open hold. ──
	r2 := newRun()
	h2 := openHold(r2, 1)
	res2, err := svc.Reserve(ctx, v1, r2, apitypes.RecoveryReserveRequest{IdempotencyKey: "k2", SourceSha: "bbbb2222"})
	if err != nil {
		t.Fatalf("v1 Reserve(single hold): %v", err)
	}
	var boundHold2 uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT hold_id FROM recovery_captures WHERE id = $1`, uuid.MustParse(res2.CaptureID)).Scan(&boundHold2); err != nil {
		t.Fatalf("read bound hold (v1 single): %v", err)
	}
	if boundHold2 != h2 {
		t.Fatalf("v1 reserve bound to hold %s, want the sole open hold %s", boundHold2, h2)
	}

	// ── (c) v1 reserve with MORE than one open hold is ErrAmbiguous and binds NOTHING. ──
	r3 := newRun()
	openHold(r3, 1)
	openHold(r3, 2)
	if _, err := svc.Reserve(ctx, v1, r3, apitypes.RecoveryReserveRequest{IdempotencyKey: "k3", SourceSha: "cccc3333"}); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("v1 Reserve(2 open holds) = %v, want ErrAmbiguous (refuse, never guess a newest hold)", err)
	}
	if n := captureCount(r3); n != 0 {
		t.Fatalf("ambiguous reserve bound %d captures, want 0 (nothing guessed)", n)
	}

	// ── (d) v2 release settles ONLY the named generation; the sibling generation stays open. ──
	r4 := newRun()
	h4a := openHold(r4, 1)
	h4b := openHold(r4, 2)
	rel, err := svc.Release(ctx, v2, r4, apitypes.RecoveryReleaseRequest{Generation: gen(2)})
	if err != nil {
		t.Fatalf("v2 Release(gen 2): %v", err)
	}
	if !rel.Released || rel.HoldsReleased != 1 || rel.Retained {
		t.Fatalf("v2 Release(gen 2) = %+v, want Released=true HoldsReleased=1 Retained=false", rel)
	}
	if s := holdState(h4b); s != "released" {
		t.Fatalf("gen-2 hold state = %q after v2 release, want released", s)
	}
	if s := holdState(h4a); s != "open" {
		t.Fatalf("gen-1 hold state = %q after v2 release of gen 2, want OPEN (exact release must not touch the sibling)", s)
	}

	// ── (e) v1 release settles a SINGLE open hold. ──
	r5 := newRun()
	h5 := openHold(r5, 1)
	rel5, err := svc.Release(ctx, v1, r5, apitypes.RecoveryReleaseRequest{})
	if err != nil {
		t.Fatalf("v1 Release(single hold): %v", err)
	}
	if !rel5.Released || rel5.HoldsReleased != 1 || rel5.Retained {
		t.Fatalf("v1 Release(single hold) = %+v, want Released=true HoldsReleased=1 Retained=false", rel5)
	}
	if s := holdState(h5); s != "released" {
		t.Fatalf("sole hold state = %q after v1 release, want released", s)
	}
	// Idempotent no-op: a repeat once none remain open settles zero and does NOT retain.
	rel5b, err := svc.Release(ctx, v1, r5, apitypes.RecoveryReleaseRequest{})
	if err != nil {
		t.Fatalf("v1 Release(idempotent repeat): %v", err)
	}
	if rel5b.Released || rel5b.HoldsReleased != 0 || rel5b.Retained {
		t.Fatalf("v1 Release(no open hold) = %+v, want Released=false HoldsReleased=0 Retained=false (idempotent no-op)", rel5b)
	}

	// ── (f) v1 release with MORE than one open hold RETAINS: nothing released, holds stay open. ──
	r6 := newRun()
	h6a := openHold(r6, 1)
	h6b := openHold(r6, 2)
	rel6, err := svc.Release(ctx, v1, r6, apitypes.RecoveryReleaseRequest{})
	if err != nil {
		t.Fatalf("v1 Release(2 open holds): %v", err)
	}
	if rel6.Released || rel6.HoldsReleased != 0 || !rel6.Retained || rel6.Reason == "" {
		t.Fatalf("v1 Release(2 open holds) = %+v, want Released=false HoldsReleased=0 Retained=true with a reason", rel6)
	}
	if s := holdState(h6a); s != "open" {
		t.Fatalf("gen-1 hold state = %q after ambiguous v1 release, want OPEN (retain, never guess)", s)
	}
	if s := holdState(h6b); s != "open" {
		t.Fatalf("gen-2 hold state = %q after ambiguous v1 release, want OPEN (retain, never guess)", s)
	}
}
