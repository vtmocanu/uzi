package workersvc

import (
	"context"
	"slices"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// fakeRunEnv is the in-memory variable state behind fakeStore and its finish transaction (PRD #4416
// M2). The key lists the finish recompute reads are derived from effective and redact, so a test
// that changes those between assembly and finish (claimTestHooks.afterAssembly) moves the keys
// too. keysEffective/keysRedact, when non-nil, replace the derived keys.
type fakeRunEnv struct {
	effective []store.ListRunEnvEffectiveForRunRow
	redact    []store.ListRunEnvRedactForRunRow

	keysEffective []store.ListRunEnvEffectiveKeysForRunRow
	keysRedact    []store.ListRunEnvRedactKeysForRunRow

	shareLocks   int
	shareLockErr error
	receipts     []store.UpsertRunEnvReceiptParams // committed only
	parked       *store.ParkRunEnvHistoryUnavailableRunParams
}

func (f *fakeStore) ListRunEnvEffectiveForRun(context.Context, store.ListRunEnvEffectiveForRunParams) ([]store.ListRunEnvEffectiveForRunRow, error) {
	if f.runEnv == nil {
		return nil, nil
	}
	return slices.Clone(f.runEnv.effective), nil
}

func (f *fakeStore) ListRunEnvRedactForRun(context.Context, store.ListRunEnvRedactForRunParams) ([]store.ListRunEnvRedactForRunRow, error) {
	if f.runEnv == nil {
		return nil, nil
	}
	return slices.Clone(f.runEnv.redact), nil
}

func (t *fakeClaimFinishTx) ListRunEnvEffectiveKeysForRun(context.Context, store.ListRunEnvEffectiveKeysForRunParams) ([]store.ListRunEnvEffectiveKeysForRunRow, error) {
	re := t.f.runEnv
	if re == nil {
		return nil, nil
	}
	if re.keysEffective != nil {
		return re.keysEffective, nil
	}
	out := make([]store.ListRunEnvEffectiveKeysForRunRow, 0, len(re.effective))
	for _, r := range re.effective {
		out = append(out, store.ListRunEnvEffectiveKeysForRunRow{VarID: r.VarID, Rev: r.Rev, Name: r.Name})
	}
	return out, nil
}

func (t *fakeClaimFinishTx) ListRunEnvRedactKeysForRun(context.Context, store.ListRunEnvRedactKeysForRunParams) ([]store.ListRunEnvRedactKeysForRunRow, error) {
	re := t.f.runEnv
	if re == nil {
		return nil, nil
	}
	if re.keysRedact != nil {
		return re.keysRedact, nil
	}
	out := make([]store.ListRunEnvRedactKeysForRunRow, 0, len(re.redact))
	for _, r := range re.redact {
		out = append(out, store.ListRunEnvRedactKeysForRunRow{VarID: r.VarID, Rev: r.Rev, ReceiptName: r.ReceiptName, Present: r.RowID.Valid, Effective: r.Effective})
	}
	return out, nil
}

func (t *fakeClaimFinishTx) UpsertRunEnvReceipt(_ context.Context, arg store.UpsertRunEnvReceiptParams) error {
	t.pending = append(t.pending, func() {
		if t.f.runEnv == nil {
			t.f.runEnv = &fakeRunEnv{}
		}
		t.f.runEnv.receipts = append(t.f.runEnv.receipts, arg)
	})
	return nil
}

func (t *fakeClaimFinishTx) LockUserVaultStateForShareNowait(context.Context, uuid.UUID) (store.LockUserVaultStateForShareNowaitRow, error) {
	if t.f.runEnv == nil {
		t.f.runEnv = &fakeRunEnv{}
	}
	t.f.runEnv.shareLocks++
	if err := t.f.runEnv.shareLockErr; err != nil {
		t.f.runEnv.shareLockErr = nil // one 55P03, then the retry succeeds
		return store.LockUserVaultStateForShareNowaitRow{}, err
	}
	return store.LockUserVaultStateForShareNowaitRow{VaultMode: store.VaultModeUnprotected}, nil
}

func (t *fakeClaimFinishTx) ParkRunEnvHistoryUnavailableRun(_ context.Context, arg store.ParkRunEnvHistoryUnavailableRunParams) (int64, error) {
	t.pending = append(t.pending, func() {
		if t.f.runEnv == nil {
			t.f.runEnv = &fakeRunEnv{}
		}
		t.f.runEnv.parked = &arg
	})
	return 1, nil
}
