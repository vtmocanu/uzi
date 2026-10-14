package workersvc

import (
	"reflect"
	"sort"
	"testing"

	"github.com/google/uuid"
)

// PRD #2865: follow-up runs (mr_rework, ci_fix) on the branch of a GUARDED run
// (runs.ci_config_guard, a default-branch CI repair on uzi/prompt-<id>) inherit the guard and
// the guarded run's ci_config_paths, so the rework agent or the CI-fix agent cannot push CI
// config to the repair branch. An unguarded source keeps ci_config_guard=false and its paths
// unchanged. Run via ./e2e/run-store-it.sh; skipped without UZI_TEST_DATABASE_URL.

// seedGuardedPromptRun inserts a completed guarded prompt run with an open MR on its
// deterministic uzi/prompt-<id> branch.
// sorted returns a sorted copy: Postgres orders text by collation, so array order is not part
// of the contract under test.
func sorted(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

func seedGuardedPromptRun(t *testing.T, env codexTestEnv, userID, repoID uuid.UUID, mrIID int64, paths []string) (uuid.UUID, string) {
	t.Helper()
	id := uuid.New()
	branch := "uzi/prompt-" + id.String()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, branch, mr_iid, mr_state, status, harness, ci_config_guard, ci_config_paths)
	          VALUES ($1, $2, $3, 'prompt', 'repair', 'd', $4, $5, 'opened', 'completed', 'claude', true, $6)`,
		id, userID, repoID, branch, mrIID, paths)
	return id, branch
}

func TestMRReworkOfGuardedRunInheritsGuardLiveDB(t *testing.T) {
	env, svc, userID, repoID := branchSerializeEnv(t)
	paths := []string{"b/**", ".gitlab-ci.yml"}

	// Auto and manual rework of a guarded run: guard + paths inherited.
	gid, branch := seedGuardedPromptRun(t, env, userID, repoID, 286501, paths)
	res := reviewResultOf(sampleReviewSnapshot())
	auto, err := svc.CreateAutoMRReworkRun(env.ctx, userID, repoID, branch, 286501, gid, "t", "d", res.Snapshot)
	if err != nil {
		t.Fatalf("auto rework: %v", err)
	}
	if !auto.CiConfigGuard || !reflect.DeepEqual(sorted(auto.CiConfigPaths), []string{".gitlab-ci.yml", "b/**"}) {
		t.Fatalf("auto rework guard=%v paths=%v, want guard with %v", auto.CiConfigGuard, auto.CiConfigPaths, paths)
	}
	env.exec(`UPDATE runs SET status = 'completed' WHERE id = $1`, auto.ID)
	manual, err := svc.CreateManualMRReworkRun(env.ctx, userID, repoID, branch, 286501, gid, "t", "d", res.Snapshot, res.PlanAssessed(), nil)
	if err != nil {
		t.Fatalf("manual rework: %v", err)
	}
	if !manual.CiConfigGuard || !reflect.DeepEqual(sorted(manual.CiConfigPaths), []string{".gitlab-ci.yml", "b/**"}) {
		t.Fatalf("manual rework guard=%v paths=%v, want guard with %v", manual.CiConfigGuard, manual.CiConfigPaths, paths)
	}

	// Unguarded source: unchanged defaults.
	const n int64 = 286502
	seedEligibleIssue(t, env, repoID, n)
	src := seedCompletedSourceRun(t, env, userID, repoID, n, 286502)
	plain, err := svc.CreateAutoMRReworkRun(env.ctx, userID, repoID, agentIssueBranch(n), 286502, src, "t", "d", res.Snapshot)
	if err != nil {
		t.Fatalf("plain rework: %v", err)
	}
	if plain.CiConfigGuard || len(plain.CiConfigPaths) != 0 {
		t.Fatalf("unguarded rework guard=%v paths=%v, want false/empty", plain.CiConfigGuard, plain.CiConfigPaths)
	}
}

func TestCIFixOnGuardedBranchInheritsGuardLiveDB(t *testing.T) {
	env, svc, userID, repoID := branchSerializeEnv(t)
	configured := []string{"z/**", "b/**"}

	// A guarded prompt run (here still ACTIVE, branch unrecorded): its deterministic
	// uzi/prompt-<id> branch is enough to find the owner.
	gid := uuid.New()
	branch := "uzi/prompt-" + gid.String()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, status, harness, ci_config_guard, ci_config_paths)
	          VALUES ($1, $2, $3, 'prompt', 'repair', 'd', 'running', 'claude', true, $4)`,
		gid, userID, repoID, []string{"b/**", ".gitlab-ci.yml"})
	fix, err := svc.CreateAutoCIFixRun(env.ctx, userID, repoID, branch, "Fix CI", "d", ciFixSnapshot(branch, 286510), configured)
	if err != nil {
		t.Fatalf("ci_fix on guarded branch: %v", err)
	}
	want := []string{".gitlab-ci.yml", "b/**", "z/**"}
	if !fix.CiConfigGuard || !reflect.DeepEqual(sorted(fix.CiConfigPaths), want) {
		t.Fatalf("ci_fix guard=%v paths=%v, want guard with %v", fix.CiConfigGuard, fix.CiConfigPaths, want)
	}

	// Unguarded branch: not guarded, configured set verbatim (order preserved).
	const n int64 = 286511
	plainBranch := agentIssueBranch(n)
	plain, err := svc.CreateCIFixRun(env.ctx, userID, repoID, plainBranch, "Fix CI", "d", ciFixSnapshot(plainBranch, 286511), configured)
	if err != nil {
		t.Fatalf("plain ci_fix: %v", err)
	}
	if plain.CiConfigGuard || !reflect.DeepEqual(plain.CiConfigPaths, configured) {
		t.Fatalf("unguarded ci_fix guard=%v paths=%v, want false/%v", plain.CiConfigGuard, plain.CiConfigPaths, configured)
	}
}
