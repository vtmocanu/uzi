package workersvc

import (
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestJudgeClaimCarriesClaimGenerationLiveDB (PRD #1247 M2 fix round) is the production-path
// gate for the judge lane's claim generation. The judge lane forks in assembleClaim BEFORE
// the ordinary `ClaimGeneration: run.ClaimGeneration` set, so assembleJudgeClaim must carry
// the field itself. A real svc.Claim runs the real ClaimRun (00223, PRD #1349), which
// increments the judge run's claim_generation from 0 to >= 1; the assembled payload must
// equal that exact post-claim row value — NOT a hard-coded/seeded number.
//
// Regression: with assembleJudgeClaim omitting ClaimGeneration, the payload ships 0 (the
// wire field is a non-pointer int64) while the row is >= 1. Downstream that makes the judge
// runner's running/completed reports and usage batch fence out on every credential_switch_v1
// worker (ErrMissingClaimGeneration / staleClaim), silently breaking the whole judge lane.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestJudgeClaimCarriesClaimGenerationLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)

	// Judge default-bind resolves the owner's default Anthropic token (D6); the seeded
	// column default is 'auto', so pin it to 'default' for a deterministic static path, and
	// seed one default token for openAnthropic(nil) to resolve.
	env.exec(`UPDATE users SET judge_anthropic_bind_mode = 'default' WHERE id = $1`, userID)
	env.seedAnthropicSecret(t, userID, "anthropic-"+uuid.NewString(), true)

	// The reviewed target run: completed (so it is NOT itself claimable) and carries a repo
	// so GetRunByID resolves it for the best-effort failure-class read.
	targetID := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
	          VALUES ($1, $2, $3, 'issue', 909, 't', 'd', 'completed')`, targetID, userID, repoID)

	// The judge run: repo-less, queued, pointing at the target. This is the only queued run,
	// so svc.Claim picks it.
	judgeID := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, target_run_id)
	          VALUES ($1, $2, 'judge', 't', 'd', 'queued', $3)`, judgeID, userID, targetID)

	svc := New(env.q, env.box, testParams())
	svc.SetSettings(fakeSettings{enabled: true, model: "haiku"})
	wkr := store.Worker{ID: workerID, UserID: userID, Name: "worker-judge", Status: "online"}

	payload, err := svc.Claim(env.ctx, wkr)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload == nil || payload.RunID != judgeID.String() {
		t.Fatalf("Claim returned %+v, want the queued judge run %s", payload, judgeID)
	}
	if payload.Kind != runkind.Judge {
		t.Fatalf("payload.Kind = %q, want judge", payload.Kind)
	}

	// The run row's generation AFTER ClaimRun (the authoritative value the fence compares).
	var runGen int64
	if err := env.pool.QueryRow(env.ctx, `SELECT claim_generation FROM runs WHERE id = $1`, judgeID).Scan(&runGen); err != nil {
		t.Fatalf("read judge run generation: %v", err)
	}
	if runGen < 1 {
		t.Fatalf("runs.claim_generation = %d, want >= 1 after ClaimRun", runGen)
	}
	// The payload MUST carry that exact row value, not 0. This is the assertion that reddens
	// on the unfixed assembleJudgeClaim (which omitted the field).
	if payload.ClaimGeneration != runGen {
		t.Fatalf("payload.ClaimGeneration = %d, want the run's post-ClaimRun generation %d", payload.ClaimGeneration, runGen)
	}
}
