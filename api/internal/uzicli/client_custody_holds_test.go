package uzicli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHTTPClientDeleteWorkerCustodyBlockingHolds pins how the custody 409 exposes the
// server's capped blocking-hold listing and authoritative total, including the older-server
// shapes that omit them. The trigger for the typed error stays `custody_holds` alone.
func TestHTTPClientDeleteWorkerCustodyBlockingHolds(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantTotal int64
		wantList  []WorkerBlockingHold
	}{
		{
			name:      "list and total",
			body:      `{"error":"custody","custody_holds":2,"blocking_holds":[{"hold_id":"h1","run_id":"r1","generation":3,"kind":"decision_needed","title":"Fix login"}],"blocking_holds_total":7}`,
			wantTotal: 7,
			wantList:  []WorkerBlockingHold{{HoldID: "h1", RunID: "r1", Generation: 3, Kind: "decision_needed", Title: "Fix login"}},
		},
		{
			name:      "older server omits both",
			body:      `{"error":"custody","custody_holds":4}`,
			wantTotal: 4,
			wantList:  []WorkerBlockingHold{},
		},
		{
			name:      "list without total falls back to custody_holds",
			body:      `{"error":"custody","custody_holds":2,"blocking_holds":[{"hold_id":"h1","run_id":"r1","generation":1,"kind":"k","title":""}]}`,
			wantTotal: 2,
			wantList:  []WorkerBlockingHold{{HoldID: "h1", RunID: "r1", Generation: 1, Kind: "k"}},
		},
		{
			name:      "null list is normalised to empty",
			body:      `{"error":"custody","custody_holds":1,"blocking_holds":null,"blocking_holds_total":1}`,
			wantTotal: 1,
			wantList:  []WorkerBlockingHold{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			err := newTestClient(srv).DeleteWorker(context.Background(), "w1")
			var custody *WorkerCustodyConflictError
			if !errors.As(err, &custody) {
				t.Fatalf("err = %v (%T), want *WorkerCustodyConflictError", err, err)
			}
			if custody.BlockingHolds == nil {
				t.Fatalf("BlockingHolds must be non-nil")
			}
			if custody.BlockingHoldsTotal != tc.wantTotal {
				t.Errorf("BlockingHoldsTotal = %d, want %d", custody.BlockingHoldsTotal, tc.wantTotal)
			}
			if len(custody.BlockingHolds) != len(tc.wantList) {
				t.Fatalf("BlockingHolds = %+v, want %+v", custody.BlockingHolds, tc.wantList)
			}
			for i := range tc.wantList {
				if custody.BlockingHolds[i] != tc.wantList[i] {
					t.Errorf("BlockingHolds[%d] = %+v, want %+v", i, custody.BlockingHolds[i], tc.wantList[i])
				}
			}
			if ExitCodeFor(err) != ExitConflict {
				t.Errorf("ExitCodeFor = %d, want %d", ExitCodeFor(err), ExitConflict)
			}
		})
	}
}
