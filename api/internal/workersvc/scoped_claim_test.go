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
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// This adapter models only database responses, never the scoped-claim decision.
// The request and registered capability use literals so this regression compiles
// before pending_policy_protection exists in the production Go type.
type scopedClaimTx struct {
	pgx.Tx
	worker           store.Worker
	runs             map[uuid.UUID]store.Run
	candidate        store.Run
	claims           int
	zeroOrdinary     bool
	afterTargets     func()
	excludedTarget   uuid.UUID
	identityWrites   map[pendingOutcomeTuple]int64
	queuedRefreshes  int
	queuedRefreshIDs []uuid.UUID
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
	case "GetActivePublicationResume":
		// Scoped-claim fixtures have no publication-resume enrollment.
		return scopedClaimRow{err: pgx.ErrNoRows}
	case "GetLatestMemoryParkRelease":
		// Scoped-claim fixtures have no memory-park release row.
		return scopedClaimRow{err: pgx.ErrNoRows}
	case "GetPriorRunCredentialEpoch":
		return scopedClaimRow{err: pgx.ErrNoRows}
	case "RefreshFrozenQueuedCustodyTargets":
		return tx.refreshQueuedCustodyTargets(args)
	case "LockSecretEnablementForShareNowait":
		return scopedClaimRow{value: false}
	default:
		return scopedClaimRow{err: fmt.Errorf("unexpected query %s", strings.Fields(sql)[2])}
	}
}

// Snapshot claims do not requeue runs. Their refresh validates the captured
// tuple against the same locked run rows; it does not replace the snapshot's
// independent LockFrozenWorkerSnapshotRuns eligibility set.
func (tx *scopedClaimTx) refreshQueuedCustodyTargets(args []any) pgx.Row {
	if len(args) != 3 || len(args[2].([]uuid.UUID)) != 0 {
		return scopedClaimRow{err: fmt.Errorf("unexpected queued-custody refresh arguments")}
	}
	var frozen []store.LockWorkerRecoveryParentsRow
	var objects []map[string]json.RawMessage
	if err := json.Unmarshal(args[0].([]byte), &frozen); err != nil {
		return scopedClaimRow{err: err}
	}
	if err := json.Unmarshal(args[0].([]byte), &objects); err != nil {
		return scopedClaimRow{err: err}
	}
	parents := args[1].([]uuid.UUID)
	selected := make([]map[string]json.RawMessage, 0, len(frozen))
	tx.queuedRefreshes++
	tx.queuedRefreshIDs = nil
	for i, row := range frozen {
		run, found := tx.runs[row.ID]
		if !found || run.EgressProfileID.Valid {
			continue
		}
		switch run.Kind {
		case "issue", "ci_fix", "self_improve", "prompt", "task", "mr_rework":
		default:
			continue
		}
		locked := false
		for _, id := range parents {
			locked = locked || id == row.ID
		}
		if !locked {
			continue
		}
		current := scopedClaimRecoveryParent(run)
		// These three association/lock fields are outside the refresh SQL's
		// recordset; preserve them only for comparing the frozen run tuple.
		current.CrossCheckID, current.ParentLeadID, current.LockedParentIds = row.CrossCheckID, row.ParentLeadID, row.LockedParentIds
		if !reflect.DeepEqual(row, current) {
			continue
		}
		delete(objects[i], "cross_check_id")
		delete(objects[i], "parent_lead_id")
		delete(objects[i], "locked_parent_ids")
		selected = append(selected, objects[i])
		tx.queuedRefreshIDs = append(tx.queuedRefreshIDs, row.ID)
	}
	value, err := json.Marshal(selected)
	return scopedClaimRow{value: value, err: err}
}

func scopedClaimRecoveryParent(r store.Run) store.LockWorkerRecoveryParentsRow {
	// PostgreSQL timestamps carry an instant, not Go's monotonic clock or location identity.
	dbTime := func(value pgtype.Timestamptz) pgtype.Timestamptz {
		if !value.Valid {
			return pgtype.Timestamptz{}
		}
		value.Time = value.Time.Round(0).UTC()
		return value
	}
	return store.LockWorkerRecoveryParentsRow{
		ID: r.ID, Kind: r.Kind, TargetRunID: r.TargetRunID, WorkerID: r.WorkerID, UserID: r.UserID,
		Status: r.Status, StatusSince: dbTime(r.StatusSince), RequeueCount: r.RequeueCount, ClaimedAt: dbTime(r.ClaimedAt),
		ReleasedWorkerID: r.ReleasedWorkerID, ReleasedWorkerNonce: r.ReleasedWorkerNonce,
		StopKind: r.StopKind, PauseMode: r.PauseMode, CredentialSwitchGeneration: r.CredentialSwitchGeneration,
		ClaimGeneration: r.ClaimGeneration, WorkerRecoveryEpisode: r.WorkerRecoveryEpisode,
		RequeueEpisodeBaseline: r.RequeueEpisodeBaseline, ClaimReleasedAt: dbTime(r.ClaimReleasedAt),
		PauseRequestedAt: dbTime(r.PauseRequestedAt), CredentialSwitchRequestedAt: dbTime(r.CredentialSwitchRequestedAt),
		StopReason: r.StopReason, HoldReason: r.HoldReason,
		ParentLeadID: pgconv.UUID(r.ID), LockedParentIds: []uuid.UUID{r.ID},
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
			rows.values = append(rows.values, scopedClaimRecoveryParent(r))
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
	run := tx.runs[id]
	run.StatusSince = pgconv.Time(time.Now()) // Round-trip a real timestamp through frozen JSON.
	tx.runs[id] = run
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
	if tx.queuedRefreshes != 1 || len(tx.queuedRefreshIDs) != 1 || tx.queuedRefreshIDs[0] != id {
		t.Fatalf("claim must refresh the real locked source, got refreshes=%d ids=%v", tx.queuedRefreshes, tx.queuedRefreshIDs)
	}
}
