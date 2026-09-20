package healthsvc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
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
	workers       []store.ListAllWorkersRow
	workersErr    error
	capacityRows  []store.ListOwnersWaitingNoCapacityRow
	capacityErr   error
	waiting       pgtype.Timestamptz
	waitingErr    error
	undispatched  pgtype.Timestamptz
	undispatchErr error
	pausedCount   int64
	pausedErr     error
	gaveUp        []store.ListGaveUpColumnMovesRow
	gaveUpErr     error
	custodyOwners []uuid.UUID
	custodyErr    error
	controller    pgtype.Timestamptz
	controllerErr error
	ciwatch       []store.CountEligibleCIWatchRefsPerRepoRow
	ciwatchErr    error
}

func (f *fakeStore) ListAllWorkers(context.Context) ([]store.ListAllWorkersRow, error) {
	return f.workers, f.workersErr
}
func (f *fakeStore) ListOwnersWaitingNoCapacity(context.Context, pgtype.Timestamptz) ([]store.ListOwnersWaitingNoCapacityRow, error) {
	return f.capacityRows, f.capacityErr
}
func (f *fakeStore) OldestWaitingWorkerRun(context.Context) (pgtype.Timestamptz, error) {
	return f.waiting, f.waitingErr
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
func (f *fakeStore) ListOwnersOverCustodyLimit(context.Context, int32) ([]uuid.UUID, error) {
	return f.custodyOwners, f.custodyErr
}
func (f *fakeStore) GetControllerReport(context.Context) (pgtype.Timestamptz, error) {
	return f.controller, f.controllerErr
}
func (f *fakeStore) CountEligibleCIWatchRefsPerRepo(context.Context, pgtype.Timestamptz) ([]store.CountEligibleCIWatchRefsPerRepoRow, error) {
	return f.ciwatch, f.ciwatchErr
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
			UserID:            uuid.New(),
			OldestHealthSince: pgtype.Timestamptz{Time: fixedNow.Add(-ago), Valid: true},
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
			svc := newSvc(&fakeStore{waiting: tc.oldest}, &fakeSettings{})
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
			c := svc.checkDB(context.Background())
			if c.Severity != tc.wantSev {
				t.Fatalf("severity = %q, want %q (summary %q)", c.Severity, tc.wantSev, c.Summary)
			}
			if !strings.Contains(c.Summary, tc.wantSubstr) {
				t.Fatalf("summary %q does not contain %q", c.Summary, tc.wantSubstr)
			}
		})
	}
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
			c := svc.checkCustodyHolds(context.Background())
			if c.Severity != tc.wantSev || !strings.Contains(c.Summary, tc.wantSubstr) {
				t.Fatalf("got %q / %q", c.Severity, c.Summary)
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
	// (integrations), so it is 14, not 11.
	wantIDs := []string{
		"fleet.roll", "fleet.capacity", "fleet.disk", "queue.waiting", "queue.undispatched",
		"controller.report", "db", "loops", "forge.ciwatch", "slack.socket",
		"schedules.paused", "board.drift", "custody.holds", "release.check",
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
		assertUnknown(t, svc.checkCustodyHolds(context.Background()), degradeSummary)
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
