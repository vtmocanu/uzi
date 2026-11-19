package apitypes

import "github.com/google/uuid"

// RunEnvRedactVersion is one secret version the worker registers for redaction and the
// exact-value scan (PRD #4416). It mirrors workersvc.ClaimRunEnvRedact on the wire; the
// maintenance route answers with it so a worker that no longer holds a run's claim can still
// protect a later publish. Never logged: Value is the usable secret.
type RunEnvRedactVersion struct {
	VarID uuid.UUID `json:"var_id"`
	Rev   int32     `json:"rev"`
	Scope string    `json:"scope"`
	Name  string    `json:"name"`
	Value string    `json:"value"`
}

// RunEnvRedactResponse is the body of GET /api/worker/runs/{id}/run-env-redact. Protected is
// false when the run holds no secret receipts (nothing to register); otherwise Versions lists
// every secret version it holds. Pinned by fixtures/claims/run-env-redact-response.json.
type RunEnvRedactResponse struct {
	Protected bool                  `json:"protected"`
	Versions  []RunEnvRedactVersion `json:"versions"`
}
