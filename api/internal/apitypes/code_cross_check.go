package apitypes

import (
	"encoding/json"
	"time"
)

// CodeCrossCheck is owner-only detail. It is never a plan approval.
type CodeCrossCheck struct {
	Stage               string          `json:"stage"`
	Round               int32           `json:"round"`
	CandidateGeneration int64           `json:"candidate_generation"`
	BaseCommit          *string         `json:"base_commit"`
	HeadCommit          *string         `json:"head_commit"`
	CandidateDigest     string          `json:"candidate_digest"`
	CheckerRunID        *string         `json:"checker_run_id"`
	CheckerHarness      *string         `json:"checker_harness"`
	CheckerModel        *string         `json:"checker_model"`
	CheckerEffort       *string         `json:"checker_effort"`
	Outcome             string          `json:"outcome"`
	ReasonClass         *string         `json:"reason_class"`
	Findings            json.RawMessage `json:"findings"`
	InterruptedAt       *time.Time      `json:"interrupted_at"`
	FinalizedAt         *time.Time      `json:"finalized_at"`
	DeadlineAt          time.Time       `json:"deadline_at"`
}
