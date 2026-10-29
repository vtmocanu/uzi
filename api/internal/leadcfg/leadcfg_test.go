package leadcfg_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/leadcfg"
	"github.com/vtmocanu/uzi/api/internal/leadcfg/leadcfgtest"
)

func strs[T ~string](in []T) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = string(v)
	}
	return out
}

func TestVocabularyMatchesFixture(t *testing.T) {
	v := leadcfgtest.LoadContract(t).Vocabulary
	for name, pair := range map[string][2][]string{
		"phases":            {strs(leadcfg.Phases()), v.Phases},
		"model_sources":     {strs(leadcfg.ModelSources()), v.ModelSources},
		"effort_sources":    {strs(leadcfg.EffortSources()), v.EffortSources},
		"effort_modes":      {strs(leadcfg.EffortModes()), v.EffortModes},
		"model_evidence":    {strs(leadcfg.ModelEvidences()), v.ModelEvidence},
		"effort_evidence":   {strs(leadcfg.EffortEvidences()), v.EffortEvidence},
		"requested_efforts": {leadcfg.RequestedEfforts(), v.RequestedEfforts},
		"observed_efforts":  {leadcfg.ObservedEfforts(), v.ObservedEfforts},
	} {
		if !slices.Equal(pair[0], pair[1]) {
			t.Errorf("%s: Go %v, fixture %v", name, pair[0], pair[1])
		}
	}
	if !slices.Equal(v.Harnesses, []string{leadcfg.HarnessClaude, leadcfg.HarnessCodex}) {
		t.Errorf("harnesses: %v", v.Harnesses)
	}
}

func TestFixedAtMatchesFixture(t *testing.T) {
	c := leadcfgtest.LoadContract(t)
	if len(c.FixedAt) != len(leadcfg.ModelSources()) {
		t.Fatalf("fixed_at has %d entries, want one per model source", len(c.FixedAt))
	}
	for src, want := range c.FixedAt {
		if got := leadcfg.FixedAt(src); got != want {
			t.Errorf("FixedAt(%q) = %q, fixture %q", src, got, want)
		}
	}
	// The explicit pair the PRD names, independent of the file.
	for src, want := range map[string]string{"run_override": "create", "schedule_pin": "create", "user_default": "claim", "builtin": "claim"} {
		if got := leadcfg.FixedAt(src); got != want {
			t.Errorf("FixedAt(%q) = %q, want %q", src, got, want)
		}
	}
}

func TestLabelsMatchFixture(t *testing.T) {
	l := leadcfgtest.LoadContract(t).Labels
	visited := 0
	for _, h := range []string{"claude", "codex"} {
		hn := map[string]string{"claude": "Claude", "codex": "Codex"}[h]
		for tok, tmpl := range l.Model {
			visited++
			if got, want := leadcfg.SourceLabel("model", tok, h), strings.ReplaceAll(tmpl, "{H}", hn); got != want {
				t.Errorf("model %s %s: %q want %q", tok, h, got, want)
			}
		}
		for tok, tmpl := range l.Effort {
			visited++
			if got, want := leadcfg.SourceLabel("effort", tok, h), strings.ReplaceAll(tmpl, "{H}", hn); got != want {
				t.Errorf("effort %s %s: %q want %q", tok, h, got, want)
			}
		}
		for tok, tmpl := range l.EffortModes {
			visited++
			if got := leadcfg.SourceLabel("effort", tok, h); got != tmpl {
				t.Errorf("mode %s: %q want %q", tok, got, tmpl)
			}
		}
	}
	if visited == 0 {
		t.Fatal("no source labels visited")
	}
	for _, tok := range leadcfg.ModelSources() {
		if _, ok := l.Model[string(tok)]; !ok {
			t.Errorf("fixture has no model label for %s", tok)
		}
	}
	for _, tok := range leadcfg.EffortSources() {
		if _, ok := l.Effort[string(tok)]; !ok {
			t.Errorf("fixture has no effort label for %s", tok)
		}
	}
	for tok, want := range l.Evidence {
		if got := leadcfg.EvidenceLabel(tok); got != want {
			t.Errorf("evidence %s: %q want %q", tok, got, want)
		}
	}
	for _, tok := range append(strs(leadcfg.ModelEvidences()), strs(leadcfg.EffortEvidences())...) {
		if _, ok := l.Evidence[tok]; !ok {
			t.Errorf("fixture has no evidence label for %s", tok)
		}
	}
	if got := leadcfg.EvidenceLabel(""); got != l.NotObserved {
		t.Errorf("EvidenceLabel(NULL) = %q want %q", got, l.NotObserved)
	}
	if leadcfg.SDKDefaultModelLabel != l.ModelSDKDefault || leadcfg.NotObservedLabel != l.NotObserved ||
		leadcfg.EffortNotSentLabel != l.EffortNotSent || leadcfg.ObservedEffortOtherLabel != l.ObservedEffortOther {
		t.Errorf("fixed label constants drift from the fixture: %+v", l)
	}
	// Acceptance-example wording, independent of the file.
	if got := leadcfg.SourceLabel("model", "user_default", "claude"); got != "your Claude default" {
		t.Errorf("got %q", got)
	}
	if got := leadcfg.SourceLabel("effort", "user_default", "codex"); got != "your Codex effort" {
		t.Errorf("got %q", got)
	}
	if got := leadcfg.SourceLabel("effort", "schedule_pin", "codex"); got != "" {
		t.Errorf("schedule pins no effort, got %q", got)
	}
	if got := leadcfg.SourceLabel("nope", "user_default", "claude"); got != "" {
		t.Errorf("unknown setting: %q", got)
	}
}

// TestLeadLaneRegistryMatrix crosses every run kind of the registry with review target and egress
// binding. Claim provenance, the GetRun overlay and the SQL fence get their agreement assertions from
// later units, which reuse leadcfgtest.LaneMatrix.
func TestLeadLaneRegistryMatrix(t *testing.T) {
	cases := leadcfgtest.LaneMatrix(t)
	wantLead := 0
	for _, c := range cases {
		if got := leadcfg.LeadLane(c.Kind, c.ReviewTarget, c.EgressBound); got != c.Want {
			t.Errorf("LeadLane(%q, review=%v, egress=%v) = %v, want %v", c.Kind, c.ReviewTarget, c.EgressBound, got, c.Want)
		}
		if c.Want {
			wantLead++
		}
	}
	// Six lead kinds, each admitted only without a review target and an egress profile.
	if wantLead != 6 {
		t.Errorf("matrix admits %d cases, want 6", wantLead)
	}
	for _, k := range []string{"chat", "judge", "job", "cross_check"} {
		if leadcfg.LeadLane(k, false, false) {
			t.Errorf("%s must not be a lead lane", k)
		}
	}
	if leadcfg.LeadLane("not-a-kind", false, false) {
		t.Error("an unknown kind must not be a lead lane")
	}
}

func s(v string) *string { return &v }

func validReq() apitypes.LeadConfigReportRequest {
	gen := int64(1)
	return apitypes.LeadConfigReportRequest{
		ClaimGeneration: &gen, Phase: "plan", PhaseEntry: 1, Seq: 1,
		RequestedModel: s("opus"), RequestedModelSource: "user_default",
		RequestedEffort: s("medium"), RequestedEffortMode: "explicit", RequestedEffortSource: "builtin",
	}
}

func TestValidateReportAcceptsEveryVocabularyValue(t *testing.T) {
	v := leadcfgtest.LoadContract(t).Vocabulary
	for _, ph := range v.Phases {
		r := validReq()
		r.Phase = ph
		if _, err := leadcfg.ValidateReport(r); err != nil {
			t.Errorf("phase %s: %v", ph, err)
		}
	}
	for _, ms := range v.ModelSources {
		r := validReq()
		r.RequestedModelSource = ms
		if _, err := leadcfg.ValidateReport(r); err != nil {
			t.Errorf("model source %s: %v", ms, err)
		}
	}
	for _, es := range v.EffortSources {
		r := validReq()
		r.RequestedEffortSource = es
		if _, err := leadcfg.ValidateReport(r); err != nil {
			t.Errorf("effort source %s: %v", es, err)
		}
	}
	for _, lvl := range v.RequestedEfforts {
		r := validReq()
		r.RequestedEffort = s(lvl)
		if _, err := leadcfg.ValidateReport(r); err != nil {
			t.Errorf("requested effort %s: %v", lvl, err)
		}
	}
	for _, lvl := range v.ObservedEfforts {
		r := validReq()
		r.ObservedEffort, r.ObservedEffortEvidence = s(lvl), s("claude_init")
		if _, err := leadcfg.ValidateReport(r); err != nil {
			t.Errorf("observed effort %s: %v", lvl, err)
		}
	}
	for _, ev := range v.ModelEvidence {
		r := validReq()
		r.ObservedModelEvidence = s(ev)
		if ev != "unknown" {
			r.ObservedModel = s("claude-opus-4")
		}
		if _, err := leadcfg.ValidateReport(r); err != nil {
			t.Errorf("model evidence %s: %v", ev, err)
		}
	}
	for _, ev := range v.EffortEvidence {
		r := validReq()
		r.ObservedEffortEvidence = s(ev)
		if ev != "unknown" {
			r.ObservedEffort = s("high")
		} else {
			r.ObservedEffort = nil
		}
		if _, err := leadcfg.ValidateReport(r); err != nil {
			t.Errorf("effort evidence %s: %v", ev, err)
		}
	}
}

func TestValidateReportShapes(t *testing.T) {
	// Accepted shapes: requested-only, Claude not-sent, Claude SDK default, runtime-default effort, final row.
	ok := map[string]func(*apitypes.LeadConfigReportRequest){
		"requested only":    func(r *apitypes.LeadConfigReportRequest) {},
		"claude not sent":   func(r *apitypes.LeadConfigReportRequest) { r.ObservedEffortEvidence = s("claude_get_settings") },
		"sdk default model": func(r *apitypes.LeadConfigReportRequest) { r.RequestedModel, r.RequestedModelSource = nil, "builtin" },
		"runtime default": func(r *apitypes.LeadConfigReportRequest) {
			r.RequestedEffort, r.RequestedEffortMode = nil, "runtime_default"
		},
		"truncated final row": func(r *apitypes.LeadConfigReportRequest) { r.Truncated, r.Seq = true, 32 },
		"unknown model":       func(r *apitypes.LeadConfigReportRequest) { r.ObservedModelEvidence = s("unknown") },
		"unknown effort":      func(r *apitypes.LeadConfigReportRequest) { r.ObservedEffortEvidence = s("unknown") },
		"entry 8 seq 32":      func(r *apitypes.LeadConfigReportRequest) { r.PhaseEntry, r.Seq = 8, 32 },
		"model trimmed":       func(r *apitypes.LeadConfigReportRequest) { r.RequestedModel = s("  opus ") },
		"100 byte model":      func(r *apitypes.LeadConfigReportRequest) { r.RequestedModel = s(strings.Repeat("m", 100)) },
	}
	for name, mut := range ok {
		r := validReq()
		mut(&r)
		if _, err := leadcfg.ValidateReport(r); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	rep, err := leadcfg.ValidateReport(func() apitypes.LeadConfigReportRequest {
		r := validReq()
		r.RequestedModel = s("  opus ")
		return r
	}())
	if err != nil || rep.RequestedModel == nil || *rep.RequestedModel != "opus" {
		t.Errorf("model is stored trimmed: %+v %v", rep, err)
	}

	bad := map[string]struct {
		mut   func(*apitypes.LeadConfigReportRequest)
		field string
	}{
		"missing generation":       {func(r *apitypes.LeadConfigReportRequest) { r.ClaimGeneration = nil }, "claim_generation"},
		"zero generation":          {func(r *apitypes.LeadConfigReportRequest) { g := int64(0); r.ClaimGeneration = &g }, "claim_generation"},
		"bad phase":                {func(r *apitypes.LeadConfigReportRequest) { r.Phase = "review" }, "phase"},
		"phase_entry 0":            {func(r *apitypes.LeadConfigReportRequest) { r.PhaseEntry = 0 }, "phase_entry"},
		"phase_entry 9":            {func(r *apitypes.LeadConfigReportRequest) { r.PhaseEntry = 9 }, "phase_entry"},
		"seq 0":                    {func(r *apitypes.LeadConfigReportRequest) { r.Seq = 0 }, "seq"},
		"seq 33":                   {func(r *apitypes.LeadConfigReportRequest) { r.Seq = 33 }, "seq"},
		"truncated seq 31":         {func(r *apitypes.LeadConfigReportRequest) { r.Truncated, r.Seq = true, 31 }, "truncated"},
		"empty model":              {func(r *apitypes.LeadConfigReportRequest) { r.RequestedModel = s("  ") }, "requested_model"},
		"101 byte model":           {func(r *apitypes.LeadConfigReportRequest) { r.RequestedModel = s(strings.Repeat("m", 101)) }, "requested_model"},
		"control char model":       {func(r *apitypes.LeadConfigReportRequest) { r.RequestedModel = s("op\nus") }, "requested_model"},
		"bidi model":               {func(r *apitypes.LeadConfigReportRequest) { r.RequestedModel = s("op‮us") }, "requested_model"},
		"spaced model":             {func(r *apitypes.LeadConfigReportRequest) { r.RequestedModel = s("op us") }, "requested_model"},
		"null model not builtin":   {func(r *apitypes.LeadConfigReportRequest) { r.RequestedModel = nil }, "requested_model"},
		"bad model source":         {func(r *apitypes.LeadConfigReportRequest) { r.RequestedModelSource = "instance_default" }, "requested_model_source"},
		"bad effort":               {func(r *apitypes.LeadConfigReportRequest) { r.RequestedEffort = s("none") }, "requested_effort"},
		"bad mode":                 {func(r *apitypes.LeadConfigReportRequest) { r.RequestedEffortMode = "auto" }, "requested_effort_mode"},
		"explicit without effort":  {func(r *apitypes.LeadConfigReportRequest) { r.RequestedEffort = nil }, "requested_effort_mode"},
		"runtime default w effort": {func(r *apitypes.LeadConfigReportRequest) { r.RequestedEffortMode = "runtime_default" }, "requested_effort_mode"},
		"bad effort source":        {func(r *apitypes.LeadConfigReportRequest) { r.RequestedEffortSource = "schedule_pin" }, "requested_effort_source"},
		"bad model evidence":       {func(r *apitypes.LeadConfigReportRequest) { r.ObservedModelEvidence = s("claude_get_settings") }, "observed_model_evidence"},
		"model w/o evidence":       {func(r *apitypes.LeadConfigReportRequest) { r.ObservedModel = s("x") }, "observed_model_evidence"},
		"model w unknown": {func(r *apitypes.LeadConfigReportRequest) {
			r.ObservedModel, r.ObservedModelEvidence = s("x"), s("unknown")
		}, "observed_model_evidence"},
		"evidence w/o model": {func(r *apitypes.LeadConfigReportRequest) { r.ObservedModelEvidence = s("claude_init") }, "observed_model_evidence"},
		"bad observed effort": {func(r *apitypes.LeadConfigReportRequest) {
			r.ObservedEffort, r.ObservedEffortEvidence = s("persistent"), s("codex_sent")
		}, "observed_effort"},
		"bad effort evidence": {func(r *apitypes.LeadConfigReportRequest) {
			r.ObservedEffort, r.ObservedEffortEvidence = s("high"), s("codex_thread_response")
		}, "observed_effort_evidence"},
		"high NULL evidence": {func(r *apitypes.LeadConfigReportRequest) { r.ObservedEffort = s("high") }, "observed_effort_evidence"},
		"high unknown": {func(r *apitypes.LeadConfigReportRequest) {
			r.ObservedEffort, r.ObservedEffortEvidence = s("high"), s("unknown")
		}, "observed_effort_evidence"},
		"null codex_sent":    {func(r *apitypes.LeadConfigReportRequest) { r.ObservedEffortEvidence = s("codex_sent") }, "observed_effort_evidence"},
		"null codex_runtime": {func(r *apitypes.LeadConfigReportRequest) { r.ObservedEffortEvidence = s("codex_runtime_default") }, "observed_effort_evidence"},
	}
	for name, tc := range bad {
		r := validReq()
		tc.mut(&r)
		_, err := leadcfg.ValidateReport(r)
		fe, isFE := err.(*leadcfg.FieldError)
		if !isFE {
			t.Errorf("%s: want *FieldError, got %v", name, err)
			continue
		}
		if fe.Field != tc.field {
			t.Errorf("%s: field %q, want %q", name, fe.Field, tc.field)
		}
	}
}

func TestValidateReportNeverEchoesTheValue(t *testing.T) {
	secret := "SECRET-op\nus-" + strings.Repeat("z", 5)
	r := validReq()
	r.RequestedModel = &secret
	_, err := leadcfg.ValidateReport(r)
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "zzz") {
		t.Errorf("error echoes the value: %v", err)
	}
	r = validReq()
	r.Phase = "SECRET-phase"
	_, err = leadcfg.ValidateReport(r)
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("error echoes the value: %v", err)
	}
}

// TestContractCasesAreWellFormed guards the shared case lists the Go, agent and web readers iterate:
// unique ids, and the shapes (request cases carry a harness and an expect; display inputs carry a status,
// a harness and a preClaim) those readers decode.
func TestContractCasesAreWellFormed(t *testing.T) {
	c := leadcfgtest.LoadContract(t)
	if len(c.RequestCases) == 0 || len(c.DisplayCases) == 0 {
		t.Fatal("contract has no cases")
	}
	seen := map[string]bool{}
	for _, raw := range c.RequestCases {
		var rc struct {
			ID      string                     `json:"id"`
			Harness string                     `json:"harness"`
			Phase   string                     `json:"phase"`
			Expect  map[string]json.RawMessage `json:"expect"`
		}
		if err := json.Unmarshal(raw, &rc); err != nil {
			t.Fatal(err)
		}
		if rc.ID == "" || seen[rc.ID] {
			t.Errorf("request case id %q empty or duplicated", rc.ID)
		}
		seen[rc.ID] = true
		if !slices.Contains(c.Vocabulary.Harnesses, rc.Harness) || !slices.Contains(c.Vocabulary.Phases, rc.Phase) || rc.Expect["modelSource"] == nil {
			t.Errorf("request case %s is malformed", rc.ID)
		}
	}
	seen = map[string]bool{}
	for _, dc := range c.DisplayCases {
		if dc.ID == "" || seen[dc.ID] {
			t.Errorf("display case id %q empty or duplicated", dc.ID)
		}
		seen[dc.ID] = true
		var in struct {
			Status   string          `json:"status"`
			Harness  string          `json:"harness"`
			PreClaim map[string]any  `json:"preClaim"`
			LeadCfg  json.RawMessage `json:"leadConfig"`
		}
		if err := json.Unmarshal(dc.Input, &in); err != nil {
			t.Fatal(err)
		}
		if in.Status == "" || !slices.Contains(c.Vocabulary.Harnesses, in.Harness) || in.PreClaim["modelSource"] != "schedule_pin" {
			t.Errorf("display case %s has a malformed input", dc.ID)
		}
		if (dc.Expect.Chip == "") != (in.LeadCfg == nil) {
			t.Errorf("display case %s: a chip exists exactly when lead_config is present (rule 1)", dc.ID)
		}
	}
}
