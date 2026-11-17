package apitypes

import "time"

// Run environment variable states (PRD #4416). A live variable is enabled or
// disabled; a secret some run holds a receipt for is kept as pending deletion.
const (
	RunEnvStateEnabled         = "enabled"
	RunEnvStateDisabled        = "disabled"
	RunEnvStatePendingDeletion = "pending_deletion"
)

// RunEnvVarDTO is one variable of a repo as the browser and the CLI see it. A secret's
// value appears in no response, ever: Value is null for it. For a plain variable Value
// is null only while the vault is locked, and ValueWithheld says so.
type RunEnvVarDTO struct {
	Name          string     `json:"name"`
	Secret        bool       `json:"secret"`
	Description   string     `json:"description"`
	Value         *string    `json:"value"`
	ValueWithheld bool       `json:"value_withheld"`
	State         string     `json:"state"`
	Rev           int32      `json:"rev"`
	DisabledAt    *time.Time `json:"disabled_at"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// RunEnvCountsDTO is the panel's "N of 50, D disabled, P pending deletion". Total
// counts what the per-scope limit counts: live rows (enabled and disabled) plus rows
// pending deletion.
type RunEnvCountsDTO struct {
	Total           int `json:"total"`
	Disabled        int `json:"disabled"`
	PendingDeletion int `json:"pending_deletion"`
	Limit           int `json:"limit"`
}

// RunEnvListDTO is the body of GET /api/repos/{id}/env.
type RunEnvListDTO struct {
	Variables   []RunEnvVarDTO  `json:"variables"`
	Counts      RunEnvCountsDTO `json:"counts"`
	VaultLocked bool            `json:"vault_locked"`
}

// RunEnvWriteDTO is the body of a PUT that created or changed one variable.
type RunEnvWriteDTO struct {
	Variable RunEnvVarDTO `json:"variable"`
}

// RunEnvDeleteDTO answers DELETE: State is "deleted" (200) or "pending_deletion" (202).
type RunEnvDeleteDTO struct {
	State string `json:"state"`
}
