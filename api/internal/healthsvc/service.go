// Package healthsvc evaluates the closed registry of admin-health checks over data the
// api already holds (PRD #1484). It exposes one Evaluate(ctx) that the admin-health
// handler — and, from M2, the once-a-minute danger-episode evaluator — both call, so the
// endpoint and the notice can never disagree about the instance's health.
//
// It depends on an INJECTED Store interface, a Settings accessor, a slack-state accessor,
// a clock, a pgxpool.Pool (the db check) and two version/config scalars, so its severity
// logic is unit-tested with fakes and no database. It MAY import store for row types (it
// is not a loop package, so there is no import-cycle concern). It composes every
// user-visible string itself from a fixed template per check id plus numbers, closed enums
// and already-sanitized identifiers; NEVER free text from kube/forge/run/worker.
package healthsvc

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Doc is the evaluated health document. It is the exact wire type the handler marshals,
// so healthsvc and the JSON contract share one shape (no second copy to drift).
type Doc = apitypes.HealthDocDTO

// Severity values (closed enum). "unknown" ranks as "warn" for the overall status; "na"
// never contributes to it.
const (
	sevOK      = "ok"
	sevWarn    = "warn"
	sevDanger  = "danger"
	sevUnknown = "unknown"
	sevNA      = "na"
)

// Check groups (closed enum).
const (
	groupWorkers      = "workers"
	groupQueue        = "queue"
	groupControl      = "control"
	groupIntegrations = "integrations"
	groupHousekeeping = "housekeeping"
)

// Store is the exact slice of read methods the M1 checks need. *store.Queries satisfies
// it structurally, so healthsvc never depends on the concrete Queries and its severity
// logic is driven by a fake in unit tests.
type Store interface {
	// fleet.roll + fleet.disk read every worker with its roll-health join.
	ListAllWorkers(ctx context.Context) ([]store.ListAllWorkersRow, error)
	// fleet.capacity: owners waiting with zero usable (online, non-draining, fresh) workers.
	ListOwnersWaitingNoCapacity(ctx context.Context, heartbeatCutoff pgtype.Timestamptz) ([]store.ListOwnersWaitingNoCapacityRow, error)
	// queue.waiting: oldest waiting_worker run's health_since (nullable).
	OldestWaitingWorkerRun(ctx context.Context) (pgtype.Timestamptz, error)
	// queue.undispatched: oldest undispatched task run's created_at (nullable).
	OldestUndispatchedTaskRun(ctx context.Context) (pgtype.Timestamptz, error)
	// schedules.paused: count of users with a pause-all and >= 1 enabled schedule.
	CountUsersPausedWithEnabledSchedules(ctx context.Context, now pgtype.Timestamptz) (int64, error)
	// board.drift: given-up column moves in a cutoff window (issue_iid filtered in Go, D12).
	ListGaveUpColumnMoves(ctx context.Context, arg store.ListGaveUpColumnMovesParams) ([]store.ListGaveUpColumnMovesRow, error)
	// custody.holds: owners at/over the custody admission limit.
	ListOwnersOverCustodyLimit(ctx context.Context, custodyHoldLimit int32) ([]uuid.UUID, error)
}

// Settings is the slice of the settings cache healthsvc reads: the run-health kill switch
// (which gates the waiting_worker writer) and the release-check facts. *settings.Cache
// satisfies it.
type Settings interface {
	HealthEnabled(ctx context.Context) (bool, error)
	ReleaseCheckEnabled(ctx context.Context) (bool, error)
	ReleaseStatus(ctx context.Context) (settings.ReleaseStatus, error)
}

// dbStat is the db check's raw probe result. Separating it from the severity logic is what
// lets the db check be unit-tested (a fake probe) while production reads the live pool.
type dbStat struct {
	pingDur       time.Duration
	pingErr       error
	acquiredConns int32
	maxConns      int32
	schemaApplied int64
	schemaHead    int64
	schemaAtHead  bool
	schemaErr     error
}

// Config carries the injected collaborators. Every field is a seam a unit test overrides.
type Config struct {
	Store    Store
	Pool     *pgxpool.Pool
	Settings Settings
	// SlackState reports the live Slack socket state (slacksvc.State* strings); nil reads
	// as StateDisabled, so slack.socket is `na`.
	SlackState func() string
	// Now is the clock seam. nil defaults to time.Now.
	Now func() time.Time
	// HostedWorkerVersion is cfg.HostedWorkerVersion (config.go): non-empty means hosted
	// workers are CONFIGURED, which (with "any kind='hosted' worker exists") is the na
	// gate for the hosted checks.
	HostedWorkerVersion string
	// RunningVersion is this api's own served version ("dev" on an unstamped build), the
	// left operand of release.check's far-behind derivation.
	RunningVersion string
	// HeartbeatStale is config.WorkerHeartbeatStale: the "fresh heartbeat" window
	// fleet.capacity and fleet.disk use. <= 0 falls back to defaultHeartbeatStale.
	HeartbeatStale time.Duration
	// CustodyHoldLimit is workersvc.CustodyHoldLimit, the admission ceiling custody.holds
	// keys on.
	CustodyHoldLimit int32
}

// Service holds the injected deps plus the process-memory slack "first-saw-non-connected"
// timestamp the 5-minute slack.socket threshold needs (the manager exposes only the
// current state, not when it entered it).
type Service struct {
	cfg     Config
	now     func() time.Time
	probeDB func(ctx context.Context) dbStat

	mu                     sync.Mutex
	slackNonConnectedSince *time.Time
}

// New builds a Service from cfg. The db probe defaults to the live-pool probe; a nil pool
// leaves it nil (a struct-literal test that never runs the db check), and a test may
// assign s.probeDB directly to drive the db severity logic without a database.
func New(cfg Config) *Service {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	s := &Service{cfg: cfg, now: now}
	if cfg.Pool != nil {
		s.probeDB = s.livePoolProbe
	}
	return s
}

// heartbeatStale returns the configured fresh-heartbeat window, or the default.
func (s *Service) heartbeatStale() time.Duration {
	if s.cfg.HeartbeatStale > 0 {
		return s.cfg.HeartbeatStale
	}
	return defaultHeartbeatStale
}

// Evaluate runs the whole M1 registry and rolls the checks into one document. The checks
// are emitted in a stable order; a per-check DB failure degrades that check to `unknown`
// (never `ok`, per the PRD's "a missing signal is unknown" rule) rather than failing the
// whole evaluation, so one broken query cannot blank the page. The one error path is a
// failed worker-list read: fleet.roll and fleet.disk both depend on it and an admin-health
// page that cannot read the fleet is not a health page.
func (s *Service) Evaluate(ctx context.Context) (Doc, error) {
	now := s.now()

	workers, err := s.cfg.Store.ListAllWorkers(ctx)
	if err != nil {
		return Doc{}, err
	}

	healthEnabled := true
	if s.cfg.Settings != nil {
		if v, herr := s.cfg.Settings.HealthEnabled(ctx); herr == nil {
			healthEnabled = v
		}
	}

	checks := []apitypes.HealthCheckDTO{
		s.checkFleetRoll(now, workers),
		s.checkFleetCapacity(ctx, now, healthEnabled),
		s.checkFleetDisk(now, workers),
		s.checkQueueWaiting(ctx, now, healthEnabled),
		s.checkQueueUndispatched(ctx, now),
		s.checkDB(ctx),
		s.checkSlackSocket(now),
		s.checkSchedulesPaused(ctx, now),
		s.checkBoardDrift(ctx, now),
		s.checkCustodyHolds(ctx),
		s.checkReleaseCheck(ctx, now),
	}

	return Doc{
		Status:    overallStatus(checks),
		CheckedAt: now.UTC().Format(time.RFC3339),
		Counts:    tally(checks),
		Checks:    checks,
	}, nil
}

// overallStatus is the worst check: danger, then warn (with unknown ranking as warn). na
// never contributes, so an all-na / all-ok instance is ok.
func overallStatus(checks []apitypes.HealthCheckDTO) string {
	worst := sevOK
	for _, c := range checks {
		switch c.Severity {
		case sevDanger:
			return sevDanger
		case sevWarn, sevUnknown:
			worst = sevWarn
		}
	}
	return worst
}

// tally counts the checks by severity for the header pills.
func tally(checks []apitypes.HealthCheckDTO) apitypes.HealthCountsDTO {
	var c apitypes.HealthCountsDTO
	for _, chk := range checks {
		switch chk.Severity {
		case sevOK:
			c.OK++
		case sevWarn:
			c.Warn++
		case sevDanger:
			c.Danger++
		case sevUnknown:
			c.Unknown++
		case sevNA:
			c.NA++
		}
	}
	return c
}
