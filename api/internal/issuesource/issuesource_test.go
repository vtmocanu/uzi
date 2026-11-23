package issuesource

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
)

type stubSources struct {
	tr    forge.IssueTracker
	err   error
	calls int
}

func (s *stubSources) IssueTrackerForRepo(context.Context, uuid.UUID, uuid.UUID) (forge.IssueTracker, error) {
	s.calls++
	return s.tr, s.err
}

func TestForUnlinkedReturnsTheCallersDriverWithoutAskingSources(t *testing.T) {
	own, src := &forgetest.BaseFake{}, &stubSources{tr: &forgetest.BaseFake{}}
	got, err := For(context.Background(), src, false, uuid.New(), uuid.New(), own)
	if err != nil || got != forge.IssueTracker(own) || src.calls != 0 {
		t.Fatalf("got=%v err=%v calls=%d", got, err, src.calls)
	}
}

func TestForLinkedUsesSourcesAndFailsClosedWithout(t *testing.T) {
	own, resolved := &forgetest.BaseFake{}, &forgetest.BaseFake{}
	src := &stubSources{tr: resolved}
	got, err := For(context.Background(), src, true, uuid.New(), uuid.New(), own)
	if err != nil || got != forge.IssueTracker(resolved) {
		t.Fatalf("got=%v err=%v", got, err)
	}
	if got, err := For(context.Background(), nil, true, uuid.New(), uuid.New(), own); got != nil || !errors.Is(err, ErrNotWired) {
		t.Fatalf("no sources: got=%v err=%v, want ErrNotWired and no forge fallback", got, err)
	}
	boom := errors.New("cannot resolve")
	if _, err := For(context.Background(), &stubSources{err: boom}, true, uuid.New(), uuid.New(), own); !errors.Is(err, boom) {
		t.Fatalf("resolver error lost: %v", err)
	}
}

func TestLinkedLabels(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   []string
	}{
		{"uzi becomes the eligibility name", []string{"uzi"}, []string{"ready"}},
		{"autopilot is dropped", []string{"uzi", "autopilot"}, []string{"ready"}},
		{"order is kept", []string{"finding", "autopilot", "uzi", "extra"}, []string{"finding", "ready", "extra"}},
		{"dedupe after the mapping", []string{"uzi", "ready", "uzi"}, []string{"ready"}},
		{"no run labels", []string{"finding"}, []string{"finding"}},
		{"empty", nil, []string{}},
		{"only autopilot", []string{"autopilot"}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LinkedLabels(tc.labels, "uzi", "autopilot", "ready")
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestLinkedLabelsDoesNotMutateItsInput(t *testing.T) {
	in := []string{"uzi", "autopilot"}
	_ = LinkedLabels(in, "uzi", "autopilot", "ready")
	if in[0] != "uzi" || in[1] != "autopilot" {
		t.Fatalf("input mutated: %v", in)
	}
}

func TestRefuseEligibilityMarkers(t *testing.T) {
	cases := []struct {
		name        string
		eligibility string
		markers     []string
		refused     bool
	}{
		{"a marker equal to the eligibility name", "ready", []string{"agent-found", "ready"}, true},
		{"no marker equal", "ready", []string{"agent-found", "proposal"}, false},
		{"the match is exact", "ready", []string{"Ready", "ready "}, false},
		{"no markers", "ready", nil, false},
		{"a blank eligibility name never matches", "", []string{"", "proposal"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := RefuseEligibilityMarkers(tc.eligibility, tc.markers...)
			if got := errors.Is(err, ErrMarkerConfersEligibility); got != tc.refused {
				t.Fatalf("err = %v, refused = %v, want %v", err, got, tc.refused)
			}
			if tc.refused && !strings.Contains(err.Error(), `"ready"`) {
				t.Errorf("err = %v does not name the colliding label", err)
			}
		})
	}
}

func TestRequireMarker(t *testing.T) {
	cases := []struct {
		name    string
		labels  []string
		marker  string
		refused bool
	}{
		{"the marker is in the set", []string{"agent-found", "ready"}, "agent-found", false},
		{"the marker was dropped", []string{"ready"}, "autopilot", true},
		{"an empty set", nil, "agent-found", true},
		{"the match is exact", []string{"Agent-found"}, "agent-found", true},
		{"a blank marker is never required", nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := RequireMarker(tc.labels, tc.marker)
			if got := errors.Is(err, ErrMarkerNotWritable); got != tc.refused {
				t.Fatalf("err = %v, refused = %v, want %v", err, got, tc.refused)
			}
			if tc.refused && !strings.Contains(err.Error(), `"`+tc.marker+`"`) {
				t.Errorf("err = %v does not name the label", err)
			}
		})
	}
}
