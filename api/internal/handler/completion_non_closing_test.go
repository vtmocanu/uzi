package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type nonClosingProtocolStore struct {
	protocolStore
	attempts []store.RecordCompletionAttemptParams
}

func (p *nonClosingProtocolStore) RecordCompletionAttempt(_ context.Context, arg store.RecordCompletionAttemptParams) (int32, error) {
	p.attempts = append(p.attempts, arg)
	return 1, nil
}
func TestWorkerCompletionAttemptNonClosingJSON(t *testing.T) {
	for _, tc := range []struct {
		body    string
		status  int
		reason  string
		present bool
	}{
		{`{}`, 200, "", false},
		{`{"non_closing":{"reason":"\u0000  😀 \u0000"}}`, 200, "😀", true},
		{`{"non_closing":null}`, 400, "", false},
		{`{"non_closing":{}}`, 400, "", false},
		{`{"non_closing":{"reason":null}}`, 400, "", false},
		{`{"non_closing":{"reason":4}}`, 400, "", false},
		{`{"non_closing":{"reason":" "}}`, 400, "", false},
		{`{"non_closing":{"reason":"ok","unknown":1}}`, 400, "", false},
		{`{"non_closing":[]}`, 400, "", false},
		{`{"non_closing":"ok"}`, 400, "", false},
		{`{"non_closing":{"reason":"ok"},"unknown":1}`, 400, "", false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			id := uuid.New()
			st := &nonClosingProtocolStore{protocolStore: protocolStore{ownedRun: store.Run{
				ID: id, Kind: "issue", Status: "running", CompletionContractVersion: pgtype.Int4{Int32: 1, Valid: true},
			}}}
			h := newProtocolHandler(t, st)
			rec := httptest.NewRecorder()
			h.WorkerRunCompletionAttempt(rec, workerReq(http.MethodPost, tc.body, id))
			if rec.Code != tc.status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
			if tc.status == 400 {
				if len(st.attempts) != 0 {
					t.Fatal("invalid input wrote an attempt")
				}
			} else {
				if len(st.attempts) != 1 {
					t.Fatalf("attempts=%v", st.attempts)
				}
				got := st.attempts[0].CompletionNonClosingReason
				if got.Valid != tc.present || got.String != tc.reason {
					t.Fatalf("reason=%+v", got)
				}
			}
		})
	}
}
