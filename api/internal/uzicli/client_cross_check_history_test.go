package uzicli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestCrossCheckHistoryQueryAndSinglePage(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts CrossCheckHistoryOptions
		want map[string][]string
	}{
		{
			name: "defaults",
			want: map[string][]string{
				"lifecycle": {"active"}, "stage": {"all"}, "outcome": {"any"},
				"limit": {"25"}, "attempt_limit": {"10"},
			},
		},
		{
			name: "group encoding",
			opts: CrossCheckHistoryOptions{Lifecycle: "past", Stage: "plan", Outcome: "passed", Cursor: "opaque+/=&?", Limit: 3, AttemptLimit: 2},
			want: map[string][]string{
				"lifecycle": {"past"}, "stage": {"plan"}, "outcome": {"passed"},
				"limit": {"3"}, "attempt_limit": {"2"}, "cursor": {"opaque+/=&?"},
			},
		},
		{
			name: "batched older histories",
			opts: CrossCheckHistoryOptions{AttemptCursors: []string{"first+/=", "second?&="}},
			want: map[string][]string{
				"lifecycle": {"active"}, "stage": {"all"}, "outcome": {"any"},
				"limit": {"25"}, "attempt_limit": {"10"}, "attempt_cursor": {"first+/=", "second?&="},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			want := apitypes.CrossCheckHistoryDTO{
				NextCursor: strings.Repeat("n", 512),
				Groups: []apitypes.CrossCheckHistoryGroupDTO{{
					ParentID: "parent", OlderCursor: strings.Repeat("o", 256),
					Attempts: []apitypes.CrossCheckHistoryAttemptDTO{{Stage: "plan", Reason: "untouched\n\t\u202e"}},
				}},
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodGet || r.URL.Path != "/api/cross-checks" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				if got := map[string][]string(r.URL.Query()); !reflect.DeepEqual(got, tc.want) {
					t.Errorf("query: %#v; want %#v", got, tc.want)
				}
				_ = json.NewEncoder(w).Encode(want)
			}))
			defer srv.Close()
			var client Client = newTestClient(srv)
			got, err := client.CrossCheckHistory(context.Background(), tc.opts)
			if err != nil || !reflect.DeepEqual(got, want) || calls != 1 {
				t.Fatalf("page mismatch or extra request: err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestCrossCheckHistoryValidationBeforeHTTP(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls++
	}))
	defer srv.Close()
	bad := []CrossCheckHistoryOptions{
		{Lifecycle: "unknown"}, {Stage: "unknown"}, {Outcome: "unknown"},
		{Outcome: "passed"}, {Outcome: "didnt_finish"}, {Outcome: "raised_issues"}, {Outcome: "replaced"},
		{Limit: -1}, {Limit: 26}, {AttemptLimit: 1}, {AttemptLimit: -1}, {AttemptLimit: 21},
		{Cursor: "fresh", AttemptCursors: []string{"older"}},
		{Cursor: strings.Repeat("x", 513)},
		{Cursor: strings.Repeat("+", 171)},
		{AttemptCursors: []string{""}},
		{AttemptCursors: []string{"same", "same"}},
		{AttemptCursors: []string{strings.Repeat("x", 257)}},
		{AttemptCursors: []string{strings.Repeat("+", 86)}},
		{AttemptCursors: []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"}},
	}
	for i, opts := range bad {
		_, err := newTestClient(srv).CrossCheckHistory(context.Background(), opts)
		if ExitCodeFor(err) != ExitUsage {
			t.Errorf("case %d: expected usage error, got %v", i, err)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid queries made %d HTTP calls", calls)
	}
}

func TestCrossCheckHistoryEnvelopeAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		code   int
	}{
		{"owner refusal", 404, "{\"error\":\"private owner detail\",\"secret\":true}", ExitNotFound},
		{"malformed", 200, "{\"private\":", ExitGeneric},
		{"oversized", 200, strings.Repeat(" ", (2<<20)+1), ExitGeneric},
		{"full envelope", 200, "{\"groups\":[],\"next_cursor\":\"next\"}" + strings.Repeat(" ", (2<<20)-34), ExitOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			_, err := newTestClient(srv).CrossCheckHistory(context.Background(), CrossCheckHistoryOptions{})
			if ExitCodeFor(err) != tc.code || calls != 1 {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
			if err != nil && strings.Contains(err.Error(), "private") {
				t.Fatal("structured server error leaked")
			}
		})
	}
}

func TestCrossCheckHistoryContextAndFake(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls++
	}))
	defer srv.Close()
	_, err := newTestClient(srv).CrossCheckHistory(ctx, CrossCheckHistoryOptions{})
	if ExitCodeFor(err) != ExitUnreachable || calls != 0 {
		t.Fatalf("canceled request: %v calls=%d", err, calls)
	}
	sentinel := errors.New("canned error")
	f := &FakeClient{Err: sentinel}
	var client Client = f
	_, err = client.CrossCheckHistory(ctx, CrossCheckHistoryOptions{})
	if !errors.Is(err, sentinel) || f.LastCrossCheckHistoryCtx != ctx || len(f.CrossCheckHistoryCalls) != 1 {
		t.Fatal("fake lost context, error or calls")
	}
	f.CrossCheckHistoryErr = errors.New("specific")
	_, err = client.CrossCheckHistory(ctx, CrossCheckHistoryOptions{})
	if err != f.CrossCheckHistoryErr {
		t.Fatal("specific error did not override shared error")
	}
	f.Err, f.CrossCheckHistoryErr = nil, nil
	page, err := client.CrossCheckHistory(ctx, CrossCheckHistoryOptions{})
	if err != nil || page.Groups == nil {
		t.Fatal("fake empty page must contain an empty groups array")
	}
}

func TestCrossCheckHistoryCursorByteBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts CrossCheckHistoryOptions
		code int
	}{
		{"group raw limit", CrossCheckHistoryOptions{Cursor: strings.Repeat("x", 512)}, ExitOK},
		{"group raw over limit", CrossCheckHistoryOptions{Cursor: strings.Repeat("x", 513)}, ExitUsage},
		{"group large raw value", CrossCheckHistoryOptions{Cursor: strings.Repeat("+", 1<<20)}, ExitUsage},
		{"group multibyte raw over limit", CrossCheckHistoryOptions{Cursor: strings.Repeat("界", 171)}, ExitUsage},
		{"group escaped limit", CrossCheckHistoryOptions{Cursor: strings.Repeat("+", 170) + "xx"}, ExitOK},
		{"group escaped over limit", CrossCheckHistoryOptions{Cursor: strings.Repeat("+", 171)}, ExitUsage},
		{"attempt raw limit", CrossCheckHistoryOptions{AttemptCursors: []string{strings.Repeat("x", 256)}}, ExitOK},
		{"attempt raw over limit", CrossCheckHistoryOptions{AttemptCursors: []string{strings.Repeat("x", 257)}}, ExitUsage},
		{"attempt large raw value", CrossCheckHistoryOptions{AttemptCursors: []string{strings.Repeat("+", 1<<20)}}, ExitUsage},
		{"attempt multibyte raw over limit", CrossCheckHistoryOptions{AttemptCursors: []string{strings.Repeat("界", 86)}}, ExitUsage},
		{"attempt escaped limit", CrossCheckHistoryOptions{AttemptCursors: []string{strings.Repeat("+", 85) + "x"}}, ExitOK},
		{"attempt escaped over limit", CrossCheckHistoryOptions{AttemptCursors: []string{strings.Repeat("+", 86)}}, ExitUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if tc.opts.Cursor != "" && r.URL.Query().Get("cursor") != tc.opts.Cursor {
					t.Error("opaque group cursor changed")
				}
				if len(tc.opts.AttemptCursors) > 0 && !reflect.DeepEqual(r.URL.Query()["attempt_cursor"], tc.opts.AttemptCursors) {
					t.Error("opaque attempt cursors changed")
				}
				_, _ = w.Write([]byte("{\"groups\":[]}"))
			}))
			defer srv.Close()
			_, err := newTestClient(srv).CrossCheckHistory(context.Background(), tc.opts)
			if ExitCodeFor(err) != tc.code {
				t.Fatalf("expected code=%d, got err=%v", tc.code, err)
			}
			wantCalls := 0
			if tc.code == ExitOK {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("HTTP calls=%d; want %d", calls, wantCalls)
			}
		})
	}
}
