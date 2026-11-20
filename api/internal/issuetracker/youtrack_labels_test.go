package issuetracker

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
)

// TestLabelGuardOnEveryGuardedMethod sends a brace, and a name one byte over
// the length limit, through every method that takes label names.
func TestLabelGuardOnEveryGuardedMethod(t *testing.T) {
	y, log := loggedDriver(t, func(w http.ResponseWriter, _ *http.Request) { writeRaw(w, "[]") })
	ctx := context.Background()
	for _, name := range []string{"a}b", strings.Repeat("a", maxLabelNameLen+1)} {
		calls := map[string]func() error{
			"EnsureLabels": func() error { return y.EnsureLabels(ctx, 1, []forge.Label{{Name: name}}) },
			"ListIssues": func() error {
				_, err := y.ListIssues(ctx, 1, forge.ListIssuesOptions{Labels: []string{name}})
				return err
			},
			"CreateIssue":              func() error { _, err := y.CreateIssue(ctx, 1, "t", "d", []string{name}); return err },
			"UpdateIssueLabels add":    func() error { return y.UpdateIssueLabels(ctx, 1, 1, []string{name}, nil) },
			"UpdateIssueLabels remove": func() error { return y.UpdateIssueLabels(ctx, 1, 1, nil, []string{name}) },
		}
		for method, call := range calls {
			if err := call(); !errors.Is(err, ErrInvalidLabelName) {
				t.Errorf("%s(len %d) = %v, want ErrInvalidLabelName", method, len(name), err)
			}
		}
	}
	if got := log.all(); len(got) != 0 {
		t.Errorf("guarded calls sent %v", got)
	}
}

func TestUpdateIssueLabelsSkipsAbsentTagRemoval(t *testing.T) {
	y, srv := newFakeDriver(t)
	keep, drop := srv.SeedOwnTag("keep"), srv.SeedOwnTag("drop")
	n := srv.SeedIssue("t", "", keep, drop)
	srv.ResetRequests()
	if err := y.UpdateIssueLabels(context.Background(), 1, n, nil, []string{"never-there", "drop"}); err != nil {
		t.Fatal(err)
	}
	var deletes int
	for _, r := range srv.Requests() {
		if r.Method == http.MethodDelete {
			deletes++
		}
	}
	if deletes != 1 {
		t.Errorf("sent %d DELETE requests, want 1 (only the tag on the issue)", deletes)
	}
	if got := srv.IssueTagIDs(n); !slices.Equal(got, []string{keep}) {
		t.Errorf("tags = %v, want [%s]", got, keep)
	}
}

func TestResolveTagIDsTakesTheFirstTagTheAccountOwns(t *testing.T) {
	y, _ := loggedDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/users/me" {
			writeRaw(w, rawMe)
			return
		}
		writeRaw(w, `[{"id":"5-1","name":"dup","owner":{"id":"1-2"}},{"id":"5-2","name":"dup","owner":{"id":"`+rawMeID+`"}},{"id":"5-3","name":"dup","owner":{"id":"`+rawMeID+`"}}]`)
	})
	ids, err := y.resolveTagIDs(context.Background(), []string{"dup"}, false)
	if err != nil || ids["dup"] != "5-2" {
		t.Fatalf("resolveTagIDs = %v, %v; want the first tag the account owns, 5-2", ids, err)
	}
	labels, err := y.ListLabels(context.Background(), 1)
	// ListLabels now reports the stored eligibility name first, then the tags the
	// account owns, each name once, so the duplicate collapses to one dup.
	if err != nil || len(labels) != 2 || labels[0].Name != youtracktest.EligibilityTagName || labels[1].Name != "dup" {
		t.Fatalf("ListLabels = %+v, %v; want the eligibility name and one dup", labels, err)
	}
}

func TestCreateIssueAttachesADuplicateLabelOnce(t *testing.T) {
	y, srv := newFakeDriver(t)
	srv.ResetRequests()
	if _, err := y.CreateIssue(context.Background(), 1, "t", "d", []string{"x", "x"}); err != nil {
		t.Fatal(err)
	}
	var attaches int
	for _, r := range srv.Requests() {
		if r.Method == http.MethodPost && strings.HasPrefix(r.Path, "/api/issues/") && strings.HasSuffix(r.Path, "/tags") {
			attaches++
		}
	}
	if attaches != 1 {
		t.Errorf("attached %d times, want once", attaches)
	}
}

// TestMalformedTagIDsNeverReachAPath feeds ids that would alter a path.
func TestMalformedTagIDsNeverReachAPath(t *testing.T) {
	const bad = ".."
	t.Run("on the issue", func(t *testing.T) {
		y, log := loggedDriver(t, func(w http.ResponseWriter, _ *http.Request) {
			writeRaw(w, rawIssue("2-1", "UZI-1", 1, "0-1", `{"id":"`+bad+`","name":"x"}`))
		})
		if err := y.UpdateIssueLabels(context.Background(), 1, 1, nil, []string{"x"}); err == nil {
			t.Error("UpdateIssueLabels accepted a malformed tag id")
		}
		if got := log.writes(); len(got) != 0 {
			t.Errorf("sent %v", got)
		}
	})
	t.Run("in the tag list", func(t *testing.T) {
		y, log := loggedDriver(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/users/me":
				writeRaw(w, rawMe)
			case "/api/tags":
				writeRaw(w, `[{"id":"`+bad+`","name":"x","owner":{"id":"`+rawMeID+`"}}]`)
			default:
				writeRaw(w, rawIssue("2-1", "UZI-1", 1, "0-1", ""))
			}
		})
		ctx := context.Background()
		if _, err := y.ListLabels(ctx, 1); err == nil {
			t.Error("ListLabels accepted a malformed tag id")
		}
		if err := y.UpdateIssueLabels(ctx, 1, 1, []string{"x"}, nil); err == nil {
			t.Error("UpdateIssueLabels attached a malformed tag id")
		}
		if got := log.writes(); len(got) != 0 {
			t.Errorf("sent %v", got)
		}
	})
	t.Run("in a created tag", func(t *testing.T) {
		y, log := loggedDriver(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/api/users/me":
				writeRaw(w, rawMe)
			case r.URL.Path == "/api/tags" && r.Method == http.MethodGet:
				writeRaw(w, `[]`)
			case r.URL.Path == "/api/tags":
				writeRaw(w, `{"id":"`+bad+`","name":"x"}`)
			case strings.HasPrefix(r.URL.Path, "/api/tags/"):
				writeRaw(w, rawNoSharing)
			default:
				writeRaw(w, rawIssue("2-1", "UZI-1", 1, "0-1", ""))
			}
		})
		if err := y.UpdateIssueLabels(context.Background(), 1, 1, []string{"x"}, nil); err == nil {
			t.Error("UpdateIssueLabels attached a malformed created tag id")
		}
		for _, w := range log.writes() {
			if strings.Contains(w, "/issues/") {
				t.Errorf("sent %v", w)
			}
		}
	})
}

// driftedEligibilityDriver builds a driver whose stored eligibility tag is an
// account-owned tag that YouTrack now calls "uzi-finding", while the stored
// name, which resolves the eligibility label on writes, is still "uzi-ready".
func driftedEligibilityDriver(t *testing.T) (*youTrack, *youtracktest.Server, string) {
	t.Helper()
	srv := youtracktest.NewServer(t)
	eligID := srv.SeedOwnTag("uzi-finding")
	opts := fakeOptions(srv)
	opts.EligibilityTag = TagRef{ID: eligID, Name: "uzi-ready"}
	y, err := newYouTrackOver(opts, srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	return y, srv, eligID
}

// TestDriftedEligibilityTagNameNeverResolvesToTheEligibilityTag pins that a
// uzi write of a label equal to the eligibility tag's current YouTrack name
// does not attach the eligibility tag: a finding issue must not become
// eligible. YouTrack refuses a second tag of that name for the same owner, so
// the write fails (409) rather than succeeding with the wrong tag.
func TestDriftedEligibilityTagNameNeverResolvesToTheEligibilityTag(t *testing.T) {
	ctx := context.Background()
	t.Run("UpdateIssueLabels", func(t *testing.T) {
		y, srv, eligID := driftedEligibilityDriver(t)
		n := srv.SeedIssue("t", "")
		if err := y.UpdateIssueLabels(ctx, 1, n, []string{"uzi-finding"}, nil); err == nil {
			t.Error("UpdateIssueLabels succeeded; want the tag creation to fail")
		}
		if got := srv.IssueTagIDs(n); slices.Contains(got, eligID) {
			t.Errorf("issue tags = %v, include the eligibility tag %s", got, eligID)
		}
	})
	t.Run("CreateIssue", func(t *testing.T) {
		y, srv, eligID := driftedEligibilityDriver(t)
		srv.ResetRequests()
		if _, err := y.CreateIssue(ctx, 1, "t", "d", []string{"uzi-finding"}); err == nil {
			t.Error("CreateIssue succeeded; want the tag creation to fail")
		}
		for _, r := range srv.Requests() {
			if r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/api/issues") {
				t.Errorf("an issue was created although its label could not be resolved (eligibility tag %s)", eligID)
			}
		}
	})
	t.Run("EnsureLabels", func(t *testing.T) {
		y, srv, _ := driftedEligibilityDriver(t)
		if err := y.EnsureLabels(ctx, 1, []forge.Label{{Name: "uzi-finding"}}); err == nil {
			t.Error("EnsureLabels succeeded; want the tag creation to fail")
		}
		if got := srv.TagsNamed("uzi-finding"); len(got) != 1 {
			t.Errorf("tags named uzi-finding = %d, want 1 (the eligibility tag alone)", len(got))
		}
	})
}
