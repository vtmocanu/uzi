package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCodeCrossCheckInstructionCommitRequest(t *testing.T) {
	pin := strings.Repeat("a", 40)
	var req planCrossCheckRequest
	if err := json.Unmarshal([]byte(`{"stage":"code","repo_instructions_commit":"`+pin+`"}`), &req); err != nil || !req.instructionFields || req.RepoInstructionsCommit == nil {
		t.Fatalf("pin decode: %v", err)
	}
	for _, raw := range []string{`{"repo_instructions_commit":null}`, `{"Repo_Instructions_Commit":"` + pin + `"}`, `{"repo_instructions_commit":"` + pin + `","repo_instructions_commit":"` + pin + `"}`} {
		if json.Unmarshal([]byte(raw), &req) == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, raw := range []string{`{"stage":"plan","claim_generation":1,"repo_instructions_commit":"` + pin + `"}`, `{"stage":"code","claim_generation":1,"reason_class":"snapshot_failed","repo_instructions_commit":"` + pin + `"}`} {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(raw))
		r = r.WithContext(mw.ContextWithWorker(r.Context(), store.Worker{}))
		router := chi.NewRouter()
		router.Post("/{id}", (&Handler{}).WorkerSubmitPlanCrossCheck)
		w := httptest.NewRecorder()
		r.URL.Path = "/" + uuid.New().String()
		router.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("refusal status: %d %s", w.Code, w.Body)
		}
	}
}

func TestCodeCrossCheckInstructionCommitStatus(t *testing.T) {
	pin := strings.Repeat("a", 40)
	for _, v := range []pgtype.Text{{}, {String: pin, Valid: true}} {
		response := codeCrossCheckResponse(store.CrossCheck{RepoInstructionsCommit: v})
		raw, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		want := "null"
		if v.Valid {
			want = `"` + pin + `"`
		}
		if string(fields["repo_instructions_commit"]) != want {
			t.Fatalf("status pin: %s", raw)
		}
	}
}
