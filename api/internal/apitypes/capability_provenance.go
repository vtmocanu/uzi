package apitypes

// CapabilityTrigger is an untrusted, display-only detector observation.
type CapabilityTrigger struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type CapabilityInferenceObservation struct {
	Triggers  []CapabilityTrigger `json:"triggers"`
	Truncated bool                `json:"truncated"`
}

type CapabilityProvenanceItem struct {
	Capability       string              `json:"capability"`
	Sources          []string            `json:"sources"`
	Required         bool                `json:"required"`
	InferenceIgnored bool                `json:"inference_ignored"`
	Triggers         []CapabilityTrigger `json:"triggers"`
	Truncated        bool                `json:"truncated"`
}

type CapabilityProvenance struct {
	HintsKnown     bool                       `json:"hints_known"`
	InferenceKnown bool                       `json:"inference_known"`
	Capabilities   []CapabilityProvenanceItem `json:"capabilities"`
}
