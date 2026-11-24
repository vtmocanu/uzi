package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// ci_autofix_override_livedb_test.go pins issue #4617 against a REAL Postgres: a ci_fix run
// copies the selected run's ci_autofix_enabled override at creation, and the AUTOMATIC create
// re-checks eligibility inside the create transaction. Skipped unless UZI_TEST_DATABASE_URL
// points at a throwaway Postgres; run via ./e2e/run-store-it.sh.

func setRunOverride(env codexTestEnv, runID uuid.UUID, v *bool) {
	var arg any
	if v != nil {
		arg = *v
	}
	env.exec(`UPDATE runs SET ci_autofix_enabled = $2 WHERE id = $1`, runID, arg)
}

func setOwnerAutofix(env codexTestEnv, userID uuid.UUID, v *bool) {
	var arg any
	if v != nil {
		arg = *v
	}
	env.exec(`UPDATE users SET ci_autofix_enabled = $2 WHERE id = $1`, userID, arg)
}

func ptrBool(b bool) *bool { return &b }

// openedBinding is a bindingEnv whose recorded PR mrIID answers opened on ref, with one
// completed source run recording it (the run the candidate listing selects).
func openedBinding(t *testing.T, ref string, mrIID int64) (codexTestEnv, *Service, uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	env, svc, _, userID, repoID := bindingEnv(t, map[int64]bindingAnswer{
		mrIID: bindSum(mrIID, forge.MRStateOpened, ref),
	})
	source := seedRecordedPR(t, env, userID, repoID, ref, mrIID, time.Now().Add(-time.Hour))
	return env, svc, userID, repoID, source
}

func assertEnabled(t *testing.T, name string, got pgtype.Bool, want *bool) {
	t.Helper()
	if want == nil {
		if got.Valid {
			t.Fatalf("%s: CiAutofixEnabled = %+v, want NULL", name, got)
		}
		return
	}
	if !got.Valid || got.Bool != *want {
		t.Fatalf("%s: CiAutofixEnabled = %+v, want %v", name, got, *want)
	}
}

// A manual Fix CI copies the selected run's override verbatim, false included, and the finished
// fix (now the newest run on the branch) keeps the branch out of the candidate listing.
func TestCIFixManualInheritsOverrideLiveDB(t *testing.T) {
	const ref = "agent/issue-46170"
	const mr int64 = 461700
	env, svc, userID, repoID, source := openedBinding(t, ref, mr)
	setRunOverride(env, source, ptrBool(false))

	fix, err := svc.CreateCIFixRun(env.ctx, userID, repoID, ref, "Fix CI", "d", ciFixSnapshot(ref, 4617000), nil)
	if err != nil {
		t.Fatalf("manual create must not be refused by an opt-out: %v", err)
	}
	assertEnabled(t, "manual create", fix.CiAutofixEnabled, ptrBool(false))

	// The fix completes with the branch and the MR it fixed, so it becomes the selected run.
	env.exec(`UPDATE runs SET status = 'completed', branch = $2, mr_iid = $3 WHERE id = $1`, fix.ID, ref, mr)
	env.exec(`INSERT INTO pipeline_statuses (repo_id, ref, pipeline_id, sha, status, web_url, synced_at)
	          VALUES ($1, $2, 4617001, 'deadbeef', 'failed', 'https://forge.e2e/p', now())`, repoID, ref)

	refs := func() map[string]bool {
		rows, err := env.q.ListCIAutofixCandidateRefs(env.ctx, repoID)
		if err != nil {
			t.Fatalf("ListCIAutofixCandidateRefs: %v", err)
		}
		m := map[string]bool{}
		for _, r := range rows {
			m[r.Ref.String] = true
		}
		return m
	}
	sel, err := env.q.GetCIAutofixSelectedRun(env.ctx, store.GetCIAutofixSelectedRunParams{
		RepoID: repoID, Ref: pgtype.Text{String: ref, Valid: true},
	})
	if err != nil || sel.ID != fix.ID {
		t.Fatalf("selected run = %v, %v; want the completed fix %s", sel.ID, err, fix.ID)
	}
	if refs()[ref] {
		t.Fatalf("a completed fix inheriting an opt-out must keep %s out of the candidate listing", ref)
	}
	// Sensitivity control: with the inherited override cleared the same ref is a candidate.
	setRunOverride(env, fix.ID, nil)
	if !refs()[ref] {
		t.Fatalf("with the override cleared %s must be a candidate", ref)
	}
}

func TestCIFixManualNoSelectedRunLiveDB(t *testing.T) {
	const ref = "agent/issue-46171"
	env, svc, userID, repoID := branchSerializeEnv(t)
	fix, err := svc.CreateCIFixRun(env.ctx, userID, repoID, ref, "Fix CI", "d", ciFixSnapshot(ref, 4617100), nil)
	if err != nil {
		t.Fatalf("manual create with no selected run: %v", err)
	}
	assertEnabled(t, "manual no selected run", fix.CiAutofixEnabled, nil)
}

func TestCIFixAutoInheritsOverrideLiveDB(t *testing.T) {
	t.Run("run true beats owner false", func(t *testing.T) {
		const ref = "agent/issue-46172"
		env, svc, userID, repoID, source := openedBinding(t, ref, 461720)
		setOwnerAutofix(env, userID, ptrBool(false))
		setRunOverride(env, source, ptrBool(true))
		fix, err := svc.CreateAutoCIFixRun(env.ctx, userID, repoID, ref, "Fix CI", "d", ciFixSnapshot(ref, 4617200), nil)
		if err != nil {
			t.Fatalf("CreateAutoCIFixRun: %v", err)
		}
		assertEnabled(t, "run true / owner false", fix.CiAutofixEnabled, ptrBool(true))
	})
	t.Run("run NULL and owner NULL inherits NULL", func(t *testing.T) {
		const ref = "agent/issue-46173"
		env, svc, userID, repoID, _ := openedBinding(t, ref, 461730)
		fix, err := svc.CreateAutoCIFixRun(env.ctx, userID, repoID, ref, "Fix CI", "d", ciFixSnapshot(ref, 4617300), nil)
		if err != nil {
			t.Fatalf("CreateAutoCIFixRun: %v", err)
		}
		assertEnabled(t, "run NULL / owner NULL", fix.CiAutofixEnabled, nil)
	})
}

// Every automatic refusal returns exactly ErrCIAutofixIneligible and inserts no ci_fix row.
func TestCIFixAutoIneligibleCreatesNothingLiveDB(t *testing.T) {
	assertRefused := func(t *testing.T, env codexTestEnv, svc *Service, userID, repoID uuid.UUID, ref string, pipeline int64) {
		t.Helper()
		_, err := svc.CreateAutoCIFixRun(env.ctx, userID, repoID, ref, "Fix CI", "d", ciFixSnapshot(ref, pipeline), nil)
		if !errors.Is(err, ErrCIAutofixIneligible) {
			t.Fatalf("err = %v, want ErrCIAutofixIneligible", err)
		}
		if got := countCIFixRuns(t, env, repoID); got != 0 {
			t.Fatalf("ci_fix runs after a refusal = %d, want 0", got)
		}
	}

	t.Run("run override false", func(t *testing.T) {
		const ref = "agent/issue-46174"
		env, svc, userID, repoID, source := openedBinding(t, ref, 461740)
		setRunOverride(env, source, ptrBool(false))
		assertRefused(t, env, svc, userID, repoID, ref, 4617400)
	})
	t.Run("run NULL and owner false", func(t *testing.T) {
		const ref = "agent/issue-46175"
		env, svc, userID, repoID, _ := openedBinding(t, ref, 461750)
		setOwnerAutofix(env, userID, ptrBool(false))
		assertRefused(t, env, svc, userID, repoID, ref, 4617500)
	})
	t.Run("only qualifying run is cancelled", func(t *testing.T) {
		const ref = "agent/issue-46176"
		// A cancelled run is still a binding candidate, so the opened answer lets the binding
		// pass and the refusal can only come from the in-transaction recheck.
		env, svc, userID, repoID, source := openedBinding(t, ref, 461760)
		env.exec(`UPDATE runs SET status = 'cancelled' WHERE id = $1`, source)
		assertRefused(t, env, svc, userID, repoID, ref, 4617600)
	})
	t.Run("selected run belongs to another owner", func(t *testing.T) {
		const ref = "agent/issue-46177"
		env, svc, userID, repoID := branchSerializeEnv(t)
		other, _ := seedClaudeOnlyOriginUser(t, env)
		// Another user's run on this user's repo: the candidate listing has no user filter, so
		// it is the selected run, and the automatic create must refuse to act on it.
		seedRecordedPR(t, env, other, repoID, ref, 461770, time.Now().Add(-time.Hour))
		assertRefused(t, env, svc, userID, repoID, ref, 4617700)
	})
}
