package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCreateRunInputPlanRevisionPending409(t *testing.T) {
	for _, selection := range []string{"null", `{"source":"own","exclusions":[]}`} {
		for _, override := range []bool{false, true} {
			for _, expected := range []string{"null", "7"} {
				t.Run(fmt.Sprintf("selection=%s/override=%t/revision=%s", selection, override, expected), func(t *testing.T) {
					owner := store.User{ID: uuid.New()}
					runID := uuid.New()
					st := gatedCapStore(owner, runID, store.Worker{ID: uuid.New()}, []string{"docker"})
					st.run.GateRevision = 7
					st.pendingRevision = true
					before := st.run
					h := newRunsHandler(t, st)

					rec := httptest.NewRecorder()
					body := fmt.Sprintf(`{"kind":"approve_plan","selection":%s,"override_capabilities":%t,"expected_gate_revision":%s}`, selection, override, expected)
					h.CreateRunInput(rec, inputReq(owner, runID, body))

					if rec.Code != http.StatusConflict {
						t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
					}
					var got struct {
						Error  string `json:"error"`
						Reason string `json:"reason"`
					}
					if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
						t.Fatal(err)
					}
					if got.Reason != "plan_revision_pending" {
						t.Fatalf("reason = %q, want plan_revision_pending", got.Reason)
					}
					for _, hint := range []string{"feedback must finish or be replaced", "cancel", "reject", "revision limit", "Approval cannot resume an expired revision"} {
						if !strings.Contains(got.Error, hint) {
							t.Errorf("owner message %q missing %q", got.Error, hint)
						}
					}
					if st.createdApproval != nil || st.clearedCapsArg != nil {
						t.Fatalf("refused approval wrote input or cleared capabilities: input=%+v clear=%+v", st.createdApproval, st.clearedCapsArg)
					}
					if !reflect.DeepEqual(st.run, before) {
						t.Fatalf("refused approval changed run: got %+v, want %+v", st.run, before)
					}
				})
			}
		}
	}
}
