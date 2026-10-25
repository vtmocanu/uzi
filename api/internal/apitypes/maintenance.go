package apitypes

// Instance maintenance (intake pause) wire types (PRD #3454).
//
// MaintenanceStateDTO is the whole body of GET /api/maintenance, readable by any
// authenticated user (session or CLI token). It deliberately carries no attribution:
// who paused intake is admin-only (MaintenanceAdminStateDTO.ByLabel).
type MaintenanceStateDTO struct {
	Active bool `json:"active"`
	// Since is the RFC3339 time the pause began; null when inactive or the singleton row
	// is missing (which reads as active, fail closed).
	Since *string `json:"since"`
	// Reason is the admin-supplied sanitized reason (at most 280 characters), or null.
	Reason *string `json:"reason"`
}

// MaintenanceAdminStateDTO is the admin view of the state: the public fields plus the label
// of the admin who set it.
type MaintenanceAdminStateDTO struct {
	Active  bool    `json:"active"`
	Since   *string `json:"since"`
	Reason  *string `json:"reason"`
	ByLabel *string `json:"by_label"`
}

// MaintenanceItemDTO is one run row of the drain readout. Tag is "progressing" or
// "needs_owner" for held and custody items and null elsewhere; HoldID is set for custody
// items only.
type MaintenanceItemDTO struct {
	RunID      string  `json:"run_id"`
	Kind       string  `json:"kind"`
	Status     string  `json:"status"`
	OwnerLabel string  `json:"owner_label"`
	Repo       *string `json:"repo"`
	Since      *string `json:"since"`
	Tag        *string `json:"tag"`
	HoldID     *string `json:"hold_id"`
}

// MaintenanceSectionDTO is an exact Count plus the oldest (at most 50) Items; Items is never null.
type MaintenanceSectionDTO struct {
	Count int                  `json:"count"`
	Items []MaintenanceItemDTO `json:"items"`
}

// MaintenanceWorkerDTO is an ephemeral worker bound to a queued run the pause will not admit.
type MaintenanceWorkerDTO struct {
	WorkerID   string  `json:"worker_id"`
	OwnerLabel string  `json:"owner_label"`
	RunID      string  `json:"run_id"`
	Since      *string `json:"since"`
}

// MaintenanceWorkersDTO is the idle-bound-worker section; Items is never null.
type MaintenanceWorkersDTO struct {
	Count int                    `json:"count"`
	Items []MaintenanceWorkerDTO `json:"items"`
}

// MaintenanceReadoutDTO is the snapshot-consistent drain readout. Clear is true exactly when
// active, held and custody are all empty; queued and idle_bound_workers are informational.
type MaintenanceReadoutDTO struct {
	AsOf             string                `json:"as_of"`
	Clear            bool                  `json:"clear"`
	Active           MaintenanceSectionDTO `json:"active"`
	Held             MaintenanceSectionDTO `json:"held"`
	Custody          MaintenanceSectionDTO `json:"custody"`
	Queued           MaintenanceSectionDTO `json:"queued"`
	IdleBoundWorkers MaintenanceWorkersDTO `json:"idle_bound_workers"`
}

// MaintenanceAdminDTO is the body of GET and PUT /api/admin/maintenance.
type MaintenanceAdminDTO struct {
	State   MaintenanceAdminStateDTO `json:"state"`
	Readout MaintenanceReadoutDTO    `json:"readout"`
}
