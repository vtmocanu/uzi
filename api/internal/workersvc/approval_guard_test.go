package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// The unit store supplies an explicit transaction binding. Database rollback and
// serialization are exercised by TestApprovalGuardCapabilityClearRollbackLiveDB
// and TestApprovalGuardSerializationLiveDB.
func (f *fakeStore) Begin(context.Context) (pgx.Tx, error) { return &approvalUnitTx{store: f}, nil }
func (f *fakeStore) GetRunByIDForUpdate(context.Context, uuid.UUID) (store.Run, error) {
	return f.runByID, f.runByIDErr
}

type approvalUnitTx struct {
	pgx.Tx
	store Store
}

func (t *approvalUnitTx) ApprovalStore() Store { return t.store }
func (t *approvalUnitTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("SET"), nil
}
func (t *approvalUnitTx) QueryRow(context.Context, string, ...any) pgx.Row { return approvalUnitRow{} }
func (t *approvalUnitTx) Commit(context.Context) error                     { return nil }
func (t *approvalUnitTx) Rollback(context.Context) error                   { return nil }

type approvalUnitRow struct{}

func (approvalUnitRow) Scan(dest ...any) error { *dest[0].(*bool) = false; return nil }

// Guard failures are observed through public admission, before any approval write.
type guardedApprovalStore struct {
	Store
	locked     store.Run
	pending    bool
	pendingErr error
	committed  bool
	rolledBack bool
}

func (s *guardedApprovalStore) GetRunByIDForUpdate(context.Context, uuid.UUID) (store.Run, error) {
	return s.locked, nil
}
func (s *guardedApprovalStore) Begin(context.Context) (pgx.Tx, error) {
	return &guardedApprovalTx{approvalUnitTx: approvalUnitTx{store: s}, guarded: s}, nil
}

type guardedApprovalTx struct {
	approvalUnitTx
	guarded *guardedApprovalStore
}

func (t *guardedApprovalTx) QueryRow(context.Context, string, ...any) pgx.Row {
	return guardedApprovalRow{t.guarded}
}
func (t *guardedApprovalTx) Commit(context.Context) error   { t.guarded.committed = true; return nil }
func (t *guardedApprovalTx) Rollback(context.Context) error { t.guarded.rolledBack = true; return nil }

type guardedApprovalRow struct{ guarded *guardedApprovalStore }

func (r guardedApprovalRow) Scan(dest ...any) error {
	*dest[0].(*bool) = r.guarded.pending
	return r.guarded.pendingErr
}

func TestApprovalGuardAdmission(t *testing.T) {
	for _, mode := range []string{"pending", "query error", "changed owner", "missing transaction"} {
		t.Run(mode, func(t *testing.T) {
			fs, _, owner, id := gatedRun(t)
			g := &guardedApprovalStore{Store: fs, locked: fs.runByID, pending: true}
			var q Store = g
			want := ErrPlanRevisionPending
			switch mode {
			case "query error":
				g.pendingErr = errors.New("pending read failed")
				want = g.pendingErr
			case "changed owner":
				g.locked.UserID = uuid.New()
				want = ErrRunNotFound
			case "missing transaction":
				q = struct{ Store }{fs}
				want = nil
			}
			svc := New(q, newBox(t), testParams())
			_, err := svc.SubmitInput(context.Background(), owner, id, "approve_plan", "", nil)
			if err == nil || (want != nil && !errors.Is(err, want)) {
				t.Fatalf("error=%v, want=%v", err, want)
			}
			if fs.createdApproval != nil || fs.createdInput != nil || g.committed {
				t.Fatal("refused approval wrote or committed")
			}
			if mode != "missing transaction" && !g.rolledBack {
				t.Fatal("refused approval did not roll back")
			}
		})
	}
}
