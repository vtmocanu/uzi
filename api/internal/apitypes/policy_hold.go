package apitypes

import "time"

// OutcomePendingPolicyHold is server-classified delivery telemetry, never a stored hold reason.
const OutcomePendingPolicyHold = "codex_policy_hold_pending"

// PolicyHoldDTO describes the row-local preserving constraint and the latest
// applied episode. Published is worker-reported provenance, not server capture
// verification. Capture identity/durability remain the existing run fields.
type PolicyHoldDTO struct {
	PreserveRequired   bool       `json:"preserve_required"`
	PendingFresh       bool       `json:"pending_fresh"`
	OutcomeID          string     `json:"outcome_id"`
	ContinuationMode   string     `json:"continuation_mode"`
	ResolvedAt         *time.Time `json:"resolved_at"`
	ApplyingWorkerID   *string    `json:"applying_worker_id"`
	ApplyingGeneration int64      `json:"applying_generation"`
	SourceGeneration   int64      `json:"source_generation"`
	RefusalCount       int        `json:"refusal_count"`
	PolicyTag          *string    `json:"policy_tag"`
	Phase              *string    `json:"phase"`
	CorrelationID      *string    `json:"correlation_id"`
	Published          *bool      `json:"published"`
	// Current unresolved hold only; later ordinary capture cannot rewrite policy history.
	CapturedHead *string `json:"captured_head"`
	// HoldCause is the closed reason the latest episode was held (refused, attempt_path,
	// saved_work_missing, session_missing, transcript_unknown, provider_resume_rejected,
	// pending_conversion); fresh_start_uncertain is in the closed set but reserved, not currently
	// emitted; null for an episode recorded before the field.
	HoldCause *string `json:"hold_cause"`
	// ResumeModes lists the owner resume modes this exact episode accepts right now. Empty (never
	// null) unless the run is paused on an unresolved codex_policy_hold.
	ResumeModes []string `json:"resume_modes"`
	// RecoveryRequired is true when a required recovery head is recorded for the run.
	RecoveryRequired bool `json:"recovery_required"`
	// RecoveryVerifiedThisClaim is true when the worker holding the current claim generation has
	// recorded verification of the required recovery head.
	RecoveryVerifiedThisClaim bool `json:"recovery_verified_this_claim"`
}
