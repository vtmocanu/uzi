package workersvc

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/autoselectrow"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/usagereserve"
)

// next_claim.go answers, read-only, "why would this run's NEXT claim not leave its current
// credential, and what could the owner do about it" (PRD #4123). It is two halves: a loader that reads
// everything one owner's answer needs ONCE (LoadNextClaimSnapshot), and a pure function over that
// snapshot (NextClaimReason) built on the very same selection kernel the claim runs (decideAuto,
// claimExcludeAt, reserveEnforced), so the preview names the credential the claim would spend by
// construction. Nothing here writes, and nothing here is on the claim path.

// The closed set of reasons NextClaimReason reports.
const (
	NextClaimSwitchPending     = "switch_pending"
	NextClaimCanSwitch         = "can_switch"
	NextClaimSelectionSame     = "selection_unchanged"
	NextClaimHeadroomOutside   = "headroom_outside_pool"
	NextClaimPoolExhausted     = "pool_exhausted"
	NextClaimNoAlternative     = "no_alternative"
	NextClaimAccountBound      = "account_bound"
	NextClaimUnknown           = "unknown"
	NextClaimNotAuto           = "not_auto"
	NextClaimPinned            = "pinned"
	nextClaimCredentialKindAnt = store.KindAnthropicToken
	nextClaimCredentialKindCdx = store.KindCodexAuth
)

// The closed set of future-run fix kinds.
const (
	// FutureFixPoolOn: pool the named credential so an auto lane can spend it.
	FutureFixPoolOn = "pool_on"
	// FutureFixCodexModeAuto: set the owner's Codex account mode to auto.
	FutureFixCodexModeAuto = "codex_mode_auto"
)

// The closed set of PreviewWait answers.
const (
	WaitWaits        = "waits"
	WaitFails        = "fails"
	WaitReservePause = "reserve_pause"
	WaitUnknown      = "unknown"
)

// CredentialRef names one credential an owner could move to.
type CredentialRef struct {
	ID    uuid.UUID
	Label string
	// Kind is the credential's secret kind ("anthropic_token" or "codex_auth").
	Kind string
}

// FutureFix is advice that helps the owner's future runs, not this one.
type FutureFix struct {
	Kind string
	// Credential is the credential the fix is about (nil for FutureFixCodexModeAuto).
	Credential *CredentialRef
}

// NextClaimResult is NextClaimReason's answer. Alternative is the credential the owner could switch
// to, when there is a usable one. ManualSwitch is whether the set-token verb's pure gates would accept
// a switch of this run right now (advice from one snapshot: the verb re-checks under its locks and
// remains the authority).
type NextClaimResult struct {
	Reason       string
	Alternative  *CredentialRef
	FutureRuns   []FutureFix
	ManualSwitch bool
}

// codexAccountView is one linked Codex account as the preview needs it.
type codexAccountView struct {
	id       uuid.UUID
	aliases  []string // enabled linked alias labels
	reauth   bool
	reading  usagereserve.CodexWeeklyReading
	hasRead  bool
	readTime *time.Time
}

// NextClaimSnapshot is everything NextClaimReason (and the weeklylimit wrappers) read about one
// owner, at one instant. It is built once per owner by LoadNextClaimSnapshot and never mutated by
// the pure functions.
type NextClaimSnapshot struct {
	userID      uuid.UUID
	now         time.Time
	policy      autoselect.Policy
	maxWaits    int
	maxPark     time.Duration
	staleCutoff time.Time

	// cands is every enabled Anthropic token as the auto selector reads it (pooled or not).
	cands []autoselect.Candidate
	// reserve is the owner's reserve state with raw weekly readings kept (keepWeekly): OFF when the
	// owner has no threshold, but its raw readings are still present.
	reserve *reserveSnap
	workers map[uuid.UUID]store.Worker

	// Codex: the owner's account mode, whether the codex reads were available, their linked
	// accounts with raw readings, their codex_auth aliases by id and label, and the pooled alias ids.
	codexMode        string
	codexAvailable   bool
	codexAccounts    []codexAccountView
	codexAliasLabels map[uuid.UUID]string
	// codexAliasAccount maps a linked alias id (enabled or not) to its provider account id.
	codexAliasAccount map[uuid.UUID]uuid.UUID
	codexPooled       map[uuid.UUID]bool

	// settling is the credential-switch settling fence of paused runs, filled by
	// LoadNextClaimSettling for the runs the caller will ask about.
	settling map[uuid.UUID]bool

	// MarginPts is the owner's warning margin in percentage points (users.weekly_warning_margin_pts,
	// read in the same query as the reserve settings): how far below its effective limit a credential
	// is "near". Zero turns the early warnings off, and is also what a store that cannot serve the
	// reserve reads reports.
	MarginPts int
}

// nextClaimCodexQueries is the optional Codex read surface of the snapshot loader. *store.Queries
// satisfies it; a fake store that does not reads as "Codex unavailable" (reason unknown).
type nextClaimCodexQueries interface {
	GetUserCodexAccountMode(ctx context.Context, id uuid.UUID) (string, error)
	ListUserSecretsForKind(ctx context.Context, arg store.ListUserSecretsForKindParams) ([]store.ListUserSecretsForKindRow, error)
	GetCodexAccountRateLimitsForUser(ctx context.Context, userID uuid.UUID) ([]store.GetCodexAccountRateLimitsForUserRow, error)
	ListCodexPoolAliases(ctx context.Context, userID uuid.UUID) ([]store.ListCodexPoolAliasesRow, error)
	ListCodexAliasAccountsForUser(ctx context.Context, userID uuid.UUID) ([]store.ListCodexAliasAccountsForUserRow, error)
}

// LoadNextClaimSnapshot reads one owner's next-claim state once: the auto-select candidates, the
// reserve settings with the raw weekly readings of every Anthropic token and Codex alias (even with
// no threshold), the judge binding, the owner's workers and, when the store serves them, the Codex
// account mode, linked accounts and aliases. All reads; the claim path is untouched.
func (s *Service) LoadNextClaimSnapshot(ctx context.Context, ownerID uuid.UUID, now time.Time) (*NextClaimSnapshot, error) {
	snap := &NextClaimSnapshot{
		userID: ownerID, now: now, policy: s.p.Autoselect,
		maxWaits: s.p.RunLimitMaxWaits, maxPark: s.p.RunLimitMaxPark,
		staleCutoff: now.Add(-s.p.WorkerHeartbeatStale),
		workers:     map[uuid.UUID]store.Worker{},
		settling:    map[uuid.UUID]bool{},
	}
	if s.p.WorkerHeartbeatStale <= 0 {
		snap.staleCutoff = time.Time{}
	}

	rows, err := s.q.ListAutoSelectCandidates(ctx, ownerID)
	if err != nil {
		return nil, fmt.Errorf("next claim: auto-select candidates: %w", err)
	}
	snap.cands = make([]autoselect.Candidate, 0, len(rows))
	for _, row := range rows {
		snap.cands = append(snap.cands, autoselectrow.FromCandidateRow(row))
	}

	if q, ok := s.q.(reserveQueries); ok {
		if snap.reserve, err = buildReserveSnapshot(ctx, q, ownerID, now, s.reserveIntervals(), true); err != nil {
			return nil, fmt.Errorf("next claim: %w", err)
		}
		snap.MarginPts = snap.reserve.marginPts
	} else {
		// A store that cannot serve the reserve reads reads as OFF, but the judge binding is still
		// one plain read.
		snap.reserve = &reserveSnap{userID: ownerID}
		bound, jerr := s.q.GetUserJudgeAnthropicBinding(ctx, ownerID)
		if jerr != nil {
			return nil, fmt.Errorf("next claim: judge binding: %w", jerr)
		}
		snap.reserve.judgeMode = bound.JudgeAnthropicBindMode
		if bound.JudgeAnthropicSecretID.Valid {
			id := uuid.UUID(bound.JudgeAnthropicSecretID.Bytes)
			snap.reserve.judgeSecret = &id
		}
	}

	workers, err := s.q.ListWorkersByUser(ctx, ownerID)
	if err != nil {
		return nil, fmt.Errorf("next claim: workers: %w", err)
	}
	for _, w := range workers {
		snap.workers[w.ID] = store.Worker{
			ID: w.ID, UserID: w.UserID, Name: w.Name, LastHeartbeatAt: w.LastHeartbeatAt,
			AnthropicBindMode: w.AnthropicBindMode, AnthropicSecretID: w.AnthropicSecretID,
			ProtocolCapabilities: w.ProtocolCapabilities, SnapshotRegisterNonce: w.SnapshotRegisterNonce,
			CredentialSwitchApplyNonce: w.CredentialSwitchApplyNonce,
		}
	}

	if cq, ok := s.q.(nextClaimCodexQueries); ok {
		if err := snap.loadCodex(ctx, cq, ownerID, now, s.p.CodexUsagePollInterval); err != nil {
			return nil, err
		}
	}
	return snap, nil
}

func (n *NextClaimSnapshot) loadCodex(ctx context.Context, q nextClaimCodexQueries, ownerID uuid.UUID, now time.Time, interval time.Duration) error {
	mode, err := q.GetUserCodexAccountMode(ctx, ownerID)
	if err != nil {
		return fmt.Errorf("next claim: codex account mode: %w", err)
	}
	n.codexMode = mode
	secrets, err := q.ListUserSecretsForKind(ctx, store.ListUserSecretsForKindParams{UserID: ownerID, Kind: store.KindCodexAuth})
	if err != nil {
		return fmt.Errorf("next claim: codex aliases: %w", err)
	}
	n.codexAliasLabels = make(map[uuid.UUID]string, len(secrets))
	for _, sec := range secrets {
		n.codexAliasLabels[sec.ID] = sec.Label
	}
	links, err := q.ListCodexAliasAccountsForUser(ctx, ownerID)
	if err != nil {
		return fmt.Errorf("next claim: codex alias links: %w", err)
	}
	n.codexAliasAccount = make(map[uuid.UUID]uuid.UUID, len(links))
	for _, l := range links {
		if l.ProviderAccountID.Valid {
			n.codexAliasAccount[l.UserSecretID] = uuid.UUID(l.ProviderAccountID.Bytes)
		}
	}
	pool, err := q.ListCodexPoolAliases(ctx, ownerID)
	if err != nil {
		return fmt.Errorf("next claim: codex pool: %w", err)
	}
	n.codexPooled = make(map[uuid.UUID]bool, len(pool))
	for _, p := range pool {
		n.codexPooled[p.AliasID] = true
	}
	accts, err := q.GetCodexAccountRateLimitsForUser(ctx, ownerID)
	if err != nil {
		return fmt.Errorf("next claim: codex readings: %w", err)
	}
	for _, a := range accts {
		v := codexAccountView{id: a.ProviderAccountID, aliases: a.Aliases, reauth: a.ReauthRequired}
		r := usagereserve.CodexReading{}
		if a.LastSuccessAt.Valid {
			t := a.LastSuccessAt.Time
			r.LastSuccessAt = &t
			v.readTime = &t
		}
		if len(a.Buckets) > 0 {
			var buckets []apitypes.CodexRateLimitBucketDTO
			if json.Unmarshal(a.Buckets, &buckets) == nil {
				r.Present = true
				r.Buckets = buckets
			}
		}
		v.hasRead = r.Present
		v.reading = usagereserve.CodexWeekly(r, interval, now)
		n.codexAccounts = append(n.codexAccounts, v)
	}
	n.codexAvailable = true
	return nil
}

// LoadNextClaimSettling reads, for the paused runs among runs, the credential-switch settling fence
// the set-token verb refuses on, so ManualSwitch can account for it. Read-only; other runs read
// nothing.
func (s *Service) LoadNextClaimSettling(ctx context.Context, snap *NextClaimSnapshot, runs []store.Run) error {
	for _, r := range runs {
		if r.Status != "paused" || (r.HoldReason.Valid && (r.HoldReason.String == holdCredentialsReset || r.HoldReason.String == "credential_disabled")) {
			continue
		}
		settling, err := s.runHoldSettling(ctx, r.ID)
		if err != nil {
			return fmt.Errorf("next claim: read hold fence: %w", err)
		}
		snap.settling[r.ID] = settling
	}
	return nil
}

// nextClaimExcluded reports a run the preview never speaks for: a provider-bound run (its credential
// is a frozen provider key) and a Codex API-key run (structurally no subscription meter).
func nextClaimExcluded(run store.Run) bool {
	return providerBoundRun(run) || (run.Harness == harnessCodex && run.CodexAuthMode.Valid && run.CodexAuthMode.String == codexAuthModeAPIKey)
}

// effectiveLimit is the percent at which the owner's reserve holds, or 100 with no threshold.
func (n *NextClaimSnapshot) effectiveLimit() float64 {
	if n.reserve != nil && n.reserve.threshold != nil {
		return float64(*n.reserve.threshold)
	}
	return 100
}

// ReserveEnforced reports whether the weekly usage reserve applies to the run (the claim path's own
// exemption rule: a threshold, not chat, not frozen to a custom model provider, no active manual-resume
// override, a started run only while pausing is on).
func (n *NextClaimSnapshot) ReserveEnforced(run store.Run) bool {
	return reserveEnforced(run, n.reserve, n.now)
}

// WeeklyRaw is a credential's raw weekly reading. OK is false when it has no usable fresh reading.
// Windows lists every Codex weekly window (nil for Anthropic).
type WeeklyRaw = usagereserve.CodexWeeklyReading

// Weekly is the raw weekly reading of a credential key: "anthropic:<secret id>" or "codex:<provider
// account id>". ok is false for an unknown key or a key whose reading was not loaded.
func (n *NextClaimSnapshot) Weekly(credKey string) (WeeklyRaw, bool) {
	kind, id, found := strings.Cut(credKey, ":")
	if !found {
		return WeeklyRaw{}, false
	}
	u, err := uuid.Parse(id)
	if err != nil {
		return WeeklyRaw{}, false
	}
	switch kind {
	case "anthropic":
		if n.reserve == nil {
			return WeeklyRaw{}, false
		}
		w, ok := n.reserve.weeklyClaude[u]
		return WeeklyRaw{OK: w.OK, Pct: w.Pct, ResetsAt: w.ResetsAt}, ok
	case "codex":
		for _, a := range n.codexAccounts {
			if a.id == u {
				return a.reading, true
			}
		}
	}
	return WeeklyRaw{}, false
}

// codexAccountOfAlias finds the linked account an alias id is bound to, by the alias's own link (so a
// disabled alias still resolves). ok is false when the alias is unlinked or its account has no view.
func (n *NextClaimSnapshot) codexAccountOfAlias(alias uuid.UUID) (codexAccountView, bool) {
	acct, ok := n.codexAliasAccount[alias]
	if !ok {
		return codexAccountView{}, false
	}
	for _, a := range n.codexAccounts {
		if a.id == acct {
			return a, true
		}
	}
	return codexAccountView{}, false
}

func (n *NextClaimSnapshot) gateFor(run store.Run) *reserveGate {
	if !reserveEnforced(run, n.reserve, n.now) {
		return nil
	}
	return &reserveGate{snap: n.reserve, now: n.now}
}

func (n *NextClaimSnapshot) candidate(id uuid.UUID) (autoselect.Candidate, bool) {
	for _, c := range n.cands {
		if c.SecretID == id {
			return c, true
		}
	}
	return autoselect.Candidate{}, false
}

func (n *NextClaimSnapshot) anthropicRef(c autoselect.Candidate) *CredentialRef {
	return &CredentialRef{ID: c.SecretID, Label: c.Label, Kind: nextClaimCredentialKindAnt}
}

// NextClaimReason computes the run's next-claim reason from one snapshot. It returns nil for a run it
// never speaks for (provider-bound, Codex API-key). It writes and reads nothing.
func (s *Service) NextClaimReason(run store.Run, snap *NextClaimSnapshot) *NextClaimResult {
	return snap.nextClaimReason(run)
}

func (n *NextClaimSnapshot) nextClaimReason(run store.Run) *NextClaimResult {
	if nextClaimExcluded(run) {
		return nil
	}
	res := &NextClaimResult{Reason: NextClaimUnknown}
	var worker store.Worker
	if run.WorkerID.Valid {
		worker = n.workers[uuid.UUID(run.WorkerID.Bytes)]
	}
	res.ManualSwitch = n.manualSwitch(run, worker)

	// A switch already requested or settling outranks everything: the next claim is decided.
	if n.switchPending(run) {
		res.Reason = NextClaimSwitchPending
		return res
	}
	if run.Harness == harnessCodex {
		n.codexReason(run, res)
		return res
	}

	mode := n.claimMode(run, worker)
	switch mode.kind {
	case claimKindUnknown:
		return res
	case claimKindPinned:
		res.Reason = NextClaimPinned
		return res
	case claimKindNotAuto:
		res.Reason = NextClaimNotAuto
		return res
	}
	n.autoReason(run, mode.judge, res)
	return res
}

type claimKind int

const (
	claimKindUnknown claimKind = iota
	claimKindPinned
	claimKindNotAuto
	claimKindAuto
)

type claimModeResult struct {
	kind  claimKind
	judge bool
}

// claimMode is the precedence ladder of the NEXT claim's credential lane, derived from the rows
// (never from anthropic_select_reason, which describes the previous claim): the judge binding for
// judge and self_improve, then the per-run override, then the recorded worker's bind mode; a missing
// worker is unknown (effectiveNextClaimMode).
func (n *NextClaimSnapshot) claimMode(run store.Run, worker store.Worker) claimModeResult {
	// Chat is deliberately not bindable (PRD #104 D1): assembleChatClaim always opens the owner's
	// default token, whatever the worker's binding or a pool says, so its lane is never auto.
	if run.Kind == runkind.Chat {
		return claimModeResult{kind: claimKindNotAuto}
	}
	if run.Kind == runkind.SelfImprove || run.Kind == runkind.Judge {
		if n.reserve != nil && n.reserve.judgeMode == BindModeAuto {
			return claimModeResult{kind: claimKindAuto, judge: true}
		}
		return claimModeResult{kind: claimKindNotAuto}
	}
	if run.CredentialOverrideMode.Valid && run.CredentialOverrideMode.String != "" {
		switch run.CredentialOverrideMode.String {
		case BindModePinned:
			if run.CredentialOverrideSecretID.Valid {
				return claimModeResult{kind: claimKindPinned}
			}
			// A nulled pin inherits the worker's binding below.
		case BindModeAuto:
			return claimModeResult{kind: claimKindAuto}
		case BindModeDefault:
			return claimModeResult{kind: claimKindNotAuto}
		}
	}
	if !run.WorkerID.Valid || worker.ID == uuid.Nil {
		return claimModeResult{kind: claimKindUnknown}
	}
	if worker.AnthropicBindMode == BindModeAuto {
		return claimModeResult{kind: claimKindAuto}
	}
	return claimModeResult{kind: claimKindNotAuto}
}

// switchPending: the settling or unsettled credential-switch hold, or a pending switch request.
func (n *NextClaimSnapshot) switchPending(run store.Run) bool {
	if run.HoldReason.Valid && (run.HoldReason.String == holdReasonCredentialSwitchSettling || run.HoldReason.String == holdReasonCredentialSwitchUnsettled) {
		return true
	}
	return run.CredentialSwitchRequestedAt.Valid
}

func (n *NextClaimSnapshot) manualSwitch(run store.Run, worker store.Worker) bool {
	facts := switchFacts{Stage: switchStageFull, Settling: n.settling[run.ID], StaleCutoff: n.staleCutoff}
	if run.Harness == harnessCodex {
		return codexSwitchRefusal(run, worker, facts) == nil
	}
	return anthropicSwitchRefusal(run, worker, facts) == nil
}

// currentAnthropic is the credential the run is spending now (or just spent): the recorded one.
func currentAnthropic(run store.Run) uuid.UUID {
	if run.AnthropicSecretID.Valid {
		return uuid.UUID(run.AnthropicSecretID.Bytes)
	}
	return uuid.Nil
}

// autoReason runs the shared selection kernel over the snapshot and maps its decision.
func (n *NextClaimSnapshot) autoReason(run store.Run, judge bool, res *NextClaimResult) {
	cur := currentAnthropic(run)
	exclude := claimExcludeAt(run.LimitDeadSecretID, run.RetryNotBefore, n.now)
	gate := n.gateFor(run)
	dec := decideAuto(autoInput{cands: n.cands, exclude: exclude, gate: gate, policy: n.policy, now: n.now})

	chosen, chose := uuid.Nil, false
	switch dec.kind {
	case autoPicked, autoFloor:
		chosen, chose = dec.id, true
	case autoPoolEmpty:
		// judgeChoice's D4: the judge lane never holds on an empty pool; it spends the owner default.
		if judge && n.reserve != nil && n.reserve.defaultID != uuid.Nil {
			chosen, chose = n.reserve.defaultID, true
		}
	}
	if chose {
		if chosen != cur {
			res.Reason = NextClaimCanSwitch
			if c, ok := n.candidate(chosen); ok {
				res.Alternative = n.anthropicRef(c)
			}
			return
		}
		if n.otherPooled(cur, gate) {
			res.Reason = NextClaimSelectionSame
			return
		}
	}

	// The kernel keeps (or cannot leave) the current credential. Classify by the alternatives the
	// owner has: usable means another credential, enabled (candidates are enabled only) and not rejected.
	if alt := n.unpooledWithHeadroom(cur); alt != nil {
		res.Reason = NextClaimHeadroomOutside
		res.Alternative = n.anthropicRef(*alt)
		res.FutureRuns = []FutureFix{{Kind: FutureFixPoolOn, Credential: n.anthropicRef(*alt)}}
		return
	}
	for _, c := range n.cands {
		if c.SecretID != cur && c.AutoEligible && !c.Rejected {
			res.Reason = NextClaimPoolExhausted
			return
		}
	}
	res.Reason = NextClaimNoAlternative
}

// otherPooled reports another eligible pooled candidate besides cur that the reserve does not block.
func (n *NextClaimSnapshot) otherPooled(cur uuid.UUID, gate *reserveGate) bool {
	for _, c := range n.cands {
		if c.SecretID == cur || !c.AutoEligible || c.Rejected {
			continue
		}
		if gate != nil && gate.snap.claudeBlocked(c.SecretID) {
			continue
		}
		return true
	}
	return false
}

// unpooledWithHeadroom is the unpooled, usable alternative with the lowest fresh weekly reading below
// its effective limit; nil when there is none.
func (n *NextClaimSnapshot) unpooledWithHeadroom(cur uuid.UUID) *autoselect.Candidate {
	if n.reserve == nil {
		return nil
	}
	limit := n.effectiveLimit()
	var best *autoselect.Candidate
	bestPct := 0.0
	for i := range n.cands {
		c := n.cands[i]
		if c.SecretID == cur || c.AutoEligible || c.Rejected {
			continue
		}
		w, ok := n.reserve.weeklyClaude[c.SecretID]
		if !ok || !w.OK || w.Pct >= limit {
			continue
		}
		if best == nil || w.Pct < bestPct || (w.Pct == bestPct && c.SecretID.String() < best.SecretID.String()) {
			best, bestPct = &c, w.Pct
		}
	}
	return best
}

// codexReason: a Codex run is bound to the alias frozen at creation (only gate-checked at claim time),
// so its next claim never changes account; the alternative is another account's enabled linked
// codex_auth alias with a fresh reading below the effective limit.
func (n *NextClaimSnapshot) codexReason(run store.Run, res *NextClaimResult) {
	if !n.codexAvailable || !run.CodexSecretID.Valid {
		return
	}
	res.Reason = NextClaimAccountBound
	cur := uuid.UUID(run.CodexSecretID.Bytes)
	curAcct, ok := n.codexAccountOfAlias(cur)
	if !ok {
		// The run's own account cannot be resolved: never guess an alternative.
		res.Reason = NextClaimUnknown
		return
	}
	limit := n.effectiveLimit()

	var bestAcct *codexAccountView
	for i := range n.codexAccounts {
		a := &n.codexAccounts[i]
		if a.id == curAcct.id {
			continue
		}
		if a.reauth || !a.reading.OK || a.reading.Pct >= limit || len(a.aliases) == 0 {
			continue
		}
		if bestAcct == nil || a.reading.Pct < bestAcct.reading.Pct {
			bestAcct = a
		}
	}
	if bestAcct != nil {
		if ref := n.codexAliasRef(*bestAcct); ref != nil {
			res.Alternative = ref
			if !n.codexPooled[ref.ID] {
				res.FutureRuns = append(res.FutureRuns, FutureFix{Kind: FutureFixPoolOn, Credential: ref})
			}
		}
	}
	if n.codexMode != codexAccountModeAuto {
		res.FutureRuns = append(res.FutureRuns, FutureFix{Kind: FutureFixCodexModeAuto})
	}
}

// codexAliasRef is the lowest-id enabled alias of an account, named by label.
func (n *NextClaimSnapshot) codexAliasRef(a codexAccountView) *CredentialRef {
	var ids []uuid.UUID
	for id, label := range n.codexAliasLabels {
		if slices.Contains(a.aliases, label) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return &CredentialRef{ID: ids[0], Label: n.codexAliasLabels[ids[0]], Kind: nextClaimCredentialKindCdx}
}

// PreviewWait says what happens to the run when its weekly window is hit: "waits" (it parks and
// resumes by itself), "fails" (the park decision would fail it), "reserve_pause" (the owner's reserve
// pauses it) or "unknown". It runs the park decision with no jitter and writes nothing.
func (n *NextClaimSnapshot) PreviewWait(run store.Run) string {
	if nextClaimExcluded(run) {
		return WaitUnknown
	}
	if run.Status == "paused" && run.HoldReason.Valid && run.HoldReason.String == holdWeeklyReserve {
		return WaitReservePause
	}
	if run.Status == "running" && reserveEnforced(run, n.reserve, n.now) && n.reserve.pauseRunning {
		return WaitReservePause
	}
	var worker store.Worker
	if run.WorkerID.Valid {
		worker = n.workers[uuid.UUID(run.WorkerID.Bytes)]
	}
	judgeMode := ""
	if n.reserve != nil {
		judgeMode = n.reserve.judgeMode
	}
	sevenDay := "seven_day"
	in := limitParkInput{
		Harness: run.Harness, WaitOnLimit: run.WaitOnLimit, LimitWaitCount: run.LimitWaitCount,
		DeadSecretID: currentAnthropic(run), ReportedType: &sevenDay, Candidates: n.cands,
		NextClaimMode: effectiveNextClaimMode(run, judgeMode, worker), Policy: n.policy,
		MaxWaits: n.maxWaits, MaxPark: n.maxPark, Jitter: 0, Now: n.now,
	}

	if run.Status == "limit_wait" {
		// Already parked: the stored stamps are the decision that was made. Otherwise the park
		// decision is re-run at the stored count, which already includes this park, so it is the
		// count a re-park after the retry would be checked at.
		if !run.LimitResetsAt.Valid {
			return WaitUnknown
		}
		reset := run.LimitResetsAt.Time
		if run.RetryNotBefore.Valid && !run.RetryNotBefore.Time.Before(reset) {
			return WaitWaits
		}
		if run.RetryNotBefore.Valid { // an earlier retry: decision 6e lowered it to a pooled alternative
			if r := n.nextClaimReason(run); r != nil && r.Reason == NextClaimCanSwitch {
				return WaitWaits
			}
		}
		ms := reset.UnixMilli()
		in.ReportedResetMs = &ms
		in.DeadSecretID = uuid.Nil
		if run.LimitDeadSecretID.Valid {
			in.DeadSecretID = uuid.UUID(run.LimitDeadSecretID.Bytes)
		}
		return waitOf(decideLimitPark(in))
	}

	key := ""
	switch {
	case run.Harness == harnessCodex && run.CodexSecretID.Valid:
		if a, ok := n.codexAccountOfAlias(uuid.UUID(run.CodexSecretID.Bytes)); ok {
			key = "codex:" + a.id.String()
		}
	case in.DeadSecretID != uuid.Nil:
		key = "anthropic:" + in.DeadSecretID.String()
	}
	if w, ok := n.Weekly(key); ok && w.OK && w.ResetsAt != nil {
		ms := w.ResetsAt.UnixMilli()
		in.ReportedResetMs = &ms
	}
	return waitOf(decideLimitPark(in))
}

// waitOf maps a park decision onto the preview vocabulary: a park with a known reset waits, a refusal
// fails, and a park with no usable reset (only the exponential fallback) is unknown.
func waitOf(d limitParkDecision) string {
	switch {
	case !d.Park:
		return WaitFails
	case d.LimitResetsAt == nil:
		return WaitUnknown
	}
	return WaitWaits
}
