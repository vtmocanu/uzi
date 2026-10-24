package workersvc

import (
	"context"
	"errors"
	"fmt"
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
	locked       store.Run
	pending      bool
	pendingErr   error
	committed    bool
	rolledBack   bool
	pendingReads int
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
	t.guarded.pendingReads++
	return guardedApprovalRow{t.guarded}
}
func (t *guardedApprovalTx) Commit(context.Context) error   { t.guarded.committed = true; return nil }
func (t *guardedApprovalTx) Rollback(context.Context) error { t.guarded.rolledBack = true; return nil }

type guardedApprovalRow struct{ guarded *guardedApprovalStore }

func (r guardedApprovalRow) Scan(dest ...any) error {
	*dest[0].(*bool) = r.guarded.pending
	return r.guarded.pendingErr
}

func TestApprovalGuardLockedStopResolution(t *testing.T) {
	for _, selection := range []bool{false, true} {
		for _, override := range []bool{false, true} {
			for _, expected := range []string{"absent", "current", "stale"} {
				for _, state := range []string{"pending", "terminal", "foreign owner"} {
					t.Run(fmt.Sprintf("selection=%v/override=%v/expected=%s/%s", selection, override, expected, state), func(t *testing.T) {
						fs, _, owner, id := gatedRun(t)
						fs.runByID.GateRevision = 1
						g := &guardedApprovalStore{Store: fs, locked: fs.runByID, pending: true}
						// Only the locked read sees the accepted resolution.
						g.locked.PolicyStopResolution = []byte(`{"version":1,"scope_input_id":7}`)
						switch state {
						case "terminal":
							g.locked.Status = "completed"
						case "foreign owner":
							g.locked.UserID = uuid.New()
						}
						opts := SubmitInputOptions{OverrideCapabilities: override}
						switch expected {
						case "current":
							opts.ExpectedGateRevision = i64(1)
						case "stale":
							opts.ExpectedGateRevision = i64(2)
						}
						var sel *AgentSelection
						if selection {
							sel = &AgentSelection{Source: AgentSourceOwn}
						}
						svc := New(g, newBox(t), testParams())
						_, err := svc.SubmitInputWithOptions(context.Background(), owner, id, "approve_plan", "", sel, opts)
						if state == "foreign owner" {
							if !errors.Is(err, ErrRunNotFound) {
								t.Fatalf("foreign owner error=%v, want ErrRunNotFound", err)
							}
						} else {
							var conflict *PolicyStopReviewConflict
							if !errors.As(err, &conflict) || conflict.Reason != "policy_stop_gate_resolved" {
								t.Fatalf("locked resolution error=%v, want policy_stop_gate_resolved", err)
							}
						}
						if g.pendingReads != 0 || fs.createdApproval != nil || fs.createdInput != nil ||
							fs.clearedCaps != nil || g.committed || !g.rolledBack {
							t.Fatalf("refusal queried pending or mutated: reads=%d approval=%v input=%v clear=%v commit=%v rollback=%v",
								g.pendingReads, fs.createdApproval, fs.createdInput, fs.clearedCaps, g.committed, g.rolledBack)
						}
					})
				}
			}
		}
	}
}

func TestApprovalGuardOrdinaryPrecedence(t *testing.T) {
	for _, mode := range []string{"expected before terminal", "expected before pending", "terminal before pending", "pending"} {
		t.Run(mode, func(t *testing.T) {
			fs, _, owner, id := gatedRun(t)
			fs.runByID.GateRevision = 1
			g := &guardedApprovalStore{Store: fs, locked: fs.runByID, pending: true}
			opts := SubmitInputOptions{}
			want := ErrPlanRevisionPending
			reads := 0
			switch mode {
			case "expected before terminal":
				g.locked.Status = "completed"
				opts.ExpectedGateRevision = i64(1)
			case "expected before pending":
				opts.ExpectedGateRevision = i64(2)
			case "terminal before pending":
				g.locked.Status = "completed"
				want = ErrRunTerminal
			case "pending":
				opts.ExpectedGateRevision = i64(1)
				reads = 1
			}
			svc := New(g, newBox(t), testParams())
			_, err := svc.SubmitInputWithOptions(context.Background(), owner, id, "approve_plan", "", nil, opts)
			if mode == "expected before terminal" || mode == "expected before pending" {
				var mismatch *GateRevisionMismatchError
				if !errors.As(err, &mismatch) || mismatch.Current != 1 || mismatch.Expected != *opts.ExpectedGateRevision {
					t.Fatalf("expected revision error=%v", err)
				}
			} else if !errors.Is(err, want) {
				t.Fatalf("error=%v, want %v", err, want)
			}
			if g.pendingReads != reads || fs.createdApproval != nil || fs.createdInput != nil ||
				fs.clearedCaps != nil || g.committed || !g.rolledBack {
				t.Fatal("ordinary refusal changed guard order or wrote")
			}
		})
	}
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
