package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/runenv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4416 M2: version-2 sealing context and the real receipt query behind the
// retire path, through the handler against PostgreSQL.

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
		t.Fatalf("versions = %+v; want rev 1 and 2, both aad_version 2", v)
	}
	for rev, want := range map[int32]string{1: secretFixture(1), 2: secretFixture(2)} {
		if got, err := f.openVersion("SWAP_ME", v[rev]); err != nil || got != want {
			t.Fatalf("rev %d opens to %q, %v; want %q", rev, got, err, want)
		}
	}
	// Swap: the rev-1 ciphertext in the rev-2 row.
	swapped := v[2]
	swapped.ct, swapped.marker = v[1].ct, v[1].marker
	cjTestExec(t, f, `UPDATE run_env_vars SET ciphertext=$2, sealed_with=$3 WHERE id=$1`, swapped.id, swapped.ct, swapped.marker)
	if got, err := f.openVersion("SWAP_ME", f.versions("SWAP_ME")[2]); err == nil {
		t.Fatalf("the rev-1 ciphertext opened as rev 2: %q", got)
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
		t.Fatalf("the retained version opens to %q, %v", got, err)
	}
}

func cjTestExecScan(t *testing.T, f *envFixture, dst any, sql string, args ...any) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(dst); err != nil {
		t.Fatalf("query: %v", err)
	}
}
