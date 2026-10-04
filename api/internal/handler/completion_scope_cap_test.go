package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type scopeCapProtocolStore struct{ protocolStore }

func (p *scopeCapProtocolStore) RecordCompletionAttempt(context.Context, store.RecordCompletionAttemptParams) (int32, error) {
	return 1, nil
}
func (p *scopeCapProtocolStore) UpsertCompletionPermit(_ context.Context, arg store.UpsertCompletionPermitParams) (store.RunCompletionPermit, error) {
	return store.RunCompletionPermit{ID: uuid.New(), RunID: arg.RunID, ContractRevision: arg.ContractRevision, Branch: arg.Branch, Head: arg.Head}, nil
}

func TestWorkerCompletionPermitScopeCapJSON(t *testing.T) {
	for _, tc := range []struct{ extra, reason string }{
		{",\"scope_capped\":true", ""},
		{",\"scope_capped\":false", "missing_milestones"},
		{"", "missing_milestones"},
	} {
		t.Run(tc.extra, func(t *testing.T) {
			id := uuid.New()
			st := &scopeCapProtocolStore{protocolStore: protocolStore{ownedRun: store.Run{
				ID: id, Status: "running",
				CompletionContractVersion: pgtype.Int4{Int32: 1, Valid: true},
				ContractRevision:          pgtype.Int4{Int32: 1, Valid: true},
				ScopeCeiling:              pgtype.Int4{Int32: 1, Valid: true},
				MilestonesFrozen:          []byte(`[{"id":"m1","title":"m1"},{"id":"m2","title":"m2"}]`),
				MilestonesCompleted:       []byte(`["m1"]`),
				CompletionContract:        []byte(`{"profile":"structural","revision":1,"criteria":[{"id":"m1.c1","milestone_id":"m1","text":"m1","audit":null,"finding_ids":[]},{"id":"m2.c1","milestone_id":"m2","text":"m2","audit":null,"finding_ids":[]}]}`),
			}}}
			h := newProtocolHandler(t, st)
			rec := httptest.NewRecorder()
			h.WorkerRunCompletionPermit(rec, workerReq(http.MethodPost, `{"contract_revision":1,"branch":"b","head":"h"`+tc.extra+"}", id))
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
			if tc.reason == "" {
				if !strings.Contains(rec.Body.String(), `"granted":true`) {
					t.Fatal(rec.Body.String())
				}
			} else if !strings.Contains(rec.Body.String(), tc.reason) {
				t.Fatal(rec.Body.String())
			}
		})
	}
}
