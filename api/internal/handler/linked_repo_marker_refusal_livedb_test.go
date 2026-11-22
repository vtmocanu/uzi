package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4173 M5 review: a bookkeeping marker uzi adds on its own must never confer the link's
// eligibility tag. When the finding marker has the eligibility name and nobody opted in, the
// filing is refused with a 409 before the claim and before any issue tracker write; an
// explicit opt-in (send-to-uzi, or an owner-chosen eligibility label) still files, and an
// ordinary filing still carries its marker without the tag.

// setFindingMarker swaps the handler's settings so the finding marker is name, as a settings
// change made after the repo was linked would.
func setFindingMarker(f *neverEnabledFixture, name string) {
	f.h.settings = settings.New(&settingsStore{rows: []store.AppSetting{
		{Key: settings.KeyFindingLabel, Value: name},
		{Key: settings.KeyUziLabel, Value: "uzi"},
	}}, time.Minute)
}

// wantNoTrackerWrite fails when the YouTrack fake saw any request other than a read.
func wantNoTrackerWrite(t *testing.T, srv *youtracktest.Server) {
	t.Helper()
	for _, r := range srv.Requests() {
		if r.Method != http.MethodGet {
			t.Errorf("an issue tracker write reached the fake: %s %s", r.Method, r.Path)
		}
	}
}

func (f *neverEnabledFixture) dispositionIsOpenAndUnclaimed(t *testing.T) {
	t.Helper()
	var status string
	var claimed bool
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status, group_operation_id IS NOT NULL FROM finding_dispositions WHERE id = $1`, f.dispID).Scan(&status, &claimed); err != nil {
		t.Fatal(err)
	}
	if status != "open" || claimed {
		t.Errorf("disposition status = %q claimed = %v, want open and unclaimed", status, claimed)
	}
}

func TestLinkedRepoFindingMarkerEqualToTheEligibilityNameIsRefusedLiveDB(t *testing.T) {
	// setting changed after the link: the finding marker becomes the stored eligibility name.
	t.Run("FileFinding", func(t *testing.T) {
		f := seedNeverEnabledFixture(t)
		srv := linkNeverEnabledFixture(t, f)
		setFindingMarker(f, youtracktest.EligibilityTagName)
		rr := httptest.NewRecorder()
		f.h.FileFinding(rr, f.request(http.MethodPost, apitypes.FileFindingRequest{}, map[string]string{"id": f.findID.String()}))
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), youtracktest.EligibilityTagName) {
			t.Fatalf("status = %d body = %s, want 409 naming the label", rr.Code, rr.Body.String())
		}
		wantNoTrackerWrite(t, srv)
		f.dispositionIsOpenAndUnclaimed(t)
	})
	// the stored name equals the default marker at link time.
	t.Run("FileFinding with a marker that was the stored name from the start", func(t *testing.T) {
		f := seedNeverEnabledFixture(t)
		srv := linkNeverEnabledFixture(t, f)
		mustExecT(context.Background(), t, f.pool, `UPDATE repos SET eligibility_tag_name = 'agent-found' WHERE id = $1`, f.repoID)
		rr := httptest.NewRecorder()
		f.h.FileFinding(rr, f.request(http.MethodPost, apitypes.FileFindingRequest{}, map[string]string{"id": f.findID.String()}))
		if rr.Code != http.StatusConflict {
			t.Fatalf("status = %d body = %s, want 409", rr.Code, rr.Body.String())
		}
		wantNoTrackerWrite(t, srv)
		f.dispositionIsOpenAndUnclaimed(t)
	})
	t.Run("FileFindingGroup", func(t *testing.T) {
		f := seedNeverEnabledFixture(t)
		srv := linkNeverEnabledFixture(t, f)
		setFindingMarker(f, youtracktest.EligibilityTagName)
		rr := httptest.NewRecorder()
		f.h.FileFindingGroup(rr, f.request(http.MethodPost, apitypes.FindingGroupFileRequest{IDs: []string{f.dispID.String()}}, nil))
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), youtracktest.EligibilityTagName) {
			t.Fatalf("status = %d body = %s, want 409 naming the label", rr.Code, rr.Body.String())
		}
		wantNoTrackerWrite(t, srv)
		f.dispositionIsOpenAndUnclaimed(t)
	})
}

// With a colliding marker the explicit opt-ins still file: the issue carries the eligibility
// tag once, on purpose.
func TestLinkedRepoCollidingMarkerStillFilesOnAnExplicitOptInLiveDB(t *testing.T) {
	cases := []struct {
		name string
		req  apitypes.FileFindingRequest
	}{
		{"send to uzi", apitypes.FileFindingRequest{SendToUzi: true}},
		{"owner-chosen eligibility label", apitypes.FileFindingRequest{Labels: []string{youtracktest.EligibilityTagName}}},
		{"owner-chosen uzi label", apitypes.FileFindingRequest{Labels: []string{"uzi"}}},
	}
	for _, tc := range cases {
		t.Run("FileFinding/"+tc.name, func(t *testing.T) {
			f := seedNeverEnabledFixture(t)
			srv := linkNeverEnabledFixture(t, f)
			setFindingMarker(f, youtracktest.EligibilityTagName)
			rr := httptest.NewRecorder()
			f.h.FileFinding(rr, f.request(http.MethodPost, tc.req, map[string]string{"id": f.findID.String()}))
			if rr.Code != http.StatusCreated {
				t.Fatalf("status = %d body = %s, want 201", rr.Code, rr.Body.String())
			}
			wantTags(t, srv, createdIID(t, rr.Body.Bytes()), true, youtracktest.EligibilityTagName)
		})
	}
	t.Run("FileFindingGroup/send to uzi", func(t *testing.T) {
		f := seedNeverEnabledFixture(t)
		srv := linkNeverEnabledFixture(t, f)
		setFindingMarker(f, youtracktest.EligibilityTagName)
		rr := httptest.NewRecorder()
		f.h.FileFindingGroup(rr, f.request(http.MethodPost, apitypes.FindingGroupFileRequest{IDs: []string{f.dispID.String()}, SendToUzi: true}, nil))
		if rr.Code != http.StatusCreated {
			t.Fatalf("status = %d body = %s, want 201", rr.Code, rr.Body.String())
		}
		wantTags(t, srv, createdIID(t, rr.Body.Bytes()), true, youtracktest.EligibilityTagName)
	})
}

// Control: a marker that is not the eligibility name files as before, with the marker and
// without the eligibility tag (also covered by TestLinkedRepoFindingFilingCreatesInTheLinkedProjectLiveDB).
func TestLinkedRepoOrdinaryFindingFilingIsUnchangedByTheMarkerCheckLiveDB(t *testing.T) {
	f := seedNeverEnabledFixture(t)
	srv := linkNeverEnabledFixture(t, f)
	setFindingMarker(f, "reported")
	rr := httptest.NewRecorder()
	f.h.FileFinding(rr, f.request(http.MethodPost, apitypes.FileFindingRequest{}, map[string]string{"id": f.findID.String()}))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s, want 201", rr.Code, rr.Body.String())
	}
	wantTags(t, srv, createdIID(t, rr.Body.Bytes()), false, "reported")
}

// changingSettingsStore serves first to the first read and then to every later one, as a
// settings update that lands while a request is in flight would. With a zero TTL every
// read reloads, so two reads in one request can see two different, each valid, settings.
type changingSettingsStore struct {
	mu          sync.Mutex
	reads       int
	first, then []store.AppSetting
}

func (s *changingSettingsStore) ListAppSettings(context.Context) ([]store.AppSetting, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.reads == 1 {
		return s.first, nil
	}
	return s.then, nil
}

// setChangingLabels makes the finding marker and the uzi label swap between two reads:
// first the marker is "agent-found" and the uzi label "uzi", then the marker is "reported"
// and the uzi label "agent-found". Each of the two settings is valid on its own.
func setChangingLabels(f *neverEnabledFixture) {
	f.h.settings = settings.New(&changingSettingsStore{
		first: []store.AppSetting{{Key: settings.KeyFindingLabel, Value: "agent-found"}, {Key: settings.KeyUziLabel, Value: "uzi"}},
		then:  []store.AppSetting{{Key: settings.KeyFindingLabel, Value: "reported"}, {Key: settings.KeyUziLabel, Value: "agent-found"}},
	}, 0)
}

// wantFiledWithoutEligibility checks a 201 whose issue carries exactly one marker, from one
// of the two settings, and not the eligibility tag.
func wantFiledWithoutEligibility(t *testing.T, srv *youtracktest.Server, rr *httptest.ResponseRecorder) {
	t.Helper()
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s, want 201", rr.Code, rr.Body.String())
	}
	ids, names := issueTags(srv, createdIID(t, rr.Body.Bytes()))
	if slices.Contains(ids, youtracktest.EligibilityTagID) {
		t.Errorf("the issue carries the eligibility tag with no opt-in (tags %v)", names)
	}
	if !slices.Equal(names, []string{"agent-found"}) && !slices.Equal(names, []string{"reported"}) {
		t.Errorf("tag names = %v, want exactly one finding marker", names)
	}
}

// PRD #4173 M5 review: the marker and the run labels are read from one settings snapshot.
// A settings change between two reads must not turn the old marker into the new uzi label,
// which the linked label set would rewrite to the eligibility tag.
func TestLinkedRepoFindingLabelsComeFromOneSettingsSnapshotLiveDB(t *testing.T) {
	t.Run("FileFinding", func(t *testing.T) {
		f := seedNeverEnabledFixture(t)
		srv := linkNeverEnabledFixture(t, f)
		setChangingLabels(f)
		rr := httptest.NewRecorder()
		f.h.FileFinding(rr, f.request(http.MethodPost, apitypes.FileFindingRequest{}, map[string]string{"id": f.findID.String()}))
		wantFiledWithoutEligibility(t, srv, rr)
	})
	t.Run("FileFindingGroup", func(t *testing.T) {
		f := seedNeverEnabledFixture(t)
		srv := linkNeverEnabledFixture(t, f)
		setChangingLabels(f)
		rr := httptest.NewRecorder()
		f.h.FileFindingGroup(rr, f.request(http.MethodPost, apitypes.FindingGroupFileRequest{IDs: []string{f.dispID.String()}}, nil))
		wantFiledWithoutEligibility(t, srv, rr)
	})
}

// PRD #4173 M5 review: a finding marker that is also the autopilot label cannot be written
// on a linked repo, because the linked label set drops the autopilot label. The filing is
// refused before any write instead of creating an issue without its marker, and an
// eligibility opt-in does not waive the marker.
func TestLinkedRepoFindingMarkerThatCannotBeWrittenIsRefusedLiveDB(t *testing.T) {
	autopilotMarker := func(f *neverEnabledFixture) {
		f.h.settings = settings.New(&settingsStore{rows: []store.AppSetting{
			{Key: settings.KeyFindingLabel, Value: "autopilot"},
			{Key: settings.KeyUziLabel, Value: "uzi"},
			{Key: settings.KeyAutopilotLabel, Value: "autopilot"},
		}}, time.Minute)
	}
	for _, sendToUzi := range []bool{false, true} {
		name := "without an opt-in"
		if sendToUzi {
			name = "with send to uzi"
		}
		t.Run("FileFinding/"+name, func(t *testing.T) {
			f := seedNeverEnabledFixture(t)
			srv := linkNeverEnabledFixture(t, f)
			autopilotMarker(f)
			rr := httptest.NewRecorder()
			f.h.FileFinding(rr, f.request(http.MethodPost, apitypes.FileFindingRequest{SendToUzi: sendToUzi}, map[string]string{"id": f.findID.String()}))
			if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "autopilot") {
				t.Fatalf("status = %d body = %s, want 409 naming the label", rr.Code, rr.Body.String())
			}
			wantNoTrackerWrite(t, srv)
			f.dispositionIsOpenAndUnclaimed(t)
		})
		t.Run("FileFindingGroup/"+name, func(t *testing.T) {
			f := seedNeverEnabledFixture(t)
			srv := linkNeverEnabledFixture(t, f)
			autopilotMarker(f)
			rr := httptest.NewRecorder()
			f.h.FileFindingGroup(rr, f.request(http.MethodPost, apitypes.FindingGroupFileRequest{IDs: []string{f.dispID.String()}, SendToUzi: sendToUzi}, nil))
			if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "autopilot") {
				t.Fatalf("status = %d body = %s, want 409 naming the label", rr.Code, rr.Body.String())
			}
			wantNoTrackerWrite(t, srv)
			f.dispositionIsOpenAndUnclaimed(t)
		})
	}
}

// PRD #4173 M5 review: when the label settings cannot be read, the accessor returns the
// default names next to the error, and those cannot say which label the owner meant as a
// run opt-in or which marker is required. Here the saved uzi label is "start", so the
// owner's "uzi" is an ordinary label; with the defaults it would be rewritten to the
// eligibility name and read as an opt-in. The single route answers 500 before the claim
// and before any write, as the group route does.
func TestFileFindingFailsClosedWhenTheLabelSettingsCannotBeReadLiveDB(t *testing.T) {
	unreadable := func(f *neverEnabledFixture) {
		f.h.settings = settings.New(&settingsStore{
			rows: []store.AppSetting{
				{Key: settings.KeyFindingLabel, Value: "reported"},
				{Key: settings.KeyUziLabel, Value: "start"},
			},
			err: errors.New("settings store down"),
		}, time.Minute)
	}
	t.Run("FileFinding", func(t *testing.T) {
		f := seedNeverEnabledFixture(t)
		srv := linkNeverEnabledFixture(t, f)
		unreadable(f)
		rr := httptest.NewRecorder()
		f.h.FileFinding(rr, f.request(http.MethodPost, apitypes.FileFindingRequest{Labels: []string{"uzi"}}, map[string]string{"id": f.findID.String()}))
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d body = %s, want 500", rr.Code, rr.Body.String())
		}
		wantNoTrackerWrite(t, srv)
		f.dispositionIsOpenAndUnclaimed(t)
	})
	// Control: the group route already refused this before any write.
	t.Run("FileFindingGroup", func(t *testing.T) {
		f := seedNeverEnabledFixture(t)
		srv := linkNeverEnabledFixture(t, f)
		unreadable(f)
		rr := httptest.NewRecorder()
		f.h.FileFindingGroup(rr, f.request(http.MethodPost, apitypes.FindingGroupFileRequest{IDs: []string{f.dispID.String()}, Labels: []string{"uzi"}}, nil))
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d body = %s, want 500", rr.Code, rr.Body.String())
		}
		wantNoTrackerWrite(t, srv)
		f.dispositionIsOpenAndUnclaimed(t)
	})
}

// PRD #4173 M5 review (Greptile on 9bcd89761 and 5c8212113): the other routes that create
// an issue (the board's new issue, a run-review recommendation, an admin judge
// recommendation) read the uzi label, and on a linked repo the autopilot label too, to
// build the set. When those settings cannot be read the defaults would stand in for the
// saved names, so on a linked and on an unlinked repo each route answers 500 before any
// write and leaves no filing claim behind.
func TestLinkedRepoIssueCreationFailsClosedWhenTheLabelSettingsCannotBeReadLiveDB(t *testing.T) {
	unreadable := func(f *neverEnabledFixture) {
		f.h.settings = settings.New(&settingsStore{
			rows: []store.AppSetting{{Key: settings.KeyUziLabel, Value: "start"}},
			err:  errors.New("settings store down"),
		}, time.Minute)
	}
	claims := func(t *testing.T, f *neverEnabledFixture) int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM recommendation_filed_issues WHERE filed_by_user_id = $1`, f.admin.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	cases := []struct {
		name string
		call func(f *neverEnabledFixture, w http.ResponseWriter, repo uuid.UUID)
	}{
		{"CreateIssue", func(f *neverEnabledFixture, w http.ResponseWriter, repo uuid.UUID) {
			f.h.CreateIssue(w, f.request(http.MethodPost, map[string]string{"title": "t", "description": "d"}, map[string]string{"id": repo.String()}))
		}},
		{"FileIssue", func(f *neverEnabledFixture, w http.ResponseWriter, repo uuid.UUID) {
			f.h.FileIssue(w, f.request(http.MethodPost, map[string]string{"repo_id": repo.String(), "title": "t", "description": "d"},
				map[string]string{"id": f.runID.String(), "recID": f.recID.String()}))
		}},
		{"AdminFileJudgeIssue", func(f *neverEnabledFixture, w http.ResponseWriter, repo uuid.UUID) {
			f.h.AdminFileJudgeIssue(w, f.request(http.MethodPost, adminFileIssueRequest{
				Category: "improve_agent", Target: "ne-" + f.repoID.String(), RepoID: repo.String(), Title: "t", Description: "d",
			}, nil))
		}},
	}
	for _, tc := range cases {
		t.Run("linked/"+tc.name, func(t *testing.T) {
			f := seedNeverEnabledFixture(t)
			srv := linkNeverEnabledFixture(t, f)
			unreadable(f)
			rr := httptest.NewRecorder()
			tc.call(f, rr, f.repoID)
			if rr.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d body = %s, want 500", rr.Code, rr.Body.String())
			}
			wantNoTrackerWrite(t, srv)
			if n := claims(t, f); n != 0 {
				t.Errorf("%d filing claim(s) left behind, want 0", n)
			}
		})
		// An unlinked repo: the saved uzi label "start" must not be replaced by the
		// default "uzi" on the forge issue.
		t.Run("unlinked/"+tc.name, func(t *testing.T) {
			f := seedNeverEnabledFixture(t)
			unreadable(f)
			rr := httptest.NewRecorder()
			tc.call(f, rr, f.goodRepo)
			if rr.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d body = %s, want 500", rr.Code, rr.Body.String())
			}
			if n := f.hits.Load(); n != 0 {
				t.Errorf("the forge saw %d request(s), want 0", n)
			}
			if n := claims(t, f); n != 0 {
				t.Errorf("%d filing claim(s) left behind, want 0", n)
			}
		})
	}
}
