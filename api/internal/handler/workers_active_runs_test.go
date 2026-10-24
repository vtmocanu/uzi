package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// activeRunSeed is one blocking run the fixture store reports.
type activeRunSeed struct {
	id, kind, status, title string
	iid                     *int64
}

// TestDeleteWorkerActiveRunsConflictNamesRuns pins #3390: the active-runs 409 keeps its
// error string byte-for-byte and additionally names the blocking runs plus the true total.
func TestDeleteWorkerActiveRunsConflictNamesRuns(t *testing.T) {
	iid := int64(7)
	runs := []activeRunSeed{
		{id: "11111111-1111-4111-8111-111111111111", kind: "issue", status: "queued", title: "Fix login", iid: &iid},
		{id: "22222222-2222-4222-8222-222222222222", kind: "chat", status: "running", title: "chat about X"},
	}
	st, deleted := activeRunsDeleteStore(t, runs, 2, 0)
	h := &Handler{wsvc: workersvc.New(st, nil, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()})}

	rec := httptest.NewRecorder()
	h.DeleteWorker(rec, deleteWorkerReq(store.User{ID: uuid.New()}, uuid.New()))

	if rec.Code != http.StatusConflict {
		t.Fatalf("DELETE = %d, want 409\nbody: %s", rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode body: %v (%s)", err, rec.Body.String())
	}
	var body struct {
		Error      string `json:"error"`
		ActiveRuns []struct {
			ID, Kind, Status, Title string
			IssueIID                *int64 `json:"issue_iid"`
		} `json:"active_runs"`
		Total int64 `json:"active_runs_total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v (%s)", err, rec.Body.String())
	}
	if want := "worker has active runs; cancel them before deleting it"; body.Error != want {
		t.Errorf("error = %q, want %q", body.Error, want)
	}
	if _, ok := raw["active_runs"]; !ok {
		t.Fatalf("active_runs key missing: %s", rec.Body.String())
	}
	if body.Total != 2 {
		t.Errorf("active_runs_total = %d, want 2", body.Total)
	}
	if len(body.ActiveRuns) != 2 {
		t.Fatalf("active_runs = %+v, want 2 entries", body.ActiveRuns)
	}
	first, second := body.ActiveRuns[0], body.ActiveRuns[1]
	if first.ID != "11111111-1111-4111-8111-111111111111" || first.Kind != "issue" || first.Status != "queued" || first.Title != "Fix login" || first.IssueIID == nil || *first.IssueIID != 7 {
		t.Errorf("first run = %+v, want issue/queued/Fix login/#7", first)
	}
	if second.ID != "22222222-2222-4222-8222-222222222222" || second.Kind != "chat" || second.Status != "running" || second.Title != "chat about X" || second.IssueIID != nil {
		t.Errorf("second run = %+v, want chat/running/chat about X/null iid", second)
	}
	if deleted() {
		t.Error("a worker with active runs must not be deleted")
	}
}

// TestDeleteWorkerActiveRunsEmptyListEncodesEmptyArray pins the race where the count says
// runs exist but the listing is empty: active_runs must be [] (not null) and the total
// still comes from the count.
func TestDeleteWorkerActiveRunsEmptyListEncodesEmptyArray(t *testing.T) {
	st, deleted := activeRunsDeleteStore(t, nil, 2, 0)
	h := &Handler{wsvc: workersvc.New(st, nil, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()})}

	rec := httptest.NewRecorder()
	h.DeleteWorker(rec, deleteWorkerReq(store.User{ID: uuid.New()}, uuid.New()))

	if rec.Code != http.StatusConflict {
		t.Fatalf("DELETE = %d, want 409\nbody: %s", rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode body: %v (%s)", err, rec.Body.String())
	}
	if got := string(raw["active_runs"]); got != "[]" {
		t.Errorf("active_runs = %s, want []", got)
	}
	if got := string(raw["active_runs_total"]); got != "2" {
		t.Errorf("active_runs_total = %s, want 2", got)
	}
	if deleted() {
		t.Error("a worker with active runs must not be deleted")
	}
}

// TestDeleteWorkerCustodyConflictUnchangedWithoutActiveRuns is the control: a custody
// refusal with no active runs keeps its own body and gains no active_runs key.
func TestDeleteWorkerCustodyConflictUnchangedWithoutActiveRuns(t *testing.T) {
	st, deleted := activeRunsDeleteStore(t, nil, 0, 1)
	h := &Handler{wsvc: workersvc.New(st, nil, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()})}

	rec := httptest.NewRecorder()
	h.DeleteWorker(rec, deleteWorkerReq(store.User{ID: uuid.New()}, uuid.New()))

	if rec.Code != http.StatusConflict {
		t.Fatalf("DELETE = %d, want 409\nbody: %s", rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode body: %v (%s)", err, rec.Body.String())
	}
	if _, ok := raw["custody_holds"]; !ok {
		t.Errorf("custody_holds missing: %s", rec.Body.String())
	}
	if _, ok := raw["active_runs"]; ok {
		t.Errorf("custody refusal must not carry active_runs: %s", rec.Body.String())
	}
	if deleted() {
		t.Error("a custody-held worker must not be deleted")
	}
}
