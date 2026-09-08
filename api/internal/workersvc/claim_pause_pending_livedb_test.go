package workersvc

import (
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestAssembleClaimPausePendingTrueLiveDB (PRD #1190 M1) pins the PENDING-pause TRUE path of
// assembleClaim: a run row that carries a pending pause (pause_requested_at set, pause_mode =
// 'milestone') produces a claim whose PausePending is true and whose PauseMode carries the
// mode through. The claim_*_wire.json goldens only pin the false/absent case ("pause_pending":
// false), so the derivation from a live row that HAS a pending pause is otherwise untested.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestAssembleClaimPausePendingTrueLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	runID := env.seedCodexRun(t, userID, workerID, repoID) // the ordinary create path, no codex binding

	// seedCodexInfra stores a placeholder forge token that box.Open cannot decrypt; the real
	// claim assembly opens the bot PAT, so reseal it with a genuine master-box blob.
	botPATSealed, err := env.box.Seal([]byte(codexToken("bot-pat")))
	if err != nil {
		t.Fatalf("seal bot PAT: %v", err)
	}
	env.exec(`UPDATE forge_connections SET token_ciphertext = $1 WHERE user_id = $2`, botPATSealed, userID)
	// A default Anthropic token so the real claim assembly (openAnthropic) resolves a
	// credential — an ordinary run always spends one.
	anthropicSealed, err := env.box.Seal([]byte(codexToken("anthropic")))
	if err != nil {
		t.Fatalf("seal anthropic token: %v", err)
	}
	env.exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	          VALUES ($1, $2, 'anthropic_token', $3, true, $4, 'master')`,
		uuid.New(), userID, "anthropic-"+uuid.NewString(), anthropicSealed)

	// Arm a PENDING pause on the run row: pause_requested_at set (⇒ PausePending) and a valid
	// pause_mode. assembleClaim derives PausePending from pause_requested_at IS NOT NULL and
	// passes pause_mode through, both read off the loaded run row.
	env.exec(`UPDATE runs SET pause_requested_at = now(), pause_mode = 'milestone' WHERE id = $1`, runID)

	svc := New(env.q, env.box, testParams())
	wkr := store.Worker{ID: workerID, UserID: userID}

	run := mustRun(t, env, runID)
	if !run.PauseRequestedAt.Valid {
		t.Fatalf("seeded run must carry a pending pause (pause_requested_at set)")
	}
	if run.PauseMode.String != "milestone" {
		t.Fatalf("seeded run pause_mode = %q, want milestone", run.PauseMode.String)
	}

	payload, err := svc.assembleClaim(env.ctx, wkr, run)
	if err != nil {
		t.Fatalf("assembleClaim: %v", err)
	}
	if !payload.PausePending {
		t.Fatalf("assembled claim PausePending = false, want true for a run with a pending pause")
	}
	if payload.PauseMode != "milestone" {
		t.Fatalf("assembled claim PauseMode = %q, want milestone", payload.PauseMode)
	}
}
