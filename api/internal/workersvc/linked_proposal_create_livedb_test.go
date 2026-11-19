package workersvc

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
	"github.com/vtmocanu/uzi/api/internal/issuetrackersvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4173 M5: the chat-proposal confirm and the scheduled-proposal filing create their
// issue in the YouTrack project a linked repo is bound to, never the forge, through the
// real issue tracker driver that issuetrackersvc builds. The resolver is a local stand-in
// for forgesvc.IssueTrackerForRepo (forgesvc imports workersvc), without its write guard;
// the handler LiveDB tests cover the production resolver. Skipped unless UZI_TEST_DATABASE_URL points
// at a throwaway Postgres.

type linkedProposalFix struct {
	env  codexTestEnv
	user uuid.UUID
	repo uuid.UUID
	svc  *Service
	srv  *youtracktest.Server
}

func newLinkedProposalFix(t *testing.T) *linkedProposalFix {
	t.Helper()
	env := setupCodexLiveDB(t)
	user, _, repo := env.seedCodexInfra(t)
	srv := youtracktest.NewServer(t)
	def, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatal("http.DefaultTransport is not an *http.Transport")
	}
	saved := def.TLSClientConfig
	def.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	t.Cleanup(func() { def.TLSClientConfig = saved })

	itSvc := issuetrackersvc.New(env.q, env.box, func(u string) bool { return u == srv.URL }, issuetracker.New)
	yt, err := itSvc.Connect(env.ctx, user, "youtrack", srv.URL, srv.Token)
	if err != nil {
		t.Fatal(err)
	}
	env.exec(`UPDATE repos SET issue_tracker_connection_id = $2, issue_tracker_project_id = $3, issue_tracker_project_short = $4,
	        eligibility_tag_id = $5, eligibility_tag_name = $6, issue_tracker_linked_at = now() WHERE id = $1`,
		repo, yt.ID, youtracktest.ProjectID, youtracktest.ProjectShortName, youtracktest.EligibilityTagID, youtracktest.EligibilityTagName)

	svc := New(env.q, env.box, testParams())
	// The forge only has to exist: the proposal sites build it, then create through the
	// resolver. The assertions read the YouTrack fake, where the issue must appear.
	svc.SetForges(m2Builder{&linkedTaskForge{}})
	svc.SetIssueSources(linkedSources{q: env.q, it: itSvc})
	srv.ResetRequests()
	return &linkedProposalFix{env: env, user: user, repo: repo, svc: svc, srv: srv}
}

// linkedSources is the issue source resolver of a linked repo: the driver the stored
// binding names, built by the production issuetrackersvc. forgesvc.Service, which wires
// the same call in cmd/server, cannot be imported here (forgesvc imports workersvc), and
// the issue write guard it wraps the driver in only refuses writes this test never makes.
type linkedSources struct {
	q  *store.Queries
	it *issuetrackersvc.Service
}

func (s linkedSources) IssueTrackerForRepo(ctx context.Context, userID, repoID uuid.UUID) (forge.IssueTracker, error) {
	src, err := s.q.GetRepoIssueSourceForUser(ctx, store.GetRepoIssueSourceForUserParams{ID: repoID, UserID: userID})
	if err != nil {
		return nil, err
	}
	return s.it.DriverFor(ctx, userID, uuid.UUID(src.LinkedConnectionID.Bytes), issuetracker.Binding{
		Project:        issuetracker.ProjectRef{ID: src.IssueTrackerProjectID.String, ShortName: src.IssueTrackerProjectShort.String},
		ProjectID:      src.ForgeProjectID,
		EligibilityTag: issuetracker.TagRef{ID: src.EligibilityTagID.String, Name: src.EligibilityTagName.String},
	}, issuetracker.StateMapping{})
}

func (f *linkedProposalFix) tagNames(n int64) []string {
	var names []string
	for _, id := range f.srv.IssueTagIDs(n) {
		names = append(names, f.srv.TagByID(id).Name)
	}
	slices.Sort(names)
	return names
}

func TestLinkedChatProposalConfirmCreatesInTheLinkedProjectLiveDB(t *testing.T) {
	f := newLinkedProposalFix(t)
	runID, propID := uuid.New(), uuid.New()
	f.env.exec(`INSERT INTO runs (id, user_id, issue_title, issue_description, status, kind)
	            VALUES ($1, $2, 'chat', 'd', 'completed', 'chat')`, runID, f.user)
	f.env.exec(`INSERT INTO issue_proposals (id, run_id, repo_id, title, description, labels, status)
	            VALUES ($1, $2, $3, 'Proposed card', 'proposal body', '["bug","uzi","autopilot"]', 'pending')`, propID, runID, f.repo)

	created, err := f.svc.ConfirmProposalForUser(f.env.ctx, f.user, runID, propID)
	if err != nil {
		t.Fatalf("ConfirmProposalForUser: %v", err)
	}
	if created.IID == 0 || created.Title != "Proposed card" {
		t.Fatalf("created = %+v", created)
	}
	if got, want := f.tagNames(created.IID), []string{"bug", youtracktest.EligibilityTagName}; !slices.Equal(got, want) {
		t.Errorf("tags on the created issue = %v, want %v", got, want)
	}
	if !slices.Contains(f.srv.IssueTagIDs(created.IID), youtracktest.EligibilityTagID) {
		t.Errorf("the stored eligibility tag id is not on the issue: %v", f.srv.IssueTagIDs(created.IID))
	}
	for _, stray := range []string{"uzi", "autopilot"} {
		if len(f.srv.TagsNamed(stray)) != 0 {
			t.Errorf("a %q tag was created on the issue tracker", stray)
		}
	}
	var status string
	var iid pgtype.Int8
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT status, created_issue_iid FROM issue_proposals WHERE id = $1`, propID).Scan(&status, &iid); err != nil {
		t.Fatal(err)
	}
	if status != "confirmed" || !iid.Valid || iid.Int64 != created.IID {
		t.Errorf("proposal = %s iid %+v, want confirmed with iid %d", status, iid, created.IID)
	}
}

func TestLinkedScheduledProposalFilesInTheLinkedProjectWithoutTheEligibilityTagLiveDB(t *testing.T) {
	f := newLinkedProposalFix(t)
	sched, err := f.env.q.CreateRunSchedule(f.env.ctx, store.CreateRunScheduleParams{
		UserID: f.user, RepoID: f.repo, Target: "prompt", Prompt: pgtype.Text{String: "propose", Valid: true},
		Timing: "recurring", CronExpr: pgtype.Text{String: "0 * * * *", Valid: true}, Timezone: "UTC",
		NextFireAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}, Enabled: true,
		OutputMode: pgtype.Text{String: "issues", Valid: true},
	})
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.env.pool.Exec(f.env.ctx, `DELETE FROM runs WHERE schedule_id = $1`, sched.ID)
		_, _ = f.env.pool.Exec(f.env.ctx, `DELETE FROM run_schedules WHERE id = $1`, sched.ID)
	})
	runID := uuid.New()
	f.env.exec(`INSERT INTO runs (id, user_id, repo_id, issue_title, issue_description, status, kind, schedule_id)
	            VALUES ($1, $2, $3, 'scheduled idea', 'd', 'completed', 'prompt', $4)`, runID, f.user, f.repo, sched.ID)
	run, err := f.env.q.GetRunByID(f.env.ctx, runID)
	if err != nil {
		t.Fatal(err)
	}

	f.svc.maybeFileProposal(f.env.ctx, run, &ProposalPayload{Title: "Scheduled proposal", Body: "proposal body"})

	var iid pgtype.Int8
	var status string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT status, created_issue_iid FROM issue_proposals WHERE run_id = $1`, runID).Scan(&status, &iid); err != nil {
		t.Fatal(err)
	}
	if status != "confirmed" || !iid.Valid {
		t.Fatalf("audit row = %s iid %+v, want confirmed with an iid", status, iid)
	}
	if got, want := f.tagNames(iid.Int64), []string{ProposalLabel("")}; !slices.Equal(got, want) {
		t.Errorf("tags on the filed issue = %v, want only %v", got, want)
	}
	if slices.Contains(f.srv.IssueTagIDs(iid.Int64), youtracktest.EligibilityTagID) {
		t.Error("a scheduled proposal carries the eligibility tag")
	}
}

// PRD #4173 M5 review: a scheduled proposal's label is uzi's own marker, never an opt-in.
// When it has the link's eligibility name the issue would be tagged eligible, so nothing is
// filed: no issue tracker write, no audit row. The control (a different name) still files.
func TestLinkedScheduledProposalRefusesAMarkerThatIsTheEligibilityNameLiveDB(t *testing.T) {
	cases := []struct {
		name        string
		slug        string
		eligibility string
		wantFiled   bool
	}{
		{"bare proposal label", "", "proposal", false},
		{"scoped proposal label", "tech-debt", "proposal::tech-debt", false},
		{"control, bare label with another eligibility name", "", youtracktest.EligibilityTagName, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLinkedProposalFix(t)
			f.env.exec(`UPDATE repos SET eligibility_tag_name = $2 WHERE id = $1`, f.repo, tc.eligibility)
			params := store.CreateRunScheduleParams{
				UserID: f.user, RepoID: f.repo, Target: "prompt", Prompt: pgtype.Text{String: "propose", Valid: true},
				Timing: "recurring", CronExpr: pgtype.Text{String: "0 * * * *", Valid: true}, Timezone: "UTC",
				NextFireAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}, Enabled: true,
				OutputMode: pgtype.Text{String: "issues", Valid: true},
			}
			sched, err := f.env.q.CreateRunSchedule(f.env.ctx, params)
			if err != nil {
				t.Fatalf("create schedule: %v", err)
			}
			t.Cleanup(func() {
				_, _ = f.env.pool.Exec(f.env.ctx, `DELETE FROM runs WHERE schedule_id = $1`, sched.ID)
				_, _ = f.env.pool.Exec(f.env.ctx, `DELETE FROM run_schedules WHERE id = $1`, sched.ID)
			})
			if tc.slug != "" {
				f.env.exec(`UPDATE run_schedules SET catalog_slug = $2 WHERE id = $1`, sched.ID, tc.slug)
			}
			runID := uuid.New()
			f.env.exec(`INSERT INTO runs (id, user_id, repo_id, issue_title, issue_description, status, kind, schedule_id)
			            VALUES ($1, $2, $3, 'scheduled idea', 'd', 'completed', 'prompt', $4)`, runID, f.user, f.repo, sched.ID)
			run, err := f.env.q.GetRunByID(f.env.ctx, runID)
			if err != nil {
				t.Fatal(err)
			}

			f.svc.maybeFileProposal(f.env.ctx, run, &ProposalPayload{Title: "Scheduled proposal", Body: "proposal body"})

			var rows int
			if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM issue_proposals WHERE run_id = $1`, runID).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if tc.wantFiled {
				if rows != 1 {
					t.Fatalf("audit rows = %d, want 1 for the control", rows)
				}
				return
			}
			if rows != 0 {
				t.Errorf("audit rows = %d, want 0 (refused before the audit row)", rows)
			}
			for _, r := range f.srv.Requests() {
				if r.Method != http.MethodGet {
					t.Errorf("an issue tracker write reached the fake: %s %s", r.Method, r.Path)
				}
			}
		})
	}
}
