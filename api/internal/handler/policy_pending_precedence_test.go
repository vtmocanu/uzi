package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Exercise the real detail handler and heartbeat tracker together: two facts in
// one current-owner report must retain the actionable permanent refusal.
func TestGetRunPolicyPendingPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		blocked  string
		policy   bool
		foreign  bool
		assigned bool
		want     string
	}{
		{"permanent wins over policy", "gap_unrecoverable", true, false, true, "gap_unrecoverable"},
		{"policy only remains transient", "", true, false, true, "codex_policy_hold_pending"},
		{"permanent only remains visible", "gap_unrecoverable", false, false, true, "gap_unrecoverable"},
		{"foreign both facts ignored", "gap_unrecoverable", true, true, true, ""},
		{"foreign policy ignored", "", true, true, true, ""},
		{"unassigned both facts ignored", "gap_unrecoverable", true, false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := store.User{ID: uuid.New()}
			worker := uuid.New()
			runID := uuid.New()
			st := &runsStore{ownerID: owner.ID, run: store.Run{
				ID: runID, UserID: owner.ID, Status: "running",
				WorkerID: pgtype.UUID{Bytes: worker, Valid: tc.assigned},
			}}
			h := newRunsHandler(t, st)
			reporter := worker
			if tc.foreign {
				reporter = uuid.New()
			}
			entry := workersvc.OutboxEntry{
				RunID: runID, PendingTerminal: 1, BlockedReason: tc.blocked, Since: time.Now(),
			}
			if tc.policy {
				entry.PolicyPendingOutcomeID = uuid.New()
				entry.PolicyPendingSourceGeneration = 2
			}
			if _, err := h.wsvc.Heartbeat(context.Background(), store.Worker{ID: reporter}, nil,
				[]workersvc.OutboxEntry{entry}, nil); err != nil {
				t.Fatalf("seed heartbeat: %v", err)
			}
			rec := httptest.NewRecorder()
			h.GetRun(rec, runReq(owner, runID))
			if rec.Code != http.StatusOK {
				t.Fatalf("GetRun = %d: %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Run struct {
					OutcomePending *struct {
						Reason string `json:"reason"`
					} `json:"outcome_pending"`
				} `json:"run"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode detail: %v", err)
			}
			if tc.want == "" {
				if body.Run.OutcomePending != nil {
					t.Fatalf("outcome_pending = %+v, want null", body.Run.OutcomePending)
				}
			} else if body.Run.OutcomePending == nil || body.Run.OutcomePending.Reason != tc.want {
				t.Fatalf("outcome_pending = %+v, want reason %q", body.Run.OutcomePending, tc.want)
			}
		})
	}
}
