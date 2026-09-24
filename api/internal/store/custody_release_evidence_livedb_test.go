package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestCustodyReleaseEvidenceLiveDB is the PRD #1392 M1 (D3) STORAGE proof: the release/discard
// store methods stamp the correct release_evidence class into recovery_custody_holds, and
// ListReleasableCustodyHolds returns the correct per-hold `reason` (publication vs archive)
// against a REAL Postgres — the CHECK-constrained column (migration 00232) and the reason CASE
// that a fake store cannot exhibit. It statically references the release/discard query methods so
// their evidence-stamping behaviour is proven, not just their reachability.
//
// It lives in the store package DELIBERATELY: e2e/run-store-it.sh and CI's test-api-store-it job
// run `-run 'LiveDB$'` over ./internal/store/... for this seam.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres. A package that prints `ok`
// with PASS=0 is INVALID, not green.
func TestCustodyReleaseEvidenceLiveDB(t *testing.T) {
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

	userID, connID, repoID, workerID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("crev-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/crev', 'https://forge.e2e/g/crev', 'main', true)`, repoID, connID)
	workerName := "w-" + workerID.String()
	exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'online')`,
		workerID, userID, workerName, workerID[:])

	iid := int64(0)
	// newRun inserts a run at the given status and (when > 0) claim_generation, returning its id.
	newRun := func(status string, claimGen int64) uuid.UUID {
		iid++
		id := uuid.New()
		exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
		      VALUES ($1, $2, $3, 'issue', $4, 't', 'd', $5)`, id, userID, repoID, iid, status)
		if claimGen > 0 {
			exec(`UPDATE runs SET claim_generation = $2 WHERE id = $1`, id, claimGen)
		}
		return id
	}
	// openHold inserts an OPEN hold at the given generation, held live by the worker (both live
	// FKs set — ReleaseCustodyHoldExact matches on live_worker_id).
	openHold := func(runID uuid.UUID, gen int64) uuid.UUID {
		id := uuid.New()
		exec(`INSERT INTO recovery_custody_holds
		        (id, user_id, repo_id, run_id, generation, state,
		         original_worker_id, original_worker_identity, live_worker_id, live_run_id)
		      VALUES ($1, $2, $3, $4, $5, 'open', $6, $7, $6, $4)`,
			id, userID, repoID, runID, gen, workerID, workerName)
		return id
	}
	holdEvidence := func(id uuid.UUID) (state string, evidence *string) {
		t.Helper()
		var ev *string
		if err := pool.QueryRow(ctx, `SELECT state, release_evidence FROM recovery_custody_holds WHERE id = $1`, id).Scan(&state, &ev); err != nil {
			t.Fatalf("read hold: %v", err)
		}
		return state, ev
	}

	// ── ReleaseCustodyHoldExact (the generation-exact worker/terminal release) stamps the
	// caller-supplied class and flips the hold to 'released'. ──
	// (a) 'forge_no_output' — a fresh-forge no-output proof.
	rNo := newRun("running", 0)
	hNo := openHold(rNo, 1)
	if n, err := q.ReleaseCustodyHoldExact(ctx, store.ReleaseCustodyHoldExactParams{
		RunID: rNo, Generation: 1, WorkerID: workerID, ReleaseEvidence: pgconv.TextOrNull("forge_no_output"),
	}); err != nil || n != 1 {
		t.Fatalf("ReleaseCustodyHoldExact(forge_no_output) = (%d, %v), want 1 row", n, err)
	}
	if state, ev := holdEvidence(hNo); state != "released" || ev == nil || *ev != "forge_no_output" {
		t.Fatalf("exact-release hold state=%q evidence=%v, want released/forge_no_output", state, ev)
	}
	// (b) 'publication' — the completed-run terminal release / a worker publication report.
	rPub := newRun("running", 0)
	hPub := openHold(rPub, 1)
	if n, err := q.ReleaseCustodyHoldExact(ctx, store.ReleaseCustodyHoldExactParams{
		RunID: rPub, Generation: 1, WorkerID: workerID, ReleaseEvidence: pgconv.TextOrNull("publication"),
	}); err != nil || n != 1 {
		t.Fatalf("ReleaseCustodyHoldExact(publication) = (%d, %v), want 1 row", n, err)
	}
	if state, ev := holdEvidence(hPub); state != "released" || ev == nil || *ev != "publication" {
		t.Fatalf("exact-release hold state=%q evidence=%v, want released/publication", state, ev)
	}

	// ── ReleaseCustodyHold (the reconciler's per-hold release) stamps the class it is given
	// ('archive' for a durable capture, 'publication' for a completed-run backstop). ──
	rArch := newRun("running", 0)
	hArch := openHold(rArch, 1)
	if n, err := q.ReleaseCustodyHold(ctx, store.ReleaseCustodyHoldParams{ID: hArch, ReleaseEvidence: pgconv.TextOrNull("archive")}); err != nil || n != 1 {
		t.Fatalf("ReleaseCustodyHold(archive) = (%d, %v), want 1 row", n, err)
	}
	if state, ev := holdEvidence(hArch); state != "released" || ev == nil || *ev != "archive" {
		t.Fatalf("reconciler-release hold state=%q evidence=%v, want released/archive", state, ev)
	}
	rPubRec := newRun("running", 0)
	hPubRec := openHold(rPubRec, 1)
	if n, err := q.ReleaseCustodyHold(ctx, store.ReleaseCustodyHoldParams{ID: hPubRec, ReleaseEvidence: pgconv.TextOrNull("publication")}); err != nil || n != 1 {
		t.Fatalf("ReleaseCustodyHold(publication) = (%d, %v), want 1 row", n, err)
	}
	if state, ev := holdEvidence(hPubRec); state != "released" || ev == nil || *ev != "publication" {
		t.Fatalf("reconciler-release hold state=%q evidence=%v, want released/publication", state, ev)
	}

	// ── DiscardCustodyHoldForOwner stamps 'owner_discard' and settles state 'discarded'. ──
	rDisc := newRun("running", 0)
	hDisc := openHold(rDisc, 1)
	if n, err := q.DiscardCustodyHoldForOwner(ctx, store.DiscardCustodyHoldForOwnerParams{
		HoldID: hDisc, RunID: rDisc, UserID: userID, ReleaseEvidence: pgconv.TextOrNull("owner_discard"),
	}); err != nil || n != 1 {
		t.Fatalf("DiscardCustodyHoldForOwner = (%d, %v), want 1 row", n, err)
	}
	if state, ev := holdEvidence(hDisc); state != "discarded" || ev == nil || *ev != "owner_discard" {
		t.Fatalf("owner-discard hold state=%q evidence=%v, want discarded/owner_discard", state, ev)
	}

	// ── Issue #1582 M1: ReleasePredecessorCustodyHoldByAncestry stamps 'ancestry' with all six
	// audit columns, and migration 00251's CHECKs refuse an 'ancestry' row missing any of them
	// (and a malformed SHA or over-long branch in an audit column). ──
	const (
		shaP = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		shaS = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		shaA = "cccccccccccccccccccccccccccccccccccccccc"
		shaH = "dddddddddddddddddddddddddddddddddddddddd"
	)
	rAnc := newRun("completed", 2)
	exec(`UPDATE runs SET worker_id = $2, branch = 'agent/issue-anc', status_since = now() WHERE id = $1`, rAnc, workerID)
	hAnc := openHold(rAnc, 1)
	hAncSibling := openHold(rAnc, 2)
	var since pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `SELECT status_since FROM runs WHERE id = $1`, rAnc).Scan(&since); err != nil {
		t.Fatalf("read status_since: %v", err)
	}
	ancParams := store.ReleasePredecessorCustodyHoldByAncestryParams{
		PushedSha: shaP, SourceSha: shaS, AdoptedSha: shaA, FinalHeadSha: shaH,
		SuccessorGeneration: 2, Branch: "agent/issue-anc", HoldID: hAnc, RunID: rAnc,
		PredecessorGeneration: 1, WorkerID: workerID, CompletedSince: since,
	}
	// A changed completion instant moves zero rows (the guard the service relies on).
	stale := ancParams
	stale.CompletedSince = pgtype.Timestamptz{Time: since.Time.Add(-time.Second), Valid: true}
	if n, err := q.ReleasePredecessorCustodyHoldByAncestry(ctx, stale); err != nil || n != 0 {
		t.Fatalf("ancestry release with a stale status_since = (%d, %v), want 0 rows", n, err)
	}
	if n, err := q.ReleasePredecessorCustodyHoldByAncestry(ctx, ancParams); err != nil || n != 1 {
		t.Fatalf("ReleasePredecessorCustodyHoldByAncestry = (%d, %v), want 1 row", n, err)
	}
	got, err := q.GetCustodyHoldForSettle(ctx, store.GetCustodyHoldForSettleParams{HoldID: hAnc, RunID: rAnc})
	if err != nil {
		t.Fatalf("GetCustodyHoldForSettle: %v", err)
	}
	if got.State != "released" || got.ReleaseEvidence.String != "ancestry" || got.ReleasePushedSha.String != shaP ||
		got.ReleaseSourceSha.String != shaS || got.ReleaseAdoptedSha.String != shaA || got.ReleaseFinalHeadSha.String != shaH ||
		got.ReleaseSuccessorGeneration.Int64 != 2 || got.ReleaseBranch.String != "agent/issue-anc" ||
		got.LiveWorkerID.Valid || got.LiveRunID.Valid || !got.ReleasedAt.Valid {
		t.Fatalf("ancestry-released hold = %+v, want released/ancestry with all six audit columns and null live FKs", got)
	}
	if state, _ := holdEvidence(hAncSibling); state != "open" {
		t.Fatalf("sibling generation hold state = %q, want open (never touched)", state)
	}
	// Idempotent: a second identical call moves zero rows.
	if n, err := q.ReleasePredecessorCustodyHoldByAncestry(ctx, ancParams); err != nil || n != 0 {
		t.Fatalf("repeat ancestry release = (%d, %v), want 0 rows", n, err)
	}
	// The all-or-nothing CHECK: an 'ancestry' row missing any audit column is refused.
	hPartial := openHold(newRun("running", 0), 1)
	for _, bad := range []string{
		`UPDATE recovery_custody_holds SET state = 'released', release_evidence = 'ancestry' WHERE id = $1`,
		`UPDATE recovery_custody_holds SET state = 'released', release_evidence = 'ancestry',
		   release_pushed_sha = '` + shaP + `', release_source_sha = '` + shaS + `', release_adopted_sha = '` + shaA + `',
		   release_final_head_sha = '` + shaH + `', release_successor_generation = 2 WHERE id = $1`,
		`UPDATE recovery_custody_holds SET release_pushed_sha = 'ABC' WHERE id = $1`,
		`UPDATE recovery_custody_holds SET release_final_head_sha = '` + strings.ToUpper("abcdef"+shaH[6:]) + `' WHERE id = $1`,
		`UPDATE recovery_custody_holds SET release_branch = '` + strings.Repeat("b", 256) + `' WHERE id = $1`,
		`UPDATE recovery_custody_holds SET release_evidence = 'worker_says_ancestor' WHERE id = $1`,
		// Issue #1582 M1 rework: a successor generation is always positive.
		`UPDATE recovery_custody_holds SET release_successor_generation = 0 WHERE id = $1`,
		`UPDATE recovery_custody_holds SET release_successor_generation = -3 WHERE id = $1`,
	} {
		_, err := pool.Exec(ctx, bad, hPartial)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("exec %q = %v, want a CHECK violation (23514)", bad, err)
		}
	}
	if state, ev := holdEvidence(hPartial); state != "open" || ev != nil {
		t.Fatalf("partial-ancestry hold state=%q evidence=%v, want untouched open", state, ev)
	}

	// ── ListReleasableCustodyHolds returns the correct per-hold reason. The list is
	// instance-wide, so assert reason on THESE hold ids rather than an exact list. ──
	// A completed-run hold whose generation matches claim_generation → reason 'publication'.
	completedRun := newRun("completed", 1)
	completedHold := openHold(completedRun, 1)
	// A hold with a ready ('available') capture on a non-completed run → reason 'archive'.
	capturedRun := newRun("failed", 0)
	capturedHold := openHold(capturedRun, 1)
	exec(`INSERT INTO recovery_captures (id, hold_id, run_id, user_id, original_worker_identity, source_sha, idempotency_key, state)
	      VALUES ($1, $2, $3, $4, $5, 'H', 'k', 'available')`, uuid.New(), capturedHold, capturedRun, userID, workerName)

	holds, err := q.ListReleasableCustodyHolds(ctx)
	if err != nil {
		t.Fatalf("ListReleasableCustodyHolds: %v", err)
	}
	reasons := map[uuid.UUID]string{}
	for _, h := range holds {
		reasons[h.ID] = h.Reason
	}
	if got, ok := reasons[completedHold]; !ok || got != "publication" {
		t.Fatalf("completed-run hold reason = %q (listed=%v), want publication (the completed-generation backstop)", got, ok)
	}
	if got, ok := reasons[capturedHold]; !ok || got != "archive" {
		t.Fatalf("captured hold reason = %q (listed=%v), want archive (a durable ready capture)", got, ok)
	}
}
