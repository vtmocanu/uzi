package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// oversizeAnswerStore serves one parked run to its owner and records any answer
// enqueue; every other query panics via the embedded interface.
type oversizeAnswerStore struct {
	workersvc.Store
	ownerID uuid.UUID
	run     store.Run
	created int
}

func (s *oversizeAnswerStore) GetRunByIDForUser(_ context.Context, arg store.GetRunByIDForUserParams) (store.Run, error) {
	if arg.ID == s.run.ID && arg.UserID == s.ownerID {
		return s.run, nil
	}
	return store.Run{}, pgx.ErrNoRows
}

func (s *oversizeAnswerStore) CreateRunAnswerInput(_ context.Context, _ store.CreateRunAnswerInputParams) (store.RunUserInput, error) {
	s.created++
	return store.RunUserInput{}, nil
}

// TestCreateRunInputAnswerOverLimitIs400 (#3136): an answer submission over the
// total-size limit is a 400 that names the size and limit, and nothing is enqueued,
// so the question stays open for a shortened resend.
func TestCreateRunInputAnswerOverLimitIs400(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	qid := uuid.NewString()
	st := &oversizeAnswerStore{ownerID: owner.ID, run: store.Run{
		ID: runID, UserID: owner.ID, Status: "awaiting_input",
		OpenQuestionID: pgtype.Text{String: qid, Valid: true},
	}}
	h := &Handler{wsvc: workersvc.New(st, nil, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()})}

	inner, err := json.Marshal(workersvc.AnswerBody{QuestionID: qid, Answers: []string{strings.Repeat("é", 40001)}})
	if err != nil {
		t.Fatal(err)
	}
	reqBody, err := json.Marshal(map[string]string{"kind": "answer", "body": string(inner)})
	if err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	h.CreateRunInput(w, inputReq(owner, runID, string(reqBody)))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", w.Code, w.Body.String())
	}
	for _, want := range []string{"40001", "40000"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("body %q lacks %q", w.Body.String(), want)
		}
	}
	if st.created != 0 {
		t.Fatalf("CreateRunAnswerInput called %d times, want 0", st.created)
	}
}
