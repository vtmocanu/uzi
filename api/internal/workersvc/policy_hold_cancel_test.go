package workersvc

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These fakes observe existing SQL and effect seams through ReportPolicyHold.
// No production helper introduced by the repair is referenced, so this file can
// also be overlaid on the unfixed cancellation path.
type policyCancelTx struct {
	pgx.Tx
	t                                   *testing.T
	worker                              store.Worker
	before, updated                     store.Run
	mutationErr, readErr, commitErr     error
	rows, mutations, readbacks, commits int
	committed                           bool
}

type policyCancelBeginner struct{ tx *policyCancelTx }

func (b policyCancelBeginner) Begin(context.Context) (pgx.Tx, error) { return b.tx, nil }
func (*policyCancelTx) Rollback(context.Context) error               { return nil }
func (tx *policyCancelTx) Commit(context.Context) error {
	tx.commits++
	if tx.commitErr != nil {
		return tx.commitErr
	}
	tx.committed = true
	return nil
}
func (tx *policyCancelTx) Exec(_ context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	if query == "SET TRANSACTION ISOLATION LEVEL READ COMMITTED" {
		return pgconn.NewCommandTag("SET"), nil
	}
	if strings.Fields(query)[2] != "CancelRunByWorker" {
		tx.t.Fatalf("unexpected transaction mutation: %s", query)
	}
	tx.mutations++
	if tx.committed || tx.mutations != 1 || !reflect.DeepEqual(args, []any{tx.before.ID, pgconv.UUID(tx.worker.ID)}) {
		tx.t.Fatal("cancel must mutate once under the locked owner")
	}
	return pgconn.NewCommandTag(fmt.Sprintf("UPDATE %d", tx.rows)), tx.mutationErr
}
func (tx *policyCancelTx) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	switch name := strings.Fields(query)[2]; name {
	case "GetWorkerForUpdate":
		return policyReplayRow{value: tx.worker}
	case "GetRunByIDForUpdate":
		return policyReplayRow{value: tx.before}
	case "GetRunOwnedByWorker":
		tx.readbacks++
		if tx.mutations != 1 || tx.committed || !reflect.DeepEqual(args, []any{tx.before.ID, pgconv.UUID(tx.worker.ID)}) {
			tx.t.Fatal("readback must use transaction after exactly one mutation and before commit")
		}
		return policyReplayRow{value: tx.updated, err: tx.readErr}
	default:
		tx.t.Fatalf("unexpected transaction read: %s", name)
		return nil
	}
}

type policyCancelEffects struct {
	*fakeStore
	tx      *policyCancelTx
	events  []string
	postErr error
}

func (s *policyCancelEffects) note(event string) {
	s.tx.t.Helper()
	if !s.tx.committed {
		s.tx.t.Fatalf("%s ran before successful commit", event)
	}
	s.events = append(s.events, event)
}
func (s *policyCancelEffects) SettleScopeInputDisposition(_ context.Context, p store.SettleScopeInputDispositionParams) (int64, error) {
	s.note("scope")
	if p.RunID != s.tx.updated.ID || p.Disposition.String != "declined" || !p.Disposition.Valid {
		s.tx.t.Fatalf("scope settlement = %+v", p)
	}
	return 1, s.postErr
}
func (s *policyCancelEffects) GetRunByID(_ context.Context, id uuid.UUID) (store.Run, error) {
	s.note("checkpoint")
	if id != s.tx.updated.ID {
		s.tx.t.Fatal("wrong checkpoint run")
	}
	// The existing task retention-coordinate query proves the helper was
	// entered with the updated kind. LiveDB tests exercise actual settlement.
	return store.Run{}, errors.New("checkpoint coordinates unavailable")
}
func (s *policyCancelEffects) DeleteEphemeralWorkerForRun(_ context.Context, p store.DeleteEphemeralWorkerForRunParams) (int64, error) {
	s.note("ephemeral")
	if p.RunID != s.tx.updated.ID {
		s.tx.t.Fatal("wrong ephemeral run")
	}
	return 1, s.postErr
}
func (s *policyCancelEffects) ReleaseCustodyHoldExact(context.Context, store.ReleaseCustodyHoldExactParams) (int64, error) {
	s.tx.t.Fatal("cancellation must retain custody")
	return 0, nil
}

type policyCancelBroadcaster struct {
	fakeBroadcaster
	effects *policyCancelEffects
}

func (b *policyCancelBroadcaster) PublishState(id uuid.UUID, status string) {
	b.effects.note("publish")
	if id != b.effects.tx.updated.ID || status != "cancelled" {
		b.effects.tx.t.Fatal("wrong published state")
	}
	b.fakeBroadcaster.PublishState(id, status)
}

type policyCancelLifecycle struct{ effects *policyCancelEffects }

func (l policyCancelLifecycle) Notify(id uuid.UUID, status string) {
	l.effects.note("notify")
	if id != l.effects.tx.updated.ID || status != "cancelled" {
		l.effects.tx.t.Fatal("wrong notified state")
	}
}

func newPolicyCancelFixture(t *testing.T) (*Service, *policyCancelTx, *policyCancelEffects, store.Worker) {
	t.Helper()
	w := store.Worker{ID: uuid.New(), UserID: uuid.New(), Ephemeral: true,
		ProtocolCapabilities: []string{capability.ProviderPolicyHoldV1}}
	before := store.Run{ID: uuid.New(), UserID: w.UserID, WorkerID: pgconv.UUID(w.ID),
		Status: "running", Kind: "issue", Harness: "codex", ClaimGeneration: 1,
		StopKind: pgconv.Text("cancelled"), SessionID: pgconv.Text("saved-session")}
	updated := before
	updated.Status = "cancelled"
	// Deliberately different: effects must consume the updated row.
	updated.Kind = "task"
	updated.ScopeCeiling = pgtype.Int4{Int32: 1, Valid: true}
	tx := &policyCancelTx{t: t, worker: w, before: before, updated: updated, rows: 1}
	effects := &policyCancelEffects{fakeStore: &fakeStore{}, tx: tx}
	svc := New(effects, newBox(t), testParams())
	svc.SetTxBeginner(policyCancelBeginner{tx})
	svc.SetBroadcaster(&policyCancelBroadcaster{effects: effects})
	svc.SetLifecycle(policyCancelLifecycle{effects})
	svc.SetRetentionLockPool(unusedConnAcquirer{})
	svc.SetForgeBaseURLAllowed(func(string) bool { return true })
	svc.SetBackground(func(fn func()) { fn() })
	svc.SetDeleteCheckpointFn(func(context.Context, pushbroker.DeleteOptions) error {
		t.Fatal("unexpected forge call after coordinate failure")
		return nil
	})
	// Authentication's snapshot predates the locked worker metadata.
	w.Ephemeral = false
	return svc, tx, effects, w
}

func TestPolicyHoldCancelCommittedEffects(t *testing.T) {
	for _, postFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("post_query_failure_%v", postFailure), func(t *testing.T) {
			svc, tx, effects, input := newPolicyCancelFixture(t)
			if postFailure {
				effects.postErr = errors.New("post-commit store unavailable")
			}
			before := tx.before
			got, err := svc.ReportPolicyHold(context.Background(), input, before.ID, policyHoldFixture())
			if err != nil || got.Disposition != "terminal" || got.ObservedStatus != "cancelled" {
				t.Fatalf("cancel reply = %+v, %v", got, err)
			}
			want := []string{"publish", "notify", "scope", "checkpoint", "ephemeral"}
			if !reflect.DeepEqual(effects.events, want) {
				t.Fatalf("effects = %v, want %v", effects.events, want)
			}
			if tx.mutations != 1 || tx.readbacks != 1 || tx.commits != 1 {
				t.Fatalf("mutation/readback/commit = %d/%d/%d", tx.mutations, tx.readbacks, tx.commits)
			}
			if !reflect.DeepEqual(tx.before, before) || tx.updated.SessionID != before.SessionID {
				t.Fatal("cancellation changed saved session/source snapshot")
			}
		})
	}
}

func TestPolicyHoldCancelLockedWorkerAndUpdatedScope(t *testing.T) {
	svc, tx, effects, input := newPolicyCancelFixture(t)
	input.Ephemeral, tx.worker.Ephemeral = true, false
	tx.before.ScopeCeiling = pgtype.Int4{Int32: 1, Valid: true}
	tx.updated.ScopeCeiling = pgtype.Int4{}
	got, err := svc.ReportPolicyHold(context.Background(), input, tx.before.ID, policyHoldFixture())
	if err != nil || got.Disposition != "terminal" {
		t.Fatalf("cancel: %+v %v", got, err)
	}
	want := []string{"publish", "notify", "checkpoint"}
	if !reflect.DeepEqual(effects.events, want) {
		t.Fatalf("effects = %v, want %v", effects.events, want)
	}
}

func TestPolicyHoldCancelFailureAndNoopEffects(t *testing.T) {
	boom := errors.New("cancel failure")
	for _, tc := range []struct {
		name                          string
		change                        func(*policyCancelTx)
		wantErr                       bool
		disposition, reason           string
		mutations, readbacks, commits int
	}{
		{"mutation failure", func(tx *policyCancelTx) { tx.mutationErr = boom }, true, "", "", 1, 0, 0},
		{"zero rows", func(tx *policyCancelTx) { tx.rows = 0 }, false, "pending", "cancel_pending", 1, 0, 0},
		{"readback failure", func(tx *policyCancelTx) { tx.readErr = boom }, true, "", "", 1, 1, 0},
		{"missing readback", func(tx *policyCancelTx) { tx.readErr = pgx.ErrNoRows }, true, "", "", 1, 1, 0},
		{"readback not cancelled", func(tx *policyCancelTx) { tx.updated.Status = "running" }, true, "", "", 1, 1, 0},
		{"readback foreign user", func(tx *policyCancelTx) { tx.updated.UserID = uuid.New() }, true, "", "", 1, 1, 0},
		{"readback foreign worker", func(tx *policyCancelTx) { tx.updated.WorkerID = pgconv.UUID(uuid.New()) }, true, "", "", 1, 1, 0},
		{"readback lane mismatch", func(tx *policyCancelTx) { tx.updated.EgressProfileID = pgconv.UUID(uuid.New()) }, true, "", "", 1, 1, 0},
		{"commit failure", func(tx *policyCancelTx) { tx.commitErr = boom }, true, "", "", 1, 1, 1},
		{"terminal replay", func(tx *policyCancelTx) { tx.before.Status = "cancelled" }, false, "terminal", "", 0, 0, 0},
		{"owner stop", func(tx *policyCancelTx) { tx.before.StopKind = pgconv.Text("stopped") }, false, "pending", "owner_disposition_pending", 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, tx, effects, input := newPolicyCancelFixture(t)
			tc.change(tx)
			got, err := svc.ReportPolicyHold(context.Background(), input, tx.before.ID, policyHoldFixture())
			if (err != nil) != tc.wantErr {
				t.Fatalf("reply=%+v err=%v", got, err)
			}
			if !tc.wantErr && (got.Disposition != tc.disposition || got.Reason != tc.reason) {
				t.Fatalf("reply=%+v", got)
			}
			if tx.mutations != tc.mutations || tx.readbacks != tc.readbacks || tx.commits != tc.commits || len(effects.events) != 0 {
				t.Fatalf("mutation/readback/commit=%d/%d/%d effects=%v", tx.mutations, tx.readbacks, tx.commits, effects.events)
			}
		})
	}
}
