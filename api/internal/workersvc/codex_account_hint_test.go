package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type accountHintStore struct {
	*store.Queries
	row      store.GetRunCodexAuthContextRow
	run      store.Run
	alias    store.CodexCredentialState
	readErr  error
	aliasErr error
	runErr   error
}

func (q accountHintStore) GetRunCodexAuthContext(context.Context, uuid.UUID) (store.GetRunCodexAuthContextRow, error) {
	return q.row, q.readErr
}
func (q accountHintStore) GetRunByID(context.Context, uuid.UUID) (store.Run, error) {
	return q.run, q.runErr
}
func (q accountHintStore) GetCodexCredentialState(context.Context, store.GetCodexCredentialStateParams) (store.CodexCredentialState, error) {
	return q.alias, q.aliasErr
}

func hintFixture(t *testing.T, tc codexHoldCase, kind string, egress bool) (*Service, store.Worker, uuid.UUID, string) {
	t.Helper()
	run, row, status, material := tc.goInputs(t)
	run.ID, run.UserID, run.Kind = uuid.New(), uuid.New(), kind
	if egress {
		run.EgressProfileID = pgconv.UUID(uuid.New())
	}
	w := store.Worker{ID: uuid.New(), UserID: run.UserID}
	secret, hash := mintCodexCapability()
	row.WorkerID, row.CodexSecretID = pgconv.UUID(w.ID), pgconv.UUID(uuid.New())
	row.CodexClaimEpoch, row.CodexCapHash = 2, hash
	q := accountHintStore{run: run, row: row, alias: store.CodexCredentialState{Status: status, MaterialRevision: material}}
	return &Service{q: q}, w, run.ID, formatCodexCapability(2, secret)
}

func TestCodexAccountHintVerdictTable(t *testing.T) {
	states := []error{ErrCodexAccountQuarantined, ErrCodexRefreshQuarantined, ErrCodexRefreshUnrecoverable,
		errors.Join(ErrCodexRefreshRejected, ErrCodexRefreshUnrecoverable), ErrCodexMaterialRevisionStale}
	for _, tc := range codexHoldTable {
		if tc.goNA != "" || tc.mode == codexAuthModeAPIKey {
			continue
		}
		for _, kind := range []string{"issue", "ci_fix", "self_improve", "prompt", "task", "mr_rework", "chat", "judge", "job", "cross_check", "egress"} {
			for _, capable := range []bool{false, true} {
				t.Run(tc.name+"/"+kind+"/"+map[bool]string{true: "capable", false: "old"}[capable], func(t *testing.T) {
					svc, w, id, cap := hintFixture(t, tc, kind, kind == "egress")
					if capable {
						w.ProtocolCapabilities = []string{capability.CodexAccountParkV1}
					}
					eligible := kind != "chat" && kind != "judge" && kind != "job" && kind != "cross_check" && kind != "egress"
					for _, state := range states {
						err := svc.decorateCodexAccountHold(context.Background(), w, id, cap, state)
						hold, computed := CodexAccountHoldVerdict(err)
						if !computed || hold != (tc.hold && eligible) || errors.Is(err, ErrCodexAccountUnavailable) != (tc.hold && eligible && capable) || !errors.Is(err, state) {
							t.Fatalf("state=%v hold=%v computed=%v typed=%v want hold=%v", state, hold, computed, errors.Is(err, ErrCodexAccountUnavailable), tc.hold && eligible)
						}
					}
				})
			}
		}
	}
}

func TestCodexAccountHintExclusionsAndRecheck(t *testing.T) {
	tc := codexHoldCase{alias: "linked", account: "same", coord: "quarantined", hold: true}
	svc, w, id, cap := hintFixture(t, tc, "issue", false)
	w.ProtocolCapabilities = []string{capability.CodexAccountParkV1}
	for _, excluded := range []error{ErrCodexRefreshContended, ErrCodexRefreshNoToken, ErrCodexRefreshNoClient,
		ErrCodexRunNotActivelyClaimed, ErrCodexVaultLocked, ErrCodexCapabilityEpoch,
		ErrCodexWorkerMismatch, ErrCodexScopeNotApplicable, ErrCodexAccountRevisionStale,
		context.DeadlineExceeded, context.Canceled} {
		t.Run(excluded.Error(), func(t *testing.T) {
			input := errors.Join(ErrCodexRefreshQuarantined, excluded)
			got := svc.decorateCodexAccountHold(context.Background(), w, id, cap, input)
			if _, computed := CodexAccountHoldVerdict(got); computed || got != input {
				t.Fatalf("exclusion decorated: %v", got)
			}
		})
	}
	for _, input := range []error{nil, errors.New("transport"), errors.New("server failure")} {
		got := svc.decorateCodexAccountHold(context.Background(), w, id, cap, input)
		if _, computed := CodexAccountHoldVerdict(got); computed || got != input {
			t.Fatalf("non-state decorated: %v", got)
		}
	}
	q := svc.q.(accountHintStore)
	for _, change := range []func(*accountHintStore){
		func(q *accountHintStore) { q.row.CodexClaimEpoch++ },
		func(q *accountHintStore) { q.row.CodexCapHash = []byte("wrong") },
		func(q *accountHintStore) { q.row.WorkerID = pgconv.UUID(uuid.New()) },
		func(q *accountHintStore) { q.row.ClaimReleasedAt = pgtype.Timestamptz{Valid: true} },
		func(q *accountHintStore) { q.readErr = errors.New("read failed") },
		func(q *accountHintStore) { q.aliasErr = errors.New("alias metadata failed") },
		func(q *accountHintStore) { q.runErr = errors.New("run metadata failed") },
		func(q *accountHintStore) { q.run.UserID = uuid.New() },
	} {
		changed := q
		change(&changed)
		svc.q = changed
		got := svc.decorateCodexAccountHold(context.Background(), w, id, cap, ErrCodexMaterialRevisionStale)
		if _, computed := CodexAccountHoldVerdict(got); computed {
			t.Fatal("failed prelude/read produced verdict")
		}
	}
}
