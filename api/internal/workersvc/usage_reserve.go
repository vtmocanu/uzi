package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/usagereserve"
)

// Weekly usage reserve admission (PRD #3522 M3). One Go source of truth for "which credential would
// this run spend, and does the owner's reserve block it":
//
//   - reserveSnapshot reads the owner's threshold and, only when one is set, their fresh 7-day
//     readings (usagereserve.EvaluateClaude / EvaluateCodex), and derives the verdict of every
//     enabled Anthropic token and every enabled Codex alias.
//   - reserveEnforced is the single exemption rule (no threshold, chat, an active manual-resume
//     override, a started run while the pause toggle is off).
//   - reserveHeld resolves the credential a run would spend in claimSecretID's order and reports
//     whether its verdict blocks it.
//
// ClaimRun's reserve clause (store/queries/runtime.sql, PRD #3522 M3) mirrors reserveEnforced and
// reserveHeld in SQL from the same snapshot, so a held run is never claimed; assembly re-applies the
// same rules through the reserveGate carried in the context, so the credential a claim finally
// spends is also checked (a pick that fell back, a reading that moved after admission).

// errReserveHeld marks a claim whose resolved credential the owner's weekly usage reserve blocks.
// It is TRANSIENT, like errVaultLocked: finishRunClaim requeues the run (claimed -> queued, started_at
// kept, no usage-park counter touched) with the visible reason persisted, and the next claim pass
// admits it once the reading allows.
var errReserveHeld = errors.New("weekly usage reserve hold")

// reserveHeldError is an errReserveHeld carrying the visible reason (usagereserve.HeldReason or
// UnknownReason) the requeue persists.
type reserveHeldError struct{ reason string }

func (e *reserveHeldError) Error() string        { return errReserveHeld.Error() + ": " + e.reason }
func (e *reserveHeldError) Is(target error) bool { return target == errReserveHeld }

// reserveHeldReasonOf extracts the reason of a held error, "" when err carries none.
func reserveHeldReasonOf(err error) string {
	var h *reserveHeldError
	if errors.As(err, &h) {
		return h.reason
	}
	return ""
}

// reserveQueries is the optional store surface the reserve reads and the marker pass writes.
// *store.Queries satisfies it; a fake Store that embeds the Store interface does not, and the
// feature then reads as OFF, so no existing fake needs the new methods.
type reserveQueries interface {
	GetUserUsageReserve(ctx context.Context, id uuid.UUID) (store.GetUserUsageReserveRow, error)
	GetUserJudgeAnthropicBinding(ctx context.Context, id uuid.UUID) (store.GetUserJudgeAnthropicBindingRow, error)
	ListReserveClaudeTokens(ctx context.Context, userID uuid.UUID) ([]store.ListReserveClaudeTokensRow, error)
	ListReserveCodexAliases(ctx context.Context, userID uuid.UUID) ([]store.ListReserveCodexAliasesRow, error)
}

// reserveSnap is one owner's reserve state at one instant: the settings and the verdict of every
// credential they could spend. A snapshot with a nil threshold is OFF and carries nothing else.
type reserveSnap struct {
	userID       uuid.UUID
	threshold    *int16
	pauseRunning bool

	claude map[uuid.UUID]usagereserve.Verdict // every enabled Anthropic token
	codex  map[uuid.UUID]usagereserve.Verdict // every enabled, metered Codex alias
	// pooled is the auto-eligible token ids in id order; defaultID is the owner's default token
	// (uuid.Nil when none); judgeMode/judgeSecret is the owner's judge-lane binding.
	pooled      []uuid.UUID
	defaultID   uuid.UUID
	judgeMode   string
	judgeSecret *uuid.UUID
}

func (r *reserveSnap) on() bool { return r != nil && r.threshold != nil }

// reserveIntervals are the usage poll cadences the freshness rule is measured in.
type reserveIntervals struct{ claude, codex time.Duration }

func (s *Service) reserveIntervals() reserveIntervals {
	return reserveIntervals{claude: s.p.UsagePollInterval, codex: s.p.CodexUsagePollInterval}
}

// ReserveIntervals is the exported form hostedsvc's ephemeral recheck passes (it builds its own
// config from the same server settings).
type ReserveIntervals struct{ Claude, Codex time.Duration }

// reserveSnapshot reads the owner's reserve state. With no threshold it returns an OFF snapshot
// after ONE read (the settings row); only a set threshold reads the tokens, the Codex aliases and
// the judge binding. A service whose store cannot serve the reserve reads (a test fake) reads as OFF.
func (s *Service) reserveSnapshot(ctx context.Context, userID uuid.UUID, now time.Time) (*reserveSnap, error) {
	q, ok := s.q.(reserveQueries)
	if !ok {
		return &reserveSnap{userID: userID}, nil
	}
	return buildReserveSnapshot(ctx, q, userID, now, s.reserveIntervals())
}

func buildReserveSnapshot(ctx context.Context, q reserveQueries, userID uuid.UUID, now time.Time, iv reserveIntervals) (*reserveSnap, error) {
	set, err := q.GetUserUsageReserve(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("weekly reserve settings: %w", err)
	}
	snap := &reserveSnap{userID: userID}
	if !set.WeeklyReservePct.Valid {
		return snap, nil
	}
	thr := set.WeeklyReservePct.Int16
	snap.threshold = &thr
	snap.pauseRunning = set.WeeklyReservePauseRunning
	snap.claude = map[uuid.UUID]usagereserve.Verdict{}
	snap.codex = map[uuid.UUID]usagereserve.Verdict{}

	tokens, err := q.ListReserveClaudeTokens(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("weekly reserve token readings: %w", err)
	}
	for _, t := range tokens {
		r := usagereserve.ClaudeReading{Present: t.HasReading}
		if t.SevenDayPct.Valid {
			v := t.SevenDayPct.Int16
			r.SevenDayPct = &v
		}
		if t.SevenDayResetsAt.Valid {
			v := t.SevenDayResetsAt.Time
			r.SevenDayResetsAt = &v
		}
		if t.SyncedAt.Valid {
			v := t.SyncedAt.Time
			r.SyncedAt = &v
		}
		snap.claude[t.UserSecretID] = usagereserve.EvaluateClaude(snap.threshold, r, iv.claude, now)
		if t.AutoEligible {
			snap.pooled = append(snap.pooled, t.UserSecretID)
		}
		if t.IsDefault {
			snap.defaultID = t.UserSecretID
		}
	}

	aliases, err := q.ListReserveCodexAliases(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("weekly reserve codex readings: %w", err)
	}
	for _, a := range aliases {
		if a.Unmetered {
			snap.codex[a.AliasID] = usagereserve.Exempt()
			continue
		}
		r := usagereserve.CodexReading{Present: a.HasReading}
		if a.LastSuccessAt.Valid {
			v := a.LastSuccessAt.Time
			r.LastSuccessAt = &v
		}
		if len(a.Buckets) > 0 {
			var buckets []apitypes.CodexRateLimitBucketDTO
			if jerr := json.Unmarshal(a.Buckets, &buckets); jerr != nil {
				// A snapshot our own poller wrote that no longer decodes proves nothing: unknown.
				r.Present = false
			} else {
				r.Buckets = buckets
			}
		}
		snap.codex[a.AliasID] = usagereserve.EvaluateCodex(snap.threshold, r, iv.codex, now)
	}

	bound, err := q.GetUserJudgeAnthropicBinding(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("weekly reserve judge binding: %w", err)
	}
	snap.judgeMode = bound.JudgeAnthropicBindMode
	if bound.JudgeAnthropicSecretID.Valid {
		id := uuid.UUID(bound.JudgeAnthropicSecretID.Bytes)
		snap.judgeSecret = &id
	}
	return snap, nil
}

// --- context plumbing --------------------------------------------------------------------------

type reserveSnapKey struct{}

// withReserveSnapshot carries the snapshot claimLane computed for the admission through to the
// claim's assembly, so one claim reads the owner's reserve once.
func withReserveSnapshot(ctx context.Context, snap *reserveSnap) context.Context {
	return context.WithValue(ctx, reserveSnapKey{}, snap)
}

func reserveSnapshotFrom(ctx context.Context, userID uuid.UUID) *reserveSnap {
	if snap, ok := ctx.Value(reserveSnapKey{}).(*reserveSnap); ok && snap != nil && snap.userID == userID {
		return snap
	}
	return nil
}

// reserveGate is the reserve applied to ONE enforced run's assembly. A nil gate means "not
// enforced": every method is nil-safe and returns today's behaviour.
type reserveGate struct {
	snap *reserveSnap
	now  time.Time
}

type reserveGateKey struct{}

func reserveGateFrom(ctx context.Context) *reserveGate {
	g, _ := ctx.Value(reserveGateKey{}).(*reserveGate)
	return g
}

// reserveGateFor builds the gate for a run's assembly: nil when the reserve does not apply to it.
// It reuses the claim's snapshot when the context carries one and otherwise reads a fresh one (the
// direct-assembly callers and the sweeper's tests).
func (s *Service) reserveGateFor(ctx context.Context, run store.Run) (*reserveGate, error) {
	// s.now is unset on a Service built as a bare literal (several direct-assembly tests do);
	// the reserve must not be the thing that panics them.
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	snap := reserveSnapshotFrom(ctx, run.UserID)
	if snap == nil {
		var err error
		if snap, err = s.reserveSnapshot(ctx, run.UserID, now); err != nil {
			return nil, err
		}
	}
	if !reserveEnforced(run, snap, now) {
		return nil, nil
	}
	return &reserveGate{snap: snap, now: now}, nil
}

// attach returns ctx carrying the gate (ctx unchanged for a nil gate).
func (g *reserveGate) attach(ctx context.Context) context.Context {
	if g == nil {
		return ctx
	}
	return context.WithValue(ctx, reserveGateKey{}, g)
}

// checkChoice refuses a resolved credential choice the reserve blocks. A nil secretID resolves the
// owner's default token, so it is checked as that.
func (g *reserveGate) checkChoice(c secretChoice) error {
	if g == nil {
		return nil
	}
	if c.secretID == nil {
		if g.snap.defaultBlocked() {
			return &reserveHeldError{reason: g.snap.reasonFor(g.snap.defaultID)}
		}
		return nil
	}
	if g.snap.claudeBlocked(*c.secretID) {
		return &reserveHeldError{reason: g.snap.reasonFor(*c.secretID)}
	}
	return nil
}

// checkCodex refuses a Codex run whose frozen alias the reserve blocks. Called before anything is
// minted or opened.
func (g *reserveGate) checkCodex(run store.Run) error {
	if g == nil || !run.CodexSecretID.Valid {
		return nil
	}
	id := uuid.UUID(run.CodexSecretID.Bytes)
	if g.snap.codexBlocked(id) {
		return &reserveHeldError{reason: g.snap.codex[id].Reason}
	}
	return nil
}

// dropBlocked removes the candidates the reserve blocks.
func (g *reserveGate) dropBlocked(cands []autoselect.Candidate) []autoselect.Candidate {
	out := make([]autoselect.Candidate, 0, len(cands))
	for _, c := range cands {
		if !g.snap.claudeBlocked(c.SecretID) {
			out = append(out, c)
		}
	}
	return out
}

// autoHeldErr is the error for an auto lane whose pooled tokens (all but the run's active
// dead-credential exclusion) are blocked.
func (g *reserveGate) autoHeldErr(exclude uuid.UUID) error {
	return &reserveHeldError{reason: g.snap.autoReason(exclude)}
}

// --- verdict helpers ---------------------------------------------------------------------------

func (r *reserveSnap) claudeBlocked(id uuid.UUID) bool {
	v, ok := r.claude[id]
	return ok && v.State != usagereserve.OK
}

func (r *reserveSnap) codexBlocked(id uuid.UUID) bool {
	v, ok := r.codex[id]
	return ok && v.State != usagereserve.OK
}

func (r *reserveSnap) defaultBlocked() bool {
	return r.defaultID != uuid.Nil && r.claudeBlocked(r.defaultID)
}

// reasonFor is the visible reason of a blocked credential.
func (r *reserveSnap) reasonFor(id uuid.UUID) string {
	if v, ok := r.claude[id]; ok && v.Reason != "" {
		return v.Reason
	}
	return usagereserve.UnknownReason
}

// autoLane reports, for an auto pool with the run's exclusion applied, whether any pooled token
// exists and whether any of them is permitted. The auto lane is held exactly when pooled && !permitted.
func (r *reserveSnap) autoLane(exclude uuid.UUID) (pooled, permitted bool) {
	for _, id := range r.pooled {
		if id == exclude {
			continue
		}
		pooled = true
		if !r.claudeBlocked(id) {
			return true, true
		}
	}
	return pooled, false
}

// autoReason is the reason shown for a blocked auto lane: the highest held reading among the
// blocked pooled tokens, else the unknown reason.
func (r *reserveSnap) autoReason(exclude uuid.UUID) string {
	best, bestPct := "", -1
	for _, id := range r.pooled {
		if id == exclude {
			continue
		}
		v := r.claude[id]
		if v.State == usagereserve.Held && v.Pct > bestPct {
			best, bestPct = v.Reason, v.Pct
		}
	}
	if best != "" {
		return best
	}
	return usagereserve.UnknownReason
}

func (r *reserveSnap) permittedIDs() []uuid.UUID {
	out := []uuid.UUID{}
	for _, id := range r.pooled {
		if !r.claudeBlocked(id) {
			out = append(out, id)
		}
	}
	return out
}

func (r *reserveSnap) pooledIDs() []uuid.UUID {
	return append([]uuid.UUID{}, r.pooled...)
}

func sortedBlocked(m map[uuid.UUID]usagereserve.Verdict) []uuid.UUID {
	out := []uuid.UUID{}
	for id, v := range m {
		if v.State != usagereserve.OK {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// --- exemptions and the held decision ----------------------------------------------------------

// reserveEnforced is the single exemption rule: the reserve applies to a run only when the owner has a
// threshold, the run is not chat (the interactive use the reserve protects), no manual-resume
// override is active, and the run is either never-started or the owner's pause toggle is on (a
// previously started run's resume claim is gated only while pausing is enabled). ClaimRun's reserve
// clause mirrors these arms in SQL.
func reserveEnforced(run store.Run, snap *reserveSnap, now time.Time) bool {
	if !snap.on() || run.Kind == runkind.Chat {
		return false
	}
	if overrideActive(run, now) {
		return false
	}
	if run.FirstStartedAt.Valid && !snap.pauseRunning {
		return false
	}
	return true
}

// overrideActive reports a manual-resume override in force. It is the one seam the observed-reset
// early end plugs into: ClaimRun's @reserve_override_ended list is the SQL side of the same rule.
func overrideActive(run store.Run, now time.Time) bool {
	return run.ReserveExemptUntil.Valid && run.ReserveExemptUntil.Time.After(now)
}

// reserveLane is the Anthropic lane a worker's claim resolves a worker-lane run onto.
type reserveLane struct {
	mode     string
	secretID *uuid.UUID
}

func laneOfWorker(w store.Worker) reserveLane {
	return reserveLane{mode: w.AnthropicBindMode, secretID: workerSecretID(w)}
}

// laneEphemeral is the lane the ephemeral provisioner binds a burst worker to: auto when the owner
// has a pooled token, else the default (hostedsvc/ephemeral.go, provisionOne).
func laneEphemeral(hasPool bool) reserveLane {
	if hasPool {
		return reserveLane{mode: BindModeAuto}
	}
	return reserveLane{mode: BindModeDefault}
}

// reserveDecision is a held verdict with its visible reason.
type reserveDecision struct {
	held   bool
	reason string
}

func decisionOf(v usagereserve.Verdict) reserveDecision {
	if v.State == usagereserve.OK {
		return reserveDecision{}
	}
	return reserveDecision{held: true, reason: v.Reason}
}

func (r *reserveSnap) tokenDecision(id uuid.UUID) reserveDecision {
	if r.claudeBlocked(id) {
		return reserveDecision{held: true, reason: r.reasonFor(id)}
	}
	return reserveDecision{}
}

func (r *reserveSnap) defaultDecision() reserveDecision {
	if r.defaultBlocked() {
		return reserveDecision{held: true, reason: r.reasonFor(r.defaultID)}
	}
	return reserveDecision{}
}

func (r *reserveSnap) autoDecision(exclude uuid.UUID) reserveDecision {
	if pooled, permitted := r.autoLane(exclude); pooled && !permitted {
		return reserveDecision{held: true, reason: r.autoReason(exclude)}
	}
	return reserveDecision{}
}

func (r *reserveSnap) staticDecision(l reserveLane) reserveDecision {
	if l.mode == BindModePinned && l.secretID != nil {
		return r.tokenDecision(*l.secretID)
	}
	return r.defaultDecision()
}

func (r *reserveSnap) judgeDecision(exclude uuid.UUID) reserveDecision {
	switch r.judgeMode {
	case BindModeAuto:
		pooled, permitted := r.autoLane(exclude)
		switch {
		case permitted:
			return reserveDecision{}
		case pooled:
			return reserveDecision{held: true, reason: r.autoReason(exclude)}
		}
		// judgeChoice's empty-pool fallback: the owner's default.
		return r.defaultDecision()
	case BindModePinned:
		if r.judgeSecret != nil {
			return r.tokenDecision(*r.judgeSecret)
		}
	}
	return r.defaultDecision()
}

// reserveExclude is claimExcludeFor evaluated at an explicit instant: the credential an auto resume
// must not re-pick (the run's dead credential while retry_not_before is still ahead), uuid.Nil
// otherwise. ClaimRun's SQL arms spell the same condition.
func reserveExclude(run store.Run, now time.Time) uuid.UUID {
	if run.LimitDeadSecretID.Valid && run.RetryNotBefore.Valid && run.RetryNotBefore.Time.After(now) {
		return uuid.UUID(run.LimitDeadSecretID.Bytes)
	}
	return uuid.Nil
}

// reserveHeld reports whether the reserve holds the run on the given worker lane: the run must be
// enforced, and the credential it would spend, resolved in claimSecretID's order, must be blocked.
//
//  1. a Codex run spends its frozen alias (a NULL alias or an API-key alias is never held);
//  2. judge and self_improve spend the owner's judge-lane binding;
//  3. a per-run override: pinned (a live id) / default / auto; a pinned override whose id was nulled
//     falls through;
//  4. otherwise the claiming worker's lane (auto, pinned, default). A Claude cross_check checker
//     carries no override and lands here.
func reserveHeld(run store.Run, snap *reserveSnap, lane reserveLane, now time.Time) reserveDecision {
	if !reserveEnforced(run, snap, now) {
		return reserveDecision{}
	}
	exclude := reserveExclude(run, now)
	if run.Harness == harnessCodex {
		if !run.CodexSecretID.Valid {
			return reserveDecision{}
		}
		if v, ok := snap.codex[uuid.UUID(run.CodexSecretID.Bytes)]; ok {
			return decisionOf(v)
		}
		return reserveDecision{}
	}
	if run.Kind == runkind.SelfImprove || run.Kind == runkind.Judge {
		return snap.judgeDecision(exclude)
	}
	if run.CredentialOverrideMode.Valid {
		switch run.CredentialOverrideMode.String {
		case BindModePinned:
			if run.CredentialOverrideSecretID.Valid {
				return snap.tokenDecision(uuid.UUID(run.CredentialOverrideSecretID.Bytes))
			}
		case BindModeDefault:
			return snap.defaultDecision()
		case BindModeAuto:
			return snap.autoDecision(exclude)
		}
	}
	if lane.mode == BindModeAuto {
		return snap.autoDecision(exclude)
	}
	return snap.staticDecision(lane)
}

// laneBlocked reports whether a worker lane is blocked for a worker-lane run with no
// dead-credential exclusion (the spread-peer approximation).
func (r *reserveSnap) laneBlocked(l reserveLane) bool {
	if l.mode == BindModeAuto {
		return r.autoDecision(uuid.Nil).held
	}
	return r.staticDecision(l).held
}

// reserveClaimParams fills ClaimRun's reserve parameters from a snapshot and the claiming
// worker's lane. blockedWorkers is the owner's workers (other than the claimant's own view) whose
// lane is blocked, for the spread-peer clause; empty when the reserve is off. Every array is
// non-nil: `= ANY(NULL)` is NULL, not false.
func reserveClaimParams(p *store.ClaimRunParams, snap *reserveSnap, wkr store.Worker, blockedWorkers []uuid.UUID, now time.Time) {
	p.ReserveNow = pgconv.Time(now)
	p.ReserveOverrideEnded = []uuid.UUID{}
	p.ReserveBlockedCodex = []uuid.UUID{}
	p.ReservePermitted = []uuid.UUID{}
	p.ReservePooled = []uuid.UUID{}
	p.ReserveBlockedClaude = []uuid.UUID{}
	p.ReserveBlockedWorkerIds = []uuid.UUID{}
	if !snap.on() {
		return
	}
	p.ReserveOn = true
	p.ReservePauseOn = snap.pauseRunning
	p.ReserveBlockedCodex = sortedBlocked(snap.codex)
	p.ReserveBlockedClaude = sortedBlocked(snap.claude)
	p.ReservePermitted = snap.permittedIDs()
	p.ReservePooled = snap.pooledIDs()
	p.ReserveOwnerDefaultBlocked = snap.defaultBlocked()
	p.ReserveJudgeMode = snap.judgeMode
	p.ReserveJudgeStaticBlocked = snap.staticDecision(reserveLane{mode: snap.judgeMode, secretID: snap.judgeSecret}).held
	lane := laneOfWorker(wkr)
	p.ReserveWorkerMode = lane.mode
	p.ReserveWorkerStaticBlocked = snap.staticDecision(lane).held
	if blockedWorkers != nil {
		p.ReserveBlockedWorkerIds = blockedWorkers
	}
}

// reserveBlockedWorkers lists the owner's workers whose lane the reserve blocks, for the spread-peer
// clause. Only called while the reserve is on.
func (s *Service) reserveBlockedWorkers(ctx context.Context, snap *reserveSnap) ([]uuid.UUID, error) {
	out := []uuid.UUID{}
	if !snap.on() {
		return out, nil
	}
	q, ok := s.q.(reserveWorkerQueries)
	if !ok {
		return out, nil
	}
	workers, err := q.ListReserveWorkers(ctx, snap.userID)
	if err != nil {
		return nil, fmt.Errorf("weekly reserve workers: %w", err)
	}
	for _, w := range workers {
		lane := reserveLane{mode: w.AnthropicBindMode}
		if w.AnthropicBindMode == BindModePinned && w.AnthropicSecretID.Valid {
			id := uuid.UUID(w.AnthropicSecretID.Bytes)
			lane.secretID = &id
		}
		if snap.laneBlocked(lane) {
			out = append(out, w.ID)
		}
	}
	return out, nil
}

// reserveWorkerQueries is the optional worker-list read behind reserveBlockedWorkers and the marker pass.
type reserveWorkerQueries interface {
	ListReserveWorkers(ctx context.Context, userID uuid.UUID) ([]store.ListReserveWorkersRow, error)
}

// EphemeralReserveHeld is the ephemeral provisioner's locked recheck (hostedsvc.provisionOne): it
// reports whether the weekly usage reserve would hold the run on the lane a freshly provisioned
// ephemeral worker would get (auto when the owner has a pooled token, else default). A plain read: it
// takes no row lock, like the intake-admission recheck beside it. A run that is not queued reads as
// not held.
func EphemeralReserveHeld(ctx context.Context, q *store.Queries, runID uuid.UUID, iv ReserveIntervals, now time.Time) (bool, error) {
	run, err := q.GetRunByID(ctx, runID)
	if err != nil {
		return false, err
	}
	if run.Status != "queued" {
		return false, nil
	}
	snap, err := buildReserveSnapshot(ctx, q, run.UserID, now, reserveIntervals{claude: iv.Claude, codex: iv.Codex})
	if err != nil {
		return false, err
	}
	if !reserveEnforced(run, snap, now) {
		return false, nil
	}
	hasPool, err := q.UserHasAutoEligibleAnthropicToken(ctx, run.UserID)
	if err != nil {
		return false, err
	}
	return reserveHeld(run, snap, laneEphemeral(hasPool), now).held, nil
}
