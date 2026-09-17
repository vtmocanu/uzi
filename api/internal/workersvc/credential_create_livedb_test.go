package workersvc

import (
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestCreateRunOverrideFirstClaimSpendsPinnedTokenLiveDB (PRD #1247 M2) proves the
// create-time credential override flows THROUGH the create path — svc.CreateRun → createRun →
// the INSERT — and the created run's FIRST real claim then spends the pinned token B on a
// worker BOUND to token A. This differs from M1's TestRunOverrideBeatsWorkerBindLiveDB, which
// set the override columns DIRECTLY via a store insert to isolate the resolution ladder; here
// the columns must flow through the create path, which is the M2 guarantee: `uzi run create
// --token B` spends B from turn one whatever the worker binds. Following the CreateRun
// "guard per creation path with a test, not the compiler" convention (runtime.sql).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh. A package that prints `ok` with PASS=0 is INVALID, not green.
func TestCreateRunOverrideFirstClaimSpendsPinnedTokenLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())

	userID, workerID, repoID := env.seedCodexInfra(t)
	// Claim opens the forge PAT alongside the Anthropic credential, so the placeholder PAT
	// seedCodexInfra leaves must be resealed decryptable first (mirrors the M1 override test).
	env.sealBotPAT(t, userID)

	// A is the worker binding; B is the create-time run-pinned override target (distinct).
	tokenA := env.seedAnthropicSecret(t, userID, "worker-bound-a-"+uuid.NewString(), true)
	tokenB := env.seedAnthropicSecret(t, userID, "run-pinned-b-"+uuid.NewString(), false)

	// A cached, uzi-labelled issue so svc.CreateRun passes the single run-eligibility gate
	// (PRD #764 M1). The default uzi_label is "uzi" with no settings wired.
	const iid = 4247
	env.exec(`INSERT INTO issues (id, repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
	          VALUES ($1, $2, $3, 'M2 issue', 'opened', '["uzi"]', 'https://forge.e2e/i', false, now(), now())`,
		uuid.New(), repoID, int64(iid))

	// Create THROUGH the create path with a pinned override to B (what the M2 handler resolves
	// and passes; here we call the service method directly to prove createRun threads it).
	waitFalse := false
	pinnedB := &RawCredentialOverride{Mode: CredentialOverrideModePinned, SecretID: &tokenB}
	run, err := svc.CreateRun(env.ctx, userID, repoID, iid, "desc", &waitFalse, nil, false /*force*/, nil /*seed*/, nil /*explicit*/, pinnedB)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// The override columns landed on the created row — createRun threaded them into the INSERT.
	if run.CredentialOverrideMode.String != CredentialOverrideModePinned {
		t.Fatalf("created run credential_override_mode = %q, want pinned", run.CredentialOverrideMode.String)
	}
	if !run.CredentialOverrideSecretID.Valid || uuid.UUID(run.CredentialOverrideSecretID.Bytes) != tokenB {
		t.Fatalf("created run credential_override_secret_id = %+v, want the pinned target %s", run.CredentialOverrideSecretID, tokenB)
	}

	// The worker is BOUND (pinned) to A; the run's first claim must still spend the
	// create-pinned B — the run override outranks the worker binding (D2).
	wkr := store.Worker{
		ID: workerID, UserID: userID, Name: "worker-bound-to-a", Status: "online",
		AnthropicBindMode: BindModePinned,
		AnthropicSecretID: pgconv.UUID(tokenA),
	}
	payload, err := svc.Claim(env.ctx, wkr, nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload == nil || payload.RunID != run.ID.String() {
		t.Fatalf("Claim returned %+v, want the created run %s", payload, run.ID)
	}

	claimed := mustRun(t, env, run.ID)
	if !claimed.AnthropicSecretID.Valid || uuid.UUID(claimed.AnthropicSecretID.Bytes) != tokenB {
		t.Fatalf("first claim spent %+v, want the create-time pinned token B %s (the worker's bind A %s must have LOST)",
			claimed.AnthropicSecretID, tokenB, tokenA)
	}
	if claimed.AnthropicSelectReason.String != selectReasonRunPinned {
		t.Fatalf("anthropic_select_reason = %q, want %q", claimed.AnthropicSelectReason.String, selectReasonRunPinned)
	}
}
