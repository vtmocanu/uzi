package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// ci_fix_binding_livedb_test.go pins issue #4079 against a REAL Postgres: a ci_fix on a
// reused branch binds to the branch's single open PR (runs.ci_fix_mr_iid), not to whichever
// older PR a source run recorded on the same branch. Skipped unless UZI_TEST_DATABASE_URL
// points at a throwaway Postgres; run via ./e2e/run-store-it.sh.

// seedRecordedPR inserts a completed issue run that recorded mrIID on branch, created at
// createdAt, the way a finished source run does.
func seedRecordedPR(t *testing.T, env codexTestEnv, userID, repoID uuid.UUID, branch string, mrIID int64, createdAt time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, mr_iid, mr_state, status, harness, created_at)
	          VALUES ($1, $2, $3, 'issue', $4, 'prior', 'd', $5, $6, 'opened', 'completed', 'claude', $7)`,
		id, userID, repoID, mrIID+400000, branch, mrIID, createdAt)
	return id
}

func countCIFixRuns(t *testing.T, env codexTestEnv, repoID uuid.UUID) int {
	t.Helper()
	var c int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM runs WHERE repo_id = $1 AND kind = 'ci_fix'`, repoID).Scan(&c); err != nil {
		t.Fatalf("count ci_fix runs: %v", err)
	}
	return c
}

// bindingEnv is branchSerializeEnv plus a scripted forge.
func bindingEnv(t *testing.T, answers map[int64]bindingAnswer) (codexTestEnv, *Service, *bindingForge, uuid.UUID, uuid.UUID) {
	t.Helper()
	env, svc, userID, repoID := branchSerializeEnv(t)
	ff := &bindingForge{answers: answers}
	svc.SetForges(bindingBuilder{ff})
	return env, svc, ff, userID, repoID
}

func seedStaleMergedAndOpen(t *testing.T, env codexTestEnv, userID, repoID uuid.UUID, ref string, stale, open int64) {
	t.Helper()
	now := time.Now()
	seedRecordedPR(t, env, userID, repoID, ref, stale, now.Add(-48*time.Hour))
	seedRecordedPR(t, env, userID, repoID, ref, open, now.Add(-time.Hour))
}

// A stale merged PR A and a newer open PR B recorded on one reused branch: the ci_fix binds to B.
func TestCIFixBindsSingleOpenPRAutoLiveDB(t *testing.T) {
	const ref = "agent/issue-41001"
	env, svc, _, userID, repoID := bindingEnv(t, map[int64]bindingAnswer{
		410010: bindSum(410010, forge.MRStateMerged, ref),
		410011: bindSum(410011, forge.MRStateOpened, ref),
	})
	seedStaleMergedAndOpen(t, env, userID, repoID, ref, 410010, 410011)

	run, err := svc.CreateAutoCIFixRun(env.ctx, userID, repoID, ref, "Fix CI", "d", ciFixSnapshot(ref, 4100100), nil)
	if err != nil {
		t.Fatalf("CreateAutoCIFixRun = %v, want success", err)
	}
	if !run.CiFixMrIid.Valid || run.CiFixMrIid.Int64 != 410011 {
		t.Fatalf("returned CiFixMrIid = %+v, want 410011", run.CiFixMrIid)
	}
	if run.MrIid.Valid {
		t.Fatalf("a ci_fix run's own mr_iid must stay NULL, got %d", run.MrIid.Int64)
	}
	var stored, own *int64
	if err := env.pool.QueryRow(env.ctx, `SELECT ci_fix_mr_iid, mr_iid FROM runs WHERE id = $1`, run.ID).Scan(&stored, &own); err != nil {
		t.Fatalf("read stored row: %v", err)
	}
	if stored == nil || *stored != 410011 || own != nil {
		t.Fatalf("stored ci_fix_mr_iid/mr_iid = %v/%v, want 410011/NULL", stored, own)
	}
}

func TestCIFixBindsSingleOpenPRManualLiveDB(t *testing.T) {
	const ref = "agent/issue-41002"
	env, svc, _, userID, repoID := bindingEnv(t, map[int64]bindingAnswer{
		410020: bindSum(410020, forge.MRStateClosed, ref),
		410021: bindSum(410021, forge.MRStateOpened, ref),
	})
	seedStaleMergedAndOpen(t, env, userID, repoID, ref, 410020, 410021)

	run, err := svc.CreateCIFixRun(env.ctx, userID, repoID, ref, "Fix CI", "d", ciFixSnapshot(ref, 4100200), nil)
	if err != nil {
		t.Fatalf("CreateCIFixRun = %v, want success", err)
	}
	if !run.CiFixMrIid.Valid || run.CiFixMrIid.Int64 != 410021 {
		t.Fatalf("CiFixMrIid = %+v, want 410021", run.CiFixMrIid)
	}
}

func TestCIFixBindingRefusalsCreateNoRunLiveDB(t *testing.T) {
	const ref = "agent/issue-41003"
	opened := func(iid int64) bindingAnswer { return bindSum(iid, forge.MRStateOpened, ref) }
	tests := []struct {
		name    string
		answers map[int64]bindingAnswer
		wantErr error
	}{
		{"two open", map[int64]bindingAnswer{410030: opened(410030), 410031: opened(410031)}, ErrCIFixPRUnresolved},
		{"404", map[int64]bindingAnswer{410030: {err: forge.ErrMergeRequestNotFound}, 410031: bindSum(410031, forge.MRStateMerged, ref)}, ErrCIFixPRUnresolved},
		{"forge error", map[int64]bindingAnswer{410030: {err: errors.New("dial tcp: connection refused")}, 410031: opened(410031)}, ErrCIFixPRUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, svc, _, userID, repoID := bindingEnv(t, tc.answers)
			seedStaleMergedAndOpen(t, env, userID, repoID, ref, 410030, 410031)
			_, err := svc.CreateAutoCIFixRun(env.ctx, userID, repoID, ref, "Fix CI", "d", ciFixSnapshot(ref, 4100300), nil)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got := countCIFixRuns(t, env, repoID); got != 0 {
				t.Fatalf("ci_fix runs after a refused binding = %d, want 0", got)
			}
		})
	}
}

func TestCIFixBindingUnboundCasesLiveDB(t *testing.T) {
	const ref = "agent/issue-41004"
	t.Run("no candidates needs no forge", func(t *testing.T) {
		env, svc, userID, repoID := branchSerializeEnv(t) // no SetForges
		run, err := svc.CreateAutoCIFixRun(env.ctx, userID, repoID, ref, "Fix CI", "d", ciFixSnapshot(ref, 4100400), nil)
		if err != nil {
			t.Fatalf("CreateAutoCIFixRun = %v, want success", err)
		}
		if run.CiFixMrIid.Valid {
			t.Fatalf("CiFixMrIid = %+v, want unbound", run.CiFixMrIid)
		}
	})
	t.Run("all merged or closed", func(t *testing.T) {
		env, svc, _, userID, repoID := bindingEnv(t, map[int64]bindingAnswer{
			410040: bindSum(410040, forge.MRStateMerged, ref),
			410041: bindSum(410041, forge.MRStateClosed, ref),
		})
		seedStaleMergedAndOpen(t, env, userID, repoID, ref, 410040, 410041)
		run, err := svc.CreateAutoCIFixRun(env.ctx, userID, repoID, ref, "Fix CI", "d", ciFixSnapshot(ref, 4100401), nil)
		if err != nil {
			t.Fatalf("CreateAutoCIFixRun = %v, want success", err)
		}
		if run.CiFixMrIid.Valid {
			t.Fatalf("CiFixMrIid = %+v, want unbound", run.CiFixMrIid)
		}
	})
}

// A run recording a new PR on the branch between the forge check and the insert makes the
// create refuse instead of binding on a stale candidate set.
func TestCIFixBindingChangedBetweenCheckAndInsertLiveDB(t *testing.T) {
	const ref = "agent/issue-41005"
	var env codexTestEnv
	var userID, repoID uuid.UUID
	inserted := false
	answers := map[int64]bindingAnswer{
		410050: bindSum(410050, forge.MRStateOpened, ref),
	}
	a := answers[410050]
	a.hook = func() {
		if !inserted {
			inserted = true
			seedRecordedPR(t, env, userID, repoID, ref, 410059, time.Now())
		}
	}
	answers[410050] = a
	env, svc, _, userID, repoID := bindingEnv(t, answers)
	seedRecordedPR(t, env, userID, repoID, ref, 410050, time.Now().Add(-time.Hour))

	_, err := svc.CreateAutoCIFixRun(env.ctx, userID, repoID, ref, "Fix CI", "d", ciFixSnapshot(ref, 4100500), nil)
	if !errors.Is(err, ErrCIFixBindingChanged) {
		t.Fatalf("err = %v, want ErrCIFixBindingChanged", err)
	}
	if got := countCIFixRuns(t, env, repoID); got != 0 {
		t.Fatalf("ci_fix runs after a changed binding = %d, want 0", got)
	}
}

// The existing branch-in-use refusal wins over a failing forge: the pool pre-check runs first.
func TestCIFixBranchInUseBeatsForgeFailureLiveDB(t *testing.T) {
	const n int64 = 41006
	ref := agentIssueBranch(n)
	env, svc, ff, userID, repoID := bindingEnv(t, map[int64]bindingAnswer{
		410060: {err: errors.New("dial tcp: connection refused")},
	})
	seedRecordedPR(t, env, userID, repoID, ref, 410060, time.Now().Add(-time.Hour))
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, harness)
	          VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'queued', 'claude')`, uuid.New(), userID, repoID, n)

	for name, create := range map[string]func() error{
		"manual": func() error {
			_, err := svc.CreateCIFixRun(env.ctx, userID, repoID, ref, "Fix CI", "d", ciFixSnapshot(ref, 4100600), nil)
			return err
		},
		"auto": func() error {
			_, err := svc.CreateAutoCIFixRun(env.ctx, userID, repoID, ref, "Fix CI", "d", ciFixSnapshot(ref, 4100601), nil)
			return err
		},
	} {
		if err := create(); !errors.Is(err, ErrBranchInUse) {
			t.Fatalf("%s: err = %v, want ErrBranchInUse", name, err)
		}
	}
	if ff.callCount() != 0 {
		t.Fatalf("forge calls = %d, want 0 (pool pre-check must refuse first)", ff.callCount())
	}
}

// An active ci_fix on the ref is ErrActiveFixExists even when the forge is failing.
func TestCIFixActiveFixBeatsForgeFailureLiveDB(t *testing.T) {
	const ref = "agent/issue-41007"
	env, svc, _, userID, repoID := bindingEnv(t, map[int64]bindingAnswer{
		410070: bindSum(410070, forge.MRStateOpened, ref),
	})
	seedRecordedPR(t, env, userID, repoID, ref, 410070, time.Now().Add(-time.Hour))
	if _, err := svc.CreateCIFixRun(env.ctx, userID, repoID, ref, "Fix CI", "d", ciFixSnapshot(ref, 4100700), nil); err != nil {
		t.Fatalf("seed ci_fix: %v", err)
	}
	svc.SetForges(bindingBuilder{&bindingForge{answers: map[int64]bindingAnswer{410070: {err: errors.New("boom")}}}})
	if _, err := svc.CreateAutoCIFixRun(env.ctx, userID, repoID, ref, "Fix CI", "d", ciFixSnapshot(ref, 4100701), nil); !errors.Is(err, ErrActiveFixExists) {
		t.Fatalf("err = %v, want ErrActiveFixExists", err)
	}
}

// An active mr_rework on the ref (pipeline_ref set, branch NULL) is ErrActiveFixExists even
// when a recorded candidate exists and the forge is failing: the same refusal the
// uq_runs_one_active_branch_ref index gave before the binding lookup, with zero forge calls.
func TestCIFixActiveMRReworkBeatsForgeFailureLiveDB(t *testing.T) {
	const ref = "agent/issue-41012"
	env, svc, ff, userID, repoID := bindingEnv(t, map[int64]bindingAnswer{
		410120: {err: errors.New("dial tcp: connection refused")},
	})
	source := seedRecordedPR(t, env, userID, repoID, ref, 410120, time.Now().Add(-time.Hour))
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, pipeline_ref, mr_iid, target_run_id, status, harness)
	          VALUES ($1, $2, $3, 'mr_rework', 't', 'd', $4, $5, $6, 'queued', 'claude')`,
		uuid.New(), userID, repoID, ref, 410120, source)

	for name, create := range map[string]func() error{
		"manual": func() error {
			_, err := svc.CreateCIFixRun(env.ctx, userID, repoID, ref, "Fix CI", "d", ciFixSnapshot(ref, 4101200), nil)
			return err
		},
		"auto": func() error {
			_, err := svc.CreateAutoCIFixRun(env.ctx, userID, repoID, ref, "Fix CI", "d", ciFixSnapshot(ref, 4101201), nil)
			return err
		},
	} {
		if err := create(); !errors.Is(err, ErrActiveFixExists) {
			t.Fatalf("%s: err = %v, want ErrActiveFixExists", name, err)
		}
	}
	if ff.callCount() != 0 {
		t.Fatalf("forge calls = %d, want 0 (pool pre-check must refuse first)", ff.callCount())
	}
}

// The bound PR is only a publication input: the claim's pr_description is identical for a bound
// and an unbound ci_fix, because the run's own mr_iid stays NULL.
func TestCIFixBindingDoesNotChangePrDescriptionLiveDB(t *testing.T) {
	const boundRef, unboundRef = "agent/issue-41008", "agent/issue-41009"
	env, svc, _, userID, repoID := bindingEnv(t, map[int64]bindingAnswer{
		410080: bindSum(410080, forge.MRStateOpened, boundRef),
	})
	seedRecordedPR(t, env, userID, repoID, boundRef, 410080, time.Now().Add(-time.Hour))
	env.exec(`INSERT INTO pr_descriptions (repo_id, mr_iid) VALUES ($1, $2)`, repoID, 410080)

	bound, err := svc.CreateAutoCIFixRun(env.ctx, userID, repoID, boundRef, "Fix CI", "d", ciFixSnapshot(boundRef, 4100800), nil)
	if err != nil || !bound.CiFixMrIid.Valid {
		t.Fatalf("bound create = %+v, %v", bound.CiFixMrIid, err)
	}
	unbound, err := svc.CreateAutoCIFixRun(env.ctx, userID, repoID, unboundRef, "Fix CI", "d", ciFixSnapshot(unboundRef, 4100801), nil)
	if err != nil || unbound.CiFixMrIid.Valid {
		t.Fatalf("unbound create = %+v, %v", unbound.CiFixMrIid, err)
	}
	if b, u := svc.claimPrDescription(env.ctx, bound), svc.claimPrDescription(env.ctx, unbound); b != nil || u != nil {
		t.Fatalf("claimPrDescription bound/unbound = %+v / %+v, want both nil", b, u)
	}
}

func TestCIFixMrIidCheckConstraintLiveDB(t *testing.T) {
	env, _, userID, repoID := branchSerializeEnv(t)
	issueRun := seedRecordedPR(t, env, userID, repoID, "agent/issue-41010", 410100, time.Now())

	assertCheckViolation := func(name, sql string, args ...any) {
		t.Helper()
		_, err := env.pool.Exec(context.Background(), sql, args...)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "runs_ci_fix_mr_iid_check" {
			t.Fatalf("%s: err = %v, want check violation runs_ci_fix_mr_iid_check", name, err)
		}
	}
	assertCheckViolation("non-ci_fix row", `UPDATE runs SET ci_fix_mr_iid = 5 WHERE id = $1`, issueRun)

	fix, err := svcForCheck(t, env, userID, repoID)
	if err != nil {
		t.Fatalf("seed ci_fix: %v", err)
	}
	assertCheckViolation("zero", `UPDATE runs SET ci_fix_mr_iid = 0 WHERE id = $1`, fix)
	assertCheckViolation("above 2^53-1", `UPDATE runs SET ci_fix_mr_iid = 9007199254740992 WHERE id = $1`, fix)
	if _, err := env.pool.Exec(env.ctx, `UPDATE runs SET ci_fix_mr_iid = 9007199254740991 WHERE id = $1`, fix); err != nil {
		t.Fatalf("2^53-1 on a ci_fix row must be accepted: %v", err)
	}
}

// svcForCheck inserts an unbound ci_fix row directly and returns its id.
func svcForCheck(t *testing.T, env codexTestEnv, userID, repoID uuid.UUID) (uuid.UUID, error) {
	t.Helper()
	id := uuid.New()
	_, err := env.pool.Exec(env.ctx, `INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, pipeline_id, pipeline_ref, status, harness)
	          VALUES ($1, $2, $3, $4, 't', 'd', 41011, 'agent/issue-41011', 'completed', 'claude')`, id, userID, repoID, runkind.CIFix)
	return id, err
}

// ListCIFixBindingCandidates returns the distinct recorded iids for exactly (user, repo, branch),
// newest first by max(created_at), ignoring other users, repos, branches and NULL mr_iid; nine
// distinct ids refuse the create as unresolved with no forge call, eight do not.
func TestCIFixBindingCandidateQueryBoundaryLiveDB(t *testing.T) {
	const ref = "agent/issue-41013"
	now := time.Now()

	t.Run("query scoping and distinct newest-first", func(t *testing.T) {
		env, svc, userID, repoID := branchSerializeEnv(t)
		otherUser, otherRepo := seedClaudeOnlyOriginUser(t, env)
		_ = svc
		// 41 is the older PR (3 runs), 42 the newer (2 runs, newest overall).
		for i, age := range []time.Duration{72, 71, 70} {
			seedRecordedPR(t, env, userID, repoID, ref, 41, now.Add(-age*time.Hour+time.Duration(i)*time.Minute))
		}
		seedRecordedPR(t, env, userID, repoID, ref, 42, now.Add(-2*time.Hour))
		seedRecordedPR(t, env, userID, repoID, ref, 42, now.Add(-1*time.Hour))
		// Unrelated records.
		seedRecordedPR(t, env, otherUser, repoID, ref, 43, now)       // another user
		seedRecordedPR(t, env, userID, otherRepo, ref, 44, now)       // another repo
		seedRecordedPR(t, env, userID, repoID, ref+"-other", 45, now) // another branch
		env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, status, harness)
		          VALUES ($1, $2, $3, 'issue', 410139, 'nil', 'd', $4, 'completed', 'claude')`, uuid.New(), userID, repoID, ref)

		got, err := env.q.ListCIFixBindingCandidates(env.ctx, store.ListCIFixBindingCandidatesParams{
			UserID: userID, RepoID: repoID, Ref: pgtype.Text{String: ref, Valid: true},
		})
		if err != nil {
			t.Fatalf("ListCIFixBindingCandidates: %v", err)
		}
		if len(got) != 2 || got[0] != 42 || got[1] != 41 {
			t.Fatalf("candidates = %v, want [42 41]", got)
		}
	})

	seedN := func(env codexTestEnv, userID, repoID uuid.UUID, n int) map[int64]bindingAnswer {
		answers := map[int64]bindingAnswer{}
		for i := 0; i < n; i++ {
			iid := int64(4101300 + i)
			seedRecordedPR(t, env, userID, repoID, ref, iid, now.Add(-time.Duration(i+1)*time.Hour))
			answers[iid] = bindSum(iid, forge.MRStateMerged, ref)
		}
		return answers
	}

	t.Run("nine distinct ids overflow", func(t *testing.T) {
		env, svc, userID, repoID := branchSerializeEnv(t)
		ff := &bindingForge{answers: seedN(env, userID, repoID, 9)}
		svc.SetForges(bindingBuilder{ff})
		got, err := env.q.ListCIFixBindingCandidates(env.ctx, store.ListCIFixBindingCandidatesParams{
			UserID: userID, RepoID: repoID, Ref: pgtype.Text{String: ref, Valid: true},
		})
		if err != nil || len(got) != 9 {
			t.Fatalf("query rows = %d, %v; want 9 rows", len(got), err)
		}
		_, err = svc.CreateAutoCIFixRun(env.ctx, userID, repoID, ref, "Fix CI", "d", ciFixSnapshot(ref, 4101301), nil)
		if !errors.Is(err, ErrCIFixPRUnresolved) {
			t.Fatalf("err = %v, want ErrCIFixPRUnresolved", err)
		}
		if ff.callCount() != 0 {
			t.Fatalf("forge calls = %d, want 0", ff.callCount())
		}
		if c := countCIFixRuns(t, env, repoID); c != 0 {
			t.Fatalf("ci_fix runs = %d, want 0", c)
		}
	})

	t.Run("eight distinct ids are not an overflow", func(t *testing.T) {
		env, svc, userID, repoID := branchSerializeEnv(t)
		ff := &bindingForge{answers: seedN(env, userID, repoID, 8)}
		svc.SetForges(bindingBuilder{ff})
		run, err := svc.CreateAutoCIFixRun(env.ctx, userID, repoID, ref, "Fix CI", "d", ciFixSnapshot(ref, 4101302), nil)
		if err != nil {
			t.Fatalf("CreateAutoCIFixRun = %v, want success", err)
		}
		if run.CiFixMrIid.Valid {
			t.Fatalf("CiFixMrIid = %+v, want unbound", run.CiFixMrIid)
		}
		if ff.callCount() != 8 {
			t.Fatalf("forge calls = %d, want 8", ff.callCount())
		}
	})
}
