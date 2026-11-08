package agenttmpl

import "strings"

// Provenance tokens ResolveWorkerDefaultModel returns.
const (
	WorkerDefaultSourceUserDefault = "user_default"
	WorkerDefaultSourceTemplate    = "template"
	WorkerDefaultSourceBuiltin     = "builtin"
)

// Built-in task-review models per harness, pinned to fixtures/model-settings/task-review.json
// builtins (TestTaskReviewBuiltinsMatchFixture) and to the agent's CODEX_TASK_REVIEW_MODEL. The
// Claude review pass sends no model (the SDK default).
const (
	ClaudeTaskReviewModel = ""
	CodexTaskReviewModel  = "gpt-6-sol"
)

// ResolveWorkerDefaultModel is the model a worker uses for a lead-lane or checker run when the
// claim carries no model, with the source of that model. It mirrors what the worker does:
//
//   - a set lane (non-nil) wins. A Codex lane of "" resolves to DefaultCodexModel (the worker's
//     render drops ""); a Claude lane holding a Codex role id is treated as unset (#3245).
//   - otherwise the lead template's pin for the harness, filtered through PinForHarness and
//     ValidateModel, wins.
//   - otherwise the built-in: DefaultCodexModel for Codex, "" (the SDK default) for Claude.
//
// The returned source is one of the WorkerDefaultSource* tokens. lane is the owner's lane for the
// harness, templateClaude/templateCodex are the lead template's per-harness pins; any may be nil.
func ResolveWorkerDefaultModel(harness string, lane, templateClaude, templateCodex *string) (model, source string) {
	if lane != nil {
		switch {
		case harness == "codex":
			if *lane == "" {
				return DefaultCodexModel, WorkerDefaultSourceBuiltin
			}
			return *lane, WorkerDefaultSourceUserDefault
		case !CodexRoleModels[strings.TrimSpace(*lane)]:
			return *lane, WorkerDefaultSourceUserDefault
		}
	}
	var claudePin, codexPin string
	if templateClaude != nil {
		claudePin = *templateClaude
	}
	if templateCodex != nil {
		codexPin = *templateCodex
	}
	if m, err := ValidateModel(PinForHarness(harness, claudePin, codexPin)); err == nil && m != "" {
		return m, WorkerDefaultSourceTemplate
	}
	if harness == "codex" {
		return DefaultCodexModel, WorkerDefaultSourceBuiltin
	}
	return "", WorkerDefaultSourceBuiltin
}
