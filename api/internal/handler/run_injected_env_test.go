package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4416 M2: the injected_env overlay on the run detail: names, scope and the secret flag of
// what the current claim generation delivered, for the run's owner only.

type injectedEnvStore struct {
	*runsStore
	rows  []store.ListRunEnvReceiptsForRunGenerationRow
	err   error
	calls int
	runID uuid.UUID
}

func (q *injectedEnvStore) ListRunEnvReceiptsForRunGeneration(_ context.Context, runID uuid.UUID) ([]store.ListRunEnvReceiptsForRunGenerationRow, error) {
	q.calls++
	q.runID = runID
	return q.rows, q.err
}

func TestGetRunInjectedEnvIsOwnerOnly(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	rows := []store.ListRunEnvReceiptsForRunGenerationRow{
		{VarID: uuid.New(), Rev: 2, Name: "API_BASE_URL", Scope: "repo", Secret: false},
		{VarID: uuid.New(), Rev: 1, Name: "API_KEY", Scope: "repo", Secret: true},
	}
	for _, tc := range []struct {
		name       string
		viewer     store.User
		err        error
		wantStatus int
		wantEnv    bool
		wantCalls  int
	}{
		{"owner", owner, nil, http.StatusOK, true, 1},
		{"adminOwner", store.User{ID: owner.ID, IsAdmin: true}, nil, http.StatusOK, true, 1},
		{"otherUser", store.User{ID: uuid.New()}, nil, http.StatusNotFound, false, 0},
		{"adminNonOwner", store.User{ID: uuid.New(), IsAdmin: true}, nil, http.StatusOK, false, 0},
		{"lookupFailure", owner, errors.New("private query error"), http.StatusOK, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &injectedEnvStore{
				runsStore: &runsStore{ownerID: owner.ID, run: store.Run{ID: runID, UserID: owner.ID, Status: "running"}},
				rows:      rows, err: tc.err,
			}
			h := newRunsHandler(t, q)
			rec := httptest.NewRecorder()
			h.GetRun(rec, runReq(tc.viewer, runID))
			if rec.Code != tc.wantStatus {
				t.Fatalf("GetRun status=%d body=%s", rec.Code, rec.Body.String())
			}
			if q.calls != tc.wantCalls {
				t.Fatalf("receipt lookups = %d, want %d (a non-owner must not read the receipts)", q.calls, tc.wantCalls)
			}
			if tc.wantStatus != http.StatusOK {
				return
			}
			var response struct {
				Run apitypes.RunDTO `json:"run"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			got := response.Run.InjectedEnv
			if !tc.wantEnv {
				if len(got) != 0 || strings.Contains(rec.Body.String(), "injected_env") {
					t.Fatalf("injected_env = %+v present for %s", got, tc.name)
				}
				return
			}
			if q.runID != runID {
				t.Fatalf("receipts read for run %s, want %s", q.runID, runID)
			}
			if len(got) != 2 || got[0] != (apitypes.InjectedEnvVarDTO{Name: "API_BASE_URL", Scope: "repo"}) ||
				got[1] != (apitypes.InjectedEnvVarDTO{Name: "API_KEY", Scope: "repo", Secret: true}) {
				t.Fatalf("injected_env = %+v", got)
			}
			if strings.Contains(rec.Body.String(), "var_id") || strings.Contains(rec.Body.String(), "\"rev\"") {
				t.Fatalf("the response carries receipt identity: %s", rec.Body.String())
			}
		})
	}
}
