package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

// A malformed query must be rejected without consuming any pack bytes.
type publishUnreadBody struct{ t *testing.T }

func (b publishUnreadBody) Read([]byte) (int, error) {
	b.t.Fatal("malformed claim_generation consumed the pack")
	return 0, nil
}

func TestWorkerRunPublishClaimGenerationMalformed(t *testing.T) {
	for _, query := range []string{"claim_generation=", "claim_generation=0", "claim_generation=-1", "claim_generation=nope", "claim_generation=9223372036854775808", "claim_generation=1&claim_generation=2"} {
		t.Run(query, func(t *testing.T) {
			h := newPublishHandler(t, &publishStore{}, newBox(t), func(string) bool { return true }, nil)
			req := publishReq(uuid.New(), validTip, publishUnreadBody{t})
			req.URL.RawQuery = query
			rec := httptest.NewRecorder()
			h.WorkerRunPublish(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestWorkerRunPublishClaimGenerationWire(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  int
	}{
		{"legacy omission", "", http.StatusOK},
		{"current generation", "claim_generation=2", http.StatusOK},
		{"old generation", "claim_generation=1", http.StatusConflict},
		{"future generation", "claim_generation=3", http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := newBox(t)
			sealed, err := box.Seal([]byte("publish-test-pat"))
			if err != nil {
				t.Fatal(err)
			}
			st := &publishStore{ownedRun: issueRun(42), claim: issueClaimRow(sealed)}
			st.ownedRun.ClaimGeneration = 2
			calls := 0
			h := newPublishHandler(t, st, box, func(string) bool { return true }, func(context.Context, pushbroker.Options) (pushbroker.Result, error) {
				calls++
				return pushbroker.Result{}, nil
			})
			req := publishReq(uuid.New(), validTip, strings.NewReader("pack"))
			req.URL.RawQuery = tc.query
			rec := httptest.NewRecorder()
			h.WorkerRunPublish(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusConflict {
				var body map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if body["reason"] != "stale_claim" || calls != 0 || st.livePublishAttemptID != uuid.Nil {
					t.Fatalf("stale response=%v broker calls=%d attempt=%v", body, calls, st.livePublishAttemptID)
				}
			} else if calls != 1 {
				t.Fatalf("current/legacy broker calls=%d", calls)
			}
		})
	}
}
