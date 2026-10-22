package apitypes

// ModelSettingCellDTO is one harness's cell of a model setting (PRD #3245): the
// stored lane value plus what "Default" means and what resolves for this user.
// A setting's cells are an array, always ordered claude then codex. Model is the
// stored lane value (null = Default); DefaultModel/DefaultSource are the walk
// without the user tier; ResolvedModel/Source are the full walk. The *Source
// fields are the shared provenance tokens (user_default, instance_default,
// builtin), labelled per surface by fixtures/model-settings/contract.json.
type ModelSettingCellDTO struct {
	Harness       string  `json:"harness"`
	Model         *string `json:"model"`
	DefaultModel  string  `json:"default_model"`
	DefaultSource string  `json:"default_source"`
	ResolvedModel string  `json:"resolved_model"`
	Source        string  `json:"source"`
}
