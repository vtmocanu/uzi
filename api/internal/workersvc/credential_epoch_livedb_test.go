package workersvc

import (
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestClaimRecordsCredentialEpochLiveDB (PRD #1247 M1, D7/D14) is the SERVICE-level gate
// for the attribution journal: a real svc.Claim writes exactly one run_credential_epochs
// row, keyed by the run's claim_generation AS INCREMENTED BY ClaimRun (00223, PRD #1349) —
// the epoch's generation equals the run row's generation AFTER the claim, and names the
// token the claim actually spent. It exercises the ListRunCredentialEpochs read query
// against a real database (sqlc type inference is never trusted from a clean generate,
// per .claude/rules/go.md).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestClaimRecordsCredentialEpochLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)

	// assembleClaim opens the bot PAT and an Anthropic token; seedCodexInfra stored a
	// placeholder the master box cannot decrypt, so reseal a real PAT and add a default
	// anthropic token (mirrors TestClaimOpensCustodyHoldLiveDB).
	botPATSealed, err := env.box.Seal([]byte(codexToken("bot-pat")))
	if err != nil {
		t.Fatalf("seal bot PAT: %v", err)
	}
	env.exec(`UPDATE forge_connections SET token_ciphertext = $1 WHERE user_id = $2`, botPATSealed, userID)
	anthropicSealed, err := env.box.Seal([]byte(codexToken("anthropic")))
	if err != nil {
		t.Fatalf("seal anthropic token: %v", err)
	}
	tokenID := uuid.New()
	tokenLabel := "anthropic-" + uuid.NewString()
	env.exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	          VALUES ($1, $2, 'anthropic_token', $3, true, $4, 'master')`,
		tokenID, userID, tokenLabel, anthropicSealed)

	runID := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
	          VALUES ($1, $2, $3, 'issue', 101, 't', 'd', 'queued')`, runID, userID, repoID)

	svc := New(env.q, env.box, testParams())
	// PRD #1590 M1: the run-lane claim settles in an exact-claim transaction, as main.go wires it.
	svc.SetTxBeginner(env.pool)
	wkr := store.Worker{ID: workerID, UserID: userID, Name: "worker-alpha", Status: "online"}
	payload, err := svc.Claim(env.ctx, wkr, nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload == nil || payload.RunID != runID.String() {
		t.Fatalf("Claim returned %+v, want the queued run %s", payload, runID)
	}
	// ClaimRun incremented the generation from 0 → 1, and it rode the payload.
	if payload.ClaimGeneration != 1 {
		t.Fatalf("payload.ClaimGeneration = %d, want 1", payload.ClaimGeneration)
	}

	// The run row's generation is the post-increment value.
	var runGen int64
	if err := env.pool.QueryRow(env.ctx, `SELECT claim_generation FROM runs WHERE id = $1`, runID).Scan(&runGen); err != nil {
		t.Fatalf("read run generation: %v", err)
	}
	if runGen != 1 {
		t.Fatalf("runs.claim_generation = %d, want 1", runGen)
	}

	// Exactly one epoch, carrying the run's post-increment generation and the spent token.
	epochs, err := env.q.ListRunCredentialEpochs(env.ctx, store.ListRunCredentialEpochsParams{RunID: runID, UserID: userID})
	if err != nil {
		t.Fatalf("ListRunCredentialEpochs: %v", err)
	}
	if len(epochs) != 1 {
		t.Fatalf("credential epochs = %d, want exactly 1: %+v", len(epochs), epochs)
	}
	ep := epochs[0]
	if ep.ClaimGeneration != runGen {
		t.Fatalf("epoch.claim_generation = %d, want the run's post-ClaimRun generation %d", ep.ClaimGeneration, runGen)
	}
	if !ep.SecretID.Valid || uuid.UUID(ep.SecretID.Bytes) != tokenID {
		t.Fatalf("epoch.secret_id = %+v, want the spent token %s", ep.SecretID, tokenID)
	}
	if ep.Label.String != tokenLabel {
		t.Fatalf("epoch.label = %q, want %q", ep.Label.String, tokenLabel)
	}
	// The default token was spent through an unbound worker, so the reason is `default`.
	if ep.SelectReason.String != selectReasonDefault {
		t.Fatalf("epoch.select_reason = %q, want default", ep.SelectReason.String)
	}
}
