package pushbroker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// #4223: once a stage of the base fetch returns after the caller's context has ended, the
// context error wins over whatever the stage returned. Each stage below ends the context and
// then returns a result a cancellation-raced response can produce, so the ordering is exact
// rather than a timing race.

const wantedRef = "refs/heads/main"

func wantedMap() map[string]string { return map[string]string{wantedRef: "refs/remotes/origin/main"} }

func advertisedWanted() []*plumbing.Reference {
	return []*plumbing.Reference{plumbing.NewHashReference(wantedRef, plumbing.NewHash("1111111111111111111111111111111111111111"))}
}

// An ender returns a context and the function a stage calls to end it: cancel for a cancel
// context; for a deadline context, wait until its short deadline has passed.
type ender func(t *testing.T) (context.Context, func())

func cancelEnder(t *testing.T) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx, cancel
}

func deadlineEnder(t *testing.T) (context.Context, func()) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(20*time.Millisecond))
	t.Cleanup(cancel)
	return ctx, func() { <-ctx.Done() }
}

func TestFetchWantedPrefersContextError(t *testing.T) {
	cases := []struct {
		name  string
		stage string // the label the error must carry
		list  func(end func()) ([]*plumbing.Reference, error)
		fetch func(end func()) error
	}{
		{
			name: "list empty input", stage: "list",
			list: func(end func()) ([]*plumbing.Reference, error) { end(); return nil, packp.ErrEmptyInput },
		},
		{
			name: "list empty remote", stage: "list",
			list: func(end func()) ([]*plumbing.Reference, error) {
				end()
				return nil, transport.ErrEmptyRemoteRepository
			},
		},
		{
			name: "list advertises none of the wanted refs", stage: "list",
			list: func(end func()) ([]*plumbing.Reference, error) {
				end()
				return []*plumbing.Reference{plumbing.NewHashReference("refs/heads/other", plumbing.NewHash("2222222222222222222222222222222222222222"))}, nil
			},
		},
		{
			name: "fetch nil (empty upload-pack treated as up to date)", stage: "fetch",
			fetch: func(end func()) error { end(); return nil },
		},
		{
			name: "fetch already up to date", stage: "fetch",
			fetch: func(end func()) error { end(); return git.NoErrAlreadyUpToDate },
		},
		{
			name: "fetch empty remote", stage: "fetch",
			fetch: func(end func()) error { end(); return transport.ErrEmptyRemoteRepository },
		},
	}
	enders := []struct {
		name  string
		mk    ender
		want  error
		other error
	}{
		{"cancelled", cancelEnder, context.Canceled, context.DeadlineExceeded},
		{"deadline", deadlineEnder, context.DeadlineExceeded, context.Canceled},
	}
	for _, e := range enders {
		for _, tc := range cases {
			t.Run(e.name+"/"+tc.name, func(t *testing.T) {
				ctx, end := e.mk(t)
				list := func(context.Context) ([]*plumbing.Reference, error) { return advertisedWanted(), nil }
				if tc.list != nil {
					list = func(context.Context) ([]*plumbing.Reference, error) { return tc.list(end) }
				}
				fetch := func(context.Context, []config.RefSpec) error {
					t.Fatal("fetch reached after the context ended at list")
					return nil
				}
				if tc.fetch != nil {
					fetch = func(context.Context, []config.RefSpec) error { return tc.fetch(end) }
				}
				err := fetchWanted(ctx, list, fetch, wantedMap())
				if err == nil {
					t.Fatalf("fetchWanted returned nil after the context ended at %s", tc.stage)
				}
				if !errors.Is(err, e.want) || errors.Is(err, e.other) {
					t.Fatalf("err = %v, want errors.Is %v and not %v", err, e.want, e.other)
				}
				if !strings.HasPrefix(err.Error(), "pushbroker: "+tc.stage+": ") {
					t.Fatalf("err = %q, want the %s stage label", err, tc.stage)
				}
			})
		}
	}
}

// With a live context the tolerances are unchanged.
func TestFetchWantedLiveContextTolerances(t *testing.T) {
	ctx := context.Background()
	noFetch := func(context.Context, []config.RefSpec) error {
		t.Fatal("fetch must not run")
		return nil
	}
	listOK := func(context.Context) ([]*plumbing.Reference, error) { return advertisedWanted(), nil }

	if err := fetchWanted(ctx, func(context.Context) ([]*plumbing.Reference, error) {
		return nil, transport.ErrEmptyRemoteRepository
	}, noFetch, wantedMap()); err != nil {
		t.Fatalf("empty remote at list: %v", err)
	}
	if err := fetchWanted(ctx, func(context.Context) ([]*plumbing.Reference, error) {
		return nil, nil
	}, noFetch, wantedMap()); err != nil {
		t.Fatalf("no wanted refs advertised: %v", err)
	}
	err := fetchWanted(ctx, func(context.Context) ([]*plumbing.Reference, error) {
		return nil, packp.ErrEmptyInput
	}, noFetch, wantedMap())
	if err == nil || err.Error() != "pushbroker: list: empty input" || errors.Is(err, context.Canceled) {
		t.Fatalf("list empty input = %v, want pushbroker: list: empty input", err)
	}
	for _, r := range []error{nil, git.NoErrAlreadyUpToDate, transport.ErrEmptyRemoteRepository} {
		var gotSpecs []config.RefSpec
		if err := fetchWanted(ctx, listOK, func(_ context.Context, specs []config.RefSpec) error {
			gotSpecs = specs
			return r
		}, wantedMap()); err != nil {
			t.Fatalf("fetch result %v: %v", r, err)
		}
		if len(gotSpecs) != 1 || gotSpecs[0] != config.RefSpec("+"+wantedRef+":refs/remotes/origin/main") {
			t.Fatalf("fetch specs = %v", gotSpecs)
		}
	}
	boom := errors.New("boom")
	if err := fetchWanted(ctx, listOK, func(context.Context, []config.RefSpec) error { return boom }, wantedMap()); !errors.Is(err, boom) ||
		!strings.HasPrefix(err.Error(), "pushbroker: fetch: ") {
		t.Fatalf("fetch error = %v, want wrapped boom", err)
	}
}

// A guarded ciConfigGuard entered with an already-ended context reports the context error,
// not a definitive ErrCIConfigGuard refusal (#4223).
func TestCIConfigGuardEndedContextIsNotARefusal(t *testing.T) {
	b := newTB(t)
	wf := func(c string) object.TreeEntry {
		return b.dir(".github", b.dir("workflows", file("ci.yml", c)))
	}
	repo, cand := commitRepo(t, b, "main", b.tree(file("a", "1"), wf("v1")), b.tree(file("a", "1"), wf("v2")))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := ciConfigGuard(ctx, repo, cand, "main", floorGlobs)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrCIConfigGuard) {
		t.Fatalf("err = %v, want context.Canceled and not ErrCIConfigGuard", err)
	}
	// Control: a live context still refuses the protected change.
	if err := ciConfigGuard(context.Background(), repo, cand, "main", floorGlobs); !errors.Is(err, ErrCIConfigGuard) {
		t.Fatalf("live context err = %v, want ErrCIConfigGuard", err)
	}
	// Nothing protected stays nil even with an ended context.
	if err := ciConfigGuard(ctx, repo, cand, "main", nil); err != nil {
		t.Fatalf("unguarded with ended context: %v", err)
	}
}
