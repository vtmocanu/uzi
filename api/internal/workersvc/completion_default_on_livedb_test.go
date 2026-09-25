package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/settings"
)

// completion_default_on_livedb_test.go pins the default-on stamp for both harnesses
// end to end: a REAL settings.Cache over the test database (no fake reader), the production
// Service.CreateRun path, and the harness the create transaction actually resolves. The
// completion_interlock_rollout row is instance-global, so these cases run serially (no
// t.Parallel) and each deletes the row before it runs and again in t.Cleanup. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh. Every
// name ends LiveDB so the store-IT sweep selects it.

// resetInterlockSetting deletes the completion_interlock_rollout row now and on cleanup, so
// every case starts from (and leaves) the no-row state.
func resetInterlockSetting(t *testing.T, env codexTestEnv) {
	t.Helper()
	del := func() {
		if _, err := env.pool.Exec(env.ctx, `DELETE FROM app_settings WHERE key = $1`, settings.KeyCompletionInterlockRollout); err != nil {
			t.Errorf("delete %s row: %v", settings.KeyCompletionInterlockRollout, err)
		}
	}
	del()
	t.Cleanup(del)
}

// interlockLiveSvc wires the production create seams: the pool as the tx beginner and a
// FRESH real settings.Cache (so no snapshot leaks between cases) as the interlock reader.
func interlockLiveSvc(env codexTestEnv) *Service {
	svc := atomicSvc(env)
	svc.SetTxBeginner(env.pool)
	svc.SetCompletionInterlockSettings(settings.New(env.q, time.Minute))
	return svc
}

// createInterlockRun seeds an eligible issue, creates a run through Service.CreateRun,
// asserts the committed row's harness, and returns the run id.
func createInterlockRun(t *testing.T, env codexTestEnv, svc *Service, userID, repoID uuid.UUID, iid int64, want Harness) uuid.UUID {
	t.Helper()
	seedEligibleIssue(t, env, repoID, iid)
	run, err := svc.CreateRun(env.ctx, userID, repoID, iid, "desc", nil, nil, false, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	assertRunHarness(t, env, run.ID, want)
	return run.ID
}

func assertContractVersion(t *testing.T, env codexTestEnv, runID uuid.UUID, wantStamped bool) {
	t.Helper()
	got := mustRun(t, env, runID).CompletionContractVersion
	switch {
	case wantStamped && (!got.Valid || got.Int32 != 1):
		t.Fatalf("run %s completion_contract_version = %+v, want 1 (interlocked)", runID, got)
	case !wantStamped && got.Valid:
		t.Fatalf("run %s completion_contract_version = %d, want NULL (legacy)", runID, got.Int32)
	}
}

func setInterlockSetting(t *testing.T, env codexTestEnv, value string) {
	t.Helper()
	env.exec(`INSERT INTO app_settings (key, value) VALUES ($1, $2)
	          ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
		settings.KeyCompletionInterlockRollout, value)
}

// (i) No row: the setting defaults ON, so a Claude-harness issue run is interlocked.
func TestCompletionInterlockDefaultOnStampsClaudeLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	resetInterlockSetting(t, env)
	userID, repoID := seedClaudeOnlyOriginUser(t, env)
	svc := interlockLiveSvc(env)

	const iid = 92001
	runID := createInterlockRun(t, env, svc, userID, repoID, iid, HarnessClaude)
	assertContractVersion(t, env, runID, true)
}

// (ii) No row + a Codex-only user: the default-on switch stamps the Codex run.
func TestCompletionInterlockDefaultOnStampsCodexLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	resetInterlockSetting(t, env)
	userID, repoID := seedCodexOnlyOriginUser(t, env)
	svc := interlockLiveSvc(env)

	const iid = 92002
	runID := createInterlockRun(t, env, svc, userID, repoID, iid, HarnessCodex)
	assertContractVersion(t, env, runID, true)
}

// (iii) An explicit "false" row is the admin kill-switch: a Claude run stays legacy.
func TestCompletionInterlockExplicitFalseKeepsClaudeLegacyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	resetInterlockSetting(t, env)
	setInterlockSetting(t, env, "false")
	userID, repoID := seedClaudeOnlyOriginUser(t, env)
	svc := interlockLiveSvc(env)

	const iid = 92003
	runID := createInterlockRun(t, env, svc, userID, repoID, iid, HarnessClaude)
	assertContractVersion(t, env, runID, false)
}

// (iv) An explicit "true" row stamps a Codex run.
func TestCompletionInterlockExplicitTrueStampsCodexLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	resetInterlockSetting(t, env)
	setInterlockSetting(t, env, "true")
	userID, repoID := seedCodexOnlyOriginUser(t, env)
	svc := interlockLiveSvc(env)

	const iid = 92004
	runID := createInterlockRun(t, env, svc, userID, repoID, iid, HarnessCodex)
	assertContractVersion(t, env, runID, true)
}

// (v) The explicit kill switch leaves Codex runs unstamped too.
func TestCompletionInterlockExplicitFalseKeepsCodexLegacyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	resetInterlockSetting(t, env)
	setInterlockSetting(t, env, "false")
	userID, repoID := seedCodexOnlyOriginUser(t, env)
	svc := interlockLiveSvc(env)

	const iid = 92005
	runID := createInterlockRun(t, env, svc, userID, repoID, iid, HarnessCodex)
	assertContractVersion(t, env, runID, false)
}

// (vi) A seeded Codex plan stays legacy even with the default-on switch.
func TestCompletionInterlockDefaultOnKeepsSeededCodexLegacyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	resetInterlockSetting(t, env)
	userID, repoID := seedCodexOnlyOriginUser(t, env)
	svc := interlockLiveSvc(env)

	const iid = 92006
	seedEligibleIssue(t, env, repoID, iid)
	seed := &SeededPlan{PlanMD: "Implement the approved plan"}
	run, err := svc.CreateRun(env.ctx, userID, repoID, iid, "desc", nil, nil, false, seed, nil, nil)
	if err != nil {
		t.Fatalf("CreateRun (seeded Codex): %v", err)
	}
	assertRunHarness(t, env, run.ID, HarnessCodex)
	assertContractVersion(t, env, run.ID, false)
}
