package workersvc

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// This adapter models only database responses, never the scoped-claim decision.
// The request and registered capability use literals so this regression compiles
// before pending_policy_protection exists in the production Go type.
type scopedClaimTx struct {
	pgx.Tx
	worker         store.Worker
	runs           map[uuid.UUID]store.Run
	candidate      store.Run
	claims         int
	zeroOrdinary   bool
	afterTargets   func()
	excludedTarget uuid.UUID
	identityWrites map[pendingOutcomeTuple]int64
}
type scopedClaimBeginner struct{ tx *scopedClaimTx }

func (b scopedClaimBeginner) Begin(context.Context) (pgx.Tx, error) { return b.tx, nil }
func (tx *scopedClaimTx) Begin(context.Context) (pgx.Tx, error)     { return tx, nil }
func (*scopedClaimTx) Commit(context.Context) error                 { return nil }
func (*scopedClaimTx) Rollback(context.Context) error               { return nil }
func (tx *scopedClaimTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "name: SetWorkerSnapshotState ") {
		tx.worker.SnapshotEpoch = args[0].(int64)
		if args[1].(bool) {
			tx.worker.PendingOverflowUntil = pgconv.Time(time.Now().Add(time.Hour))
		}
	}
	if strings.Contains(sql, "name: UpsertFrozenWorkerPendingOutcomeIdentity ") {
		r, ok := tx.runs[args[1].(uuid.UUID)]
		rows := int64(1)
		if tx.zeroOrdinary || !ok || r.WorkerID != pgconv.UUID(tx.worker.ID) ||
			r.ClaimGeneration != args[2].(int64) || r.ClaimReleasedAt.Valid ||
			r.Status == "completed" || r.Status == "failed" || r.Status == "cancelled" || r.ID == tx.excludedTarget {
			rows = 0
		}
		if tx.identityWrites == nil {
			tx.identityWrites = make(map[pendingOutcomeTuple]int64)
		}
		tx.identityWrites[pendingOutcomeTuple{args[1].(uuid.UUID), args[2].(int64)}] = rows
		if rows == 0 {
			return pgconn.NewCommandTag("INSERT 0 0"), nil
		}
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

type scopedClaimRow struct {
	value any
	err   error
}

func (r scopedClaimRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	v := reflect.ValueOf(r.value)
	if v.Kind() != reflect.Struct {
		if len(dest) != 1 {
			return fmt.Errorf("scalar scan width %d", len(dest))
		}
		reflect.ValueOf(dest[0]).Elem().Set(v)
		return nil
	}
	if len(dest) != v.NumField() {
		return fmt.Errorf("struct scan width %d want %d (%T)", len(dest), v.NumField(), r.value)
	}
	for i, d := range dest {
		reflect.ValueOf(d).Elem().Set(v.Field(i))
	}
	return nil
}
func (tx *scopedClaimTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch strings.Fields(sql)[2] {
	case "GetWorkerForUpdate", "GetWorkerByID":
		return scopedClaimRow{value: tx.worker}
	case "GetRunByID":
		r, ok := tx.runs[args[0].(uuid.UUID)]
		if !ok {
			return scopedClaimRow{err: pgx.ErrNoRows}
		}
		return scopedClaimRow{value: r}
	case "ClaimRun":
		tx.claims++
		return scopedClaimRow{value: tx.candidate}
	case "GetRunOwnedByWorkerForUpdate", "GetRunByIDForUpdate":
		return scopedClaimRow{value: tx.candidate}
	case "GetPriorRunCredentialEpoch":
		return scopedClaimRow{err: pgx.ErrNoRows}
	case "LockSecretEnablementForShareNowait":
		return scopedClaimRow{value: false}
	default:
		return scopedClaimRow{err: fmt.Errorf("unexpected query %s", strings.Fields(sql)[2])}
	}
}

type scopedClaimRows struct {
	pgx.Rows
	values []any
	index  int
}

func (*scopedClaimRows) Close()       {}
func (*scopedClaimRows) Err() error   { return nil }
func (r *scopedClaimRows) Next() bool { r.index++; return r.index <= len(r.values) }
func (r *scopedClaimRows) Scan(dest ...any) error {
	return (scopedClaimRow{value: r.values[r.index-1]}).Scan(dest...)
}
func (tx *scopedClaimTx) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	rows := &scopedClaimRows{}
	switch strings.Fields(sql)[2] {
	case "LockWorkerRecoveryParents":
		for _, r := range tx.runs {
			rows.values = append(rows.values, store.LockWorkerRecoveryParentsRow{
				ID: r.ID, Kind: r.Kind, WorkerID: r.WorkerID, ClaimGeneration: r.ClaimGeneration,
				ParentLeadID: pgconv.UUID(r.ID), LockedParentIds: []uuid.UUID{r.ID},
			})
		}
	case "LockFrozenWorkerSnapshotRuns":
		for id := range tx.runs {
			if id != tx.excludedTarget {
				rows.values = append(rows.values, id)
			}
		}
		if tx.afterTargets != nil {
			tx.afterTargets()
		}
	case "ListWorkerPendingSince", "ListFreshWorkerPendingOutcomeSince", "LockOpenCustodyHoldsForRunWorkerGeneration":
	default:
		return nil, fmt.Errorf("unexpected collection query %s", strings.Fields(sql)[2])
	}
	return rows, nil
}

func scopedClaimFixture(t *testing.T) (*Service, *scopedClaimTx, uuid.UUID) {
	t.Helper()
	w := worker()
	w.SnapshotRegisterNonce = pgconv.TextOrNull("scoped")
	w.ProtocolCapabilities = []string{"pending_outcome_scoped_claim_v1"}
	w.Status = "online"
	id := uuid.New()
	tx := &scopedClaimTx{worker: w, runs: map[uuid.UUID]store.Run{id: {
		ID: id, UserID: w.UserID, WorkerID: pgconv.UUID(w.ID), Kind: "issue", Status: "running", ClaimGeneration: 9,
	}}, candidate: store.Run{ID: uuid.New(), UserID: w.UserID, WorkerID: pgconv.UUID(w.ID), Kind: "issue", Status: "claimed", ClaimGeneration: 1}}
	box := newBox(t)
	pat, err := box.Seal([]byte("scoped-fake-pat"))
	if err != nil {
		t.Fatal(err)
	}
	token, err := box.Seal([]byte("scoped-fake-model"))
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeStore{claimRun: tx.candidate, claimCtx: store.GetRunClaimContextRow{
		RepoWebUrl: "https://forge.example.com/g/r", RepoPath: "g/r", ForgeType: "gitlab", BaseUrl: "https://forge.example.com",
		BotUsername: "bot", TokenCiphertext: pat,
	}, anthropic: token}
	p := testParams()
	p.WorkerOutboxMaxPending = 2
	svc := New(fs, box, p)
	m := newMemVaultStore()
	m.states[w.UserID] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeUnprotected}
	svc.SetVault(newMemoryVault(box, m))
	svc.SetTxBeginner(scopedClaimBeginner{tx})
	return svc, tx, id
}

func TestScopedClaimRawJSON(t *testing.T) {
	svc, tx, id := scopedClaimFixture(t)
	raw := fmt.Sprintf(`{"snapshot_epoch":1,"register_nonce":"scoped","active":[],"pending_overflow":true,"pending_all":[{"run_id":%q,"claim_generation":7}],"pending_policy_protection":[{"run_id":%q,"owner_generation":9}]}`, id.String(), id.String())
	var snap ActiveSnapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatal(err)
	}
	payload, err := svc.Claim(context.Background(), tx.worker, &snap)
	if err != nil {
		t.Fatal(err)
	}
	if payload == nil || payload.RunID != tx.candidate.ID.String() {
		t.Fatalf("Claim returned idle instead of unrelated eligible candidate; SQL claims=%d", tx.claims)
	}
	if !tx.worker.PendingOverflowUntil.Valid {
		t.Fatal("global overflow closure was erased")
	}
}
