package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestLinkedTaskLifecycleLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	user, repo := seedCodexOnlyOriginUser(t, env)
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool)
	svc.SetForges(m2Builder{&linkedTaskForge{issue: forge.Issue{IID: 42, State: "closed"}}})
	iid := int64(42)
	create := func() store.Run {
		t.Helper()
		run, err := svc.CreateTaskRun(env.ctx, user, repo, "implement inline context", "main", false, true, true, false, &iid)
		if err != nil {
			t.Fatal(err)
		}
		if run.IssueIid.Valid || !run.ContinuedIssueIid.Valid || run.ContinuedIssueIid.Int64 != 42 {
			t.Fatalf("wrong run identity: %+v", run)
		}
		return run
	}
	undispatched := create()
	if undispatched.DispatchedAt.Valid || undispatched.Status != "queued" {
		t.Fatalf("expected queued undispatched task: %+v", undispatched)
	}
	cancelled, err := env.q.CancelRunServerSide(env.ctx, store.CancelRunServerSideParams{ID: undispatched.ID, UserID: user})
	if err != nil || cancelled != 1 {
		t.Fatalf("cancel undispatched rows=%d error=%v", cancelled, err)
	}
	original := create() // cancellation of the undispatched task released the shared guard
	if _, err := svc.CreateTaskRun(env.ctx, user, repo, "duplicate", "", false, false, false, false, &iid); !errors.Is(err, ErrActiveRunExists) {
		t.Fatalf("duplicate error=%v", err)
	}
	if _, err := svc.DispatchTaskRun(env.ctx, user, original.ID); err != nil {
		t.Fatal(err)
	}
	env.exec("UPDATE runs SET status='completed', finished_at=now() WHERE id=$1", original.ID)
	original = mustRun(t, env, original.ID)
	svc.maybeEnqueueTaskReview(env.ctx, original)
	reviewID, _ := firstTaskRunForReviewTarget(t, env, original.ID)
	review := mustRun(t, env, reviewID)
	assertLinkedChild := func(run store.Run) {
		t.Helper()
		if run.IssueIid.Valid || run.ContinuedIssueIid != original.ContinuedIssueIid || run.Branch != original.Branch || run.Harness != original.Harness || !run.DispatchedAt.Valid {
			t.Fatalf("child lost inherited identity: %+v", run)
		}
	}
	assertLinkedChild(review)
	env.exec("UPDATE runs SET status='completed', finished_at=now() WHERE id=$1", review.ID)
	seedCompleteTaskReview(t, env, original.ID, review.ID, user)
	svc.maybeEnqueueThenFix(env.ctx, mustRun(t, env, review.ID))
	fixID, _ := firstTaskRunForThenFixOriginal(t, env, original.ID)
	fix := mustRun(t, env, fixID)
	assertLinkedChild(fix)
	if fix.BudgetWallSeconds != original.BudgetWallSeconds || fix.BudgetMaxIterations != original.BudgetMaxIterations {
		t.Fatal("fix lost original budget")
	}
	rows, err := env.q.CancelRunServerSide(env.ctx, store.CancelRunServerSideParams{ID: fix.ID, UserID: user})
	if err != nil || rows != 1 {
		t.Fatalf("cancel rows=%d error=%v", rows, err)
	}
	replacement := create() // cancellation released the shared guard
	env.exec("UPDATE runs SET created_at=now()-interval '1 hour' WHERE id=$1", replacement.ID)
	expired, err := env.q.SweepTaskNeverDispatched(env.ctx, store.SweepTaskNeverDispatchedParams{FailureReason: pgconv.Text("setup expired"), Cutoff: pgconv.Time(time.Now().Add(-time.Minute))})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range expired {
		if r.ID == replacement.ID {
			found = true
		}
	}
	if !found || mustRun(t, env, replacement.ID).FailOrigin.String != "task_undispatched" {
		t.Fatal("undispatched task was not expired")
	}
	latest := create() // expiry also released the guard
	detail, err := env.q.GetRunByIDForUser(env.ctx, store.GetRunByIDForUserParams{ID: latest.ID, UserID: user})
	if err != nil || detail.ContinuedIssueIid != original.ContinuedIssueIid || detail.IssueIid.Valid {
		t.Fatalf("detail identity: %+v %v", detail, err)
	}
	listed, err := env.q.ListRunsForUser(env.ctx, store.ListRunsForUserParams{UserID: user, BackgroundGraceCutoff: pgconv.Time(time.Now())})
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, r := range listed {
		if r.Run.ID == latest.ID {
			found = true
			if r.Run.ContinuedIssueIid != original.ContinuedIssueIid || r.Run.IssueIid.Valid {
				t.Fatal("list lost continued link")
			}
		}
	}
	if !found {
		t.Fatal("latest linked task missing from list")
	}
}

func TestLinkedTaskInterposedCreatorLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	user, repo := seedCodexOnlyOriginUser(t, env)
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool)
	for _, child := range []string{"review", "fix"} {
		t.Run(child, func(t *testing.T) {
			iid := int64(51)
			if child == "fix" {
				iid = 52
			}
			originalID := seedTaskRun(t, env, user, repo, "uzi/task/"+uuid.NewString(), "codex", "completed", true, true)
			env.exec("UPDATE runs SET continued_issue_iid=$2 WHERE id=$1", originalID, iid)
			original := mustRun(t, env, originalID)
			interposed := uuid.New()
			env.exec("INSERT INTO runs (id,user_id,repo_id,kind,issue_iid,issue_title,issue_description) VALUES ($1,$2,$3,'issue',$4,'issue','')", interposed, user, repo, iid)
			createChild := func() (store.Run, error) {
				if child == "review" {
					return svc.CreateTaskReviewRun(env.ctx, user, repo, originalID, original.Branch.String, "main", HarnessCodex)
				}
				return svc.CreateThenFixRun(env.ctx, user, repo, originalID, original.Branch.String, "main", "fix", pgtype.Int4{}, pgtype.Int4{}, HarnessCodex)
			}
			if _, err := createChild(); !errors.Is(err, ErrActiveRunExists) {
				t.Fatalf("interposed conflict=%v", err)
			}
			env.exec("UPDATE runs SET status='pool_wait' WHERE id=$1", interposed)
			// Initial handoffs still treat a held run as active; derived children only
			// acquire the shared index, whose approved pool_wait exception remains in force.
			svc.SetForges(m2Builder{&linkedTaskForge{issue: forge.Issue{IID: iid}}})
			if _, err := svc.CreateTaskRun(env.ctx, user, repo, "task", "", false, false, false, false, &iid); !errors.Is(err, ErrActiveRunExists) {
				t.Fatalf("held precheck=%v", err)
			}
			run, err := createChild()
			if err != nil {
				t.Fatal(err)
			}
			if run.ContinuedIssueIid != original.ContinuedIssueIid || run.IssueIid.Valid {
				t.Fatal("derived link lost")
			}
			env.exec("UPDATE runs SET status='cancelled' WHERE id=$1 OR id=$2", interposed, run.ID)
		})
	}
}

func TestLinkedTaskPoolPromotionConflictLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	o := seedReevalOwner(t, env, BindModeAuto, true)
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool)
	held := seedTaskRun(t, env, o.userID, o.repoID, "uzi/task/"+uuid.NewString(), "claude", "pool_wait", false, false)
	env.exec("UPDATE runs SET continued_issue_iid=71 WHERE id=$1", held)
	winner := uuid.New()
	env.exec("INSERT INTO runs (id,user_id,repo_id,kind,issue_iid,issue_title,issue_description) VALUES ($1,$2,$3,'issue',71,'winner','')", winner, o.userID, o.repoID)
	// The sweep spans all owners; an independent eligible run must still resume.
	other := seedReevalOwner(t, env, BindModeAuto, true)
	control := seedTaskRun(t, env, other.userID, other.repoID, "uzi/task/"+uuid.NewString(), "claude", "pool_wait", false, false)
	before := mustRun(t, env, held)
	bc := &parkBroadcaster{}
	svc.SetBroadcaster(bc)
	n, err := svc.resumePoolWaitRuns(env.ctx)
	if err != nil || statusOf(t, env, held) != "pool_wait" || bc.sawState(held, "queued") {
		t.Fatalf("sweep conflict n=%d err=%v", n, err)
	}
	if statusOf(t, env, control) != "queued" || !bc.sawState(control, "queued") {
		t.Fatal("independent owner did not resume and broadcast queued")
	}
	queued := make(map[uuid.UUID]bool)
	for _, state := range bc.states {
		if state.status != "queued" {
			continue
		}
		if state.id == held || queued[state.id] || statusOf(t, env, state.id) != "queued" {
			t.Fatalf("invalid queued broadcast for run %s", state.id)
		}
		queued[state.id] = true
	}
	if n != int64(len(queued)) {
		t.Fatalf("sweep resumed %d runs but broadcast %d distinct queued runs", n, len(queued))
	}
	_, err = svc.SetRunCredential(env.ctx, o.userID, held, CredentialOverrideModePinned, &o.altTok)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != "uq_runs_one_active_per_issue" {
		t.Fatalf("credential promote conflict=%v", err)
	}
	after := mustRun(t, env, held)
	if after.Status != "pool_wait" || after.CredentialOverrideMode != before.CredentialOverrideMode || after.CredentialOverrideSecretID != before.CredentialOverrideSecretID || after.StatusSince != before.StatusSince {
		t.Fatal("failed credential promotion did not roll back")
	}
	if bc.sawState(held, "queued") {
		t.Fatal("failed credential promotion broadcast queued")
	}
	var audits int
	if err := env.pool.QueryRow(env.ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='resume'", held).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 0 {
		t.Fatal("failed promotion wrote a resume audit")
	}
}
