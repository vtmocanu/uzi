package issuetracker

import (
	"context"
	"errors"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

func TestStateFieldOption(t *testing.T) {
	srv, y := newFakeDriverWithField(t, "Stage")
	_ = srv
	ctx := context.Background()
	if _, err := y.ListStates(ctx, "0-1"); err != nil {
		t.Fatal(err)
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
