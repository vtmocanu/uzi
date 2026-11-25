package handler

import (
	"encoding/json"
	"net/http"
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/runenv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// held_by_runs on env_history_full, the delete-with-receipt re-create, and
// the secret to plain conversion, through the handler against PostgreSQL.

// paddedRetiredVersions fills the owner's retired-version budget and returns the var ids.
func (f *envFixture) paddedRetiredVersions() []uuid.UUID {
	f.t.Helper()
	ids := make([]uuid.UUID, 0, runenv.MaxRetiredVersions)
	for range runenv.MaxRetiredVersions {
		id := uuid.New()
		cjTestExec(f.t, f, `INSERT INTO run_env_vars (var_id,user_id,repo_id,name,secret,ciphertext,sealed_with,value_bytes,rev,retired_at)
 VALUES ($1,$2,$3,'PADDING',true,'\x01','master_aad',1,1,now())`, id, f.owner, f.repo)
		ids = append(ids, id)
	}
	return ids
}

// runWithReceipt seeds a run (status running, or terminal for three days) with a secret receipt for one
// padded version and returns its id.
func (f *envFixture) runWithReceipt(iid int, varID uuid.UUID, terminalDaysAgo bool) uuid.UUID {
	f.t.Helper()
	var runID uuid.UUID
	cjTestExecScan(f.t, f, &runID, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
 VALUES (gen_random_uuid(), $1, $2, $3, 't', 'd', 'running', 'issue') RETURNING id`, f.owner, f.repo, iid)
	if terminalDaysAgo {
		cjTestExec(f.t, f, `UPDATE runs SET status='failed', fail_origin='agent_failure', finished_at=now()-interval '3 days', updated_at=now()-interval '3 days', claim_released_at=now()-interval '3 days' WHERE id=$1`, runID)
	}
	if err := store.New(f.pool).UpsertRunEnvReceipt(f.t.Context(), store.UpsertRunEnvReceiptParams{
		RunID: runID, UserID: f.owner, VarID: varID, Rev: 1, Scope: "repo", Name: "PADDING", Secret: true, Generation: 1,
	}); err != nil {
		f.t.Fatal(err)
	}
	return runID
}

// An env_history_full answer names the held runs that keep retired secret versions sealed, at
// most 20 of them; a run past its grace window with no custody does not hold anything.
func TestRunEnvHistoryFullNamesHeldRunsLiveDB(t *testing.T) {
	f := newEnvFixture(t)
	f.put("HELD_TOKEN", jsonBody(map[string]any{"value": secretFixture(1)}), http.StatusCreated)
	f.receiptedRun("HELD_TOKEN", true, 1)
	padded := f.paddedRetiredVersions()
	settled := f.runWithReceipt(1, padded[0], true)
	want := map[string]bool{}
	for i := 1; i <= 22; i++ {
		want[f.runWithReceipt(1+i, padded[i], false).String()] = true
	}

	rec := f.do(http.MethodPut, "/HELD_TOKEN", jsonBody(map[string]any{"value": secretFixture(3)}))
	f.must(rec, http.StatusConflict)
	var body struct {
		Code       string    `json:"code"`
		HeldByRuns *[]string `json:"held_by_runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "env_history_full" || body.HeldByRuns == nil {
		t.Fatalf("answer = %s; want env_history_full with held_by_runs", respShape(rec))
	}
	got := *body.HeldByRuns
	if len(got) != 20 {
		t.Fatalf("held_by_runs has %d ids; want the cap of 20: %v", len(got), got)
	}
	sort.Strings(got)
	for i, id := range got {
		if !want[id] || id == settled.String() {
			t.Fatalf("held_by_runs[%d] = %s is not a held run of this owner", i, id)
		}
	}
}

// With every receipting run settled, held_by_runs is present and empty.
func TestRunEnvHistoryFullWithNoHeldRunsLiveDB(t *testing.T) {
	f := newEnvFixture(t)
	f.put("HELD_TOKEN", jsonBody(map[string]any{"value": secretFixture(1)}), http.StatusCreated)
	f.receiptedRun("HELD_TOKEN", true, 1)
	padded := f.paddedRetiredVersions()
	f.runWithReceipt(1, padded[0], true)
	rec := f.do(http.MethodPut, "/HELD_TOKEN", jsonBody(map[string]any{"value": secretFixture(3)}))
	f.must(rec, http.StatusConflict)
	var body struct {
		Code       string    `json:"code"`
		HeldByRuns *[]string `json:"held_by_runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.HeldByRuns == nil || len(*body.HeldByRuns) != 0 {
		t.Fatalf("answer = %s (%v); want an empty held_by_runs array", respShape(rec), err)
	}
}

// Deleting a receipted secret keeps it pending deletion; the name is free again at once and a
// new variable under it gets a new var_id.
func TestRunEnvDeleteWithReceiptFreesTheNameLiveDB(t *testing.T) {
	f := newEnvFixture(t)
	f.put("REUSED_NAME", jsonBody(map[string]any{"value": secretFixture(1)}), http.StatusCreated)
	old := f.versions("REUSED_NAME")[1]
	f.receiptedRun("REUSED_NAME", true, 1)
	rec := f.do(http.MethodDelete, "/REUSED_NAME", "")
	f.must(rec, http.StatusAccepted)
	var d apitypes.RunEnvDeleteDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil || d.State != apitypes.RunEnvStatePendingDeletion {
		t.Fatalf("delete body = %s (%v)", respShape(rec), err)
	}
	f.put("REUSED_NAME", jsonBody(map[string]any{"value": secretFixture(2)}), http.StatusCreated)
	var newVar uuid.UUID
	cjTestExecScan(t, f, &newVar, `SELECT var_id FROM run_env_vars WHERE user_id=$1 AND repo_id=$2 AND name='REUSED_NAME' AND pending_delete_at IS NULL`, f.owner, f.repo)
	if newVar == old.varID {
		t.Fatal("the re-created variable reused the deleted variable's var_id")
	}
	if f.rowCount("var_id=$3 AND pending_delete_at IS NOT NULL", old.varID) != 1 {
		t.Fatal("the pending-deletion version did not stay")
	}
}

// Changing a receipted secret to a plain value keeps the secret version sealed and retired for
// the runs that hold it, and the live version is plain. An unreceipted one leaves one row.
func TestRunEnvSecretToPlainRetiresTheSecretSideLiveDB(t *testing.T) {
	f := newEnvFixture(t)
	f.put("HELD_SECRET", jsonBody(map[string]any{"value": secretFixture(1)}), http.StatusCreated)
	f.put("FREE_SECRET", jsonBody(map[string]any{"value": secretFixture(2)}), http.StatusCreated)
	f.receiptedRun("HELD_SECRET", true, 1)

	f.put("HELD_SECRET", jsonBody(map[string]any{"value": "now-plain", "secret": false}), http.StatusOK)
	if f.rowCount("name='HELD_SECRET' AND rev=1 AND secret AND retired_at IS NOT NULL") != 1 {
		t.Fatal("the receipted secret version was not retired")
	}
	if f.rowCount("name='HELD_SECRET' AND rev=2 AND NOT secret AND retired_at IS NULL") != 1 {
		t.Fatal("the new live version is not plain")
	}
	if got, err := f.openVersion("HELD_SECRET", f.versions("HELD_SECRET")[1]); err != nil || got != secretFixture(1) {
		t.Fatalf("the retired secret version opens to a value of length %d (matches=%v), %v", len(got), got == secretFixture(1), err)
	}

	f.put("FREE_SECRET", jsonBody(map[string]any{"value": "now-plain", "secret": false}), http.StatusOK)
	if n := f.rowCount("name='FREE_SECRET'"); n != 1 {
		t.Fatalf("an unreceipted secret to plain left %d rows; want 1", n)
	}
}
