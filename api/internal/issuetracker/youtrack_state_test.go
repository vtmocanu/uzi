package issuetracker

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
)

// TestStateFieldOption: with the State field renamed, a driver told the new
// name lists that field's values as the fake defines them, and a driver left on
// the default name finds no state field.
func TestStateFieldOption(t *testing.T) {
	srv, y := newFakeDriverWithField(t, "Stage")
	ctx := context.Background()
	got, err := y.ListStates(ctx, youtracktest.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	want := []State{
		{ID: "10-1", Name: "Open", Resolved: false, Ordinal: 0},
		{ID: "10-2", Name: "Fixed", Resolved: true, Ordinal: 1},
		{ID: "10-3", Name: "Won't fix", Resolved: true, Ordinal: 2},
	}
	if !slices.Equal(got, want) {
		t.Errorf("ListStates = %+v, want %+v", got, want)
	}
	byDefault, err := newYouTrackOver(fakeOptions(srv), srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := byDefault.ListStates(ctx, youtracktest.ProjectID); err == nil {
		t.Errorf("a driver on the default field name listed %+v from a project whose field is named Stage", got)
	}
}

func TestStateMappingUnset(t *testing.T) {
	_, srv := newFakeDriver(t)
	opts := fakeOptions(srv)
	opts.OpenState, opts.ClosedState = "", ""
	y, err := newYouTrackOver(opts, srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	n := srv.SeedIssue("t", "")
	for _, s := range []forge.IssueState{forge.StateOpened, forge.StateClosed} {
		if err := y.SetIssueState(context.Background(), 1, n, s); !errors.Is(err, ErrStateMappingUnset) {
			t.Errorf("SetIssueState(%s) = %v", s, err)
		}
	}
	y2, _ := newFakeDriver(t)
	if err := y2.SetIssueState(context.Background(), 1, 1, forge.StateAll); err == nil {
		t.Error("SetIssueState accepted StateAll")
	}
}

func TestSetIssueStateHonoursStateField(t *testing.T) {
	srv, y := newFakeDriverWithField(t, "Stage")
	n := srv.SeedIssue("t", "")
	ctx := context.Background()
	if err := y.SetIssueState(ctx, 1, n, forge.StateClosed); err != nil {
		t.Fatalf("SetIssueState: %v", err)
	}
	got, err := y.GetIssue(ctx, 1, n)
	if err != nil || got.State != string(forge.StateClosed) {
		t.Fatalf("GetIssue = %+v, %v; want closed", got, err)
	}
}
