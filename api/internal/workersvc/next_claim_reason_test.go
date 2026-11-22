package workersvc

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/usagereserve"
)

// Tests of the next-claim preview (NextClaimReason, PreviewWait, the raw-reading wrappers) over a
// snapshot LoadNextClaimSnapshot built from a fake store, and a parity matrix against autoChoice, the
// claim path's own resolver, on the same store.

// ncFake is the fake store the snapshot loader reads. It embeds the shared fakeStore for the
// candidate list and judge binding and adds the reserve, worker and Codex reads.
type ncFake struct {
	*fakeStore
	reserve       store.GetUserUsageReserveRow
	tokens        []store.ListReserveClaudeTokensRow
	aliases       []store.ListReserveCodexAliasesRow
	workers       []store.ListWorkersByUserRow
	codexMode     string
	codexSecrets  []store.ListUserSecretsForKindRow
	codexAccounts []store.GetCodexAccountRateLimitsForUserRow
	codexPool     []store.ListCodexPoolAliasesRow
	codexLinks    []store.ListCodexAliasAccountsForUserRow
}

func (f *ncFake) ListCodexAliasAccountsForUser(context.Context, uuid.UUID) ([]store.ListCodexAliasAccountsForUserRow, error) {
	return f.codexLinks, nil
}

func (f *ncFake) GetUserUsageReserve(context.Context, uuid.UUID) (store.GetUserUsageReserveRow, error) {
	return f.reserve, nil
}
func (f *ncFake) ListReserveClaudeTokens(context.Context, uuid.UUID) ([]store.ListReserveClaudeTokensRow, error) {
	return f.tokens, nil
}
func (f *ncFake) ListReserveCodexAliases(context.Context, uuid.UUID) ([]store.ListReserveCodexAliasesRow, error) {
	return f.aliases, nil
}
func (f *ncFake) ListWorkersByUser(context.Context, uuid.UUID) ([]store.ListWorkersByUserRow, error) {
	return f.workers, nil
}
func (f *ncFake) GetUserCodexAccountMode(context.Context, uuid.UUID) (string, error) {
	return f.codexMode, nil
}
func (f *ncFake) ListUserSecretsForKind(context.Context, store.ListUserSecretsForKindParams) ([]store.ListUserSecretsForKindRow, error) {
	return f.codexSecrets, nil
}
func (f *ncFake) GetCodexAccountRateLimitsForUser(context.Context, uuid.UUID) ([]store.GetCodexAccountRateLimitsForUserRow, error) {
	return f.codexAccounts, nil
}
func (f *ncFake) ListCodexPoolAliases(context.Context, uuid.UUID) ([]store.ListCodexPoolAliasesRow, error) {
	return f.codexPool, nil
}

var (
	ncOwner  = uuid.MustParse("00000000-0000-0000-0000-00000000f001")
	ncWorker = uuid.MustParse("00000000-0000-0000-0000-00000000f002")
	ncTokA   = uuid.MustParse("00000000-0000-0000-0000-0000000000a1")
	ncTokB   = uuid.MustParse("00000000-0000-0000-0000-0000000000a2")
	ncTokD   = uuid.MustParse("00000000-0000-0000-0000-0000000000d1")
)

// ncTok describes one Anthropic token: the candidate row the selector reads and the reserve row the
// raw weekly reading comes from are generated together so they cannot disagree.
type ncTok struct {
	id        uuid.UUID
	label     string
	pooled    bool
	rejected  bool
	isDefault bool
	weekly    int16
	stale     bool
	noReading bool
	resetIn   time.Duration
}

func (t ncTok) rows() (store.ListAutoSelectCandidatesRow, store.ListReserveClaudeTokensRow) {
	synced := autoNow.Add(-time.Minute)
	if t.stale {
		synced = autoNow.Add(-3 * time.Hour)
	}
	reset := t.resetIn
	if reset == 0 {
		reset = 72 * time.Hour
	}
	cand := store.ListAutoSelectCandidatesRow{UserSecretID: t.id, Label: t.label, AutoEligible: t.pooled, Rejected: t.rejected}
	rsv := store.ListReserveClaudeTokensRow{UserSecretID: t.id, IsDefault: t.isDefault, AutoEligible: t.pooled, Rejected: t.rejected}
	if !t.noReading {
		cand.SevenDayPct = pgtype.Int2{Int16: t.weekly, Valid: true}
		cand.SevenDayResetsAt = pgconv.Time(autoNow.Add(reset))
		cand.SyncedAt = pgconv.Time(synced)
		rsv.HasReading = true
		rsv.SevenDayPct = pgtype.Int2{Int16: t.weekly, Valid: true}
		rsv.SevenDayResetsAt = pgconv.Time(autoNow.Add(reset))
		rsv.SyncedAt = pgconv.Time(synced)
	}
	return cand, rsv
}

func ncParams() Params {
	p := autoParams()
	p.UsagePollInterval = 5 * time.Minute
	p.CodexUsagePollInterval = 5 * time.Minute
	p.RunLimitMaxWaits = 3
	p.RunLimitMaxPark = 8 * 24 * time.Hour
	return p
}

type ncEnv struct {
	f   *ncFake
	svc *Service
}

func newNCEnv(t *testing.T, p Params) *ncEnv {
	t.Helper()
	f := &ncFake{fakeStore: &fakeStore{}}
	svc := New(f, newBox(t), p)
	svc.now = func() time.Time { return autoNow }
	return &ncEnv{f: f, svc: svc}
}

func (e *ncEnv) setTokens(toks ...ncTok) {
	e.f.autoCandidates, e.f.tokens = nil, nil
	for _, t := range toks {
		c, r := t.rows()
		e.f.autoCandidates = append(e.f.autoCandidates, c)
		e.f.tokens = append(e.f.tokens, r)
	}
}

func (e *ncEnv) snapshot(t *testing.T) *NextClaimSnapshot {
	t.Helper()
	snap, err := e.svc.LoadNextClaimSnapshot(context.Background(), ncOwner, autoNow)
	if err != nil {
		t.Fatalf("LoadNextClaimSnapshot: %v", err)
	}
	return snap
}

func ncWorkerRow(mode string, caps ...string) store.ListWorkersByUserRow {
	return store.ListWorkersByUserRow{
		ID: ncWorker, UserID: ncOwner, Name: "w1", AnthropicBindMode: mode, ProtocolCapabilities: caps,
		LastHeartbeatAt: pgconv.Time(autoNow.Add(-time.Second)),
	}
}

// ncRun is a parked Claude run that spent token A when it hit its weekly window: A is the dead
// credential, its retry window is still closed.
func ncRun() store.Run {
	return store.Run{
		ID: uuid.New(), UserID: ncOwner, Kind: "issue", Harness: harnessClaude, Status: "limit_wait",
		WorkerID:          pgtype.UUID{Bytes: ncWorker, Valid: true},
		AnthropicSecretID: pgtype.UUID{Bytes: ncTokA, Valid: true},
		LimitDeadSecretID: pgtype.UUID{Bytes: ncTokA, Valid: true},
		RetryNotBefore:    pgconv.Time(autoNow.Add(time.Hour)),
		LimitResetsAt:     pgconv.Time(autoNow.Add(time.Hour)),
		WaitOnLimit:       true,
	}
}

func ncThreshold(v int16) store.GetUserUsageReserveRow {
	return store.GetUserUsageReserveRow{WeeklyReservePct: pgtype.Int2{Int16: v, Valid: true}}
}

type ncCase struct {
	name   string
	toks   []ncTok
	thr    *int16
	order  autoselect.Order
	judge  string
	worker string // worker bind mode; "" = auto
	noWkr  bool   // the run has no recorded worker
	ghost  bool   // the run names a worker the owner does not have
	mutate func(*store.Run)
	want   string
	wantAl string // alternative label ("" = none)
	parity bool   // the claim's autoChoice resolves this run
}

func ncCases() []ncCase {
	A := func(w int16) ncTok { return ncTok{id: ncTokA, label: "tok-a", pooled: true, weekly: w} }
	B := func(w int16) ncTok { return ncTok{id: ncTokB, label: "tok-b", pooled: true, weekly: w} }
	thr70 := i16p(70)
	retryNow := func(r *store.Run) { r.RetryNotBefore = pgconv.Time(autoNow) }
	retryPast := func(r *store.Run) { r.RetryNotBefore = pgconv.Time(autoNow.Add(-time.Minute)) }
	return []ncCase{
		{name: "excluded dead token, a pooled alternative with headroom", toks: []ncTok{A(95), B(40)}, want: NextClaimCanSwitch, wantAl: "tok-b", parity: true},
		{name: "exclude relaxed once retry passed keeps the roomier current token", toks: []ncTok{A(20), B(60)}, mutate: retryPast, want: NextClaimSelectionSame, parity: true},
		{name: "retry exactly now is relaxed", toks: []ncTok{A(20), B(60)}, mutate: retryNow, want: NextClaimSelectionSame, parity: true},
		{name: "retry one nanosecond ahead still excludes", toks: []ncTok{A(20), B(60)}, mutate: func(r *store.Run) { r.RetryNotBefore = pgconv.Time(autoNow.Add(time.Nanosecond)) }, want: NextClaimCanSwitch, wantAl: "tok-b", parity: true},
		{name: "reset-first keeps the soonest weekly reset: selection unchanged", order: autoselect.OrderResetFirst,
			toks:   []ncTok{{id: ncTokA, label: "tok-a", pooled: true, weekly: 50, resetIn: 24 * time.Hour}, {id: ncTokB, label: "tok-b", pooled: true, weekly: 50, resetIn: 120 * time.Hour}},
			mutate: retryPast, want: NextClaimSelectionSame, parity: true},
		{name: "unpooled alternative with headroom", toks: []ncTok{A(95), {id: ncTokB, label: "tok-b", weekly: 30}}, want: NextClaimHeadroomOutside, wantAl: "tok-b"},
		{name: "unpooled rejected alternative is not named", toks: []ncTok{A(95), {id: ncTokB, label: "tok-b", weekly: 30, rejected: true}}, want: NextClaimNoAlternative},
		{name: "unpooled alternative at the effective limit is not named", toks: []ncTok{A(95), {id: ncTokB, label: "tok-b", weekly: 100}}, want: NextClaimNoAlternative},
		{name: "unpooled alternative over the reserve threshold is not named", thr: thr70, toks: []ncTok{A(95), {id: ncTokB, label: "tok-b", weekly: 75}}, want: NextClaimNoAlternative},
		{name: "unpooled alternative with no fresh reading is not named", toks: []ncTok{A(95), {id: ncTokB, label: "tok-b", noReading: true}}, want: NextClaimNoAlternative},
		{name: "sole token", toks: []ncTok{A(95)}, want: NextClaimNoAlternative, parity: true},
		{name: "pooled alternative the reserve blocks is exhausted", thr: thr70, toks: []ncTok{A(95), B(90)}, want: NextClaimPoolExhausted, parity: true},
		{name: "reserve-held stale alternative is exhausted, not named", thr: thr70, toks: []ncTok{A(95), {id: ncTokB, label: "tok-b", pooled: true, weekly: 10, stale: true}}, want: NextClaimPoolExhausted, parity: true},
		{name: "without a reserve a stale pooled alternative is floored onto", toks: []ncTok{A(95), {id: ncTokB, label: "tok-b", pooled: true, weekly: 10, stale: true}}, want: NextClaimCanSwitch, wantAl: "tok-b", parity: true},
		{name: "all exhausted: the floor still moves the run", toks: []ncTok{A(100), B(100)}, want: NextClaimCanSwitch, wantAl: "tok-b", parity: true},
		{name: "reserve with room on the alternative can switch", thr: thr70, toks: []ncTok{A(95), B(40)}, want: NextClaimCanSwitch, wantAl: "tok-b", parity: true},
		{name: "rejected pooled alternative is not usable", toks: []ncTok{A(95), {id: ncTokB, label: "tok-b", pooled: true, rejected: true, weekly: 10}}, want: NextClaimNoAlternative, parity: true},
		{name: "worker not on auto", worker: BindModeDefault, toks: []ncTok{A(95), B(40)}, want: NextClaimNotAuto},
		{name: "worker pinned", worker: BindModePinned, toks: []ncTok{A(95), B(40)}, want: NextClaimNotAuto},
		{name: "run override pinned", toks: []ncTok{A(95), B(40)}, want: NextClaimPinned,
			mutate: func(r *store.Run) {
				r.CredentialOverrideMode = pgtype.Text{String: BindModePinned, Valid: true}
				r.CredentialOverrideSecretID = pgtype.UUID{Bytes: ncTokA, Valid: true}
			}},
		{name: "run override default", toks: []ncTok{A(95), B(40)}, want: NextClaimNotAuto,
			mutate: func(r *store.Run) { r.CredentialOverrideMode = pgtype.Text{String: BindModeDefault, Valid: true} }},
		{name: "per-run auto on a pinned worker", worker: BindModePinned, toks: []ncTok{A(95), B(40)}, want: NextClaimCanSwitch, wantAl: "tok-b", parity: true,
			mutate: func(r *store.Run) { r.CredentialOverrideMode = pgtype.Text{String: BindModeAuto, Valid: true} }},
		{name: "a nulled pin inherits the worker", toks: []ncTok{A(95), B(40)}, want: NextClaimCanSwitch, wantAl: "tok-b", parity: true,
			mutate: func(r *store.Run) { r.CredentialOverrideMode = pgtype.Text{String: BindModePinned, Valid: true} }},
		{name: "no recorded worker", noWkr: true, toks: []ncTok{A(95), B(40)}, want: NextClaimUnknown},
		{name: "recorded worker the owner no longer has", ghost: true, toks: []ncTok{A(95), B(40)}, want: NextClaimUnknown},
		{name: "a pending switch request", toks: []ncTok{A(95), B(40)}, want: NextClaimSwitchPending,
			mutate: func(r *store.Run) { r.CredentialSwitchRequestedAt = pgconv.Time(autoNow) }},
		{name: "a settling switch hold", toks: []ncTok{A(95), B(40)}, want: NextClaimSwitchPending,
			mutate: func(r *store.Run) {
				r.Status = "paused"
				r.HoldReason = pgtype.Text{String: holdReasonCredentialSwitchSettling, Valid: true}
			}},
		{name: "judge on auto with a pooled alternative", judge: BindModeAuto, toks: []ncTok{A(95), B(40)}, want: NextClaimCanSwitch, wantAl: "tok-b",
			mutate: func(r *store.Run) { r.Kind = runkind.Judge }},
		{name: "self_improve follows the judge binding, not the worker", judge: BindModeAuto, worker: BindModeDefault, toks: []ncTok{A(95), B(40)}, want: NextClaimCanSwitch, wantAl: "tok-b",
			mutate: func(r *store.Run) { r.Kind = runkind.SelfImprove }},
		{name: "judge not on auto", judge: BindModePinned, toks: []ncTok{A(95), B(40)}, want: NextClaimNotAuto,
			mutate: func(r *store.Run) { r.Kind = runkind.Judge }},
		{name: "judge empty pool falls back to the default, which is another token", judge: BindModeAuto,
			toks: []ncTok{{id: ncTokA, label: "tok-a", weekly: 95}, {id: ncTokD, label: "tok-d", isDefault: true, weekly: 10}}, want: NextClaimCanSwitch, wantAl: "tok-d",
			mutate: func(r *store.Run) { r.Kind = runkind.Judge }},
		{name: "judge empty pool whose default is the current token is never pool_exhausted", judge: BindModeAuto,
			toks: []ncTok{{id: ncTokA, label: "tok-a", isDefault: true, weekly: 95}}, want: NextClaimNoAlternative,
			mutate: func(r *store.Run) { r.Kind = runkind.Judge }},
	}
}

func TestNextClaimReasonMatrix(t *testing.T) {
	for _, c := range ncCases() {
		t.Run(c.name, func(t *testing.T) {
			e := newNCEnv(t, ncParams())
			if c.order != "" {
				e.svc.p.Autoselect.Order = c.order
			}
			e.setTokens(c.toks...)
			if c.thr != nil {
				e.f.reserve = ncThreshold(*c.thr)
			}
			e.f.judgeBindMode = c.judge
			mode := c.worker
			if mode == "" {
				mode = BindModeAuto
			}
			e.f.workers = []store.ListWorkersByUserRow{ncWorkerRow(mode)}
			run := ncRun()
			if c.noWkr {
				run.WorkerID = pgtype.UUID{}
			}
			if c.ghost {
				run.WorkerID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
			}
			if c.mutate != nil {
				c.mutate(&run)
			}
			snap := e.snapshot(t)
			got := e.svc.NextClaimReason(run, snap)
			if got == nil {
				t.Fatalf("NextClaimReason = nil, want %q", c.want)
			}
			if got.Reason != c.want {
				t.Fatalf("reason = %q, want %q", got.Reason, c.want)
			}
			switch {
			case c.wantAl == "" && got.Alternative != nil:
				t.Errorf("alternative = %+v, want none", got.Alternative)
			case c.wantAl != "" && (got.Alternative == nil || got.Alternative.Label != c.wantAl || got.Alternative.Kind != "anthropic_token"):
				t.Errorf("alternative = %+v, want %q", got.Alternative, c.wantAl)
			}
			if c.want == NextClaimHeadroomOutside && (len(got.FutureRuns) != 1 || got.FutureRuns[0].Kind != FutureFixPoolOn || got.FutureRuns[0].Credential.Label != c.wantAl) {
				t.Errorf("future runs = %+v, want pool_on %q", got.FutureRuns, c.wantAl)
			}
			if c.parity {
				ncAssertParity(t, e, snap, run, got)
			}
		})
	}
}

// ncAssertParity runs the claim path's own resolver (autoChoice, with the reserve gate the claim
// would carry) on the same store and requires the preview to agree with it: the preview says
// can_switch exactly when the claim would spend a credential other than the current one, and then
// names that very credential.
func ncAssertParity(t *testing.T, e *ncEnv, snap *NextClaimSnapshot, run store.Run, got *NextClaimResult) {
	t.Helper()
	ctx := snap.gateFor(run).attach(context.Background())
	choice, err := e.svc.autoChoice(ctx, run)
	cur := currentAnthropic(run)
	if err == nil && choice.secretID != nil && *choice.secretID != cur {
		if got.Reason != NextClaimCanSwitch || got.Alternative == nil || got.Alternative.ID != *choice.secretID {
			t.Errorf("parity: autoChoice picks %v (not current %v) but the preview says %+v", *choice.secretID, cur, got)
		}
		return
	}
	if got.Reason == NextClaimCanSwitch {
		t.Errorf("parity: autoChoice = (%+v, %v) keeps the current credential but the preview says can_switch", choice, err)
	}
}

func TestNextClaimReasonNeverSpeaksForStructurallyExcludedRuns(t *testing.T) {
	e := newNCEnv(t, ncParams())
	e.setTokens(ncTok{id: ncTokA, label: "tok-a", pooled: true, weekly: 95}, ncTok{id: ncTokB, label: "tok-b", pooled: true, weekly: 10})
	e.f.workers = []store.ListWorkersByUserRow{ncWorkerRow(BindModeAuto)}
	snap := e.snapshot(t)

	provider := ncRun()
	provider.ModelProviderID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	apiKey := ncRun()
	apiKey.Harness = harnessCodex
	apiKey.CodexAuthMode = pgtype.Text{String: codexAuthModeAPIKey, Valid: true}
	for name, run := range map[string]store.Run{"provider-bound": provider, "codex api key": apiKey} {
		if got := e.svc.NextClaimReason(run, snap); got != nil {
			t.Errorf("%s: NextClaimReason = %+v, want nil", name, got)
		}
		if got := snap.PreviewWait(run); got != WaitUnknown {
			t.Errorf("%s: PreviewWait = %q, want unknown", name, got)
		}
	}
}

func TestNextClaimReasonIsReadOnlyAndLoadsOncePerOwner(t *testing.T) {
	e := newNCEnv(t, ncParams())
	e.setTokens(ncTok{id: ncTokA, label: "tok-a", pooled: true, weekly: 95}, ncTok{id: ncTokB, label: "tok-b", pooled: true, weekly: 10})
	e.f.workers = []store.ListWorkersByUserRow{ncWorkerRow(BindModeAuto)}
	snap := e.snapshot(t)
	if n := len(e.f.autoCandidateLookups); n != 1 {
		t.Fatalf("snapshot candidate lookups = %d, want exactly 1", n)
	}
	for i := 0; i < 5; i++ {
		e.svc.NextClaimReason(ncRun(), snap)
		snap.PreviewWait(ncRun())
	}
	if n := len(e.f.autoCandidateLookups); n != 1 {
		t.Fatalf("pure reasons re-read candidates: lookups = %d, want 1", n)
	}
}

func TestNextClaimManualSwitch(t *testing.T) {
	e := newNCEnv(t, ncParams())
	e.setTokens(ncTok{id: ncTokA, label: "tok-a", pooled: true, weekly: 95}, ncTok{id: ncTokB, label: "tok-b", pooled: true, weekly: 10})
	e.f.workers = []store.ListWorkersByUserRow{ncWorkerRow(BindModeAuto)}
	snap := e.snapshot(t)

	cases := []struct {
		name   string
		mutate func(*store.Run)
		want   bool
	}{
		{"parked limit_wait run", func(*store.Run) {}, true},
		{"queued run", func(r *store.Run) { r.Status = "queued" }, true},
		{"terminal run", func(r *store.Run) { r.Status = "failed" }, false},
		{"claimed run", func(r *store.Run) { r.Status = "claimed" }, false},
		{"judge lane is not switchable", func(r *store.Run) { r.Kind = runkind.Judge }, false},
		{"chat lane is not switchable", func(r *store.Run) { r.Kind = runkind.Chat }, false},
		{"running run whose worker lacks credential_switch", func(r *store.Run) { r.Status = "running" }, false},
		{"owner hold on recovery_wait", func(r *store.Run) {
			r.Status = "recovery_wait"
			r.RecoveryWaitCause = pgtype.Text{String: "worker_requeue_exhausted", Valid: true}
		}, false},
		{"credentials_reset hold", func(r *store.Run) {
			r.Status = "paused"
			r.HoldReason = pgtype.Text{String: holdCredentialsReset, Valid: true}
		}, false},
		{"diff-review lane held", func(r *store.Run) {
			r.Status = "running"
			r.ReviewTargetRunID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
		}, false},
		{"provider-bound is excluded entirely", func(r *store.Run) { r.ModelProviderID = pgtype.UUID{Bytes: uuid.New(), Valid: true} }, false},
	}
	for _, c := range cases {
		run := ncRun()
		c.mutate(&run)
		res := e.svc.NextClaimReason(run, snap)
		got := res != nil && res.ManualSwitch
		if got != c.want {
			t.Errorf("%s: ManualSwitch = %v, want %v", c.name, got, c.want)
		}
	}

	t.Run("a running run on a capable worker", func(t *testing.T) {
		e.f.workers = []store.ListWorkersByUserRow{ncWorkerRow(BindModeAuto, capability.CredentialSwitchV1)}
		snap := e.snapshot(t)
		run := ncRun()
		run.Status = "running"
		if res := e.svc.NextClaimReason(run, snap); !res.ManualSwitch {
			t.Error("ManualSwitch = false, want true for a capable holding worker")
		}
	})
	t.Run("a settling paused run", func(t *testing.T) {
		run := ncRun()
		run.Status = "paused"
		run.HoldReason = pgtype.Text{String: "owner_pause", Valid: true}
		if res := e.svc.NextClaimReason(run, snap); !res.ManualSwitch {
			t.Fatal("ManualSwitch = false for a plain owner pause")
		}
		settled := *snap
		settled.settling = map[uuid.UUID]bool{run.ID: true}
		if res := e.svc.NextClaimReason(run, &settled); res.ManualSwitch {
			t.Error("ManualSwitch = true while the settling fence is up")
		}
	})
}

// --- Codex -------------------------------------------------------------------------------------

var (
	cxAliasOne = uuid.MustParse("00000000-0000-0000-0000-0000000000c1")
	cxAliasTwo = uuid.MustParse("00000000-0000-0000-0000-0000000000c2")
	cxAcctOne  = uuid.MustParse("00000000-0000-0000-0000-0000000000e1")
	cxAcctTwo  = uuid.MustParse("00000000-0000-0000-0000-0000000000e2")
)

func cxBuckets(t *testing.T, used float64) []byte {
	t.Helper()
	win := &apitypes.CodexRateLimitWindowDTO{UsedPercent: &used, LimitWindowSeconds: ncPtr(usagereserve.WeeklyWindowSeconds), ResetAfterSeconds: ncPtr(int64(7200))}
	b, err := json.Marshal([]apitypes.CodexRateLimitBucketDTO{{ID: "codex", Primary: win}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func ncPtr[T any](v T) *T { return &v }

func cxAccount(t *testing.T, id uuid.UUID, alias string, used float64, reauth bool) store.GetCodexAccountRateLimitsForUserRow {
	return store.GetCodexAccountRateLimitsForUserRow{
		ProviderAccountID: id, Aliases: []string{alias}, ReauthRequired: reauth,
		Buckets: cxBuckets(t, used), LastSuccessAt: pgconv.Time(autoNow.Add(-time.Minute)),
	}
}

func cxEnv(t *testing.T, used1, used2 float64, thr *int16, mode string) (*ncEnv, *NextClaimSnapshot) {
	e := newNCEnv(t, ncParams())
	e.f.codexMode = mode
	e.f.codexSecrets = []store.ListUserSecretsForKindRow{
		{ID: cxAliasOne, Kind: store.KindCodexAuth, Label: "cx-one"},
		{ID: cxAliasTwo, Kind: store.KindCodexAuth, Label: "cx-two"},
	}
	e.f.codexLinks = []store.ListCodexAliasAccountsForUserRow{
		{UserSecretID: cxAliasOne, ProviderAccountID: pgtype.UUID{Bytes: cxAcctOne, Valid: true}},
		{UserSecretID: cxAliasTwo, ProviderAccountID: pgtype.UUID{Bytes: cxAcctTwo, Valid: true}},
	}
	e.f.codexAccounts = []store.GetCodexAccountRateLimitsForUserRow{
		cxAccount(t, cxAcctOne, "cx-one", used1, false), cxAccount(t, cxAcctTwo, "cx-two", used2, false),
	}
	if thr != nil {
		e.f.reserve = ncThreshold(*thr)
	}
	caps := []string{capability.CodexCredentialSwitchV1, capability.CodexSwitchStoppedV1}
	w := ncWorkerRow(BindModeDefault, caps...)
	w.SnapshotRegisterNonce = pgtype.Text{String: "n1", Valid: true}
	w.CredentialSwitchApplyNonce = pgtype.Text{String: "n1", Valid: true}
	e.f.workers = []store.ListWorkersByUserRow{w}
	return e, e.snapshot(t)
}

func cxRun() store.Run {
	return store.Run{
		ID: uuid.New(), UserID: ncOwner, Kind: "issue", Harness: harnessCodex, Status: "running",
		WorkerID:      pgtype.UUID{Bytes: ncWorker, Valid: true},
		CodexSecretID: pgtype.UUID{Bytes: cxAliasOne, Valid: true},
		CodexAuthMode: pgtype.Text{String: codexAuthModeSubscription, Valid: true},
	}
}

func TestNextClaimReasonCodexAccountBound(t *testing.T) {
	t.Run("live run: bound, another account has room", func(t *testing.T) {
		e, snap := cxEnv(t, 95, 30, nil, "default")
		res := e.svc.NextClaimReason(cxRun(), snap)
		if res.Reason != NextClaimAccountBound {
			t.Fatalf("reason = %q, want account_bound", res.Reason)
		}
		if res.Alternative == nil || res.Alternative.ID != cxAliasTwo || res.Alternative.Label != "cx-two" || res.Alternative.Kind != "codex_auth" {
			t.Errorf("alternative = %+v, want cx-two", res.Alternative)
		}
		kinds := map[string]bool{}
		for _, f := range res.FutureRuns {
			kinds[f.Kind] = true
		}
		if !kinds[FutureFixPoolOn] || !kinds[FutureFixCodexModeAuto] {
			t.Errorf("future runs = %+v, want pool_on and codex_mode_auto", res.FutureRuns)
		}
		if !res.ManualSwitch {
			t.Error("ManualSwitch = false for a live run on a capable worker")
		}
	})
	t.Run("pooled alternative in codex auto mode needs no future fix", func(t *testing.T) {
		e, _ := cxEnv(t, 95, 30, nil, codexAccountModeAuto)
		e.f.codexPool = []store.ListCodexPoolAliasesRow{{AliasID: cxAliasTwo}}
		snap := e.snapshot(t)
		if res := e.svc.NextClaimReason(cxRun(), snap); len(res.FutureRuns) != 0 {
			t.Errorf("future runs = %+v, want none", res.FutureRuns)
		}
	})
	t.Run("no other account with room names no alternative", func(t *testing.T) {
		e, snap := cxEnv(t, 95, 85, i16p(70), "default")
		res := e.svc.NextClaimReason(cxRun(), snap)
		if res.Reason != NextClaimAccountBound || res.Alternative != nil {
			t.Errorf("got %+v, want account_bound with no alternative (the other account is over the 70 limit)", res)
		}
	})
	t.Run("a reauth-flagged account is not an alternative", func(t *testing.T) {
		e, _ := cxEnv(t, 95, 30, nil, "default")
		e.f.codexAccounts[1] = cxAccount(t, cxAcctTwo, "cx-two", 30, true)
		if res := e.svc.NextClaimReason(cxRun(), e.snapshot(t)); res.Alternative != nil {
			t.Errorf("alternative = %+v, want none", res.Alternative)
		}
	})
	t.Run("a stale reading is not an alternative", func(t *testing.T) {
		e, _ := cxEnv(t, 95, 30, nil, "default")
		e.f.codexAccounts[1].LastSuccessAt = pgconv.Time(autoNow.Add(-3 * time.Hour))
		if res := e.svc.NextClaimReason(cxRun(), e.snapshot(t)); res.Alternative != nil {
			t.Errorf("alternative = %+v, want none", res.Alternative)
		}
	})
	t.Run("disabled current alias with a sibling on the same account never offers that account", func(t *testing.T) {
		e, _ := cxEnv(t, 95, 30, nil, "default")
		sibling := uuid.MustParse("00000000-0000-0000-0000-0000000c1a03")
		e.f.codexSecrets = append(e.f.codexSecrets, store.ListUserSecretsForKindRow{ID: sibling, Kind: store.KindCodexAuth, Label: "cx-one-sibling"})
		e.f.codexLinks = append(e.f.codexLinks, store.ListCodexAliasAccountsForUserRow{UserSecretID: sibling, ProviderAccountID: pgtype.UUID{Bytes: cxAcctOne, Valid: true}})
		// cx-one (the run's alias) is disabled: only the sibling stays listed on account one, whose
		// reading is the lowest here so it would win if the run's own account were not excluded.
		e.f.codexAccounts[0] = cxAccount(t, cxAcctOne, "cx-one-sibling", 5, false)
		res := e.svc.NextClaimReason(cxRun(), e.snapshot(t))
		if res.Alternative == nil || res.Alternative.ID != cxAliasTwo {
			t.Errorf("alternative = %+v, want cx-two, never the run's own account", res.Alternative)
		}
	})
	t.Run("run alias with no resolvable account is unknown", func(t *testing.T) {
		e, _ := cxEnv(t, 95, 30, nil, "default")
		e.f.codexLinks = e.f.codexLinks[1:]
		if res := e.svc.NextClaimReason(cxRun(), e.snapshot(t)); res.Reason != NextClaimUnknown || res.Alternative != nil {
			t.Errorf("got %+v, want unknown with no alternative", res)
		}
	})
	t.Run("parked run cannot be switched manually", func(t *testing.T) {
		e, snap := cxEnv(t, 95, 30, nil, "default")
		run := cxRun()
		run.Status = "limit_wait"
		res := e.svc.NextClaimReason(run, snap)
		if res.Reason != NextClaimAccountBound || res.ManualSwitch {
			t.Errorf("got %+v, want account_bound with ManualSwitch false (Codex switches only live runs)", res)
		}
	})
	t.Run("pending switch", func(t *testing.T) {
		e, snap := cxEnv(t, 95, 30, nil, "default")
		run := cxRun()
		run.CredentialSwitchRequestedAt = pgconv.Time(autoNow)
		if res := e.svc.NextClaimReason(run, snap); res.Reason != NextClaimSwitchPending {
			t.Errorf("reason = %q, want switch_pending", res.Reason)
		}
	})
	t.Run("unsupported worker", func(t *testing.T) {
		e, _ := cxEnv(t, 95, 30, nil, "default")
		e.f.workers[0].ProtocolCapabilities = nil
		res := e.svc.NextClaimReason(cxRun(), e.snapshot(t))
		if res.Reason != NextClaimAccountBound || res.ManualSwitch {
			t.Errorf("got %+v, want account_bound with ManualSwitch false", res)
		}
	})
	t.Run("codex reads unavailable is unknown", func(t *testing.T) {
		e, snap := cxEnv(t, 95, 30, nil, "default")
		off := *snap
		off.codexAvailable = false
		if res := e.svc.NextClaimReason(cxRun(), &off); res.Reason != NextClaimUnknown {
			t.Errorf("reason = %q, want unknown", res.Reason)
		}
	})
	t.Run("raw weekly readings by credential key", func(t *testing.T) {
		_, snap := cxEnv(t, 95, 30, nil, "default")
		w, ok := snap.Weekly("codex:" + cxAcctTwo.String())
		if !ok || !w.OK || w.Pct != 30 || len(w.Windows) != 1 || w.Windows[0].Slot != "primary" || w.Windows[0].BucketID != "codex" {
			t.Errorf("Weekly(codex) = %+v, %v", w, ok)
		}
		if _, ok := snap.Weekly("codex:" + uuid.New().String()); ok {
			t.Error("Weekly of an unknown account reported ok")
		}
		if _, ok := snap.Weekly("nonsense"); ok {
			t.Error("Weekly of a malformed key reported ok")
		}
	})
}

func TestNextClaimWeeklyAnthropicAndReserveEnforced(t *testing.T) {
	e := newNCEnv(t, ncParams())
	e.setTokens(ncTok{id: ncTokA, label: "tok-a", pooled: true, weekly: 61}, ncTok{id: ncTokB, label: "tok-b", noReading: true})
	e.f.workers = []store.ListWorkersByUserRow{ncWorkerRow(BindModeAuto)}
	snap := e.snapshot(t) // no reserve threshold: raw readings are kept anyway
	w, ok := snap.Weekly("anthropic:" + ncTokA.String())
	if !ok || !w.OK || w.Pct != 61 || w.ResetsAt == nil {
		t.Errorf("Weekly(A) = %+v, %v, want a kept raw reading without any threshold", w, ok)
	}
	if w, ok := snap.Weekly("anthropic:" + ncTokB.String()); !ok || w.OK {
		t.Errorf("Weekly(B) = %+v, %v, want a present-but-unusable reading", w, ok)
	}
	if snap.ReserveEnforced(ncRun()) {
		t.Error("ReserveEnforced = true with no threshold")
	}

	e.f.reserve = ncThreshold(70)
	snap = e.snapshot(t)
	if !snap.ReserveEnforced(ncRun()) {
		t.Error("ReserveEnforced = false for a never-started run under a threshold")
	}
	chat := ncRun()
	chat.Kind = runkind.Chat
	if snap.ReserveEnforced(chat) {
		t.Error("ReserveEnforced = true for a chat run")
	}
	started := ncRun()
	started.FirstStartedAt = pgconv.Time(autoNow.Add(-time.Hour))
	if snap.ReserveEnforced(started) {
		t.Error("ReserveEnforced = true for a started run while pausing is off")
	}
}

// --- PreviewWait -------------------------------------------------------------------------------

func TestPreviewWait(t *testing.T) {
	reset := autoNow.Add(2 * time.Hour)
	base := func() *NextClaimSnapshot {
		a := ncTokA
		return &NextClaimSnapshot{
			userID: ncOwner, now: autoNow, policy: autoParams().Autoselect, maxWaits: 3, maxPark: 8 * 24 * time.Hour,
			cands: []autoselect.Candidate{{SecretID: a, Label: "tok-a", AutoEligible: true}},
			reserve: &reserveSnap{userID: ncOwner, threshold: i16p(70), pauseRunning: true,
				weeklyClaude: map[uuid.UUID]usagereserve.WeeklyReading{a: {OK: true, Pct: 80, ResetsAt: &reset}}},
			workers: map[uuid.UUID]store.Worker{ncWorker: {ID: ncWorker, UserID: ncOwner, AnthropicBindMode: BindModeAuto}},
		}
	}
	running := func() store.Run {
		r := ncRun()
		r.Status = "running"
		r.LimitDeadSecretID = pgtype.UUID{}
		r.RetryNotBefore, r.LimitResetsAt = pgtype.Timestamptz{}, pgtype.Timestamptz{}
		r.FirstStartedAt = pgconv.Time(autoNow.Add(-time.Hour))
		return r
	}
	parked := func(count int32, retry, resetAt time.Time) store.Run {
		r := ncRun()
		r.LimitWaitCount = count
		r.RetryNotBefore, r.LimitResetsAt = pgconv.Time(retry), pgconv.Time(resetAt)
		return r
	}
	cases := []struct {
		name   string
		snap   func(*NextClaimSnapshot)
		run    store.Run
		want   string
		mutate func(*store.Run)
	}{
		{name: "weekly reserve pause", run: func() store.Run {
			r := ncRun()
			r.Status = "paused"
			r.HoldReason = pgtype.Text{String: holdWeeklyReserve, Valid: true}
			return r
		}(), want: WaitReservePause},
		{name: "running run, reserve enforced with pausing on", run: running(), want: WaitReservePause},
		{name: "running chat run is exempt from the reserve", run: func() store.Run { r := running(); r.Kind = runkind.Chat; return r }(), want: WaitWaits},
		{name: "running run, pausing off, reads the park decision", snap: func(n *NextClaimSnapshot) { n.reserve.pauseRunning = false }, run: running(), want: WaitWaits},
		{name: "parked at count == MaxWaits with the retry at the reset still waits", run: parked(3, reset, reset), want: WaitWaits},
		{name: "parked at count == MaxWaits, earlier retry, no switch possible, fails", run: parked(3, reset.Add(-time.Hour), reset), want: WaitFails},
		{name: "parked past the budget fails", run: parked(4, reset.Add(-time.Hour), reset), want: WaitFails},
		{name: "parked with the stored retry at the reset waits", run: parked(1, reset, reset), want: WaitWaits},
		{name: "parked with the stored retry past the reset waits", run: parked(1, reset.Add(time.Hour), reset), want: WaitWaits},
		{name: "parked with no known reset is unknown", run: func() store.Run { r := parked(1, reset, reset); r.LimitResetsAt = pgtype.Timestamptz{}; return r }(), want: WaitUnknown},
		{name: "parked that opted out of waiting fails", run: func() store.Run { r := parked(1, reset.Add(-time.Hour), reset); r.WaitOnLimit = false; return r }(), want: WaitFails},
		{name: "parked further out than the maximum park fails", snap: func(n *NextClaimSnapshot) { n.maxPark = time.Hour }, run: parked(1, reset.Add(-time.Hour), reset), want: WaitFails},
		{name: "parked retry lowered to a pooled alternative waits", snap: func(n *NextClaimSnapshot) {
			b := ncTokB
			pct := int16(10)
			synced := autoNow.Add(-time.Minute)
			n.cands = append(n.cands, autoselect.Candidate{SecretID: b, Label: "tok-b", AutoEligible: true, HasReading: true, SevenDayPct: &pct, SyncedAt: &synced})
			n.maxPark = time.Minute // would fail a recomputation; the earlier retry is the decision
		}, run: parked(1, autoNow.Add(10*time.Minute), reset), want: WaitWaits},
		{name: "not parked, no reserve, a fresh reading with a reset waits", snap: func(n *NextClaimSnapshot) { n.reserve.threshold = nil }, run: func() store.Run {
			r := running()
			r.AnthropicSecretID = pgtype.UUID{Bytes: ncTokA, Valid: true}
			return r
		}(), want: WaitWaits},
		{name: "not parked, opted out of waiting, fails", snap: func(n *NextClaimSnapshot) { n.reserve.threshold = nil }, run: func() store.Run { r := running(); r.WaitOnLimit = false; return r }(), want: WaitFails},
		{name: "not parked, retry budget spent, fails", snap: func(n *NextClaimSnapshot) { n.reserve.threshold = nil }, run: func() store.Run { r := running(); r.LimitWaitCount = 3; return r }(), want: WaitFails},
		{name: "not parked, reset beyond the maximum park, fails", snap: func(n *NextClaimSnapshot) { n.reserve.threshold = nil; n.maxPark = time.Minute }, run: running(), want: WaitFails},
		{name: "not parked, no usable weekly reset, unknown", snap: func(n *NextClaimSnapshot) {
			n.reserve.threshold = nil
			n.reserve.weeklyClaude[ncTokA] = usagereserve.WeeklyReading{}
		}, run: running(), want: WaitUnknown},
	}
	for _, c := range cases {
		snap := base()
		if c.snap != nil {
			c.snap(snap)
		}
		if got := snap.PreviewWait(c.run); got != c.want {
			t.Errorf("%s: PreviewWait = %q, want %q", c.name, got, c.want)
		}
	}
}
