package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runenv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
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

	// runErrFrom and openErrFrom, when positive, make the authority and openness reads fail with
	// a database error from that call number on (1 is the first call).
	runErrFrom, openErrFrom int
	runCalls, openCalls     int
}

func (q *receiptRowsStore) ListRunEnvSecretReceiptVersionsForRun(context.Context, uuid.UUID) ([]store.ListRunEnvSecretReceiptVersionsForRunRow, error) {
	return q.rows, nil
}

func (q *receiptRowsStore) GetRunByIDForUser(context.Context, store.GetRunByIDForUserParams) (store.Run, error) {
	q.runCalls++
	if q.runErrFrom > 0 && q.runCalls >= q.runErrFrom {
		return store.Run{}, errors.New("connection reset")
	}
	return q.run, nil
}

func (q *receiptRowsStore) RunEnvRedactOpen(context.Context, uuid.UUID) (bool, error) {
	q.openCalls++
	if q.openErrFrom > 0 && q.openCalls >= q.openErrFrom {
		return false, errors.New("connection reset")
	}
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

// scriptedStateVaultStore serves the owner's vault state (unprotected, epoch 0) for its first
// okReads reads, then either a database error (invalid false) or a state row with an
// unrecognised mode (invalid true).
type scriptedStateVaultStore struct {
	*memVaultStore
	okReads int
	invalid bool
	reads   int
}

func (s *scriptedStateVaultStore) GetUserVaultState(ctx context.Context, id uuid.UUID) (store.GetUserVaultStateRow, error) {
	s.reads++
	if s.reads <= s.okReads {
		return s.memVaultStore.GetUserVaultState(ctx, id)
	}
	if s.invalid {
		return store.GetUserVaultStateRow{VaultMode: "unrecognised"}, nil
	}
	return store.GetUserVaultStateRow{}, errors.New("connection reset")
}

// openableRedactFixture is a RunEnvRedact fixture whose one receipted version opens (master
// AAD sealed, unprotected vault, epoch 0) and whose owner's vault state read is scripted.
func openableRedactFixture(t *testing.T, okReads int, invalid bool) (*Service, *receiptRowsStore, store.Worker, uuid.UUID) {
	t.Helper()
	owner, lead := uuid.New(), uuid.New()
	box, err := secretbox.New(make([]byte, secretbox.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	m := newMemVaultStore()
	m.defaultMode = store.VaultModeUnprotected
	varID := uuid.New()
	kind, err := runenv.AADFor(2, owner, runenv.ScopeUser, nil, true, "API_KEY", varID, 1)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := newMemoryVault(box, m).Seal(context.Background(), owner, kind, []byte("value-for-the-test"), vault.SealForUse)
	if err != nil {
		t.Fatal(err)
	}
	q := &receiptRowsStore{rows: []store.ListRunEnvSecretReceiptVersionsForRunRow{{
		VarID: varID, Rev: 1, Name: "API_KEY", Scope: "user",
		RowID:      pgtype.UUID{Bytes: uuid.New(), Valid: true},
		Ciphertext: sealed.Ciphertext,
		SealedWith: pgtype.Text{String: sealed.SealedWith, Valid: true},
		AadVersion: pgtype.Int2{Int16: 2, Valid: true},
		RowSecret:  pgtype.Bool{Bool: true, Valid: true},
		RowUserID:  pgtype.UUID{Bytes: owner, Valid: true},
		RowName:    pgtype.Text{String: "API_KEY", Valid: true},
	}}}
	wkr := store.Worker{ID: uuid.New(), UserID: owner}
	q.run = store.Run{ID: lead, UserID: owner, Kind: runkind.Issue, WorkerID: pgconv.UUID(wkr.ID)}
	scripted := &scriptedStateVaultStore{memVaultStore: m, okReads: okReads, invalid: invalid}
	return &Service{q: q, box: box, vlt: vault.New(box, scripted, m.transaction)}, q, wkr, lead
}

func TestRunEnvRedactPostOpenFailures(t *testing.T) {
	cases := []struct {
		name        string
		okReads     int
		invalid     bool
		runErrFrom  int
		openErrFrom int
		want        error // sentinel the error must match
		notWant     []error
	}{
		// The open reads the vault state once; the post-open read is the second.
		{name: "state read database error", okReads: 1, want: secretopen.ErrReaderFailure,
			notWant: []error{ErrRunEnvHistoryUnavailable, ErrRunEnvVaultLocked}},
		{name: "state read invalid state", okReads: 1, invalid: true, want: ErrRunEnvHistoryUnavailable,
			notWant: []error{secretopen.ErrReaderFailure}},
		// The authority read runs before the open and once after it.
		{name: "post-open authority read database error", okReads: 100, runErrFrom: 2, want: errRunEnvTestDB},
		{name: "post-open openness read database error", okReads: 100, openErrFrom: 2, want: errRunEnvTestDB},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, q, wkr, lead := openableRedactFixture(t, c.okReads, c.invalid)
			q.runErrFrom, q.openErrFrom = c.runErrFrom, c.openErrFrom
			res, err := s.RunEnvRedact(context.Background(), wkr, lead)
			if err == nil || len(res.Versions) != 0 || res.Protected {
				t.Fatalf("err=%v versions=%d protected=%v; want an error and no version", err, len(res.Versions), res.Protected)
			}
			if c.want == errRunEnvTestDB {
				if errors.Is(err, ErrRunEnvHistoryUnavailable) || errors.Is(err, ErrRunEnvVaultLocked) || errors.Is(err, ErrRunEnvRedactClosed) || errors.Is(err, ErrRunNotOwned) {
					t.Fatalf("err=%v; a database error is not a typed verdict", err)
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("err=%v; want %v", err, c.want)
			}
			for _, n := range c.notWant {
				if errors.Is(err, n) {
					t.Fatalf("err=%v; must not be %v", err, n)
				}
			}
		})
	}
}

// errRunEnvTestDB marks the cases whose error is any untyped database failure.
var errRunEnvTestDB = errors.New("any database failure")

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
