package workersvc

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

func normalizeCrossCheckIdentifier(s string, max int) (string, error) {
	// Printable sentinels let Validate inspect the original bytes without rejecting
	// edge spaces that this boundary has always trimmed. Controls remain interior.
	if len(s) > max || termsafe.Validate("cross-check identifier", "x"+s+"x") != nil {
		return "", ErrCrossCheckRefused
	}
	return scrubThenBound(strings.TrimSpace(s), max), nil
}

// NormalizePlanCrossCheckCandidate defines the sole canonical text boundary for
// submission and for the required plan write. Bounds precede decoding, but secret
// redaction always sees the whole normalized string before a display byte cap.
func NormalizePlanCrossCheckCandidate(c PlanCrossCheckCandidate) (PlanCrossCheckCandidate, error) {
	if len(c.PlanMd) > 256*1024 || len(c.PlanningDiff) > 512*1024 || len(c.Milestones) > 256*1024 ||
		len(c.RequiredCapabilities) > 64 || len(c.RequiredTools) > 64 {
		return c, ErrCrossCheckRefused
	}
	// Decode the actual apitypes.Milestone shape before prose scrubbing can
	// erase an unsafe ID. Unknown nested fields are prose, not milestone IDs.
	milestones, err := DecodeMilestones(c.Milestones)
	if err != nil {
		return c, ErrCrossCheckRefused
	}
	for _, milestone := range milestones {
		if len(milestone.ID) > 64 || termsafe.Validate("milestone id", "x"+milestone.ID+"x") != nil {
			return c, ErrCrossCheckRefused
		}
	}
	c.PlanMd = scrubThenBound(c.PlanMd, 256*1024)
	c.PlanningDiff = scrubThenBound(c.PlanningDiff, 512*1024)
	decoder := json.NewDecoder(bytes.NewReader(c.Milestones))
	decoder.UseNumber()
	var values []any
	if decoder.Decode(&values) != nil || values == nil || len(values) > 64 {
		return c, ErrCrossCheckRefused
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return c, ErrCrossCheckRefused
	}
	var scrub func(any, int) (any, error)
	scrub = func(value any, depth int) (any, error) {
		if depth > 32 {
			return nil, ErrCrossCheckRefused
		}
		switch item := value.(type) {
		case string:
			return scrubThenBound(item, 256*1024), nil
		case []any:
			for i, child := range item {
				next, err := scrub(child, depth+1)
				if err != nil {
					return nil, err
				}
				item[i] = next
			}
		case map[string]any:
			for key, child := range item {
				cleanKey, err := normalizeCrossCheckIdentifier(key, 128)
				if err != nil || key != cleanKey {
					return nil, ErrCrossCheckRefused
				}
				next, err := scrub(child, depth+1)
				if err != nil {
					return nil, err
				}
				item[key] = next
			}
		}
		return value, nil
	}
	clean, err := scrub(values, 0)
	if err != nil {
		return c, err
	}
	c.Milestones, err = json.Marshal(clean)
	if err != nil || len(c.Milestones) > 256*1024 {
		return c, ErrCrossCheckRefused
	}
	for i, s := range c.RequiredCapabilities {
		c.RequiredCapabilities[i], err = normalizeCrossCheckIdentifier(s, 256)
		if err != nil {
			return c, err
		}
	}
	for i, s := range c.RequiredTools {
		c.RequiredTools[i], err = normalizeCrossCheckIdentifier(s, 256)
		if err != nil {
			return c, err
		}
	}
	c.SizeClass, err = normalizeCrossCheckIdentifier(c.SizeClass, 64)
	return c, err
}

// boundCrossCheckReport is the normalized, digest-bound view of a worker report that claims a
// checked plan: the one canonical text boundary shared by the autopilot plan write
// (bindPlanCrossCheckWrite) and the gated awaiting_approval report (PRD #3708).
type boundCrossCheckReport struct {
	candidate  PlanCrossCheckCandidate
	caps       []string
	tools      []string
	digest     []byte
	milestones []Milestone
}

// normalizeBoundCrossCheckReport requires the cross-check plan fields and a 32-byte hex
// candidate digest on the report, then runs planMd and the reported fields through
// NormalizePlanCrossCheckCandidate and the server-owned vocabularies. It writes nothing.
func normalizeBoundCrossCheckReport(run store.Run, req StateRequest, planMd string) (boundCrossCheckReport, error) {
	var out boundCrossCheckReport
	if req.RequiredCapabilities == nil || req.RequiredTools == nil || req.SizeClass == nil {
		return out, fmt.Errorf("%w: cross-check plan fields are required", ErrInvalidState)
	}
	digest, err := hex.DecodeString(req.CandidateDigest)
	if err != nil || len(digest) != 32 {
		return out, fmt.Errorf("%w: invalid cross-check candidate digest", ErrInvalidState)
	}
	rawMilestones := reportPlanMilestonesParam(run, req)
	if req.Milestones != nil {
		rawMilestones, err = json.Marshal(*req.Milestones)
		if err != nil {
			return out, fmt.Errorf("%w: invalid cross-check milestones", ErrInvalidState)
		}
	}
	c, err := NormalizePlanCrossCheckCandidate(PlanCrossCheckCandidate{
		PlanMd: planMd, Milestones: rawMilestones,
		RequiredCapabilities: append([]string{}, (*req.RequiredCapabilities)...), RequiredTools: append([]string{}, (*req.RequiredTools)...), SizeClass: *req.SizeClass})
	if err != nil {
		return out, fmt.Errorf("%w: invalid cross-check candidate", ErrInvalidState)
	}
	caps := capability.Filter(c.RequiredCapabilities)
	tools := capability.FilterTools(c.RequiredTools)
	if caps == nil {
		caps = []string{}
	}
	if tools == nil {
		tools = []string{}
	}
	switch c.SizeClass {
	case "s", "m", "l":
	default:
		return out, fmt.Errorf("%w: invalid cross-check size", ErrInvalidState)
	}
	milestones, err := DecodeMilestones(c.Milestones)
	if err != nil {
		return out, fmt.Errorf("%w: invalid cross-check milestones", ErrInvalidState)
	}
	if _, valid := validateMilestones(milestones); !valid {
		return out, fmt.Errorf("%w: invalid cross-check milestones", ErrInvalidState)
	}
	return boundCrossCheckReport{candidate: c, caps: caps, tools: tools, digest: digest, milestones: milestones}, nil
}

func bindPlanCrossCheckWrite(p *store.SetRunAutopilotPlanParams, run store.Run, req *StateRequest) error {
	if req.ClaimGeneration == nil {
		return ErrClaimGenerationRequired
	}
	b, err := normalizeBoundCrossCheckReport(run, *req, p.PlanMd.String)
	if err != nil {
		return err
	}
	c := b.candidate
	p.PlanMd = pgconv.Text(c.PlanMd)
	p.MilestonesFrozen = c.Milestones
	p.InferredCapabilities = b.caps
	p.InferredTools = b.tools
	p.SizeClass = pgconv.Text(c.SizeClass)
	p.CandidateDigest = b.digest
	// The structural contract frozen later in this same transaction must use
	// the same normalized fields the checker approved.
	req.PlanMd = &c.PlanMd
	req.Milestones = &b.milestones
	req.RequiredCapabilities = &b.caps
	req.RequiredTools = &b.tools
	req.SizeClass = &c.SizeClass
	return nil
}

// bindGatedAwaitingApproval binds a gated run's awaiting_approval report to the candidate the
// checker approved (PRD #3708). SetRunAwaitingApproval derives a NULL ("passed") reason from
// an approve row only when its stored candidate digest, plan, milestones and requirements
// equal what is written here, so this writes the normalized report fields the checker saw. A
// report without a digest, or whose fields do not normalize, is left untouched with no digest:
// the SQL then derives interrupted instead of refusing the report, because a refused report
// would leave a finished check with no human gate. semantic leaves the plan text raw because
// semanticGateReport already hashed the reported bytes.
func bindGatedAwaitingApproval(run store.Run, req StateRequest, semantic bool, p *store.SetRunAwaitingApprovalParams) {
	if req.CandidateDigest == "" {
		return
	}
	if semantic {
		if digest, err := hex.DecodeString(req.CandidateDigest); err == nil && len(digest) == 32 {
			p.CandidateDigest = digest
		}
		return
	}
	b, err := normalizeBoundCrossCheckReport(run, req, p.PlanMd.String)
	if err != nil {
		return
	}
	bound := req
	bound.PlanMd = &b.candidate.PlanMd
	bound.Milestones = &b.milestones
	p.PlanMd = pgconv.Text(b.candidate.PlanMd)
	p.MilestonesCandidate = reportPlanMilestonesParam(run, bound)
	p.InferredCapabilities = b.caps
	p.InferredTools = b.tools
	p.SizeClass = pgconv.Text(b.candidate.SizeClass)
	p.CandidateDigest = b.digest
}

// gatedServerGateReason is the server-recorded "unavailable" for a gated run (PRD #3708): a
// worker that does not advertise cross_check_gated_v1 never ran the check and declared no
// reason, so the server records worker_unsupported itself. It is never a worker-declared
// value, which validatePlanCrossCheckGateReason keeps refusing.
func gatedServerGateReason(lead store.Run, wkr store.Worker, req StateRequest) pgtype.Text {
	if req.State != "awaiting_approval" || !lead.PlanCrossCheckGated || req.PlanCrossCheckGateReason != nil ||
		slices.Contains(wkr.ProtocolCapabilities, capability.CrossCheckGatedV1) {
		return pgtype.Text{}
	}
	return pgconv.Text("worker_unsupported")
}
