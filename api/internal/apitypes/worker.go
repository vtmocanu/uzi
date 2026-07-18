package apitypes

import "time"

// WorkerDTO is the browser/CLI view of a worker.
type WorkerDTO struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	// Kind is "external" (a worker its owner runs by hand) or "hosted" (one the
	// controller runs in the cluster, PRD #58). HostedSize is the S/M/L preset name,
	// null for an external worker. Together they are what lets the UI mark hosted
	// rows (Decision 10: status stays heartbeat-driven for both kinds).
	//
	// hosted_generation is deliberately absent: it is controller-internal reconcile
	// state, not browser state — the same rule that keeps session_id/last_seq off
	// RunDTO.
	Kind       string  `json:"kind"`
	HostedSize *string `json:"hosted_size"`
	// Busy is the any-kind non-terminal signal (PRD #42 Decision 10): true whenever
	// the worker holds ANY active run, chat included — so a lone active chat still
	// reads as busy even though active_runs (run-lane only) is 0.
	Busy bool `json:"busy"`
	// ActiveRuns is the worker's live count of active RUN-lane runs (chat excluded —
	// chat has its own session budget); MaxConcurrentRuns is its advertised slot cap
	// (null when unadvertised). Together they drive the "N/M runs" saturation badge
	// (PRD #42).
	ActiveRuns        int  `json:"active_runs"`
	MaxConcurrentRuns *int `json:"max_concurrent_runs"`
	// Worker template (PRD #18): the UI-declared choice and the worker's
	// self-reported value. Either may be null (no choice / older image); the UI
	// badges drift when both are set and differ.
	TemplateDeclared *string    `json:"template_declared"`
	TemplateReported *string    `json:"template_reported"`
	Version          *string    `json:"version"`
	LastHeartbeatAt  *time.Time `json:"last_heartbeat_at"`
	CreatedAt        time.Time  `json:"created_at"`
	// Latest container resource sample (PRD #49), all null until the worker reports
	// one (and re-nulled if it stops). StatsMemLimitBytes is null when the container
	// is unlimited or the sample came from the process fallback; StatsCPUPct is null
	// on the worker's first tick. StatsSource is "cgroup" or "process" (the UI labels
	// a process-source sample "worker process only"). Freshness is LastHeartbeatAt —
	// an offline worker's stats are last-known, dimmed by the client.
	StatsCPUPct        *float64 `json:"stats_cpu_pct"`
	StatsMemBytes      *int64   `json:"stats_mem_bytes"`
	StatsMemLimitBytes *int64   `json:"stats_mem_limit_bytes"`
	StatsSource        *string  `json:"stats_source"`
}

// AdminWorkerDTO is a worker plus its owner email for the admin Agents-status
// page.
type AdminWorkerDTO struct {
	WorkerDTO
	OwnerEmail string `json:"owner_email"`
}
