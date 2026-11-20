package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/runenv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4416 M2: the env routes against REAL delivery receipts (no receipt seam), through
// the real router. Skipped without UZI_TEST_DATABASE_URL.

var envRunIID = 5000

// holdReceipt makes a new run of the fixture's repo hold a delivery receipt for the live
// version of a repo variable, as a claim would write it.
func (f *envFixture) holdReceipt(name string) uuid.UUID {
	f.t.Helper()
	var varID uuid.UUID
	var rev int32
	var secret bool
	if err := f.pool.QueryRow(context.Background(),
		`SELECT var_id, rev, secret FROM run_env_vars WHERE user_id=$1 AND repo_id=$2 AND name=$3 AND retired_at IS NULL AND pending_delete_at IS NULL`,
		f.owner, f.repo, name).Scan(&varID, &rev, &secret); err != nil {
		f.t.Fatalf("live version of %s: %v", name, err)
	}
	envRunIID++
	run := uuid.New()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
 VALUES ($1,$2,$3,$4,'t','d','completed','issue')`, run, f.owner, f.repo, envRunIID); err != nil {
		f.t.Fatal(err)
	}
	if err := store.New(f.pool).UpsertRunEnvReceipt(context.Background(), store.UpsertRunEnvReceiptParams{
		RunID: run, UserID: f.owner, VarID: varID, Rev: rev, Scope: runenv.ScopeRepo, Name: name, Secret: secret, ClaimGeneration: 1,
	}); err != nil {
		f.t.Fatal(err)
	}
	return run
}

func TestRunEnvRealReceiptsRetainAndPendingLiveDB(t *testing.T) {
	f := newEnvFixture(t)
	retired := func(name string) int { return f.rowCount("name=$3 AND retired_at IS NOT NULL", name) }

	// Rotation while a run holds a receipt for the live version retires the old one.
	f.put("ROTATED", jsonBody(map[string]any{"value": secretFixture(1)}), http.StatusCreated)
	f.holdReceipt("ROTATED")
	f.put("ROTATED", jsonBody(map[string]any{"value": secretFixture(2)}), http.StatusOK)
	if retired("ROTATED") != 1 || f.rowCount("name='ROTATED'") != 2 {
		t.Fatalf("after a receipted rotation: %d retired of %d rows; want 1 of 2", retired("ROTATED"), f.rowCount("name='ROTATED'"))
	}
	// The new version has no receipt: the next rotation drops it instead of retaining it.
	f.put("ROTATED", jsonBody(map[string]any{"value": secretFixture(3)}), http.StatusOK)
	if retired("ROTATED") != 1 || f.rowCount("name='ROTATED'") != 2 {
		t.Fatalf("after an unreceipted rotation: %d retired of %d rows; want 1 of 2", retired("ROTATED"), f.rowCount("name='ROTATED'"))
	}

	// Delete of a receipted secret: 202 pending_deletion, the name is free again as a new var_id.
	var oldVar uuid.UUID
	_ = f.pool.QueryRow(context.Background(), `SELECT var_id FROM run_env_vars WHERE name='ROTATED' AND retired_at IS NULL AND repo_id=$1`, f.repo).Scan(&oldVar)
	f.holdReceipt("ROTATED")
	del := f.do(http.MethodDelete, "/ROTATED", "")
	f.must(del, http.StatusAccepted)
	if got := del.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("DELETE Cache-Control = %q, want no-store", got)
	}
	var d apitypes.RunEnvDeleteDTO
	_ = json.Unmarshal(del.Body.Bytes(), &d)
	if d.State != apitypes.RunEnvStatePendingDeletion || f.rowCount("name='ROTATED' AND pending_delete_at IS NOT NULL") != 1 {
		t.Fatalf("delete state %q, pending rows %d", d.State, f.rowCount("name='ROTATED' AND pending_delete_at IS NOT NULL"))
	}
	f.put("ROTATED", jsonBody(map[string]any{"value": secretFixture(4)}), http.StatusCreated)
	var newVar uuid.UUID
	_ = f.pool.QueryRow(context.Background(), `SELECT var_id FROM run_env_vars WHERE name='ROTATED' AND retired_at IS NULL AND pending_delete_at IS NULL AND repo_id=$1`, f.repo).Scan(&newVar)
	if newVar == oldVar {
		t.Fatal("a re-created name reused the var_id")
	}

	// Delete without a receipt drops the variable at once.
	f.put("UNHELD", jsonBody(map[string]any{"value": secretFixture(5)}), http.StatusCreated)
	del = f.do(http.MethodDelete, "/UNHELD", "")
	f.must(del, http.StatusOK)
	if f.rowCount("name='UNHELD'") != 0 {
		t.Fatal("an unreceipted delete kept a row")
	}

	// A plain delivery is receipted but never retained: delete is immediate, rotation drops.
	f.put("PLAIN_HELD", jsonBody(map[string]any{"value": "https://one.example.test", "secret": false}), http.StatusCreated)
	f.holdReceipt("PLAIN_HELD")
	f.put("PLAIN_HELD", jsonBody(map[string]any{"value": "https://two.example.test", "secret": false}), http.StatusOK)
	if f.rowCount("name='PLAIN_HELD'") != 1 {
		t.Fatal("a plain version was retained")
	}
	f.holdReceipt("PLAIN_HELD")
	f.must(f.do(http.MethodDelete, "/PLAIN_HELD", ""), http.StatusOK)
	if f.rowCount("name='PLAIN_HELD'") != 0 {
		t.Fatal("a plain delete kept a row")
	}

	// secret -> plain retires the secret side when a run holds it.
	f.put("FLIP", jsonBody(map[string]any{"value": secretFixture(6)}), http.StatusCreated)
	f.holdReceipt("FLIP")
	f.put("FLIP", jsonBody(map[string]any{"secret": false, "value": "now-plain-value"}), http.StatusOK)
	if retired("FLIP") != 1 || f.rowCount("name='FLIP' AND retired_at IS NULL AND secret=false") != 1 {
		t.Fatalf("secret to plain: %d retired, live plain rows %d", retired("FLIP"), f.rowCount("name='FLIP' AND retired_at IS NULL AND secret=false"))
	}
}

func TestRunEnvHistoryFullNamesHoldersLiveDB(t *testing.T) {
	f := newEnvFixture(t)
	f.put("HELD", jsonBody(map[string]any{"value": secretFixture(1)}), http.StatusCreated)
	var holders []uuid.UUID
	for range runEnvHeldByLimit + 5 {
		holders = append(holders, f.holdReceipt("HELD"))
	}
	// Pad the per-user retired history to the limit.
	var have int
	_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM run_env_vars WHERE user_id=$1 AND retired_at IS NOT NULL`, f.owner).Scan(&have)
	for range runenv.MaxRetiredVersions - have {
		if _, err := f.pool.Exec(context.Background(), `INSERT INTO run_env_vars (var_id,user_id,repo_id,name,secret,ciphertext,sealed_with,value_bytes,rev,retired_at)
 VALUES ($1,$2,$3,'PADDING',true,'\x01','master_aad',1,1,now())`, uuid.New(), f.owner, f.repo); err != nil {
			t.Fatal(err)
		}
	}
	before := f.rowSnapshot("HELD")
	rec := f.do(http.MethodPut, "/HELD", jsonBody(map[string]any{"value": secretFixture(2)}))
	f.must(rec, http.StatusConflict)
	var body struct {
		Code   string      `json:"code"`
		Held   []uuid.UUID `json:"held_by_runs"`
		Detail string      `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != runEnvCodeHistoryFull {
		t.Fatalf("code = %q; body %s", body.Code, rec.Body.String())
	}
	if len(body.Held) != runEnvHeldByLimit {
		t.Fatalf("held_by_runs has %d ids, want the cap %d", len(body.Held), runEnvHeldByLimit)
	}
	known := map[uuid.UUID]bool{}
	for _, h := range holders {
		known[h] = true
	}
	for _, id := range body.Held {
		if !known[id] {
			t.Fatalf("held_by_runs names %s, which holds no receipt", id)
		}
	}
	if after := f.rowSnapshot("HELD"); after["HELD"] != before["HELD"] { // a refused rotation changes nothing
		t.Fatalf("HELD changed by the refused rotation:\n before %s\n after  %s", before["HELD"], after["HELD"])
	}

	// Disable and Delete are never refused for the budget; Delete keeps the receipted secret.
	f.must(f.do(http.MethodPost, "/HELD/disable", ""), http.StatusOK)
	f.must(f.do(http.MethodPost, "/HELD/enable", ""), http.StatusOK)
	f.must(f.do(http.MethodDelete, "/HELD", ""), http.StatusAccepted)
}
