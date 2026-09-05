package uzicli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// runLogsPageServer records the raw query of every /messages request it sees, so a
// test can assert BOTH the exact query string a valid form produces AND that a
// forbidden combination reaches the server zero times (validated client-side first).
func runLogsPageServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		writeJSON(w, map[string]any{"messages": []apitypes.MessageDTO{{Seq: 1, Kind: "text"}}})
	}))
	t.Cleanup(srv.Close)
	return srv, &queries
}

func TestHTTPClientRunLogsPageQueryStrings(t *testing.T) {
	cases := []struct {
		name string
		q    LogsPageQuery
		want string
	}{
		{"tail", LogsPageQuery{Tail: 5}, "tail=5"},
		{"before+limit", LogsPageQuery{Before: 4, Limit: 2}, "before=4&limit=2"},
		{"after+limit", LogsPageQuery{After: 3, Limit: 2}, "after=3&limit=2"},
		{"tail+payload_max", LogsPageQuery{Tail: 5, PayloadMax: 2048}, "payload_max=2048&tail=5"},
		{"before+limit+payload_max", LogsPageQuery{Before: 4, Limit: 2, PayloadMax: 64}, "before=4&limit=2&payload_max=64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, queries := runLogsPageServer(t)
			if _, err := newTestClient(srv).RunLogsPage(context.Background(), testRunID, tc.q); err != nil {
				t.Fatalf("RunLogsPage: %v", err)
			}
			if len(*queries) != 1 {
				t.Fatalf("server saw %d requests, want 1", len(*queries))
			}
			if got := (*queries)[0]; got != tc.want {
				t.Errorf("query = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHTTPClientRunLogsPageForbiddenCombosMakeNoRequest(t *testing.T) {
	bad := []struct {
		name string
		q    LogsPageQuery
	}{
		{"tail+after", LogsPageQuery{Tail: 5, After: 3}},
		{"tail+before", LogsPageQuery{Tail: 5, Before: 4}},
		{"tail+limit", LogsPageQuery{Tail: 5, Limit: 2}},
		{"before+after", LogsPageQuery{Before: 4, After: 3, Limit: 2}},
		{"before-without-limit", LogsPageQuery{Before: 4}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			srv, queries := runLogsPageServer(t)
			_, err := newTestClient(srv).RunLogsPage(context.Background(), testRunID, tc.q)
			if got := ExitCodeFor(err); got != ExitUsage {
				t.Errorf("exit code = %d, want ExitUsage(%d) (err=%v)", got, ExitUsage, err)
			}
			if len(*queries) != 0 {
				t.Errorf("server saw %d requests on a forbidden combo, want 0", len(*queries))
			}
		})
	}
}

func TestFakeRunLogsPageRecordsAndFilters(t *testing.T) {
	seed := []apitypes.MessageDTO{
		{Seq: 1, Kind: "text"}, {Seq: 2, Kind: "tool_use"},
		{Seq: 3, Kind: "text"}, {Seq: 4, Kind: "tool_use"}, {Seq: 5, Kind: "text"},
	}
	seqsOf := func(ms []apitypes.MessageDTO) []int32 {
		out := make([]int32, len(ms))
		for i, m := range ms {
			out[i] = m.Seq
		}
		return out
	}
	eq := func(a []int32, b ...int32) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	t.Run("windows", func(t *testing.T) {
		f := &FakeClient{LogsByID: map[string][]apitypes.MessageDTO{testRunID: seed}}
		tail, err := f.RunLogsPage(context.Background(), testRunID, LogsPageQuery{Tail: 2})
		if err != nil {
			t.Fatalf("tail: %v", err)
		}
		if got := seqsOf(tail); !eq(got, 4, 5) {
			t.Errorf("tail=2 seqs = %v, want [4 5]", got)
		}
		before, err := f.RunLogsPage(context.Background(), testRunID, LogsPageQuery{Before: 4, Limit: 2})
		if err != nil {
			t.Fatalf("before: %v", err)
		}
		if got := seqsOf(before); !eq(got, 2, 3) {
			t.Errorf("before=4&limit=2 seqs = %v, want [2 3]", got)
		}
		after, err := f.RunLogsPage(context.Background(), testRunID, LogsPageQuery{After: 3})
		if err != nil {
			t.Fatalf("after: %v", err)
		}
		if got := seqsOf(after); !eq(got, 4, 5) {
			t.Errorf("after=3 seqs = %v, want [4 5]", got)
		}
		afterCapped, err := f.RunLogsPage(context.Background(), testRunID, LogsPageQuery{After: 0, Limit: 2})
		if err != nil {
			t.Fatalf("after+limit: %v", err)
		}
		if got := seqsOf(afterCapped); !eq(got, 1, 2) {
			t.Errorf("after=0&limit=2 seqs = %v, want [1 2]", got)
		}
		// Every valid call was recorded, in order.
		if len(f.RunLogsPageCalls) != 4 {
			t.Fatalf("RunLogsPageCalls len = %d, want 4", len(f.RunLogsPageCalls))
		}
		if f.RunLogsPageCalls[0] != (LogsPageQuery{Tail: 2}) {
			t.Errorf("call[0] = %+v, want {Tail:2}", f.RunLogsPageCalls[0])
		}
		if f.RunLogsPageCalls[1] != (LogsPageQuery{Before: 4, Limit: 2}) {
			t.Errorf("call[1] = %+v, want {Before:4,Limit:2}", f.RunLogsPageCalls[1])
		}
	})

	t.Run("forbidden combo not recorded", func(t *testing.T) {
		f := &FakeClient{LogsByID: map[string][]apitypes.MessageDTO{testRunID: seed}}
		_, err := f.RunLogsPage(context.Background(), testRunID, LogsPageQuery{Tail: 2, After: 1})
		if got := ExitCodeFor(err); got != ExitUsage {
			t.Errorf("exit code = %d, want ExitUsage(%d)", got, ExitUsage)
		}
		if len(f.RunLogsPageCalls) != 0 {
			t.Errorf("forbidden combo recorded %d calls, want 0", len(f.RunLogsPageCalls))
		}
	})
}
