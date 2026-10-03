package workersvc

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

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

func bindPlanCrossCheckWrite(p *store.SetRunAutopilotPlanParams, run store.Run, req *StateRequest) error {
	if req.ClaimGeneration == nil {
		return ErrClaimGenerationRequired
	}
	if req.RequiredCapabilities == nil || req.RequiredTools == nil || req.SizeClass == nil {
		return fmt.Errorf("%w: cross-check plan fields are required", ErrInvalidState)
	}
	digest, err := hex.DecodeString(req.CandidateDigest)
	if err != nil || len(digest) != 32 {
		return fmt.Errorf("%w: invalid cross-check candidate digest", ErrInvalidState)
	}
	rawMilestones := planMilestonesParam(run, req.PlanMd, req.Milestones)
	if req.Milestones != nil {
		rawMilestones, err = json.Marshal(*req.Milestones)
		if err != nil {
			return fmt.Errorf("%w: invalid cross-check milestones", ErrInvalidState)
		}
	}
	c, err := NormalizePlanCrossCheckCandidate(PlanCrossCheckCandidate{
		PlanMd: p.PlanMd.String, Milestones: rawMilestones,
		RequiredCapabilities: append([]string{}, (*req.RequiredCapabilities)...), RequiredTools: append([]string{}, (*req.RequiredTools)...), SizeClass: *req.SizeClass})
	if err != nil {
		return fmt.Errorf("%w: invalid cross-check candidate", ErrInvalidState)
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
		return fmt.Errorf("%w: invalid cross-check size", ErrInvalidState)
	}
	p.PlanMd = pgconv.Text(c.PlanMd)
	p.MilestonesFrozen = c.Milestones
	p.InferredCapabilities = caps
	p.InferredTools = tools
	p.SizeClass = pgconv.Text(c.SizeClass)
	p.CandidateDigest = digest
	milestones, err := DecodeMilestones(c.Milestones)
	if err != nil {
		return fmt.Errorf("%w: invalid cross-check milestones", ErrInvalidState)
	}
	if _, valid := validateMilestones(milestones); !valid {
		return fmt.Errorf("%w: invalid cross-check milestones", ErrInvalidState)
	}
	// The structural contract frozen later in this same transaction must use
	// the same normalized fields the checker approved.
	req.PlanMd = &c.PlanMd
	req.Milestones = &milestones
	req.RequiredCapabilities = &caps
	req.RequiredTools = &tools
	req.SizeClass = &c.SizeClass
	return nil
}
