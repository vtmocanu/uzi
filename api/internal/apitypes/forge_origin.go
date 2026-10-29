package apitypes

import "time"

// ForgeOriginCooldownDTO is an advisory owner-only snapshot. Expiry permits a
// retry; it does not verify recovery or grant network, execution or custody authority.
type ForgeOriginCooldownDTO struct {
	Origin        string    `json:"origin"`
	CooledUntil   time.Time `json:"cooled_until"`
	HalfOpenRunID *string   `json:"half_open_run_id"`
	// State is cooling, half_open, retry_eligible_unverified, or
	// legacy_serialized_until_upgrade or reserved_unverified (no verified run ID).
	// Detail is fixed server wording.
	State  string `json:"state"`
	Detail string `json:"detail"`
}
