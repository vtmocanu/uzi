package healthsvc

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/dbdiskfull"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/slacksvc"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// These are pure severity-logic tests: a FAKE Store and Settings, an injected clock and
// db probe, and NO database. They drive each check through every severity it can take and
// assert on the emitted summary TEXT and severity — never on a bare count — plus the
// registry rollup and a hostile-input case. The live SQL is proven separately in the store
// package (health_livedb_test.go); the router auth in the handler package.

// ---- fakes -----------------------------------------------------------------

type fakeStore struct {
	storageRows            []store.RecoveryStorageHealthRow
	storageErr             error
	storageRead            func(context.Context, store.RecoveryStorageHealthParams) ([]store.RecoveryStorageHealthRow, error)
	custodyAggregateErr    error
	custodyAggregate       store.GetCustodyAggregateForOwnerRow
	custodyListParams      []store.ListOwnersOverCustodyLimitParams
	custodyAggregateParams []store.GetCustodyAggregateForOwnerParams
	enabledIDs             []uuid.UUID
	enabledErr             error
	workers                []store.ListAllWorkersRow
	workersErr             error
	capacityRows           []store.ListOwnersWaitingNoCapacityRow
	capacityErr            error
	capacityQueries        atomic.Int32
	waitingQueries         atomic.Int32
	waitingRows            []store.ListWaitingWorkerRunsRow
	waitingErr             error
	undispatched           pgtype.Timestamptz
	undispatchErr          error
	pausedCount            int64
	pausedErr              error
	gaveUp                 []store.ListGaveUpColumnMovesRow
	gaveUpErr              error
	custodyOwners          []uuid.UUID
	custodyErr             error
	custodySelect          func(context.Context, store.ListOwnersOverCustodyLimitParams) ([]uuid.UUID, error)
	custodyList            func(context.Context, store.ListCustodyHoldsForOwnerParams) ([]store.ListCustodyHoldsForOwnerRow, error)
	custodyQueries         []store.ListCustodyHoldsForOwnerParams
	controller             pgtype.Timestamptz
	controllerErr          error
	ciwatch                []store.CountEligibleCIWatchRefsPerRepoRow
	ciwatchErr             error
	runDisk                []store.WorkerRunDisk
	runDiskErr             error
}

func (f *fakeStore) RecoveryStorageHealth(ctx context.Context, p store.RecoveryStorageHealthParams) ([]store.RecoveryStorageHealthRow, error) {
	if f.storageRead != nil {
		return f.storageRead(ctx, p)
	}
	if f.storageRows == nil && f.storageErr == nil {
		return []store.RecoveryStorageHealthRow{{}}, nil
	}
	return f.storageRows, f.storageErr
}

func (f *fakeStore) ListEnabledRepoIDs(context.Context) ([]uuid.UUID, error) {
	return f.enabledIDs, f.enabledErr
}

func (f *fakeStore) ListAllWorkers(context.Context) ([]store.ListAllWorkersRow, error) {
	return f.workers, f.workersErr
}
func (f *fakeStore) ListOwnersWaitingNoCapacity(context.Context, store.ListOwnersWaitingNoCapacityParams) ([]store.ListOwnersWaitingNoCapacityRow, error) {
	f.capacityQueries.Add(1)
	return f.capacityRows, f.capacityErr
}
func (f *fakeStore) ListWaitingWorkerRuns(context.Context) ([]store.ListWaitingWorkerRunsRow, error) {
	f.waitingQueries.Add(1)
	return f.waitingRows, f.waitingErr
}
func (f *fakeStore) OldestUndispatchedTaskRun(context.Context) (pgtype.Timestamptz, error) {
	return f.undispatched, f.undispatchErr
}
func (f *fakeStore) CountUsersPausedWithEnabledSchedules(context.Context, pgtype.Timestamptz) (int64, error) {
	return f.pausedCount, f.pausedErr
}
func (f *fakeStore) ListGaveUpColumnMoves(context.Context, store.ListGaveUpColumnMovesParams) ([]store.ListGaveUpColumnMovesRow, error) {
	return f.gaveUp, f.gaveUpErr
}
func (f *fakeStore) ListOwnersOverCustodyLimit(ctx context.Context, arg store.ListOwnersOverCustodyLimitParams) ([]uuid.UUID, error) {
	f.custodyListParams = append(f.custodyListParams, arg)
	if f.custodySelect != nil {
		return f.custodySelect(ctx, arg)
	}
	return f.custodyOwners, f.custodyErr
}
func (f *fakeStore) GetCustodyAggregateForOwner(_ context.Context, arg store.GetCustodyAggregateForOwnerParams) (store.GetCustodyAggregateForOwnerRow, error) {
	f.custodyAggregateParams = append(f.custodyAggregateParams, arg)
	return f.custodyAggregate, f.custodyAggregateErr
}
func (f *fakeStore) ListCustodyHoldsForOwner(ctx context.Context, arg store.ListCustodyHoldsForOwnerParams) ([]store.ListCustodyHoldsForOwnerRow, error) {
	f.custodyQueries = append(f.custodyQueries, arg)
	if f.custodyList != nil {
		return f.custodyList(ctx, arg)
	}
	return nil, nil
}
func (f *fakeStore) GetControllerReport(context.Context) (pgtype.Timestamptz, error) {
	return f.controller, f.controllerErr
}
func (f *fakeStore) CountEligibleCIWatchRefsPerRepo(context.Context, pgtype.Timestamptz) ([]store.CountEligibleCIWatchRefsPerRepoRow, error) {
	return f.ciwatch, f.ciwatchErr
}
func (f *fakeStore) ListLargestRunDiskForWorkers(context.Context, store.ListLargestRunDiskForWorkersParams) ([]store.WorkerRunDisk, error) {
	return f.runDisk, f.runDiskErr
}

type fakeSettings struct {
	healthEnabled  bool
	releaseEnabled bool
	release        settings.ReleaseStatus
	healthErr      error
	relEnabledErr  error
	relStatusErr   error
}

func (f *fakeSettings) HealthEnabled(context.Context) (bool, error) {
	return f.healthEnabled, f.healthErr
}
func (f *fakeSettings) ReleaseCheckEnabled(context.Context) (bool, error) {
	return f.releaseEnabled, f.relEnabledErr
}
func (f *fakeSettings) ReleaseStatus(context.Context) (settings.ReleaseStatus, error) {
	return f.release, f.relStatusErr
}

// fixedNow is the clock every test anchors to.
var fixedNow = time.Date(2026, 9, 20, 3, 0, 0, 0, time.UTC)

func newSvc(fs *fakeStore, sett *fakeSettings) *Service {
	return New(Config{
		Store:               fs,
		Settings:            sett,
		Now:                 func() time.Time { return fixedNow },
		HostedWorkerVersion: "0.84.0",
		RunningVersion:      "0.84.0",
		HeartbeatStale:      45 * time.Second,
		CustodyHoldLimit:    3,
	})
}

func find(t *testing.T, doc Doc, id string) (found bool) {
	t.Helper()
	for _, c := range doc.Checks {
		if c.ID == id {
			return true
		}
	}
	return false
}

func check(t *testing.T, doc Doc, id string) (sev, summary string) {
	t.Helper()
	for _, c := range doc.Checks {
		if c.ID == id {
			return c.Severity, c.Summary
		}
	}
	t.Fatalf("check %q not in doc", id)
	return "", ""
}

// hostedRow builds a hosted-worker ListAllWorkers row with a roll signal.
func hostedRow(name, phase string, observedAgo time.Duration, reason, container, tag string) store.ListAllWorkersRow {
	return store.ListAllWorkersRow{
		Worker:                store.Worker{Kind: "hosted", Name: name},
		RollPhase:             pgtype.Text{String: phase, Valid: phase != ""},
		RollObservedAt:        pgtype.Timestamptz{Time: fixedNow.Add(-observedAgo), Valid: true},
		RollPhaseSince:        pgtype.Timestamptz{Time: fixedNow.Add(-observedAgo), Valid: true},
		RollBlockingReason:    pgtype.Text{String: reason, Valid: reason != ""},
		RollBlockingContainer: pgtype.Text{String: container, Valid: container != ""},
		RollWorkerImageTag:    pgtype.Text{String: tag, Valid: tag != ""},
	}
}

// ---- fleet.roll ------------------------------------------------------------

func TestFleetRoll(t *testing.T) {
	fresh := 10 * time.Second
	stale := rollSignalTTL + 30*time.Second

	tests := []struct {
		name       string
		hostedVer  string
		workers    []store.ListAllWorkersRow
		crOK       bool // controller.report ok-ness fed to the M2 unknown conjunct
		wantSev    string
		wantSubstr string
	}{
		{
			name:       "na when not configured",
			hostedVer:  "",
			workers:    nil,
			wantSev:    sevNA,
			wantSubstr: "No hosted workers are configured",
		},
		{
			// M2 conjunct: a stale signal is unknown ONLY when controller.report is not ok.
			name:       "unknown when stale AND controller not ok",
			hostedVer:  "0.84.0",
			workers:    []store.ListAllWorkersRow{hostedRow("w1", workersvc.PhaseStuck, stale, "ImagePullBackOff", "agent", "0.84.0")},
			crOK:       false,
			wantSev:    sevUnknown,
			wantSubstr: "roll health cannot be determined",
		},
		{
			// The other half of the conjunct: stale but controller reporting cleanly ⇒ NOT
			// unknown; falls through to the ordinary classification (ok, no fresh stuck signal).
			name:       "ok when stale but controller ok",
			hostedVer:  "0.84.0",
			workers:    []store.ListAllWorkersRow{hostedRow("w1", workersvc.PhaseStuck, stale, "ImagePullBackOff", "agent", "0.84.0")},
			crOK:       true,
			wantSev:    sevOK,
			wantSubstr: "rolling cleanly",
		},
		{
			name:      "ok when fresh and none stuck",
			hostedVer: "0.84.0",
			workers: []store.ListAllWorkersRow{
				hostedRow("w1", workersvc.PhaseSettled, fresh, "", "", "0.84.0"),
				hostedRow("w2", workersvc.PhaseRolling, fresh, "", "", "0.84.0"),
			},
			crOK:       true,
			wantSev:    sevOK,
			wantSubstr: "rolling cleanly",
		},
		{
			name:      "warn when some stuck",
			hostedVer: "0.84.0",
			workers: []store.ListAllWorkersRow{
				hostedRow("w1", workersvc.PhaseStuck, fresh, "ImagePullBackOff", "agent", "0.84.0"),
				hostedRow("w2", workersvc.PhaseSettled, fresh, "", "", "0.84.0"),
			},
			crOK:       true,
			wantSev:    sevWarn,
			wantSubstr: "1 of 2 hosted workers stuck rolling",
		},
		{
			name:      "danger when all stuck",
			hostedVer: "0.84.0",
			workers: []store.ListAllWorkersRow{
				hostedRow("w1", workersvc.PhaseStuck, fresh, "ImagePullBackOff", "agent", "0.84.0"),
				hostedRow("w2", workersvc.PhaseStuck, fresh, "ImagePullBackOff", "agent", "0.84.0"),
			},
			crOK:       true,
			wantSev:    sevDanger,
			wantSubstr: "2 of 2 hosted workers stuck rolling",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := New(Config{
				Store:               &fakeStore{workers: tc.workers},
				Settings:            &fakeSettings{healthEnabled: true},
				Now:                 func() time.Time { return fixedNow },
				HostedWorkerVersion: tc.hostedVer,
			})
			c := svc.checkFleetRoll(fixedNow, tc.workers, tc.crOK)
			if c.Severity != tc.wantSev {
				t.Fatalf("severity = %q, want %q (summary %q)", c.Severity, tc.wantSev, c.Summary)
			}
			if !strings.Contains(c.Summary, tc.wantSubstr) {
				t.Fatalf("summary %q does not contain %q", c.Summary, tc.wantSubstr)
			}
		})
	}
}

// ---- fleet.capacity --------------------------------------------------------

func TestFleetCapacity(t *testing.T) {
	waitRow := func(ago time.Duration) store.ListOwnersWaitingNoCapacityRow {
		return store.ListOwnersWaitingNoCapacityRow{
			UserID:      uuid.New(),
			HealthSince: pgtype.Timestamptz{Time: fixedNow.Add(-ago), Valid: true},
		}
	}
	tests := []struct {
		name       string
		health     healthDetectorState
		rows       []store.ListOwnersWaitingNoCapacityRow
		wantSev    string
		wantSubstr string
	}{
		{"unknown when health disabled", healthDetectorDisabled, nil, sevUnknown, "run-health detector is disabled"},
		// Read-failed: rows that WOULD be danger if queried; asserting unknown proves the
		// check does not query the run tables when the kill-switch state is indeterminate.
		{"unknown when health read failed", healthDetectorUnknown, []store.ListOwnersWaitingNoCapacityRow{waitRow(7 * time.Minute)}, sevUnknown, "could not be determined"},
		{"ok when nobody waiting", healthDetectorEnabled, nil, sevOK, "has a usable worker"},
		{"ok under the danger window", healthDetectorEnabled, []store.ListOwnersWaitingNoCapacityRow{waitRow(2 * time.Minute)}, sevOK, "transient window"},
		{"danger past the window", healthDetectorEnabled, []store.ListOwnersWaitingNoCapacityRow{waitRow(7 * time.Minute)}, sevDanger, "no usable worker"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := newSvc(&fakeStore{capacityRows: tc.rows}, &fakeSettings{})
			c := svc.checkFleetCapacity(context.Background(), fixedNow, tc.health)
			if c.Severity != tc.wantSev {
				t.Fatalf("severity = %q, want %q (summary %q)", c.Severity, tc.wantSev, c.Summary)
			}
			if !strings.Contains(c.Summary, tc.wantSubstr) {
				t.Fatalf("summary %q does not contain %q", c.Summary, tc.wantSubstr)
			}
		})
	}
}

// ---- fleet.disk ------------------------------------------------------------

func TestFleetDisk(t *testing.T) {
	diskWorker := func(streak int32, hbAgo time.Duration) store.ListAllWorkersRow {
		return store.ListAllWorkersRow{Worker: store.Worker{
			Kind:                    "hosted",
			StatsDiskPressureStreak: streak,
			LastHeartbeatAt:         pgtype.Timestamptz{Time: fixedNow.Add(-hbAgo), Valid: true},
		}}
	}
	tests := []struct {
		name       string
		workers    []store.ListAllWorkersRow
		wantSev    string
		wantSubstr string
	}{
		{"ok when no pressure", []store.ListAllWorkersRow{diskWorker(0, time.Second)}, sevOK, "No worker is under sustained disk pressure"},
		{"warn on fresh streak", []store.ListAllWorkersRow{diskWorker(2, time.Second)}, sevWarn, "1 worker(s) are under sustained disk pressure"},
		{"stale heartbeat does not count", []store.ListAllWorkersRow{diskWorker(3, 5*time.Minute)}, sevOK, "No worker is under sustained disk pressure"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := newSvc(&fakeStore{}, &fakeSettings{})
			c := svc.checkFleetDisk(fixedNow, tc.workers)
			if c.Severity != tc.wantSev {
				t.Fatalf("severity = %q, want %q (summary %q)", c.Severity, tc.wantSev, c.Summary)
			}
			if !strings.Contains(c.Summary, tc.wantSubstr) {
				t.Fatalf("summary %q does not contain %q", c.Summary, tc.wantSubstr)
			}
		})
	}
}

// ---- fleet.rundisk ---------------------------------------------------------

func TestFleetRunDisk(t *testing.T) {
	const gib = int64(1) << 30
	wid := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	worker := func(dataTotal, inodesUsed, inodesTotal int64, hbAgo time.Duration) store.ListAllWorkersRow {
		w := store.Worker{
			ID:              wid,
			Kind:            "hosted",
			LastHeartbeatAt: pgtype.Timestamptz{Time: fixedNow.Add(-hbAgo), Valid: true},
		}
		if dataTotal > 0 {
			w.StatsDiskDataTotalBytes = pgtype.Int8{Int64: dataTotal, Valid: true}
		}
		if inodesTotal > 0 {
			w.StatsDiskDataInodes = pgtype.Int8{Int64: inodesUsed, Valid: true}
			w.StatsDiskDataTotalInodes = pgtype.Int8{Int64: inodesTotal, Valid: true}
		}
		return store.ListAllWorkersRow{Worker: w}
	}
	largest := func(home int64) []store.WorkerRunDisk {
		return []store.WorkerRunDisk{{WorkerID: wid, RunID: uuid.New(), HomeBytes: home, CacheBytes: home / 2}}
	}
	tests := []struct {
		name       string
		worker     store.ListAllWorkersRow
		runDisk    []store.WorkerRunDisk
		runDiskErr error
		wantSev    string
		wantSubstr string
	}{
		{"ok under the fraction", worker(100*gib, 0, 0, time.Second), largest(39 * gib), nil, sevOK, "No run is close to filling"},
		{"warn at the fraction", worker(100*gib, 0, 0, time.Second), largest(40 * gib), nil, sevWarn, "1 worker(s) hold a run using 40% or more of the data volume"},
		{"warn above the fraction", worker(100*gib, 0, 0, time.Second), largest(90 * gib), nil, sevWarn, "hold a run using 40% or more"},
		{"stale heartbeat does not count", worker(100*gib, 0, 0, 5*time.Minute), largest(90 * gib), nil, sevOK, "No run is close to filling"},
		{"no data total is skipped", worker(0, 0, 0, time.Second), largest(90 * gib), nil, sevOK, "No run is close to filling"},
		{"no reported runs is ok", worker(100*gib, 0, 0, time.Second), nil, nil, sevOK, "No run is close to filling"},
		{"inodes free above 5% is ok", worker(100*gib, 94, 100, time.Second), nil, nil, sevOK, "No run is close to filling"},
		{"inodes free under 5% warns", worker(100*gib, 96, 100, time.Second), nil, nil, sevWarn, "1 worker(s) are low on data-volume inodes"},
		{"both conditions", worker(100*gib, 99, 100, time.Second), largest(50 * gib), nil, sevWarn, "and 1 worker(s) are low on data-volume inodes"},
		{"size read error is unknown", worker(100*gib, 0, 0, time.Second), nil, errors.New("boom"), sevUnknown, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := newSvc(&fakeStore{runDisk: tc.runDisk, runDiskErr: tc.runDiskErr}, &fakeSettings{})
			c := svc.checkFleetRunDisk(context.Background(), fixedNow, []store.ListAllWorkersRow{tc.worker})
			if c.Severity != tc.wantSev {
				t.Fatalf("severity = %q, want %q (summary %q)", c.Severity, tc.wantSev, c.Summary)
			}
			if !strings.Contains(c.Summary, tc.wantSubstr) {
				t.Fatalf("summary %q does not contain %q", c.Summary, tc.wantSubstr)
			}
		})
	}
}

// ---- queue.waiting ---------------------------------------------------------

func TestQueueWaiting(t *testing.T) {
	at := func(ago time.Duration) pgtype.Timestamptz {
		return pgtype.Timestamptz{Time: fixedNow.Add(-ago), Valid: true}
	}
	tests := []struct {
		name       string
		health     healthDetectorState
		oldest     pgtype.Timestamptz
		wantSev    string
		wantSubstr string
	}{
		{"unknown when health disabled", healthDetectorDisabled, pgtype.Timestamptz{}, sevUnknown, "run-health detector is disabled"},
		// Read-failed: a run old enough to be danger if queried; asserting unknown proves the
		// check does not query the run tables when the kill-switch state is indeterminate.
		{"unknown when health read failed", healthDetectorUnknown, at(45 * time.Minute), sevUnknown, "could not be determined"},
		{"ok when nothing waiting", healthDetectorEnabled, pgtype.Timestamptz{}, sevOK, "No run is waiting"},
		{"ok under the warn band", healthDetectorEnabled, at(3 * time.Minute), sevOK, "longer than 10 minutes"},
		{"warn at 10m", healthDetectorEnabled, at(12 * time.Minute), sevWarn, "waiting for a worker for 12m"},
		{"danger at 30m", healthDetectorEnabled, at(45 * time.Minute), sevDanger, "waiting for a worker for 45m"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var rows []store.ListWaitingWorkerRunsRow
			if tc.oldest.Valid {
				rows = []store.ListWaitingWorkerRunsRow{{RunID: uuid.New(), UserID: uuid.New(), HealthSince: tc.oldest}}
			}
			svc := newSvc(&fakeStore{waitingRows: rows}, &fakeSettings{})
			c := svc.checkQueueWaiting(context.Background(), fixedNow, tc.health)
			if c.Severity != tc.wantSev {
				t.Fatalf("severity = %q, want %q (summary %q)", c.Severity, tc.wantSev, c.Summary)
			}
			if !strings.Contains(c.Summary, tc.wantSubstr) {
				t.Fatalf("summary %q does not contain %q", c.Summary, tc.wantSubstr)
			}
		})
	}
}

// ---- queue.undispatched ----------------------------------------------------

func TestQueueUndispatched(t *testing.T) {
	at := func(ago time.Duration) pgtype.Timestamptz {
		return pgtype.Timestamptz{Time: fixedNow.Add(-ago), Valid: true}
	}
	tests := []struct {
		name       string
		oldest     pgtype.Timestamptz
		wantSev    string
		wantSubstr string
	}{
		{"ok when none", pgtype.Timestamptz{}, sevOK, "No task run is stuck undispatched"},
		{"ok under 10m", at(3 * time.Minute), sevOK, "No task run is stuck undispatched"},
		{"danger past 10m", at(15 * time.Minute), sevDanger, "queued undispatched for 15m"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := newSvc(&fakeStore{undispatched: tc.oldest}, &fakeSettings{})
			c := svc.checkQueueUndispatched(context.Background(), fixedNow)
			if c.Severity != tc.wantSev {
				t.Fatalf("severity = %q, want %q (summary %q)", c.Severity, tc.wantSev, c.Summary)
			}
			if !strings.Contains(c.Summary, tc.wantSubstr) {
				t.Fatalf("summary %q does not contain %q", c.Summary, tc.wantSubstr)
			}
		})
	}
}

// ---- db --------------------------------------------------------------------

func TestDB(t *testing.T) {
	tests := []struct {
		name       string
		probe      *dbStat
		nilProbe   bool
		wantSev    string
		wantSubstr string
	}{
		{"unknown when probe unset", nil, true, sevUnknown, "probe is not configured"},
		{"danger on ping failure", &dbStat{pingErr: errors.New("boom"), schemaAtHead: true}, false, sevDanger, "ping failed"},
		{"danger on schema mismatch", &dbStat{schemaApplied: 190, schemaHead: 192, schemaAtHead: false}, false, sevDanger, "differs from the embedded head"},
		{"danger on schema read error", &dbStat{schemaErr: errors.New("no goose"), schemaAtHead: false}, false, sevDanger, "Could not read the applied migration"},
		{"warn on slow ping", &dbStat{pingDur: 400 * time.Millisecond, schemaAtHead: true}, false, sevWarn, "ping is slow"},
		{"warn on saturated pool", &dbStat{acquiredConns: 18, maxConns: 20, schemaAtHead: true}, false, sevWarn, "near capacity"},
		{"ok when healthy", &dbStat{pingDur: 5 * time.Millisecond, acquiredConns: 1, maxConns: 20, schemaAtHead: true}, false, sevOK, "schema at head"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := newSvc(&fakeStore{}, &fakeSettings{})
			if tc.nilProbe {
				svc.probeDB = nil
			} else {
				st := *tc.probe
				svc.probeDB = func(context.Context) dbStat { return st }
			}
			c := svc.checkDB(context.Background(), svc.now())
			if c.Severity != tc.wantSev {
				t.Fatalf("severity = %q, want %q (summary %q)", c.Severity, tc.wantSev, c.Summary)
			}
			if !strings.Contains(c.Summary, tc.wantSubstr) {
				t.Fatalf("summary %q does not contain %q", c.Summary, tc.wantSubstr)
			}
		})
	}
}

// Evaluate reads the clock once and judges db disk-full at that instant. A clock that
// jumps past the window on later calls must not clear a sighting made at that instant.
func TestEvaluateJudgesDiskFullAtItsSingleNow(t *testing.T) {
	t0 := fixedNow
	sig := dbdiskfull.New(func() time.Time { return t0 })
	sig.Observe(fmt.Errorf("write: %w", &pgconn.PgError{Code: "53100"}))
	calls := 0
	svc := New(Config{Store: &fakeStore{}, Settings: &fakeSettings{}, DiskFull: sig, Now: func() time.Time {
		calls++
		if calls == 1 {
			return t0
		}
		return t0.Add(dbdiskfull.Window + time.Second)
	}})
	svc.probeDB = func(context.Context) dbStat { return dbStat{schemaAtHead: true, maxConns: 20} }
	doc, err := svc.Evaluate(context.Background())
	if err != nil || calls != 1 {
		t.Fatalf("Evaluate err=%v clock calls=%d, want exactly 1", err, calls)
	}
	for _, check := range doc.Checks {
		if check.ID == "db" {
			if check.Severity != sevDanger {
				t.Fatalf("db check severity=%q, want danger", check.Severity)
			}
			return
		}
	}
	t.Fatal("no db check in the document")
}

func TestDBDiskFull(t *testing.T) {
	pg := func(code string) error { return fmt.Errorf("write: %w", &pgconn.PgError{Code: code}) }
	clock := fixedNow
	mk := func() (*Service, *dbdiskfull.Signal) {
		sig := dbdiskfull.New(func() time.Time { return clock })
		svc := New(Config{Store: &fakeStore{}, Settings: &fakeSettings{}, DiskFull: sig, Now: func() time.Time { return clock }})
		svc.probeDB = func(context.Context) dbStat { return dbStat{schemaAtHead: true, maxConns: 20} }
		return svc, sig
	}
	t.Run("store write error and commit-shaped error go danger then recover", func(t *testing.T) {
		for _, err := range []error{pg("53100"), fmt.Errorf("commit: %w", fmt.Errorf("tx: %w", &pgconn.PgError{Code: "53100"}))} {
			clock = fixedNow
			svc, sig := mk()
			sig.Observe(err)
			c := svc.checkDB(context.Background(), svc.now())
			if c.Severity != sevDanger || c.Summary != "Database writes are failing: disk full (53100)." {
				t.Fatalf("got %q / %q", c.Severity, c.Summary)
			}
			if len(c.Evidence) != 1 || c.Evidence[0].Label != "Last seen" || c.Evidence[0].Value != fixedNow.UTC().Format(time.RFC3339) {
				t.Fatalf("evidence = %+v", c.Evidence)
			}
			clock = fixedNow.Add(dbdiskfull.Window + time.Second)
			if c := svc.checkDB(context.Background(), svc.now()); c.Severity != sevOK {
				t.Fatalf("after window: %q / %q", c.Severity, c.Summary)
			}
		}
	})
	t.Run("since is the incident start, evidence is the last sighting", func(t *testing.T) {
		clock = fixedNow
		svc, sig := mk()
		sig.Observe(pg("53100"))
		clock = fixedNow.Add(time.Minute)
		sig.Observe(pg("53100"))
		c := svc.checkDB(context.Background(), svc.now())
		if c.Since == nil || *c.Since != fixedNow.UTC().Format(time.RFC3339) {
			t.Fatalf("Since = %v, want %v", c.Since, fixedNow.UTC().Format(time.RFC3339))
		}
		if c.Evidence[0].Value != fixedNow.Add(time.Minute).UTC().Format(time.RFC3339) {
			t.Fatalf("evidence = %+v", c.Evidence)
		}
	})
	t.Run("nil probe does not hide it", func(t *testing.T) {
		clock = fixedNow
		svc, sig := mk()
		svc.probeDB = nil
		sig.Observe(pg("53100"))
		if c := svc.checkDB(context.Background(), svc.now()); c.Severity != sevDanger {
			t.Fatalf("got %q", c.Severity)
		}
	})
	t.Run("other errors stay ok", func(t *testing.T) {
		for _, err := range []error{pg("53200"), pg("53300"), errors.New("unrelated")} {
			clock = fixedNow
			svc, sig := mk()
			sig.Observe(err)
			if c := svc.checkDB(context.Background(), svc.now()); c.Severity != sevOK {
				t.Fatalf("%v: got %q / %q", err, c.Severity, c.Summary)
			}
		}
	})
}

// ---- slack.socket ----------------------------------------------------------

func TestSlackSocket(t *testing.T) {
	t.Run("na when disabled", func(t *testing.T) {
		svc := New(Config{Store: &fakeStore{}, SlackState: func() string { return slacksvc.StateDisabled }, Now: func() time.Time { return fixedNow }})
		c := svc.checkSlackSocket(fixedNow)
		if c.Severity != sevNA || !strings.Contains(c.Summary, "not configured") {
			t.Fatalf("got %q / %q", c.Severity, c.Summary)
		}
	})
	t.Run("ok when connected", func(t *testing.T) {
		svc := New(Config{Store: &fakeStore{}, SlackState: func() string { return slacksvc.StateConnected }, Now: func() time.Time { return fixedNow }})
		c := svc.checkSlackSocket(fixedNow)
		if c.Severity != sevOK || !strings.Contains(c.Summary, "connected") {
			t.Fatalf("got %q / %q", c.Severity, c.Summary)
		}
	})
	t.Run("ok then warn as disconnection ages, reset on reconnect", func(t *testing.T) {
		state := slacksvc.StateErrorConnection
		svc := New(Config{Store: &fakeStore{}, SlackState: func() string { return state }})
		// First evaluation: disconnected now, under the 5m window ⇒ ok, but since is armed.
		if c := svc.checkSlackSocket(fixedNow); c.Severity != sevOK {
			t.Fatalf("first eval severity = %q, want ok (%q)", c.Severity, c.Summary)
		}
		// Six minutes later, still disconnected ⇒ warn.
		if c := svc.checkSlackSocket(fixedNow.Add(6 * time.Minute)); c.Severity != sevWarn || !strings.Contains(c.Summary, "disconnected for 6m") {
			t.Fatalf("aged eval = %q / %q, want warn", c.Severity, c.Summary)
		}
		// Reconnect clears the armed timestamp.
		state = slacksvc.StateConnected
		if c := svc.checkSlackSocket(fixedNow.Add(7 * time.Minute)); c.Severity != sevOK {
			t.Fatalf("reconnect severity = %q, want ok", c.Severity)
		}
		// A fresh disconnection re-arms from zero (ok again, not warn).
		state = slacksvc.StateErrorConnection
		if c := svc.checkSlackSocket(fixedNow.Add(8 * time.Minute)); c.Severity != sevOK {
			t.Fatalf("re-armed severity = %q, want ok (the since must have reset on reconnect)", c.Severity)
		}
	})
}

// ---- schedules.paused ------------------------------------------------------

func TestSchedulesPaused(t *testing.T) {
	for _, tc := range []struct {
		name       string
		count      int64
		wantSev    string
		wantSubstr string
	}{
		{"ok when none", 0, sevOK, "No user has paused"},
		{"warn when some", 2, sevWarn, "2 user(s) have paused all schedules"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newSvc(&fakeStore{pausedCount: tc.count}, &fakeSettings{})
			c := svc.checkSchedulesPaused(context.Background(), fixedNow)
			if c.Severity != tc.wantSev || !strings.Contains(c.Summary, tc.wantSubstr) {
				t.Fatalf("got %q / %q", c.Severity, c.Summary)
			}
		})
	}
}

// ---- board.drift -----------------------------------------------------------

func TestBoardDrift(t *testing.T) {
	withIssue := store.ListGaveUpColumnMovesRow{IssueIid: pgtype.Int8{Int64: 42, Valid: true}}
	noIssue := store.ListGaveUpColumnMovesRow{IssueIid: pgtype.Int8{Valid: false}}
	for _, tc := range []struct {
		name       string
		rows       []store.ListGaveUpColumnMovesRow
		wantSev    string
		wantSubstr string
	}{
		{"ok when none", nil, sevOK, "No column move has been given up"},
		{"issue-less rows do not count (D12)", []store.ListGaveUpColumnMovesRow{noIssue, noIssue}, sevOK, "No column move has been given up"},
		{"warn on an issue move", []store.ListGaveUpColumnMovesRow{withIssue, noIssue}, sevWarn, "1 issue column move(s) were given up"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newSvc(&fakeStore{gaveUp: tc.rows}, &fakeSettings{})
			c := svc.checkBoardDrift(context.Background(), fixedNow)
			if c.Severity != tc.wantSev || !strings.Contains(c.Summary, tc.wantSubstr) {
				t.Fatalf("got %q / %q", c.Severity, c.Summary)
			}
		})
	}
}

// ---- custody.holds ---------------------------------------------------------

func TestCustodyHolds(t *testing.T) {
	for _, tc := range []struct {
		name       string
		limit      int32
		owners     []uuid.UUID
		wantSev    string
		wantSubstr string
	}{
		{"na when limit not configured", 0, nil, sevNA, "not configured"},
		{"ok when none over", 3, nil, sevOK, "No owner is at the custody-hold"},
		{"warn when owners over", 3, []uuid.UUID{uuid.New(), uuid.New()}, sevWarn, "2 owner(s) are at the custody-hold"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := New(Config{Store: &fakeStore{custodyOwners: tc.owners}, Settings: &fakeSettings{}, Now: func() time.Time { return fixedNow }, CustodyHoldLimit: tc.limit})
			c := svc.checkCustodyHolds(context.Background(), svc.now())
			if c.Severity != tc.wantSev || !strings.Contains(c.Summary, tc.wantSubstr) {
				t.Fatalf("got %q / %q", c.Severity, c.Summary)
			}
		})
	}
}

func TestCustodyHoldsAdvice(t *testing.T) {
	active := store.ListCustodyHoldsForOwnerRow{Attention: "active", DecisionNeeded: false, State: "open", RunStatus: "running", RunID: uuid.New(), Generation: 2}
	capturing := store.ListCustodyHoldsForOwnerRow{Attention: "capturing", DecisionNeeded: false, State: "open", RunStatus: "failed", CaptureState: "uploading"}
	ready := store.ListCustodyHoldsForOwnerRow{Attention: "archive_ready", DecisionNeeded: false, State: "open", RunStatus: "failed", HasAvailableCapture: true, CaptureState: "needs_action"}
	decision := store.ListCustodyHoldsForOwnerRow{Attention: "needs_action", DecisionNeeded: true, State: "open", RunStatus: "running", CaptureState: "needs_action"}
	sourceOnly := store.ListCustodyHoldsForOwnerRow{Attention: "source_only", DecisionNeeded: true, State: "open", RunStatus: "completed", RunID: uuid.New(), Generation: 1}
	guarded := ready
	guarded.InventoryGuarded = true
	guarded.DecisionNeeded = true
	guarded.Attention = "needs_action"
	for _, tc := range []struct {
		name          string
		holds         [][]store.ListCustodyHoldsForOwnerRow
		with, without string
		action        string
	}{
		{"no decisions", [][]store.ListCustodyHoldsForOwnerRow{{active, capturing, ready}}, "0", "1",
			"No owner decision is currently required; admission resumes when counted holds settle."},
		{"decisions counted once", [][]store.ListCustodyHoldsForOwnerRow{{decision, sourceOnly, guarded}}, "1", "0",
			"Owners with decision-bearing holds can discard or resolve those holds as appropriate (uzi run recovery). Admission resumes when counted holds settle."},
		{"mixed", [][]store.ListCustodyHoldsForOwnerRow{{decision, sourceOnly}, {active, capturing, ready}}, "1", "1",
			"Owners with decision-bearing holds can discard or resolve those holds as appropriate (uzi run recovery). Admission resumes when counted holds settle. Other at-limit owners need no decision; admission resumes when their counted holds settle."},
		{"SQL decision overrides raw labels", [][]store.ListCustodyHoldsForOwnerRow{{{State: "released", HasAvailableCapture: true, Attention: "active", DecisionNeeded: true}}}, "1", "0",
			"Owners with decision-bearing holds can discard or resolve those holds as appropriate (uzi run recovery). Admission resumes when counted holds settle."},
		// A hold from the terminal predecessor still needs a decision even when
		// the same owner's newer run has an active hold.
		{"terminal predecessor", [][]store.ListCustodyHoldsForOwnerRow{{sourceOnly, active}}, "1", "0",
			"Owners with decision-bearing holds can discard or resolve those holds as appropriate (uzi run recovery). Admission resumes when counted holds settle."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{}
			byOwner := make(map[uuid.UUID][]store.ListCustodyHoldsForOwnerRow)
			for _, holds := range tc.holds {
				owner := uuid.New()
				fs.custodyOwners = append(fs.custodyOwners, owner)
				byOwner[owner] = holds
			}
			fs.custodyList = func(_ context.Context, arg store.ListCustodyHoldsForOwnerParams) ([]store.ListCustodyHoldsForOwnerRow, error) {
				return byOwner[arg.UserID], nil
			}
			c := newSvc(fs, &fakeSettings{}).checkCustodyHolds(context.Background(), fixedNow)
			if c.Severity != sevWarn || c.Action == nil || *c.Action != tc.action {
				t.Fatalf("advice = %+v (action %v), want warn with %q", c, c.Action, tc.action)
			}
			want := []apitypes.HealthEvidenceDTO{
				{Label: "Owners", Value: fmt.Sprint(len(tc.holds))},
				{Label: "Admission-counted holds", Value: "0"},
				{Label: "Total open custody", Value: "0"},
				{Label: "Owners with decisions", Value: tc.with},
				{Label: "Owners without decisions", Value: tc.without},
			}
			if fmt.Sprint(c.Evidence) != fmt.Sprint(want) {
				t.Fatalf("evidence = %+v, want %+v", c.Evidence, want)
			}
			if len(fs.custodyQueries) != len(fs.custodyOwners) {
				t.Fatalf("listing calls = %d, want %d", len(fs.custodyQueries), len(fs.custodyOwners))
			}
			for i, arg := range fs.custodyQueries {
				wantArg := store.ListCustodyHoldsForOwnerParams{UserID: fs.custodyOwners[i], State: pgtype.Text{String: "open", Valid: true}}
				if arg != wantArg {
					t.Fatalf("listing filter = %+v, want %+v", arg, wantArg)
				}
			}
		})
	}
}

func TestCustodyHoldsReadFailures(t *testing.T) {
	owners := []uuid.UUID{uuid.New(), uuid.New()}
	decision := []store.ListCustodyHoldsForOwnerRow{{State: "open", RunStatus: "failed", DecisionNeeded: true}}
	for _, tc := range []struct {
		name string
		at   int
	}{
		{"first owner", 0},
		{"second owner after partial classification", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{custodyOwners: owners}
			fs.custodyList = func(_ context.Context, arg store.ListCustodyHoldsForOwnerParams) ([]store.ListCustodyHoldsForOwnerRow, error) {
				if arg.UserID == owners[tc.at] {
					return nil, errors.New("listing failed")
				}
				return decision, nil
			}
			c := newSvc(fs, &fakeSettings{}).checkCustodyHolds(context.Background(), fixedNow)
			if c.Severity != sevUnknown || c.Action != nil || len(c.Evidence) != 0 {
				t.Fatalf("failed listing returned advice or partial evidence: %+v", c)
			}
		})
	}
}

func TestCustodyHoldsCancellation(t *testing.T) {
	for _, stage := range []string{"pre-cancel", "selection wait", "listing wait", "after empty selection", "before owner", "before advice"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			owner := uuid.New()
			fs := &fakeStore{custodyOwners: []uuid.UUID{owner}}
			fs.custodySelect = func(readCtx context.Context, _ store.ListOwnersOverCustodyLimitParams) ([]uuid.UUID, error) {
				switch stage {
				case "selection wait":
					<-readCtx.Done()
					return nil, readCtx.Err()
				case "after empty selection":
					cancel()
					return nil, nil
				case "before owner":
					cancel()
				}
				return fs.custodyOwners, nil
			}
			fs.custodyList = func(readCtx context.Context, _ store.ListCustodyHoldsForOwnerParams) ([]store.ListCustodyHoldsForOwnerRow, error) {
				if stage == "listing wait" {
					<-readCtx.Done()
					return nil, readCtx.Err()
				}
				if stage == "before advice" {
					cancel()
				}
				return []store.ListCustodyHoldsForOwnerRow{{State: "open", RunStatus: "failed", DecisionNeeded: true}}, nil
			}
			if stage == "pre-cancel" {
				cancel()
			}
			start := time.Now()
			c := newSvc(fs, &fakeSettings{}).checkCustodyHolds(ctx, fixedNow)
			if c.Severity != sevUnknown || c.Action != nil || len(c.Evidence) != 0 {
				t.Fatalf("cancelled read returned health, advice or partial evidence: %+v", c)
			}
			if time.Since(start) > time.Second {
				t.Fatal("read did not honor short parent deadline")
			}
			if (stage == "pre-cancel" || stage == "before owner" || stage == "after empty selection") && len(fs.custodyQueries) != 0 {
				t.Fatal("listed holds after cancellation")
			}
		})
	}
}

func TestCustodyHoldsSharedReadBudget(t *testing.T) {
	owner := uuid.New()
	fs := &fakeStore{}
	var selectionCtx context.Context
	start := time.Now()
	fs.custodySelect = func(ctx context.Context, arg store.ListOwnersOverCustodyLimitParams) ([]uuid.UUID, error) {
		if arg.CustodyHoldLimit != 3 {
			t.Fatalf("owner selection limit = %d, want 3", arg.CustodyHoldLimit)
		}
		deadline, ok := ctx.Deadline()
		if !ok || deadline.Before(start.Add(3*time.Second)) || deadline.After(start.Add(4*time.Second+100*time.Millisecond)) {
			t.Fatalf("selection deadline = %v, present = %v; want four-second budget", deadline, ok)
		}
		selectionCtx = ctx
		return []uuid.UUID{owner}, nil
	}
	fs.custodyList = func(ctx context.Context, _ store.ListCustodyHoldsForOwnerParams) ([]store.ListCustodyHoldsForOwnerRow, error) {
		if ctx != selectionCtx {
			t.Fatal("owner reads must share selection context and budget")
		}
		return nil, nil
	}
	c := newSvc(fs, &fakeSettings{}).checkCustodyHolds(context.Background(), fixedNow)
	if c.Severity != sevWarn {
		t.Fatalf("severity = %q, want warn", c.Severity)
	}
	if selectionCtx.Err() != context.Canceled {
		t.Fatal("read context was not cancelled on return")
	}
}

func TestCustodyHoldsSkipListing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limit  int32
		owners []uuid.UUID
		want   string
	}{
		{"disabled", 0, []uuid.UUID{uuid.New()}, sevNA},
		{"no owners", 3, nil, sevOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{custodyOwners: tc.owners}
			fs.custodySelect = func(context.Context, store.ListOwnersOverCustodyLimitParams) ([]uuid.UUID, error) {
				if tc.limit == 0 {
					t.Fatal("selected owners when disabled")
				}
				return tc.owners, nil
			}
			c := New(Config{Store: fs, CustodyHoldLimit: tc.limit}).checkCustodyHolds(context.Background(), fixedNow)
			if c.Severity != tc.want || len(fs.custodyQueries) != 0 || c.Action != nil {
				t.Fatalf("skip listing = %+v, calls = %d", c, len(fs.custodyQueries))
			}
		})
	}
}

// ---- release.check ---------------------------------------------------------

func TestReleaseCheck(t *testing.T) {
	// far_behind requires a valid, newer semver latest tag; a major gap is unambiguously
	// far behind (see releasecheck.FarBehind).
	farBehind := settings.ReleaseStatus{LatestTag: "v2.0.0"}
	current := settings.ReleaseStatus{LatestTag: "v0.84.0"}
	for _, tc := range []struct {
		name       string
		enabled    bool
		status     settings.ReleaseStatus
		wantSev    string
		wantSubstr string
	}{
		{"na when disabled", false, current, sevNA, "release check is disabled"},
		{"ok when current", true, current, sevOK, "on a recent release"},
		{"warn when far behind", true, farBehind, sevWarn, "far behind the latest release"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := New(Config{Store: &fakeStore{}, Settings: &fakeSettings{releaseEnabled: tc.enabled, release: tc.status}, Now: func() time.Time { return fixedNow }, RunningVersion: "0.84.0"})
			c := svc.checkReleaseCheck(context.Background(), fixedNow)
			if c.Severity != tc.wantSev || !strings.Contains(c.Summary, tc.wantSubstr) {
				t.Fatalf("got %q / %q", c.Severity, c.Summary)
			}
		})
	}
	t.Run("unknown when settings unavailable", func(t *testing.T) {
		svc := New(Config{Store: &fakeStore{}, Now: func() time.Time { return fixedNow }})
		c := svc.checkReleaseCheck(context.Background(), fixedNow)
		if c.Severity != sevUnknown {
			t.Fatalf("got %q, want unknown", c.Severity)
		}
	})
}

// ---- Evaluate: rollup, tally, health_enabled off, full registry ------------

func TestEvaluateRollupAndRegistry(t *testing.T) {
	svc := New(Config{
		Store:               &fakeStore{},
		Settings:            &fakeSettings{healthEnabled: true, releaseEnabled: true, release: settings.ReleaseStatus{LatestTag: "v0.84.0"}},
		SlackState:          func() string { return slacksvc.StateDisabled },
		Now:                 func() time.Time { return fixedNow },
		HostedWorkerVersion: "", // no hosted workers configured ⇒ fleet.roll na
		RunningVersion:      "0.84.0",
		CustodyHoldLimit:    3,
	})
	svc.probeDB = func(context.Context) dbStat {
		return dbStat{pingDur: time.Millisecond, acquiredConns: 1, maxConns: 20, schemaAtHead: true}
	}
	doc, err := svc.Evaluate(context.Background())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	// The full registry, in a stable order (PRD "Checks in v1" table order), always
	// present. M2-B added controller.report + loops (control) and forge.ciwatch
	// (integrations), so it is 14, not 11; PRD #1809 M6 added fleet.rundisk
	// (workers), issue #2203 adds forge.sync (integrations), and issue #2213 adds
	// fleet.quarantine (workers); pricing.codex follows release.check, db.size follows db and
	// recovery.storage follows custody.holds, making 20.
	wantIDs := []string{
		"fleet.roll", "fleet.capacity", "fleet.disk", "fleet.rundisk", "fleet.quarantine", "queue.waiting", "queue.undispatched",
		"controller.report", "db", "db.size", "loops", "forge.ciwatch", "forge.sync", "slack.socket",
		"schedules.paused", "board.drift", "custody.holds", "recovery.storage", "release.check", "pricing.codex",
	}
	if len(doc.Checks) != len(wantIDs) {
		t.Fatalf("doc has %d checks, want %d", len(doc.Checks), len(wantIDs))
	}
	for i, id := range wantIDs {
		if doc.Checks[i].ID != id {
			t.Fatalf("check %d = %q, want %q (order is load-bearing)", i, doc.Checks[i].ID, id)
		}
		if !find(t, doc, id) {
			t.Fatalf("missing check %q", id)
		}
	}
	// A clean empty-fleet instance with Slack off is ok overall.
	if doc.Status != sevOK {
		t.Fatalf("overall status = %q, want ok\n%+v", doc.Status, doc.Checks)
	}
	// Every check DTO carries a non-nil evidence slice (marshals [] not null).
	for _, c := range doc.Checks {
		if c.Evidence == nil {
			t.Fatalf("check %q has nil evidence", c.ID)
		}
	}
	// M1 always leaves the episode/snooze coordinates null.
	if doc.SnoozedUntil != nil || doc.EpisodeID != nil {
		t.Fatalf("M1 doc must carry null snoozed_until/episode_id, got %v/%v", doc.SnoozedUntil, doc.EpisodeID)
	}
	// tally sums to the registry size.
	total := doc.Counts.OK + doc.Counts.Warn + doc.Counts.Danger + doc.Counts.Unknown + doc.Counts.NA
	if total != len(wantIDs) {
		t.Fatalf("counts sum to %d, want %d", total, len(wantIDs))
	}
}

func TestEvaluateHealthDisabled(t *testing.T) {
	svc := New(Config{
		Store:               &fakeStore{},
		Settings:            &fakeSettings{healthEnabled: false, releaseEnabled: true, release: settings.ReleaseStatus{LatestTag: "v0.84.0"}},
		SlackState:          func() string { return slacksvc.StateDisabled },
		Now:                 func() time.Time { return fixedNow },
		HostedWorkerVersion: "",
		RunningVersion:      "0.84.0",
		CustodyHoldLimit:    3,
	})
	svc.probeDB = func(context.Context) dbStat {
		return dbStat{pingDur: time.Millisecond, acquiredConns: 1, maxConns: 20, schemaAtHead: true}
	}
	doc, err := svc.Evaluate(context.Background())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	// The two run-health-derived checks report unknown, never ok, when the detector is off.
	if sev, summary := check(t, doc, "fleet.capacity"); sev != sevUnknown {
		t.Fatalf("fleet.capacity = %q (%q), want unknown when health_enabled off", sev, summary)
	}
	if sev, summary := check(t, doc, "queue.waiting"); sev != sevUnknown {
		t.Fatalf("queue.waiting = %q (%q), want unknown when health_enabled off", sev, summary)
	}
	// unknown ranks as warn for the overall rollup.
	if doc.Status != sevWarn {
		t.Fatalf("overall status = %q, want warn (unknown ranks as warn)", doc.Status)
	}
}

// TestEvaluateHealthReadError covers Fix 1: when the run-health kill switch read ERRORS,
// the two run-health-derived checks must degrade to `unknown` with the distinct read-error
// summary — never default to enabled and query the run tables for a green verdict (D6). If
// the degrade were removed (read error swallowed as enabled=true), both checks would query
// the empty fake store and read `ok`, so these assertions fail exactly when the fix is gone.
func TestEvaluateHealthReadError(t *testing.T) {
	svc := New(Config{
		Store:               &fakeStore{},
		Settings:            &fakeSettings{healthErr: errors.New("settings read failed"), releaseEnabled: true, release: settings.ReleaseStatus{LatestTag: "v0.84.0"}},
		SlackState:          func() string { return slacksvc.StateDisabled },
		Now:                 func() time.Time { return fixedNow },
		HostedWorkerVersion: "",
		RunningVersion:      "0.84.0",
		CustodyHoldLimit:    3,
	})
	svc.probeDB = func(context.Context) dbStat {
		return dbStat{pingDur: time.Millisecond, acquiredConns: 1, maxConns: 20, schemaAtHead: true}
	}
	doc, err := svc.Evaluate(context.Background())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	const wantSummary = "Run-health detection state could not be determined."
	for _, id := range []string{"fleet.capacity", "queue.waiting"} {
		sev, summary := check(t, doc, id)
		if sev != sevUnknown {
			t.Fatalf("%s = %q (%q), want unknown when the HealthEnabled read errors", id, sev, summary)
		}
		if summary != wantSummary {
			t.Fatalf("%s summary = %q, want the distinct read-error summary %q", id, summary, wantSummary)
		}
	}
	// unknown ranks as warn for the overall rollup.
	if doc.Status != sevWarn {
		t.Fatalf("overall status = %q, want warn (unknown ranks as warn)", doc.Status)
	}
}

// TestDegradeUnknownOnQueryError covers Fix 2: a per-check query/read failure must degrade
// that check to `unknown` with the degradeUnknown summary, never let a nil result read
// green. Each subtest injects a non-nil error into the fake and asserts the check emits
// `unknown` + the shared summary; if the degrade path were removed, each would fall through
// to its ok verdict (queue.undispatched/release.check status especially would read green).
func TestDegradeUnknownOnQueryError(t *testing.T) {
	const degradeSummary = "This signal is temporarily unavailable."
	boom := errors.New("query failed")

	t.Run("fleet.capacity", func(t *testing.T) {
		svc := newSvc(&fakeStore{capacityErr: boom}, &fakeSettings{})
		assertUnknown(t, svc.checkFleetCapacity(context.Background(), fixedNow, healthDetectorEnabled), degradeSummary)
	})
	t.Run("queue.waiting", func(t *testing.T) {
		svc := newSvc(&fakeStore{waitingErr: boom}, &fakeSettings{})
		assertUnknown(t, svc.checkQueueWaiting(context.Background(), fixedNow, healthDetectorEnabled), degradeSummary)
	})
	t.Run("queue.undispatched", func(t *testing.T) {
		svc := newSvc(&fakeStore{undispatchErr: boom}, &fakeSettings{})
		assertUnknown(t, svc.checkQueueUndispatched(context.Background(), fixedNow), degradeSummary)
	})
	t.Run("schedules.paused", func(t *testing.T) {
		svc := newSvc(&fakeStore{pausedErr: boom}, &fakeSettings{})
		assertUnknown(t, svc.checkSchedulesPaused(context.Background(), fixedNow), degradeSummary)
	})
	t.Run("board.drift", func(t *testing.T) {
		svc := newSvc(&fakeStore{gaveUpErr: boom}, &fakeSettings{})
		assertUnknown(t, svc.checkBoardDrift(context.Background(), fixedNow), degradeSummary)
	})
	t.Run("custody.holds", func(t *testing.T) {
		// CustodyHoldLimit is 3 (via newSvc), so the na guard does not fire first.
		svc := newSvc(&fakeStore{custodyErr: boom}, &fakeSettings{})
		assertUnknown(t, svc.checkCustodyHolds(context.Background(), svc.now()), degradeSummary)
	})
	t.Run("release.check enabled read", func(t *testing.T) {
		svc := newSvc(&fakeStore{}, &fakeSettings{relEnabledErr: boom})
		assertUnknown(t, svc.checkReleaseCheck(context.Background(), fixedNow), degradeSummary)
	})
	t.Run("release.check status read", func(t *testing.T) {
		svc := newSvc(&fakeStore{}, &fakeSettings{releaseEnabled: true, relStatusErr: boom})
		assertUnknown(t, svc.checkReleaseCheck(context.Background(), fixedNow), degradeSummary)
	})
}

// assertUnknown fails unless the check emits severity `unknown` with the exact summary
// (text, not a bare count).
func assertUnknown(t *testing.T, c apitypes.HealthCheckDTO, wantSummary string) {
	t.Helper()
	if c.Severity != sevUnknown {
		t.Fatalf("severity = %q, want unknown (summary %q)", c.Severity, c.Summary)
	}
	if c.Summary != wantSummary {
		t.Fatalf("summary = %q, want %q", c.Summary, wantSummary)
	}
}

func TestEvaluateWorkerListErrorFails(t *testing.T) {
	svc := New(Config{Store: &fakeStore{workersErr: errors.New("db down")}, Settings: &fakeSettings{healthEnabled: true}, Now: func() time.Time { return fixedNow }})
	if _, err := svc.Evaluate(context.Background()); err == nil {
		t.Fatal("Evaluate must return an error when the worker list read fails")
	}
}

func TestOverallStatus(t *testing.T) {
	sev := func(vals ...string) []apitypes.HealthCheckDTO {
		out := make([]apitypes.HealthCheckDTO, len(vals))
		for i, v := range vals {
			out[i] = apitypes.HealthCheckDTO{Severity: v}
		}
		return out
	}
	for _, tc := range []struct {
		name string
		in   []apitypes.HealthCheckDTO
		want string
	}{
		{"all ok / na is ok", sev(sevOK, sevNA, sevOK), sevOK},
		{"warn present", sev(sevOK, sevWarn, sevNA), sevWarn},
		{"unknown ranks as warn", sev(sevOK, sevUnknown), sevWarn},
		{"danger dominates warn+unknown", sev(sevWarn, sevUnknown, sevDanger), sevDanger},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := overallStatus(tc.in); got != tc.want {
				t.Fatalf("overallStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---- hostile input ---------------------------------------------------------

func TestHostileInputSanitized(t *testing.T) {
	// A worker name with control bytes (an ANSI clear-screen + NUL + newline) and a
	// hostile blocking_reason fed through the registry: both must render with no control
	// characters and bounded length. This is the adversarial fixture the PRD requires.
	hostileName := "ev\x00il\x1b[2Jworker\nrow\tforge" + strings.Repeat("A", 500)
	hostileReason := "Image\x07Pull\x1b[31mBack\nOff" + strings.Repeat("B", 500)

	workers := []store.ListAllWorkersRow{
		hostedRow(hostileName, workersvc.PhaseStuck, 5*time.Second, hostileReason, "ag\x1bent", "0.84.0\x00"),
	}
	svc := New(Config{
		Store:               &fakeStore{workers: workers},
		Settings:            &fakeSettings{healthEnabled: true, releaseEnabled: false},
		SlackState:          func() string { return slacksvc.StateDisabled },
		Now:                 func() time.Time { return fixedNow },
		HostedWorkerVersion: "0.84.0",
		RunningVersion:      "0.84.0",
		CustodyHoldLimit:    3,
	})
	svc.probeDB = func(context.Context) dbStat {
		return dbStat{pingDur: time.Millisecond, acquiredConns: 1, maxConns: 20, schemaAtHead: true}
	}
	doc, err := svc.Evaluate(context.Background())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	for _, c := range doc.Checks {
		if c.ID != "fleet.roll" {
			continue
		}
		if c.Severity != sevDanger {
			t.Fatalf("fleet.roll severity = %q, want danger (single stuck worker)", c.Severity)
		}
		assertClean(t, "summary", c.Summary)
		for _, e := range c.Evidence {
			assertClean(t, "evidence["+e.Label+"]", e.Value)
			if len(e.Value) > maxEvidenceBytes {
				t.Fatalf("evidence %q value is %d bytes, want <= %d", e.Label, len(e.Value), maxEvidenceBytes)
			}
		}
		return
	}
	t.Fatal("fleet.roll not found in doc")
}

// assertClean fails if s carries any control character (terminal escapes, NUL, tab,
// newline, bidi override), the property the registry's text discipline guarantees.
func assertClean(t *testing.T, where, s string) {
	t.Helper()
	for _, r := range s {
		if unicode.IsControl(r) {
			t.Fatalf("%s carries a control character %U: %q", where, r, s)
		}
	}
}

func TestFleetCapacityRollConfirmation(t *testing.T) {
	owner := uuid.New()
	row := store.ListOwnersWaitingNoCapacityRow{UserID: owner, RunID: uuid.New(), HealthSince: pgtype.Timestamptz{Time: fixedNow.Add(-48 * time.Hour), Valid: true}, HasRollReason: true, HealthReason: pgtype.Text{String: workersvc.ReasonWorkersUpgrading, Valid: true}}
	eligible := store.CountOnlineWorkersClaimableForRunRow{DrainingEligible: 2, LatestSuitableDrainingSince: pgtype.Timestamptz{Time: fixedNow.Add(-time.Hour), Valid: true}}
	for _, tc := range []struct {
		name        string
		change      func(*store.ListOwnersWaitingNoCapacityRow, *store.CountOnlineWorkersClaimableForRunRow)
		err         error
		nilCallback bool
		want        string
		since       time.Time
	}{
		{name: "ordinary roll informational", want: sevOK},
		{name: "older unrelated health uses drain overlap", want: sevOK},
		{name: "24h equality", change: func(_ *store.ListOwnersWaitingNoCapacityRow, e *store.CountOnlineWorkersClaimableForRunRow) {
			e.LatestSuitableDrainingSince.Time = fixedNow.Add(-24 * time.Hour)
		}, want: sevDanger, since: fixedNow.Add(-24 * time.Hour)},
		{name: "just below 24h", change: func(_ *store.ListOwnersWaitingNoCapacityRow, e *store.CountOnlineWorkersClaimableForRunRow) {
			e.LatestSuitableDrainingSince.Time = fixedNow.Add(-24*time.Hour + time.Second)
		}, want: sevOK},
		{name: "health newer than drain", change: func(r *store.ListOwnersWaitingNoCapacityRow, e *store.CountOnlineWorkersClaimableForRunRow) {
			r.HealthSince.Time = fixedNow.Add(-time.Hour)
			e.LatestSuitableDrainingSince.Time = fixedNow.Add(-48 * time.Hour)
		}, want: sevOK},
		{name: "stale or incompatible now", change: func(_ *store.ListOwnersWaitingNoCapacityRow, e *store.CountOnlineWorkersClaimableForRunRow) {
			e.DrainingEligible = 0
		}, want: sevDanger, since: row.HealthSince.Time},
		{name: "non draining suitable", change: func(_ *store.ListOwnersWaitingNoCapacityRow, e *store.CountOnlineWorkersClaimableForRunRow) {
			e.NonDrainingEligible = 1
		}, want: sevDanger, since: row.HealthSince.Time},
		{name: "own veto one", change: func(_ *store.ListOwnersWaitingNoCapacityRow, e *store.CountOnlineWorkersClaimableForRunRow) {
			e.DrainingEligible = 1
			e.SuitableOwnDraining = 1
		}, want: sevDanger, since: row.HealthSince.Time},
		{name: "own veto several", change: func(_ *store.ListOwnersWaitingNoCapacityRow, e *store.CountOnlineWorkersClaimableForRunRow) {
			e.SuitableOwnDraining = 1
		}, want: sevDanger, since: row.HealthSince.Time},
		{name: "different stored reason", change: func(r *store.ListOwnersWaitingNoCapacityRow, _ *store.CountOnlineWorkersClaimableForRunRow) {
			r.HasRollReason = false
			r.HealthReason = pgtype.Text{}
		}, want: sevDanger, since: row.HealthSince.Time},
		{name: "failed callback", err: errors.New("eligibility failed"), want: sevDanger, since: row.HealthSince.Time},
		{name: "nil callback", nilCallback: true, want: sevDanger, since: row.HealthSince.Time},
		{name: "null latest drain", change: func(_ *store.ListOwnersWaitingNoCapacityRow, e *store.CountOnlineWorkersClaimableForRunRow) {
			e.LatestSuitableDrainingSince.Valid = false
		}, want: sevDanger, since: row.HealthSince.Time},
		{name: "infinite drain", change: func(_ *store.ListOwnersWaitingNoCapacityRow, e *store.CountOnlineWorkersClaimableForRunRow) {
			e.LatestSuitableDrainingSince.InfinityModifier = pgtype.Infinity
		}, want: sevDanger, since: row.HealthSince.Time},
		{name: "future drain", change: func(_ *store.ListOwnersWaitingNoCapacityRow, e *store.CountOnlineWorkersClaimableForRunRow) {
			e.LatestSuitableDrainingSince.Time = fixedNow.Add(time.Hour)
		}, want: sevDanger, since: row.HealthSince.Time},
		{name: "null health since", change: func(r *store.ListOwnersWaitingNoCapacityRow, _ *store.CountOnlineWorkersClaimableForRunRow) {
			r.HealthSince.Valid = false
		}, want: sevUnknown},
		{name: "genuine exact five minutes", change: func(r *store.ListOwnersWaitingNoCapacityRow, _ *store.CountOnlineWorkersClaimableForRunRow) {
			r.HasRollReason = false
			r.HealthReason = pgtype.Text{}
			r.HealthSince.Time = fixedNow.Add(-5 * time.Minute)
		}, want: sevDanger, since: fixedNow.Add(-5 * time.Minute)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, e := row, eligible
			if tc.change != nil {
				tc.change(&r, &e)
			}
			svc := newSvc(&fakeStore{capacityRows: []store.ListOwnersWaitingNoCapacityRow{r}}, &fakeSettings{})
			calls := 0
			if !tc.nilCallback {
				svc.cfg.WorkerEligibilityForHealth = func(_ context.Context, now time.Time, id uuid.UUID) (store.CountOnlineWorkersClaimableForRunRow, error) {
					calls++
					if id != r.RunID || !now.Equal(fixedNow) {
						t.Fatal("callback arguments")
					}
					return e, tc.err
				}
			}
			c := svc.checkFleetCapacity(context.Background(), fixedNow, healthDetectorEnabled)
			if c.Severity != tc.want {
				t.Fatalf("severity=%s want=%s summary=%s", c.Severity, tc.want, c.Summary)
			}
			if !tc.since.IsZero() && (c.Since == nil || *c.Since != tc.since.UTC().Format(time.RFC3339)) {
				t.Fatalf("since=%v want=%s", c.Since, tc.since)
			}
			if tc.name == "different stored reason" && calls != 0 {
				t.Fatal("different reason called eligibility")
			}
			if c.Severity == sevOK && r.HasRollReason && r.HealthSince.Valid && !strings.Contains(c.Summary, "upgrade") {
				t.Fatalf("missing informative upgrade summary: %s", c.Summary)
			}
		})
	}
}

// TestFleetCapacityRollConfirmationBounded pins the per-evaluation callback cap and the
// per-callback deadline: a fleet-wide roll with more waiting rows than the cap makes at
// most the cap's callbacks, each under a deadline, and an over-cap row stays unconfirmed.
func TestFleetCapacityRollConfirmationBounded(t *testing.T) {
	eligible := store.CountOnlineWorkersClaimableForRunRow{DrainingEligible: 1, LatestSuitableDrainingSince: pgtype.Timestamptz{Time: fixedNow.Add(-time.Hour), Valid: true}}
	rows := make([]store.ListOwnersWaitingNoCapacityRow, fleetCapacityMaxRollConfirmations+1)
	for i := range rows {
		rows[i] = store.ListOwnersWaitingNoCapacityRow{UserID: uuid.New(), RunID: uuid.New(), HealthSince: pgtype.Timestamptz{Time: fixedNow.Add(-time.Hour), Valid: true}, HasRollReason: true, HealthReason: pgtype.Text{String: workersvc.ReasonWorkersUpgrading, Valid: true}}
	}
	svc := newSvc(&fakeStore{capacityRows: rows}, &fakeSettings{})
	calls := 0
	svc.cfg.WorkerEligibilityForHealth = func(ctx context.Context, _ time.Time, _ uuid.UUID) (store.CountOnlineWorkersClaimableForRunRow, error) {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > fleetCapacityConfirmTimeout {
			t.Fatalf("callback deadline=%v ok=%v, want within %s", deadline, ok, fleetCapacityConfirmTimeout)
		}
		return eligible, nil
	}
	c := svc.checkFleetCapacity(context.Background(), fixedNow, healthDetectorEnabled)
	if calls != fleetCapacityMaxRollConfirmations {
		t.Fatalf("calls=%d want=%d", calls, fleetCapacityMaxRollConfirmations)
	}
	if c.Severity != sevDanger || !strings.Contains(c.Summary, "1 owner(s) affected") {
		t.Fatalf("over-cap row must stay unconfirmed: severity=%s summary=%s", c.Severity, c.Summary)
	}
}

// TestFleetCapacityRollConfirmationBudget pins the shared confirmation budget: slow
// callbacks that each run to their own deadline stop being made once the budget is
// spent, the evaluation returns well inside the api write timeout, the unconfirmed
// rows take genuine treatment, and the caller's context survives for later checks.
func TestFleetCapacityRollConfirmationBudget(t *testing.T) {
	rows := make([]store.ListOwnersWaitingNoCapacityRow, 10)
	for i := range rows {
		rows[i] = store.ListOwnersWaitingNoCapacityRow{UserID: uuid.New(), RunID: uuid.New(), HealthSince: pgtype.Timestamptz{Time: fixedNow.Add(-time.Hour), Valid: true}, HasRollReason: true, HealthReason: pgtype.Text{String: workersvc.ReasonWorkersUpgrading, Valid: true}}
	}
	svc := newSvc(&fakeStore{capacityRows: rows}, &fakeSettings{})
	calls := 0
	svc.cfg.WorkerEligibilityForHealth = func(ctx context.Context, _ time.Time, _ uuid.UUID) (store.CountOnlineWorkersClaimableForRunRow, error) {
		calls++
		<-ctx.Done()
		return store.CountOnlineWorkersClaimableForRunRow{}, ctx.Err()
	}
	ctx := context.Background()
	start := time.Now()
	c := svc.checkFleetCapacity(ctx, fixedNow, healthDetectorEnabled)
	elapsed := time.Since(start)
	// The api's WriteTimeout (api/cmd/server/main.go) is the absolute ceiling: a
	// constant-relative bound would pass a mutation that inflated the budget itself.
	const apiWriteTimeout = 15 * time.Second
	if elapsed >= apiWriteTimeout/2 {
		t.Fatalf("evaluation took %s, want well under the %s api write timeout", elapsed, apiWriteTimeout)
	}
	if calls >= len(rows) {
		t.Fatalf("calls=%d for %d rows: confirmations continued after the budget ran out", calls, len(rows))
	}
	if ctx.Err() != nil {
		t.Fatal("caller context was cancelled")
	}
	if c.Severity != sevDanger || !strings.Contains(c.Summary, fmt.Sprintf("%d owner(s) affected", len(rows))) {
		t.Fatalf("over-budget rows must stay unconfirmed: severity=%s summary=%s", c.Severity, c.Summary)
	}
}

func TestFleetCapacityGenuineThresholdActions(t *testing.T) {
	for _, age := range []time.Duration{5*time.Minute - time.Second, 5 * time.Minute} {
		t.Run(age.String(), func(t *testing.T) {
			svc := newSvc(&fakeStore{capacityRows: []store.ListOwnersWaitingNoCapacityRow{{UserID: uuid.New(), RunID: uuid.New(), HealthSince: pgtype.Timestamptz{Time: fixedNow.Add(-age), Valid: true}}}}, &fakeSettings{})
			c := svc.checkFleetCapacity(context.Background(), fixedNow, healthDetectorEnabled)
			if age < 5*time.Minute {
				if c.Severity != sevOK || c.Since != nil || c.Action != nil || c.Command != nil {
					t.Fatalf("transient=%+v", c)
				}
			} else {
				if c.Severity != sevDanger || c.Since == nil || *c.Since != fixedNow.Add(-age).Format(time.RFC3339) || c.Action == nil || !strings.Contains(strings.ToLower(*c.Action), "provision") {
					t.Fatalf("genuine danger=%+v", c)
				}
			}
		})
	}
}

func TestFleetCapacityYoungGenuineAndRoll(t *testing.T) {
	for _, sameOwner := range []bool{false, true} {
		t.Run(fmt.Sprint("same owner=", sameOwner), func(t *testing.T) {
			owner, other := uuid.New(), uuid.New()
			if sameOwner {
				other = owner
			}
			roll := uuid.New()
			rows := []store.ListOwnersWaitingNoCapacityRow{
				{UserID: owner, RunID: uuid.New(), HealthSince: pgtype.Timestamptz{Time: fixedNow.Add(-5*time.Minute + time.Second), Valid: true}},
				{UserID: other, RunID: roll, HasRollReason: true, HealthReason: pgtype.Text{String: workersvc.ReasonWorkersUpgrading, Valid: true}, HealthSince: pgtype.Timestamptz{Time: fixedNow.Add(-48 * time.Hour), Valid: true}},
			}
			svc := newSvc(&fakeStore{capacityRows: rows}, &fakeSettings{})
			svc.cfg.WorkerEligibilityForHealth = func(context.Context, time.Time, uuid.UUID) (store.CountOnlineWorkersClaimableForRunRow, error) {
				return store.CountOnlineWorkersClaimableForRunRow{DrainingEligible: 1, LatestSuitableDrainingSince: pgtype.Timestamptz{Time: fixedNow.Add(-23 * time.Hour), Valid: true}}, nil
			}
			c := svc.checkFleetCapacity(context.Background(), fixedNow, healthDetectorEnabled)
			if c.Severity != sevOK || c.Since != nil || c.Action != nil || c.Command != nil {
				t.Fatalf("young verdict=%+v", c)
			}
			if sameOwner && c.Summary != "Owners waiting for a worker are within the transient window." {
				t.Errorf("mixed owner must retain genuine transient summary: %q", c.Summary)
			}
		})
	}
}

func TestFleetCapacityOverdueOnlyAction(t *testing.T) {
	svc := newSvc(&fakeStore{capacityRows: []store.ListOwnersWaitingNoCapacityRow{{UserID: uuid.New(), RunID: uuid.New(), HasRollReason: true, HealthReason: pgtype.Text{String: workersvc.ReasonWorkersUpgrading, Valid: true}, HealthSince: pgtype.Timestamptz{Time: fixedNow.Add(-48 * time.Hour), Valid: true}}}}, &fakeSettings{})
	svc.cfg.WorkerEligibilityForHealth = func(context.Context, time.Time, uuid.UUID) (store.CountOnlineWorkersClaimableForRunRow, error) {
		return store.CountOnlineWorkersClaimableForRunRow{DrainingEligible: 1, LatestSuitableDrainingSince: pgtype.Timestamptz{Time: fixedNow.Add(-24 * time.Hour), Valid: true}}, nil
	}
	c := svc.checkFleetCapacity(context.Background(), fixedNow, healthDetectorEnabled)
	if c.Severity != sevDanger || c.Action == nil || c.Command == nil {
		t.Fatalf("overdue verdict=%+v", c)
	}
	if strings.Contains(strings.ToLower(*c.Action), "provision") || strings.Contains(strings.ToLower(*c.Action), "recover") || !strings.Contains(*c.Action, "fleet.roll") {
		t.Errorf("overdue-only action must direct roll investigation: %q", *c.Action)
	}
}

func TestFleetCapacityMixedOwnersAndAges(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	genuine, overdue := uuid.New(), uuid.New()
	rows := []store.ListOwnersWaitingNoCapacityRow{
		{UserID: a, RunID: genuine, HealthSince: pgtype.Timestamptz{Time: fixedNow.Add(-7 * time.Minute), Valid: true}, HasRollReason: true, HealthReason: pgtype.Text{String: workersvc.ReasonWorkersUpgrading, Valid: true}},
		{UserID: a, RunID: overdue, HealthSince: pgtype.Timestamptz{Time: fixedNow.Add(-72 * time.Hour), Valid: true}, HasRollReason: true, HealthReason: pgtype.Text{String: workersvc.ReasonWorkersUpgrading, Valid: true}},
		{UserID: b, RunID: uuid.New(), HealthSince: pgtype.Timestamptz{Time: fixedNow.Add(-10 * time.Minute), Valid: true}},
	}
	svc := newSvc(&fakeStore{capacityRows: rows}, &fakeSettings{})
	svc.cfg.WorkerEligibilityForHealth = func(_ context.Context, _ time.Time, id uuid.UUID) (store.CountOnlineWorkersClaimableForRunRow, error) {
		if id == genuine {
			return store.CountOnlineWorkersClaimableForRunRow{}, errors.New("one row failed")
		}
		return store.CountOnlineWorkersClaimableForRunRow{DrainingEligible: 2, LatestSuitableDrainingSince: pgtype.Timestamptz{Time: fixedNow.Add(-25 * time.Hour), Valid: true}}, nil
	}
	c := svc.checkFleetCapacity(context.Background(), fixedNow, healthDetectorEnabled)
	if c.Severity != sevDanger || c.Since == nil || *c.Since != fixedNow.Add(-25*time.Hour).Format(time.RFC3339) {
		t.Fatalf("mixed verdict: %+v", c)
	}
	values := map[string]string{}
	for _, e := range c.Evidence {
		values[e.Label] = e.Value
	}
	for label, want := range map[string]string{"Owners affected": "2", "Owners without capacity": "2", "Owners overdue during upgrade": "1", "Capacity wait since": fixedNow.Add(-10 * time.Minute).Format(time.RFC3339), "Upgrade overlap since": fixedNow.Add(-25 * time.Hour).Format(time.RFC3339)} {
		if values[label] != want {
			t.Errorf("%s=%s want=%s", label, values[label], want)
		}
	}
}

// ---- fleet.quarantine ------------------------------------------------------

func TestFleetQuarantine(t *testing.T) {
	wid := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	runID := uuid.MustParse("00000000-0000-4000-8000-000000000000")
	worker := func(name string, hbAgo time.Duration) store.ListAllWorkersRow {
		return store.ListAllWorkersRow{Worker: store.Worker{
			ID: wid, Name: name,
			LastHeartbeatAt: pgtype.Timestamptz{Time: fixedNow.Add(-hbAgo), Valid: true},
		}}
	}
	latched := func(id uuid.UUID) (workersvc.ResidueQuarantine, bool) {
		if id != wid {
			return workersvc.ResidueQuarantine{}, false
		}
		return workersvc.ResidueQuarantine{Cause: "RAWCAUSE-must-not-render", LatchedAt: fixedNow.Add(-90 * time.Minute), RunID: &runID, Site: "pre_clone"}, true
	}
	tests := []struct {
		name       string
		lookup     func(uuid.UUID) (workersvc.ResidueQuarantine, bool)
		worker     store.ListAllWorkersRow
		wantSev    string
		wantSubstr string
	}{
		{"ok when the seam is unwired", nil, worker("w", time.Second), sevOK, "No worker reports"},
		{"ok when nothing is latched", func(uuid.UUID) (workersvc.ResidueQuarantine, bool) { return workersvc.ResidueQuarantine{}, false }, worker("w", time.Second), sevOK, "No worker reports"},
		{"warn on a fresh latched worker", latched, worker("w", time.Second), sevWarn, "1 worker(s) are quarantined"},
		{"stale heartbeat is skipped", latched, worker("w", 5*time.Minute), sevOK, "No worker reports"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := New(Config{Store: &fakeStore{}, Settings: &fakeSettings{}, Now: func() time.Time { return fixedNow }, HeartbeatStale: 45 * time.Second, ResidueQuarantine: tc.lookup})
			c := svc.checkFleetQuarantine(fixedNow, []store.ListAllWorkersRow{tc.worker})
			if c.Severity != tc.wantSev || !strings.Contains(c.Summary, tc.wantSubstr) {
				t.Fatalf("got %q %q, want %q containing %q", c.Severity, c.Summary, tc.wantSev, tc.wantSubstr)
			}
			if c.Scope != "owner" || c.Group != groupWorkers {
				t.Fatalf("scope/group = %q/%q", c.Scope, c.Group)
			}
		})
	}

	t.Run("evidence names the worker, age and run and never the raw cause", func(t *testing.T) {
		svc := New(Config{Store: &fakeStore{}, Settings: &fakeSettings{}, Now: func() time.Time { return fixedNow }, HeartbeatStale: 45 * time.Second, ResidueQuarantine: latched})
		c := svc.checkFleetQuarantine(fixedNow, []store.ListAllWorkersRow{worker("evil\x1b[31m\u202ename\n", time.Second)})
		if len(c.Evidence) != 1 {
			t.Fatalf("evidence = %+v", c.Evidence)
		}
		v := c.Evidence[0].Value
		for _, want := range []string{"1h30m", runID.String()} {
			if !strings.Contains(v, want) {
				t.Fatalf("evidence %q lacks %q", v, want)
			}
		}
		blob := c.Summary + v
		for _, bad := range []string{"RAWCAUSE", "\x1b", "\u202e", "\n"} {
			if strings.Contains(blob, bad) {
				t.Fatalf("health text %q carries %q", blob, bad)
			}
		}
		if c.Since == nil {
			t.Fatal("since must carry the oldest latch time")
		}
		// The action must name surfaces that actually render the cause: the admin table
		// never does.
		if c.Action == nil || !strings.Contains(*c.Action, "uzi admin workers --json") || !strings.Contains(*c.Action, "uzi tui") {
			t.Fatalf("action = %v, want it to name uzi admin workers --json and uzi tui", c.Action)
		}
	})
}

func TestCustodyHoldsConfiguredCutoffAndFacts(t *testing.T) {
	owners := []uuid.UUID{uuid.New(), uuid.New()}
	st := &fakeStore{custodyOwners: owners, custodyAggregate: store.GetCustodyAggregateForOwnerRow{OpenHolds: 11, AdmissionCountedHolds: 8}}
	calls := 0
	svc := New(Config{Store: st, Settings: &fakeSettings{}, CustodyHoldLimit: 8, HeartbeatStale: 73 * time.Second, Now: func() time.Time {
		calls++
		return time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC).Add(time.Duration(calls-1) * time.Hour)
	}})
	doc, err := svc.Evaluate(context.Background())
	if err != nil || calls != 1 {
		t.Fatalf("Evaluate err=%v clock calls=%d", err, calls)
	}
	var got apitypes.HealthCheckDTO
	for _, check := range doc.Checks {
		if check.ID == "custody.holds" {
			got = check
		}
	}
	want := []apitypes.HealthEvidenceDTO{{Label: "Owners", Value: "2"}, {Label: "Admission-counted holds", Value: "16"}, {Label: "Total open custody", Value: "22"}, {Label: "Owners with decisions", Value: "0"}, {Label: "Owners without decisions", Value: "2"}}
	if got.Severity != sevWarn || !reflect.DeepEqual(got.Evidence, want) {
		t.Fatalf("custody check=%+v, want evidence=%v", got, want)
	}
	cutoff := time.Date(2026, 10, 7, 11, 58, 47, 0, time.UTC)
	if len(st.custodyListParams) != 1 || len(st.custodyAggregateParams) != 2 {
		t.Fatalf("list=%v aggregates=%v", st.custodyListParams, st.custodyAggregateParams)
	}
	list := st.custodyListParams[0]
	if list.CustodyHoldLimit != 8 || !list.HeartbeatCutoff.Valid || !list.HeartbeatCutoff.Time.Equal(cutoff) {
		t.Fatalf("list params=%v", list)
	}
	for i, arg := range st.custodyAggregateParams {
		if arg.UserID != owners[i] || arg.CustodyHoldLimit != 8 || arg.HeartbeatCutoff != list.HeartbeatCutoff {
			t.Fatalf("aggregate params=%v", arg)
		}
	}
}

func TestCustodyHoldsAggregateError(t *testing.T) {
	st := &fakeStore{custodyOwners: []uuid.UUID{uuid.New()}, custodyAggregateErr: errors.New("aggregate failed")}
	svc := New(Config{Store: st, CustodyHoldLimit: 8, HeartbeatStale: 73 * time.Second})
	got := svc.checkCustodyHolds(context.Background(), fixedNow)
	if got.Severity != sevUnknown || len(got.Evidence) != 0 {
		t.Fatalf("failed aggregate check=%+v", got)
	}
}

func (f *fakeStore) ListRecentUnpricedCodexModels(context.Context, store.ListRecentUnpricedCodexModelsParams) ([]store.ListRecentUnpricedCodexModelsRow, error) {
	return nil, nil
}
