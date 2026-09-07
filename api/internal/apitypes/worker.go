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
	// Docker is the worker's DinD capability, mirroring HostedSize's semantics: null
	// for an external worker (docker not applicable), false for a hosted worker without
	// the rootless-DinD sidecar, true for a docker-capable hosted worker (PRD #83 M3).
	Docker *bool `json:"docker"`
	// Ephemeral marks an auto-provisioned, run-bound throwaway hosted worker (PRD #529):
	// the api created it for one unplaceable run and tears it down (drops the row →
	// controller reaps the pod) when that run finishes. Always false for an external or a
	// standing hosted worker. Display-only; the scheduler binds via workers.ephemeral_run_id,
	// which stays server-internal like hosted_generation.
	Ephemeral bool `json:"ephemeral"`
	// Capabilities is the worker's server-authoritative capability set (PRD #84 M1):
	// the Filter-ed union of its self-reported caps and its template-derived caps,
	// v1 vocabulary {docker, jvm}. Read-only display for the workers UI. Serialized by
	// stdlib encoding/json, so nullness rides on whether the slice is nil: pgx yields a
	// non-nil empty slice for the text[] column, so a worker with no capabilities
	// encodes as [] rather than null.
	Capabilities []string `json:"capabilities"`
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
	// OnlineSince is the api-owned anchor of when the worker became online (PRD #251);
	// null when offline or never online. Uptime is derived client-side as now − online_since.
	OnlineSince *time.Time `json:"online_since"`
	// DrainingSince is set (to when it was cordoned) while a hosted worker is
	// draining/cordoned: it finishes its in-flight runs but claims nothing new
	// (PRD #422/#496). null for a worker that will claim normally. Hosted-only by
	// construction — every non-null write is WHERE kind='hosted'.
	DrainingSince *time.Time `json:"draining_since"`
	CreatedAt     time.Time  `json:"created_at"`
	// Derived upgrade health (PRD #113): "up_to_date" | "outdated" | "unknown" |
	// "upgrading" | "upgrade_failed". Computed at read time from Version against the
	// control plane's release — never stored, so it cannot go stale against the row
	// it describes. UpgradeDetail is the human sentence behind the badge ("running
	// 0.11.0, target 0.11.7"), null in the steady up_to_date case where there is
	// nothing to explain.
	//
	// Version is written ONLY at register, so a worker that is offline mid-roll keeps
	// reporting its old version — which is why "upgrading"/"upgrade_failed" cannot come
	// from this field and arrive from the controller's roll report instead.
	UpgradeStatus string  `json:"upgrade_status"`
	UpgradeDetail *string `json:"upgrade_detail"`
	// UpgradeTarget is the coordinate this worker was compared AGAINST — the
	// controller's rolled tag for a hosted worker with a fresh report (Decision 9,
	// because values.yaml may pin the worker image independently of the api's release),
	// otherwise the control plane's own version.
	//
	// On the wire so the UI can state the divergence when a hosted target sits below
	// the control-plane version. That divergence is a supported operation (a deliberate
	// pin) AND the shape of a fleet-wide alert-suppression attack, and the api cannot
	// tell them apart — so it is surfaced rather than judged. Empty when the control
	// plane has no version stamp, i.e. when classification is off entirely.
	UpgradeTarget string `json:"upgrade_target"`
	// The blocking container and the k8s waiting REASON behind an upgrade_failed, split
	// out from UpgradeDetail so the UI can key a lookup on them rather than parsing a
	// sentence. Null for every other status.
	//
	// The reason ONLY — `state.waiting.message` is deliberately never sent. Reason is a
	// short k8s enum; message is free text that routinely carries filesystem paths,
	// image references and Secret names, and this field reaches a browser badge and a
	// terminal via the CLI.
	UpgradeBlockingContainer *string `json:"upgrade_blocking_container"`
	UpgradeBlockingReason    *string `json:"upgrade_blocking_reason"`
	// The blocking container's last exit code, when it has one. Null for "never
	// terminated", which is a different fact from "exited 0".
	//
	// On the wire because it DISCRIMINATES causes the reason alone conflates: a
	// seed-nix CrashLoopBackOff is produced both by a permissions error unpacking the
	// nix store and by that volume running out of space, and the exit code is what tells
	// them apart. Without it the UI can only name both and guess.
	UpgradeLastExitCode *int32 `json:"upgrade_last_exit_code"`
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
	// Latest per-volume disk sample (PRD #837 M1), all null until the worker reports one
	// (and re-nulled if it stops). Used + total bytes for the /nix and data volumes; a
	// volume's pair is null when its statfs failed or the mount is absent (dev/compose
	// has no /nix). Same display-only, last-known-dimmed-by-freshness contract as the mem
	// fields above.
	StatsDiskNixBytes       *int64 `json:"stats_disk_nix_bytes"`
	StatsDiskNixTotalBytes  *int64 `json:"stats_disk_nix_total_bytes"`
	StatsDiskDataBytes      *int64 `json:"stats_disk_data_bytes"`
	StatsDiskDataTotalBytes *int64 `json:"stats_disk_data_total_bytes"`
	// Which Anthropic credential this worker's RUN-lane claims spend (PRD #104 M3).
	// Both null means "unbound": the worker spends its owner's default token, which
	// is every worker's state until someone binds one. The label rides alongside the
	// id so a client can render "spends: console-key" without a second lookup — it
	// is a name, never the credential, and the token value continues to appear in no
	// response at all.
	//
	// Chat runs on a bound worker still spend the DEFAULT (D1): the binding is a
	// property of the run lane, not of the worker's every activity.
	AnthropicSecretID    *string `json:"anthropic_secret_id"`
	AnthropicSecretLabel *string `json:"anthropic_secret_label"`
	// AnthropicBindMode is how this worker's run-lane claims choose a credential
	// (PRD #111 M3): "default" (the owner's default), "pinned" (the id above), or
	// "auto" (the selector picks from the owner's opted-in pool at claim time).
	//
	// 🔴 IT IS THE EFFECTIVE MODE, NOT THE STORED COLUMN, and the difference is the
	// whole reason a client can trust it. 00078's FK nulls anthropic_secret_id when
	// the credential is deleted and deliberately leaves the mode alone, so the
	// database legitimately holds "pinned" with no id — a worker that D9 resolves as
	// default. Shipping the raw column would let every client render a pin to a token
	// that no longer exists, and each of them would have to re-derive the same rule to
	// avoid it. The server applies it once instead, so "pinned" here always has an id
	// beside it.
	//
	// THE COST, NAMED SO IT IS NOT READ AS AN OVERSIGHT: "you stored pinned" is
	// deliberately UNRECOVERABLE through this API. There is no field that reports the
	// raw column, and adding one would recreate the hazard — a client could then
	// render the stored value and show a pin to a deleted token. It is not
	// information a user is missing, because D9 makes such a worker BE on the
	// default: the mode it resolves under is the only one that describes what its
	// next claim will spend. Do not "fix" this by exposing the stored value.
	AnthropicBindMode string `json:"anthropic_bind_mode"`
}

// AdminWorkerDTO is a worker plus its owner email for the admin Agents-status
// page.
type AdminWorkerDTO struct {
	WorkerDTO
	OwnerEmail string `json:"owner_email"`
}
