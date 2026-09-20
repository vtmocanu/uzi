package healthsvc

import (
	"time"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Thresholds for the admin-health checks (PRD #1484). Every time/percentage the check
// severity logic compares against lives HERE, named once, never as a literal at a
// comparison site — the same discipline workersvc.UpgradeParams follows, and what the
// PRD means by "constants in v1, named in one place, not settings". A test overrides a
// check's behaviour by controlling the injected clock/data, never by editing a literal.
const (
	// fleetCapacityDanger: an owner with a waiting_worker run and zero usable workers is
	// a danger once the oldest such wait has lasted this long. Below it the capacity gap
	// may be a transient claim delay, so it does not yet alarm.
	fleetCapacityDanger = 5 * time.Minute

	// queue.waiting age bands over the oldest waiting_worker run.
	queueWaitingWarn   = 10 * time.Minute
	queueWaitingDanger = 30 * time.Minute

	// queue.undispatched: a task run queued with no dispatch older than this is a danger
	// (the #1367 failure class).
	queueUndispatchedDanger = 10 * time.Minute

	// fleet.disk: a worker with a fresh heartbeat and a debounced disk-pressure streak of
	// at least this many consecutive polls warns.
	diskPressureStreakWarn = 2

	// db check: a ping slower than this warns; the pool warns when acquired connections
	// reach this fraction of the max.
	dbPingWarn      = 250 * time.Millisecond
	dbPoolWarnRatio = 0.80

	// slack.socket: configured but not connected for at least this long warns. Below it a
	// reconnect blip is not worth alarming (the manager auto-reconnects).
	slackDisconnectedWarn = 5 * time.Minute

	// board.drift: a given-up column move counts when its marker crossed the give-up
	// boundary and is no older than this window.
	boardDriftWindow = 24 * time.Hour

	// boardGiveUp mirrors runlifecycle's give-up boundary: a move pending past this is
	// "given up". Declared here so board.drift's cutoffs are named, not literal.
	boardGiveUp = 30 * time.Minute

	// defaultHeartbeatStale is the fallback "fresh heartbeat" window when the caller
	// injects a non-positive one — it mirrors config's WORKER_HEARTBEAT_STALE default so
	// fleet.capacity and fleet.disk use the same "online worker" freshness the recovery
	// and claim queries do.
	defaultHeartbeatStale = 45 * time.Second

	// rollSignalTTL is how long a controller roll report stays fresh for fleet.roll. It
	// reuses workersvc's own TTL so the health check and the per-worker upgrade classifier
	// can never disagree on freshness.
	rollSignalTTL = workersvc.DefaultControllerSignalTTL

	// controllerReportInterval is how often the controller posts its status report — the
	// controller's CONTROLLER_POLL_INTERVAL default (controller/internal/config: 10s). The
	// controller.report unknown band is 3 of these intervals; there is no config for it on
	// the api side, so it is named once here rather than pulled from the wire's
	// poll_interval_seconds (which the report may or may not carry).
	controllerReportInterval = 10 * time.Second

	// controllerReportDanger: no controller report for this long is a danger (the fleet's
	// only fleet-independent liveness signal has gone silent). It doubles as the boot-grace
	// window: for the first controllerReportDanger after api start with no report yet, the
	// check is unknown rather than danger.
	controllerReportDanger = 5 * time.Minute

	// Loop-beat bands for the loops check, expressed as multiples of each loop's own tick
	// interval (D8): a loop whose last beat is older than loopBeatDangerIntervals of its
	// intervals is danger, older than loopBeatWarnIntervals is warn. A loop that has not
	// beaten since registration and is younger than loopBeatWarnIntervals intervals is
	// unknown (it has not yet had a chance to tick), not warn.
	loopBeatWarnIntervals   = 3
	loopBeatDangerIntervals = 10
)
