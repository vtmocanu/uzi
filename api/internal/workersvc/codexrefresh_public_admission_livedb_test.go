package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Exercise the portable public entry point: disconnecting its admission callback
// must start a provider call and fail this regression, even when the final release
// recheck still withholds the token. Runtime proof belongs to exact-head LiveDB CI.
func TestCodexRefreshPublicLateAdmissionLiveDB(t *testing.T) {
	for _, mutation := range []string{"revoke", "unbind", "valid replacement"} {
		t.Run(mutation, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: codexToken("new")}}
			f := newRefreshFixture(t, env, fake)
			env.exec("UPDATE runs SET harness='codex' WHERE id=$1", f.runID)
			capability := env.mintCap(t, f.runID, f.workerID)
			var replacement uuid.UUID
			if mutation == "valid replacement" {
				replacement = env.seedLinkedSubscription(t, f.userID, "replacement", codexToken("other-access"), codexToken("other-refresh"))
			}
			changed := false
			f.svc.q = intentReadHookStore{
				Queries: env.q,
				onIntentRead: func(_ store.CodexRefreshIntent, readErr error) {
					if changed || !errors.Is(readErr, pgx.ErrNoRows) {
						return
					}
					changed = true
					switch mutation {
					case "revoke":
						env.exec("UPDATE runs SET codex_claim_epoch=codex_claim_epoch+1,codex_cap_hash=NULL WHERE id=$1", f.runID)
					case "unbind":
						env.exec("UPDATE runs SET codex_secret_id=NULL WHERE id=$1", f.runID)
					case "valid replacement":
						st, err := env.q.GetCodexCredentialState(env.ctx, store.GetCodexCredentialStateParams{UserSecretID: replacement, UserID: f.userID})
						if err != nil || !st.ProviderAccountID.Valid {
							t.Fatalf("replacement state missing: %v", err)
						}
						acct := env.mustAccount(t, f.userID, uuid.UUID(st.ProviderAccountID.Bytes))
						key, err := codexAccountKey(acct.ProviderUserID, acct.WorkspaceAccountID)
						if err != nil {
							t.Fatal(err)
						}
						// Preserve the cap deliberately to isolate the captured-binding guard.
						// The real owner switch additionally revokes and remints it atomically.
						env.exec(`UPDATE runs SET codex_secret_id=$2,codex_secret_label='replacement',
       codex_material_revision=$3,codex_account_key=$4,codex_account_revision=$5 WHERE id=$1`,
							f.runID, replacement, st.MaterialRevision, key, acct.CredentialRevision)
					}
				},
			}
			op := uuid.New()
			res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capability, op, 0)
			want := ErrCodexCapabilityEpoch
			if mutation == "unbind" {
				want = ErrCodexRunNotBound
			}
			if mutation == "valid replacement" {
				want = ErrCodexBindingConflict
			}
			if !changed || !errors.Is(err, want) || res.AccessToken != "" || fake.calls != 0 {
				t.Fatalf("late public admission: changed=%t token_present=%t calls=%d err=%v want=%v", changed, res.AccessToken != "", fake.calls, err, want)
			}
			if _, err := env.q.GetCodexRefreshIntent(env.ctx, store.GetCodexRefreshIntentParams{OperationID: op, UserID: f.userID}); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("late public request left intent: %v", err)
			}
			if acct := env.mustAccount(t, f.userID, f.accountID); acct.Generation != 0 || acct.CoordOperationID.Valid {
				t.Fatal("late public request acquired an old-account lease")
			}
		})
	}
}
