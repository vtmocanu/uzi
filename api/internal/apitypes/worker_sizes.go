package apitypes

import "time"

// WorkerSizeSpec is a hosted worker size's six resolved integers (PRD #4213): CPU in
// millicores, memory in MiB, disks in GiB. It is the unit every size display reads, so a
// client never parses a quantity string.
type WorkerSizeSpec struct {
	CPURequestMillicores int64 `json:"cpu_request_millicores"`
	CPULimitMillicores   int64 `json:"cpu_limit_millicores"`
	MemoryRequestMiB     int64 `json:"memory_request_mib"`
	MemoryLimitMiB       int64 `json:"memory_limit_mib"`
	DataGiB              int64 `json:"data_gib"`
	DinDDataGiB          int64 `json:"dind_data_gib"`
}

// HostedConfigSizeDTO is one entry of GET /api/workers/hosted/config sizes: the name plus
// the six integers, in display order.
type HostedConfigSizeDTO struct {
	Name string `json:"name"`
	WorkerSizeSpec
}

// AdminWorkerSizeDTO is one row of GET /api/admin/worker-sizes. Shipped is the builtin
// default for a builtin size and null for an admin-added one.
type AdminWorkerSizeDTO struct {
	Name string `json:"name"`
	WorkerSizeSpec
	DisplayOrder int32           `json:"display_order"`
	Builtin      bool            `json:"builtin"`
	Customized   bool            `json:"customized"`
	Shipped      *WorkerSizeSpec `json:"shipped"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// WorkerSizeCeilingDTO is a binding ceiling: the smallest applicable integer and the chart
// value an operator changes to move it.
type WorkerSizeCeilingDTO struct {
	Value     int64  `json:"value"`
	ChartPath string `json:"chart_path"`
}

// WorkerSizeCeilingsDTO are the ceilings a size is validated against. A null entry means no
// ceiling applies to that field (only the absolute caps do).
type WorkerSizeCeilingsDTO struct {
	CPULimitMillicores *WorkerSizeCeilingDTO `json:"cpu_limit_millicores"`
	MemoryLimitMiB     *WorkerSizeCeilingDTO `json:"memory_limit_mib"`
	DataGiB            *WorkerSizeCeilingDTO `json:"data_gib"`
	DinDDataGiB        *WorkerSizeCeilingDTO `json:"dind_data_gib"`
	// DinDOverridden is true when workers.docker.dindDataSize overrides the per-size DinD
	// claim, in which case dind_data_gib has no PVC ceiling.
	DinDOverridden bool `json:"dind_overridden"`
	// EnabledTiers lists the tiers the ceilings were minimized over; null when the api does
	// not know (UZI_WORKER_SIZE_TIERS unset), in which case every present ceiling applies.
	EnabledTiers []string `json:"enabled_tiers"`
}

// WorkerSizeWarningDTO is a non-blocking note (disk_lowered).
type WorkerSizeWarningDTO struct {
	Kind    string `json:"kind"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

// WorkerSizeAdvisoryDTO is a quota advisory for one size and tier. Tier is empty when the
// enabled tiers are unknown. kind is quota_unavailable, quota_zero or quota_exceeded;
// quota_unavailable means the api cannot compare, never that the size fits.
type WorkerSizeAdvisoryDTO struct {
	Size    string `json:"size"`
	Tier    string `json:"tier"`
	Kind    string `json:"kind"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

// AdminWorkerSizesDTO is the GET /api/admin/worker-sizes response.
type AdminWorkerSizesDTO struct {
	Sizes    []AdminWorkerSizeDTO  `json:"sizes"`
	Ceilings WorkerSizeCeilingsDTO `json:"ceilings"`
	Editable bool                  `json:"editable"`
	// ReadOnlyReason is the human reason the editor is read-only, null when editable.
	ReadOnlyReason *string `json:"read_only_reason"`
	// ReadOnlyCode is the stable code of ReadOnlyReason (controller_report_missing,
	// controller_unsupported, controller_report_stale), null when editable.
	ReadOnlyCode *string                 `json:"read_only_code"`
	Advisories   []WorkerSizeAdvisoryDTO `json:"advisories"`
	MaxSizes     int                     `json:"max_sizes"`
	// Refusals lists hosted workers whose latest fresh controller report refused their size or
	// PVC (kind size_refused or pvc_rejected), keyed by the worker's hosted size. Always an
	// array, at most 50, sorted by size then worker name.
	Refusals []WorkerSizeRefusalDTO `json:"refusals"`
}

// WorkerSizeRefusalDTO is one hosted worker the controller refused to render or provision.
type WorkerSizeRefusalDTO struct {
	Size       string    `json:"size"`
	WorkerID   string    `json:"worker_id"`
	WorkerName string    `json:"worker_name"`
	Kind       string    `json:"kind"`
	Reason     string    `json:"reason"`
	ReportedAt time.Time `json:"reported_at"`
}

// AdminWorkerSizeWriteDTO is the response of a size edit or reset.
type AdminWorkerSizeWriteDTO struct {
	Size       AdminWorkerSizeDTO      `json:"size"`
	Warnings   []WorkerSizeWarningDTO  `json:"warnings"`
	Advisories []WorkerSizeAdvisoryDTO `json:"advisories"`
}
