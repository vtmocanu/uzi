package issuetracker

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
)

// countingTripper counts the requests that reach the transport and fails each
// one, so a call that gets past a guard shows up as a count.
type countingTripper struct{ n atomic.Int64 }

func (c *countingTripper) RoundTrip(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return nil, errors.New("transport reached")
}

// TestProjectGuardRefusesAMismatchedProjectBeforeAnyRequest is PRD #4173 D10:
// every forge.IssueTracker method answers only for the project id the driver is
// bound to. A different id fails with ErrProjectMismatch and sends no request,
// where before the argument was ignored.
func TestProjectGuardRefusesAMismatchedProjectBeforeAnyRequest(t *testing.T) {
	for _, pid := range []int64{0, testProjectID + 1, -testProjectID, 4242} {
		rt := &countingTripper{}
		y, err := newYouTrackOver(Options{
			BaseURL: "https://yt.example.com", Token: credential(),
			Project: ProjectRef{ID: "0-1", ShortName: "UZI"}, ProjectID: testProjectID, EligibilityTag: fakeEligibility,
			OpenState: "Open", ClosedState: "Fixed",
		}, rt)
		if err != nil {
			t.Fatal(err)
		}
		calls := everyMethodWith(y, pid)
		if len(calls) != 11 {
			t.Fatalf("table covers %d methods, want 11", len(calls))
		}
		for name, call := range calls {
			if err := call(); !errors.Is(err, ErrProjectMismatch) {
				t.Errorf("%s with project id %d = %v, want ErrProjectMismatch", name, pid, err)
			}
		}
		if n := rt.n.Load(); n != 0 {
			t.Errorf("project id %d: %d requests were sent, want none", pid, n)
		}
	}
}

// TestProjectGuardLetsTheBoundProjectThrough is the control for the test above:
// with the bound id every method gets as far as the transport.
func TestProjectGuardLetsTheBoundProjectThrough(t *testing.T) {
	for name := range everyMethod(&youTrack{}) {
		rt := &countingTripper{}
		y, err := newYouTrackOver(Options{
			BaseURL: "https://yt.example.com", Token: credential(),
			Project: ProjectRef{ID: "0-1", ShortName: "UZI"}, ProjectID: testProjectID, EligibilityTag: fakeEligibility,
			OpenState: "Open", ClosedState: "Fixed",
		}, rt)
		if err != nil {
			t.Fatal(err)
		}
		err = everyMethod(y)[name]()
		if errors.Is(err, ErrProjectMismatch) || errors.Is(err, ErrProjectNotBound) {
			t.Errorf("%s with the bound id = %v", name, err)
		}
		if rt.n.Load() == 0 {
			t.Errorf("%s with the bound id sent no request", name)
		}
	}
}
