package weeklylimit

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Tests of the M2 presence condition (PRD #4123 examples 1 and 2): a non-terminal run on a credential
// near or at its effective weekly limit carries a block even though it is not parked, while a margin of
// 0, a stale reading, an unbound queued run and a terminal run carry none.

// nearStore is example 1's owner: token A (pooled) at pctA and B (not pooled) at 20%, the worker on
// auto, no reserve, the given margin.
func nearStore(pctA int16, margin int16) *fakeStore {
	f := &fakeStore{}
	for _, c := range []struct {
		id     uuid.UUID
		label  string
		pooled bool
		pct    int16
	}{{tokA, "work", true, pctA}, {tokB, "spare", false, 20}} {
		cand, res := tok(c.id, c.label, c.pooled, c.pct)
		f.cands, f.tokens = append(f.cands, cand), append(f.tokens, res)
	}
	f.workers = []store.ListWorkersByUserRow{{
		ID: wkr, UserID: owner, Name: "w1", AnthropicBindMode: "auto",
		LastHeartbeatAt: pgconv.Time(now.Add(-time.Second)),
	}}
	f.reserve = store.GetUserUsageReserveRow{WeeklyWarningMarginPts: margin}
	return f
}

// runningRun is a live Claude run on token A, on the worker, with no override.
func runningRun() store.Run {
	return store.Run{
		ID: uuid.New(), UserID: owner, Kind: "issue", Harness: "claude", Status: "running",
		WorkerID: pgtype.UUID{Bytes: wkr, Valid: true}, WaitOnLimit: true,
		AnthropicSecretID: pgtype.UUID{Bytes: tokA, Valid: true},
	}
}

func blockFor(t *testing.T, f *fakeStore, runs ...store.Run) map[uuid.UUID]*apitypes.WeeklyLimitDTO {
	t.Helper()
	return newLoader(t, f).ForRuns(context.Background(), owner, runs)
}

func fixKinds(fixes []apitypes.WeeklyLimitFixDTO) map[string]string {
	out := map[string]string{}
	for _, f := range fixes {
		out[f.Kind] = f.Command
	}
	return out
}

func TestExample1RunningRunOnANearTokenWithHeadroomOutsideThePool(t *testing.T) {
	run := runningRun()
	got := blockFor(t, nearStore(86, 15), run)[run.ID]
	if got == nil {
		t.Fatal("no block for a running run on a token at 86% with margin 15")
	}
	if got.Reason != workersvc.NextClaimHeadroomOutside || got.AlternativeLabel != "spare" {
		t.Fatalf("reason/alternative = %q %q; want headroom_outside_pool and spare", got.Reason, got.AlternativeLabel)
	}
	if got.CredentialKey != "anthropic:"+tokA.String() || got.CredentialLabel != "work" || got.Harness != "claude" {
		t.Fatalf("key/label/harness = %q %q %q", got.CredentialKey, got.CredentialLabel, got.Harness)
	}
	if got.Pct == nil || *got.Pct != 86 || got.EffectiveLimit != 100 || got.ResetsAt == nil {
		t.Fatalf("pct/limit/resets = %v %v %v; want 86, 100, a reset", got.Pct, got.EffectiveLimit, got.ResetsAt)
	}
	if want := "uzi token pool --on --kind anthropic -- spare"; fixKinds(got.Fix)[FixPoolOn] != want {
		t.Fatalf("run fix = %+v; want pool_on %q", got.Fix, want)
	}
	if got.Tier != apitypes.WeeklyLimitTierWarning {
		t.Fatalf("tier = %q; want warning (the run itself has a fix)", got.Tier)
	}
	if got.Wait == "" {
		t.Fatalf("wait is empty")
	}
}

func TestNearBoundaryIsMarginBelowTheEffectiveLimit(t *testing.T) {
	run := runningRun()
	for _, c := range []struct {
		name   string
		pct    int16
		margin int16
		thr    *int16
		want   bool
	}{
		{"just below the margin", 84, 15, nil, false},
		{"exactly limit minus margin", 85, 15, nil, true},
		{"at the limit", 100, 15, nil, true},
		{"reserve moves the limit: below", 64, 15, i16p(80), false},
		{"reserve moves the limit: near", 65, 15, i16p(80), true},
		{"reserve moves the limit: past it", 90, 15, i16p(80), true},
		{"margin 0, even at the limit, shows no early block", 100, 0, nil, false},
		{"margin 0 with a reserve", 85, 0, i16p(80), false},
		{"margin 50 reaches half", 50, 50, nil, true},
	} {
		f := nearStore(c.pct, c.margin)
		if c.thr != nil {
			f.reserve.WeeklyReservePct = pgtype.Int2{Int16: *c.thr, Valid: true}
		}
		got := blockFor(t, f, run)[run.ID]
		if (got != nil) != c.want {
			t.Errorf("%s: block present = %v, want %v", c.name, got != nil, c.want)
			continue
		}
		if got != nil && c.thr != nil && got.EffectiveLimit != float64(*c.thr) {
			t.Errorf("%s: effective_limit = %v, want the reserve %d", c.name, got.EffectiveLimit, *c.thr)
		}
	}
}

func i16p(v int16) *int16 { return &v }

func TestMarginZeroKeepsTheParkBlock(t *testing.T) {
	parked := parkedRun("seven_day")
	got := blockFor(t, nearStore(96, 0), parked)[parked.ID]
	if got == nil {
		t.Fatal("margin 0 removed the park explanation; only the early warnings are off")
	}
	// The reserve pause is also a park explanation.
	pause := parkedRun("")
	pause.Status = "paused"
	pause.HoldReason = pgtype.Text{String: "weekly_reserve", Valid: true}
	pause.LimitResetsAt, pause.RetryNotBefore, pause.RateLimitType = pgtype.Timestamptz{}, pgtype.Timestamptz{}, pgtype.Text{}
	if blockFor(t, nearStore(96, 0), pause)[pause.ID] == nil {
		t.Fatal("margin 0 removed the weekly-reserve pause block")
	}
}

func TestStaleReadingCarriesNoNearBlock(t *testing.T) {
	f := nearStore(99, 15)
	old := now.Add(-time.Hour) // far past three 5-minute poll intervals
	f.cands[0].SyncedAt = pgconv.Time(old)
	f.tokens[0].SyncedAt = pgconv.Time(old)
	run := runningRun()
	if got := blockFor(t, f, run)[run.ID]; got != nil {
		t.Fatalf("a stale reading produced a block: %+v", got)
	}
	// A reading with no utilization is unknown as well.
	f = nearStore(99, 15)
	f.tokens[0].SevenDayPct = pgtype.Int2{}
	if got := blockFor(t, f, run)[run.ID]; got != nil {
		t.Fatalf("a utilization-less reading produced a block: %+v", got)
	}
}

func TestNoNearBlockForUnboundTerminalOrExcludedRuns(t *testing.T) {
	unbound := runningRun()
	unbound.Status, unbound.AnthropicSecretID = "queued", pgtype.UUID{}
	done := runningRun()
	done.Status = "completed"
	failed := runningRun()
	failed.Status = "failed"
	cancelled := runningRun()
	cancelled.Status = "cancelled"
	provider := runningRun()
	provider.ModelProviderID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	apiKey := runningRun()
	apiKey.Harness = "codex"
	apiKey.CodexSecretID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	apiKey.CodexAuthMode = pgtype.Text{String: "api_key", Valid: true}
	codexUnbound := runningRun()
	codexUnbound.Harness = "codex"
	codexUnbound.AnthropicSecretID = pgtype.UUID{}

	f := nearStore(99, 15)
	l := newLoader(t, f)
	got := l.ForRuns(context.Background(), owner, []store.Run{unbound, done, failed, cancelled, provider, apiKey, codexUnbound})
	if len(got) != 0 {
		t.Fatalf("blocks for runs that cannot carry one: %+v", got)
	}
	if f.reads.Load() != 0 {
		t.Fatalf("those runs caused %d store reads; want none", f.reads.Load())
	}
}

func TestNearBlockOnARunParkedOnAnotherWindowDoesNotPromiseAResume(t *testing.T) {
	run := parkedRun("five_hour")
	got := blockFor(t, nearStore(96, 15), run)[run.ID]
	if got == nil {
		t.Fatal("a five_hour-parked run on a near-limit credential carries no early block")
	}
	if got.Wait != workersvc.WaitUnknown {
		t.Fatalf("wait = %q; the stored five_hour stamps say nothing about the weekly window", got.Wait)
	}
}

func TestNearBlockAndParkBlockAreOneBlockPerRun(t *testing.T) {
	// A seven_day-parked run on a near token is one block (the park case wins nothing extra).
	run := parkedRun("seven_day")
	got := blockFor(t, nearStore(96, 15), run)
	if len(got) != 1 || got[run.ID] == nil {
		t.Fatalf("blocks = %+v; want exactly the run's one", got)
	}
}

func TestTwoRunsOnOneCredentialDifferInTheirFixes(t *testing.T) {
	auto := runningRun()
	pinned := runningRun()
	pinned.CredentialOverrideMode = pgtype.Text{String: "pinned", Valid: true}
	pinned.CredentialOverrideSecretID = pgtype.UUID{Bytes: tokA, Valid: true}
	got := blockFor(t, nearStore(90, 15), auto, pinned)
	a, p := got[auto.ID], got[pinned.ID]
	if a == nil || p == nil {
		t.Fatalf("blocks = %+v", got)
	}
	if a.CredentialKey != p.CredentialKey {
		t.Fatalf("keys differ: %q vs %q", a.CredentialKey, p.CredentialKey)
	}
	if a.Reason != workersvc.NextClaimHeadroomOutside || p.Reason != workersvc.NextClaimPinned {
		t.Fatalf("reasons = %q / %q; want headroom_outside_pool / pinned", a.Reason, p.Reason)
	}
	if _, ok := fixKinds(a.Fix)[FixPoolOn]; !ok {
		t.Errorf("auto run lacks pool_on: %+v", a.Fix)
	}
	if _, ok := fixKinds(p.Fix)[FixPoolOn]; ok {
		t.Errorf("pinned run offered pool_on, which cannot move it: %+v", p.Fix)
	}
}

// codexBuckets is a one-bucket Codex reading with a single weekly window.
func codexBuckets(pct float64, reset time.Time) []byte {
	secs, at := int64(604800), reset.Unix()
	b, _ := json.Marshal([]apitypes.CodexRateLimitBucketDTO{{ID: "codex", Primary: &apitypes.CodexRateLimitWindowDTO{
		UsedPercent: &pct, LimitWindowSeconds: &secs, ResetAt: &at,
	}}})
	return b
}

// example2 is two linked Codex accounts, C1 at 88% and C2 at 10%, a capable worker and margin 15.
func example2() (f *fakeStore, aliasC1, aliasC2, acctC1, acctC2 uuid.UUID) {
	aliasC1, aliasC2, acctC1, acctC2 = uuid.New(), uuid.New(), uuid.New(), uuid.New()
	f = nearStore(10, 15)
	codexCapableWorker(f)
	f.codex = &codexData{
		secrets: []store.ListUserSecretsForKindRow{{ID: aliasC1, Kind: "codex_auth", Label: "c1"}, {ID: aliasC2, Kind: "codex_auth", Label: "c2"}},
		links: []store.ListCodexAliasAccountsForUserRow{
			{UserSecretID: aliasC1, ProviderAccountID: pgtype.UUID{Bytes: acctC1, Valid: true}},
			{UserSecretID: aliasC2, ProviderAccountID: pgtype.UUID{Bytes: acctC2, Valid: true}},
		},
		accounts: []store.GetCodexAccountRateLimitsForUserRow{
			{ProviderAccountID: acctC1, Aliases: []string{"c1"}, Buckets: codexBuckets(88, now.Add(48*time.Hour)), LastSuccessAt: pgconv.Time(now.Add(-time.Minute))},
			{ProviderAccountID: acctC2, Aliases: []string{"c2"}, Buckets: codexBuckets(10, now.Add(48*time.Hour)), LastSuccessAt: pgconv.Time(now.Add(-time.Minute))},
		},
	}
	return
}

func liveCodexRun(alias uuid.UUID) store.Run {
	r := runningRun()
	r.Harness = "codex"
	r.AnthropicSecretID = pgtype.UUID{}
	r.CodexSecretID = pgtype.UUID{Bytes: alias, Valid: true}
	return r
}

func TestExample2RunningCodexRunIsAccountBoundWithASwitchOffer(t *testing.T) {
	f, aliasC1, aliasC2, acctC1, _ := example2()
	run := liveCodexRun(aliasC1)
	got := blockFor(t, f, run)[run.ID]
	if got == nil {
		t.Fatal("no block for a running Codex run on an account at 88%")
	}
	if got.Reason != workersvc.NextClaimAccountBound || got.CredentialKey != "codex:"+acctC1.String() || got.CredentialLabel != "c1" || got.AlternativeLabel != "c2" {
		t.Fatalf("block = %+v", got)
	}
	if got.Pct == nil || *got.Pct != 88 {
		t.Fatalf("pct = %v, want 88", got.Pct)
	}
	st := fixOf(got, FixSetToken)
	if want := "uzi run set-token " + run.ID.String() + " -- c2"; st == nil || st.Command != want || st.TargetID != aliasC2.String() {
		t.Fatalf("set_token = %+v; want command %q", st, want)
	}
	future := fixKinds(got.FutureRuns)
	if future[FixPoolOn] != "uzi token pool --on --kind codex -- c2" || future[FixAutoSelect] != "uzi settings codex-mode auto" {
		t.Fatalf("future_runs = %+v; want the codex pool_on and codex-mode auto advice", got.FutureRuns)
	}
	if got.Tier != apitypes.WeeklyLimitTierWarning {
		t.Fatalf("tier = %q", got.Tier)
	}

	// The same account with the run parked on its weekly window: no live switch is offered.
	parked := liveCodexRun(aliasC1)
	parked.Status = "limit_wait"
	parked.RateLimitType = pgtype.Text{String: "seven_day", Valid: true}
	parked.LimitResetsAt = pgconv.Time(now.Add(48 * time.Hour))
	pg := blockFor(t, f, parked)[parked.ID]
	if pg == nil || fixOf(pg, FixSetToken) != nil {
		t.Fatalf("parked codex block = %+v; want a block without set_token", pg)
	}
	if pg.Reason != workersvc.NextClaimAccountBound {
		t.Fatalf("parked reason = %q", pg.Reason)
	}
}

func TestCodexNearUsesTheHighestWeeklyWindow(t *testing.T) {
	f, aliasC1, _, _, _ := example2()
	secs, reset := int64(604800), now.Add(48*time.Hour).Unix()
	lo, hi := 30.0, 92.0
	multi, _ := json.Marshal([]apitypes.CodexRateLimitBucketDTO{
		{ID: "a", Primary: &apitypes.CodexRateLimitWindowDTO{UsedPercent: &lo, LimitWindowSeconds: &secs, ResetAt: &reset}},
		{ID: "b", Secondary: &apitypes.CodexRateLimitWindowDTO{UsedPercent: &hi, LimitWindowSeconds: &secs, ResetAt: &reset}},
	})
	f.codex.accounts[0].Buckets = multi
	run := liveCodexRun(aliasC1)
	got := blockFor(t, f, run)[run.ID]
	if got == nil || got.Pct == nil || *got.Pct != 92 {
		t.Fatalf("block = %+v; want the 92%% window to decide", got)
	}
	// Margin 0: no early block for the same reading.
	f.reserve.WeeklyWarningMarginPts = 0
	if got := blockFor(t, f, run)[run.ID]; got != nil {
		t.Fatalf("margin 0 still produced a block: %+v", got)
	}
}

func TestCodexStaleOrInvalidReadingCarriesNoBlock(t *testing.T) {
	f, aliasC1, _, _, _ := example2()
	f.codex.accounts[0].LastSuccessAt = pgconv.Time(now.Add(-time.Hour))
	run := liveCodexRun(aliasC1)
	if got := blockFor(t, f, run)[run.ID]; got != nil {
		t.Fatalf("a stale Codex reading produced a block: %+v", got)
	}
}

func TestTwoAliasesOfOneCodexAccountShareACredentialKey(t *testing.T) {
	f, aliasC1, _, acctC1, _ := example2()
	alias2 := uuid.New()
	f.codex.secrets = append(f.codex.secrets, store.ListUserSecretsForKindRow{ID: alias2, Kind: "codex_auth", Label: "c1-second"})
	f.codex.links = append(f.codex.links, store.ListCodexAliasAccountsForUserRow{UserSecretID: alias2, ProviderAccountID: pgtype.UUID{Bytes: acctC1, Valid: true}})
	f.codex.accounts[0].Aliases = []string{"c1", "c1-second"}
	r1, r2 := liveCodexRun(aliasC1), liveCodexRun(alias2)
	got := blockFor(t, f, r1, r2)
	if got[r1.ID] == nil || got[r2.ID] == nil {
		t.Fatalf("blocks = %+v", got)
	}
	if got[r1.ID].CredentialKey != got[r2.ID].CredentialKey || got[r1.ID].CredentialKey != "codex:"+acctC1.String() {
		t.Fatalf("keys = %q / %q; want one shared codex:<account> key", got[r1.ID].CredentialKey, got[r2.ID].CredentialKey)
	}
	if got[r1.ID].CredentialLabel == got[r2.ID].CredentialLabel {
		t.Fatalf("the two aliases show one label %q; each run names its own alias", got[r1.ID].CredentialLabel)
	}
}

func TestSameLabelOnDifferentAccountsKeepsDistinctKeys(t *testing.T) {
	// An Anthropic token and a Codex alias share the label "work", and two Codex aliases of different
	// accounts cannot be told apart by label either: the key is the identity, never the label.
	f, aliasC1, aliasC2, acctC1, acctC2 := example2()
	f.codex.secrets[0].Label, f.codex.secrets[1].Label = "work", "work"
	f.codex.accounts[0].Aliases, f.codex.accounts[1].Aliases = []string{"work"}, []string{"work"}
	// Token A and both accounts near, so each run carries a block.
	f.cands[0].SevenDayPct, f.tokens[0].SevenDayPct = pgtype.Int2{Int16: 90, Valid: true}, pgtype.Int2{Int16: 90, Valid: true}
	f.codex.accounts[1].Buckets = codexBuckets(90, now.Add(48*time.Hour))
	claude := runningRun() // token A is labelled "work" as well
	c1, c2 := liveCodexRun(aliasC1), liveCodexRun(aliasC2)
	got := blockFor(t, f, claude, c1, c2)
	if got[claude.ID] == nil || got[c1.ID] == nil || got[c2.ID] == nil {
		t.Fatalf("blocks = %+v", got)
	}
	keys := map[string]bool{}
	for _, id := range []uuid.UUID{claude.ID, c1.ID, c2.ID} {
		if got[id].CredentialLabel != "work" {
			t.Fatalf("label = %q, want work on all three", got[id].CredentialLabel)
		}
		keys[got[id].CredentialKey] = true
	}
	if len(keys) != 3 || !keys["anthropic:"+tokA.String()] || !keys["codex:"+acctC1.String()] || !keys["codex:"+acctC2.String()] {
		t.Fatalf("keys = %v; want three distinct identity keys", keys)
	}
}

// A multi-run list with the early warnings off and nothing parked loads exactly one snapshot and
// yields no block.
func TestMarginOffListWithNothingParkedLoadsOneSnapshot(t *testing.T) {
	f := nearStore(96, 0)
	a, b, c := runningRun(), runningRun(), runningRun()
	got := blockFor(t, f, a, b, c)
	if len(got) != 0 {
		t.Fatalf("blocks = %+v; want none with margin 0", got)
	}
	if f.candCnt.Load() != 1 {
		t.Fatalf("candidate reads = %d; want exactly one snapshot", f.candCnt.Load())
	}
}

// Chat is not bindable (PRD #104 D1): its claim always opens the owner's default token, so the
// worker's auto binding and the pool never move it. Its block must say not_auto and offer no run
// fix that pools a token or re-binds the worker (those cannot help the chat and change other
// work), with the worker on auto and on a fixed binding alike.
func TestChatRunNeverGetsPoolOrWorkerAdvice(t *testing.T) {
	for _, mode := range []string{"auto", "default"} {
		t.Run("worker_"+mode, func(t *testing.T) {
			f := nearStore(86, 15)
			f.workers[0].AnthropicBindMode = mode
			run := runningRun()
			run.Kind = "chat"
			got := blockFor(t, f, run)[run.ID]
			if got == nil {
				t.Fatal("no block for a chat on a near-limit default token")
			}
			if got.Reason != workersvc.NextClaimNotAuto {
				t.Fatalf("reason = %q; want not_auto (chat spends the owner's default token)", got.Reason)
			}
			kinds := fixKinds(got.Fix)
			if _, ok := kinds[FixPoolOn]; ok {
				t.Fatalf("chat got a pool_on run fix: %+v", got.Fix)
			}
			for _, fx := range got.Fix {
				if fx.Kind == FixAutoSelect && fx.TargetID == wkr.String() {
					t.Fatalf("chat got a worker re-bind run fix: %+v", got.Fix)
				}
			}
			for _, fx := range got.FutureRuns {
				if fx.Kind == FixAutoSelect && fx.TargetID == wkr.String() {
					t.Fatalf("chat got a worker re-bind future fix: %+v", got.FutureRuns)
				}
			}
		})
	}
}
