package workersvc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/secretopen"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The unit fixture exercises production mode/epoch fencing with a transaction
// carrying its in-memory state. Real locking and rollback remain LiveDB proofs.
type reuseWorkerBeginner struct{ memory *memVaultStore }
type reuseWorkerTx struct {
	pgx.Tx
	memory *memVaultStore
}
type reuseWorkerRow struct {
	state store.GetUserVaultStateRow
	err   error
}

func (b reuseWorkerBeginner) Begin(context.Context) (pgx.Tx, error) {
	return &reuseWorkerTx{memory: b.memory}, nil
}
func (tx *reuseWorkerTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if sql != "SET TRANSACTION ISOLATION LEVEL READ COMMITTED" && !strings.Contains(sql, "pg_advisory_xact_lock") {
		return pgconn.CommandTag{}, fmt.Errorf("reuse fixture: unexpected statement %q", sql)
	}
	return pgconn.NewCommandTag("SELECT 1"), ctx.Err()
}
func (tx *reuseWorkerTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if !strings.Contains(sql, "SELECT vault_mode, vault_epoch FROM users") || len(args) != 1 {
		return reuseWorkerRow{err: fmt.Errorf("reuse fixture: unexpected query %q", sql)}
	}
	state, err := tx.memory.GetUserVaultState(ctx, args[0].(uuid.UUID))
	return reuseWorkerRow{state: state, err: err}
}
func (row reuseWorkerRow) Scan(dest ...any) error {
	if row.err != nil {
		return row.err
	}
	if len(dest) != 2 {
		return fmt.Errorf("reuse fixture: unexpected column count %d", len(dest))
	}
	*dest[0].(*string) = row.state.VaultMode
	*dest[1].(*int64) = row.state.VaultEpoch
	return nil
}
func (*reuseWorkerTx) Commit(ctx context.Context) error                         { return ctx.Err() }
func (*reuseWorkerTx) Rollback(context.Context) error                           { return nil }
func (f *reuseWorkerStore) bindCodexRefreshTx(*store.Queries) codexRefreshStore { return f }
func (f *reuseWorkerStore) GetCodexProviderAccountSnapshot(ctx context.Context, arg store.GetCodexProviderAccountSnapshotParams) (store.GetCodexProviderAccountSnapshotRow, error) {
	acct, err := f.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams(arg))
	return store.GetCodexProviderAccountSnapshotRow{CodexProviderAccount: acct, VaultEpoch: f.epoch}, err
}
func openReuseSealed(s *Service, userID uuid.UUID, ciphertext []byte, marker string) (codexLoginBlob, error) {
	state, err := s.vlt.State(context.Background(), userID)
	if err != nil {
		return codexLoginBlob{}, err
	}
	plain, err := secretopen.OpenSealedAtEpoch(context.Background(), s.vlt, s.box, userID, store.KindCodexAuth, marker, ciphertext, state.VaultEpoch)
	if err != nil {
		return codexLoginBlob{}, err
	}
	defer clear(plain)
	var blob codexLoginBlob
	err = json.Unmarshal(plain, &blob)
	return blob, err
}

// Main's retry-reuse path must obey the branch's durable vault mode rather
// than requiring a cached DEK for an owner who has opted out.
func TestCodexReuseWorkerUnprotected(t *testing.T) {
	s, st, provider, admit := newReuseWorker(t)
	st.memory.mu.Lock()
	st.memory.states[st.acct.UserID] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeUnprotected, VaultEpoch: 1}
	st.memory.mu.Unlock()
	st.epoch = 1
	s.vlt.Lock(st.acct.UserID)
	resolved, changed, err := s.reconcileCodexRefreshWithReuse(context.Background(), st.acct.UserID, st.acct.ID, admit)
	if err != nil || !changed || resolved != 1 || provider.calls != 1 || st.promotes != 1 {
		t.Fatalf("unprotected reuse: resolved=%d changed=%t exchanges=%d promotions=%d err=%v", resolved, changed, provider.calls, st.promotes, err)
	}
	if st.lastWrite.RecoverySealedWith.String != store.SealedWithMasterAAD || st.lastWrite.RecoveryCause.Valid {
		t.Fatal("unprotected reuse must persist AAD master material without a vault-lock cause")
	}
}
