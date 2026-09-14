package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1332 M5A (D6) LiveDB proof that TWO runs bound to ONE canonical Codex subscription
// account can be CLAIMED and ACTIVE CONCURRENTLY — there is NO run-long seat lock. M5A adds
// no seat-held state, unique index, whole-run claim predicate or run-long lease; the ONLY
// subscription-specific serialization is M1's brief coordinated-refresh lease (a distinct
// path, exercised by codex_m4_livedb_test.go, and NOT touched here). These tests are the C5
// acceptance for success-criterion 6.
//
// WHY SEPARATE, OVERLAPPING TRANSACTIONS (D6, explicitly): a test that claims run A, commits,
// then claims run B serially is NOT discriminating — it would pass even under a run-long seat
// lock, because the first claim's lock would already be released before the second begins. So
// these tests hold tx1 OPEN and uncommitted, claim run A inside it, then in a SEPARATE tx2
// claim run B while A's claim is still IN FLIGHT, and assert both succeed. Production's
// Service.Claim runs ClaimRun directly on the pool (one autocommit statement) whose only lock
// is `FOR UPDATE SKIP LOCKED` on the single target run row (never the subscription/account
// row); binding the generated Queries to a caller-held pgx.Tx via WithTx lets us model the
// stronger "what if the claim tx were held open" case and prove no cross-run subscription
// serialization exists even then.
//
// HOW THE TEST STAYS DISCRIMINATING:
//   - An independent (autocommit) connection reads BOTH runs as still 'queued' WHILE both
//     claim transactions are open and uncommitted — the definitive evidence the two claims
//     overlapped rather than ran one-at-a-time.
//   - Each claim tx sets a short lock_timeout, so a future regression that adds a BLOCKING
//     subscription/account lock inside the claim (a seat lock) surfaces as a deterministic
//     lock-timeout ERROR instead of hanging the suite. lock_timeout never fires on the green
//     path because ClaimRun waits on nothing (SKIP LOCKED skips the peer's run row and no
//     account row is ever locked).
//
// CALIBRATION (performed transiently against a throwaway Postgres, then REMOVED — the tree
// carries no seat lock): claimInOwnTx was temporarily given, right after BEGIN and before
// ClaimRun, a blocking `SELECT 1 FROM codex_provider_account WHERE id = <shared account> FOR
// UPDATE`, modeling a hypothetical run-long subscription seat lock taken at claim time. With
// that injected, both tests FAILED for the intended reason: tx1 took the account-row lock and
// claimed; tx2 then blocked on that same row lock and its claim aborted with SQLSTATE 55P03
// ("canceling statement due to lock timeout"). Removing the injected SELECT restored green.
// No production or schema mutation was made; the injected line lived only in this test file
// during the experiment.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (run via
// ./e2e/run-store-it.sh). Reuses the codexTestEnv fixtures (setupCodexLiveDB,
// seedLinkedSubscription, seedStagingAlias, the reconciler) and the ClaimRun params shape
// from completion_interlock_livedb_test.go. A package that prints `ok` with PASS=0 is INVALID,
// not green.

// seedCapableCodexWorker inserts an ONLINE worker advertising codex_harness_v1 (C2b's
// non-bypassable Codex-harness claim gate requires the claimant advertise it). max_concurrent_runs
// is left NULL so fleet-aware spread (PRD #216) never defers a claim to a peer — isolating the
// subscription-seat-lock question as the only thing that could serialize the two claims.
func (e codexTestEnv) seedCapableCodexWorker(t *testing.T, userID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	e.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, protocol_capabilities)
	        VALUES ($1, $2, $3, $4, 'online', $5)`,
		id, userID, "codexw-"+id.String()[:8], id[:], []string{capability.CodexHarnessV1})
	return id
}

// seedQueuedCodexRun inserts a QUEUED issue run and freezes its Codex subscription binding onto
// the given alias via the real FreezeCodexBinding path — which sets harness='codex' and, for a
// linked subscription alias, the frozen codex_account_key (the canonical identity tuple). The
// result is a Codex-indicating, claimable run bound to that subscription. iid is the issue_iid
// (distinct per repo so two active runs never collide on uq_runs_one_active_per_issue).
func (e codexTestEnv) seedQueuedCodexRun(t *testing.T, userID, repoID, aliasID uuid.UUID, iid int64) uuid.UUID {
	t.Helper()
	runID := uuid.New()
	e.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
	        VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'queued', NULL)`, runID, userID, repoID, iid)
	svc := &Service{q: e.q, box: e.box}
	if err := svc.FreezeCodexBinding(e.ctx, userID, runID, aliasID, codexAuthModeSubscription); err != nil {
		t.Fatalf("freeze codex binding on queued run %s: %v", runID, err)
	}
	return runID
}

// codexClaimParams mirrors completion_interlock_livedb_test.go's claimParams for a single-worker
// claim: cutoffs chosen so neither affinity nor fleet-spread interferes (the run is unclaimed so
// affinity passes; seeded workers carry NULL max_concurrent_runs so no peer qualifies as a spread
// target), WorkerCaps empty + capability_aware false so fn_worker_can_claim is trivially true, and
// the claimant's stored protocol caps threaded into @worker_protocol_caps — exactly as
// Service.Claim threads wkr.ProtocolCapabilities. The D3 Codex clause is thus the only capability
// gate, satisfied by the codex_harness_v1 caps passed here.
func (e codexTestEnv) codexClaimParams(userID, workerID uuid.UUID, protocolCaps []string) store.ClaimRunParams {
	now := time.Now()
	return store.ClaimRunParams{
		WorkerID:              pgtype.UUID{Bytes: workerID, Valid: true},
		UserID:                userID,
		HeartbeatCutoff:       pgtype.Timestamptz{Time: now.Add(-45 * time.Second), Valid: true},
		AffinityCutoff:        pgtype.Timestamptz{Time: now.Add(-25 * time.Minute), Valid: true},
		SpreadCutoff:          pgtype.Timestamptz{Time: now.Add(-9 * time.Second), Valid: true},
		BackgroundGraceCutoff: pgtype.Timestamptz{Time: now.Add(-15 * time.Minute), Valid: true},
		WorkerCaps:            []string{},
		CapabilityAware:       false,
		WorkerProtocolCaps:    protocolCaps,
	}
}

// runClaimState is the subset of a run's columns these tests assert on.
type runClaimState struct {
	status     string
	harness    string
	worker     uuid.UUID
	workerSet  bool
	accountKey string
	secretID   uuid.UUID
	secretSet  bool
}

// readRun reads a run's claim/binding state on a FRESH pool connection (autocommit) — used both
// as a precondition check and, mid-flight, as an INDEPENDENT observer proving the two claim
// transactions are genuinely open at the same time. A plain SELECT never blocks on a row write
// lock, so this reads the last committed snapshot without waiting on either open tx.
func (e codexTestEnv) readRun(t *testing.T, runID uuid.UUID) runClaimState {
	t.Helper()
	var st runClaimState
	var worker, secret pgtype.UUID
	var key pgtype.Text
	if err := e.pool.QueryRow(e.ctx,
		`SELECT status, harness, worker_id, codex_account_key, codex_secret_id FROM runs WHERE id = $1`, runID,
	).Scan(&st.status, &st.harness, &worker, &key, &secret); err != nil {
		t.Fatalf("read run %s: %v", runID, err)
	}
	st.worker, st.workerSet = uuid.UUID(worker.Bytes), worker.Valid
	st.accountKey = key.String
	st.secretID, st.secretSet = uuid.UUID(secret.Bytes), secret.Valid
	return st
}

// claimInOwnTx opens a NEW transaction, claims a run inside it via the generated ClaimRun bound
// to that tx, and returns the still-OPEN tx and the claimed run. It sets a short lock_timeout so
// a regression that made the claim block on a shared subscription/account lock would abort with a
// lock-timeout error rather than hang (see the file header's calibration note). On the green path
// nothing blocks, so the timeout never fires.
func (e codexTestEnv) claimInOwnTx(t *testing.T, userID, workerID uuid.UUID) (pgx.Tx, store.Run) {
	t.Helper()
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := tx.Exec(e.ctx, "SET LOCAL lock_timeout = '10s'"); err != nil {
		_ = tx.Rollback(e.ctx)
		t.Fatalf("set lock_timeout: %v", err)
	}
	run, err := e.q.WithTx(tx).ClaimRun(e.ctx, e.codexClaimParams(userID, workerID, []string{capability.CodexHarnessV1}))
	if err != nil {
		_ = tx.Rollback(e.ctx)
		t.Fatalf("claim run in tx (worker %s): %v", workerID, err)
	}
	return tx, run
}

// assertConcurrentClaimsNoSeatLock is the shared D6 proof driven by both binding shapes. runA/runB
// are two queued runs bound to ONE canonical Codex account; wA/wB are two capable workers. It
// claims each run in a SEPARATE, OVERLAPPING transaction and asserts both succeed without either
// serializing on the other's subscription, then commits both and asserts they are concurrently
// active on distinct workers.
func (e codexTestEnv) assertConcurrentClaimsNoSeatLock(t *testing.T, userID, runA, runB, wA, wB uuid.UUID) {
	t.Helper()

	// tx1: claim run A, and KEEP tx1 open and uncommitted.
	tx1, claim1 := e.claimInOwnTx(t, userID, wA)
	defer func() { _ = tx1.Rollback(e.ctx) }() // no-op after Commit; safety net on an early t.Fatalf

	// While tx1 is still open, an INDEPENDENT connection must NOT yet see its claim committed —
	// confirming tx1 is genuinely in flight (not already committed) when tx2 starts below.
	if s := e.readRun(t, claim1.ID).status; s != "queued" {
		t.Fatalf("mid-flight: run %s reads %q on an independent connection while tx1 is open; want queued — tx1 must still be uncommitted", claim1.ID, s)
	}

	// tx2: in a SEPARATE, OVERLAPPING transaction, claim run B WHILE tx1's claim is still in
	// flight. A run-long subscription seat lock would make this block on tx1 and hit the
	// lock_timeout; instead it must succeed immediately.
	tx2, claim2 := e.claimInOwnTx(t, userID, wB)
	defer func() { _ = tx2.Rollback(e.ctx) }()

	// Both claims are now IN FLIGHT SIMULTANEOUSLY. Neither blocked on the other's subscription.
	if claim1.ID == claim2.ID {
		t.Fatalf("both overlapping claims took the SAME run %s; FOR UPDATE SKIP LOCKED must hand each tx a distinct run", claim1.ID)
	}
	claimed := map[uuid.UUID]bool{claim1.ID: true, claim2.ID: true}
	if !claimed[runA] || !claimed[runB] {
		t.Fatalf("overlapping claims took %s and %s, want the two seeded runs %s and %s", claim1.ID, claim2.ID, runA, runB)
	}
	if claim1.Status != "claimed" || claim2.Status != "claimed" {
		t.Fatalf("claim statuses = %q,%q, want claimed,claimed", claim1.Status, claim2.Status)
	}
	if uuid.UUID(claim1.WorkerID.Bytes) != wA {
		t.Fatalf("claim1 worker = %x, want %s", claim1.WorkerID.Bytes, wA)
	}
	if uuid.UUID(claim2.WorkerID.Bytes) != wB {
		t.Fatalf("claim2 worker = %x, want %s", claim2.WorkerID.Bytes, wB)
	}
	// The claim UPDATE never clears the Codex binding — both stay Codex-indicating.
	if claim1.Harness != harnessCodex || claim2.Harness != harnessCodex {
		t.Fatalf("claimed run harnesses = %q,%q, want codex,codex", claim1.Harness, claim2.Harness)
	}

	// Definitive overlap proof: with BOTH claim transactions open and uncommitted, an
	// independent connection still sees BOTH runs as 'queued'. If the claims had serialized
	// (one committing before the other began), one would already read 'claimed' here.
	if s := e.readRun(t, runA).status; s != "queued" {
		t.Fatalf("mid-flight: run A (%s) reads %q on an independent connection while both claim tx are open; want queued", runA, s)
	}
	if s := e.readRun(t, runB).status; s != "queued" {
		t.Fatalf("mid-flight: run B (%s) reads %q on an independent connection while both claim tx are open; want queued", runB, s)
	}

	// Commit both: the two same-account claims durably coexist.
	if err := tx1.Commit(e.ctx); err != nil {
		t.Fatalf("commit tx1: %v", err)
	}
	if err := tx2.Commit(e.ctx); err != nil {
		t.Fatalf("commit tx2: %v", err)
	}

	// Both runs are now concurrently ACTIVE (claimed), each on its own worker.
	sA := e.readRun(t, claim1.ID)
	sB := e.readRun(t, claim2.ID)
	if sA.status != "claimed" || sB.status != "claimed" {
		t.Fatalf("post-commit statuses = %q,%q, want claimed,claimed — both same-account runs must be concurrently active", sA.status, sB.status)
	}
	if !sA.workerSet || sA.worker != wA {
		t.Fatalf("run %s worker after commit = (%v,%s), want %s", claim1.ID, sA.workerSet, sA.worker, wA)
	}
	if !sB.workerSet || sB.worker != wB {
		t.Fatalf("run %s worker after commit = (%v,%s), want %s", claim2.ID, sB.workerSet, sB.worker, wB)
	}
}

// TestCodexTwoRunsSharedSubscriptionClaimConcurrentlyLiveDB — D6 shape (a): TWO runs sharing ONE
// saved subscription credential (a single alias → one canonical account) both claim in separate
// overlapping transactions. No seat lock serializes them.
func TestCodexTwoRunsSharedSubscriptionClaimConcurrentlyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)

	// ONE saved subscription credential → ONE canonical account.
	access, refresh := codexToken("access"), codexToken("refresh")
	alias := env.seedLinkedSubscription(t, userID, "codex-shared-"+uuid.NewString(), access, refresh)

	// TWO runs, BOTH bound to that ONE saved credential.
	runA := env.seedQueuedCodexRun(t, userID, repoID, alias, 101)
	runB := env.seedQueuedCodexRun(t, userID, repoID, alias, 102)

	// Precondition: both Codex-indicating, both bound to the SAME saved alias, and — the point of
	// the "same canonical subscription" — carrying the SAME frozen account key.
	a, b := env.readRun(t, runA), env.readRun(t, runB)
	if a.harness != harnessCodex || b.harness != harnessCodex {
		t.Fatalf("both runs must be Codex-indicating; harnesses = %q,%q", a.harness, b.harness)
	}
	if a.secretID != alias || b.secretID != alias {
		t.Fatalf("shared-credential shape: both runs must bind the one saved alias %s; got %s and %s", alias, a.secretID, b.secretID)
	}
	if a.accountKey == "" || a.accountKey != b.accountKey {
		t.Fatalf("both runs must share ONE canonical account key; got %q and %q", a.accountKey, b.accountKey)
	}

	// Capable capacity for both (two workers advertising codex_harness_v1).
	wA := env.seedCapableCodexWorker(t, userID)
	wB := env.seedCapableCodexWorker(t, userID)

	env.assertConcurrentClaimsNoSeatLock(t, userID, runA, runB, wA, wB)
}

// TestCodexTwoRunsDuplicateImportsClaimConcurrentlyLiveDB — D6 shape (b): TWO runs each bound via a
// SEPARATELY SAVED IMPORT of the SAME canonical account (two aliases whose discovery converges to
// one provider account) both claim in separate overlapping transactions. Distinct credentials,
// one canonical subscription, still no seat lock.
func TestCodexTwoRunsDuplicateImportsClaimConcurrentlyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)

	// TWO separately-saved imports of ONE canonical account: two aliases whose discovery yields
	// the SAME identity tuple, so the second links to the first's account instead of creating a
	// new one (canonical duplicate-import convergence, the D6 "two aliases → one account" shape).
	tok1, tok2 := codexToken("access-1"), codexToken("access-2")
	alias1 := env.seedStagingAlias(t, userID, "codex-import-a-"+uuid.NewString(), codexLoginBlob{AccessToken: tok1, RefreshToken: codexToken("r1")})
	alias2 := env.seedStagingAlias(t, userID, "codex-import-b-"+uuid.NewString(), codexLoginBlob{AccessToken: tok2, RefreshToken: codexToken("r2")})
	fake := newFakeCodexIdentity()
	sameID := codexauth.Identity{ProviderUserID: "user-" + uuid.NewString(), WorkspaceAccountID: "acct-" + uuid.NewString()}
	fake.idByToken[tok1] = sameID
	fake.idByToken[tok2] = sameID
	r := NewCodexReconciler(env.q, nil, env.box, fake, env.pool)
	if err := r.ReconcileCodexAuthIdentity(env.ctx, userID, alias1); err != nil {
		t.Fatalf("reconcile alias1: %v", err)
	}
	if err := r.ReconcileCodexAuthIdentity(env.ctx, userID, alias2); err != nil {
		t.Fatalf("reconcile alias2: %v", err)
	}
	if n := env.countProviderAccounts(t, userID); n != 1 {
		t.Fatalf("duplicate imports must converge to 1 provider account, got %d", n)
	}

	// ONE run per SEPARATELY-SAVED import.
	runA := env.seedQueuedCodexRun(t, userID, repoID, alias1, 201)
	runB := env.seedQueuedCodexRun(t, userID, repoID, alias2, 202)

	a, b := env.readRun(t, runA), env.readRun(t, runB)
	if a.harness != harnessCodex || b.harness != harnessCodex {
		t.Fatalf("both runs must be Codex-indicating; harnesses = %q,%q", a.harness, b.harness)
	}
	// DISTINCT saved credentials …
	if a.secretID != alias1 || b.secretID != alias2 {
		t.Fatalf("each run must bind its OWN imported alias; got %s (want %s) and %s (want %s)", a.secretID, alias1, b.secretID, alias2)
	}
	if a.secretID == b.secretID {
		t.Fatalf("duplicate-imports shape must use DISTINCT saved credentials, both bound %s", a.secretID)
	}
	// … but the SAME canonical account.
	if a.accountKey == "" || a.accountKey != b.accountKey {
		t.Fatalf("both imports must resolve the SAME canonical account key; got %q and %q", a.accountKey, b.accountKey)
	}

	wA := env.seedCapableCodexWorker(t, userID)
	wB := env.seedCapableCodexWorker(t, userID)

	env.assertConcurrentClaimsNoSeatLock(t, userID, runA, runB, wA, wB)
}
