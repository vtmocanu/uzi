package workersvc

import (
	"bytes"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// TestVaultModeQueuedIssueRestartLiveDB exercises a queued issue across an API
// restart represented by fresh Service and Vault objects on the same DB and master.
func TestVaultModeQueuedIssueRestartLiveDB(t *testing.T) {
	for _, mode := range []string{store.VaultModeUnprotected, store.VaultModeProtected} {
		t.Run(mode, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			userID, workerID, repoID := env.seedCodexInfra(t)
			env.sealBotPAT(t, userID)
			var forgeCiphertext []byte
			if err := env.pool.QueryRow(env.ctx,
				"SELECT token_ciphertext FROM forge_connections WHERE user_id = $1",
				userID).Scan(&forgeCiphertext); err != nil {
				t.Fatalf("read seeded forge PAT: %v", err)
			}
			forgePAT, err := env.box.Open(forgeCiphertext)
			if err != nil {
				t.Fatalf("open seeded forge PAT: %v", err)
			}

			// The original API has its own service and policy-aware writer vault.
			writerVault := vault.NewPostgres(env.box, env.pool)
			writerSvc := snapshotSvc(env, testParams())
			writerSvc.SetVault(writerVault)
			if mode == store.VaultModeProtected {
				enrollCodexProtectedVault(t, env, writerVault, userID, codexVaultLockedTestPassword)
			}
			anthropicToken := codexToken("anthropic-restart")
			sealed, err := writerSvc.vlt.Seal(env.ctx, userID, store.KindAnthropicToken,
				[]byte(anthropicToken), vault.SealForUse)
			if err != nil {
				t.Fatalf("seal Anthropic credential: %v", err)
			}
			if sealed.Mode != mode {
				t.Fatalf("seal mode = %q, want %q", sealed.Mode, mode)
			}
			tx, err := env.pool.BeginTx(env.ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			if err != nil {
				t.Fatalf("begin credential writer: %v", err)
			}
			defer func() { _ = tx.Rollback(env.ctx) }()
			qtx := store.New(tx)
			if err := qtx.LockSecretMutation(env.ctx, userID); err != nil {
				t.Fatalf("lock credential mutation: %v", err)
			}
			if _, err := qtx.FenceVaultWrite(env.ctx, userID, sealed.Mode, sealed.Epoch); err != nil {
				t.Fatalf("fence credential write: %v", err)
			}
			secret, err := qtx.InsertUserSecret(env.ctx, store.InsertUserSecretParams{
				UserID: userID, Kind: store.KindAnthropicToken, Label: "restart-default",
				WantDefault: true, Ciphertext: sealed.Ciphertext, SealedWith: sealed.SealedWith,
			})
			if err != nil {
				t.Fatalf("insert credential: %v", err)
			}
			if err := tx.Commit(env.ctx); err != nil {
				t.Fatalf("commit credential writer: %v", err)
			}

			readSecret := func() store.UserSecret {
				t.Helper()
				var row store.UserSecret
				err := env.pool.QueryRow(env.ctx, `SELECT id, user_id, kind, ciphertext,
					created_at, updated_at, sealed_with, label, is_default, auto_eligible,
					disabled_at, enablement_rev, anthropic_rejected_at, anthropic_success_generation
					FROM user_secrets WHERE id = $1 AND user_id = $2`, secret.ID, userID).Scan(
					&row.ID, &row.UserID, &row.Kind, &row.Ciphertext, &row.CreatedAt, &row.UpdatedAt,
					&row.SealedWith, &row.Label, &row.IsDefault, &row.AutoEligible, &row.DisabledAt,
					&row.EnablementRev, &row.AnthropicRejectedAt, &row.AnthropicSuccessGeneration)
				if err != nil {
					t.Fatalf("read credential snapshot: %v", err)
				}
				return row
			}
			beforeSecret := readSecret()
			beforeState, err := env.q.GetUserVaultState(env.ctx, userID)
			if err != nil {
				t.Fatalf("read pre-restart mode/epoch: %v", err)
			}
			if beforeState.VaultMode != mode || beforeState.VaultEpoch != sealed.Epoch ||
				beforeSecret.ID != secret.ID || beforeSecret.UserID != userID ||
				beforeSecret.Kind != store.KindAnthropicToken || beforeSecret.Label != "restart-default" ||
				!beforeSecret.IsDefault || beforeSecret.DisabledAt.Valid ||
				beforeSecret.SealedWith != sealed.SealedWith || !bytes.Equal(beforeSecret.Ciphertext, sealed.Ciphertext) {
				t.Fatal("seeded credential does not match fenced default credential")
			}
			beforeWrapper, wrapperErr := env.q.GetUserVault(env.ctx, userID)
			if mode == store.VaultModeProtected {
				if wrapperErr != nil || !writerVault.Unlocked(userID) {
					t.Fatalf("protected writer must have an enrolled, cached DEK: %v", wrapperErr)
				}
			} else if !errors.Is(wrapperErr, pgx.ErrNoRows) || writerVault.Unlocked(userID) {
				t.Fatalf("unprotected writer must have no wrapper or cached DEK: %v", wrapperErr)
			}

			// Queue before restart; bypass fleet spreading without a sleep.
			runID := seedUnassignedRun(t, env, userID, repoID, 0)
			env.exec("UPDATE runs SET updated_at = now() - interval '3 hours' WHERE id = $1", runID)
			queued, err := env.q.GetRunByID(env.ctx, runID)
			if err != nil {
				t.Fatalf("read queued issue: %v", err)
			}
			if queued.Status != "queued" || queued.WorkerID.Valid || queued.ClaimGeneration != 0 ||
				queued.UserID != userID || !queued.IssueIid.Valid || queued.Harness != "claude" {
				t.Fatal("pre-restart issue must be queued, unassigned, generation zero and Claude")
			}

			// A restarted API retains only Postgres and the master key, not either object.
			freshVault := vault.NewPostgres(env.box, env.pool)
			freshSvc := snapshotSvc(env, testParams())
			freshSvc.SetVault(freshVault)
			if freshSvc == writerSvc || freshVault == writerVault || freshVault.Unlocked(userID) {
				t.Fatal("restart must construct a new service and vault with an empty DEK cache")
			}
			env.exec("UPDATE workers SET last_heartbeat_at = now() WHERE id = $1", workerID)
			worker := wkrRow(t, env, workerID)
			payload, err := freshSvc.Claim(env.ctx, worker, nil)
			if err != nil {
				t.Fatalf("cold-cache claim: %v", err)
			}
			if mode == store.VaultModeProtected {
				if payload != nil {
					t.Fatal("protected cold-cache claim must wait for unlock")
				}
				waiting, err := env.q.GetRunByID(env.ctx, runID)
				if err != nil {
					t.Fatalf("read issue waiting for unlock: %v", err)
				}
				if waiting.Status != "queued" || waiting.WorkerID.Valid || waiting.ClaimGeneration != 0 ||
					waiting.UserID != userID || freshVault.Unlocked(userID) {
					t.Fatal("locked claim must leave the issue queued, unassigned and generation zero")
				}
				if err := freshVault.UnlockExisting(env.ctx, userID, codexVaultLockedTestPassword); err != nil {
					t.Fatalf("unlock restarted vault: %v", err)
				}
				env.exec("UPDATE workers SET last_heartbeat_at = now() WHERE id = $1", workerID)
				payload, err = freshSvc.Claim(env.ctx, wkrRow(t, env, workerID), nil)
				if err != nil {
					t.Fatalf("same restarted service claim after unlock: %v", err)
				}
			}
			if payload == nil {
				t.Fatal("claim must return the queued issue payload")
			}
			if payload.RunID != runID.String() || payload.Kind != "issue" ||
				payload.IssueIID == nil || *payload.IssueIID != queued.IssueIid.Int64 ||
				payload.IssueTitle != queued.IssueTitle || payload.IssueDescription != queued.IssueDescription ||
				payload.Repo.ID != repoID.String() || payload.Repo.DefaultBranch == nil ||
				*payload.Repo.DefaultBranch != "main" || payload.ClaimGeneration != 1 ||
				payload.Secrets.Codex != nil {
				t.Fatal("claim payload lost queued issue, repo, branch, Claude credentials or generation")
			}
			if payload.Secrets.AnthropicOAuthToken != anthropicToken || payload.Secrets.ForgePAT != string(forgePAT) {
				t.Fatal("claim must return the exact seeded Anthropic token and forge PAT")
			}
			claimed, err := env.q.GetRunByID(env.ctx, runID)
			if err != nil {
				t.Fatalf("read claimed issue: %v", err)
			}
			if claimed.ID != runID || claimed.UserID != userID || claimed.Status != "claimed" ||
				!claimed.WorkerID.Valid || claimed.WorkerID.Bytes != [16]byte(workerID) ||
				!claimed.RepoID.Valid || claimed.RepoID.Bytes != [16]byte(repoID) ||
				claimed.Harness != "claude" || claimed.ClaimGeneration != 1 ||
				!claimed.AnthropicSecretID.Valid || claimed.AnthropicSecretID.Bytes != [16]byte(secret.ID) ||
				!claimed.AnthropicSecretLabel.Valid || claimed.AnthropicSecretLabel.String != beforeSecret.Label ||
				!claimed.AnthropicSelectReason.Valid || claimed.AnthropicSelectReason.String != selectReasonDefault {
				t.Fatal("persisted claim lost owner, worker, repo, harness, credential binding or default reason")
			}
			var workerOwnedCredentials bool
			if err := env.pool.QueryRow(env.ctx, `SELECT EXISTS (
				SELECT 1 FROM runs r
				JOIN workers w ON w.id = r.worker_id AND w.user_id = r.user_id
				JOIN user_secrets s ON s.id = r.anthropic_secret_id AND s.user_id = w.user_id
				JOIN repos p ON p.id = r.repo_id
				JOIN forge_connections f ON f.id = p.connection_id AND f.user_id = w.user_id
				WHERE r.id = $1 AND w.id = $2 AND w.user_id = $3 AND s.id = $4
			)`, runID, workerID, userID, secret.ID).Scan(&workerOwnedCredentials); err != nil {
				t.Fatalf("read worker credential ownership: %v", err)
			}
			if !workerOwnedCredentials {
				t.Fatal("claimed worker must own both delivered credentials")
			}

			afterSecret := readSecret()
			if afterSecret.ID != beforeSecret.ID || afterSecret.UserID != beforeSecret.UserID ||
				afterSecret.Kind != beforeSecret.Kind || afterSecret.Label != beforeSecret.Label ||
				afterSecret.IsDefault != beforeSecret.IsDefault || afterSecret.AutoEligible != beforeSecret.AutoEligible ||
				afterSecret.DisabledAt != beforeSecret.DisabledAt || afterSecret.EnablementRev != beforeSecret.EnablementRev ||
				afterSecret.AnthropicRejectedAt != beforeSecret.AnthropicRejectedAt ||
				afterSecret.AnthropicSuccessGeneration != beforeSecret.AnthropicSuccessGeneration ||
				afterSecret.CreatedAt != beforeSecret.CreatedAt || afterSecret.UpdatedAt != beforeSecret.UpdatedAt ||
				afterSecret.SealedWith != beforeSecret.SealedWith || !bytes.Equal(afterSecret.Ciphertext, beforeSecret.Ciphertext) {
				t.Fatal("restart and claim must preserve credential identity, ciphertext and selection/enablement metadata")
			}
			afterState, err := env.q.GetUserVaultState(env.ctx, userID)
			if err != nil || afterState != beforeState {
				t.Fatalf("restart and claim changed vault mode/epoch: %v", err)
			}
			afterWrapper, err := env.q.GetUserVault(env.ctx, userID)
			if mode == store.VaultModeProtected {
				if err != nil || afterWrapper.UserID != beforeWrapper.UserID ||
					!bytes.Equal(afterWrapper.KekSalt, beforeWrapper.KekSalt) ||
					!bytes.Equal(afterWrapper.WrappedDek, beforeWrapper.WrappedDek) ||
					afterWrapper.CreatedAt != beforeWrapper.CreatedAt || afterWrapper.UpdatedAt != beforeWrapper.UpdatedAt {
					t.Fatalf("restart and claim changed enrolled wrapper material: %v", err)
				}
			} else if !errors.Is(err, pgx.ErrNoRows) || freshVault.Unlocked(userID) {
				t.Fatalf("unprotected claim must never create a wrapper or cache a DEK: %v", err)
			}
			var afterForgeCiphertext []byte
			if err := env.pool.QueryRow(env.ctx,
				"SELECT token_ciphertext FROM forge_connections WHERE user_id = $1",
				userID).Scan(&afterForgeCiphertext); err != nil {
				t.Fatalf("read forge ciphertext after claim: %v", err)
			}
			if !bytes.Equal(afterForgeCiphertext, forgeCiphertext) {
				t.Fatal("restart and claim changed forge credential ciphertext")
			}
		})
	}
}
