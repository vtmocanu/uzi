package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type baselineClaimStore struct {
	*fakeStore
	run                              store.Run
	writeErr, commitErr, rollbackErr error
	replace                          bool
	committed                        bool
	writes, begins                   int
}
type baselineClaimTx struct {
	*fakeClaimFinishTx
	owner *baselineClaimStore
	wrote bool
}

func (s *baselineClaimStore) BeginClaimFinish(context.Context) (claimFinishTx, error) {
	s.begins++
	return &baselineClaimTx{fakeClaimFinishTx: &fakeClaimFinishTx{f: s.fakeStore}, owner: s}, nil
}
func (t *baselineClaimTx) GetRunOwnedByWorkerForUpdate(context.Context, store.GetRunOwnedByWorkerForUpdateParams) (store.Run, error) {
	return t.owner.run, nil
}
func (t *baselineClaimTx) LockCodexAliasForShareNowait(context.Context, store.LockCodexAliasForShareNowaitParams) (store.LockCodexAliasForShareNowaitRow, error) {
	return store.LockCodexAliasForShareNowaitRow{MaterialRevision: 1}, nil
}
func (t *baselineClaimTx) GetRunCodexAuthContext(context.Context, uuid.UUID) (store.GetRunCodexAuthContextRow, error) {
	return store.GetRunCodexAuthContextRow{
		CodexAuthMode: pgconv.TextOrNull(codexAuthModeAPIKey), BoundKind: store.KindOpenAIAPIKey,
		Status: "claimed", CodexMaterialRevision: pgtype.Int8{Int64: 1, Valid: true}, CurrentMaterialRevision: 1,
	}, nil
}
func (t *baselineClaimTx) RecordCodexCredentialEpoch(_ context.Context, arg store.RecordCodexCredentialEpochParams) error {
	t.owner.writes++
	t.wrote = true
	return t.owner.writeErr
}
func (t *baselineClaimTx) Rollback(ctx context.Context) error {
	if t.owner.replace && t.wrote {
		t.owner.run.CodexClaimEpoch++
		t.owner.replace = false
	}
	if t.owner.rollbackErr != nil {
		return t.owner.rollbackErr
	}
	return t.fakeClaimFinishTx.Rollback(ctx)
}
func (t *baselineClaimTx) Commit(ctx context.Context) error {
	if t.wrote {
		t.owner.committed = true
		if t.owner.commitErr != nil {
			return t.owner.commitErr
		}
	}
	return t.fakeClaimFinishTx.Commit(ctx)
}

func TestFinishRunClaimCodexBaseline(t *testing.T) {
	sqlErr := errors.New("baseline SQL failed")
	lostReply := errors.New("commit reply lost")
	for _, tc := range []struct {
		name                                   string
		writeErr, commitErr, rollbackErr       error
		replace                                bool
		wantBegins                             int
		wantFailed, wantCommitted, wantPayload bool
	}{
		{name: "success", wantBegins: 1, wantCommitted: true, wantPayload: true},
		{name: "confirmed rollback settles exact mint", writeErr: sqlErr, wantBegins: 2, wantFailed: true},
		{name: "replacement survives settlement", writeErr: sqlErr, replace: true, wantBegins: 2},
		{name: "lost commit reply does not settle", commitErr: lostReply, wantBegins: 1, wantCommitted: true},
		{name: "unconfirmed rollback does not settle", writeErr: sqlErr, rollbackErr: errors.New("rollback reply lost"), wantBegins: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs, _, run, id := finishFixture(t)
			run.Harness = harnessCodex
			run.Status = "claimed"
			run.WorkerID = pgconv.UUID(id.workerID)
			run.CodexSecretID = pgconv.UUID(uuid.New())
			run.CodexMaterialRevision = pgtype.Int8{Int64: 1, Valid: true}
			run.CodexAuthMode = pgconv.TextOrNull(codexAuthModeAPIKey)
			// The claim snapshot predates mint 7; the payload names the actual mint.
			locked := run
			locked.CodexClaimEpoch = 7
			locked.CodexCapHash = hashCodexCapability("baseline-test-capability")
			bs := &baselineClaimStore{fakeStore: fs, run: locked, writeErr: tc.writeErr, commitErr: tc.commitErr, rollbackErr: tc.rollbackErr, replace: tc.replace}
			svc := New(bs, nil, testParams())
			payload := &ClaimPayload{Secrets: ClaimSecrets{Codex: &ClaimCodexSecrets{
				AuthMode: codexAuthModeAPIKey, Capability: formatCodexCapability(7, "baseline-test-capability"),
			}}}
			got, err := svc.finishRunClaim(context.Background(), run, payload, nil, id)
			if (err != nil) != (tc.writeErr != nil || tc.commitErr != nil) {
				t.Fatalf("error=%v", err)
			}
			if (got != nil) != tc.wantPayload || bs.begins != tc.wantBegins || bs.writes != 1 ||
				(fs.claimFailed != nil) != tc.wantFailed || bs.committed != tc.wantCommitted {
				t.Fatalf("payload=%v begins=%d writes=%d failed=%v committed=%v err=%v", got != nil, bs.begins, bs.writes, fs.claimFailed, bs.committed, err)
			}
			if fs.claimFailed != nil && (fs.claimFailed.ClaimGeneration != run.ClaimGeneration || fs.claimFailed.WorkerID != run.WorkerID) {
				t.Fatal("settlement changed claim identity")
			}
		})
	}
}
