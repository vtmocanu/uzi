package apitypes

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"time"
)

// PolicyStopDecimal is a canonical positive int64 encoded as a JSON string.
type PolicyStopDecimal string

func (v PolicyStopDecimal) Validate() error {
	n, err := strconv.ParseInt(string(v), 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != string(v) {
		return errors.New("invalid positive decimal int64")
	}
	return nil
}
func (v PolicyStopDecimal) MarshalJSON() ([]byte, error) {
	// Response contracts record Go zero values as empty strings. Empty values
	// remain invalid on decode and in source-authority validation.
	if v == "" {
		return json.Marshal("")
	}
	if err := v.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(string(v))
}
func (v *PolicyStopDecimal) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	n := PolicyStopDecimal(s)
	if err := n.Validate(); err != nil {
		return err
	}
	*v = n
	return nil
}

var policyStopUUID = regexp.MustCompile("^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")
var policyStopDigest = regexp.MustCompile("^[0-9a-f]{64}$")

type PolicyRevisionRef struct {
	Source       string             `json:"source"`
	InputID      *PolicyStopDecimal `json:"input_id,omitempty"`
	CrossCheckID *string            `json:"cross_check_id,omitempty"`
}

func (v PolicyRevisionRef) Validate() error {
	switch v.Source {
	case "run_user_input":
		if v.InputID != nil && v.CrossCheckID == nil {
			return v.InputID.Validate()
		}
	case "plan_cross_check":
		if v.InputID == nil && v.CrossCheckID != nil && policyStopUUID.MatchString(*v.CrossCheckID) {
			return nil
		}
	}
	return errors.New("invalid revision reference")
}
func (v *PolicyRevisionRef) UnmarshalJSON(b []byte) error {
	type plain PolicyRevisionRef
	var n plain
	if err := policyStopClosedJSON(b, &n); err != nil {
		return err
	}
	if err := PolicyRevisionRef(n).Validate(); err != nil {
		return err
	}
	// Explicit null in the other union arm is an extra field too.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		return err
	}
	arm := "input_id"
	if n.Source == "plan_cross_check" {
		arm = "cross_check_id"
	}
	if !policyStopExactKeys(keys, "source", arm) {
		return errors.New("revision reference is not a closed union")
	}
	*v = PolicyRevisionRef(n)
	return nil
}

type PolicyStopGateContext struct {
	Revision       PolicyStopDecimal `json:"revision"`
	PresentationID string            `json:"presentation_id"`
	PayloadDigest  string            `json:"payload_digest"`
}
type PolicyRevisionSourceV1 struct {
	Version               int32                  `json:"version"`
	Gate                  *PolicyStopGateContext `json:"gate"`
	PrimaryRevision       *PolicyRevisionRef     `json:"primary_revision"`
	AssociatedCrossChecks []PolicyRevisionRef    `json:"associated_cross_checks"`
}

// ParsePolicyRevisionSourceV1 rejects unknown coverage, duplicate keys and references,
// unknown versions, and extra fields. An explicit empty association is verified absence.
func ParsePolicyRevisionSourceV1(b []byte) (PolicyRevisionSourceV1, error) {
	var v PolicyRevisionSourceV1
	if err := policyStopClosedJSON(b, &v); err != nil {
		return v, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return v, err
	}
	if !policyStopExactKeys(fields, "version", "gate", "primary_revision", "associated_cross_checks") || v.Version != 1 || v.AssociatedCrossChecks == nil {
		return v, errors.New("unknown revision source coverage")
	}
	if v.Gate != nil {
		var gateFields map[string]json.RawMessage
		if err := json.Unmarshal(fields["gate"], &gateFields); err != nil {
			return v, err
		}
		if !policyStopExactKeys(gateFields, "revision", "presentation_id", "payload_digest") || v.Gate.Revision.Validate() != nil || !policyStopUUID.MatchString(v.Gate.PresentationID) || !policyStopDigest.MatchString(v.Gate.PayloadDigest) {
			return v, errors.New("invalid revision source gate")
		}
	}
	seen := map[string]bool{}
	for _, ref := range v.AssociatedCrossChecks {
		if ref.Source != "plan_cross_check" || ref.Validate() != nil || seen[*ref.CrossCheckID] {
			return v, errors.New("invalid associated cross-check")
		}
		seen[*ref.CrossCheckID] = true
	}
	if v.PrimaryRevision != nil {
		if err := v.PrimaryRevision.Validate(); err != nil {
			return v, err
		}
		if v.PrimaryRevision.CrossCheckID != nil && seen[*v.PrimaryRevision.CrossCheckID] {
			return v, errors.New("duplicate revision source")
		}
	}
	return v, nil
}

// policyStopExactKeys rejects encoding/json case aliases as well as absent keys.
// Field counts alone cannot distinguish missing coverage from a duplicate alias.
func policyStopExactKeys(fields map[string]json.RawMessage, keys ...string) bool {
	if len(fields) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, present := fields[key]; !present {
			return false
		}
	}
	return true
}

// policyStopClosedJSON checks duplicate object keys recursively before strict decoding.
// Traversal is bounded by the supplied JSON bytes; it performs no retries or I/O.
func policyStopClosedJSON(b []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var walk func() error
	walk = func() error {
		t, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				s, ok := key.(string)
				if !ok || seen[s] {
					return errors.New("duplicate JSON field")
				}
				seen[s] = true
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid JSON delimiter")
		}
		_, err = d.Token()
		return err
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	d = json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	return d.Decode(dst)
}

// PolicyStopReceipt acknowledges one durable Stop decision; it is not plan approval.
type PolicyStopReceipt struct {
	Disposition         string              `json:"disposition"`
	ResolutionID        string              `json:"resolution_id"`
	ScopeInputID        PolicyStopDecimal   `json:"scope_input_id"`
	ScopeCeiling        int                 `json:"scope_ceiling"`
	AcceptedAt          time.Time           `json:"accepted_at"`
	ReviewGeneration    PolicyStopDecimal   `json:"review_generation"`
	SupersededRevisions []PolicyRevisionRef `json:"superseded_revisions"`
}

type PolicyStopReview struct {
	Manifest    PolicyStopManifest `json:"manifest"`
	StopIfMatch string             `json:"stop_if_match"`
}
type PolicyStopManifest struct {
	CandidateCrossCheckID *string                        `json:"candidate_cross_check_id"`
	Version               int32                          `json:"version"`
	RunID                 string                         `json:"run_id"`
	OutcomeID             string                         `json:"outcome_id"`
	SourceGeneration      PolicyStopDecimal              `json:"source_generation"`
	ReviewGeneration      PolicyStopDecimal              `json:"review_generation"`
	SourceContext         PolicyRevisionSourceV1         `json:"source_context"`
	Hold                  PolicyStopHoldSnapshot         `json:"hold"`
	Gate                  *PolicyStopGateSnapshot        `json:"gate"`
	Execution             PolicyStopExecution            `json:"execution"`
	HumanFeedback         []PolicyStopHumanFeedback      `json:"human_feedback"`
	CrossCheckFeedback    []PolicyStopCrossCheckFeedback `json:"cross_check_feedback"`
}

// PolicyStopHoldSnapshot binds the paused episode even when claim/outcome identity is unchanged.
type PolicyStopHoldSnapshot struct {
	Reason       *string   `json:"reason"`
	PolicyDetail *string   `json:"policy_detail"`
	Since        time.Time `json:"since"`
}

type PolicyStopGateSnapshot struct {
	PolicyStopGateContext
	Payload PolicyStopGatePayload `json:"payload"`
}
type PolicyStopGatePayload struct {
	ReviseCount            *int32                 `json:"revise_count"`
	CheckpointWitness      *GateCheckpointWitness `json:"checkpoint_witness"`
	WitnessWorkerID        *string                `json:"witness_worker_id"`
	WitnessClaimGeneration *PolicyStopDecimal     `json:"witness_claim_generation"`
	PlanMd                 *string                `json:"plan_md"`
	Milestones             json.RawMessage        `json:"milestones"`
	RequiredCapabilities   []string               `json:"required_capabilities"`
	RequiredTools          []string               `json:"required_tools"`
	SizeClass              *string                `json:"size_class"`
}
type PolicyStopExecution struct {
	ContractVersion  *int32             `json:"contract_version"`
	ContractRevision *PolicyStopDecimal `json:"contract_revision"`
	Contract         json.RawMessage    `json:"contract"`
	Frozen           []Milestone        `json:"frozen"`
	CompletedIDs     []string           `json:"completed_ids"`
	CurrentCeiling   *int32             `json:"current_ceiling"`
	ProposedCeiling  int32              `json:"proposed_ceiling"`
}
type PolicyStopHumanFeedback struct {
	InputID                 PolicyStopDecimal  `json:"input_id"`
	Body                    *string            `json:"body"`
	CreatedAt               *time.Time         `json:"created_at"`
	GateBinding             *string            `json:"gate_binding"`
	GateRevision            *PolicyStopDecimal `json:"gate_revision"`
	ConsumedAt              *time.Time         `json:"consumed_at"`
	ConsumedClaimGeneration *PolicyStopDecimal `json:"consumed_claim_generation"`
	ConsumedWorkerID        *string            `json:"consumed_worker_id"`
	AppliedAt               *time.Time         `json:"applied_at"`
	IncludedAt              *time.Time         `json:"included_at"`
	InclusionReported       bool               `json:"inclusion_reported"`
	Disposition             *string            `json:"disposition"`
	SettledAt               *time.Time         `json:"settled_at"`
	SupersededByStopInputID *PolicyStopDecimal `json:"superseded_by_stop_input_id"`
}
type PolicyStopCrossCheckCandidate struct {
	PlanMd               *string         `json:"plan_md"`
	Milestones           json.RawMessage `json:"milestones"`
	RequiredCapabilities []string        `json:"required_capabilities"`
	RequiredTools        []string        `json:"required_tools"`
	SizeClass            *string         `json:"size_class"`
	BaseCommit           *string         `json:"base_commit"`
	PlanningDiff         *string         `json:"planning_diff"`
}
type PolicyStopCrossCheckFeedback struct {
	CrossCheckID                    string                        `json:"cross_check_id"`
	LeadRunID                       string                        `json:"lead_run_id"`
	Stage                           string                        `json:"stage"`
	Round                           int32                         `json:"round"`
	CandidateGeneration             PolicyStopDecimal             `json:"candidate_generation"`
	CandidateDigest                 string                        `json:"candidate_digest"`
	Candidate                       PolicyStopCrossCheckCandidate `json:"candidate"`
	Verdict                         string                        `json:"verdict"`
	Supersede                       bool                          `json:"supersede"`
	ReasonClass                     *string                       `json:"reason_class"`
	Findings                        json.RawMessage               `json:"findings"`
	CreatedAt                       *time.Time                    `json:"created_at"`
	DecidedAt                       *time.Time                    `json:"decided_at"`
	DeadlineAt                      *time.Time                    `json:"deadline_at"`
	InterruptedAt                   *time.Time                    `json:"interrupted_at"`
	FinalizedAt                     *time.Time                    `json:"finalized_at"`
	AutomaticRevisionLimit          int32                         `json:"automatic_revision_limit"`
	AutomaticRoundsEnabled          bool                          `json:"automatic_rounds_enabled"`
	CheckerRunID                    *string                       `json:"checker_run_id"`
	CheckerHarness                  *string                       `json:"checker_harness"`
	CheckerModel                    *string                       `json:"checker_model"`
	CheckerEffort                   *string                       `json:"checker_effort"`
	CheckerModelSource              *string                       `json:"checker_model_source"`
	CheckerEffortSource             *string                       `json:"checker_effort_source"`
	RevisionDisposition             *string                       `json:"revision_disposition"`
	RevisionSettledAt               *time.Time                    `json:"revision_settled_at"`
	RevisionSupersededByStopInputID *PolicyStopDecimal            `json:"revision_superseded_by_stop_input_id"`
}
