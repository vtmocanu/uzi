package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/codexselect"
	"github.com/vtmocanu/uzi/api/internal/store"
)

var autoTestNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func autoTestTime(d time.Duration) *time.Time {
	t := autoTestNow.Add(d)
	return &t
}

func qualifiedSide(headroom float64, reset *time.Time) autoSide {
	return autoSide{State: autoQualified, Headroom: floatOf(headroom), Measured: floatOf(headroom), WeeklyReset: reset}
}

func TestChooseAutomaticHarness(t *testing.T) {
	depleted := autoSide{State: autoDepleted, Headroom: floatOf(2), Measured: floatOf(2)}
	unknown := autoSide{State: autoUnknown}
	tests := []struct {
		name          string
		claude, codex autoSide
		want          Harness
		reason        string
	}{
		{"codex resets sooner", qualifiedSide(50, autoTestTime(48*time.Hour)), qualifiedSide(50, autoTestTime(24*time.Hour)), HarnessCodex, harnessReasonAutoSoonestReset},
		{"claude resets sooner", qualifiedSide(50, autoTestTime(24*time.Hour)), qualifiedSide(90, autoTestTime(48*time.Hour)), HarnessClaude, harnessReasonAutoSoonestReset},
		{"claude missing reset loses", qualifiedSide(90, nil), qualifiedSide(20, autoTestTime(72*time.Hour)), HarnessCodex, harnessReasonAutoSoonestReset},
		{"codex missing reset loses", qualifiedSide(20, autoTestTime(72*time.Hour)), qualifiedSide(90, nil), HarnessClaude, harnessReasonAutoSoonestReset},
		{"claude past reset counts as missing", qualifiedSide(90, autoTestTime(-time.Hour)), qualifiedSide(20, autoTestTime(72*time.Hour)), HarnessCodex, harnessReasonAutoSoonestReset},
		{"codex past reset counts as missing", qualifiedSide(20, autoTestTime(72*time.Hour)), qualifiedSide(90, autoTestTime(-time.Hour)), HarnessClaude, harnessReasonAutoSoonestReset},
		{"both past resets tie on headroom for codex", qualifiedSide(30, autoTestTime(-2*time.Hour)), qualifiedSide(60, autoTestTime(-time.Hour)), HarnessCodex, harnessReasonAutoSoonestReset},
		{"exact tie more claude headroom", qualifiedSide(80, autoTestTime(24*time.Hour)), qualifiedSide(40, autoTestTime(24*time.Hour)), HarnessClaude, harnessReasonAutoSoonestReset},
		{"exact tie more codex headroom", qualifiedSide(40, autoTestTime(24*time.Hour)), qualifiedSide(80, autoTestTime(24*time.Hour)), HarnessCodex, harnessReasonAutoSoonestReset},
		{"exact tie same headroom prefers claude", qualifiedSide(55, autoTestTime(24*time.Hour)), qualifiedSide(55, autoTestTime(24*time.Hour)), HarnessClaude, harnessReasonAutoSoonestReset},
		{"both missing same headroom prefers claude", qualifiedSide(55, nil), qualifiedSide(55, nil), HarnessClaude, harnessReasonAutoSoonestReset},
		{"only codex qualified", depleted, qualifiedSide(15, autoTestTime(time.Hour)), HarnessCodex, harnessReasonAutoOnlyQualified},
		{"only claude qualified", qualifiedSide(15, autoTestTime(time.Hour)), depleted, HarnessClaude, harnessReasonAutoOnlyQualified},
		{"both depleted", depleted, depleted, HarnessClaude, harnessReasonAutoFallback},
		{"claude unknown", unknown, qualifiedSide(90, autoTestTime(time.Hour)), HarnessClaude, harnessReasonAutoFallback},
		{"codex unknown", qualifiedSide(90, autoTestTime(time.Hour)), unknown, HarnessClaude, harnessReasonAutoFallback},
		{"codex unknown claude depleted", depleted, unknown, HarnessClaude, harnessReasonAutoFallback},
		{"both unknown", unknown, unknown, HarnessClaude, harnessReasonAutoFallback},
		{"zero value side is unknown", autoSide{}, qualifiedSide(90, nil), HarnessClaude, harnessReasonAutoFallback},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := chooseAutomaticHarness(tt.claude, tt.codex, autoTestNow)
			if got != tt.want || reason != tt.reason {
				t.Fatalf("got %s/%s want %s/%s", got, reason, tt.want, tt.reason)
			}
			if !isAutomaticReason(reason) {
				t.Fatalf("reason %q is not automatic: an automatic decision must never record overflow", reason)
			}
		})
	}
}

func TestHarnessCreationDecisionChosenAutomatic(t *testing.T) {
	for _, reason := range []string{harnessReasonAutoSoonestReset, harnessReasonAutoOnlyQualified, harnessReasonAutoFallback} {
		for _, chosen := range []Harness{HarnessClaude, HarnessCodex} {
			d := harnessCreationDecision{Preferred: HarnessClaude, Reason: reason, Chosen: chosen}
			if d.chosen() != chosen {
				t.Fatalf("%s chosen=%s want %s", reason, d.chosen(), chosen)
			}
		}
	}
	// The overflow and non-overflow derivations ignore Chosen.
	for _, preferred := range []Harness{HarnessClaude, HarnessCodex} {
		other := HarnessCodex
		if preferred == HarnessCodex {
			other = HarnessClaude
		}
		overflow := harnessCreationDecision{Preferred: preferred, Reason: harnessReasonOverflow, Chosen: preferred}
		if overflow.chosen() != other {
			t.Fatalf("overflow from %s chose %s", preferred, overflow.chosen())
		}
		for _, reason := range []string{harnessReasonOff, harnessReasonPreferred, harnessReasonUnknownUsage, harnessReasonExcluded} {
			d := harnessCreationDecision{Preferred: preferred, Reason: reason, Chosen: other}
			if d.chosen() != preferred {
				t.Fatalf("%s chose %s want preferred %s", reason, d.chosen(), preferred)
			}
		}
	}
}

func TestClaudeAutoSide(t *testing.T) {
	policy := autoselect.Policy{MinHeadroom: 10, MaxStaleness: 15 * time.Minute}
	synced := autoTestNow.Add(-time.Minute)
	cand := func(id uuid.UUID, pooled bool, usedPct int16, weekly *time.Time) autoselect.Candidate {
		pct := usedPct
		return autoselect.Candidate{SecretID: id, AutoEligible: pooled, HasReading: true,
			FiveHourPct: &pct, SevenDayPct: &pct, SyncedAt: &synced, WeeklyResetsAt: weekly, SevenResetsAt: weekly}
	}
	a, b, def := uuid.New(), uuid.New(), uuid.New()
	none := func(uuid.UUID) bool { return false }
	only := func(ids ...uuid.UUID) func(uuid.UUID) bool {
		return func(id uuid.UUID) bool {
			for _, x := range ids {
				if x == id {
					return true
				}
			}
			return false
		}
	}
	unmeasuredDefault := autoselect.Candidate{SecretID: def}
	staleDefault := cand(def, false, 10, nil)
	old := autoTestNow.Add(-time.Hour)
	staleDefault.SyncedAt = &old
	noPct := cand(def, false, 10, nil)
	noPct.SevenDayPct = nil
	rejectedDefault := cand(def, false, 10, nil)
	rejectedDefault.Rejected = true

	tests := []struct {
		name    string
		cands   []autoselect.Candidate
		blocked func(uuid.UUID) bool
		state   autoSideState
		held    bool
		reset   *time.Time
		head    float64
	}{
		{"top token held, second evaluated", []autoselect.Candidate{cand(a, true, 5, autoTestTime(time.Hour)), cand(b, true, 40, autoTestTime(48*time.Hour))}, only(a), autoQualified, false, autoTestTime(48 * time.Hour), 60},
		{"pooled picks soonest reset", []autoselect.Candidate{cand(a, true, 5, autoTestTime(72*time.Hour)), cand(b, true, 40, autoTestTime(24*time.Hour))}, none, autoQualified, false, autoTestTime(24 * time.Hour), 60},
		{"every pooled token held, default not consulted", []autoselect.Candidate{cand(a, true, 5, nil), cand(b, true, 5, nil), cand(def, false, 5, nil)}, only(a, b), autoDepleted, true, nil, 0},
		{"pooled below floor is best-of-pool", []autoselect.Candidate{cand(a, true, 95, autoTestTime(time.Hour)), cand(b, true, 97, nil)}, none, autoDepleted, false, autoTestTime(time.Hour), 5},
		{"mixed held and unheld stale pool", []autoselect.Candidate{func() autoselect.Candidate { c := cand(a, true, 5, nil); c.SyncedAt = &old; return c }(), cand(b, true, 5, nil)}, only(b), autoUnknown, false, nil, 0},
		{"rejected pooled token is not pooled", []autoselect.Candidate{func() autoselect.Candidate { c := cand(a, true, 5, nil); c.Rejected = true; return c }(), cand(def, false, 40, autoTestTime(time.Hour))}, none, autoQualified, false, autoTestTime(time.Hour), 60},
		{"unpooled default measured", []autoselect.Candidate{cand(def, false, 40, autoTestTime(time.Hour))}, none, autoQualified, false, autoTestTime(time.Hour), 60},
		{"unpooled default below floor", []autoselect.Candidate{cand(def, false, 95, nil)}, none, autoDepleted, false, nil, 5},
		{"unpooled default reserve-held", []autoselect.Candidate{cand(def, false, 5, nil)}, only(def), autoDepleted, true, nil, 0},
		{"reserve-unknown hold on unmeasured default", []autoselect.Candidate{unmeasuredDefault}, only(def), autoDepleted, true, nil, 0},
		{"unpooled default stale", []autoselect.Candidate{staleDefault}, none, autoUnknown, false, nil, 0},
		{"unpooled default unmeasured", []autoselect.Candidate{unmeasuredDefault}, none, autoUnknown, false, nil, 0},
		{"unpooled default no seven-day pct", []autoselect.Candidate{noPct}, none, autoUnknown, false, nil, 0},
		{"rejected default", []autoselect.Candidate{rejectedDefault}, none, autoUnknown, false, nil, 0},
		{"default missing from candidates", []autoselect.Candidate{cand(a, false, 5, nil)}, none, autoUnknown, false, nil, 0},
		{"no candidates", nil, none, autoUnknown, false, nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := claudeAutoSide(tt.cands, def, tt.blocked, policy, autoTestNow)
			if got.State != tt.state || got.Held != tt.held {
				t.Fatalf("state=%s held=%t want %s/%t (%+v)", got.State, got.Held, tt.state, tt.held, got)
			}
			if (got.WeeklyReset == nil) != (tt.reset == nil) || (tt.reset != nil && !got.WeeklyReset.Equal(*tt.reset)) {
				t.Fatalf("weekly reset=%v want %v", got.WeeklyReset, tt.reset)
			}
			if tt.head != 0 && (!got.Headroom.Valid || got.Headroom.Float64 != tt.head) {
				t.Fatalf("headroom=%v want %v", got.Headroom, tt.head)
			}
		})
	}

	t.Run("poller off leaves every reading stale", func(t *testing.T) {
		off := policy
		off.MaxStaleness = 0
		if got := claudeAutoSide([]autoselect.Candidate{cand(def, false, 5, nil)}, def, none, off, autoTestNow); got.State != autoUnknown {
			t.Fatalf("unpooled default with poller off: %+v", got)
		}
		if got := claudeAutoSide([]autoselect.Candidate{cand(a, true, 5, nil)}, def, none, off, autoTestNow); got.State != autoUnknown {
			t.Fatalf("pooled token with poller off: %+v", got)
		}
	})
}

func TestCodexSideFromCandidate(t *testing.T) {
	reset := autoTestTime(24 * time.Hour)
	cand := func(known bool, score float64, held bool) codexselect.Candidate {
		return codexselect.Candidate{Structural: true, Score: score, ScoreKnown: known, CapacityExcluded: held, WeeklyResetAt: reset}
	}
	tests := []struct {
		name     string
		reason   codexselect.Reason
		c        codexselect.Candidate
		state    autoSideState
		held     bool
		measured bool
	}{
		{"pool exhausted", codexselect.ReasonPoolExhausted, cand(true, 40, false), autoDepleted, true, true},
		{"pool exhausted unmeasured", codexselect.ReasonPoolExhausted, cand(false, 0, true), autoDepleted, true, false},
		{"held candidate on auto", codexselect.ReasonAuto, cand(true, 40, true), autoDepleted, true, true},
		{"pool stale", codexselect.ReasonPoolStale, cand(false, 0, false), autoUnknown, false, false},
		{"pool auto", codexselect.ReasonAuto, cand(true, 60, false), autoQualified, false, true},
		{"best of pool", codexselect.ReasonBestOfPool, cand(true, 5, false), autoDepleted, false, true},
		{"default held", "default", cand(true, 80, true), autoDepleted, true, true},
		{"default unscored", "default", cand(false, 0, false), autoUnknown, false, false},
		{"default below floor", "default", cand(true, 9.5, false), autoDepleted, false, true},
		{"default at floor", "default", cand(true, 10, false), autoQualified, false, true},
		{"default above floor", "default", cand(true, 70, false), autoQualified, false, true},
		{"pool empty default above floor", codexselect.ReasonPoolEmpty, cand(true, 70, false), autoQualified, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := codexSideFromCandidate(tt.reason, tt.c, 10)
			if got.State != tt.state || got.Held != tt.held || got.Measured.Valid != tt.measured || got.Headroom.Valid != tt.measured {
				t.Fatalf("got %+v want state=%s held=%t measured=%t", got, tt.state, tt.held, tt.measured)
			}
			if tt.measured && got.Measured.Float64 != tt.c.Score {
				t.Fatalf("measured=%v want %v", got.Measured, tt.c.Score)
			}
			if got.WeeklyReset == nil || !got.WeeklyReset.Equal(*reset) {
				t.Fatalf("weekly reset=%v", got.WeeklyReset)
			}
		})
	}
}

func TestAutoSideEvidence(t *testing.T) {
	held := autoSide{State: autoDepleted, Held: true, Measured: floatOf(44)}
	if e := held.evidence(); e.comparison() != floatOf(0) || e.Measured != floatOf(44) || !e.Held {
		t.Fatalf("held evidence: %+v", e)
	}
	q := qualifiedSide(61, nil)
	if e := q.evidence(); e.comparison() != floatOf(61) || e.Measured != floatOf(61) || e.Held {
		t.Fatalf("qualified evidence: %+v", e)
	}
	if e := (autoSide{State: autoUnknown}).evidence(); e.comparison() != (pgtype.Float8{}) || e.Measured.Valid {
		t.Fatalf("unknown evidence must stay NULL: %+v", e)
	}
}

// An unknown side's reading did not take part in the comparison, so the decision record
// stores no weekly reset for it even when the stale reading carried one.
func TestAutomaticDecisionRecordsResetOnlyForKnownSides(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	stale := now.Add(24 * time.Hour)
	known := now.Add(48 * time.Hour)
	d := &harnessCreationDecision{}
	d.applyAutomatic(autoSide{State: autoUnknown, WeeklyReset: &stale}, qualifiedSide(50, &known), now)
	if d.PreferredWeeklyReset != nil {
		t.Fatalf("unknown Claude side recorded reset %v", d.PreferredWeeklyReset)
	}
	if d.OtherWeeklyReset == nil || !d.OtherWeeklyReset.Equal(known) {
		t.Fatalf("qualified Codex side reset = %v, want %v", d.OtherWeeklyReset, known)
	}
	// A known side whose reset already passed carried no reset into the comparison.
	past := now.Add(-time.Hour)
	d = &harnessCreationDecision{}
	d.applyAutomatic(qualifiedSide(50, &past), qualifiedSide(50, &known), now)
	if d.PreferredWeeklyReset != nil {
		t.Fatalf("past Claude reset recorded: %v", d.PreferredWeeklyReset)
	}
}

func TestClearsInheritOverride(t *testing.T) {
	tests := []struct {
		reason  string
		harness Harness
		want    bool
	}{
		{harnessReasonOverflow, HarnessCodex, true},
		{harnessReasonOverflow, HarnessClaude, true},
		{harnessReasonAutoSoonestReset, HarnessCodex, true},
		{harnessReasonAutoOnlyQualified, HarnessCodex, true},
		{harnessReasonAutoSoonestReset, HarnessClaude, false},
		{harnessReasonAutoOnlyQualified, HarnessClaude, false},
		{harnessReasonAutoFallback, HarnessClaude, false},
		{harnessReasonAutoFallback, HarnessCodex, false},
		{harnessReasonOff, HarnessCodex, false},
		{harnessReasonExcluded, HarnessCodex, false},
		{harnessReasonPreferred, HarnessCodex, false},
		{harnessReasonUnknownUsage, HarnessCodex, false},
	}
	for _, tt := range tests {
		got := clearsInheritOverride(resolvedHarness{Harness: tt.harness, Decision: &harnessCreationDecision{Reason: tt.reason}})
		if got != tt.want {
			t.Fatalf("%s/%s: got %t want %t", tt.reason, tt.harness, got, tt.want)
		}
	}
	if clearsInheritOverride(resolvedHarness{Harness: HarnessCodex}) {
		t.Fatal("a resolution with no decision must not clear the override")
	}
}

func TestAutomaticHarnessEligible(t *testing.T) {
	pin, model := HarnessClaude, "opus"
	res := resolvedHarness{Harness: HarnessClaude, DefaultUnset: true, BothUsable: true}
	origin := harnessCreationOrigin{Kind: "issue", TriggerSource: "manual"}
	var policy store.GetHarnessCreationPolicyRow
	for _, source := range []string{"manual", "autopilot", "schedule"} {
		o := origin
		o.TriggerSource = source
		if !automaticHarnessEligible(res, nil, o, policy) {
			t.Fatalf("source %s must be eligible", source)
		}
	}
	// The overflow toggle plays no part.
	policy.HarnessOverflowEnabled = true
	if !automaticHarnessEligible(res, nil, origin, policy) {
		t.Fatal("overflow toggle changed automatic eligibility")
	}
	policy.HarnessOverflowEnabled = false

	setDefault, oneUsable := res, res
	setDefault.DefaultUnset = false
	oneUsable.BothUsable = false
	tests := []struct {
		name     string
		res      resolvedHarness
		explicit *Harness
		origin   harnessCreationOrigin
		policy   store.GetHarnessCreationPolicyRow
	}{
		{"explicit harness argument", res, &pin, origin, policy},
		{"origin explicit harness", res, nil, harnessCreationOrigin{Kind: "issue", TriggerSource: "manual", Explicit: &pin}, policy},
		{"model pin", res, nil, harnessCreationOrigin{Kind: "issue", TriggerSource: "manual", Model: &model}, policy},
		{"anthropic override", res, nil, harnessCreationOrigin{Kind: "issue", TriggerSource: "manual", AnthropicOverride: true}, policy},
		{"plan cross-check", res, nil, origin, store.GetHarnessCreationPolicyRow{PlanCrossCheckEnabled: true}},
		{"code cross-check", res, nil, origin, store.GetHarnessCreationPolicyRow{CodeCrossCheckEnabled: true}},
		{"non-issue kind", res, nil, harnessCreationOrigin{Kind: "task", TriggerSource: "manual"}, policy},
		{"other trigger", res, nil, harnessCreationOrigin{Kind: "issue", TriggerSource: "chat"}, policy},
		{"only one harness usable", oneUsable, nil, origin, policy},
		{"default set", setDefault, nil, origin, policy},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if automaticHarnessEligible(tt.res, tt.explicit, tt.origin, tt.policy) {
				t.Fatal("excluded creation admitted to the automatic path")
			}
		})
	}
}

// A DefaultUnset resolution that is not automatic records excluded whatever the toggle says;
// a set default keeps the toggle-driven off. None of these reads the database.
func TestEvaluateCreationOverflowDefaultUnsetExcluded(t *testing.T) {
	pin := HarnessClaude
	svc := &Service{}
	origin := harnessCreationOrigin{Kind: "issue", TriggerSource: "manual"}
	for _, toggle := range []bool{false, true} {
		policy := store.GetHarnessCreationPolicyRow{HarnessOverflowEnabled: toggle}
		for _, tt := range []struct {
			name     string
			res      resolvedHarness
			explicit *Harness
			origin   harnessCreationOrigin
		}{
			{"one harness usable", resolvedHarness{Harness: HarnessClaude, DefaultUnset: true}, nil, origin},
			{"explicit harness", resolvedHarness{Harness: HarnessClaude, DefaultUnset: true, BothUsable: true}, &pin, origin},
			{"ineligible origin", resolvedHarness{Harness: HarnessClaude, DefaultUnset: true, BothUsable: true}, nil, harnessCreationOrigin{Kind: "task", TriggerSource: "manual"}},
		} {
			t.Run(tt.name, func(t *testing.T) {
				got, pick, err := svc.evaluateCreationOverflow(context.Background(), nil, uuid.New(), tt.explicit, tt.res, policy, tt.origin)
				if err != nil || pick != nil {
					t.Fatalf("err=%v pick=%v", err, pick)
				}
				if got.Decision.Reason != harnessReasonExcluded || got.Harness != HarnessClaude {
					t.Fatalf("toggle=%t reason=%s harness=%s", toggle, got.Decision.Reason, got.Harness)
				}
			})
		}
	}
	// A set default with the toggle off is unchanged: off.
	got, _, err := svc.evaluateCreationOverflow(context.Background(), nil, uuid.New(), nil, resolvedHarness{Harness: HarnessClaude, BothUsable: true}, store.GetHarnessCreationPolicyRow{}, origin)
	if err != nil || got.Decision.Reason != harnessReasonOff {
		t.Fatalf("set default, toggle off: reason=%v err=%v", got.Decision, err)
	}
}
