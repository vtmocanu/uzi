package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestHarnessDecisionSharedBoardAbsent(t *testing.T) {
	typ := reflect.TypeOf(latestRunDTO{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if strings.Split(field.Tag.Get("json"), ",")[0] == "harness_decision" || field.Type == reflect.TypeOf((*apitypes.HarnessDecisionDTO)(nil)) {
			t.Fatalf("shared board exposes private harness decision via %s", field.Name)
		}
	}
}

func TestHarnessDecisionGetRun(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	for _, state := range []string{"legacy", "unknown", "zero"} {
		t.Run(state, func(t *testing.T) {
			run := store.Run{ID: uuid.New(), UserID: owner.ID, Status: "queued", Harness: "codex"}
			if state != "legacy" {
				run.HarnessPreferred = txt("claude")
				run.HarnessDecisionReason = txt("unknown_usage")
				run.HarnessPreferredHeld = pgtype.Bool{Valid: true}
				run.HarnessOtherHeld = pgtype.Bool{Valid: true}
			}
			if state == "zero" {
				run.HarnessDecisionReason = txt("preferred")
				run.HarnessPreferredScore = pgtype.Float8{Valid: true}
				run.HarnessOtherScore = pgtype.Float8{Valid: true}
				run.HarnessPreferredMeasured = pgtype.Float8{Valid: true}
				run.HarnessOtherMeasured = pgtype.Float8{Valid: true}
			}
			h := newRunsHandler(t, &runsStore{ownerID: owner.ID, run: run})
			for _, viewer := range []store.User{owner, {ID: uuid.New(), IsAdmin: true}} {
				rec := httptest.NewRecorder()
				h.GetRun(rec, runReq(viewer, run.ID))
				if rec.Code != http.StatusOK {
					t.Fatalf("GetRun=%d %s", rec.Code, rec.Body)
				}
				var body struct {
					Run apitypes.RunDTO `json:"run"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				d := body.Run.HarnessDecision
				if state == "legacy" || viewer.ID != owner.ID {
					if d != nil {
						t.Fatalf("legacy/foreign decision=%+v", d)
					}
					continue
				}
				if d == nil || d.Preferred != "claude" || d.Chosen != "codex" || d.Reason != run.HarnessDecisionReason.String || d.PreferredHeld || d.OtherHeld {
					t.Fatalf("decision=%+v", d)
				}
				values := []*float64{d.PreferredScore, d.OtherScore, d.PreferredMeasured, d.OtherMeasured}
				for _, value := range values {
					if state == "unknown" && value != nil {
						t.Fatal("unknown usage became zero")
					}
					if state == "zero" && (value == nil || *value != 0) {
						t.Fatal("measured zero became unknown")
					}
				}
			}
			// Every caller without an explicit owner read gets a null decision, including
			// worker ACK/conflict responses and admin mutation responses.
			generic := runToDTO(run, "normal", 0, 0, 0, dtoTestNow, 0)
			if generic.HarnessDecision != nil || h.workerRunDTO(t.Context(), run).HarnessDecision != nil {
				t.Fatal("generic or worker response disclosed private decision")
			}
		})
	}
}
