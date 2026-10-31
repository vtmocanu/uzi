package leadcfg

import "strings"

// The run-level lead label table (PRD #3244 section 8), used by the run header, uzi run get and uzi run
// lead-config only. Settings surfaces use the (field, token) table of fixtures/model-settings, so the same
// token may read differently there. {H} is the harness name, Claude or Codex.
const (
	// SDKDefaultModelLabel labels a builtin model source on a row that sent no model (Claude SDK default).
	SDKDefaultModelLabel = "Claude SDK default"
	// NotObservedLabel labels an observation that has not been reported yet (NULL evidence).
	NotObservedLabel = "not reported yet"
	// EffortNotSentLabel labels a NULL observed effort with Claude evidence: no effort parameter is sent.
	EffortNotSentLabel = "not sent"
	// ObservedEffortOtherLabel labels the stored level "other".
	ObservedEffortOtherLabel = "an unrecognized level"
)

var modelLabels = map[string]string{
	"run_override":          "this run",
	"schedule_pin":          "fixed by schedule",
	"user_planning_default": "your {H} planning default",
	"user_default":          "your {H} default",
	"template":              "lead template",
	"builtin":               "built-in",
}

// An empty effort label means the source has no effort wording (schedules pin no effort).
var effortLabels = map[string]string{
	"run_override":          "this run",
	"schedule_pin":          "",
	"user_planning_default": "your {H} planning effort",
	"user_default":          "your {H} effort",
	"template":              "agent template",
	"builtin":               "built-in",
	"runtime_default":       "runtime default",
}

var evidenceLabels = map[string]string{
	"claude_init":           "Claude SDK init",
	"claude_get_settings":   "Claude SDK settings",
	"codex_thread_response": "Codex thread config",
	"codex_sent":            "sent to Codex",
	"codex_runtime_default": "Codex thread config",
	"unknown":               "not reported by the runtime",
}

// SourceLabel renders a source token for a setting ("model" or "effort") on a harness. The effort setting
// also accepts the mode token runtime_default. An unknown setting or token, and a source with no wording
// for that setting, render as "".
func SourceLabel(setting, token, harness string) string {
	var tmpl string
	switch setting {
	case "model":
		tmpl = modelLabels[token]
	case "effort":
		tmpl = effortLabels[token]
	}
	return strings.ReplaceAll(tmpl, "{H}", harnessName(harness))
}

// EvidenceLabel renders an observation evidence token. The empty token (NULL, not yet observed) renders
// as NotObservedLabel; an unknown token renders as "".
func EvidenceLabel(token string) string {
	if token == "" {
		return NotObservedLabel
	}
	return evidenceLabels[token]
}

func harnessName(h string) string {
	switch h {
	case HarnessClaude:
		return "Claude"
	case HarnessCodex:
		return "Codex"
	default:
		return h
	}
}
