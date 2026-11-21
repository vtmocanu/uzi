package workersvc

import (
	"context"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// fakeRunEnv is the in-memory run environment state of fakeStore. The key lists the finish
// transaction recomputes are derived from the rows, so a test changes the world by editing
// the rows (typically from claimTestHooks.afterAssembly).
type fakeRunEnv struct {
	effective []store.ListRunEnvEffectiveForRunRow
	redact    []store.ListRunEnvRedactForRunRow

	receipts []store.UpsertRunEnvReceiptParams
	parked   *store.ParkRunEnvHistoryUnavailableRunParams
	// lockErrs are returned in order by LockUserVaultStateForKeyShareNowait; lockCalls counts calls.
	lockErrs  []error
	lockCalls int
}

func (f *fakeStore) ListRunEnvEffectiveForRun(context.Context, uuid.UUID) ([]store.ListRunEnvEffectiveForRunRow, error) {
	return f.runEnv.effective, nil
}

func (f *fakeStore) ListRunEnvRedactForRun(context.Context, uuid.UUID) ([]store.ListRunEnvRedactForRunRow, error) {
	return f.runEnv.redact, nil
}

func (f *fakeStore) ListRunEnvSecretReceiptVersionsForRun(context.Context, uuid.UUID) ([]store.ListRunEnvSecretReceiptVersionsForRunRow, error) {
	return nil, nil
}

func (f *fakeStore) WorkerHoldsOpenCustodyForRun(context.Context, store.WorkerHoldsOpenCustodyForRunParams) (bool, error) {
	return false, nil
}

func (f *fakeStore) RunEnvRedactOpen(context.Context, uuid.UUID) (bool, error) {
	return true, nil
}

func (t *fakeClaimFinishTx) LockUserVaultStateForKeyShareNowait(context.Context, uuid.UUID) (store.LockUserVaultStateForKeyShareNowaitRow, error) {
	t.f.runEnv.lockCalls++
	if len(t.f.runEnv.lockErrs) > 0 {
		err := t.f.runEnv.lockErrs[0]
		t.f.runEnv.lockErrs = t.f.runEnv.lockErrs[1:]
		if err != nil {
			return store.LockUserVaultStateForKeyShareNowaitRow{}, err
		}
	}
	return store.LockUserVaultStateForKeyShareNowaitRow{}, nil
}

func (t *fakeClaimFinishTx) ListRunEnvEffectiveKeysForRun(context.Context, uuid.UUID) ([]store.ListRunEnvEffectiveKeysForRunRow, error) {
	var out []store.ListRunEnvEffectiveKeysForRunRow
	for _, r := range t.f.runEnv.effective {
		out = append(out, store.ListRunEnvEffectiveKeysForRunRow{VarID: r.VarID, Rev: r.Rev, Name: r.Name, Secret: r.Secret})
	}
	return out, nil
}

func (t *fakeClaimFinishTx) ListRunEnvRedactKeysForRun(context.Context, uuid.UUID) ([]store.ListRunEnvRedactKeysForRunRow, error) {
	var out []store.ListRunEnvRedactKeysForRunRow
	for _, r := range t.f.runEnv.redact {
		out = append(out, store.ListRunEnvRedactKeysForRunRow{VarID: r.VarID, Rev: r.Rev, Name: r.Name, Scope: r.Scope, Present: r.RowID.Valid})
	}
	return out, nil
}

func (t *fakeClaimFinishTx) UpsertRunEnvReceipt(_ context.Context, arg store.UpsertRunEnvReceiptParams) error {
	t.pending = append(t.pending, func() { t.f.runEnv.receipts = append(t.f.runEnv.receipts, arg) })
	return nil
}

func (t *fakeClaimFinishTx) ParkRunEnvHistoryUnavailableRun(_ context.Context, arg store.ParkRunEnvHistoryUnavailableRunParams) (int64, error) {
	t.pending = append(t.pending, func() { t.f.runEnv.parked = &arg })
	return 1, nil
}
