package schedsvc

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
	"github.com/vtmocanu/uzi/api/internal/issuetrackersvc"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4173 M5: remove-label-on-dispatch on a repo linked to YouTrack compares the sweep's
// selector with the stored eligibility name, not the configured uzi label, so the eligibility
// tag is never stripped, and any other selector tag is removed as on a forge.
// linkScheduleRepo links repo to a fresh YouTrack fake and returns the fake, the real
// resolver the scheduler reaches the linked issue side through, and the cache wrapper it is
// built over.
func linkScheduleRepo(t *testing.T, ctx context.Context, pool *pgxpool.Pool, q *store.Queries, box *secretbox.Box, user, repo uuid.UUID) (*youtracktest.Server, *forgesvc.Service, removalCache) {
	t.Helper()
	srv := youtracktest.NewServer(t)
	def, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatal("http.DefaultTransport is not an *http.Transport")
	}
	saved := def.TLSClientConfig
	def.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	t.Cleanup(func() { def.TLSClientConfig = saved })

	cache := removalCache{Queries: q}
	svc := forgesvc.New(cache, box, time.Second, nil)
	itSvc := issuetrackersvc.New(q, box, func(u string) bool { return u == srv.URL }, issuetracker.New)
	svc.SetIssueTrackerDrivers(itSvc)
	yt, err := itSvc.Connect(ctx, user, "youtrack", srv.URL, srv.Token)
	if err != nil {
		t.Fatal(err)
	}
	capacityExec(t, ctx, pool, `UPDATE repos SET issue_tracker_connection_id=$2, issue_tracker_project_id=$3, issue_tracker_project_short=$4,
        eligibility_tag_id=$5, eligibility_tag_name=$6, issue_tracker_linked_at=now() WHERE id=$1`,
		repo, yt.ID, youtracktest.ProjectID, youtracktest.ProjectShortName, youtracktest.EligibilityTagID, youtracktest.EligibilityTagName)
	return srv, svc, cache
}

// PRD #4173 M5: the self-improvement tracking issue is created in the linked project through
// the repo's issue side, carries only the tracking label (no eligibility tag), and is reused
// by the next cycle.
func TestLinkedSelfImproveTrackingIssueIsCreatedInTheLinkedProjectLiveDB(t *testing.T) {
	ctx, pool, q, box := openScheduleFireLiveDB(t)
	user, repo := scheduleFireUser(ctx, t, pool)
	srv, svc, _ := linkScheduleRepo(t, ctx, pool, q, box, user, repo)
	tr, err := svc.IssueTrackerForRepo(ctx, user, repo)
	if err != nil {
		t.Fatal(err)
	}
	iid, err := ensureTrackingIssue(ctx, tr, 1)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, id := range srv.IssueTagIDs(iid) {
		names = append(names, srv.TagByID(id).Name)
	}
	if !slices.Equal(names, []string{SelfImproveTrackingLabel}) {
		t.Fatalf("tracking issue tags = %v, want only %q (no eligibility tag)", names, SelfImproveTrackingLabel)
	}
	again, err := ensureTrackingIssue(ctx, tr, 1)
	if err != nil || again != iid {
		t.Fatalf("the next cycle created issue %d (%v), want to reuse %d", again, err, iid)
	}
}

func TestLinkedSweepRemovalKeepsTheEligibilityTagLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name        string
		selector    string
		wantRemoved bool
		wantTags    []string
	}{
		{"selector equal to the eligibility name is kept", youtracktest.EligibilityTagName, false, []string{youtracktest.EligibilityTagName, "triage"}},
		{"another selector tag is removed", "triage", true, []string{youtracktest.EligibilityTagName}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, pool, q, box := openScheduleFireLiveDB(t)
			user, repo := scheduleFireUser(ctx, t, pool)

			srv, svc, _ := linkScheduleRepo(t, ctx, pool, q, box, user, repo)

			triage := srv.SeedOwnTag("triage")
			n := srv.SeedIssue("candidate", "body", youtracktest.EligibilityTagID, triage)
			capacityExec(t, ctx, pool, `INSERT INTO issues (repo_id,forge_issue_iid,title,state,labels,assignee_ids,web_url,forge_updated_at,synced_at)
	        VALUES ($1,$2,'candidate','opened',$3::jsonb,'[]','https://yt.test/i',now(),now())`, repo, n, `["`+youtracktest.EligibilityTagName+`","triage"]`)

			sc := capacityLiveSchedule(t, ctx, pool, q, user, repo, 4, 2, pgtype.Int4{Int32: 1, Valid: true})
			capacityExec(t, ctx, pool, `UPDATE run_schedules SET labels=$2::jsonb,remove_label_on_dispatch=true,capacity_limit=NULL,capacity_room_needed=NULL WHERE id=$1`,
				sc.ID, `["`+tc.selector+`"]`)
			sc, err := q.GetRunSchedule(ctx, sc.ID)
			if err != nil {
				t.Fatal(err)
			}

			st := &removalLiveStore{capacityLiveStore: &capacityLiveStore{Queries: q, scheduleID: sc.ID}}
			runs := &removalRuns{pool: pool}
			settings := &removalSettings{fakeSettings: fakeSettings{uziLabel: "uzi"}}
			e := New(st, runs, removalBuilder{Service: svc, f: &removalForge{}}, settings, nil, nil, time.Minute, nil)
			e.SetIssueSources(svc)
			srv.ResetRequests()

			out, err := e.RunNow(ctx, sc)
			if err != nil {
				t.Fatal(err)
			}
			if len(out.Started) != 1 {
				t.Fatalf("out = %+v, want one started run", out)
			}
			started := out.Started[0]
			if started.LabelRemoved != tc.wantRemoved || started.LabelRemoveFailed || started.SelectorLabel != tc.selector {
				t.Fatalf("started = %+v, want LabelRemoved=%v without a failure", started, tc.wantRemoved)
			}
			var names []string
			for _, id := range srv.IssueTagIDs(n) {
				names = append(names, srv.TagByID(id).Name)
			}
			slices.Sort(names)
			slices.Sort(tc.wantTags)
			if !slices.Equal(names, tc.wantTags) {
				t.Fatalf("tags on the issue = %v, want %v", names, tc.wantTags)
			}
			if !slices.Contains(srv.IssueTagIDs(n), youtracktest.EligibilityTagID) {
				t.Fatal("the eligibility tag was stripped")
			}
			cached, err := q.GetIssueByIID(ctx, store.GetIssueByIIDParams{RepoID: repo, ForgeIssueIid: n})
			if err != nil {
				t.Fatal(err)
			}
			if got := scheduleRemovalLabels(t, cached.Labels); !slices.Contains(got, youtracktest.EligibilityTagName) {
				t.Fatalf("cached labels = %v, want the eligibility name kept", got)
			}
		})
	}
}
