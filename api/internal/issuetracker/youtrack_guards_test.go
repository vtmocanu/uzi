package issuetracker

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
)

func TestSetIssueStateRefusesAStateThatIsNeitherOpenedNorClosed(t *testing.T) {
	y, srv := newFakeDriver(t)
	n := srv.SeedIssue("t", "")
	srv.ResetRequests()
	if err := y.SetIssueState(context.Background(), 1, n, forge.IssueState("merged")); err == nil {
		t.Fatal("SetIssueState accepted an unknown state")
	}
	for _, r := range srv.Requests() {
		if r.Method != http.MethodGet {
			t.Errorf("sent a write: %v", r)
		}
	}
}

func TestSetIssueStateOnAnUnboundDriverWithNoMappingIsNotBound(t *testing.T) {
	y, err := newYouTrackOver(Options{BaseURL: "https://yt.example.com", Token: credential()}, roundTripFunc(
		func(*http.Request) (*http.Response, error) { return nil, errors.New("no request expected") }))
	if err != nil {
		t.Fatal(err)
	}
	err = y.SetIssueState(context.Background(), 1, 1, forge.StateClosed)
	if !errors.Is(err, ErrProjectNotBound) {
		t.Fatalf("err = %v, want ErrProjectNotBound (checked before the mapping)", err)
	}
}

func TestListIssuesRefusesMalformedIDs(t *testing.T) {
	for name, issue := range map[string]string{
		"id-readable": rawIssue("2-1", "x y/../z", 1, "0-1", ""),
		"entity id":   rawIssue("../..", "UZI-1", 1, "0-1", ""),
		"tag id":      rawIssue("2-1", "UZI-1", 1, "0-1", `{"id":"..","name":"x"}`),
	} {
		t.Run(name, func(t *testing.T) {
			y, _ := loggedDriver(t, rawListHandler(issue))
			if got, err := y.ListIssues(context.Background(), 1, forge.ListIssuesOptions{}); err == nil {
				t.Errorf("ListIssues accepted a malformed %s: %+v", name, got)
			}
		})
	}
}

func TestListIssuesLimitWithinOnePageStopsAfterOneRequest(t *testing.T) {
	y, srv := newFakeDriver(t)
	for range 250 {
		srv.SeedIssue("t", "")
	}
	srv.ResetRequests()
	got, err := y.ListIssues(context.Background(), 1, forge.ListIssuesOptions{Limit: 5})
	if err != nil || len(got) != 5 {
		t.Fatalf("got %d issues, %v; want 5", len(got), err)
	}
	reqs := srv.Requests()
	if len(reqs) != 1 || reqs[0].Method != http.MethodGet || reqs[0].Path != "/api/issues" {
		t.Errorf("requests = %v, want one GET /api/issues", reqs)
	}
}

func TestIssueWithoutANumberIsNotInTheProject(t *testing.T) {
	zero := rawIssue("2-9", "UZI-9", 0, "0-1", "")
	good := rawIssue("2-1", "UZI-1", 1, "0-1", "")
	y, _ := loggedDriver(t, rawListHandler(zero, good))
	got, err := y.ListIssues(context.Background(), 1, forge.ListIssuesOptions{})
	if err != nil || len(got) != 1 || got[0].IID != 1 {
		t.Fatalf("ListIssues = %+v, %v; want only issue 1", got, err)
	}
	// CreateIssue reads the created issue back; a number of 0 there is refused.
	y, _ = loggedDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || strings.HasSuffix(r.URL.Path, "/2-1") {
			writeRaw(w, rawIssue("2-1", "UZI-1", 0, "0-1", ""))
			return
		}
		writeRaw(w, "[]")
	})
	if got, err := y.CreateIssue(context.Background(), 1, "t", "d", nil); err == nil {
		t.Errorf("CreateIssue accepted a read-back with number 0: %+v", got)
	}
}

func TestCreateIssueRefusesAMalformedCreatedID(t *testing.T) {
	y, log := loggedDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writeRaw(w, rawIssue("..", "UZI-1", 1, "0-1", ""))
			return
		}
		writeRaw(w, rawIssue("2-1", "UZI-1", 1, "0-1", ""))
	})
	if _, err := y.CreateIssue(context.Background(), 1, "t", "d", []string{fakeEligibility.Name}); err == nil {
		t.Fatal("CreateIssue accepted a malformed created id")
	}
	// The eligibility label needs no lookup, so the POST is the only request:
	// the malformed id stops the attach and the read-back.
	if got := log.all(); len(got) != 1 {
		t.Errorf("requests = %v, want only the POST", got)
	}
}

func TestUpdateIssueLabelsDeduplicates(t *testing.T) {
	y, srv := newFakeDriver(t)
	n := srv.SeedIssue("t", "")
	ctx := context.Background()
	count := func(method, suffix string) int {
		c := 0
		for _, r := range srv.Requests() {
			if r.Method == method && strings.HasSuffix(r.Path, suffix) && strings.Contains(r.Path, "/issues/") {
				c++
			}
		}
		return c
	}
	srv.ResetRequests()
	if err := y.UpdateIssueLabels(ctx, 1, n, []string{"x", "x"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := count(http.MethodPost, "/tags"); got != 1 {
		t.Errorf("%d attach calls for [x x], want 1", got)
	}
	srv.ResetRequests()
	if err := y.UpdateIssueLabels(ctx, 1, n, nil, []string{"x", "x"}); err != nil {
		t.Fatal(err)
	}
	deletes := 0
	for _, r := range srv.Requests() {
		if r.Method == http.MethodDelete {
			deletes++
		}
	}
	if deletes != 1 {
		t.Errorf("%d detach calls for [x x], want 1", deletes)
	}
}

// TestUpdateIssueLabelsAddOfAPresentTagWritesNothing: "already on the issue" is
// decided by tag id. The eligibility name resolves to its stored id with no
// request at all; another name needs the account and its tags, and then still
// writes nothing.
func TestUpdateIssueLabelsAddOfAPresentTagWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		seed     func(*youtracktest.Server) string // returns the tag id put on the issue
		label    string
		wantRead bool // the tag list and users/me are read
	}{
		{"eligibility tag", func(*youtracktest.Server) string { return youtracktest.EligibilityTagID }, youtracktest.EligibilityTagName, false},
		{"account tag", func(s *youtracktest.Server) string { return s.SeedOwnTag("x") }, "x", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			y, srv := newFakeDriver(t)
			n := srv.SeedIssue("t", "", tc.seed(srv))
			srv.ResetRequests()
			if err := y.UpdateIssueLabels(context.Background(), testProjectID, n, []string{tc.label}, nil); err != nil {
				t.Fatal(err)
			}
			readsTags := false
			for _, r := range srv.Requests() {
				if r.Method != http.MethodGet {
					t.Errorf("wrote for an already-present label: %v", r)
				}
				readsTags = readsTags || strings.HasPrefix(r.Path, "/api/tags") || r.Path == "/api/users/me"
			}
			if readsTags != tc.wantRead {
				t.Errorf("read tags or the account = %v, want %v", readsTags, tc.wantRead)
			}
		})
	}
}

func TestListedCommentsAndEventsCarryTheirTimestamps(t *testing.T) {
	y, _ := loggedDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/comments"):
			writeRaw(w, `[{"id":"4-1","text":"hi","created":1500,"author":{"id":"1-1","login":"alice"}}]`)
		case strings.HasSuffix(r.URL.Path, "/activities"):
			writeRaw(w, `[{"id":"3-1","timestamp":2500,"author":{"id":"1-1","login":"bob"},"added":[{"id":"5-1","name":"a"}]}]`)
		default:
			writeRaw(w, rawIssue("2-1", "UZI-1", 1, "0-1", ""))
		}
	})
	ctx := context.Background()
	comments, err := y.ListIssueComments(ctx, 1, 1)
	if err != nil || len(comments) != 1 || !comments[0].CreatedAt.Equal(time.UnixMilli(1500)) || comments[0].AuthorUsername != "alice" {
		t.Errorf("comments = %+v, %v", comments, err)
	}
	events, err := y.ListIssueLabelEvents(ctx, 1, 1)
	if err != nil || len(events) != 1 || !events[0].CreatedAt.Equal(time.UnixMilli(2500)) || events[0].Username != "bob" {
		t.Errorf("events = %+v, %v", events, err)
	}
}

func TestErrorTextJoinsErrorAndDescription(t *testing.T) {
	y, _ := loggedDriver(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		writeRaw(w, `{"error":"bad_request","error_description":"nope"}`)
	})
	_, err := y.GetIssue(context.Background(), 1, 1)
	if err == nil || !strings.Contains(err.Error(), "bad_request: nope") {
		t.Errorf("err = %v, want a message with \"bad_request: nope\"", err)
	}
}

func TestRawErrorBodyIsCutToTwoHundredBytes(t *testing.T) {
	y, _ := loggedDriver(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		writeRaw(w, strings.Repeat("x", 400))
	})
	_, err := y.GetIssue(context.Background(), 1, 1)
	if err == nil || strings.Count(err.Error(), "x") != 200 {
		t.Errorf("err = %v, want exactly 200 body bytes", err)
	}
}

func TestResponseFailuresAreClassifiedAndRedacted(t *testing.T) {
	classified := func(t *testing.T, err error) *classifiedError {
		t.Helper()
		var ce *classifiedError
		if !errors.As(err, &ce) {
			t.Fatalf("err %T %v is not a redacted, classified error", err, err)
		}
		return ce
	}
	t.Run("decode error", func(t *testing.T) {
		y, _ := loggedDriver(t, func(w http.ResponseWriter, _ *http.Request) { writeRaw(w, `{not json`) })
		_, err := y.GetIssue(context.Background(), 1, 1)
		if ce := classified(t, err); !strings.Contains(ce.message, "decode response") {
			t.Errorf("message = %q", ce.message)
		}
	})
	// A body shorter than its Content-Length ends in a read error, on both the
	// success path and the error-body path.
	truncated := func(status int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(status)
			_, _ = w.Write([]byte("0123456789"))
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
	}
	t.Run("read error on success", func(t *testing.T) {
		y, _ := loggedDriver(t, truncated(http.StatusOK))
		_, err := y.GetIssue(context.Background(), 1, 1)
		_ = classified(t, err)
	})
	t.Run("read error on a server error", func(t *testing.T) {
		y, _ := loggedDriver(t, truncated(http.StatusInternalServerError))
		_, err := y.GetIssue(context.Background(), 1, 1)
		if ce := classified(t, err); ce.class != forge.ErrorClassServerError {
			t.Errorf("class = %v, want server error", ce.class)
		}
	})
}

func TestRedactorScrubsBeforeItTruncates(t *testing.T) {
	tok := fixtureCredential()
	r := newRedactor(tok)
	// The token starts before the cut at maxMessageBytes and ends after it, so
	// truncating first would leave its prefix behind.
	out := r.string(strings.Repeat("a", maxMessageBytes-7) + tok)
	if strings.Contains(out, "perm") {
		t.Errorf("a token that straddles the cut left a prefix: %q", out[len(out)-12:])
	}
}

func TestValidProjectRef(t *testing.T) {
	for ref, want := range map[ProjectRef]bool{
		{}:                              true,
		{ID: "0-1", ShortName: "UZI"}:   true,
		{ID: "0-1"}:                     false,
		{ShortName: "UZI"}:              false,
		{ID: "../x", ShortName: "UZI"}:  false,
		{ID: "0-1", ShortName: "1X"}:    false,
		{ID: "0-1", ShortName: "U Z I"}: false,
	} {
		if got := ValidProjectRef(ref) == nil; got != want {
			t.Errorf("ValidProjectRef(%+v) ok = %v, want %v", ref, got, want)
		}
	}
	if !ValidProjectID("12-3") || ValidProjectID("12") || ValidProjectID("") {
		t.Error("ValidProjectID disagrees with the entity id shape")
	}
}

// TestBuildErrorsNeverCarryTheToken builds a driver with each kind of invalid
// option and checks that no message holds the token or a piece of it.
func TestBuildErrorsNeverCarryTheToken(t *testing.T) {
	long := strings.Repeat("Qz7", maxTokenLen)
	secret := strings.Join([]string{"perm", ":", "Zq9xLm2VnB7wKp3T", "-", "Yk3mQ8vR2xTn5Lw7"}, "")
	good := Options{BaseURL: "https://yt.example.com", Token: secret}
	for name, o := range map[string]Options{
		"bad URL":        {BaseURL: "http://yt.example.com", Token: secret},
		"URL with token": {BaseURL: "https://" + secret + "@yt.example.com", Token: secret},
		"control char":   {BaseURL: good.BaseURL, Token: secret + "\u001b"},
		"line separator": {BaseURL: good.BaseURL, Token: secret + "\u2028"},
		"too long":       {BaseURL: good.BaseURL, Token: long},
		"bad project":    {BaseURL: good.BaseURL, Token: secret, Project: ProjectRef{ID: "x", ShortName: "UZI"}},
		"bad short name": {BaseURL: good.BaseURL, Token: secret, Project: ProjectRef{ID: "0-1", ShortName: "1"}},
	} {
		_, err := New(KindYouTrack, o)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		msg := err.Error()
		for _, w := range eightCharWindows(o.Token) {
			if strings.Contains(msg, w) {
				t.Errorf("%s: the message carries the token fragment %q", name, w)
				break
			}
		}
	}
}
