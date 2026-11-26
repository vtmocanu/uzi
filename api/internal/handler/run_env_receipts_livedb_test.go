package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/runenv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// PRD #4416 M2: version-2 sealing context and the real receipt query behind the
// retire path, through the handler against PostgreSQL.

// envRevs lists the revisions of a version map for failure messages (the rows hold ciphertext).
func envRevs(m map[int32]envVersionRow) []int32 {
	out := make([]int32, 0, len(m))
	for r := range m {
		out = append(out, r)
	}
	slices.Sort(out)
	return out
}

type envVersionRow struct {
	id         uuid.UUID
	varID      uuid.UUID
	rev        int32
	ct         []byte
	marker     string
	aadVersion int16
}

func (f *envFixture) versions(name string) map[int32]envVersionRow {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT id, var_id, rev, ciphertext, sealed_with, aad_version FROM run_env_vars WHERE user_id=$1 AND repo_id=$2 AND name=$3`, f.owner, f.repo, name)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	out := map[int32]envVersionRow{}
	for rows.Next() {
		var r envVersionRow
		if err := rows.Scan(&r.id, &r.varID, &r.rev, &r.ct, &r.marker, &r.aadVersion); err != nil {
			f.t.Fatal(err)
		}
		out[r.rev] = r
	}
	return out
}

func (f *envFixture) openVersion(name string, r envVersionRow) (string, error) {
	f.t.Helper()
	kind, err := runenv.AADFor(r.aadVersion, f.owner, runenv.ScopeRepo, &f.repo, true, name, r.varID, r.rev)
	if err != nil {
		f.t.Fatal(err)
	}
	pt, err := f.h.vault.Open(f.owner, kind, r.marker, r.ct)
	return string(pt), err
}

// A ciphertext copied from one version of a variable to another opens under neither
// the other version's context: the version-2 context binds var_id and rev.
func TestRunEnvVersionTwoCiphertextCannotSwapVersionsLiveDB(t *testing.T) {
	f := newEnvFixture(t)
	f.withReceipts() // rev 1 is retired, not dropped, so both versions exist
	f.put("SWAP_ME", jsonBody(map[string]any{"value": secretFixture(1)}), http.StatusCreated)
	f.put("SWAP_ME", jsonBody(map[string]any{"value": secretFixture(2)}), http.StatusOK)
	v := f.versions("SWAP_ME")
	if len(v) != 2 || v[1].aadVersion != runenv.AADVersion2 || v[2].aadVersion != runenv.AADVersion2 {
		t.Fatalf("%d versions %v; want rev 1 and 2, both aad_version 2", len(v), envRevs(v))
	}
	for rev, want := range map[int32]string{1: secretFixture(1), 2: secretFixture(2)} {
		if got, err := f.openVersion("SWAP_ME", v[rev]); err != nil || got != want {
			t.Fatalf("rev %d opens to a value of length %d (matches=%v), %v; want length %d", rev, len(got), got == want, err, len(want))
		}
	}
	// Swap: the rev-1 ciphertext in the rev-2 row.
	swapped := v[2]
	swapped.ct, swapped.marker = v[1].ct, v[1].marker
	cjTestExec(t, f, `UPDATE run_env_vars SET ciphertext=$2, sealed_with=$3 WHERE id=$1`, swapped.id, swapped.ct, swapped.marker)
	if got, err := f.openVersion("SWAP_ME", f.versions("SWAP_ME")[2]); err == nil {
		t.Fatalf("the rev-1 ciphertext opened as rev 2 (value length %d)", len(got))
	}
}

func cjTestExec(t *testing.T, f *envFixture, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec: %v", err)
	}
}

// The real receipt query decides retire versus drop: a rotation keeps the version a run
// holds a secret receipt for, and drops one it does not.
func TestRunEnvRotationHonoursSecretReceiptsLiveDB(t *testing.T) {
	f := newEnvFixture(t)
	f.put("HELD_TOKEN", jsonBody(map[string]any{"value": secretFixture(1)}), http.StatusCreated)
	f.put("PLAIN_NAME", jsonBody(map[string]any{"value": "plain-value", "secret": false}), http.StatusCreated)
	var runID uuid.UUID
	cjTestExecScan(t, f, &runID, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
 VALUES (gen_random_uuid(), $1, $2, 4416, 't', 'd', 'running', 'issue') RETURNING id`, f.owner, f.repo)
	held := f.versions("HELD_TOKEN")[1]
	q := store.New(f.pool)
	if err := q.UpsertRunEnvReceipt(context.Background(), store.UpsertRunEnvReceiptParams{RunID: runID, UserID: f.owner, VarID: held.varID, Rev: 1, Scope: "repo", Name: "HELD_TOKEN", Secret: true, Generation: 1}); err != nil {
		t.Fatal(err)
	}
	plain := f.versions("PLAIN_NAME")[1]
	if err := q.UpsertRunEnvReceipt(context.Background(), store.UpsertRunEnvReceiptParams{RunID: runID, UserID: f.owner, VarID: plain.varID, Rev: 1, Scope: "repo", Name: "PLAIN_NAME", Secret: false, Generation: 1}); err != nil {
		t.Fatal(err)
	}
	f.put("HELD_TOKEN", jsonBody(map[string]any{"value": secretFixture(2)}), http.StatusOK)
	f.put("PLAIN_NAME", jsonBody(map[string]any{"value": "plain-value-2", "secret": false}), http.StatusOK)
	if n := len(f.versions("HELD_TOKEN")); n != 2 {
		t.Fatalf("a receipted secret rotation left %d versions, want 2 (rev 1 retired)", n)
	}
	if n := len(f.versions("PLAIN_NAME")); n != 1 {
		t.Fatalf("a plain rotation left %d versions, want 1 (plain receipts retain nothing)", n)
	}
	if got, err := f.openVersion("HELD_TOKEN", f.versions("HELD_TOKEN")[1]); err != nil || got != secretFixture(1) {
		t.Fatalf("the retained version opens to a value of length %d (matches=%v), %v", len(got), got == secretFixture(1), err)
	}
}

func cjTestExecScan(t *testing.T, f *envFixture, dst any, sql string, args ...any) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(dst); err != nil {
		t.Fatalf("query: %v", err)
	}
}

// receiptedRun seeds a run of the fixture's owner and repo and records a delivery receipt
// for one version, through the real receipt table (no stub on the handler).
func (f *envFixture) receiptedRun(name string, secret bool, rev int32) {
	f.t.Helper()
	var runID uuid.UUID
	cjTestExecScan(f.t, f, &runID, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
 VALUES (gen_random_uuid(), $1, $2, floor(random()*1000000)::int, 't', 'd', 'running', 'issue') RETURNING id`, f.owner, f.repo)
	v := f.versions(name)[rev]
	if err := store.New(f.pool).UpsertRunEnvReceipt(context.Background(), store.UpsertRunEnvReceiptParams{
		RunID: runID, UserID: f.owner, VarID: v.varID, Rev: rev, Scope: "repo", Name: name, Secret: secret, Generation: 1,
	}); err != nil {
		f.t.Fatal(err)
	}
}

// Delete keeps a secret some run holds a receipt for (202 pending_deletion, the row stays and
// the response is not cacheable) and drops everything else at once (200).
func TestRunEnvDeleteWithRealReceiptsLiveDB(t *testing.T) {
	f := newEnvFixture(t)
	f.put("HELD_SECRET", jsonBody(map[string]any{"value": secretFixture(1)}), http.StatusCreated)
	f.put("FREE_SECRET", jsonBody(map[string]any{"value": secretFixture(2)}), http.StatusCreated)
	f.put("HELD_PLAIN", jsonBody(map[string]any{"value": "plain-one", "secret": false}), http.StatusCreated)
	f.receiptedRun("HELD_SECRET", true, 1)
	f.receiptedRun("HELD_PLAIN", false, 1)

	rec := f.do(http.MethodDelete, "/HELD_SECRET", "")
	f.must(rec, http.StatusAccepted)
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("DELETE Cache-Control = %q, want no-store", got)
	}
	var d apitypes.RunEnvDeleteDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil || d.State != apitypes.RunEnvStatePendingDeletion {
		t.Fatalf("delete body = %s (%v), want pending_deletion", respShape(rec), err)
	}
	if f.rowCount("name='HELD_SECRET' AND pending_delete_at IS NOT NULL") != 1 {
		t.Fatal("the receipted secret was not kept pending deletion")
	}
	rec = f.do(http.MethodDelete, "/FREE_SECRET", "")
	f.must(rec, http.StatusOK)
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("DELETE (200) Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}
	if f.rowCount("name='FREE_SECRET'") != 0 {
		t.Fatal("an unreceipted secret was kept")
	}
	f.must(f.do(http.MethodDelete, "/HELD_PLAIN", ""), http.StatusOK)
	if f.rowCount("name='HELD_PLAIN'") != 0 {
		t.Fatal("a plain version with a plain receipt was kept")
	}
}

// With the retired-version budget full, a rotation of a really receipted secret is refused with
// env_history_full and changes nothing; an unreceipted one needs no history and succeeds.
func TestRunEnvHistoryFullWithRealReceiptsLiveDB(t *testing.T) {
	f := newEnvFixture(t)
	f.put("HELD_TOKEN", jsonBody(map[string]any{"value": secretFixture(1)}), http.StatusCreated)
	f.put("OTHER_TOKEN", jsonBody(map[string]any{"value": secretFixture(2)}), http.StatusCreated)
	f.receiptedRun("HELD_TOKEN", true, 1)
	for range runenv.MaxRetiredVersions {
		cjTestExec(t, f, `INSERT INTO run_env_vars (var_id,user_id,repo_id,name,secret,ciphertext,sealed_with,value_bytes,rev,retired_at)
 VALUES ($1,$2,$3,'PADDING',true,'\x01','master_aad',1,1,now())`, uuid.New(), f.owner, f.repo)
	}
	rec := f.do(http.MethodPut, "/HELD_TOKEN", jsonBody(map[string]any{"value": secretFixture(3)}))
	f.must(rec, http.StatusConflict)
	var e struct{ Code string }
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e.Code != "env_history_full" {
		t.Fatalf("rotation of a receipted secret at the budget: %s", respShape(rec))
	}
	if k := f.find(f.list(), "HELD_TOKEN"); k == nil || k.Rev != 1 {
		t.Fatalf("a refused rotation changed the variable: %s", dtoShape(k))
	}
	// Nothing holds a receipt for OTHER_TOKEN: its old version is dropped, not retained.
	f.put("OTHER_TOKEN", jsonBody(map[string]any{"value": secretFixture(4)}), http.StatusOK)
	// Disable and Delete are never refused for budget reasons.
	f.must(f.do(http.MethodPost, "/HELD_TOKEN/disable", ""), http.StatusOK)
	f.must(f.do(http.MethodDelete, "/HELD_TOKEN", ""), http.StatusAccepted)
}

// A version-1 plain row, written before the version-2 context existed, converts to a secret
// with PUT {"secret":true}: the new row is version 2 and opens under its own context.
func TestRunEnvVersionOnePlainRowConvertsToSecretLiveDB(t *testing.T) {
	f := newEnvFixture(t)
	f.put("LEGACY_PLAIN", jsonBody(map[string]any{"value": "legacy-plain-value", "secret": false}), http.StatusCreated)
	v1 := f.versions("LEGACY_PLAIN")[1]
	// Reseal the stored value under the version-1 context and mark the row version 1.
	kind1, err := runenv.AADFor(runenv.AADVersion1, f.owner, runenv.ScopeRepo, &f.repo, false, "LEGACY_PLAIN", v1.varID, 1)
	if err != nil {
		t.Fatal(err)
	}
	material, err := f.h.vault.Seal(context.Background(), f.owner, kind1, []byte("legacy-plain-value"), vault.SealForUse)
	if err != nil {
		t.Fatal(err)
	}
	cjTestExec(t, f, `UPDATE run_env_vars SET ciphertext=$2, sealed_with=$3, aad_version=1 WHERE id=$1`, v1.id, material.Ciphertext, material.SealedWith)

	rec := f.do(http.MethodPut, "/LEGACY_PLAIN", jsonBody(map[string]any{"secret": true}))
	f.must(rec, http.StatusOK)
	versions := f.versions("LEGACY_PLAIN")
	live, ok := versions[2]
	if !ok {
		t.Fatalf("%d versions %v, want a new rev 2", len(versions), envRevs(versions))
	}
	if live.aadVersion != runenv.AADVersion2 {
		t.Fatalf("converted row aad_version = %d, want 2", live.aadVersion)
	}
	if got, err := f.openVersion("LEGACY_PLAIN", live); err != nil || got != "legacy-plain-value" {
		t.Fatalf("the converted secret opens to a value of length %d (matches=%v), %v", len(got), got == "legacy-plain-value", err)
	}
	if k := f.find(f.list(), "LEGACY_PLAIN"); k == nil || !k.Secret || k.Value != nil {
		t.Fatalf("listed variable = %s, want a secret without a value", dtoShape(k))
	}
}
