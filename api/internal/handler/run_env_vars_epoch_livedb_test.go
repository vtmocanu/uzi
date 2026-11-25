package handler

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Skipped unless UZI_TEST_DATABASE_URL is set; run via ./e2e/run-store-it.sh.
//
// An unchanged-description save writes nothing and answers from a fresh read of the live row,
// whose ciphertext and vault epoch come from one statement. These tests interleave a change
// right after that read returns.

// afterReadDB runs hook once, right after the statement whose text contains match has been
// scanned by its caller.
type afterReadDB struct {
	store.DBTX
	match string
	hook  func()
	once  sync.Once
	fired int
}

type afterScanRow struct {
	pgx.Row
	fn func()
}

func (r afterScanRow) Scan(dest ...any) error {
	err := r.Row.Scan(dest...)
	r.fn()
	return err
}

func (d *afterReadDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	row := d.DBTX.QueryRow(ctx, sql, args...)
	if !strings.Contains(sql, d.match) {
		return row
	}
	return afterScanRow{Row: row, fn: func() {
		d.fired++
		d.once.Do(d.hook)
	}}
}

// interleave installs hook to run once right after the named statement returns.
func (f *envFixture) interleave(match string, hook func()) *afterReadDB {
	db := &afterReadDB{DBTX: f.pool, match: match, hook: hook}
	f.h.q = store.New(db)
	return db
}

func newProtectableEnvFixture(t *testing.T) (*envFixture, string) {
	t.Helper()
	h, router, pool := cliVaultLiveDB(t)
	vaultFixturePolicy(t, pool, "optional")
	owner := cliSeedUser(t, pool, false)
	const password = "a-vault-epoch-password" //nolint:gosec // G101: test-only vault password.
	setVaultFixturePassword(t, pool, owner, password)
	repo := cliSeedOwnedRepo(t, pool, owner)
	f := &envFixture{t: t, h: h, router: router, pool: pool, owner: owner, repo: repo, jwt: cliMintJWT(t, pool, owner)}
	return f, password
}

const epochReread = "name: GetLiveRunEnvVarByNameWithEpoch"

// The owner protects and locks the vault right after the unchanged-description save re-read the
// row: the plain value is withheld (the row was read at the old epoch), and the control without
// that change returns it.
func TestRunEnvDescriptionUnchangedStaleEpochWithholdsLiveDB(t *testing.T) {
	f, password := newProtectableEnvFixture(t)
	body := jsonBody(map[string]any{"value": "https://sandbox.example.test", "secret": false, "description": "same"})
	f.put("PLAIN_ONE", body, http.StatusCreated)
	same := jsonBody(map[string]any{"description": "same"})

	// Control: nothing changes after the re-read.
	db := f.interleave(epochReread, func() {})
	got := f.put("PLAIN_ONE", same, http.StatusOK)
	if db.fired != 1 || got.Value == nil || *got.Value != "https://sandbox.example.test" || got.ValueWithheld {
		t.Fatalf("control: re-reads=%d value present=%v withheld=%v", db.fired, got.Value != nil, got.ValueWithheld)
	}

	db = f.interleave(epochReread, func() {
		if err := f.h.vault.Enable(context.Background(), f.owner, password); err != nil {
			t.Errorf("enable protection: %v", err)
		}
		f.h.vault.Lock(f.owner)
	})
	got = f.put("PLAIN_ONE", same, http.StatusOK)
	if db.fired != 1 {
		t.Fatalf("re-reads = %d, want 1", db.fired)
	}
	if got.Value != nil || !got.ValueWithheld {
		t.Fatalf("response after the owner protected and locked: value present=%v withheld=%v", got.Value != nil, got.ValueWithheld)
	}
}

// A row deleted between the planning read and the re-read is a 404, as the route answers a
// missing variable.
func TestRunEnvDescriptionUnchangedRowGoneIs404LiveDB(t *testing.T) {
	f := newEnvFixture(t)
	f.put("API_BASE", jsonBody(map[string]any{"value": "first-value", "secret": false, "description": "same"}), http.StatusCreated)
	db := f.interleave("name: GetLiveRunEnvVarByName :one", func() {
		cliMustExec(t, f.pool, `DELETE FROM run_env_vars WHERE repo_id=$1 AND name='API_BASE'`, f.repo)
	})
	rec := f.do(http.MethodPut, "/API_BASE", jsonBody(map[string]any{"description": "same"}))
	f.must(rec, http.StatusNotFound)
	if db.fired != 1 {
		t.Fatalf("planning reads = %d, want 1", db.fired)
	}
}

// A row that moved between the planning read and the re-read is answered as the fresh row, never
// as the planning row paired with a newer epoch.
func TestRunEnvDescriptionUnchangedMovedRowAnswersFreshRowLiveDB(t *testing.T) {
	f := newEnvFixture(t)
	f.put("API_BASE", jsonBody(map[string]any{"value": "first-value", "secret": false, "description": "same"}), http.StatusCreated)
	f.interleave("name: GetLiveRunEnvVarByName :one", func() {
		cliMustExec(t, f.pool, `UPDATE run_env_vars SET description='moved' WHERE repo_id=$1 AND name='API_BASE' AND retired_at IS NULL`, f.repo)
	})
	got := f.put("API_BASE", jsonBody(map[string]any{"description": "same"}), http.StatusOK)
	if got.Description != "moved" || got.Value == nil || *got.Value != "first-value" {
		t.Fatalf("moved row: description matches fresh=%v value present=%v", got.Description == "moved", got.Value != nil)
	}
}
