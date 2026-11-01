package workersvc

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/codexselect"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func overflowMeasured(score float64) overflowEvidence {
	return overflowEvidence{Measured: pgtype.Float8{Float64: score, Valid: true}, Destination: true}
}

func TestHarnessOverflowThresholds(t *testing.T) {
	tests := []struct {
		name             string
		preferred, other float64
		reason           string
	}{
		{"below preferred ceiling", 19.99, 40, "overflow"},
		{"above preferred ceiling", 20.01, 40, "preferred"},
		{"at preferred ceiling", 20, 40, "preferred"},
		{"below destination floor", 0, 29.99, "preferred"},
		{"at destination floor", 0, 30, "overflow"},
		{"above destination floor", 0, 30.01, "overflow"},
		{"below advantage", 19, 33.99, "preferred"},
		{"at advantage", 19, 34, "overflow"},
		{"above advantage", 19, 34.01, "overflow"},
		{"equal usage", 10, 10, "preferred"},
		{"wrong direction", 80, 10, "preferred"},
	}
	for _, direction := range []Harness{HarnessClaude, HarnessCodex} {
		for _, tt := range tests {
			t.Run(string(direction)+"/"+tt.name, func(t *testing.T) {
				d := harnessCreationDecision{Preferred: direction, PreferredEvidence: overflowMeasured(tt.preferred), OtherEvidence: overflowMeasured(tt.other)}
				d.Reason = overflowReason(true, true, d.PreferredEvidence, d.OtherEvidence)
				if d.Reason != tt.reason {
					t.Fatalf("reason=%s want %s", d.Reason, tt.reason)
				}
				want := direction
				if tt.reason == "overflow" {
					if direction == HarnessClaude {
						want = HarnessCodex
					} else {
						want = HarnessClaude
					}
				}
				if d.chosen() != want {
					t.Fatalf("chosen=%s want %s", d.chosen(), want)
				}
			})
		}
	}
	held := overflowMeasured(90)
	held.Held = true
	if got := overflowReason(true, true, held, overflowMeasured(30)); got != "overflow" {
		t.Fatalf("held preferred: %s", got)
	}
	if got := overflowReason(true, true, overflowMeasured(0), held); got != "preferred" {
		t.Fatalf("held destination: %s", got)
	}
	if got := overflowReason(true, true, overflowEvidence{}, overflowMeasured(90)); got != "unknown_usage" {
		t.Fatalf("unknown preferred: %s", got)
	}
	if got := overflowReason(true, true, overflowMeasured(0), overflowEvidence{}); got != "unknown_usage" {
		t.Fatalf("unknown destination: %s", got)
	}
	if got := overflowReason(false, false, overflowEvidence{}, overflowEvidence{}); got != "off" {
		t.Fatalf("off precedence: %s", got)
	}
	if got := overflowReason(true, false, overflowEvidence{}, overflowEvidence{}); got != "excluded" {
		t.Fatalf("exclusion precedence: %s", got)
	}
}

func TestHarnessOverflowOriginExclusions(t *testing.T) {
	pin := HarnessClaude
	model := "opus"
	empty := ""
	base := harnessCreationOrigin{Kind: "issue", TriggerSource: "manual"}
	for _, source := range []string{"manual", "autopilot", "schedule"} {
		o := base
		o.TriggerSource = source
		if !o.eligible(store.GetHarnessCreationPolicyRow{}) {
			t.Fatalf("eligible source %s rejected", source)
		}
	}
	tests := []struct {
		name   string
		origin harnessCreationOrigin
		p      store.GetHarnessCreationPolicyRow
	}{
		{"unknown", harnessCreationOrigin{}, store.GetHarnessCreationPolicyRow{}},
		{"chat", harnessCreationOrigin{Kind: "chat", TriggerSource: "manual"}, store.GetHarnessCreationPolicyRow{}},
		{"task", harnessCreationOrigin{Kind: "task", TriggerSource: "manual"}, store.GetHarnessCreationPolicyRow{}},
		{"unknown trigger", harnessCreationOrigin{Kind: "issue", TriggerSource: "other"}, store.GetHarnessCreationPolicyRow{}},
		{"explicit manual", harnessCreationOrigin{Kind: "issue", TriggerSource: "manual", Explicit: &pin}, store.GetHarnessCreationPolicyRow{}},
		{"explicit schedule", harnessCreationOrigin{Kind: "issue", TriggerSource: "schedule", Explicit: &pin}, store.GetHarnessCreationPolicyRow{}},
		{"label harness", harnessCreationOrigin{Kind: "issue", TriggerSource: "autopilot", Explicit: &pin}, store.GetHarnessCreationPolicyRow{}},
		{"model", harnessCreationOrigin{Kind: "issue", TriggerSource: "schedule", Model: &model}, store.GetHarnessCreationPolicyRow{}},
		{"run override", harnessCreationOrigin{Kind: "issue", TriggerSource: "manual", AnthropicOverride: true}, store.GetHarnessCreationPolicyRow{}},
		{"schedule override", harnessCreationOrigin{Kind: "issue", TriggerSource: "schedule", AnthropicOverride: true}, store.GetHarnessCreationPolicyRow{}},
		{"raw plan", base, store.GetHarnessCreationPolicyRow{PlanCrossCheckEnabled: true}},
		{"raw code", base, store.GetHarnessCreationPolicyRow{CodeCrossCheckEnabled: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.origin.eligible(tt.p) {
				t.Fatal("excluded origin admitted")
			}
		})
	}
	// Each excluded kind/source is named, rather than relying on "unknown" to
	// stand in for a known caller of the shared creation helper.
	for _, kind := range []string{"prompt", "mr_rework", "ci_fix", "task", "judge", "self_improve", "cross_check", "job", "chat", "resume", "unknown"} {
		t.Run("kind/"+kind, func(t *testing.T) {
			o := harnessCreationOrigin{Kind: kind, TriggerSource: "schedule"}
			if o.eligible(store.GetHarnessCreationPolicyRow{}) {
				t.Fatal("non-issue origin admitted")
			}
		})
	}
	for _, source := range []string{"chat", "resume", "", "unknown"} {
		t.Run("issue-source/"+source, func(t *testing.T) {
			o := harnessCreationOrigin{Kind: "issue", TriggerSource: source}
			if o.eligible(store.GetHarnessCreationPolicyRow{}) {
				t.Fatal("excluded issue source admitted")
			}
		})
	}
	base.Model = &empty
	if !base.eligible(store.GetHarnessCreationPolicyRow{}) {
		t.Fatal("empty model must inherit")
	}
}

func TestHarnessOverflowOffPreservesResolverMatrix(t *testing.T) {
	claude, codex, invalid := HarnessClaude, HarnessCodex, Harness("unknown")
	for _, explicit := range []*Harness{nil, &claude, &codex, &invalid} {
		for _, preference := range []*Harness{nil, &claude, &codex, &invalid} {
			for _, a := range []harnessAvailability{{}, {claudeUsable: true}, {codexUsable: true}, {claudeUsable: true, codexUsable: true}} {
				preferred, err := resolveHarness(resolveHarnessInput{explicit: explicit, userDefault: preference, avail: a})
				if err != nil {
					continue // The unchanged resolver's existing tests pin refusals.
				}
				// Extreme and unknown gauges cannot change a setting-off answer.
				for _, score := range []overflowEvidence{{}, overflowMeasured(0), overflowMeasured(100), {Held: true}} {
					d := harnessCreationDecision{Preferred: preferred, PreferredEvidence: score, OtherEvidence: overflowMeasured(100)}
					d.Reason = overflowReason(false, true, d.PreferredEvidence, d.OtherEvidence)
					if d.Reason != "off" || d.chosen() != preferred {
						t.Fatalf("off changed resolver answer: availability=%+v preferred=%s decision=%+v", a, preferred, d)
					}
				}
			}
		}
	}
}

func TestHarnessOverflowCodexEvidence(t *testing.T) {
	for _, reason := range []codexselect.Reason{codexselect.ReasonAuto, codexselect.ReasonBestOfPool, codexselect.ReasonPoolStale, codexselect.ReasonPoolExhausted, codexselect.ReasonPoolEmpty, "default"} {
		c := codexselect.Candidate{Score: 48, ScoreKnown: true}
		e := codexOverflowEvidence(c, reason)
		if !e.Measured.Valid || e.Measured.Float64 != 48 {
			t.Fatalf("%s lost actual measurement: %+v", reason, e)
		}
		want := reason == codexselect.ReasonAuto || reason == codexselect.ReasonBestOfPool || reason == "default"
		if e.Destination != want {
			t.Fatalf("%s destination=%v", reason, e.Destination)
		}
		c.CapacityExcluded = true
		e = codexOverflowEvidence(c, reason)
		if e.comparison().Float64 != 0 || !e.comparison().Valid || e.Measured.Float64 != 48 {
			t.Fatalf("held %s: %+v", reason, e)
		}
		c.ScoreKnown = false
		e = codexOverflowEvidence(c, reason)
		if e.Measured.Valid || !e.comparison().Valid {
			t.Fatalf("unknown held %s: %+v", reason, e)
		}
	}
}

func TestHarnessOverflowClaudeAggregation(t *testing.T) {
	held := overflowMeasured(70)
	held.Held = true
	unknownHeld := overflowEvidence{Held: true}
	unknown := overflowEvidence{}
	tests := []struct {
		name        string
		rows        []overflowEvidence
		destination bool
		known       bool
		score       float64
		usable      bool
	}{
		{"preferred max", []overflowEvidence{overflowMeasured(10), overflowMeasured(80)}, false, true, 80, true},
		{"destination min", []overflowEvidence{overflowMeasured(10), overflowMeasured(80)}, true, true, 10, true},
		{"empty", nil, false, false, 0, false},
		{"unknown preferred", []overflowEvidence{overflowMeasured(80), unknown}, false, false, 0, true},
		{"unknown destination", []overflowEvidence{overflowMeasured(80), unknown}, true, false, 0, true},
		{"all held", []overflowEvidence{held, unknownHeld}, false, true, 0, true},
		{"held and unknown", []overflowEvidence{held, unknown}, false, false, 0, true},
		{"held destination", []overflowEvidence{held, overflowMeasured(80)}, true, true, 0, false},
		{"unknown held destination", []overflowEvidence{held, unknown}, true, false, 0, false},
		{"healthy with held preferred", []overflowEvidence{held, overflowMeasured(80)}, false, true, 80, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := aggregateClaudeOverflow(tt.rows, tt.destination)
			c := e.comparison()
			if c.Valid != tt.known || (tt.known && c.Float64 != tt.score) || e.Destination != tt.usable {
				t.Fatalf("evidence=%+v comparison=%+v", e, c)
			}
		})
	}
}
