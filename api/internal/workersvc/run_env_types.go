package workersvc

import "github.com/google/uuid"

// ClaimRunEnvVar is one live, enabled repo environment variable delivered in
// ClaimSecrets.RunEnv (PRD #4416, "Claim"). Value is the plaintext, secret or not; the
// struct is never logged. Mirrors ClaimRunEnvVar in agent/src/protocol.ts and is pinned by
// fixtures/claims/run-env-claim.json.
type ClaimRunEnvVar struct {
	VarID       uuid.UUID `json:"var_id"`
	Scope       string    `json:"scope"` // "repo" or "user"
	Rev         int       `json:"rev"`
	Name        string    `json:"name"`
	Value       string    `json:"value"`
	Secret      bool      `json:"secret"`
	Description string    `json:"description"`
}

// ClaimRunEnvRedact is one secret version delivered in ClaimSecrets.RunEnvRedact, or in the
// run-env redact maintenance response: for redaction and scanning only, so it carries no
// description. Never logged.
type ClaimRunEnvRedact struct {
	VarID uuid.UUID `json:"var_id"`
	Scope string    `json:"scope"`
	Rev   int       `json:"rev"`
	Name  string    `json:"name"`
	Value string    `json:"value"`
}

// RunEnvRedactResponse is the body of the worker maintenance endpoint that returns the
// redact-only versions a run holds receipts for after its claim (PRD #4416). Protected is
// false when the run's historical protection is unavailable; Versions is then absent.
// Pinned by fixtures/claims/run-env-redact-response.json.
type RunEnvRedactResponse struct {
	Protected bool                `json:"protected"`
	Versions  []ClaimRunEnvRedact `json:"versions,omitempty"`
}
