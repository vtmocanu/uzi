package workersvc

import (
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestClaimOpensCustodyHoldLiveDB (PRD #1296 M1) is the SERVICE-level gate for the durable
// recovery claim/custody wiring: a real svc.Claim by a recovery-capable worker on a
// code-publishing run increments claim_generation, opens exactly one OPEN custody hold in
// the SAME claim (with live FKs and the worker identity), and returns the generation in the
// ClaimPayload. The mirror case — a NON-capable worker — proves the D9 additive contract:
// the generation still increments but NO hold is opened, so an old worker on a supporting API
// is honestly unsupported rather than falsely promised recovery.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestClaimOpensCustodyHoldLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)

	// assembleClaim opens the bot PAT and an Anthropic token; seedCodexInfra stored a
	// placeholder the master box cannot decrypt, so reseal a real PAT and add a default token
	// (mirrors the pause-pending live-DB test).
	botPATSealed, err := env.box.Seal([]byte(codexToken("bot-pat")))
	if err != nil {
		t.Fatalf("seal bot PAT: %v", err)
	}
	env.exec(`UPDATE forge_connections SET token_ciphertext = $1 WHERE user_id = $2`, botPATSealed, userID)
	anthropicSealed, err := env.box.Seal([]byte(codexToken("anthropic")))
	if err != nil {
		t.Fatalf("seal anthropic token: %v", err)
	}
	env.exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	          VALUES ($1, $2, 'anthropic_token', $3, true, $4, 'master')`,
		uuid.New(), userID, "anthropic-"+uuid.NewString(), anthropicSealed)

	seedQueuedRun := func(iid int64) uuid.UUID {
		id := uuid.New()
		env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
		          VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'queued')`, id, userID, repoID, iid)
		return id
	}
	countHolds := func(runID uuid.UUID) int {
		var n int
		if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM recovery_custody_holds WHERE run_id = $1 AND state = 'open'`, runID).Scan(&n); err != nil {
			t.Fatalf("count holds: %v", err)
		}
		return n
	}

	svc := New(env.q, env.box, testParams())

	// ── Recovery-capable worker: claim opens a hold. ──
	runA := seedQueuedRun(101)
	capableWkr := store.Worker{
		ID: workerID, UserID: userID, Name: "worker-alpha", Status: "online",
		ProtocolCapabilities: []string{capability.RecoveryArchiveV1},
	}
	payload, err := svc.Claim(env.ctx, capableWkr)
	if err != nil {
		t.Fatalf("Claim(capable): %v", err)
	}
	if payload == nil {
		t.Fatal("Claim(capable) returned idle (nil), want a claimed run")
	}
	if payload.RunID != runA.String() {
		t.Fatalf("claimed run = %s, want %s", payload.RunID, runA)
	}
	if payload.ClaimGeneration != 1 {
		t.Fatalf("payload.ClaimGeneration = %d, want 1", payload.ClaimGeneration)
	}
	if countHolds(runA) != 1 {
		t.Fatalf("open holds for run A = %d, want 1 (opened in the claim)", countHolds(runA))
	}
	var (
		holdIdentity                  string
		liveWorker, liveRun, origWkID []byte
		holdGen                       int64
	)
	if err := env.pool.QueryRow(env.ctx, `SELECT original_worker_identity, live_worker_id, live_run_id, original_worker_id, generation
	         FROM recovery_custody_holds WHERE run_id = $1`, runA).Scan(&holdIdentity, &liveWorker, &liveRun, &origWkID, &holdGen); err != nil {
		t.Fatalf("read hold: %v", err)
	}
	if holdIdentity != workerIdentity(capableWkr) {
		t.Fatalf("original_worker_identity = %q, want %q", holdIdentity, workerIdentity(capableWkr))
	}
	if uuid.UUID(liveWorker) != workerID || uuid.UUID(liveRun) != runA || uuid.UUID(origWkID) != workerID {
		t.Fatalf("hold FKs = live_worker %x live_run %x orig %x, want worker/run/worker", liveWorker, liveRun, origWkID)
	}
	if holdGen != 1 {
		t.Fatalf("hold generation = %d, want 1", holdGen)
	}

	// ── Non-capable worker: claim increments the generation but opens NO hold (D9). ──
	runB := seedQueuedRun(102)
	plainWkr := store.Worker{ID: workerID, UserID: userID, Name: "worker-alpha", Status: "online"} // no recovery_archive_v1
	payloadB, err := svc.Claim(env.ctx, plainWkr)
	if err != nil {
		t.Fatalf("Claim(non-capable): %v", err)
	}
	if payloadB == nil {
		t.Fatal("Claim(non-capable) returned idle, want a claimed run")
	}
	if payloadB.RunID != runB.String() {
		t.Fatalf("non-capable claimed run = %s, want %s", payloadB.RunID, runB)
	}
	if payloadB.ClaimGeneration != 1 {
		t.Fatalf("non-capable payload.ClaimGeneration = %d, want 1 (increment is unconditional)", payloadB.ClaimGeneration)
	}
	if countHolds(runB) != 0 {
		t.Fatalf("open holds for run B = %d, want 0 (a non-capable worker opens no hold)", countHolds(runB))
	}
}
