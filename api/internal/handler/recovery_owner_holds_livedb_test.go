package handler

// PRD #1349 M5 live-DB proofs for the OWNER custody-hold API: the owner-wide list + aggregate
// (GET /api/recovery/holds) and the exact hold DISCARD
// (DELETE /api/runs/{id}/recovery-holds/{holdID}?confirm=discard). These drive the REAL
// h.Routes() router so the auth MOUNT is load-bearing (RequireUser: cookie OR uzc_/uza_ Bearer,
// mirroring TestDeleteRepoBearerMountAndNoCredLiveDB), plus the pre-SQL ?confirm=discard gate,
// SQL-level owner scope, read-only-admin refusal, idempotency, sibling safety, and the
// attention/DecisionNeeded derivation against a real Postgres.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

// custodySeedWorker inserts an online worker owned by userID and returns its id.
func custodySeedWorker(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	cliMustExec(t, pool, `INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'online')`,
		id, userID, "cw-"+id.String(), id[:])
	return id
}

// custodySeedRunHold seeds a repo + run (at runStatus) + one OPEN custody hold at generation gen
// held live by worker, and returns the run and hold ids. Each hold gets its own repo (distinct
// projectID) so multiple ACTIVE issue runs never collide on uq_runs_one_active_per_issue.
func custodySeedRunHold(t *testing.T, pool *pgxpool.Pool, owner, conn, worker uuid.UUID, projectID int64, runStatus string, gen int64) (uuid.UUID, uuid.UUID) {
	t.Helper()
	repo := rmSeedRepo(t, pool, conn, projectID, true)
	run := rmSeedRun(t, pool, owner, repo, runStatus)
	hold := uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO recovery_custody_holds
		   (id, user_id, repo_id, run_id, generation, state, original_worker_id, original_worker_identity, live_worker_id, live_run_id)
		 VALUES ($1, $2, $3, $4, $5, 'open', $6, $7, $6, $4)`,
		hold, owner, repo, run, gen, worker, "cw-ident")
	return run, hold
}

// custodySeedCapture inserts a capture in the given state under hold, with n byte chunks. An
// 'available' capture is manifest-bound with a byte size/checksum (so the sibling-safety test
// can assert it SURVIVES a hold discard); any other state is left unbound.
func custodySeedCapture(t *testing.T, pool *pgxpool.Pool, owner, run, worker, hold uuid.UUID, key, state string, chunks int) uuid.UUID {
	t.Helper()
	capID := uuid.New()
	if state == "available" {
		cliMustExec(t, pool,
			`INSERT INTO recovery_captures
			   (id, hold_id, run_id, user_id, original_worker_id, original_worker_identity, source_sha, idempotency_key, state, manifest_bound, byte_size, checksum)
			 VALUES ($1, $2, $3, $4, $5, 'cw-ident', 'H0', $6, 'available', true, $7, 'deadbeef')`,
			capID, hold, run, owner, worker, key, int64(3*chunks))
	} else {
		cliMustExec(t, pool,
			`INSERT INTO recovery_captures
			   (id, hold_id, run_id, user_id, original_worker_id, original_worker_identity, source_sha, idempotency_key, state)
			 VALUES ($1, $2, $3, $4, $5, 'cw-ident', 'H0', $6, $7)`,
			capID, hold, run, owner, worker, key, state)
	}
	for i := 0; i < chunks; i++ {
		cliMustExec(t, pool, `INSERT INTO recovery_capture_chunks (capture_id, chunk_index, length, sealed) VALUES ($1, $2, 3, $3)`,
			capID, i, []byte("abc"))
	}
	return capID
}

func custodyHoldStateByID(t *testing.T, pool *pgxpool.Pool, holdID uuid.UUID) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM recovery_custody_holds WHERE id=$1`, holdID).Scan(&s); err != nil {
		t.Fatalf("read hold state: %v", err)
	}
	return s
}

func custodyCaptureStateByID(t *testing.T, pool *pgxpool.Pool, capID uuid.UUID) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM recovery_captures WHERE id=$1`, capID).Scan(&s); err != nil {
		t.Fatalf("read capture state: %v", err)
	}
	return s
}

func custodyChunkCountByID(t *testing.T, pool *pgxpool.Pool, capID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM recovery_capture_chunks WHERE capture_id=$1`, capID).Scan(&n); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	return n
}

// TestRecoveryOwnerHoldsMountAndDiscardBearerCookieLiveDB proves both new routes sit in the
// RequireUser group: a uzc_ Bearer AND a session cookie reach them, and a no-credential request
// is 401 (a cookie-only RequireAuth mount would 401 the Bearer, issue #428's shape).
func TestRecoveryOwnerHoldsMountAndDiscardBearerCookieLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	jwt := cliMintJWT(t, pool, owner)
	conn := rmSeedConn(t, pool, owner)
	worker := custodySeedWorker(t, pool, owner)
	run, hold := custodySeedRunHold(t, pool, owner, conn, worker, 4200, "failed", 1)

	// GET via Bearer → 200 with the owner's hold.
	rec := bearerReq(router, http.MethodGet, "/api/recovery/holds", uzc)
	if rec.Code != http.StatusOK {
		t.Fatalf("Bearer GET holds = %d, want 200 (proves RequireUser Bearer mount)\nbody: %s", rec.Code, rec.Body.String())
	}
	var got apitypes.RecoveryCustodyHoldsDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode holds: %v", err)
	}
	if len(got.Holds) != 1 || got.Holds[0].ID != hold.String() {
		t.Fatalf("holds = %+v, want the single seeded hold %s", got.Holds, hold)
	}

	// GET via session cookie → 200 (the other RequireUser credential path).
	if rec := cookieReq(t, router, http.MethodGet, "/api/recovery/holds", jwt, ""); rec.Code != http.StatusOK {
		t.Fatalf("cookie GET holds = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}

	// No credential → 401 (the mount requires auth).
	if rec := bearerReq(router, http.MethodGet, "/api/recovery/holds", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-cred GET holds = %d, want 401", rec.Code)
	}

	// DELETE via Bearer with the confirm gate → 200, hold discarded (proves the /runs Bearer mount).
	del := fmt.Sprintf("/api/runs/%s/recovery-holds/%s?confirm=discard", run, hold)
	if rec := bearerReq(router, http.MethodDelete, del, uzc); rec.Code != http.StatusOK {
		t.Fatalf("Bearer DELETE hold = %d, want 200 (a cookie-only mount would 401 this)\nbody: %s", rec.Code, rec.Body.String())
	}
	if st := custodyHoldStateByID(t, pool, hold); st != "discarded" {
		t.Fatalf("hold state after discard = %q, want discarded", st)
	}
}

// TestRecoveryOwnerHoldDiscardConfirmGateLiveDB proves ?confirm=discard is enforced BEFORE any
// SQL: a missing or wrong value is a 400 that mutates nothing, and only the exact value discards.
// A second discard is an idempotent no-op (404).
func TestRecoveryOwnerHoldDiscardConfirmGateLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	conn := rmSeedConn(t, pool, owner)
	worker := custodySeedWorker(t, pool, owner)
	run, hold := custodySeedRunHold(t, pool, owner, conn, worker, 4201, "failed", 1)
	base := fmt.Sprintf("/api/runs/%s/recovery-holds/%s", run, hold)

	// Missing confirm → 400, hold still open (no mutation).
	if rec := bearerReq(router, http.MethodDelete, base, uzc); rec.Code != http.StatusBadRequest {
		t.Fatalf("DELETE without confirm = %d, want 400\nbody: %s", rec.Code, rec.Body.String())
	}
	if st := custodyHoldStateByID(t, pool, hold); st != "open" {
		t.Fatalf("hold state after missing-confirm = %q, want open (no mutation)", st)
	}
	// Wrong confirm → 400, still open.
	if rec := bearerReq(router, http.MethodDelete, base+"?confirm=delete", uzc); rec.Code != http.StatusBadRequest {
		t.Fatalf("DELETE with wrong confirm = %d, want 400", rec.Code)
	}
	if st := custodyHoldStateByID(t, pool, hold); st != "open" {
		t.Fatalf("hold state after wrong-confirm = %q, want open (no mutation)", st)
	}
	// Correct confirm → 200, discarded.
	if rec := bearerReq(router, http.MethodDelete, base+"?confirm=discard", uzc); rec.Code != http.StatusOK {
		t.Fatalf("DELETE confirm=discard = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	if st := custodyHoldStateByID(t, pool, hold); st != "discarded" {
		t.Fatalf("hold state = %q, want discarded", st)
	}
	// Second discard → 404 (terminal + idempotent), still discarded.
	if rec := bearerReq(router, http.MethodDelete, base+"?confirm=discard", uzc); rec.Code != http.StatusNotFound {
		t.Fatalf("second DELETE = %d, want 404 (idempotent no-op)", rec.Code)
	}
	if st := custodyHoldStateByID(t, pool, hold); st != "discarded" {
		t.Fatalf("hold state after second discard = %q, want still discarded", st)
	}
}

// TestRecoveryOwnerHoldDiscardOwnerScopeLiveDB proves the discard is owner-scoped in SQL: a
// foreign owner and a read-only admin (uza_, IsAdmin=true) both get 404 (GetRun ignores admin)
// and mutate nothing; the rightful owner discards.
func TestRecoveryOwnerHoldDiscardOwnerScopeLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	foreign := cliSeedUser(t, pool, false)
	admin := cliSeedUser(t, pool, true)
	ownerTok := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	foreignTok := cliMintToken(t, pool, foreign, clitoken.ScopeUser)
	adminTok := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)
	conn := rmSeedConn(t, pool, owner)
	worker := custodySeedWorker(t, pool, owner)
	run, hold := custodySeedRunHold(t, pool, owner, conn, worker, 4202, "failed", 1)
	del := fmt.Sprintf("/api/runs/%s/recovery-holds/%s?confirm=discard", run, hold)

	// Foreign owner → 404, no mutation.
	if rec := bearerReq(router, http.MethodDelete, del, foreignTok); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign-owner DELETE = %d, want 404", rec.Code)
	}
	if st := custodyHoldStateByID(t, pool, hold); st != "open" {
		t.Fatalf("hold state after foreign attempt = %q, want open (no mutation)", st)
	}
	// Read-only admin on another owner's run → 404 (GetRun is owner-only), no mutation.
	if rec := bearerReq(router, http.MethodDelete, del, adminTok); rec.Code != http.StatusNotFound {
		t.Fatalf("admin_ro DELETE = %d, want 404 (GetRun ignores admin)", rec.Code)
	}
	if st := custodyHoldStateByID(t, pool, hold); st != "open" {
		t.Fatalf("hold state after admin attempt = %q, want open (no mutation)", st)
	}
	// A foreign owner also cannot even SEE the hold in its own list.
	rec := bearerReq(router, http.MethodGet, "/api/recovery/holds", foreignTok)
	var foreignList apitypes.RecoveryCustodyHoldsDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &foreignList)
	if len(foreignList.Holds) != 0 {
		t.Fatalf("foreign owner list = %+v, want no holds", foreignList.Holds)
	}
	// The rightful owner discards.
	if rec := bearerReq(router, http.MethodDelete, del, ownerTok); rec.Code != http.StatusOK {
		t.Fatalf("owner DELETE = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
}

// TestRecoveryOwnerHoldDiscardSiblingSafetyLiveDB proves discarding one hold: settles that hold's
// NON-ready captures (bytes freed) but PRESERVES its available archive, and leaves a sibling
// generation's hold + capture completely untouched.
func TestRecoveryOwnerHoldDiscardSiblingSafetyLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	conn := rmSeedConn(t, pool, owner)
	worker := custodySeedWorker(t, pool, owner)

	// holdA (gen 1): an available archive + a preparing capture.
	runA, holdA := custodySeedRunHold(t, pool, owner, conn, worker, 4203, "failed", 1)
	capAvail := custodySeedCapture(t, pool, owner, runA, worker, holdA, "kA-avail", "available", 2)
	capPrep := custodySeedCapture(t, pool, owner, runA, worker, holdA, "kA-prep", "preparing", 1)
	// holdB (gen 2, a different run): its own preparing capture — the sibling.
	runB, holdB := custodySeedRunHold(t, pool, owner, conn, worker, 4204, "failed", 1)
	capB := custodySeedCapture(t, pool, owner, runB, worker, holdB, "kB", "preparing", 1)

	del := fmt.Sprintf("/api/runs/%s/recovery-holds/%s?confirm=discard", runA, holdA)
	if rec := bearerReq(router, http.MethodDelete, del, uzc); rec.Code != http.StatusOK {
		t.Fatalf("discard holdA = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}

	// holdA discarded; its available archive SURVIVES with its bytes; its preparing capture settled.
	if st := custodyHoldStateByID(t, pool, holdA); st != "discarded" {
		t.Fatalf("holdA state = %q, want discarded", st)
	}
	if st := custodyCaptureStateByID(t, pool, capAvail); st != "available" {
		t.Fatalf("available capture state = %q, want available (an archive survives a hold discard)", st)
	}
	if n := custodyChunkCountByID(t, pool, capAvail); n != 2 {
		t.Fatalf("available capture chunks = %d, want 2 (bytes preserved)", n)
	}
	if st := custodyCaptureStateByID(t, pool, capPrep); st != "discarded" {
		t.Fatalf("preparing capture state = %q, want discarded", st)
	}
	if n := custodyChunkCountByID(t, pool, capPrep); n != 0 {
		t.Fatalf("preparing capture chunks = %d, want 0 (partial bytes freed)", n)
	}

	// The sibling hold + capture are completely untouched.
	if st := custodyHoldStateByID(t, pool, holdB); st != "open" {
		t.Fatalf("sibling hold state = %q, want open (untouched)", st)
	}
	if st := custodyCaptureStateByID(t, pool, capB); st != "preparing" {
		t.Fatalf("sibling capture state = %q, want preparing (untouched)", st)
	}
	if n := custodyChunkCountByID(t, pool, capB); n != 1 {
		t.Fatalf("sibling capture chunks = %d, want 1 (untouched)", n)
	}
}

// TestRecoveryOwnerHoldsAggregateAndAttentionLiveDB proves the list+aggregate happy path: each
// hold's server-derived Attention and the DecisionNeeded count (needs_action + source_only only),
// plus OpenHolds and the configured CustodyHoldLimit.
func TestRecoveryOwnerHoldsAggregateAndAttentionLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	conn := rmSeedConn(t, pool, owner)
	worker := custodySeedWorker(t, pool, owner)

	// source_only: terminal run, no capture (a decision).
	_, hSource := custodySeedRunHold(t, pool, owner, conn, worker, 4205, "failed", 1)
	// active: live run, no capture (healthy protection, NOT a decision).
	_, hActive := custodySeedRunHold(t, pool, owner, conn, worker, 4206, "running", 1)
	// archive_ready: an available archive (self-releasing, NOT a decision).
	runReady, hReady := custodySeedRunHold(t, pool, owner, conn, worker, 4207, "failed", 1)
	custodySeedCapture(t, pool, owner, runReady, worker, hReady, "kReady", "available", 1)
	// needs_action: a stalled capture (a decision).
	runNA, hNA := custodySeedRunHold(t, pool, owner, conn, worker, 4208, "failed", 1)
	custodySeedCapture(t, pool, owner, runNA, worker, hNA, "kNA", "needs_action", 0)

	rec := bearerReq(router, http.MethodGet, "/api/recovery/holds", uzc)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET holds = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	var got apitypes.RecoveryCustodyHoldsDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantAttention := map[string]string{
		hSource.String(): "source_only",
		hActive.String(): "active",
		hReady.String():  "archive_ready",
		hNA.String():     "needs_action",
	}
	seen := map[string]string{}
	for _, h := range got.Holds {
		seen[h.ID] = h.Attention
	}
	for id, want := range wantAttention {
		if seen[id] != want {
			t.Errorf("hold %s attention = %q, want %q", id, seen[id], want)
		}
	}
	if got.Aggregate.OpenHolds != 4 {
		t.Errorf("open_holds = %d, want 4", got.Aggregate.OpenHolds)
	}
	if got.Aggregate.CustodyHoldLimit != 8 {
		t.Errorf("custody_hold_limit = %d, want 8", got.Aggregate.CustodyHoldLimit)
	}
	// DecisionNeeded counts ONLY source_only + needs_action (not active, not archive_ready).
	if got.Aggregate.DecisionNeeded != 2 {
		t.Errorf("decision_needed = %d, want 2 (source_only + needs_action)", got.Aggregate.DecisionNeeded)
	}
}
