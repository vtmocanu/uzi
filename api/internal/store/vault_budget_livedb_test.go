package store_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

type vbCounts struct{ totals, loads, reseals int }
type vbStore struct {
	*store.Queries
	counts *vbCounts
}

func (q *vbStore) GetVaultMaterialTotals(ctx context.Context, id uuid.UUID) (store.GetVaultMaterialTotalsRow, error) {
	q.counts.totals++
	return q.Queries.GetVaultMaterialTotals(ctx, id)
}
func (q *vbStore) LockVaultAccountMaterialBatch(ctx context.Context, p store.LockVaultAccountMaterialBatchParams) ([]store.LockVaultAccountMaterialBatchRow, error) {
	q.counts.loads++
	return q.Queries.LockVaultAccountMaterialBatch(ctx, p)
}
func (q *vbStore) LockVaultSecretMaterialBatch(ctx context.Context, p store.LockVaultSecretMaterialBatchParams) ([]store.LockVaultSecretMaterialBatchRow, error) {
	q.counts.loads++
	return q.Queries.LockVaultSecretMaterialBatch(ctx, p)
}
func (q *vbStore) ResealVaultAccountMaterial(ctx context.Context, p store.ResealVaultAccountMaterialParams) (int64, error) {
	q.counts.reseals++
	return q.Queries.ResealVaultAccountMaterial(ctx, p)
}
func (q *vbStore) ResealVaultSecretMaterial(ctx context.Context, p store.ResealVaultSecretMaterialParams) (int64, error) {
	q.counts.reseals++
	return q.Queries.ResealVaultSecretMaterial(ctx, p)
}

func vbVault(master *secretbox.Box, pool *pgxpool.Pool, counts *vbCounts) *vault.Vault {
	q := store.New(pool)
	return vault.New(master, q, func(ctx context.Context, fn func(context.Context, vault.Store) error) error {
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			return err
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = tx.Rollback(cleanup)
		}()
		wrapped := &vbStore{Queries: q.WithTx(tx), counts: counts}
		if err := fn(vault.WithStateReader(ctx, wrapped), wrapped); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return &vault.CommitError{Err: err}
		}
		return nil
	})
}

type vbCase struct {
	name                               string
	count, size, extra, live, recovery int
	rows, total, max                   int64
	refuse                             bool
}

// Each top-level test owns one database. Subtests use distinct owners and run
// serially; conversion and verification loops are bounded by the fixture counts.
func TestVaultBudgetEnableLiveDB(t *testing.T)  { vbBudget(t, false) }
func TestVaultBudgetDisableLiveDB(t *testing.T) { vbBudget(t, true) }

func vbBudget(t *testing.T, disable bool) {
	t.Helper()
	pool := vlIsolatedPool(t)
	const mib = 1024 * 1024
	cases := []vbCase{
		{name: "rows_at", count: 10000, size: 32, rows: 10000, total: 320000, max: 32},
		{name: "rows_over", count: 10001, size: 32, rows: 10001, total: 320032, max: 32, refuse: true},
		{name: "total_at", count: 64, size: mib, rows: 64, total: 64 * mib, max: mib},
		{name: "total_over", count: 64, size: mib, extra: 29, rows: 65, total: 64*mib + 29, max: mib, refuse: true},
		{name: "account_pair_at", live: 524288, recovery: 524288, rows: 2, total: mib, max: mib},
		{name: "account_pair_over", live: 524288, recovery: 524289, rows: 2, total: mib + 1, max: mib + 1, refuse: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			id := uuid.New()
			hash, err := auth.HashPassword(vlLoginPhrase)
			if err != nil {
				t.Fatal(err)
			}
			mode := store.VaultModeUnprotected
			if disable {
				mode = store.VaultModeProtected
			}
			vlExec(t, pool, "INSERT INTO users (id,email,password_hash,vault_mode) VALUES ($1,$2,$3,$4)", id, id.String()+"@example.test", hash, mode)
			vlExec(t, pool, "UPDATE app_settings SET value='optional' WHERE key='vault_policy'")
			master, err := secretbox.New(bytes.Repeat([]byte{0x57}, secretbox.KeySize))
			if err != nil {
				t.Fatal(err)
			}
			counts := &vbCounts{}
			v := vbVault(master, pool, counts)
			t.Cleanup(func() { v.Lock(id) })
			if disable {
				if err := v.Unlock(ctx, id, vlLoginPhrase); err != nil {
					t.Fatal(err)
				}
			}
			seal := func(kind string, size int) []byte {
				t.Helper()
				plain := bytes.Repeat([]byte{'b'}, size-28)
				var ct []byte
				if disable {
					material, e := v.Seal(ctx, id, kind, plain, vault.SealForUse)
					err = e
					ct = material.Ciphertext
				} else {
					binding := append(append(append([]byte{}, id[:]...), 0), []byte(kind)...)
					ct, err = master.SealWithAAD(plain, binding)
				}
				if err != nil || len(ct) != size {
					t.Fatalf("fixture seal size=%d: %v", size, err)
				}
				return ct
			}
			marker := store.SealedWithMasterAAD
			if disable {
				marker = store.SealedWithDEK
			}
			var primary, extra, live, recovery []byte
			if tc.count > 0 {
				primary = seal(store.KindAnthropicToken, tc.size)
				vlExec(t, pool, `INSERT INTO user_secrets (user_id,kind,label,ciphertext,sealed_with,is_default,enablement_rev,auto_eligible,disabled_at)
     SELECT $1,'anthropic_token','budget-'||n,$2,$3,false,17,false,'2026-01-02T03:04:05Z'
     FROM generate_series(1,$4::int) n`, id, primary, marker, tc.count)
			}
			if tc.extra > 0 {
				extra = seal(store.KindAnthropicToken, tc.extra)
				vlExec(t, pool, `INSERT INTO user_secrets (user_id,kind,label,ciphertext,sealed_with,is_default,enablement_rev,auto_eligible,disabled_at)
     VALUES ($1,'anthropic_token','extra',$2,$3,false,17,false,'2026-01-02T03:04:05Z')`, id, extra, marker)
			}
			if tc.live > 0 {
				live = seal(store.KindCodexAuth, tc.live)
				recovery = seal(store.KindCodexAuth, tc.recovery)
				vlExec(t, pool, `INSERT INTO codex_provider_account (user_id,provider_user_id,workspace_account_id,sealed_login,sealed_with,recovery_sealed,recovery_sealed_with,generation,credential_revision,recovery_generation,coord_state,recovery_cause,committed_generation)
     VALUES ($1,'budget-principal','workspace',$2,$3,$4,$3,7,9,6,'quarantined','vault_locked',7)`, id, live, marker, recovery)
			}
			// Keep only the small user/wrapper snapshot in Go, never a ciphertext JSON aggregate.
			snapshot := func() []byte {
				t.Helper()
				var out []byte
				if err := pool.QueryRow(ctx, `SELECT jsonb_build_object('user',to_jsonb(u),'wrapper',(SELECT to_jsonb(v) FROM user_vaults v WHERE user_id=$1)) FROM users u WHERE id=$1`, id).Scan(&out); err != nil {
					t.Fatal(err)
				}
				return out
			}
			before := snapshot()
			state, err := v.State(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			cached := v.Unlocked(id)
			var cacheProbe vault.SealedMaterial
			if disable {
				cacheProbe, err = v.Seal(ctx, id, store.KindCodexAuth, []byte("cache continuity"), vault.SealForUse)
				if err != nil {
					t.Fatal(err)
				}
			}
			totals, err := store.New(pool).GetVaultMaterialTotals(ctx, id)
			if err != nil || totals.MaterialRows != tc.rows || totals.CiphertextBytes != tc.total || totals.MaxRowBytes != tc.max {
				t.Fatalf("seed totals=%+v, %v; case=%s", totals, err, tc.name)
			}
			// Server-side metadata baseline retains every column, timestamp and identity,
			// excluding only columns the conversion is allowed to change.
			vlExec(t, pool, `CREATE TABLE IF NOT EXISTS vault_budget_metadata (owner uuid, collection text, id uuid, metadata jsonb)`)
			vlExec(t, pool, `INSERT INTO vault_budget_metadata SELECT user_id,'secret',id,to_jsonb(s)-ARRAY['ciphertext','sealed_with'] FROM user_secrets s WHERE user_id=$1
    UNION ALL SELECT user_id,'account',id,to_jsonb(a)-ARRAY['sealed_login','sealed_with','recovery_sealed','recovery_sealed_with'] FROM codex_provider_account a WHERE user_id=$1`, id)
			*counts = vbCounts{}
			conversionStarted := time.Now()
			if disable {
				err = v.Disable(ctx, id)
			} else {
				err = v.Enable(ctx, id, vlLoginPhrase)
			}
			t.Logf("conversion rows=%d ciphertext_bytes=%d largest_record=%d refused=%v elapsed=%s", tc.rows, tc.total, tc.max, tc.refuse, time.Since(conversionStarted))
			target := store.SealedWithDEK
			if disable {
				target = store.SealedWithMasterAAD
			}
			if tc.refuse {
				var limit *vault.ConversionLimitError
				if !errors.As(err, &limit) {
					t.Fatalf("conversion error=%v, want ConversionLimitError", err)
				}
				if limit.Rows != tc.rows || limit.Bytes != tc.total || limit.MaxRowBytes != tc.max {
					t.Fatalf("measured refusal=%+v", limit)
				}
				if *counts != (vbCounts{totals: 1}) {
					t.Fatalf("preflight loaded or wrote material: %+v", counts)
				}
				if !bytes.Equal(before, snapshot()) || v.Unlocked(id) != cached {
					t.Fatal("refusal changed user/wrapper/cache")
				}
				if disable {
					plain, err := v.OpenAtEpoch(ctx, id, store.KindCodexAuth, cacheProbe.SealedWith, cacheProbe.Ciphertext, state.VaultEpoch)
					if err != nil || string(plain) != "cache continuity" {
						t.Fatal("refusal changed cached DEK")
					}
				}
				// Check ALL stored bytes and markers against the exact seeded ciphertext.
				var bad int
				if err := pool.QueryRow(ctx, `SELECT
     (SELECT count(*) FROM user_secrets WHERE user_id=$1 AND (sealed_with IS DISTINCT FROM $2 OR ciphertext IS DISTINCT FROM CASE WHEN label='extra' THEN $4::bytea ELSE $3::bytea END))+
     (SELECT count(*) FROM codex_provider_account WHERE user_id=$1 AND (sealed_with IS DISTINCT FROM $2 OR recovery_sealed_with IS DISTINCT FROM $2 OR sealed_login IS DISTINCT FROM $5::bytea OR recovery_sealed IS DISTINCT FROM $6::bytea))`,
					id, marker, primary, extra, live, recovery).Scan(&bad); err != nil {
					t.Fatal(err)
				}
				if bad != 0 {
					t.Fatalf("refusal changed %d ciphertext records", bad)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				after, err := v.State(ctx, id)
				wantMode := store.VaultModeProtected
				if disable {
					wantMode = store.VaultModeUnprotected
				}
				if err != nil || after.VaultMode != wantMode || after.VaultEpoch != state.VaultEpoch+1 || v.Unlocked(id) == disable {
					t.Fatalf("converted state=%+v cached=%v err=%v", after, v.Unlocked(id), err)
				}
				exists, err := v.Exists(ctx, id)
				if err != nil || exists == disable {
					t.Fatalf("converted wrapper exists=%v err=%v", exists, err)
				}
				// Streaming keeps the 64 MiB fixture out of aggregate snapshots and diffs.
				rows, err := pool.Query(ctx, `SELECT kind,ciphertext,sealed_with FROM user_secrets WHERE user_id=$1
     UNION ALL SELECT 'codex_auth',sealed_login,sealed_with FROM codex_provider_account WHERE user_id=$1
     UNION ALL SELECT 'codex_auth',recovery_sealed,recovery_sealed_with FROM codex_provider_account WHERE user_id=$1`, id)
				if err != nil {
					t.Fatal(err)
				}
				var n int64
				for rows.Next() {
					var kind, m string
					var ct []byte
					if err := rows.Scan(&kind, &ct, &m); err != nil {
						rows.Close()
						t.Fatal(err)
					}
					plain, err := v.Open(id, kind, m, ct)
					wantSize := tc.size
					if kind == store.KindCodexAuth {
						wantSize = 524288
					}
					if m != target || err != nil || !bytes.Equal(plain, bytes.Repeat([]byte{'b'}, wantSize-28)) {
						rows.Close()
						t.Fatalf("material %d marker=%s open=%v", n, m, err)
					}
					n++
				}
				rows.Close()
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				if n != tc.rows {
					t.Fatalf("readable slots=%d want=%d", n, tc.rows)
				}
			}
			var changed int
			if err := pool.QueryRow(ctx, `WITH actual AS (
    SELECT user_id AS owner,'secret' AS collection,id,to_jsonb(s)-ARRAY['ciphertext','sealed_with'] AS metadata FROM user_secrets s WHERE user_id=$1
    UNION ALL SELECT user_id,'account',id,to_jsonb(a)-ARRAY['sealed_login','sealed_with','recovery_sealed','recovery_sealed_with'] FROM codex_provider_account a WHERE user_id=$1
   ), expected AS (SELECT * FROM vault_budget_metadata WHERE owner=$1)
   SELECT count(*) FROM actual FULL JOIN expected USING(owner,collection,id) WHERE actual.metadata IS DISTINCT FROM expected.metadata`, id).Scan(&changed); err != nil {
				t.Fatal(err)
			}
			if changed != 0 {
				t.Fatalf("metadata changed on %d records", changed)
			}
			var slots int64
			if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM user_secrets WHERE user_id=$1)+(SELECT count(*)+count(recovery_sealed) FROM codex_provider_account WHERE user_id=$1)`, id).Scan(&slots); err != nil {
				t.Fatal(err)
			}
			if slots != tc.rows {
				t.Fatalf("slot count changed: %d want %d", slots, tc.rows)
			}
		})
	}
}
