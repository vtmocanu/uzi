package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/secretopen"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// PRD #4416 review: a failed read of the owner's vault state while opening the lead's receipted
// versions (secretopen.ErrReaderFailure) is a transient database failure. In the cross-check lane
// it must neither decide the check unavailable nor park, and the redact route must answer it as a
// plain error, never as a history verdict.

// failingStateVaultStore is a vault store whose owner vault-state read fails.
type failingStateVaultStore struct{ *memVaultStore }

func (failingStateVaultStore) GetUserVaultState(context.Context, uuid.UUID) (store.GetUserVaultStateRow, error) {
	return store.GetUserVaultStateRow{}, errors.New("connection reset")
}

// receiptRowsStore serves one receipted secret version and the run lookups the redact route makes.
type receiptRowsStore struct {
	Store
	run  store.Run
	rows []store.ListRunEnvSecretReceiptVersionsForRunRow
}

func (q *receiptRowsStore) ListRunEnvSecretReceiptVersionsForRun(context.Context, uuid.UUID) ([]store.ListRunEnvSecretReceiptVersionsForRunRow, error) {
	return q.rows, nil
}

func (q *receiptRowsStore) GetRunByIDForUser(context.Context, store.GetRunByIDForUserParams) (store.Run, error) {
	return q.run, nil
}

func (q *receiptRowsStore) RunEnvRedactOpen(context.Context, uuid.UUID) (bool, error) {
	return true, nil
}

func readerFailureFixture(owner, lead uuid.UUID) (*Service, *receiptRowsStore) {
	m := newMemVaultStore()
	row := store.ListRunEnvSecretReceiptVersionsForRunRow{
		VarID: uuid.New(), Rev: 1, Name: "API_KEY", Scope: "user",
		RowID:      pgtype.UUID{Bytes: uuid.New(), Valid: true},
		Ciphertext: []byte("not-opened"),
		SealedWith: pgtype.Text{String: store.SealedWithMasterAAD, Valid: true},
		AadVersion: pgtype.Int2{Int16: 2, Valid: true},
		RowSecret:  pgtype.Bool{Bool: true, Valid: true},
		RowUserID:  pgtype.UUID{Bytes: owner, Valid: true},
		RowName:    pgtype.Text{String: "API_KEY", Valid: true},
	}
	q := &receiptRowsStore{rows: []store.ListRunEnvSecretReceiptVersionsForRunRow{row}}
	q.run = store.Run{ID: lead, UserID: owner, Kind: runkind.Issue}
	return &Service{q: q, vlt: vault.New(nil, failingStateVaultStore{m}, m.transaction)}, q
}

func TestCrossCheckRedactReaderFailureIsTransient(t *testing.T) {
	owner, lead := uuid.New(), uuid.New()
	s, _ := readerFailureFixture(owner, lead)
	child := store.Run{ID: uuid.New(), UserID: owner, Kind: runkind.CrossCheck, TargetRunID: pgtype.UUID{Bytes: lead, Valid: true}}
	wkr := store.Worker{ProtocolCapabilities: []string{capability.RunEnvV1}}
	var payload ClaimPayload
	err := s.attachRunEnv(context.Background(), wkr, child, &payload)
	if !errors.Is(err, secretopen.ErrReaderFailure) {
		t.Fatalf("attachRunEnv = %v, want the reader failure passed through", err)
	}
	if errors.Is(err, errCheckerRunEnvUnavailable) || errors.Is(err, errCheckerPinUnavailable) || errors.Is(err, errRunEnvHistoryUnavailable) {
		t.Fatalf("attachRunEnv = %v, a reader failure must not decide the check or park", err)
	}
	if len(payload.Secrets.RunEnvRedact) != 0 {
		t.Fatalf("redact delivered after a failed open: %+v", payload.Secrets.RunEnvRedact)
	}
}

func TestRunEnvRedactReaderFailureIsNotHistoryUnavailable(t *testing.T) {
	owner, lead := uuid.New(), uuid.New()
	s, q := readerFailureFixture(owner, lead)
	wkr := store.Worker{ID: uuid.New(), UserID: owner}
	q.run.WorkerID = pgconv.UUID(wkr.ID)
	res, err := s.RunEnvRedact(context.Background(), wkr, lead)
	if !errors.Is(err, secretopen.ErrReaderFailure) {
		t.Fatalf("RunEnvRedact = %+v, %v; want the reader failure", res, err)
	}
	if errors.Is(err, ErrRunEnvHistoryUnavailable) || errors.Is(err, ErrRunEnvVaultLocked) || len(res.Versions) != 0 {
		t.Fatalf("RunEnvRedact = %+v, %v; a reader failure is neither a history verdict nor a version", res, err)
	}
}
