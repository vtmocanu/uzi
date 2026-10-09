package forge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v92/github"
)

// controlledAuthorDeadline expires only when the fake transport enters a shared
// request. Its Done and Err have the same deadline semantics as WithTimeout.
type controlledAuthorDeadline struct {
	context.Context
	cancel   context.CancelCauseFunc
	deadline time.Time
}

func newAuthorDeadline(ctx context.Context) *controlledAuthorDeadline {
	child, cancel := context.WithCancelCause(ctx)
	return &controlledAuthorDeadline{Context: child, cancel: cancel, deadline: time.Now().Add(50 * time.Millisecond)}
}
func (c *controlledAuthorDeadline) Deadline() (time.Time, bool) { return c.deadline, true }
func (c *controlledAuthorDeadline) Err() error {
	if errors.Is(context.Cause(c.Context), context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return c.Context.Err()
}
func (c *controlledAuthorDeadline) expire() { c.cancel(context.DeadlineExceeded) }
func authorResponse(r *http.Request, body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

type authorContextFixture struct {
	t             *testing.T
	pages         map[int]int
	identities    map[int]int
	identityEnter func(*http.Request)
	repos         int
	enter         func(*http.Request)
	listingErr    error
}

func (f *authorContextFixture) driver() *github {
	client, err := gh.NewClient(gh.WithHTTPClient(&http.Client{Transport: githubLabelTransport(f.roundTrip)}))
	if err != nil {
		f.t.Fatal(err)
	}
	return &github{client: client, redact: newRedactor("fixture")}
}
func (f *authorContextFixture) roundTrip(r *http.Request) (*http.Response, error) {
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/user/"):
		id, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/user/"))
		if err != nil {
			return nil, err
		}
		if f.identities == nil {
			f.identities = make(map[int]int)
		}
		f.identities[id]++
		if f.identityEnter != nil {
			f.identityEnter(r)
		}
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		return authorResponse(r, fmt.Sprintf("{\"id\":%d,\"login\":\"user%d\"}", id, id)), nil
	case r.URL.Path == "/repositories/7":
		f.repos++
		return authorResponse(r, `{"id":7,"name":"widgets","owner":{"login":"acme"}}`), nil
	case r.URL.Path == "/repos/acme/widgets/collaborators":
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		f.pages[page]++
		if r.URL.Query().Get("affiliation") != "all" {
			f.t.Error("missing affiliation=all")
		}
		if f.enter != nil {
			f.enter(r)
		}
		if f.listingErr != nil {
			return nil, f.listingErr
		}
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		id := 41 + page
		resp := authorResponse(r, fmt.Sprintf("[{\"id\":%d,\"login\":\"user%d\",\"permissions\":{\"pull\":true,\"triage\":true,\"push\":false,\"maintain\":false,\"admin\":false}}]", id, id))
		if page == 1 {
			resp.Header.Set("Link", `<https://api.github.com/repos/acme/widgets/collaborators?page=2>; rel="next"`)
		}
		return resp, nil
	default:
		return nil, fmt.Errorf("unexpected request %s", r.URL)
	}
}

func TestGitHubAuthorParentCancellationDuringListing(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx := BeginAuthorAssessment(parent)
	f := &authorContextFixture{t: t, pages: map[int]int{}}
	f.enter = func(r *http.Request) {
		cancel()
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
			t.Fatal("listing did not observe parent cancellation")
		}
	}
	d := f.driver()
	got, err := d.RepositoryAuthorEligibility(ctx, 7, 42)
	if got != AuthorUnknown || err == nil || !errors.Is(parent.Err(), context.Canceled) {
		t.Fatalf("got=%v err=%v", got, err)
	}
	assertAuthor(t, d, ctx, 43, AuthorUnknown)
	if f.pages[1] != 1 || f.pages[2] != 0 {
		t.Fatalf("pages=%v", f.pages)
	}
}

func TestGitHubAuthorNoCacheUsesCallerCancellation(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lookup := newAuthorDeadline(parent)
	defer lookup.expire()
	f := &authorContextFixture{t: t, pages: map[int]int{}}
	f.enter = func(*http.Request) { lookup.expire() }
	got, err := f.driver().RepositoryAuthorEligibility(lookup, 7, 42)
	if got != AuthorUnknown || err == nil || !errors.Is(lookup.Err(), context.DeadlineExceeded) {
		t.Fatalf("got=%v err=%v", got, err)
	}
	if f.pages[1] != 1 || f.pages[2] != 0 {
		t.Fatalf("pages=%v", f.pages)
	}
}

func TestGitHubAuthorAssessmentErrorsAreCached(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx := BeginAuthorAssessment(parent)
	f := &authorContextFixture{t: t, pages: map[int]int{}}
	sentinel := errors.New("shared evidence unavailable")
	d := f.driver()
	f.listingErr = sentinel
	var cachedErr error
	for _, id := range []int64{42, 43} {
		got, err := d.RepositoryAuthorEligibility(ctx, 7, id)
		if got != AuthorUnknown || err == nil || !strings.Contains(err.Error(), sentinel.Error()) {
			t.Fatalf("author=%d got=%v err=%v", id, got, err)
		}
		if cachedErr != nil && err != cachedErr {
			t.Fatal("shared error was not cached")
		}
		cachedErr = err
	}
	if f.repos != 1 || f.pages[1] != 1 {
		t.Fatalf("repos=%d pages=%v", f.repos, f.pages)
	}
}

func TestGitHubAuthorIdentityKeepsLookupContext(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	assessment := BeginAuthorAssessment(parent)
	lookup := newAuthorDeadline(assessment)
	defer lookup.expire()
	f := &authorContextFixture{t: t, pages: map[int]int{}}
	f.identityEnter = func(r *http.Request) {
		if r.Context().Done() != lookup.Done() {
			t.Error("identity did not use lookup context")
		}
		lookup.expire()
	}
	got, err := f.driver().RepositoryAuthorEligibility(lookup, 7, 42)
	if got != AuthorUnknown || err == nil || !errors.Is(lookup.Err(), context.DeadlineExceeded) {
		t.Fatalf("identity got=%v err=%v", got, err)
	}
	if f.repos != 0 || len(f.pages) != 0 || parent.Err() != nil {
		t.Fatalf("repos=%d pages=%v assessment error=%v", f.repos, f.pages, parent.Err())
	}
}

func TestGitHubAuthorAssessmentDeadlineDuringListing(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	deadline := newAuthorDeadline(parent)
	defer deadline.expire()
	assessment := BeginAuthorAssessment(deadline)
	f := &authorContextFixture{t: t, pages: map[int]int{}}
	f.enter = func(r *http.Request) {
		deadline.expire()
		select {
		case <-r.Context().Done():
		default:
			t.Fatal("listing did not observe assessment expiry")
		}
	}
	d := f.driver()
	got, err := d.RepositoryAuthorEligibility(assessment, 7, 42)
	if got != AuthorUnknown || err == nil || !errors.Is(deadline.Err(), context.DeadlineExceeded) {
		t.Fatalf("expired assessment got=%v err=%v", got, err)
	}
	assertAuthor(t, d, assessment, 43, AuthorUnknown)
	if f.pages[1] != 1 || f.pages[2] != 0 || parent.Err() != nil {
		t.Fatalf("pages=%v parent error=%v", f.pages, parent.Err())
	}
}

func TestGitHubAuthorSharedEvidenceOutlivesLookupDeadline(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	assessment := BeginAuthorAssessment(parent)
	lookup := newAuthorDeadline(assessment)
	defer lookup.expire()
	f := &authorContextFixture{t: t, pages: map[int]int{}}
	f.enter = func(r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			lookup.expire()
		}
	}
	d := f.driver()
	assertAuthor(t, d, lookup, 42, AuthorEligible)
	select {
	case <-lookup.Done():
	default:
		t.Fatal("lookup Done did not close on collaborator entry")
	}
	if !errors.Is(lookup.Err(), context.DeadlineExceeded) {
		t.Fatalf("lookup Err=%v", lookup.Err())
	}
	fresh, stop := context.WithCancel(assessment)
	defer stop()
	assertAuthor(t, d, fresh, 43, AuthorEligible)
	if f.identities[42] != 1 || f.identities[43] != 1 || len(f.identities) != 2 {
		t.Fatalf("identity requests=%v; want one per author", f.identities)
	}
	if f.repos != 1 || f.pages[1] != 1 || f.pages[2] != 1 || len(f.pages) != 2 {
		t.Fatalf("repository=%d pages=%v; want one fetch per page", f.repos, f.pages)
	}
}
