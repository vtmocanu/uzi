package healthsvc

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/releasecheck"
	"github.com/vtmocanu/uzi/api/internal/slacksvc"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// maxEvidenceBytes bounds any identifier/enum interpolated into a summary or evidence
// value — worker names, blocking reasons, tags. It is the last line of the text-discipline
// defense: even an already-sanitized enum is re-bounded and re-stripped here so a hostile
// value that slipped a prior gate cannot render control bytes or an unbounded string into
// an admin surface.
const maxEvidenceBytes = 96

// checkMeta is the fixed per-id metadata: the group, the human title, and the docs slug
// (empty ⇒ no doc link in M1). The registry order in Evaluate is the stable order; this
// map only supplies the constant fields so each check builder sets just severity + text.
var checkMeta = map[string]struct {
	group string
	title string
	doc   string
}{
	"fleet.roll":         {groupWorkers, "Worker image roll", "worker-upgrades"},
	"fleet.capacity":     {groupWorkers, "Worker capacity", "hosted-workers"},
	"fleet.disk":         {groupWorkers, "Worker disk", "hosted-workers"},
	"queue.waiting":      {groupQueue, "Runs waiting for a worker", ""},
	"queue.undispatched": {groupQueue, "Undispatched task runs", ""},
	"db":                 {groupControl, "Database", ""},
	"slack.socket":       {groupIntegrations, "Slack socket", ""},
	"schedules.paused":   {groupHousekeeping, "Paused schedules", ""},
	"board.drift":        {groupHousekeeping, "Board drift", ""},
	"custody.holds":      {groupHousekeeping, "Recovery custody holds", ""},
	"release.check":      {groupHousekeeping, "Upstream release", ""},
}

// base returns a check DTO pre-filled with the id's fixed metadata and a non-nil (empty)
// evidence slice, so every check marshals evidence as [] rather than null.
func (s *Service) base(id string) apitypes.HealthCheckDTO {
	m := checkMeta[id]
	c := apitypes.HealthCheckDTO{
		ID:       id,
		Group:    m.group,
		Title:    m.title,
		Evidence: []apitypes.HealthEvidenceDTO{},
	}
	if m.doc != "" {
		c.Doc = strPtr(m.doc)
	}
	return c
}

// strPtr returns a pointer to s (a required-non-nil string field).
func strPtr(s string) *string { return &s }

// sincePtr formats a source timestamp as an RFC3339 UTC string pointer for a check's
// `since`, nil when the source has none.
func sincePtr(t time.Time) *string {
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// safe strips control bytes (incl. tabs/newlines) and bounds an untrusted identifier or
// enum before it renders into an admin surface. CellText folds \t/\n to spaces and strips
// terminal escapes / bidi overrides; SanitizeBounded then caps the length. The result
// carries no control characters and is at most maxEvidenceBytes long.
func safe(v string) string {
	return termsafe.SanitizeBounded(termsafe.CellText(v), maxEvidenceBytes)
}

// humanDur renders a duration for a summary: whole minutes under an hour, else Hh Mm.
func humanDur(d time.Duration) string {
	if d < time.Minute {
		return "under a minute"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) - h*60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

// -------------------------------------------------------------------------
// workers group
// -------------------------------------------------------------------------

// checkFleetRoll classifies the hosted fleet's roll health. A worker is upgrade_failed
// exactly when it has a FRESH controller signal whose phase is `stuck` (R1 of the upgrade
// classifier, the only path to upgrade_failed — deliberately not version-dependent, so it
// needs no CPVersion). warn when some hosted workers are stuck, danger when every one is.
// `na` when hosted workers are not configured — HostedWorkerVersion=="" AND no kind='hosted'
// worker exists (the predicate confirmed against config.go:641). `unknown` when hosted
// workers exist but the NEWEST roll signal is stale (older than rollSignalTTL): a silent
// controller must not read green (D6). (M2 adds the "controller.report is not ok"
// conjunct; controller.report does not exist yet.)
func (s *Service) checkFleetRoll(now time.Time, workers []store.ListAllWorkersRow) apitypes.HealthCheckDTO {
	c := s.base("fleet.roll")

	hosted := make([]store.ListAllWorkersRow, 0, len(workers))
	for _, w := range workers {
		if w.Worker.Kind == "hosted" {
			hosted = append(hosted, w)
		}
	}

	// "Hosted workers are configured" — HostedWorkerVersion non-empty OR any hosted worker.
	configured := s.cfg.HostedWorkerVersion != "" || len(hosted) > 0
	if !configured {
		c.Severity = sevNA
		c.Summary = "No hosted workers are configured on this deployment."
		return c
	}
	if len(hosted) == 0 {
		// Configured (a pinned tag) but no hosted worker rows: nothing is rolling, nothing
		// is stuck, and we are not blind about any worker.
		c.Severity = sevOK
		c.Summary = "No hosted workers are present."
		return c
	}

	var (
		failed          int
		anyFreshSignal  bool
		stuckSince      time.Time
		haveStuckSince  bool
		blockingReason  string
		blockingCont    string
		targetTag       string
		firstStuckName  string
		haveStuckWorker bool
	)
	for _, w := range hosted {
		signalFresh := w.RollPhase.Valid && w.RollPhase.String != "" &&
			w.RollObservedAt.Valid && now.Sub(w.RollObservedAt.Time) <= rollSignalTTL
		if signalFresh {
			anyFreshSignal = true
		}
		if signalFresh && w.RollPhase.String == workersvc.PhaseStuck {
			failed++
			if !haveStuckWorker {
				haveStuckWorker = true
				firstStuckName = w.Worker.Name
				blockingReason = w.RollBlockingReason.String
				blockingCont = w.RollBlockingContainer.String
				targetTag = w.RollWorkerImageTag.String
			}
			// Oldest phase_since across the stuck workers is the best "since" the source
			// carries (for a stuck pod it is the pod creation time, not the transition).
			if w.RollPhaseSince.Valid && (!haveStuckSince || w.RollPhaseSince.Time.Before(stuckSince)) {
				stuckSince = w.RollPhaseSince.Time
				haveStuckSince = true
			}
		}
	}

	if !anyFreshSignal {
		c.Severity = sevUnknown
		c.Summary = "No fresh controller signal for the hosted fleet; roll health cannot be determined."
		c.Action = strPtr("Check that the worker controller is running and reporting.")
		return c
	}

	if failed == 0 {
		c.Severity = sevOK
		c.Summary = fmt.Sprintf("All %d hosted workers are rolling cleanly.", len(hosted))
		return c
	}

	tagLabel := "the pinned tag"
	if t := safe(targetTag); t != "" {
		tagLabel = t
	}
	reasonLabel := safe(blockingReason)
	if reasonLabel == "" {
		reasonLabel = "unknown reason"
	}
	c.Summary = fmt.Sprintf("%d of %d hosted workers stuck rolling to %s: %s", failed, len(hosted), tagLabel, reasonLabel)
	c.Evidence = []apitypes.HealthEvidenceDTO{
		{Label: "Target tag", Value: tagLabel},
		{Label: "Blocking container", Value: orDash(safe(blockingCont))},
		{Label: "Blocking reason", Value: reasonLabel},
		{Label: "Worker", Value: orDash(safe(firstStuckName))},
		{Label: "Stuck workers", Value: fmt.Sprintf("%d of %d", failed, len(hosted))},
	}
	c.Action = strPtr("Publish the image, or release a chart whose workers.image.tag names a published tag.")
	c.Command = strPtr("kubectl -n <worker-namespace> describe pod -l uzi.dev/hosted-worker-id=<id>")
	if haveStuckSince {
		c.Since = sincePtr(stuckSince)
	}
	if failed == len(hosted) {
		c.Severity = sevDanger
	} else {
		c.Severity = sevWarn
	}
	return c
}

// checkFleetCapacity is danger when, for at least fleetCapacityDanger, an owner has a
// waiting_worker run and zero usable (online, non-draining, fresh-heartbeat) workers.
// `unknown` when health_enabled is off — the sole writer of waiting_worker is gated by it,
// so the signal is absent, not green (D6) — AND `unknown` when the kill-switch read itself
// failed (we cannot tell whether the writer is running, so the run tables must not be
// queried for a green verdict). Below the threshold it is ok (a transient claim delay), and
// there is no warn band.
func (s *Service) checkFleetCapacity(ctx context.Context, now time.Time, health healthDetectorState) apitypes.HealthCheckDTO {
	c := s.base("fleet.capacity")
	switch health {
	case healthDetectorDisabled:
		return unknownHealthDisabled(c)
	case healthDetectorUnknown:
		return unknownHealthReadFailed(c)
	}
	rows, err := s.cfg.Store.ListOwnersWaitingNoCapacity(ctx, pgconv.Time(now.Add(-s.heartbeatStale())))
	if err != nil {
		return degradeUnknown(c, "fleet.capacity", err)
	}
	if len(rows) == 0 {
		c.Severity = sevOK
		c.Summary = "Every owner with queued work has a usable worker."
		return c
	}
	var oldest time.Time
	haveOldest := false
	for _, r := range rows {
		if r.OldestHealthSince.Valid && (!haveOldest || r.OldestHealthSince.Time.Before(oldest)) {
			oldest = r.OldestHealthSince.Time
			haveOldest = true
		}
	}
	if haveOldest && now.Sub(oldest) >= fleetCapacityDanger {
		c.Severity = sevDanger
		c.Summary = fmt.Sprintf("%d owner(s) have queued runs and no usable worker (oldest waiting %s).", len(rows), humanDur(now.Sub(oldest)))
		c.Since = sincePtr(oldest)
		c.Evidence = []apitypes.HealthEvidenceDTO{{Label: "Owners affected", Value: fmt.Sprintf("%d", len(rows))}}
		c.Action = strPtr("Recover or provision a worker for the affected owners, or check fleet.roll for stuck pods.")
		c.Command = strPtr("kubectl -n <worker-namespace> get pods")
		return c
	}
	c.Severity = sevOK
	c.Summary = "Owners waiting for a worker are within the transient window."
	return c
}

// checkFleetDisk warns when any worker (of any kind) with a fresh heartbeat has a debounced
// disk-pressure streak of at least diskPressureStreakWarn consecutive polls. There is no
// danger, unknown or na band for it (it reads live workers, no controller signal).
func (s *Service) checkFleetDisk(now time.Time, workers []store.ListAllWorkersRow) apitypes.HealthCheckDTO {
	c := s.base("fleet.disk")
	var affected int
	for _, w := range workers {
		fresh := w.Worker.LastHeartbeatAt.Valid && now.Sub(w.Worker.LastHeartbeatAt.Time) <= s.heartbeatStale()
		if fresh && w.Worker.StatsDiskPressureStreak >= diskPressureStreakWarn {
			affected++
		}
	}
	if affected == 0 {
		c.Severity = sevOK
		c.Summary = "No worker is under sustained disk pressure."
		return c
	}
	c.Severity = sevWarn
	c.Summary = fmt.Sprintf("%d worker(s) are under sustained disk pressure.", affected)
	c.Evidence = []apitypes.HealthEvidenceDTO{{Label: "Workers", Value: fmt.Sprintf("%d", affected)}}
	c.Action = strPtr("Free disk on the affected worker(s) or increase the worker volume size.")
	return c
}

// -------------------------------------------------------------------------
// queue group
// -------------------------------------------------------------------------

// checkQueueWaiting bands the oldest waiting_worker run by age: warn at queueWaitingWarn,
// danger at queueWaitingDanger. `unknown` when health_enabled is off (the writer is gated
// by it, D6), and `unknown` when the kill-switch read itself failed (the signal cannot be
// trusted to read green, so the run tables are not queried).
func (s *Service) checkQueueWaiting(ctx context.Context, now time.Time, health healthDetectorState) apitypes.HealthCheckDTO {
	c := s.base("queue.waiting")
	switch health {
	case healthDetectorDisabled:
		return unknownHealthDisabled(c)
	case healthDetectorUnknown:
		return unknownHealthReadFailed(c)
	}
	ts, err := s.cfg.Store.OldestWaitingWorkerRun(ctx)
	if err != nil {
		return degradeUnknown(c, "queue.waiting", err)
	}
	if !ts.Valid {
		c.Severity = sevOK
		c.Summary = "No run is waiting for a worker."
		return c
	}
	age := now.Sub(ts.Time)
	switch {
	case age >= queueWaitingDanger:
		c.Severity = sevDanger
		c.Summary = fmt.Sprintf("A run has been waiting for a worker for %s.", humanDur(age))
		c.Since = sincePtr(ts.Time)
		c.Action = strPtr("Check fleet.capacity and fleet.roll — a stuck roll or zero capacity is the usual cause.")
	case age >= queueWaitingWarn:
		c.Severity = sevWarn
		c.Summary = fmt.Sprintf("A run has been waiting for a worker for %s.", humanDur(age))
		c.Since = sincePtr(ts.Time)
		c.Action = strPtr("Check fleet.capacity and fleet.roll — a stuck roll or zero capacity is the usual cause.")
	default:
		c.Severity = sevOK
		c.Summary = "No run has been waiting for a worker longer than 10 minutes."
	}
	return c
}

// checkQueueUndispatched is danger when a task run has been queued with no dispatch for
// longer than queueUndispatchedDanger (the #1367 failure class). No warn / unknown / na.
func (s *Service) checkQueueUndispatched(ctx context.Context, now time.Time) apitypes.HealthCheckDTO {
	c := s.base("queue.undispatched")
	ts, err := s.cfg.Store.OldestUndispatchedTaskRun(ctx)
	if err != nil {
		return degradeUnknown(c, "queue.undispatched", err)
	}
	if !ts.Valid || now.Sub(ts.Time) < queueUndispatchedDanger {
		c.Severity = sevOK
		c.Summary = "No task run is stuck undispatched."
		return c
	}
	c.Severity = sevDanger
	c.Summary = fmt.Sprintf("A task run has been queued undispatched for %s.", humanDur(now.Sub(ts.Time)))
	c.Since = sincePtr(ts.Time)
	c.Action = strPtr("A queued task run was never dispatched (issue #1367); re-dispatch it or check the CLI push.")
	return c
}

// -------------------------------------------------------------------------
// control group
// -------------------------------------------------------------------------

// checkDB warns on a slow ping or a saturated pool and is danger on a failed ping or a
// migration-version mismatch. It reads the injected probe (the live pool in production, a
// fake in unit tests). A nil probe (no pool wired) is `unknown`.
func (s *Service) checkDB(ctx context.Context) apitypes.HealthCheckDTO {
	c := s.base("db")
	if s.probeDB == nil {
		c.Severity = sevUnknown
		c.Summary = "The database probe is not configured."
		return c
	}
	st := s.probeDB(ctx)
	switch {
	case st.pingErr != nil:
		c.Severity = sevDanger
		c.Summary = "Database ping failed."
		c.Action = strPtr("The database is unreachable; check the Postgres pod and connection.")
		return c
	case st.schemaErr != nil:
		c.Severity = sevDanger
		c.Summary = "Could not read the applied migration version."
		c.Action = strPtr("Check that migrations have been applied to this database.")
		return c
	case !st.schemaAtHead:
		c.Severity = sevDanger
		c.Summary = fmt.Sprintf("Applied migration version %d differs from the embedded head %d.", st.schemaApplied, st.schemaHead)
		c.Evidence = []apitypes.HealthEvidenceDTO{
			{Label: "Applied", Value: fmt.Sprintf("%d", st.schemaApplied)},
			{Label: "Head", Value: fmt.Sprintf("%d", st.schemaHead)},
		}
		c.Action = strPtr("Run the pending migrations, or roll back to the release whose embedded head matches.")
		return c
	}
	poolRatio := 0.0
	if st.maxConns > 0 {
		poolRatio = float64(st.acquiredConns) / float64(st.maxConns)
	}
	if st.pingDur > dbPingWarn {
		c.Severity = sevWarn
		c.Summary = fmt.Sprintf("Database ping is slow (%dms).", st.pingDur.Milliseconds())
		c.Action = strPtr("The database is slow to respond; check its load and the network path.")
		return c
	}
	if poolRatio >= dbPoolWarnRatio {
		c.Severity = sevWarn
		c.Summary = fmt.Sprintf("Connection pool is near capacity (%d of %d in use).", st.acquiredConns, st.maxConns)
		c.Action = strPtr("Connection demand is high; consider raising the pool size or reducing load.")
		return c
	}
	c.Severity = sevOK
	c.Summary = fmt.Sprintf("Database reachable (%dms); schema at head.", st.pingDur.Milliseconds())
	return c
}

// livePoolProbe is the production db probe: a real ping (timed with wall-clock time, not
// the injected clock), the pool stat, and the goose applied-vs-embedded-head comparison.
func (s *Service) livePoolProbe(ctx context.Context) dbStat {
	var st dbStat
	start := time.Now()
	st.pingErr = s.cfg.Pool.Ping(ctx)
	st.pingDur = time.Since(start)
	stat := s.cfg.Pool.Stat()
	st.acquiredConns = stat.AcquiredConns()
	st.maxConns = stat.MaxConns()
	st.schemaApplied, st.schemaHead, st.schemaAtHead, st.schemaErr = store.SchemaVersionStatus(ctx, s.cfg.Pool)
	return st
}

// -------------------------------------------------------------------------
// integrations group
// -------------------------------------------------------------------------

// checkSlackSocket is `na` when Slack is not configured (StateDisabled), ok when connected,
// and warn when configured but disconnected for at least slackDisconnectedWarn. The manager
// exposes only the current state, so the "first saw non-connected" timestamp is tracked in
// process memory here (mutex-guarded), set on the first non-connected evaluation and reset
// on connect. Below the threshold it is ok (a reconnect blip is not worth alarming).
func (s *Service) checkSlackSocket(now time.Time) apitypes.HealthCheckDTO {
	c := s.base("slack.socket")
	state := slacksvc.StateDisabled
	if s.cfg.SlackState != nil {
		state = s.cfg.SlackState()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	switch state {
	case slacksvc.StateDisabled:
		s.slackNonConnectedSince = nil
		c.Severity = sevNA
		c.Summary = "Slack is not configured."
		return c
	case slacksvc.StateConnected:
		s.slackNonConnectedSince = nil
		c.Severity = sevOK
		c.Summary = "Slack socket is connected."
		return c
	}

	if s.slackNonConnectedSince == nil {
		t := now
		s.slackNonConnectedSince = &t
	}
	disconnectedFor := now.Sub(*s.slackNonConnectedSince)
	if disconnectedFor >= slackDisconnectedWarn {
		c.Severity = sevWarn
		c.Summary = fmt.Sprintf("Slack socket has been disconnected for %s (state: %s).", humanDur(disconnectedFor), state)
		c.Since = sincePtr(*s.slackNonConnectedSince)
		c.Action = strPtr("Check the Slack app tokens and the socket connection.")
		return c
	}
	c.Severity = sevOK
	c.Summary = fmt.Sprintf("Slack socket is reconnecting (state: %s).", state)
	return c
}

// -------------------------------------------------------------------------
// housekeeping group
// -------------------------------------------------------------------------

// checkSchedulesPaused warns when at least one user has a pause-all in force AND owns at
// least one enabled schedule — the set whose labelled issues look queued forever.
func (s *Service) checkSchedulesPaused(ctx context.Context, now time.Time) apitypes.HealthCheckDTO {
	c := s.base("schedules.paused")
	n, err := s.cfg.Store.CountUsersPausedWithEnabledSchedules(ctx, pgconv.Time(now))
	if err != nil {
		return degradeUnknown(c, "schedules.paused", err)
	}
	if n == 0 {
		c.Severity = sevOK
		c.Summary = "No user has paused schedules while owning enabled ones."
		return c
	}
	c.Severity = sevWarn
	c.Summary = fmt.Sprintf("%d user(s) have paused all schedules while owning enabled schedules.", n)
	c.Evidence = []apitypes.HealthEvidenceDTO{{Label: "Users", Value: fmt.Sprintf("%d", n)}}
	c.Action = strPtr("Their labelled issues look queued but will not run until they unpause.")
	return c
}

// checkBoardDrift warns when at least one column move was given up (pending past the give-up
// boundary) within the last boardDriftWindow, on a run that HAS an issue (D12: issue_iid IS
// NOT NULL, filtered in Go so the issue-less false positives of #1482 never count).
func (s *Service) checkBoardDrift(ctx context.Context, now time.Time) apitypes.HealthCheckDTO {
	c := s.base("board.drift")
	rows, err := s.cfg.Store.ListGaveUpColumnMoves(ctx, store.ListGaveUpColumnMovesParams{
		GiveupCutoff: pgconv.Time(now.Add(-boardGiveUp)),
		PriorCutoff:  pgconv.Time(now.Add(-boardDriftWindow)),
	})
	if err != nil {
		return degradeUnknown(c, "board.drift", err)
	}
	var count int
	for _, r := range rows {
		if r.IssueIid.Valid {
			count++
		}
	}
	if count == 0 {
		c.Severity = sevOK
		c.Summary = "No column move has been given up in the last 24 hours."
		return c
	}
	c.Severity = sevWarn
	c.Summary = fmt.Sprintf("%d issue column move(s) were given up in the last 24 hours.", count)
	c.Evidence = []apitypes.HealthEvidenceDTO{{Label: "Given-up moves", Value: fmt.Sprintf("%d", count)}}
	c.Action = strPtr("An automation could not move an issue's card; move it by hand or investigate the board sync.")
	return c
}

// checkCustodyHolds warns when at least one owner is at the custody-hold admission limit.
// `na` when the limit is non-positive (custody admission disabled) — a defensive guard the
// caller's positive constant never trips in production.
func (s *Service) checkCustodyHolds(ctx context.Context) apitypes.HealthCheckDTO {
	c := s.base("custody.holds")
	if s.cfg.CustodyHoldLimit <= 0 {
		c.Severity = sevNA
		c.Summary = "Custody-hold admission is not configured."
		return c
	}
	owners, err := s.cfg.Store.ListOwnersOverCustodyLimit(ctx, s.cfg.CustodyHoldLimit)
	if err != nil {
		return degradeUnknown(c, "custody.holds", err)
	}
	if len(owners) == 0 {
		c.Severity = sevOK
		c.Summary = "No owner is at the custody-hold admission limit."
		return c
	}
	c.Severity = sevWarn
	c.Summary = fmt.Sprintf("%d owner(s) are at the custody-hold admission limit.", len(owners))
	c.Evidence = []apitypes.HealthEvidenceDTO{{Label: "Owners", Value: fmt.Sprintf("%d", len(owners))}}
	c.Action = strPtr("Their new runs are blocked until they discard or resolve held work (uzi run recovery).")
	return c
}

// checkReleaseCheck warns when the upstream release check reports far_behind. `na` when the
// release check is disabled; `unknown` on a settings read error (a missing signal must not
// read green, D6). Zero store involvement.
func (s *Service) checkReleaseCheck(ctx context.Context, now time.Time) apitypes.HealthCheckDTO {
	c := s.base("release.check")
	if s.cfg.Settings == nil {
		c.Severity = sevUnknown
		c.Summary = "Release-check settings are unavailable."
		return c
	}
	enabled, err := s.cfg.Settings.ReleaseCheckEnabled(ctx)
	if err != nil {
		return degradeUnknown(c, "release.check", err)
	}
	if !enabled {
		c.Severity = sevNA
		c.Summary = "The upstream release check is disabled."
		return c
	}
	st, err := s.cfg.Settings.ReleaseStatus(ctx)
	if err != nil {
		return degradeUnknown(c, "release.check", err)
	}
	if releasecheck.FarBehind(s.cfg.RunningVersion, st.LatestTag, st.PublishedAt, now) {
		c.Severity = sevWarn
		c.Summary = fmt.Sprintf("This instance is far behind the latest release (%s).", safe(st.LatestTag))
		c.Evidence = []apitypes.HealthEvidenceDTO{
			{Label: "Latest", Value: orDash(safe(st.LatestTag))},
			{Label: "Running", Value: orDash(safe(s.cfg.RunningVersion))},
		}
		c.Action = strPtr("Plan an upgrade to the latest release.")
		return c
	}
	c.Severity = sevOK
	c.Summary = "This instance is on a recent release."
	return c
}

// -------------------------------------------------------------------------
// shared helpers
// -------------------------------------------------------------------------

// unknownHealthDisabled is the shared `unknown` verdict for the two checks whose signal is
// written only when the run-health detector is enabled.
func unknownHealthDisabled(c apitypes.HealthCheckDTO) apitypes.HealthCheckDTO {
	c.Severity = sevUnknown
	c.Summary = "The run-health detector is disabled, so this signal is unavailable."
	return c
}

// unknownHealthReadFailed is the shared `unknown` verdict for those same two checks when the
// run-health kill switch could not be read: with the detector's state indeterminate we
// cannot tell whether the waiting_worker signal is being written, so the check must not
// query the run tables for a green verdict (D6). Its summary is deliberately distinct from
// the known-disabled one.
func unknownHealthReadFailed(c apitypes.HealthCheckDTO) apitypes.HealthCheckDTO {
	c.Severity = sevUnknown
	c.Summary = "Run-health detection state could not be determined."
	return c
}

// degradeUnknown turns a per-check query failure into `unknown` (never ok, per D6) and logs
// it, so one broken query cannot blank the whole page or read green.
func degradeUnknown(c apitypes.HealthCheckDTO, id string, err error) apitypes.HealthCheckDTO {
	slog.Warn("healthsvc: check query failed", "check", id, "error", err)
	c.Severity = sevUnknown
	c.Summary = "This signal is temporarily unavailable."
	return c
}

// orDash renders an empty value as an em-dash placeholder in evidence.
func orDash(v string) string {
	if v == "" {
		return "—"
	}
	return v
}
