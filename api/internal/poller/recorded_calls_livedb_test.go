package poller

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Recorded-call test (PRD 4173 M4): for an enabled repo with no issue-tracker
// link, pin every forge call a poller tick makes, for a full reconcile tick and
// then an incremental tick carrying the marks the first one produced. The repo
// is seeded so every forge-touching step of syncRepo has work to do and so
// removing any of them changes the golden:
//
//   - issue 7: an open card in Human Review whose run (MR 13) is recorded
//     opened while the forge reports it closed, so SyncMRStates reads the MR
//     and moves the card (EnsureLabels, UpdateIssueLabels);
//   - a prompt run with MR 21 and no issue, read by SyncBoardFreeMRStates;
//   - issue 8: a run whose merged MR 31 declared a moved PRD, so
//     SyncPRDLinkPatches reads the live description and rewrites it;
//   - a queued ci_fix bound to MR 51 (ci_fix_mr_iid only), read by
//     SyncActiveMRTargets, which hands the verified merged MR to a recording
//     canceller;
//   - issue 5: carries the autopilot label, so the autopilot detector reads its
//     label events and captures its issue input. The detector is the real
//     poller.Autopilot over the real store; only the run starter is a fake,
//     because the run it creates makes no forge call of its own.
//
// The CI-autofix, MR-review watch and project reverse-sync detectors stay
// unwired, as in the other poller live-DB tests; the pipeline watch is on. The
// golden was captured before the tracker-link rerouting; regenerate only
// deliberately, with UZI_UPDATE_GOLDEN=1.

// recordedPollForge answers the reads and accepts the writes the seeded repo
// makes. The MR states are keyed by MR iid: 13 closed, 21 opened, 31 merged.
type recordedPollForge struct {
	forgetest.BaseFake
	// getProjectErr is what GetProject returns (nil: the project reads fine), the linked
	// repo's code-host probe.
	getProjectErr error
	// ciBranch is the source branch the forge reports for MR 51 (the seeded ci_fix's ref).
	ciBranch string
}

func (f *recordedPollForge) GetProject(context.Context, int64) (forge.Project, error) {
	return forge.Project{}, f.getProjectErr
}

var recordedPollAt = time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)

func (*recordedPollForge) ListIssues(_ context.Context, _ int64, opts forge.ListIssuesOptions) ([]forge.Issue, error) {
	if len(opts.Labels) == 1 && opts.Labels[0] == "agent-found" {
		// One agent-found issue so the finding mark is non-zero and tick 2's
		// incremental finding fetch carries its UpdatedAfter bound.
		return []forge.Issue{{IID: 11, Title: "finding", State: "opened", Labels: []string{"agent-found"}, UpdatedAt: recordedPollAt}}, nil
	}
	return []forge.Issue{
		{IID: 5, Title: "autopilot", State: "opened", Labels: []string{"uzi", "autopilot"}, UpdatedAt: recordedPollAt},
		{IID: 7, Title: "card", State: "opened", Labels: []string{"uzi", "Human Review"}, UpdatedAt: recordedPollAt},
	}, nil
}
func (*recordedPollForge) GetMergeRequest(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
	state := map[int64]string{13: forge.MRStateClosed, 21: forge.MRStateOpened, 31: forge.MRStateMerged}[iid]
	return forge.MergeRequest{IID: iid, State: state}, nil
}

// recordedPollCIBranch is the source branch of MR 51, the target of the seeded ci_fix.
func recordedPollCIBranch(repo uuid.UUID) string { return "agent/ci-fix-" + repo.String() }

func (f *recordedPollForge) GetMergeRequestSummary(_ context.Context, _, iid int64) (forge.MergeRequestSummary, error) {
	if iid != 51 {
		return forge.MergeRequestSummary{}, forge.ErrMergeRequestNotFound
	}
	return forge.MergeRequestSummary{
		IID: 51, State: forge.MRStateMerged, SourceBranch: f.ciBranch,
		HeadSHA: "0123456789abcdef0123456789abcdef01234567",
	}, nil
}

// recordedTerminalCanceller records the observations SyncActiveMRTargets hands it; the real
// fenced cancel is covered by the forgesvc live-DB tests.
type recordedTerminalCanceller struct {
	obs []workersvc.TerminalTargetObservation
}

func (c *recordedTerminalCanceller) CancelRunForTerminalTarget(_ context.Context, _ uuid.UUID, obs workersvc.TerminalTargetObservation) error {
	c.obs = append(c.obs, obs)
	return nil
}

func (*recordedPollForge) GetIssue(_ context.Context, _, iid int64) (forge.Issue, error) {
	desc := "see the issue"
	if iid == 8 {
		desc = "Implements prds/72-x.md end to end."
	}
	return forge.Issue{IID: iid, Title: "t", State: "opened", Description: desc, Labels: []string{"uzi", "autopilot"}, AuthorForgeUserID: 2, UpdatedAt: recordedPollAt}, nil
}
func (*recordedPollForge) UpdateIssueDescription(context.Context, int64, int64, string) error {
	return nil
}
func (*recordedPollForge) EnsureLabels(context.Context, int64, []forge.Label) error { return nil }
func (*recordedPollForge) UpdateIssueLabels(context.Context, int64, int64, []string, []string) error {
	return nil
}
func (*recordedPollForge) ListIssueLabelEvents(context.Context, int64, int64) ([]forge.LabelEvent, error) {
	return []forge.LabelEvent{{ID: 100, Action: "add", LabelName: "autopilot", Username: "alice"}}, nil
}
func (*recordedPollForge) ListIssueComments(context.Context, int64, int64) ([]forge.IssueComment, error) {
	return nil, nil
}
func (*recordedPollForge) RepositoryAuthorEligibility(context.Context, int64, int64) (forge.AuthorEligibility, error) {
	return forge.AuthorEligible, nil
}

func TestRecordedCallsPollerTickLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL unset; run e2e/run-store-it.sh")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)
	box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	pollForge := &recordedPollForge{}
	rec := forgetest.NewRecorder(pollForge)
	svc := forgesvc.NewWithForgeBuilder(q, box, time.Second, nil, func(forge.Type, string, string, time.Duration) (forge.Forge, error) {
		return rec, nil
	})
	sealed, err := svc.EncryptToken("fixture")
	if err != nil {
		t.Fatal(err)
	}
	user, conn, repo := uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("INSERT INTO users(id,email,password_hash,autopilot_enabled) VALUES($1,$2,'x',true)", user, fmt.Sprintf("%s@recorded.test", user))
	exec("INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,human_username,token_ciphertext) VALUES($1,$2,'gitlab',$3,'bot',1,'alice',$4)", conn, user, "https://"+uuid.NewString()+".forge.test", sealed)
	exec("INSERT INTO user_secrets(user_id,kind,label,is_default,ciphertext,sealed_with) VALUES($1,'anthropic_token','default',true,$2,'master')", user, []byte{2})
	exec("INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled) VALUES($1,$2,42,$3,'https://forge.test/g/r','main',true)", repo, conn, repo.String())
	pollForge.ciBranch = recordedPollCIBranch(repo)
	exec("INSERT INTO board_columns(repo_id,label_name,position) VALUES($1,'In Progress',0),($1,'Human Review',1)", repo)
	const runCols = "INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,branch,mr_iid,mr_state,prd_done_path,status,created_at) "
	// Board lane: issue 7's card is in Human Review, MR 13 recorded opened.
	exec(runCols+"VALUES($1,$2,$3,'issue',7,'t','d',$4,13,'opened',NULL,'completed','2020-01-01')", uuid.New(), user, repo, "agent/issue-7-"+repo.String())
	// Board-free lane: an issue-less prompt run whose MR 21 was never observed.
	exec(runCols+"VALUES($1,$2,$3,'prompt',NULL,'t','d',$4,21,NULL,NULL,'completed','2020-01-01')", uuid.New(), user, repo, "uzi/prompt-"+repo.String())
	// PRD link patch: issue 8's run merged MR 31 after moving its PRD.
	exec(runCols+"VALUES($1,$2,$3,'issue',8,'t','Implements prds/72-x.md end to end.',$4,31,'merged','prds/done/72-x.md','completed','2020-01-01')", uuid.New(), user, repo, "agent/issue-8-"+repo.String())
	// Active MR target: a queued ci_fix bound to MR 51 (no mr_iid, no issue, no watch row).
	ciFix := uuid.New()
	exec("INSERT INTO runs(id,user_id,repo_id,kind,pipeline_id,pipeline_ref,ci_fix_mr_iid,status,issue_title,issue_description) VALUES($1,$2,$3,'ci_fix',99,$4,51,'queued','t','d')", ciFix, user, repo, recordedPollCIBranch(repo))

	// reconcileEvery 3: tick 1 is the first poll (full reconcile), tick 2 is
	// incremental.
	engine := New(svc, mergedRepoStore{q, repo}, time.Minute, 3)
	engine.SetPipelineWatch(time.Hour, 5)
	starter := &apRuns{}
	engine.SetAutopilot(NewAutopilot(q, starter, nil))
	canceller := &recordedTerminalCanceller{}
	svc.SetTerminalTargetCanceller(canceller)

	var lines []string
	for i, label := range []string{"tick 1 (full reconcile)", "tick 2 (incremental)"} {
		before := len(rec.Calls())
		engine.PollOnce(ctx)
		calls := rec.Calls()[before:]
		if len(calls) == 0 {
			t.Fatalf("tick %d made no forge call", i+1)
		}
		lines = append(lines, "# "+label)
		lines = append(lines, calls...)
	}
	if len(starter.calls) != 1 || starter.calls[0].iid != 5 {
		t.Fatalf("autopilot run starter calls = %+v, want exactly one, for issue 5", starter.calls)
	}
	// One verified merged observation per tick for the bound ci_fix.
	if len(canceller.obs) != 2 || canceller.obs[0].RunID != ciFix || canceller.obs[0].TargetMRIID != 51 || canceller.obs[0].State != forge.MRStateMerged {
		t.Fatalf("terminal-target canceller observations = %+v, want two merged observations of ci_fix %s on MR 51", canceller.obs, ciFix)
	}
	forgetest.AssertGolden(t, "recorded_calls_poller_tick.golden", lines)
}
