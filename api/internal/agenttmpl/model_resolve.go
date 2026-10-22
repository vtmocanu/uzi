package agenttmpl

import "strings"

// Provenance tokens of a resolved model: the shared #1703 vocabulary.
const (
	ModelSourceUser     = "user_default"
	ModelSourceInstance = "instance_default"
	ModelSourceBuiltin  = "builtin"
)

// Built-in judge models per harness. fixtures/model-settings/judge.json pins
// both (and settings.Defaults) to the same values.
const (
	DefaultClaudeJudgeModel = "opus"
	// Literal, not DefaultCodexModel: a later worker-default change must not move judges (PRD #3245 D1).
	DefaultCodexJudgeModel = "gpt-6.1-sol"
)

// ModelResolution is the result of walking the user, instance and built-in
// tiers. DefaultModel/DefaultSource are the same walk without the user tier
// (what "Default" means to this user). Dropped lists each non-blank candidate
// the walk rejected before the winner, in tier order.
type ModelResolution struct {
	Model         string
	Source        string
	DefaultModel  string
	DefaultSource string
	Dropped       []string
}

// ResolveTieredModel walks [user, instance, builtin], trimming each; the first
// non-blank value that accept approves wins.
func ResolveTieredModel(accept func(string) bool, user, instance, builtin string) ModelResolution {
	var r ModelResolution
	walk := func(tiers [3]string, skipUser bool) (model, source string, dropped []string) {
		sources := [3]string{ModelSourceUser, ModelSourceInstance, ModelSourceBuiltin}
		for i, raw := range tiers {
			if skipUser && i == 0 {
				continue
			}
			v := strings.TrimSpace(raw)
			if v == "" {
				continue
			}
			if accept(v) {
				return v, sources[i], dropped
			}
			dropped = append(dropped, v)
		}
		return "", "", dropped
	}
	tiers := [3]string{user, instance, builtin}
	r.Model, r.Source, r.Dropped = walk(tiers, false)
	r.DefaultModel, r.DefaultSource, _ = walk(tiers, true)
	return r
}

// ResolveJudgeModel resolves the judge model for a harness. Claude accepts any
// value that is not a Codex role id (custom Claude ids stay provider-validated,
// so a malformed stored token is passed through, as before); Codex accepts only
// curated ids, which keeps a Codex Model always curated. Any other harness uses
// the Claude rules.
func ResolveJudgeModel(harness, user, instance string) ModelResolution {
	if harness == "codex" {
		return ResolveTieredModel(func(m string) bool { return CuratedCodexModels[m] },
			user, instance, DefaultCodexJudgeModel)
	}
	return ResolveTieredModel(func(m string) bool { return !CodexRoleModels[m] },
		user, instance, DefaultClaudeJudgeModel)
}
