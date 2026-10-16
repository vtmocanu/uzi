package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type auditDetailStore struct {
	*runsStore
	calls  int
	before int64
}

func (s *auditDetailStore) ListRunMilestoneReconciliations(_ context.Context, p store.ListRunMilestoneReconciliationsParams) ([]store.ListRunMilestoneReconciliationsRow, error) {
	s.calls++
	s.before = p.BeforeGateRevision
	rows := []store.ListRunMilestoneReconciliationsRow{}
	for revision := int64(60); revision > 0; revision-- {
		if p.BeforeGateRevision > 0 && revision >= p.BeforeGateRevision {
			continue
		}
		rows = append(rows, store.ListRunMilestoneReconciliationsRow{ID: uuid.New(), RunID: s.run.ID, OwnerID: s.ownerID, GateRevision: revision, Reason: "reset", Consent: []byte("{}"), BeforeState: []byte("{}"), AfterState: []byte("{}")})
	}
	return rows, nil
}
func TestGetRunReconciliationBoundedRead(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &auditDetailStore{runsStore: &runsStore{ownerID: owner.ID, run: store.Run{ID: runID, UserID: owner.ID, Status: "completed"}}}
	h := newRunsHandler(t, st)
	for _, query := range []string{"", "reconciliation_before_gate_revision=11"} {
		rec := httptest.NewRecorder()
		h.GetRun(rec, runReqQuery(owner, runID, query))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Run apitypes.RunDTO `json:"run"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if query == "" {
			if len(body.Run.MilestoneReconciliations) != 50 || body.Run.MilestoneReconciliationsNextBeforeGateRevision == nil || *body.Run.MilestoneReconciliationsNextBeforeGateRevision != 11 {
				t.Fatal("first page/cursor missing")
			}
		} else if len(body.Run.MilestoneReconciliations) != 10 || body.Run.MilestoneReconciliationsNextBeforeGateRevision != nil || st.before != 11 {
			t.Fatal("older page/cursor incorrect")
		}
	}
	for _, query := range []string{"reconciliation_before_gate_revision=", "reconciliation_before_gate_revision=0", "reconciliation_before_gate_revision=-1", "reconciliation_before_gate_revision=9223372036854775808", "reconciliation_before_gate_revision=no", "reconciliation_before_gate_revision=1&reconciliation_before_gate_revision=2"} {
		calls := st.calls
		rec := httptest.NewRecorder()
		h.GetRun(rec, runReqQuery(owner, runID, query))
		if rec.Code != http.StatusBadRequest || st.calls != calls {
			t.Fatalf("invalid cursor accepted: %q %d", query, rec.Code)
		}
	}
	calls := st.calls
	for _, user := range []store.User{{ID: uuid.New()}, {ID: uuid.New(), IsAdmin: true}} {
		rec := httptest.NewRecorder()
		h.GetRun(rec, runReqQuery(user, runID, "reconciliation_before_gate_revision=invalid"))
		want := http.StatusNotFound
		if user.IsAdmin {
			want = http.StatusOK
		}
		if rec.Code != want || st.calls != calls {
			t.Fatalf("foreign read: %d want %d calls %d", rec.Code, want, st.calls)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if run, ok := body["run"].(map[string]any); ok {
			for _, key := range []string{"milestone_reconciliations", "milestone_reconciliations_next_before_gate_revision"} {
				if _, exists := run[key]; exists {
					t.Fatalf("foreign DTO exposes %s", key)
				}
			}
		}
	}
}
