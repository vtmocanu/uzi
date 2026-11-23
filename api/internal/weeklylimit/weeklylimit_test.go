package weeklylimit

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// fakeStore serves exactly the reads LoadNextClaimSnapshot makes and counts them. It embeds the
// workersvc.Store interface nil, so any read the loader makes beyond these panics: an unexpected
// read is a test failure, not a silent pass.
type fakeStore struct {
	workersvc.Store
	cands   []store.ListAutoSelectCandidatesRow
	tokens  []store.ListReserveClaudeTokensRow
	workers []store.ListWorkersByUserRow
	codex   *codexData
	reserve store.GetUserUsageReserveRow
	reads   atomic.Int64
	candCnt atomic.Int64
}

func (f *fakeStore) ListAutoSelectCandidates(context.Context, uuid.UUID) ([]store.ListAutoSelectCandidatesRow, error) {
	f.reads.Add(1)
	f.candCnt.Add(1)
	return f.cands, nil
}
func (f *fakeStore) GetUserUsageReserve(context.Context, uuid.UUID) (store.GetUserUsageReserveRow, error) {
	f.reads.Add(1)
	return f.reserve, nil
}
func (f *fakeStore) GetUserJudgeAnthropicBinding(context.Context, uuid.UUID) (store.GetUserJudgeAnthropicBindingRow, error) {
	f.reads.Add(1)
	return store.GetUserJudgeAnthropicBindingRow{JudgeAnthropicBindMode: "default"}, nil
}
func (f *fakeStore) ListReserveClaudeTokens(context.Context, uuid.UUID) ([]store.ListReserveClaudeTokensRow, error) {
	f.reads.Add(1)
	return f.tokens, nil
}
func (f *fakeStore) ListReserveCodexAliases(context.Context, uuid.UUID) ([]store.ListReserveCodexAliasesRow, error) {
	f.reads.Add(1)
	return nil, nil
}
func (f *fakeStore) CredentialSwitchHoldSettling(context.Context, store.CredentialSwitchHoldSettlingParams) (bool, error) {
	f.reads.Add(1)
	return false, nil
}

// codexData is the optional Codex read surface; a fakeStore without it reads as Codex unavailable only
// if these methods are absent, so they always answer (empty when codex is nil).
type codexData struct {
	secrets  []store.ListUserSecretsForKindRow
	links    []store.ListCodexAliasAccountsForUserRow
	accounts []store.GetCodexAccountRateLimitsForUserRow
}

func (f *fakeStore) cdx() codexData {
	f.reads.Add(1)
	if f.codex == nil {
		return codexData{}
	}
	return *f.codex
}
func (f *fakeStore) GetUserCodexAccountMode(context.Context, uuid.UUID) (string, error) {
	f.reads.Add(1)
	return "default", nil
}
func (f *fakeStore) ListUserSecretsForKind(context.Context, store.ListUserSecretsForKindParams) ([]store.ListUserSecretsForKindRow, error) {
	return f.cdx().secrets, nil
}
func (f *fakeStore) ListCodexAliasAccountsForUser(context.Context, uuid.UUID) ([]store.ListCodexAliasAccountsForUserRow, error) {
	return f.cdx().links, nil
}
func (f *fakeStore) GetCodexAccountRateLimitsForUser(context.Context, uuid.UUID) ([]store.GetCodexAccountRateLimitsForUserRow, error) {
	return f.cdx().accounts, nil
}
func (f *fakeStore) ListCodexPoolAliases(context.Context, uuid.UUID) ([]store.ListCodexPoolAliasesRow, error) {
	f.reads.Add(1)
	return nil, nil
}
func (f *fakeStore) ListWorkersByUser(context.Context, uuid.UUID) ([]store.ListWorkersByUserRow, error) {
	f.reads.Add(1)
	return f.workers, nil
}

var (
	now    = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	owner  = uuid.MustParse("00000000-0000-0000-0000-00000000f001")
	wkr    = uuid.MustParse("00000000-0000-0000-0000-00000000f002")
	tokA   = uuid.MustParse("00000000-0000-0000-0000-0000000000a1")
	tokB   = uuid.MustParse("00000000-0000-0000-0000-0000000000a2")
	runOne = uuid.MustParse("00000000-0000-0000-0000-0000000000c1")
)

func tok(id uuid.UUID, label string, pooled bool, weekly int16) (store.ListAutoSelectCandidatesRow, store.ListReserveClaudeTokensRow) {
	synced, reset := now.Add(-time.Minute), now.Add(72*time.Hour)
	return store.ListAutoSelectCandidatesRow{
		UserSecretID: id, Label: label, AutoEligible: pooled,
		SevenDayPct: pgtype.Int2{Int16: weekly, Valid: true}, SevenDayResetsAt: pgconv.Time(reset), SyncedAt: pgconv.Time(synced),
	}, store.ListReserveClaudeTokensRow{
		UserSecretID: id, AutoEligible: pooled, HasReading: true,
		SevenDayPct: pgtype.Int2{Int16: weekly, Valid: true}, SevenDayResetsAt: pgconv.Time(reset), SyncedAt: pgconv.Time(synced),
	}
}

func newLoader(t *testing.T, f *fakeStore) *Loader {
	t.Helper()
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	svc := workersvc.New(f, box, workersvc.Params{
		Autoselect:        autoselect.Policy{Order: autoselect.OrderHeadroomFirst, MinHeadroom: 15, HeadroomTiePct: 5, MaxStaleness: 15 * time.Minute, InflightPenalty: 3},
		UsagePollInterval: 5 * time.Minute, CodexUsagePollInterval: 5 * time.Minute,
		RunLimitMaxWaits: 3, RunLimitMaxPark: 8 * 24 * time.Hour, WorkerHeartbeatStale: 45 * time.Second,
	})
	l := NewLoader(svc)
	l.Now = func() time.Time { return now }
	return l
}

func twoTokens(labelB string) *fakeStore {
	f := &fakeStore{}
	for _, t := range []struct {
		id     uuid.UUID
		label  string
		pooled bool
		pct    int16
	}{{tokA, "work", true, 96}, {tokB, labelB, true, 20}} {
		c, r := tok(t.id, t.label, t.pooled, t.pct)
		f.cands, f.tokens = append(f.cands, c), append(f.tokens, r)
	}
	f.workers = []store.ListWorkersByUserRow{{
		ID: wkr, UserID: owner, Name: "w1", AnthropicBindMode: "default",
		LastHeartbeatAt: pgconv.Time(now.Add(-time.Second)),
	}}
	return f
}

func parkedRun(limitType string) store.Run {
	return store.Run{
		ID: runOne, UserID: owner, Kind: "issue", Harness: "claude", Status: "limit_wait",
		WorkerID: pgtype.UUID{Bytes: wkr, Valid: true}, WaitOnLimit: true, LimitWaitCount: 1,
		RateLimitType:              pgtype.Text{String: limitType, Valid: true},
		AnthropicSecretID:          pgtype.UUID{Bytes: tokA, Valid: true},
		LimitDeadSecretID:          pgtype.UUID{Bytes: tokA, Valid: true},
		LimitResetsAt:              pgconv.Time(now.Add(72 * time.Hour)),
		RetryNotBefore:             pgconv.Time(now.Add(72 * time.Hour)),
		CredentialOverrideMode:     pgtype.Text{String: "pinned", Valid: true},
		CredentialOverrideSecretID: pgtype.UUID{Bytes: tokA, Valid: true},
	}
}

func fixOf(d *apitypes.WeeklyLimitDTO, kind string) *apitypes.WeeklyLimitFixDTO {
	for i := range d.Fix {
		if d.Fix[i].Kind == kind {
			return &d.Fix[i]
		}
	}
	return nil
}

func TestPinnedSevenDayParkExplainsAndOffersSetToken(t *testing.T) {
	f := twoTokens("spare")
	l := newLoader(t, f)
	run := parkedRun("seven_day")
	got := l.ForRuns(context.Background(), owner, []store.Run{run})[run.ID]
	if got == nil {
		t.Fatal("no block for a pinned seven_day park")
	}
	if got.Reason != workersvc.NextClaimPinned || got.CredentialKey != "anthropic:"+tokA.String() || got.CredentialLabel != "work" {
		t.Fatalf("reason/key/label = %q %q %q", got.Reason, got.CredentialKey, got.CredentialLabel)
	}
	if got.Pct == nil || *got.Pct != 96 || got.Harness != "claude" || got.Wait != workersvc.WaitWaits {
		t.Fatalf("pct/harness/wait = %v %q %q", got.Pct, got.Harness, got.Wait)
	}
	st := fixOf(got, FixSetToken)
	if st == nil || st.TargetID != tokB.String() || st.Command != "uzi run set-token "+runOne.String()+" -- spare" {
		t.Fatalf("set_token fix = %+v", st)
	}
	if as := fixOf(got, FixAutoSelect); as == nil || as.Command != "uzi run set-token "+runOne.String()+" --auto" {
		t.Fatalf("auto_select fix = %+v", as)
	}
	if got.Tier != apitypes.WeeklyLimitTierWarning {
		t.Fatalf("tier = %q", got.Tier)
	}
}

// With the early warnings off (margin 0, the fake store's default) only a weekly park carries a block:
// a five_hour or per-model park of a run on a credential near its weekly limit has none.
func TestOnlySevenDayWeeklyParksCarryABlockWithMarginOff(t *testing.T) {
	for _, lt := range []string{"five_hour", "seven_day_opus", "seven_day_sonnet", ""} {
		f := twoTokens("spare")
		l := newLoader(t, f)
		run := parkedRun(lt)
		if got := l.ForRuns(context.Background(), owner, []store.Run{run}); len(got) != 0 {
			t.Errorf("rate_limit_type %q carried a block: %+v", lt, got[run.ID])
		}
	}
}

func TestExcludedRunsLoadNothing(t *testing.T) {
	f := twoTokens("spare")
	l := newLoader(t, f)
	provider := parkedRun("seven_day")
	provider.ID = uuid.New()
	provider.ModelProviderID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	apiKey := parkedRun("seven_day")
	apiKey.ID, apiKey.Harness = uuid.New(), "codex"
	apiKey.CodexAuthMode = pgtype.Text{String: "api_key", Valid: true}
	got := l.ForRuns(context.Background(), owner, []store.Run{provider, apiKey})
	if len(got) != 0 {
		t.Fatalf("excluded runs carried blocks: %+v", got)
	}
	if f.reads.Load() != 0 || f.candCnt.Load() != 0 {
		t.Fatalf("excluded runs caused %d reads (%d candidate reads); want none", f.reads.Load(), f.candCnt.Load())
	}
}

func TestOneSnapshotPerOwnerBatch(t *testing.T) {
	f := twoTokens("spare")
	l := newLoader(t, f)
	a, b := parkedRun("seven_day"), parkedRun("seven_day")
	b.ID = uuid.New()
	other := parkedRun("five_hour")
	other.ID = uuid.New()
	got := l.ForRuns(context.Background(), owner, []store.Run{a, b, other})
	if len(got) != 2 || got[a.ID] == nil || got[b.ID] == nil {
		t.Fatalf("blocks = %+v", got)
	}
	if f.candCnt.Load() != 1 {
		t.Fatalf("candidate reads = %d; want one snapshot", f.candCnt.Load())
	}
}

func TestWeeklyReservePauseOffersResumeAndSettings(t *testing.T) {
	f := twoTokens("spare")
	l := newLoader(t, f)
	run := parkedRun("")
	run.Status = "paused"
	run.HoldReason = pgtype.Text{String: "weekly_reserve", Valid: true}
	run.LimitResetsAt, run.RetryNotBefore, run.RateLimitType = pgtype.Timestamptz{}, pgtype.Timestamptz{}, pgtype.Text{}
	got := l.ForRuns(context.Background(), owner, []store.Run{run})[run.ID]
	if got == nil {
		t.Fatal("no block for a weekly-reserve pause")
	}
	if got.Wait != workersvc.WaitReservePause {
		t.Fatalf("wait = %q", got.Wait)
	}
	if r := fixOf(got, FixResumeReserve); r == nil || r.Command != "uzi run resume "+runOne.String() {
		t.Fatalf("resume fix = %+v", r)
	}
	if e := fixOf(got, FixEditReserve); e == nil || e.Command != "" {
		t.Fatalf("edit_reserve fix = %+v", e)
	}
}

func TestHostileLabelsAreQuotedOrWithheld(t *testing.T) {
	// Assembled at runtime so no scanner sees a literal payload.
	dollar := "$"
	quoted := map[string]string{
		"it's":               `'it'\''s'`,
		dollar + "(touch x)": `'$(touch x)'`,
		"--default":          "--default",
		"-x":                 "-x",
		"two words":          "'two words'",
		"_prod_":             "_prod_",
	}
	for label, want := range quoted {
		f := twoTokens(label)
		run := parkedRun("seven_day")
		got := newLoader(t, f).ForRuns(context.Background(), owner, []store.Run{run})[run.ID]
		st := fixOf(got, FixSetToken)
		if st == nil || st.CommandUnavailable != "" {
			t.Fatalf("%q: set_token = %+v", label, st)
		}
		if wantCmd := "uzi run set-token " + runOne.String() + " -- " + want; st.Command != wantCmd {
			t.Errorf("%q: command = %q; want %q", label, st.Command, wantCmd)
		}
	}
	// Grandfathered labels that CellText would change get no command, and no other label's command.
	legacy := []string{"tab\tlabel", "line\nbreak", "esc\x1b[31mred", "zero\u200bwidth", "bad\xffbyte", "repl�char", " padded"}
	for _, label := range legacy {
		f := twoTokens(label)
		run := parkedRun("seven_day")
		got := newLoader(t, f).ForRuns(context.Background(), owner, []store.Run{run})[run.ID]
		st := fixOf(got, FixSetToken)
		if st == nil || st.Command != "" || st.CommandUnavailable != apitypes.WeeklyFixCommandLabel {
			t.Errorf("%q: set_token = %+v; want no command, command_unavailable=label", label, st)
			continue
		}
		if strings.ContainsAny(st.TargetLabel, "\t\n\x1b\u200b") {
			t.Errorf("%q: display label not sanitised: %q", label, st.TargetLabel)
		}
		if as := fixOf(got, FixAutoSelect); as == nil || as.Command == "" {
			t.Errorf("%q: label-free fix lost its command: %+v", label, as)
		}
	}
}

func TestBlockJSONShape(t *testing.T) {
	f := twoTokens("spare")
	run := parkedRun("seven_day")
	got := newLoader(t, f).ForRuns(context.Background(), owner, []store.Run{run})[run.ID]
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"credential_label", "credential_key", "harness", "pct", "effective_limit", "resets_at", "tier", "reason", "future_runs", "fix", "wait"} {
		if _, ok := m[k]; !ok {
			t.Errorf("key %q missing from %s", k, b)
		}
	}
}

// codexFixture is an owner with two linked Codex accounts: cx-a at 97% of its weekly window and
// cx-b at 10%. Workers come from twoTokens (default bind, no Codex capabilities).
func codexFixture() (f *fakeStore, aliasA, aliasB, acctA uuid.UUID) {
	aliasA, aliasB = uuid.New(), uuid.New()
	acctA, acctB := uuid.New(), uuid.New()
	weekly := func(pct float64) []byte {
		secs, reset := int64(604800), now.Add(48*time.Hour).Unix()
		b, _ := json.Marshal([]apitypes.CodexRateLimitBucketDTO{{ID: "codex", Primary: &apitypes.CodexRateLimitWindowDTO{
			UsedPercent: &pct, LimitWindowSeconds: &secs, ResetAt: &reset,
		}}})
		return b
	}
	f = twoTokens("spare")
	f.codex = &codexData{
		secrets: []store.ListUserSecretsForKindRow{{ID: aliasA, Kind: "codex_auth", Label: "cx-a"}, {ID: aliasB, Kind: "codex_auth", Label: "cx-b"}},
		links: []store.ListCodexAliasAccountsForUserRow{
			{UserSecretID: aliasA, ProviderAccountID: pgtype.UUID{Bytes: acctA, Valid: true}},
			{UserSecretID: aliasB, ProviderAccountID: pgtype.UUID{Bytes: acctB, Valid: true}},
		},
		accounts: []store.GetCodexAccountRateLimitsForUserRow{
			{ProviderAccountID: acctA, Aliases: []string{"cx-a"}, Buckets: weekly(97), LastSuccessAt: pgconv.Time(now.Add(-time.Minute))},
			{ProviderAccountID: acctB, Aliases: []string{"cx-b"}, Buckets: weekly(10), LastSuccessAt: pgconv.Time(now.Add(-time.Minute))},
		},
	}
	return f, aliasA, aliasB, acctA
}

func codexRun(aliasA uuid.UUID) store.Run {
	run := parkedRun("seven_day")
	run.Harness = "codex"
	run.CodexSecretID = pgtype.UUID{Bytes: aliasA, Valid: true}
	run.CredentialOverrideMode, run.CredentialOverrideSecretID = pgtype.Text{}, pgtype.UUID{}
	return run
}

func TestParkedCodexRunIsAccountBound(t *testing.T) {
	f, aliasA, _, acctA := codexFixture()
	run := codexRun(aliasA)
	got := newLoader(t, f).ForRuns(context.Background(), owner, []store.Run{run})[run.ID]
	if got == nil {
		t.Fatal("no block for a parked Codex run")
	}
	if got.Reason != workersvc.NextClaimAccountBound || got.CredentialKey != "codex:"+acctA.String() || got.CredentialLabel != "cx-a" || got.Harness != "codex" {
		t.Fatalf("block = %+v", got)
	}
	if got.Pct == nil || *got.Pct != 97 || got.AlternativeLabel != "cx-b" {
		t.Fatalf("pct/alternative = %v %q", got.Pct, got.AlternativeLabel)
	}
	foundPool := false
	for _, ff := range got.FutureRuns {
		if ff.Kind == FixPoolOn && ff.Command == "uzi token pool --on --kind codex -- cx-b" {
			foundPool = true
		}
	}
	if !foundPool {
		t.Fatalf("future_runs lacks the codex pool_on command: %+v", got.FutureRuns)
	}
}

// codexCapableWorker makes the twoTokens worker advertise the Codex switch protocol, as a current
// worker does.
func codexCapableWorker(f *fakeStore) {
	f.workers[0].ProtocolCapabilities = []string{capability.CodexCredentialSwitchV1, capability.CodexSwitchStoppedV1}
	f.workers[0].SnapshotRegisterNonce = pgtype.Text{String: "n1", Valid: true}
	f.workers[0].CredentialSwitchApplyNonce = pgtype.Text{String: "n1", Valid: true}
}

func TestCodexSetTokenFixOnlyForALiveRunWithAnAlternativeAccount(t *testing.T) {
	ctx := context.Background()
	f, aliasA, aliasB, _ := codexFixture()
	codexCapableWorker(f)
	l := newLoader(t, f)

	// A live Codex run on a capable worker: the owner can switch it, so the fix is offered.
	live := codexRun(aliasA)
	live.Status = "running"
	live.RateLimitType, live.RetryNotBefore, live.LimitResetsAt = pgtype.Text{}, pgtype.Timestamptz{}, pgtype.Timestamptz{}
	snap, err := l.svc.LoadNextClaimSnapshot(ctx, owner, now)
	if err != nil {
		t.Fatal(err)
	}
	res := l.svc.NextClaimReason(live, snap)
	if res == nil || !res.ManualSwitch || res.Alternative == nil || res.Alternative.ID != aliasB {
		t.Fatalf("live run result = %+v; want a manual switch to cx-b", res)
	}
	runFix, _ := fixes(live, res, snap)
	var st *apitypes.WeeklyLimitFixDTO
	for i := range runFix {
		if runFix[i].Kind == FixSetToken {
			st = &runFix[i]
		}
	}
	if want := "uzi run set-token " + live.ID.String() + " -- cx-b"; st == nil || st.Command != want || st.TargetID != aliasB.String() {
		t.Fatalf("set_token fix = %+v; want command %q", st, want)
	}

	// The same account parked on its weekly window is not switchable in place: no set_token fix.
	parked := codexRun(aliasA)
	got := l.ForRuns(ctx, owner, []store.Run{parked})[parked.ID]
	if got == nil {
		t.Fatal("no block for a parked Codex run")
	}
	if fixOf(got, FixSetToken) != nil {
		t.Fatalf("parked Codex run offered set_token: %+v", got.Fix)
	}
}

// TestAnthropicPoolOnCommandGrammar pins the pool_on command the CLI round-trip test replays: an
// Anthropic run in an auto lane whose only headroom is an unpooled token is told to pool it.
func TestAnthropicPoolOnCommandGrammar(t *testing.T) {
	f := &fakeStore{}
	for _, c := range []struct {
		id     uuid.UUID
		label  string
		pooled bool
		pct    int16
	}{{tokA, "work", true, 96}, {tokB, "spare", false, 20}} {
		cand, res := tok(c.id, c.label, c.pooled, c.pct)
		f.cands, f.tokens = append(f.cands, cand), append(f.tokens, res)
	}
	f.workers = []store.ListWorkersByUserRow{{
		ID: wkr, UserID: owner, Name: "w1", AnthropicBindMode: "auto",
		LastHeartbeatAt: pgconv.Time(now.Add(-time.Second)),
	}}
	run := parkedRun("seven_day")
	run.CredentialOverrideMode, run.CredentialOverrideSecretID = pgtype.Text{}, pgtype.UUID{}
	got := newLoader(t, f).ForRuns(context.Background(), owner, []store.Run{run})[run.ID]
	if got == nil {
		t.Fatal("no block")
	}
	all := append(append([]apitypes.WeeklyLimitFixDTO{}, got.Fix...), got.FutureRuns...)
	for _, fx := range all {
		if fx.Kind == FixPoolOn {
			if want := "uzi token pool --on --kind anthropic -- spare"; fx.Command != want {
				t.Fatalf("pool_on command = %q; want %q", fx.Command, want)
			}
			return
		}
	}
	t.Fatalf("no pool_on fix: %+v", all)
}

func TestBacktickLabelGetsNoCommand(t *testing.T) {
	label := "a" + "`" + "id" + "`"
	f := twoTokens(label)
	run := parkedRun("seven_day")
	got := newLoader(t, f).ForRuns(context.Background(), owner, []store.Run{run})[run.ID]
	st := fixOf(got, FixSetToken)
	if st == nil || st.Command != "" || st.CommandUnavailable != apitypes.WeeklyFixCommandLabel {
		t.Fatalf("set_token = %+v; want no command, command_unavailable=label", st)
	}
}
