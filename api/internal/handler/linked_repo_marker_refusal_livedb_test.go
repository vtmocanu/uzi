package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
