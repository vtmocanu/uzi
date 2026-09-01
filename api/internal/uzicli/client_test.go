package uzicli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// --- FakeClient ---

func TestFakeGetRunNotFound(t *testing.T) {
	f := &FakeClient{RunByID: map[string]apitypes.RunDTO{}}
	_, err := f.GetRun(context.Background(), "missing")
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != ExitNotFound {
		t.Fatalf("GetRun err = %v, want ExitError{ExitNotFound}", err)
	}
}

// The fake's RunReview is tri-state: a present-but-nil entry is a
// visible-but-unjudged run (nil, nil) — NOT the 404 path an absent key takes.
func TestFakeReviewTriState(t *testing.T) {
	f := &FakeClient{Reviews: map[string]*apitypes.ReviewDTO{
		"judged":   {Verdict: "good"},
		"unjudged": nil,
	}}
	if rv, _, err := f.RunReview(context.Background(), "judged"); err != nil || rv == nil {
		t.Fatalf("judged: rv=%v err=%v", rv, err)
	}
	if rv, _, err := f.RunReview(context.Background(), "unjudged"); err != nil || rv != nil {
		t.Fatalf("unjudged: want (nil,nil), got rv=%v err=%v", rv, err)
	}
	if _, _, err := f.RunReview(context.Background(), "absent"); ExitCodeFor(err) != ExitNotFound {
		t.Fatalf("absent: exit = %d, want %d", ExitCodeFor(err), ExitNotFound)
	}
}

// PendingJudges is a SEPARATE axis from the tri-state above (PRD #119): it decides
// whether a verdict is on its way, never whether the run is found. Both nil-review
// cases — with and without a pending judge — stay exit-0 reads, and a pending judge
// rides alongside a PRESENT review too (a re-judge in flight).
func TestFakePendingJudgeIsIndependentOfReview(t *testing.T) {
	f := &FakeClient{
		Reviews: map[string]*apitypes.ReviewDTO{
			"pending":  nil,
			"unjudged": nil,
			"rejudge":  {Verdict: "good"},
		},
		PendingJudges: map[string]*apitypes.PendingJudgeDTO{
			"pending": {State: "scheduled"},
			"rejudge": {State: "running"},
		},
	}
	rv, pj, err := f.RunReview(context.Background(), "pending")
	if err != nil || rv != nil || pj == nil || pj.State != "scheduled" {
		t.Fatalf("pending: want (nil, scheduled, nil), got rv=%v pj=%+v err=%v", rv, pj, err)
	}
	if rv, pj, err := f.RunReview(context.Background(), "unjudged"); err != nil || rv != nil || pj != nil {
		t.Fatalf("unjudged: want (nil,nil,nil), got rv=%v pj=%+v err=%v", rv, pj, err)
	}
	rv, pj, err = f.RunReview(context.Background(), "rejudge")
	if err != nil || rv == nil || pj == nil || pj.State != "running" {
		t.Fatalf("rejudge: want (review, running, nil), got rv=%v pj=%+v err=%v", rv, pj, err)
	}
	// A pending judge does NOT rescue an absent Reviews key: only that map decides
	// found-vs-404, exactly as before this field existed.
	f.PendingJudges["absent"] = &apitypes.PendingJudgeDTO{State: "running"}
	if _, _, err := f.RunReview(context.Background(), "absent"); ExitCodeFor(err) != ExitNotFound {
		t.Fatalf("absent: exit = %d, want %d", ExitCodeFor(err), ExitNotFound)
	}
}

// The fake's RunInputs mirrors the owner-only endpoint: a present key returns the
// queue (empty allowed), an absent key is the non-owner/unknown 404 path.
func TestFakeRunInputs(t *testing.T) {
	body := "steer me"
	f := &FakeClient{InputsByID: map[string][]apitypes.SteerInputDTO{
		"mine":  {{ID: 1, Body: &body}},
		"empty": {},
	}}
	if in, err := f.RunInputs(context.Background(), "mine"); err != nil || len(in) != 1 {
		t.Errorf("mine: in=%v err=%v", in, err)
	}
	if in, err := f.RunInputs(context.Background(), "empty"); err != nil || len(in) != 0 {
		t.Errorf("empty: in=%v err=%v", in, err)
	}
	if _, err := f.RunInputs(context.Background(), "foreign"); ExitCodeFor(err) != ExitNotFound {
		t.Errorf("foreign: exit = %d, want %d", ExitCodeFor(err), ExitNotFound)
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
		Runs:    []apitypes.RunListItemDTO{{RunDTO: apitypes.RunDTO{ID: "r1"}}},
		Workers: []apitypes.WorkerDTO{{ID: "w1"}},
		Repos:   []apitypes.RepoDTO{{ID: "p1"}},
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

// --- HTTPClient (against an httptest server) ---

// newTestClient points an HTTPClient at srv. httptest listens on 127.0.0.1, so
// its http URL is loopback and passes the https-only guard (the compose-laptop
// exception is exactly this shape).
func newTestClient(srv *httptest.Server) *HTTPClient {
	return &HTTPClient{BaseURL: srv.URL, Token: "uzc_test", HTTP: srv.Client()}
}

func TestHTTPClientWhoami(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer uzc_test" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"user":{"id":"u1","email":"a@b.c","is_admin":true},"uzi_label":"uzi"}`))
	}))
	defer srv.Close()
	u, err := newTestClient(srv).Whoami(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != "u1" || u.Email != "a@b.c" || !u.IsAdmin {
		t.Errorf("user = %+v", u)
	}
}

func TestHTTPClientGetMySettings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/me/settings" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"settings":{"default_model":"claude","judge_model":null,` +
			`"summary_model":null,"theme":null,"sidebar_token_ids":["a","b"]}}`))
	}))
	defer srv.Close()
	s, err := newTestClient(srv).GetMySettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.SidebarTokenIds) != 2 || s.SidebarTokenIds[0] != "a" || s.SidebarTokenIds[1] != "b" {
		t.Errorf("sidebar_token_ids = %v", s.SidebarTokenIds)
	}
	if s.DefaultModel == nil || *s.DefaultModel != "claude" {
		t.Errorf("default_model = %v", s.DefaultModel)
	}
	if s.JudgeModel != nil || s.Theme != nil {
		t.Errorf("nullable fields should decode to nil, got judge=%v theme=%v", s.JudgeModel, s.Theme)
	}
}

func TestFakeGetMySettings(t *testing.T) {
	f := &FakeClient{Settings: apitypes.UserSettingsDTO{SidebarTokenIds: []string{"x", "y"}}}
	s, err := f.GetMySettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.SidebarTokenIds) != 2 || s.SidebarTokenIds[0] != "x" {
		t.Errorf("settings = %+v", s)
	}
}

// A 200 {"review": null} is a visible-but-unjudged run: (nil, nil), exit 0. It
// must NOT be turned into a 404.
func TestHTTPClientReviewNull(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"review":null}`))
	}))
	defer srv.Close()
	rv, pj, err := newTestClient(srv).RunReview(context.Background(), "r1")
	if err != nil || rv != nil || pj != nil {
		t.Fatalf("null review: want (nil,nil,nil), got rv=%v pj=%v err=%v", rv, pj, err)
	}
}

func TestHTTPClientReviewPresent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"review":{"id":"rv1","verdict":"needs_work","status":"failed"}}`))
	}))
	defer srv.Close()
	rv, _, err := newTestClient(srv).RunReview(context.Background(), "r1")
	if err != nil || rv == nil || rv.Verdict != "needs_work" || rv.Status != "failed" {
		t.Fatalf("present review: rv=%+v err=%v", rv, err)
	}
}

// The response is a TWO-key envelope since PRD #119: {"review", "pending_judge"},
// each independently nullable. This pins all three shapes the server can send —
// pending over no review (the auto-judge case), pending over a present review (a
// re-judge in flight), and an explicit `"pending_judge": null` — because the field
// exists precisely to tell the first apart from the third, and a decode that dropped
// it would leave the CLI printing "not judged" at both.
func TestHTTPClientReviewPendingJudge(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantRv    bool
		wantState string // "" = want no pending judge
	}{
		{"pending over unjudged", `{"review":null,"pending_judge":{"state":"scheduled","enqueued_at":"2026-07-20T10:00:00Z"}}`, false, "scheduled"},
		{"pending over a review", `{"review":{"id":"rv1","verdict":"good"},"pending_judge":{"state":"running","enqueued_at":"2026-07-20T10:00:00Z"}}`, true, "running"},
		{"explicit null pending", `{"review":{"id":"rv1","verdict":"good"},"pending_judge":null}`, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			rv, pj, err := newTestClient(srv).RunReview(context.Background(), "r1")
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if (rv != nil) != tc.wantRv {
				t.Errorf("review present = %v, want %v", rv != nil, tc.wantRv)
			}
			if tc.wantState == "" {
				if pj != nil {
					t.Fatalf("pending_judge = %+v, want nil", pj)
				}
				return
			}
			if pj == nil {
				t.Fatalf("pending_judge = nil, want state %q", tc.wantState)
			}
			if pj.State != tc.wantState {
				t.Errorf("state = %q, want %q", pj.State, tc.wantState)
			}
			if pj.EnqueuedAt.IsZero() {
				t.Errorf("enqueued_at did not decode: %+v", pj)
			}
		})
	}
}

// A real 404 (run absent or not visible) is exit 4 — reserved, distinct from the
// null-review exit-0 path above.
func TestHTTPClientReview404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"run not found"}`))
	}))
	defer srv.Close()
	_, _, err := newTestClient(srv).RunReview(context.Background(), "r1")
	if ExitCodeFor(err) != ExitNotFound {
		t.Fatalf("exit = %d, want %d", ExitCodeFor(err), ExitNotFound)
	}
}

// RunInputs decodes the {"inputs": [...]} envelope into the DTO list, carrying
// consumed_at through as a pointer (null → nil = Queued, set → Delivered).
func TestHTTPClientRunInputs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"inputs":[{"id":2,"body":"b2","created_at":"2026-07-20T10:00:00Z","consumed_at":"2026-07-20T10:01:00Z"},{"id":1,"body":"b1","created_at":"2026-07-20T09:00:00Z","consumed_at":null}]}`))
	}))
	defer srv.Close()
	in, err := newTestClient(srv).RunInputs(context.Background(), "r1")
	if err != nil || len(in) != 2 {
		t.Fatalf("RunInputs: in=%+v err=%v", in, err)
	}
	if in[0].ID != 2 || in[0].ConsumedAt == nil {
		t.Errorf("first row should be consumed (Delivered): %+v", in[0])
	}
	if in[1].ID != 1 || in[1].ConsumedAt != nil {
		t.Errorf("second row should be unconsumed (Queued): %+v", in[1])
	}
}

// runLogsPagingServer serves a synthetic run history of `total` messages
// (seq 1..total) as bounded ?after=&limit= pages, exactly like the M2 endpoint:
// it returns at most `limit` messages with seq strictly greater than `after`, in
// ascending order. It counts the requests it served so tests can assert the paging
// arithmetic. If failAfterReq > 0, the (failAfterReq+1)-th request returns 500 —
// used to prove the all-or-nothing contract.
func runLogsPagingServer(t *testing.T, total int, failAfterReq int) (*httptest.Server, *int32, *int32) {
	t.Helper()
	var reqs int32
	var minAfter int32 = 1 << 30 // smallest `after` the server ever observed
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&reqs, 1)
		after, err := strconv.Atoi(r.URL.Query().Get("after"))
		if err != nil {
			t.Errorf("bad after: %q", r.URL.Query().Get("after"))
		}
		for {
			old := atomic.LoadInt32(&minAfter)
			if int32(after) >= old || atomic.CompareAndSwapInt32(&minAfter, old, int32(after)) { //nolint:gosec // G109: test-only cursor from a small paginated fixture, never overflows int32
				break
			}
		}
		limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil || limit <= 0 {
			t.Errorf("bad limit: %q", r.URL.Query().Get("limit"))
		}
		if failAfterReq > 0 && int(n) > failAfterReq {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		var b strings.Builder
		b.WriteString(`{"messages":[`)
		count := 0
		for seq := after + 1; seq <= total && count < limit; seq++ {
			if count > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"seq":%d,"kind":"stdout","payload":{},"created_at":"2026-08-16T00:00:00Z"}`, seq)
			count++
		}
		b.WriteString(`]}`)
		_, _ = w.Write([]byte(b.String()))
	}))
	return srv, &reqs, &minAfter
}

// RunLogs pages a large history and reassembles it in order. 450 messages come back
// as all 450 with contiguous seqs. With stop-on-empty the loop cannot treat the
// 50-message third page as terminal (a short page may be a server-clamped limit, not
// the end), so it issues a fourth request that comes back empty and stops: pages of
// 200/200/50/empty, four requests total.
func TestHTTPClientRunLogsMultiPage(t *testing.T) {
	srv, reqs, _ := runLogsPagingServer(t, 450, 0)
	defer srv.Close()
	msgs, err := newTestClient(srv).RunLogs(context.Background(), "r1", 0)
	if err != nil {
		t.Fatalf("RunLogs: %v", err)
	}
	if len(msgs) != 450 {
		t.Fatalf("len = %d, want 450", len(msgs))
	}
	for i, m := range msgs {
		if m.Seq != int32(i+1) {
			t.Fatalf("msg[%d].Seq = %d, want %d", i, m.Seq, i+1)
		}
	}
	if got := atomic.LoadInt32(reqs); got != 4 {
		t.Fatalf("requests = %d, want 4 (200+200+50+empty)", got)
	}
}

// The key contract test: if a page mid-sequence fails, RunLogs returns an error
// and DISCARDS everything accumulated so far. A caller must never receive the
// first 200 messages dressed up as a complete history.
func TestHTTPClientRunLogsAllOrNothing(t *testing.T) {
	srv, reqs, _ := runLogsPagingServer(t, 450, 1) // first page ok, second 500s
	defer srv.Close()
	msgs, err := newTestClient(srv).RunLogs(context.Background(), "r1", 0)
	if err == nil {
		t.Fatalf("expected an error, got nil (msgs=%d)", len(msgs))
	}
	if len(msgs) != 0 {
		t.Fatalf("partial history leaked: len = %d, want 0", len(msgs))
	}
	if got := atomic.LoadInt32(reqs); got != 2 {
		t.Fatalf("requests = %d, want 2 (page ok, page 500)", got)
	}
}

// When the total is an exact multiple of logsPageSize the first page is full, so
// the loop cannot tell it is done and must issue one more request; that page is
// empty and terminates the loop cleanly, returning exactly the whole history.
func TestHTTPClientRunLogsExactMultipleBoundary(t *testing.T) {
	srv, reqs, _ := runLogsPagingServer(t, logsPageSize, 0)
	defer srv.Close()
	msgs, err := newTestClient(srv).RunLogs(context.Background(), "r1", 0)
	if err != nil {
		t.Fatalf("RunLogs: %v", err)
	}
	if len(msgs) != logsPageSize {
		t.Fatalf("len = %d, want %d", len(msgs), logsPageSize)
	}
	if got := atomic.LoadInt32(reqs); got != 2 {
		t.Fatalf("requests = %d, want 2 (full page + empty page)", got)
	}
}

// Paging starts at the caller's `after`: the server must never see a request with
// after < 100 when RunLogs is called with after=100.
func TestHTTPClientRunLogsHonorsAfter(t *testing.T) {
	srv, _, minAfter := runLogsPagingServer(t, 450, 0)
	defer srv.Close()
	msgs, err := newTestClient(srv).RunLogs(context.Background(), "r1", 100)
	if err != nil {
		t.Fatalf("RunLogs: %v", err)
	}
	if len(msgs) != 350 { // seqs 101..450
		t.Fatalf("len = %d, want 350", len(msgs))
	}
	if msgs[0].Seq != 101 {
		t.Fatalf("first seq = %d, want 101", msgs[0].Seq)
	}
	if got := atomic.LoadInt32(minAfter); got < 100 {
		t.Fatalf("server saw after=%d, want never < 100", got)
	}
}

// A server may clamp ?limit= to its own maxRunMessagesPage, which could be BELOW
// logsPageSize. This one always returns at most 50 messages per page regardless of the
// requested 200, over a 300-message history. Stop-on-empty must still return ALL 300 in
// order: a stop-on-short loop would have taken the first 50-message page as terminal and
// silently truncated the history to 50 while returning a nil error — the exact partial
// -that-looks-complete this fix defeats.
func TestHTTPClientRunLogsRobustToClamp(t *testing.T) {
	const total = 300
	const serverCap = 50 // the server's own maxRunMessagesPage, below logsPageSize
	var reqs int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqs, 1)
		after, err := strconv.Atoi(r.URL.Query().Get("after"))
		if err != nil {
			t.Errorf("bad after: %q", r.URL.Query().Get("after"))
		}
		// The client asks for logsPageSize; the server ignores it and clamps to its own cap.
		if limit, _ := strconv.Atoi(r.URL.Query().Get("limit")); limit != logsPageSize {
			t.Errorf("client requested limit=%d, want %d", limit, logsPageSize)
		}
		var b strings.Builder
		b.WriteString(`{"messages":[`)
		count := 0
		for seq := after + 1; seq <= total && count < serverCap; seq++ {
			if count > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"seq":%d,"kind":"stdout","payload":{},"created_at":"2026-08-16T00:00:00Z"}`, seq)
			count++
		}
		b.WriteString(`]}`)
		_, _ = w.Write([]byte(b.String()))
	}))
	defer srv.Close()

	msgs, err := newTestClient(srv).RunLogs(context.Background(), "r1", 0)
	if err != nil {
		t.Fatalf("RunLogs: %v", err)
	}
	if len(msgs) != total {
		t.Fatalf("len = %d, want %d — stop-on-empty must defeat the server clamp, not truncate at the first short page", len(msgs), total)
	}
	for i, m := range msgs {
		if m.Seq != int32(i+1) {
			t.Fatalf("msg[%d].Seq = %d, want %d", i, m.Seq, i+1)
		}
	}
}

// A hostile server that ALWAYS returns a full logsPageSize page with strictly-increasing
// seqs never sends an empty page, so the stop-on-empty loop would run forever and grow
// the accumulator toward OOM. maxLogsMessages is the backstop: RunLogs must abort with a
// non-nil error and a nil/empty slice (all-or-nothing) once it crosses the cap. The cap
// is lowered here (and restored in a defer) so the test reaches it with a modest number
// of pages instead of accumulating a million messages.
func TestHTTPClientRunLogsBackstop(t *testing.T) {
	defer func(orig int) { maxLogsMessages = orig }(maxLogsMessages)
	maxLogsMessages = 500 // crossed after 3 full pages of 200

	var reqs int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqs, 1)
		after, err := strconv.Atoi(r.URL.Query().Get("after"))
		if err != nil {
			t.Errorf("bad after: %q", r.URL.Query().Get("after"))
		}
		// Always a full page with strictly-increasing seqs — never empty.
		var b strings.Builder
		b.WriteString(`{"messages":[`)
		for i := 0; i < logsPageSize; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"seq":%d,"kind":"stdout","payload":{},"created_at":"2026-08-16T00:00:00Z"}`, after+1+i)
		}
		b.WriteString(`]}`)
		_, _ = w.Write([]byte(b.String()))
	}))
	defer srv.Close()

	msgs, err := newTestClient(srv).RunLogs(context.Background(), "r1", 0)
	if err == nil {
		t.Fatalf("expected the backstop to abort, got nil error (msgs=%d)", len(msgs))
	}
	if len(msgs) != 0 {
		t.Fatalf("partial history leaked past the backstop: len = %d, want 0", len(msgs))
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("error = %q, want the backstop message", err.Error())
	}
	// It must abort promptly, not accumulate toward maxLogsMessages' real value: at
	// 200 per page and a cap of 500, three pages cross it.
	if got := atomic.LoadInt32(&reqs); got != 3 {
		t.Fatalf("requests = %d, want 3 (backstop crossed after 3 full pages)", got)
	}
}

// A malformed 200 body must fail cleanly with the decode exit code (1), never
// panic.
func TestHTTPClientMalformedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"user": not-json`))
	}))
	defer srv.Close()
	_, err := newTestClient(srv).Whoami(context.Background())
	if ExitCodeFor(err) != ExitGeneric {
		t.Fatalf("malformed: exit = %d, want %d (err=%v)", ExitCodeFor(err), ExitGeneric, err)
	}
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("malformed err is not *ExitError: %v", err)
	}
}

// TestHTTPClientStatusMapping pins the HTTP-status → exit-code contract.
func TestHTTPClientStatusMapping(t *testing.T) {
	cases := []struct {
		status int
		want   int
	}{
		{http.StatusBadRequest, ExitUsage},
		{http.StatusUnauthorized, ExitAuth},
		{http.StatusForbidden, ExitAuth},
		{http.StatusNotFound, ExitNotFound},
		{http.StatusConflict, ExitConflict},
		{http.StatusRequestEntityTooLarge, ExitUsage}, // oversize body keeps the 400's exit code across the 413 flip
		{http.StatusInternalServerError, ExitUnreachable},
		{http.StatusBadGateway, ExitUnreachable},
		{http.StatusTeapot, ExitGeneric}, // an unenumerated 4xx falls to generic
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"error":"x"}`))
		}))
		got := ExitCodeFor(func() error { _, e := newTestClient(srv).Whoami(context.Background()); return e }())
		srv.Close()
		if got != tc.want {
			t.Errorf("status %d: exit = %d, want %d", tc.status, got, tc.want)
		}
	}
}

// A 403 carries an actionable admin-scope hint (M7b).
func TestHTTPClient403AdminHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"admin required"}`))
	}))
	defer srv.Close()
	_, err := newTestClient(srv).AdminListUsers(context.Background())
	if ExitCodeFor(err) != ExitAuth {
		t.Fatalf("exit = %d, want %d", ExitCodeFor(err), ExitAuth)
	}
	if !strings.Contains(err.Error(), "admin-scoped token") {
		t.Errorf("403 message = %q, want an admin-scope hint", err.Error())
	}
}

// Reject a non-https, non-loopback base URL BEFORE any request is sent — the
// credential-leak guard. The token must never travel in cleartext to a foreign
// host.
func TestHTTPClientRejectsNonHTTPS(t *testing.T) {
	c := &HTTPClient{BaseURL: "http://uzi.example.com", Token: "uzc_secret", HTTP: http.DefaultClient}
	_, err := c.Whoami(context.Background())
	if ExitCodeFor(err) != ExitUsage {
		t.Fatalf("exit = %d, want %d", ExitCodeFor(err), ExitUsage)
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("message = %q, want an https guard message", err.Error())
	}
}

// A TLS endpoint that 302-redirects to plain http must NOT cause the client to
// replay the bearer token in cleartext. The shipped CheckRedirect refuses every
// redirect, so the http target is never contacted and the Authorization header
// can never leak. Mirrors the auditor's proof of the https→http scheme-downgrade
// token leak: without CheckRedirect, Go follows the hop and forwards Bearer over
// http. The https guard in newRequest only vets the INITIAL URL, not redirect
// hops — this is the layer that closes the gap.
func TestHTTPClientRefusesRedirect(t *testing.T) {
	var httpSawAuth atomic.Bool
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			httpSawAuth.Store(true)
		}
		_, _ = w.Write([]byte(`{"user":{"id":"leaked"}}`))
	}))
	defer plain.Close()

	// A TLS server that 302s every request down to the plain-http endpoint (same
	// loopback host, scheme downgraded https→http).
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/api/auth/me", http.StatusFound)
	}))
	defer tlsSrv.Close()

	// The SHIPPED client (its CheckRedirect), but trusting the test's TLS cert.
	c := NewHTTPClient(Settings{URL: tlsSrv.URL, Token: "uzc_secret"})
	c.HTTP.Transport = tlsSrv.Client().Transport

	_, err := c.Whoami(context.Background())
	if err == nil {
		t.Fatal("want an error when the API redirects (the hop must not be followed), got nil")
	}
	if ExitCodeFor(err) != ExitUnreachable {
		t.Errorf("redirect refusal exit = %d, want %d (transport failure)", ExitCodeFor(err), ExitUnreachable)
	}
	if httpSawAuth.Load() {
		t.Fatal("Authorization: Bearer leaked to the http redirect target in cleartext")
	}
}

func TestHTTPClientEmptyURL(t *testing.T) {
	c := &HTTPClient{BaseURL: "", Token: "uzc_x", HTTP: http.DefaultClient}
	_, err := c.Whoami(context.Background())
	if ExitCodeFor(err) != ExitUsage {
		t.Fatalf("empty URL: exit = %d, want %d", ExitCodeFor(err), ExitUsage)
	}
}

// TestHTTPClientOnlyReturnsExitError is the reviewer-mandated invariant: no
// method of the live client ever returns a bare error — every failure path
// (unreachable, 5xx, malformed body) is an *ExitError, so a raw error can never
// leak to ExitCodeFor and be misclassified.
func TestHTTPClientOnlyReturnsExitError(t *testing.T) {
	// Every method as a uniform func(*HTTPClient) error closure.
	calls := []struct {
		name string
		fn   func(*HTTPClient) error
	}{
		{"whoami", func(c *HTTPClient) error { _, e := c.Whoami(context.Background()); return e }},
		{"list-runs", func(c *HTTPClient) error { _, e := c.ListRuns(context.Background()); return e }},
		{"get-run", func(c *HTTPClient) error { _, e := c.GetRun(context.Background(), "r1"); return e }},
		{"run-logs", func(c *HTTPClient) error { _, e := c.RunLogs(context.Background(), "r1", 0); return e }},
		{"run-review", func(c *HTTPClient) error { _, _, e := c.RunReview(context.Background(), "r1"); return e }},
		{"run-inputs", func(c *HTTPClient) error { _, e := c.RunInputs(context.Background(), "r1"); return e }},
		{"list-workers", func(c *HTTPClient) error { _, e := c.ListWorkers(context.Background()); return e }},
		{"list-repos", func(c *HTTPClient) error { _, e := c.ListRepos(context.Background()); return e }},
		{"admin-users", func(c *HTTPClient) error { _, e := c.AdminListUsers(context.Background()); return e }},
		{"admin-runs", func(c *HTTPClient) error { _, e := c.AdminListRuns(context.Background()); return e }},
		{"admin-workers", func(c *HTTPClient) error { _, e := c.AdminListWorkers(context.Background()); return e }},
		{"admin-cli-tokens", func(c *HTTPClient) error { _, e := c.AdminListCLITokens(context.Background()); return e }},
		{"admin-usage", func(c *HTTPClient) error { _, e := c.AdminUsage(context.Background()); return e }},
		{"admin-rate-limits", func(c *HTTPClient) error { _, e := c.AdminRateLimits(context.Background()); return e }},
		{"start-cli-auth", func(c *HTTPClient) error { _, e := c.StartCLIAuth(context.Background(), "ch", "desc"); return e }},
		{"poll-cli-auth", func(c *HTTPClient) error { _, e := c.PollCLIAuth(context.Background(), "req", "ver"); return e }},
		{"create-run", func(c *HTTPClient) error {
			_, e := c.CreateRun(context.Background(), "p1", 7, nil, nil, false, nil)
			return e
		}},
		{"submit-run-input", func(c *HTTPClient) error {
			_, e := c.SubmitRunInput(context.Background(), "r1", "cancel", "", nil)
			return e
		}},
	}

	// Three failure surfaces: 5xx, malformed 200 body, and unreachable server.
	fiveHundred := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer fiveHundred.Close()
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("this is not json at all <<<"))
	}))
	defer garbage.Close()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // closed → connection refused

	surfaces := map[string]*HTTPClient{
		"5xx":         {BaseURL: fiveHundred.URL, Token: "uzc_x", HTTP: fiveHundred.Client()},
		"malformed":   {BaseURL: garbage.URL, Token: "uzc_x", HTTP: garbage.Client()},
		"unreachable": {BaseURL: deadURL, Token: "uzc_x", HTTP: &http.Client{}},
	}

	for _, call := range calls {
		for sName, c := range surfaces {
			err := call.fn(c)
			if err == nil {
				t.Errorf("%s/%s: want error, got nil", call.name, sName)
				continue
			}
			var ee *ExitError
			if !errors.As(err, &ee) {
				t.Errorf("%s/%s: err is not *ExitError: %T %v", call.name, sName, err, err)
			}
		}
	}
}

// TestBuildInfoIsGatedByCredentialSafeBase pins the property that makes the https
// guard REQUIRED on this call rather than merely consistent with the others.
//
// GET /api/version is an unauthenticated ROUTE, so it is tempting to argue the
// guard is defence-in-depth here and skip it. The argument is wrong, and the third
// arm below is why: newRequest attaches `Authorization: Bearer <token>` to EVERY
// request whose client holds one, this call included. The route needs no
// credential; the request carries one anyway. Skipping credentialSafeBase for
// BuildInfo would therefore put a real uzc_ token on the wire in cleartext the
// first time someone ran `uzi version --url http://…`.
//
// Nothing else pins this. Every `uzi version` command test uses FakeClient, which
// has no BaseURL and never reaches the guard — so the whole M4 suite would stay
// green if a future change built a bare http.Client for this one call.
//
// The loopback arm is what makes the refusal attributable: without it, a guard
// that blanket-refused http would pass the first arm while breaking every compose
// user.
func TestBuildInfoIsGatedByCredentialSafeBase(t *testing.T) {
	t.Run("non-loopback http is refused before any request is sent", func(t *testing.T) {
		c := NewHTTPClient(Settings{URL: "http://uzi.example.com", Token: "uzc_secret"})

		_, err := c.BuildInfo(context.Background())
		if err == nil {
			t.Fatal("want a refusal for a plain-http remote, got nil — the bearer token would have gone out in cleartext")
		}
		if ExitCodeFor(err) != ExitUsage {
			t.Errorf("exit = %d, want %d", ExitCodeFor(err), ExitUsage)
		}
		// The guard's own message, not just any error: a connection failure would
		// also be non-nil, and would mean the request WAS attempted.
		if !strings.Contains(err.Error(), "refusing to send credentials") {
			t.Errorf("error is not the credential guard's: %v", err)
		}
	})

	t.Run("loopback http is allowed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"version":"0.11.12","founded":"2026-07-03"}`))
		}))
		defer srv.Close()

		c := NewHTTPClient(Settings{URL: srv.URL, Token: "uzc_secret"})
		got, err := c.BuildInfo(context.Background())
		if err != nil {
			t.Fatalf("loopback http must be allowed (it is the compose stack): %v", err)
		}
		if got.Version != "0.11.12" {
			t.Errorf("version = %q, want %q", got.Version, "0.11.12")
		}
	})

	t.Run("the request carries the bearer token, which is why the guard is required", func(t *testing.T) {
		var gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"version":"0.11.12","founded":"2026-07-03"}`))
		}))
		defer srv.Close()

		c := NewHTTPClient(Settings{URL: srv.URL, Token: "uzc_SECRETTOKENVALUE"}) //nolint:gosec //gitleaks:allow // fake uzi CLI token fixture, never a real credential
		if _, err := c.BuildInfo(context.Background()); err != nil {
			t.Fatalf("BuildInfo: %v", err)
		}
		if gotAuth != "Bearer uzc_SECRETTOKENVALUE" {
			t.Fatalf("Authorization = %q, want the bearer token.\n"+
				"If this now fails because the token is NOT attached, the guard's justification has "+
				"changed and the doc comments on BuildInfo and Client.BuildInfo must be re-derived — "+
				"do not simply delete this arm.", gotAuth)
		}
	})
}

// TestCreateRunWireBodyOmitsAbsentWaitOnLimit asserts the tri-state on the WIRE, which
// is the only place it is actually decided.
//
// The command-level test proves the CLI resolves the flag to nil; this proves nil then
// produces a body with NO `wait_on_limit` key at all. Those are two different failures
// with the same visible symptom: a `map[string]any{"wait_on_limit": waitOnLimit}` built
// without a nil guard, or a non-pointer field, marshals `"wait_on_limit": null` or
// `false` — both of which the server reads as a decision the user never made, and
// neither of which the command-level test can see.
//
// Asserted on the RAW BYTES rather than by decoding into a struct: decoding into a
// *bool maps an explicit `null` and an absent key to the same nil, so it would agree
// with the broken implementation.
func TestCreateRunWireBodyOmitsAbsentWaitOnLimit(t *testing.T) {
	tr := true
	fa := false
	for _, tc := range []struct {
		name        string
		in          *bool
		wantPresent bool
		wantJSON    string
	}{
		{"absent omits the key entirely", nil, false, ""},
		{"explicit true", &tr, true, `"wait_on_limit":true`},
		{"explicit false is SENT, not omitted", &fa, true, `"wait_on_limit":false`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b := make([]byte, r.ContentLength)
				_, _ = io.ReadFull(r.Body, b)
				body = string(b)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"run":{"id":"r1","status":"queued"}}`))
			}))
			defer srv.Close()

			if _, err := newTestClient(srv).CreateRun(context.Background(), "p1", 42, tc.in, nil, false, nil); err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			if !strings.Contains(body, `"issue_iid":42`) {
				t.Errorf("body lost issue_iid: %s", body)
			}
			if got := strings.Contains(body, "wait_on_limit"); got != tc.wantPresent {
				t.Errorf("wait_on_limit present = %v, want %v; body = %s\n"+
					"An absent flag must send NO key — the server stamps the run from the owner's Settings "+
					"default when the field is missing, and reads null or false as an explicit decision.",
					got, tc.wantPresent, body)
			}
			if tc.wantJSON != "" && !strings.Contains(body, tc.wantJSON) {
				t.Errorf("body = %s, want it to contain %s", body, tc.wantJSON)
			}
		})
	}
}

// TestCreateRunWireBodyMrRework mirrors the wait_on_limit wire test for the per-run
// MR-rework override (PRD #841 M3): the tri-state `mr_rework_enabled` field must OMIT its
// key when nil (server inherits the account default), and SEND an explicit true/false
// otherwise — the same omitempty-on-a-pointer contract, asserted on raw bytes so an
// explicit `null` (which decoding would collapse into nil) is caught.
func TestCreateRunWireBodyMrRework(t *testing.T) {
	tr := true
	fa := false
	for _, tc := range []struct {
		name        string
		in          *bool
		wantPresent bool
		wantJSON    string
	}{
		{"absent omits the key entirely", nil, false, ""},
		{"explicit true", &tr, true, `"mr_rework_enabled":true`},
		{"explicit false is SENT, not omitted", &fa, true, `"mr_rework_enabled":false`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b := make([]byte, r.ContentLength)
				_, _ = io.ReadFull(r.Body, b)
				body = string(b)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"run":{"id":"r1","status":"queued"}}`))
			}))
			defer srv.Close()

			if _, err := newTestClient(srv).CreateRun(context.Background(), "p1", 42, nil, tc.in, false, nil); err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			if got := strings.Contains(body, "mr_rework_enabled"); got != tc.wantPresent {
				t.Errorf("mr_rework_enabled present = %v, want %v; body = %s\n"+
					"An absent flag must send NO key — the server stamps the run by inheriting the account "+
					"default when the field is missing, and reads null or false as an explicit decision.",
					got, tc.wantPresent, body)
			}
			if tc.wantJSON != "" && !strings.Contains(body, tc.wantJSON) {
				t.Errorf("body = %s, want it to contain %s", body, tc.wantJSON)
			}
		})
	}
}

// TestCreateRunWireBodyForce asserts the --force flag's wire mapping (issue #856). Unlike
// wait_on_limit/mr_rework_enabled, force is NOT tri-state: it is a plain bool with omitempty,
// so a false force (the default, and every non-forced create) must send NO `force` key —
// keeping a plain create byte-identical to before — while `force:true` (set only by --force)
// asks the server to bypass ONLY the open-MR guard. Asserted on raw bytes so an unwanted
// `"force":false` a decode-into-struct would erase is caught.
func TestCreateRunWireBodyForce(t *testing.T) {
	for _, tc := range []struct {
		name        string
		in          bool
		wantPresent bool
		wantJSON    string
	}{
		{"false omits the key entirely (byte-identical to a non-forced create)", false, false, ""},
		{"true is SENT", true, true, `"force":true`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b := make([]byte, r.ContentLength)
				_, _ = io.ReadFull(r.Body, b)
				body = string(b)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"run":{"id":"r1","status":"queued"}}`))
			}))
			defer srv.Close()

			if _, err := newTestClient(srv).CreateRun(context.Background(), "p1", 42, nil, nil, tc.in, nil); err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			if got := strings.Contains(body, "force"); got != tc.wantPresent {
				t.Errorf("force present = %v, want %v; body = %s\n"+
					"A false force must send NO key (omitempty) so a plain create stays byte-identical; "+
					"only --force sends force:true to bypass the open-MR guard.",
					got, tc.wantPresent, body)
			}
			if tc.wantJSON != "" && !strings.Contains(body, tc.wantJSON) {
				t.Errorf("body = %s, want it to contain %s", body, tc.wantJSON)
			}
		})
	}
}

// TestSetRunMrReworkWire asserts `SetRunMrRework` PUTs to /api/runs/{id}/mr-rework with an
// `enabled` field that carries the tri-state faithfully (PRD #841 M3). Unlike CreateRun's
// omit-when-nil, this endpoint's `enabled: null` is MEANINGFUL — it clears the override
// back to inherit — so the field has NO omitempty and nil must marshal as `"enabled":null`.
// Asserted on raw bytes so the null case is not collapsed by decoding.
func TestSetRunMrReworkWire(t *testing.T) {
	tr := true
	fa := false
	for _, tc := range []struct {
		name     string
		in       *bool
		wantJSON string
	}{
		{"explicit true", &tr, `"enabled":true`},
		{"explicit false", &fa, `"enabled":false`},
		{"nil clears to inherit (null, NOT omitted)", nil, `"enabled":null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotMethod, gotPath, body string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.Path
				b := make([]byte, r.ContentLength)
				_, _ = io.ReadFull(r.Body, b)
				body = string(b)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"run":{"id":"r1","status":"completed"}}`))
			}))
			defer srv.Close()

			run, err := newTestClient(srv).SetRunMrRework(context.Background(), "r1", tc.in)
			if err != nil {
				t.Fatalf("SetRunMrRework: %v", err)
			}
			if gotMethod != http.MethodPut || gotPath != "/api/runs/r1/mr-rework" {
				t.Errorf("request = %s %s, want PUT /api/runs/r1/mr-rework", gotMethod, gotPath)
			}
			if !strings.Contains(body, tc.wantJSON) {
				t.Errorf("body = %s, want it to contain %s", body, tc.wantJSON)
			}
			if run.ID != "r1" {
				t.Errorf("decoded run id = %q, want r1", run.ID)
			}
		})
	}
}

// TestCreateRunWireBodySeededPlan pins PRD #209's seed → wire mapping at the RAW body,
// which the command-level (fake-client) tests cannot see. The load-bearing case is the
// first: a nil seed must send NEITHER plan_md nor agent_selection, so a run created
// without --plan-file is byte-identical to a pre-#209 create (Success Criterion 2).
// Asserted on the bytes, not a decoded struct, because omitempty is precisely what a
// decode-into-pointer would erase.
func TestCreateRunWireBodySeededPlan(t *testing.T) {
	for _, tc := range []struct {
		name         string
		seed         *CreateRunSeed
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:       "nil seed omits every seed key (byte-identical to a pre-#209 create)",
			seed:       nil,
			wantAbsent: []string{"plan_md", "agent_selection", "planned_commit", "require_base"},
		},
		{
			name:         "plan, no roster/staleness: plan_md present, the rest omitted",
			seed:         &CreateRunSeed{PlanMD: "do the thing"},
			wantContains: []string{`"plan_md":"do the thing"`},
			wantAbsent:   []string{"agent_selection", "planned_commit", "require_base"},
		},
		{
			name: "plan + roster: both present, selection is {source, exclusions}",
			seed: &CreateRunSeed{
				PlanMD:    "do it",
				Selection: &apitypes.AgentSelection{Source: "own", Exclusions: []string{"tester"}},
			},
			wantContains: []string{`"plan_md":"do it"`, `"agent_selection":{"source":"own","exclusions":["tester"]}`},
			wantAbsent:   []string{"planned_commit", "require_base"},
		},
		{
			// PRD #209 M4: planned_commit + require_base ride the wire when set.
			name: "plan + staleness guard: planned_commit and require_base present",
			seed: &CreateRunSeed{
				PlanMD:        "do it",
				PlannedCommit: "abc123def456",
				RequireBase:   true,
			},
			wantContains: []string{`"planned_commit":"abc123def456"`, `"require_base":true`},
		},
		{
			// planned_commit alone (warn-only): the commit rides, require_base is omitted
			// (omitempty on a false bool), so a warn-default seed sends no require_base key.
			name: "plan + planned_commit only: require_base omitted (warn default)",
			seed: &CreateRunSeed{
				PlanMD:        "do it",
				PlannedCommit: "abc123def456",
			},
			wantContains: []string{`"planned_commit":"abc123def456"`},
			wantAbsent:   []string{"require_base"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b := make([]byte, r.ContentLength)
				_, _ = io.ReadFull(r.Body, b)
				body = string(b)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"run":{"id":"r1","status":"queued"}}`))
			}))
			defer srv.Close()

			if _, err := newTestClient(srv).CreateRun(context.Background(), "p1", 42, nil, nil, false, tc.seed); err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(body, want) {
					t.Errorf("body = %s, want it to contain %s", body, want)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(body, absent) {
					t.Errorf("body = %s, want it to OMIT %q (a nil/absent field must send no key)", body, absent)
				}
			}
		})
	}
}
