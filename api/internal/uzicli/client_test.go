package uzicli

import (
	"context"
	"errors"
	"testing"
)

func TestFakeGetRunNotFound(t *testing.T) {
	f := &FakeClient{RunByID: map[string]Run{}}
	_, err := f.GetRun(context.Background(), "missing")
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != ExitNotFound {
		t.Fatalf("GetRun err = %v, want ExitError{ExitNotFound}", err)
	}
}

func TestFakeReviewNotFound(t *testing.T) {
	f := &FakeClient{Reviews: map[string]Review{}}
	_, err := f.RunReview(context.Background(), "missing")
	if ExitCodeFor(err) != ExitNotFound {
		t.Fatalf("RunReview exit = %d, want %d", ExitCodeFor(err), ExitNotFound)
	}
}

func TestFakeErrPropagates(t *testing.T) {
	sentinel := Exitf(ExitAuth, "nope")
	f := &FakeClient{Err: sentinel}
	if _, err := f.ListRuns(context.Background()); !errors.Is(err, sentinel) {
		t.Errorf("ListRuns err = %v, want sentinel", err)
	}
	if _, err := f.Whoami(context.Background()); !errors.Is(err, sentinel) {
		t.Errorf("Whoami err = %v, want sentinel", err)
	}
}

func TestFakeHappyPath(t *testing.T) {
	f := &FakeClient{
		Runs:    []Run{{ID: "r1"}},
		Workers: []Worker{{ID: "w1"}},
		Repos:   []Repo{{ID: "p1"}},
	}
	if runs, err := f.ListRuns(context.Background()); err != nil || len(runs) != 1 {
		t.Errorf("ListRuns = %v, %v", runs, err)
	}
	if ws, err := f.ListWorkers(context.Background()); err != nil || len(ws) != 1 {
		t.Errorf("ListWorkers = %v, %v", ws, err)
	}
	if ps, err := f.ListRepos(context.Background()); err != nil || len(ps) != 1 {
		t.Errorf("ListRepos = %v, %v", ps, err)
	}
}

// HTTPClient methods are stubs in M3: they must not panic and must report
// not-implemented rather than making a live call.
func TestHTTPClientStubbed(t *testing.T) {
	c := NewHTTPClient(Settings{URL: "https://x", Token: "uzc_x"})
	if _, err := c.Whoami(context.Background()); err == nil {
		t.Error("HTTPClient.Whoami should return not-implemented in M3")
	}
}
