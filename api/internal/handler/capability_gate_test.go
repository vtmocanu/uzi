package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #84 M4 4c: the handler wiring for the capability approval gate — the 409 mapping of
// ErrCapabilityUnmet with the unmet names + remediation hint, and the override_capabilities
// pre-clear that unblocks the approve. The service-level block behaviour is pinned by
// workersvc; here we assert the HTTP surface (status, body, and the override glue in
// CreateRunInput). The service has a nil capabilitySettings reader → the kill-switch defaults
// ON, so the gate is live without extra wiring.

// gatedCapStore builds a runsStore for a run parked at the plan gate, owned by `worker`,
// requiring `required`, with an own-roster that validates a no-exclusion own selection.
func gatedCapStore(owner store.User, runID uuid.UUID, worker store.Worker, required []string) *runsStore {
	return &runsStore{
		ownerID: owner.ID,
		run: store.Run{
			ID: runID, UserID: owner.ID, Status: "awaiting_approval",
			WorkerID:             pgtype.UUID{Bytes: worker.ID, Valid: true},
			RequiredCapabilities: required,
		},
		worker:         worker,
		clearCapsRows:  1,
		claimTemplates: []store.AgentTemplate{{Name: "lead"}, {Name: "coder"}},
	}
}

const approveOwn = `{"kind":"approve_plan","selection":{"source":"own","exclusions":[]}}`

// A base owning worker (no docker) + a docker requirement → 409 with the unmet name and the
// override hint, and no approve_plan enqueued.
func TestCreateRunInputCapabilityBlock409(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID, wkrID := uuid.New(), uuid.New()
	st := gatedCapStore(owner, runID, store.Worker{ID: wkrID}, []string{"docker"})
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.CreateRunInput(rec, inputReq(owner, runID, approveOwn))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "docker") {
		t.Errorf("body %q must name the unmet capability docker", body)
	}
	if !strings.Contains(body, "override_capabilities") {
		t.Errorf("body %q must hint the override_capabilities remediation", body)
	}
	if st.createdApproval != nil {
		t.Error("a blocked approve must not enqueue an approve_plan input")
	}
}

// The override_capabilities flag clears the run's required set first, so the same approve
// succeeds (202) and the clear is scoped to the owner + run.
func TestCreateRunInputOverrideCapabilitiesApproves(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID, wkrID := uuid.New(), uuid.New()
	st := gatedCapStore(owner, runID, store.Worker{ID: wkrID}, []string{"docker"})
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.CreateRunInput(rec, inputReq(owner, runID,
		`{"kind":"approve_plan","selection":{"source":"own","exclusions":[]},"override_capabilities":true}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if st.clearedCapsArg == nil {
		t.Fatal("override_capabilities must call ClearRunRequiredCapabilities")
	}
	if st.clearedCapsArg.ID != runID || st.clearedCapsArg.UserID != owner.ID {
		t.Fatalf("clear scoped to id=%v user=%v, want id=%v user=%v",
			st.clearedCapsArg.ID, st.clearedCapsArg.UserID, runID, owner.ID)
	}
	if st.createdApproval == nil {
		t.Fatal("approve_plan must be enqueued after the override cleared the requirement")
	}
}

// Without the override flag, no clear happens (the normal approve path), and a satisfying
// owning worker approves cleanly.
func TestCreateRunInputDockerWorkerApprovesNoClear(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID, wkrID := uuid.New(), uuid.New()
	worker := store.Worker{ID: wkrID, DockerEnabled: pgtype.Bool{Bool: true, Valid: true}}
	st := gatedCapStore(owner, runID, worker, []string{"docker"})
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.CreateRunInput(rec, inputReq(owner, runID, approveOwn))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if st.clearedCapsArg != nil {
		t.Error("no override flag → ClearRunRequiredCapabilities must not be called")
	}
	if st.createdApproval == nil {
		t.Fatal("a satisfying owning worker must approve")
	}
}
