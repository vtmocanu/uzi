package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// attestedPublishStore records the attestation each publish write carried (issue #3194).
type attestedPublishStore struct {
	*publishStore
	attempts []pgtype.Text
	persists []pgtype.Text
}

func (p *attestedPublishStore) RecordLiveCheckpointPublishAttempt(ctx context.Context, arg store.RecordLiveCheckpointPublishAttemptParams) (uuid.UUID, error) {
	p.attempts = append(p.attempts, arg.AttestedSource)
	return p.publishStore.RecordLiveCheckpointPublishAttempt(ctx, arg)
}

func (p *attestedPublishStore) SetRunCheckpointTip(_ context.Context, arg store.SetRunCheckpointTipParams) (int64, error) {
	p.persists = append(p.persists, arg.AttestedSource)
	return 1, nil
}

const (
	attestedSHA1   = "89abcdef0123456789abcdef0123456789abcdef"
	attestedSHA256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func TestWorkerRunPublishAttestedSourceMalformed400(t *testing.T) {
	for name, values := range map[string][]string{
		"empty":            {""},
		"short prefix":     {attestedSHA1[:12]},
		"39 hex":           {attestedSHA1[:39]},
		"41 hex":           {attestedSHA1 + "0"},
		"63 hex":           {attestedSHA256[:63]},
		"65 hex":           {attestedSHA256 + "0"},
		"uppercase":        {strings.ToUpper(attestedSHA1)},
		"padded":           {" " + attestedSHA1},
		"non hex":          {strings.Repeat("g", 40)},
		"duplicate header": {attestedSHA1, attestedSHA1},
	} {
		t.Run(name, func(t *testing.T) {
			h := newPublishHandler(t, &publishStore{}, newBox(t), func(string) bool { return true }, nil)
			req := publishReq(uuid.New(), validTip, publishUnreadBody{t})
			for _, v := range values {
				req.Header.Add("X-Uzi-Checkpoint-Attested-Source", v)
			}
			req.URL.RawQuery = "claim_generation=2"
			rec := httptest.NewRecorder()
			h.WorkerRunPublish(rec, req)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid attested source") {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestWorkerRunPublishAttestedSourceWire(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		query  string
		want   int
		stored pgtype.Text
	}{
		{"absent is unattested", "", "claim_generation=2", http.StatusOK, pgtype.Text{}},
		{"absent legacy without generation", "", "", http.StatusOK, pgtype.Text{}},
		{"sha1 source", attestedSHA1, "claim_generation=2", http.StatusOK, pgtype.Text{String: attestedSHA1, Valid: true}},
		{"sha256 source", attestedSHA256, "claim_generation=2", http.StatusOK, pgtype.Text{String: attestedSHA256, Valid: true}},
		// An attestation binds only to a request admitted at the run's current generation.
		{"attested without generation", attestedSHA1, "", http.StatusConflict, pgtype.Text{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := newBox(t)
			sealed, err := box.Seal([]byte("publish-test-pat"))
			if err != nil {
				t.Fatal(err)
			}
			st := &attestedPublishStore{publishStore: &publishStore{ownedRun: issueRun(42), claim: issueClaimRow(sealed)}}
			st.ownedRun.ClaimGeneration = 2
			calls := 0
			h := newPublishHandler(t, st, box, func(string) bool { return true }, func(context.Context, pushbroker.Options) (pushbroker.Result, error) {
				calls++
				return pushbroker.Result{Disposition: pushbroker.PublishAdvanced}, nil
			})
			req := publishReq(uuid.New(), validTip, strings.NewReader("pack"))
			if tc.header != "" {
				req.Header.Set("X-Uzi-Checkpoint-Attested-Source", tc.header)
			}
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
				if body["reason"] != "stale_claim" || calls != 0 || len(st.attempts) != 0 || len(st.persists) != 0 {
					t.Fatalf("response=%v broker calls=%d attempts=%v persists=%v", body, calls, st.attempts, st.persists)
				}
				return
			}
			if calls != 1 || len(st.attempts) == 0 || len(st.persists) != 1 {
				t.Fatalf("broker calls=%d attempts=%v persists=%v", calls, st.attempts, st.persists)
			}
			for i, got := range st.attempts {
				if got != tc.stored {
					t.Fatalf("attempt %d attested=%+v want %+v", i, got, tc.stored)
				}
			}
			if st.persists[0] != tc.stored {
				t.Fatalf("persisted attested=%+v want %+v", st.persists[0], tc.stored)
			}
		})
	}
}
