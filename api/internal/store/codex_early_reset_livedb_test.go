package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Each subtest owns a unique user/account. Operations are sequential and use no
// polling or retries; a failed prerequisite stops only its own subtest.
type codexEarlyResetFixture struct {
	ctx                  context.Context
	pool                 *pgxpool.Pool
	q                    *store.Queries
	user, account, alias uuid.UUID
	sig                  string
}

func newCodexEarlyResetFixture(t *testing.T) *codexEarlyResetFixture {
	t.Helper()
	ctx, pool, q, user := codexLiveDB(t)
	account, alias := mkLinkedCodexAccount(ctx, t, pool, q, user, "early-reset", false)
	mustExec(ctx, t, pool, "UPDATE users SET notify_early_limit_reset = true WHERE id = $1", user)
	return &codexEarlyResetFixture{ctx: ctx, pool: pool, q: q, user: user, account: account, alias: alias,
		sig: codexEnablementSig(ctx, t, q, user, account)}
}

func (f *codexEarlyResetFixture) success(t *testing.T, gen, rev int64) {
	t.Helper()
	n, err := f.q.UpsertCodexAccountRateLimits(f.ctx, store.UpsertCodexAccountRateLimitsParams{
		UserID: f.user, ProviderAccountID: f.account, EnablementSig: f.sig,
		ObservedGeneration: gen, ObservedCredentialRevision: rev,
		Buckets: []byte(`{"weekly":{"used_percent":40}}`), AttemptStatus: "ok",
	})
	if err != nil || n != 1 {
		t.Fatalf("success write = (%d, %v), want (1, nil)", n, err)
	}
}

func (f *codexEarlyResetFixture) failure(t *testing.T, sig string) {
	t.Helper()
	n, err := f.q.RecordCodexAccountPollFailure(f.ctx, store.RecordCodexAccountPollFailureParams{
		UserID: f.user, ProviderAccountID: f.account, EnablementSig: sig,
		AttemptStatus: "transient", AttemptError: "unavailable",
	})
	if err != nil || n != 1 {
		t.Fatalf("failure write = (%d, %v), want (1, nil)", n, err)
	}
}

func (f *codexEarlyResetFixture) baseline(t *testing.T, user, account uuid.UUID, sig string, want bool) {
	t.Helper()
	row, err := f.q.GetCodexEarlyResetBaseline(f.ctx, store.GetCodexEarlyResetBaselineParams{
		UserID: user, ProviderAccountID: account, EnablementSig: sig,
	})
	if !want {
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("baseline refusal = (%+v, %v), want ErrNoRows", row, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if row.UserID != user || row.ProviderAccountID != account || !row.LastSuccessAt.Valid ||
		!row.EnablementSig.Valid || row.EnablementSig.String != sig || !row.NotifyEarlyLimitReset {
		t.Fatalf("baseline lost identity, success, provenance or preference: %+v", row)
	}
	var pct int
	if err := f.pool.QueryRow(f.ctx, "SELECT ($1::jsonb #>> '{weekly,used_percent}')::int", row.Buckets).Scan(&pct); err != nil || pct != 40 {
		t.Fatalf("baseline buckets pct = (%d, %v), want (40, nil)", pct, err)
	}
}

func (f *codexEarlyResetFixture) current(t *testing.T, user, account uuid.UUID, sig string, gen, rev int64, want bool) {
	t.Helper()
	ok, err := f.q.IsCodexEarlyResetCurrent(f.ctx, store.IsCodexEarlyResetCurrentParams{
		UserID: user, ProviderAccountID: account, EnablementSig: sig,
		ObservedGeneration: gen, ObservedCredentialRevision: rev,
	})
	if want {
		if err != nil || !ok {
			t.Fatalf("currentness = (%v, %v), want (true, nil)", ok, err)
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("currentness refusal = (%v, %v), want ErrNoRows", ok, err)
	}
}

func (f *codexEarlyResetFixture) both(t *testing.T, baseline, current bool) {
	t.Helper()
	f.baseline(t, f.user, f.account, f.sig, baseline)
	f.current(t, f.user, f.account, f.sig, 0, 0, current)
}

func TestCodexEarlyResetBaselineLiveDB(t *testing.T) {
	t.Run("initial_zero_counters_and_ordinary_poll", func(t *testing.T) {
		f := newCodexEarlyResetFixture(t)
		f.both(t, false, true)
		f.success(t, 0, 0)
		f.both(t, true, true)
		original := getUserSnapshot(f.ctx, t, f.q, f.user, f.account)
		before, err := f.q.GetCodexEarlyResetBaseline(f.ctx, store.GetCodexEarlyResetBaselineParams{
			UserID: f.user, ProviderAccountID: f.account, EnablementSig: f.sig,
		})
		if err != nil {
			t.Fatal(err)
		}
		f.success(t, 0, 0)
		f.both(t, true, true)
		// A later ordinary poll may replace the gauge entirely without revoking
		// the queued authority fence.
		mustExec(f.ctx, t, f.pool, "UPDATE codex_account_rate_limits SET buckets = '{}' WHERE provider_account_id = $1", f.account)
		f.current(t, f.user, f.account, f.sig, 0, 0, true)
		if !before.LastSuccessAt.Valid || !before.LastSuccessAt.Time.Equal(original.LastSuccessAt.Time) {
			t.Fatal("baseline did not return the original stored success time")
		}
	})
	t.Run("first_failure", func(t *testing.T) {
		f := newCodexEarlyResetFixture(t)
		f.failure(t, f.sig)
		f.both(t, false, true)
	})
	for _, column := range []string{"last_success_at", "buckets", "enablement_sig"} {
		t.Run("missing_"+column, func(t *testing.T) {
			f := newCodexEarlyResetFixture(t)
			f.success(t, 0, 0)
			// column comes only from the fixed list above.
			mustExec(f.ctx, t, f.pool, "UPDATE codex_account_rate_limits SET "+column+" = NULL WHERE provider_account_id = $1", f.account)
			f.both(t, false, true)
		})
	}
	t.Run("json_null_data", func(t *testing.T) {
		f := newCodexEarlyResetFixture(t)
		f.success(t, 0, 0)
		mustExec(f.ctx, t, f.pool, "UPDATE codex_account_rate_limits SET buckets = 'null'::jsonb WHERE provider_account_id = $1", f.account)
		f.both(t, false, true)
	})
	t.Run("health_failure_preserves_success_then_provenance_failure_clears", func(t *testing.T) {
		f := newCodexEarlyResetFixture(t)
		f.success(t, 0, 0)
		f.failure(t, f.sig)
		f.both(t, true, true)
		setEnabled(f.ctx, t, f.q, f.user, f.alias, false)
		setEnabled(f.ctx, t, f.q, f.user, f.alias, true)
		fresh := codexEnablementSig(f.ctx, t, f.q, f.user, f.account)
		f.baseline(t, f.user, f.account, fresh, false)
		f.failure(t, fresh)
		f.baseline(t, f.user, f.account, fresh, false)
		snapshot := getUserSnapshot(f.ctx, t, f.q, f.user, f.account)
		if snapshot.Buckets != nil || snapshot.LastSuccessAt.Valid {
			t.Fatalf("failure retained obsolete data: %+v", snapshot)
		}
	})
	t.Run("owner_account_scope", func(t *testing.T) {
		f := newCodexEarlyResetFixture(t)
		f.success(t, 0, 0)
		foreign := newCodexEarlyResetFixture(t)
		foreign.success(t, 0, 0)
		for _, pair := range [][2]uuid.UUID{{foreign.user, f.account}, {f.user, foreign.account}, {f.user, uuid.New()}} {
			f.baseline(t, pair[0], pair[1], f.sig, false)
			f.current(t, pair[0], pair[1], f.sig, 0, 0, false)
		}
	})
	for _, notify := range []bool{false, true} {
		for _, wait := range []bool{false, true} {
			t.Run(fmt.Sprintf("notify_%t_wait_%t", notify, wait), func(t *testing.T) {
				f := newCodexEarlyResetFixture(t)
				f.success(t, 0, 0)
				mustExec(f.ctx, t, f.pool, "UPDATE users SET notify_early_limit_reset = $2, wait_on_limit = $3 WHERE id = $1", f.user, notify, wait)
				var storedNotify, storedWait bool
				if err := f.pool.QueryRow(f.ctx, "SELECT notify_early_limit_reset, wait_on_limit FROM users WHERE id = $1", f.user).Scan(&storedNotify, &storedWait); err != nil {
					t.Fatal(err)
				}
				if storedNotify != notify || storedWait != wait {
					t.Fatal("stored preferences differ")
				}
				f.both(t, notify, notify)
			})
		}
	}
}

func TestCodexEarlyResetSignatureLiveDB(t *testing.T) {
	t.Run("exact_known_signature", func(t *testing.T) {
		f := newCodexEarlyResetFixture(t)
		addLinkedAlias(f.ctx, t, f.pool, f.q, f.user, f.account, "second", false)
		f.sig = codexEnablementSig(f.ctx, t, f.q, f.user, f.account)
		f.success(t, 0, 0)
		f.both(t, true, true)
		parts := strings.Split(f.sig, ",")
		for _, sig := range []string{"", "unknown", parts[1] + "," + parts[0], parts[0], f.sig + ",extra:0"} {
			f.baseline(t, f.user, f.account, sig, false)
			f.current(t, f.user, f.account, sig, 0, 0, false)
		}
		mustExec(f.ctx, t, f.pool, "UPDATE codex_account_rate_limits SET enablement_sig = '' WHERE provider_account_id = $1", f.account)
		f.baseline(t, f.user, f.account, "", false)
	})
	t.Run("disabled_sibling_public_meter_survives", func(t *testing.T) {
		f := newCodexEarlyResetFixture(t)
		sibling := addLinkedAlias(f.ctx, t, f.pool, f.q, f.user, f.account, "sibling", false)
		f.sig = codexEnablementSig(f.ctx, t, f.q, f.user, f.account)
		f.success(t, 0, 0)
		setEnabled(f.ctx, t, f.q, f.user, sibling, false)
		if !getUserSnapshot(f.ctx, t, f.q, f.user, f.account).LastSuccessAt.Valid {
			t.Fatal("public meter should retain success after sibling disable")
		}
		f.both(t, false, false)
		fresh := codexEnablementSig(f.ctx, t, f.q, f.user, f.account)
		f.baseline(t, f.user, f.account, fresh, false)
		f.current(t, f.user, f.account, fresh, 0, 0, true)
	})
	t.Run("disabled_sibling_in_stored_signature", func(t *testing.T) {
		f := newCodexEarlyResetFixture(t)
		sibling := addLinkedAlias(f.ctx, t, f.pool, f.q, f.user, f.account, "disabled", false)
		setEnabled(f.ctx, t, f.q, f.user, sibling, false)
		f.sig = codexEnablementSig(f.ctx, t, f.q, f.user, f.account)
		f.success(t, 0, 0)
		f.both(t, true, true)
		parts := strings.Split(f.sig, ",")
		var enabledOnly string
		for _, part := range parts {
			if strings.HasPrefix(part, f.alias.String()+":") {
				enabledOnly = part
			}
		}
		f.baseline(t, f.user, f.account, enabledOnly, false)
		f.current(t, f.user, f.account, enabledOnly, 0, 0, false)
		// Even a matching supplied/stored subset cannot omit a disabled sibling.
		mustExec(f.ctx, t, f.pool, "UPDATE codex_account_rate_limits SET enablement_sig = $2 WHERE provider_account_id = $1", f.account, enabledOnly)
		f.baseline(t, f.user, f.account, enabledOnly, false)
	})
	t.Run("material_bump_alone_does_not_change_signature", func(t *testing.T) {
		f := newCodexEarlyResetFixture(t)
		f.success(t, 0, 0)
		n, err := f.q.BumpCodexMaterialRevision(f.ctx, store.BumpCodexMaterialRevisionParams{
			UserSecretID: f.alias, UserID: f.user, Status: "staging",
		})
		if err != nil || n != 1 {
			t.Fatalf("bump = (%d, %v)", n, err)
		}
		n, err = f.q.LinkCodexCredentialState(f.ctx, store.LinkCodexCredentialStateParams{
			UserSecretID: f.alias, UserID: f.user, MaterialRevision: 1,
			ProviderAccountID: pgtype.UUID{Bytes: f.account, Valid: true},
		})
		if err != nil || n != 1 {
			t.Fatalf("relink = (%d, %v)", n, err)
		}
		if fresh := codexEnablementSig(f.ctx, t, f.q, f.user, f.account); fresh != f.sig {
			t.Fatalf("material-only bump changed signature: %q != %q", fresh, f.sig)
		}
		f.both(t, true, true)
	})
	t.Run("last_alias_disable_reenable", func(t *testing.T) {
		f := newCodexEarlyResetFixture(t)
		f.success(t, 0, 0)
		setEnabled(f.ctx, t, f.q, f.user, f.alias, false)
		f.both(t, false, false)
		setEnabled(f.ctx, t, f.q, f.user, f.alias, true)
		f.both(t, false, false)
		fresh := codexEnablementSig(f.ctx, t, f.q, f.user, f.account)
		f.baseline(t, f.user, f.account, fresh, false)
		f.current(t, f.user, f.account, fresh, 0, 0, true)
	})
	t.Run("new_sibling_and_duplicate_dedup", func(t *testing.T) {
		f := newCodexEarlyResetFixture(t)
		f.success(t, 0, 0)
		addLinkedAlias(f.ctx, t, f.pool, f.q, f.user, f.account, "duplicate", false)
		f.both(t, false, false)
		f.sig = codexEnablementSig(f.ctx, t, f.q, f.user, f.account)
		f.success(t, 0, 0)
		f.both(t, true, true)
		rows, err := f.q.ListLinkedCodexAccountsForUser(f.ctx, f.user)
		if err != nil || len(rows) != 1 || rows[0].ProviderAccountID != f.account {
			t.Fatalf("duplicate account listing = (%+v, %v)", rows, err)
		}
	})
	for _, action := range []string{"unlink", "delete", "relink_elsewhere", "replace"} {
		t.Run(action, func(t *testing.T) {
			f := newCodexEarlyResetFixture(t)
			f.success(t, 0, 0)
			if action == "delete" {
				mustExec(f.ctx, t, f.pool, "DELETE FROM user_secrets WHERE id = $1", f.alias)
			} else {
				if action == "replace" {
					if _, err := f.q.RotateUserSecret(f.ctx, store.RotateUserSecretParams{
						ID: f.alias, UserID: f.user, Ciphertext: []byte("replacement"), SealedWith: store.SealedWithMaster,
					}); err != nil {
						t.Fatal(err)
					}
				}
				n, err := f.q.BumpCodexMaterialRevision(f.ctx, store.BumpCodexMaterialRevisionParams{
					UserSecretID: f.alias, UserID: f.user, Status: "staging",
				})
				if err != nil || n != 1 {
					t.Fatalf("bump = (%d, %v)", n, err)
				}
				f.both(t, false, false)
				if action == "relink_elsewhere" || action == "replace" {
					target := f.account
					if action == "relink_elsewhere" {
						target, _ = mkLinkedCodexAccount(f.ctx, t, f.pool, f.q, f.user, "elsewhere", false)
					}
					n, err := f.q.LinkCodexCredentialState(f.ctx, store.LinkCodexCredentialStateParams{
						UserSecretID: f.alias, UserID: f.user, MaterialRevision: 1,
						ProviderAccountID: pgtype.UUID{Bytes: target, Valid: true},
					})
					if err != nil || n != 1 {
						t.Fatalf("relink = (%d, %v)", n, err)
					}
				}
			}
			f.both(t, false, false)
			if action == "replace" {
				fresh := codexEnablementSig(f.ctx, t, f.q, f.user, f.account)
				if fresh == f.sig {
					t.Fatal("replacement did not advance enablement signature")
				}
				f.baseline(t, f.user, f.account, fresh, false)
				f.current(t, f.user, f.account, fresh, 0, 0, true)
			}
		})
	}
}

func TestCodexEarlyResetAuthorityLiveDB(t *testing.T) {
	t.Run("validated_refresh_continuity", func(t *testing.T) {
		f := newCodexEarlyResetFixture(t)
		f.success(t, 0, 0)
		op := uuid.New()
		n, err := f.q.AcquireCodexRefreshLease(f.ctx, store.AcquireCodexRefreshLeaseParams{
			ID: f.account, UserID: f.user, Op: op,
			Deadline: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
		})
		if err != nil || n != 1 {
			t.Fatalf("acquire = (%d, %v)", n, err)
		}
		f.both(t, true, false)
		committed, err := f.q.CommitCodexRefresh(f.ctx, store.CommitCodexRefreshParams{
			ID: f.account, UserID: f.user, Op: op, Sealed: []byte("refreshed"), SealedWith: store.SealedWithMaster,
		})
		if err != nil || committed.Generation != 1 {
			t.Fatalf("commit = (%+v, %v)", committed, err)
		}
		f.both(t, true, false)
		f.current(t, f.user, f.account, f.sig, 1, 0, true)
		n, err = f.q.ResetCodexCoordIdle(f.ctx, store.ResetCodexCoordIdleParams{ID: f.account, UserID: f.user})
		if err != nil || n != 1 {
			t.Fatalf("reset = (%d, %v)", n, err)
		}
		f.both(t, true, false)
		f.current(t, f.user, f.account, f.sig, 1, 0, true)
		mustExec(f.ctx, t, f.pool, "UPDATE codex_provider_account SET credential_revision = credential_revision + 1 WHERE id = $1", f.account)
		f.baseline(t, f.user, f.account, f.sig, true)
		f.current(t, f.user, f.account, f.sig, 1, 0, false)
		f.current(t, f.user, f.account, f.sig, 1, 1, true)
	})
	for _, state := range []string{"in_progress", "quarantined"} {
		t.Run(state, func(t *testing.T) {
			f := newCodexEarlyResetFixture(t)
			f.success(t, 0, 0)
			mustExec(f.ctx, t, f.pool, "UPDATE codex_provider_account SET coord_state = $2 WHERE id = $1", f.account, state)
			f.both(t, true, false)
		})
	}
	t.Run("recovery_material_in_idle", func(t *testing.T) {
		f := newCodexEarlyResetFixture(t)
		f.success(t, 0, 0)
		mustExec(f.ctx, t, f.pool, "UPDATE codex_provider_account SET recovery_sealed = 'recovery', recovery_sealed_with = 'master', recovery_generation = generation WHERE id = $1", f.account)
		f.both(t, true, false)
	})
	t.Run("reauth_required", func(t *testing.T) {
		f := newCodexEarlyResetFixture(t)
		f.success(t, 0, 0)
		mustExec(f.ctx, t, f.pool, "UPDATE codex_provider_account SET reauth_required = true, reauth_generation = generation, reauth_credential_revision = credential_revision, reauth_reason = 'provider_rejected' WHERE id = $1", f.account)
		f.both(t, true, false)
	})
}
