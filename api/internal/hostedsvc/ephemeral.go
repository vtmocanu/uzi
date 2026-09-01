package hostedsvc

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// ephemeralProvisionBatch bounds how many unplaceable runs one ProvisionPass tick
// considers. It is a small cap so a large backlog is drained across several ticks
// rather than in one long burst of provision transactions — the sweep runs
// frequently, so eventual coverage is fine and the per-tick footprint stays bounded.
const ephemeralProvisionBatch int32 = 50

// The two trigger paths, emitted as the `trigger` field on each provision log line so a
// saturation burst is distinguishable from a capability-gap provision in logs (issue #747).
const (
	triggerCapabilityGap = "capability_gap"
	triggerSaturation    = "saturation"
)

// durationToInterval converts a Go duration to a pgtype.Interval for the saturation
// query's @saturation_delay param. Microseconds carries the whole duration (the debounce
// is well under a day), matching how Postgres compares now() - status_since to the interval.
func durationToInterval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}

// EphemeralSettings is the narrow settings dependency of the provisioner: the
// instance-wide kill-switch. *settings.Cache satisfies it via EphemeralWorkersEnabled.
type EphemeralSettings interface {
	EphemeralWorkersEnabled(ctx context.Context) (bool, error)
}

// EphemeralConfig carries the tuning knobs the provisioner needs (PRD #529 M2), lifted
// out of config.Config so hostedsvc does not depend on the config package.
type EphemeralConfig struct {
	// MaxPerUser is the per-user concurrent-ephemeral cap (UZI_EPHEMERAL_MAX_PER_USER).
	MaxPerUser int
	// DefaultSize is the workersize preset every ephemeral worker is provisioned at
	// (UZI_EPHEMERAL_DEFAULT_SIZE); validated at config load.
	DefaultSize string
	// ProvisionDeadline (UZI_EPHEMERAL_PROVISION_DEADLINE, default 10m) bounds how long a
	// freshly provisioned ephemeral worker may make no progress before ReapPass GCs it. The
	// SAME deadline drives BOTH orphan shapes the reaper times out — (b) never booted
	// (online_since still NULL past the deadline) and (c) idle-stolen (online past the
	// deadline but its bound run is being served by a sibling). There is deliberately no
	// separate idle-grace knob: one deadline keeps the config surface to the approved set.
	//
	// The saturation trigger path (issue #747) relies on this SAME (c) idle-stolen arm:
	// when a burst worker loses the race and a freed base slot claims its bound run, the
	// now-idle burst pod is reaped here. That race resolves in seconds while this grace is
	// 10m, so a lost-race burst pod (DinD, 20Gi PVC) can sit fully provisioned and idle for
	// up to the deadline before GC. This cost is ACCEPTED (issue #747 M3): the debounce
	// lowers the race probability, and a shorter idle-grace for the saturation arm would
	// require a second knob, which collides with the "one deadline" decision above — so the
	// saturation path deliberately shares the 10m grace rather than adding config surface.
	ProvisionDeadline time.Duration
	// SaturationDelay is the queue-wait debounce for the saturation trigger path
	// (UZI_EPHEMERAL_SATURATION_DELAY, default 90s). It is threaded to
	// ListSaturationQueuedRunsForEphemeral as its @saturation_delay interval param, so
	// the debounce is evaluated in-query against runs.status_since rather than off the
	// provisioner's private clock. A run capability-placeable but slot-blocked provisions
	// a burst worker only once queued longer than this — roughly worker cold-start — so a
	// freeing slot claims it first and a transient claim-cycle queue does not churn pods.
	SaturationDelay time.Duration
}

// EphemeralProvisioner is the background pass that auto-provisions run-bound ephemeral
// hosted workers for unplaceable queued runs (PRD #529 M2). It mirrors the persistent
// provision path (handler.provisionHostedWorker): a per-user advisory lock, a cap check,
// CreateEphemeralHostedWorker + SealJoinToken in one transaction. It never exposes the
// plaintext join token outside that transaction — the token's whole lifetime is the tx
// body, exactly as in the persistent path.
type EphemeralProvisioner struct {
	pool     *pgxpool.Pool
	q        *store.Queries
	box      *secretbox.Box
	settings EphemeralSettings
	cfg      EphemeralConfig
	// now is the clock, defaulted to time.Now and overridable in tests for a deterministic
	// reaper cutoff (follows the workersvc.Service pattern).
	now func() time.Time
}

// NewEphemeralProvisioner wires the provisioner. pool is needed for Begin + the
// advisory lock; q is the store used both for the trigger query and, via WithTx, inside
// each provision transaction; box seals the join token; settings supplies the instance
// kill-switch; cfg carries the cap and default size.
func NewEphemeralProvisioner(pool *pgxpool.Pool, q *store.Queries, box *secretbox.Box, s EphemeralSettings, cfg EphemeralConfig) *EphemeralProvisioner {
	return &EphemeralProvisioner{pool: pool, q: q, box: box, settings: s, cfg: cfg, now: time.Now}
}

// ProvisionPass is one tick of the auto-provisioner, wired as a sweeper.Pass. It
// returns the number of ephemeral workers actually created this tick.
//
// Flag-off footprint is exactly ONE settings read: with the instance kill-switch off it
// returns (0, nil) before touching the database. When on, it unions the capability-gap and
// saturation trigger sets for opted-in users and, for each, provisions one run-bound
// ephemeral worker under the per-user cap. A hard error on one run is logged and does not
// abort the whole pass — the sibling sweeper passes have the same resilience — so one bad
// run cannot starve the rest of the backlog.
func (p *EphemeralProvisioner) ProvisionPass(ctx context.Context) (int64, error) {
	enabled, err := p.settings.EphemeralWorkersEnabled(ctx)
	if err != nil {
		return 0, fmt.Errorf("hostedsvc: read ephemeral kill-switch: %w", err)
	}
	if !enabled {
		return 0, nil
	}

	// @max_per_user is the cross-user FAIRNESS filter (see the query comment): it excludes
	// runs whose owner is already at/over the per-user ephemeral cap so one at-cap user with
	// a large backlog cannot monopolize every batch. It is an unlocked snapshot, so it is
	// only an optimization — provisionOne's advisory-locked count remains the authoritative
	// cap and is what actually prevents over-provisioning.
	//
	// Two trigger paths feed one provision loop (issue #747). Capability-gap: a run
	// nothing online can satisfy — provision immediately (permanent gap). Saturation: a
	// run some worker COULD claim but every capable worker is at its cap — provision only
	// after the queue-wait debounce (transient; a freeing slot may claim it first). Both
	// query LIMITs are ephemeralProvisionBatch, but the UNION could hold up to 2× that, so
	// we dedup by run id and re-apply the per-tick LIMIT to the combined set — a run cannot
	// legitimately be in both sets, but the guard keeps one run from consuming two slots.
	gapRuns, err := p.q.ListUnplaceableQueuedRunsForEphemeral(ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{
		MaxRows:    ephemeralProvisionBatch,
		MaxPerUser: int32(p.cfg.MaxPerUser), //nolint:gosec // small configured cap, never near int32 range
	})
	if err != nil {
		return 0, fmt.Errorf("hostedsvc: list unplaceable queued runs: %w", err)
	}
	satRuns, err := p.q.ListSaturationQueuedRunsForEphemeral(ctx, store.ListSaturationQueuedRunsForEphemeralParams{
		SaturationDelay: durationToInterval(p.cfg.SaturationDelay),
		MaxRows:         ephemeralProvisionBatch,
		MaxPerUser:      int32(p.cfg.MaxPerUser), //nolint:gosec // small configured cap, never near int32 range
	})
	if err != nil {
		return 0, fmt.Errorf("hostedsvc: list saturation queued runs: %w", err)
	}

	// Capability-gap first: it is permanent starvation (nothing can ever serve the run),
	// so under a full batch it takes priority over saturation runs, which are transient and
	// may still be claimed by a freeing slot.
	type ephemeralCandidate struct {
		id      uuid.UUID
		userID  uuid.UUID
		caps    []string
		trigger string
	}
	candidates := make([]ephemeralCandidate, 0, len(gapRuns)+len(satRuns))
	seen := make(map[uuid.UUID]struct{}, len(gapRuns)+len(satRuns))
	for _, run := range gapRuns {
		seen[run.ID] = struct{}{}
		candidates = append(candidates, ephemeralCandidate{id: run.ID, userID: run.UserID, caps: run.RequiredCapabilities, trigger: triggerCapabilityGap})
	}
	for _, run := range satRuns {
		if _, dup := seen[run.ID]; dup {
			continue
		}
		seen[run.ID] = struct{}{}
		candidates = append(candidates, ephemeralCandidate{id: run.ID, userID: run.UserID, caps: run.RequiredCapabilities, trigger: triggerSaturation})
	}
	if len(candidates) > int(ephemeralProvisionBatch) {
		candidates = candidates[:ephemeralProvisionBatch]
	}

	var created int64
	for _, c := range candidates {
		template, docker, rerr := capability.ResolveEphemeralSpec(c.caps)
		if rerr != nil {
			slog.Warn("ephemeral provisioner: run has unprovisionable capabilities; skipping",
				"run_id", c.id, "trigger", c.trigger, "required_capabilities", c.caps, "error", rerr)
			continue
		}
		ok, perr := p.provisionOne(ctx, c.userID, c.id, template, docker)
		if perr != nil {
			slog.Error("ephemeral provisioner: provision failed; continuing with the rest of the pass",
				"run_id", c.id, "trigger", c.trigger, "error", perr)
			continue
		}
		if ok {
			created++
			slog.Info("ephemeral provisioner: provisioned run-bound worker",
				"run_id", c.id, "trigger", c.trigger, "template", template, "docker", docker)
		}
	}
	return created, nil
}

// ReapPass is the orphan/failure GC backstop (PRD #529 M5), wired as a sweeper.Pass.
// UNCONDITIONAL — deliberately NOT gated on the kill-switch (mirrors ExpirePendingTokens):
// a stack that provisioned ephemeral workers and then turned the feature off is exactly
// the one whose orphans would otherwise never be reaped. Its flag-off/no-ephemeral
// footprint is one indexed DELETE that matches nothing.
func (p *EphemeralProvisioner) ReapPass(ctx context.Context) (int64, error) {
	cutoff := p.now().Add(-p.cfg.ProvisionDeadline)
	return p.q.ReapEphemeralWorkers(ctx, pgTime(cutoff))
}

// pgTime wraps a known-present time as a valid pgtype.Timestamptz (mirrors the helper in
// workersvc/usagepoller so ReapPass's cutoff is assignable to the generated param).
func pgTime(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// provisionOne runs the provision transaction for a single run, mirroring
// handler.provisionHostedWorker. It returns (true, nil) when a worker was created,
// (false, nil) when the provision was correctly skipped (over the cap, or the run was
// already served — a 23505 from the partial unique index), and (false, err) on a real
// failure. The lock → count → create → seal → commit ordering is load-bearing: the
// advisory lock is taken FIRST, before the cap count it protects, so ephemeral and
// persistent provisions for one user serialize (same lock class + key) and the cap
// count is never a decorative TOCTOU.
func (p *EphemeralProvisioner) provisionOne(ctx context.Context, userID, runID uuid.UUID, template string, docker bool) (bool, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit

	qtx := p.q.WithTx(tx)

	// FIRST, before the count it protects — the same shape and the SAME lock class + key
	// as provisionHostedWorker, so a persistent provision and an ephemeral one for one
	// user cannot run their counts concurrently.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)",
		store.HostedProvisionLockClass, ephemeralProvisionLockObjID(userID)); err != nil {
		return false, err
	}

	n, err := qtx.CountEphemeralHostedWorkersForUser(ctx, userID)
	if err != nil {
		return false, err
	}
	if n >= int64(p.cfg.MaxPerUser) {
		// Over the concurrent-ephemeral cap: refuse before minting anything, so there is
		// no token to seal and nothing to roll back but the lock. Not an error — the cap
		// working as intended.
		return false, nil
	}

	token, hash, err := jointoken.Generate()
	if err != nil {
		return false, err
	}

	// Bind mode (issue #804): default an auto-provisioned burst worker to `auto` ONLY when
	// the owner has ≥1 auto_eligible anthropic_token, so the auto-select pool is non-empty
	// and the worker never parks its run in pool_wait (autoselect.ReasonPoolEmpty); a
	// pre-existing multi-token owner who never opted in gets `default`. Read via qtx for a
	// snapshot consistent with the rest of the tx — holding the provision lock over this
	// read buys no atomicity (the auto_eligible toggle runs under a different lock class),
	// it just keeps the read in one transaction.
	hasPool, err := qtx.UserHasAutoEligibleAnthropicToken(ctx, userID)
	if err != nil {
		return false, err
	}
	mode := workersvc.BindModeDefault
	if hasPool {
		mode = workersvc.BindModeAuto
	}

	wkr, err := qtx.CreateEphemeralHostedWorker(ctx, store.CreateEphemeralHostedWorkerParams{
		UserID:           userID,
		Name:             ephemeralWorkerName(runID),
		TokenHash:        hash,
		TemplateDeclared: pgtype.Text{String: template, Valid: true},
		HostedSize:       pgtype.Text{String: p.cfg.DefaultSize, Valid: true},
		// Explicit true/false (Valid always), never NULL: on a hosted row a false is a
		// real "no sidecar", matching CreateHostedWorker.
		DockerEnabled:     pgtype.Bool{Bool: docker, Valid: true},
		EphemeralRunID:    runID,
		AnthropicBindMode: mode,
	})
	if err != nil {
		if isUniqueViolation(err) {
			// The partial unique index uq_workers_ephemeral_run rejected the duplicate:
			// another provision (this replica or another) already bound a worker to this
			// run between the trigger query and now. Already provisioned — skip, do not
			// count as an error. The tx is aborted; the deferred Rollback unwinds it.
			return false, nil
		}
		return false, err
	}

	// The co-write, in THIS transaction: seal the join token so the worker can never
	// exist with a token_hash whose plaintext was never queued (identical to the
	// persistent path). The plaintext lives only for this call.
	if err := SealJoinToken(ctx, qtx, p.box, wkr.ID, token); err != nil {
		return false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// ephemeralWorkerName is the display name for an auto-provisioned worker. The bound run
// id makes it self-identifying in the worker list and unique per run (the one-per-run
// unique index guarantees at most one live ephemeral worker per run).
func ephemeralWorkerName(runID uuid.UUID) string {
	return "ephemeral-" + runID.String()
}

// ephemeralProvisionLockObjID derives the objid half of the per-user advisory lock from
// a user's uuid, IDENTICALLY to handler.hostedProvisionLockObjID so ephemeral and
// persistent provisions for one user take the SAME lock and serialize. A uuid's leading
// bytes are random, so two users can collide here; the consequence is that two unrelated
// provisions serialize for a moment, which costs latency and never correctness.
func ephemeralProvisionLockObjID(userID uuid.UUID) int32 {
	return int32(binary.BigEndian.Uint32(userID[:4])) //nolint:gosec // wraparound is fine: this is a lock key, not a number
}

// isUniqueViolation reports whether err is a Postgres unique-constraint failure
// (SQLSTATE 23505) — here, the partial index uq_workers_ephemeral_run rejecting a
// second ephemeral worker for a run. Mirrors handler.isUniqueViolation.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
