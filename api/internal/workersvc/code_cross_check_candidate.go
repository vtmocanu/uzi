package workersvc

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// codeCandidateDigest binds the bounded, frozen inputs using canonical JSON.
// JSONB spacing and object key ordering must not change snapshot identity.
func codeCandidateDigest(cc store.CrossCheck) ([]byte, error) {
	var context struct {
		IssueTitle       string `json:"issue_title"`
		IssueDescription string `json:"issue_description"`
	}
	decoder := json.NewDecoder(bytes.NewReader(cc.CodeContext))
	decoder.DisallowUnknownFields()
	if len(cc.CodeContext) > 2*262144 || decoder.Decode(&context) != nil ||
		validateCrossCheckContext(context.IssueTitle, context.IssueDescription) != nil ||
		len(cc.PlanMd.String) > 256*1024 || !utf8.ValidString(cc.PlanMd.String) ||
		len(cc.Milestones) > 256*1024 || len(cc.RequiredCapabilities) > 64 ||
		len(cc.RequiredTools) > 64 || len(cc.GuidanceSnapshot.String) > 8192 ||
		!utf8.ValidString(cc.GuidanceSnapshot.String) {
		return nil, ErrCrossCheckRefused
	}
	var milestones []json.RawMessage
	if len(cc.Milestones) > 0 && (json.Unmarshal(cc.Milestones, &milestones) != nil || len(milestones) > 64) {
		return nil, ErrCrossCheckRefused
	}
	for _, values := range [][]string{cc.RequiredCapabilities, cc.RequiredTools} {
		for _, value := range values {
			if len(value) > 256 || !utf8.ValidString(value) {
				return nil, ErrCrossCheckRefused
			}
		}
	}
	if cc.SizeClass.String != "" && cc.SizeClass.String != "s" && cc.SizeClass.String != "m" && cc.SizeClass.String != "l" {
		return nil, ErrCrossCheckRefused
	}
	// Re-encode milestones too: PostgreSQL JSONB can alter their whitespace.
	var normalizedMilestones any
	if len(cc.Milestones) > 0 && json.Unmarshal(cc.Milestones, &normalizedMilestones) != nil {
		return nil, ErrCrossCheckRefused
	}
	raw, err := json.Marshal(map[string]any{
		"base_commit": cc.BaseCommit.String, "head_commit": cc.HeadCommit.String,
		"context": context, "plan_md": cc.PlanMd.String, "milestones": normalizedMilestones,
		"required_capabilities": cc.RequiredCapabilities, "required_tools": cc.RequiredTools,
		"size_class": cc.SizeClass.String, "guidance": cc.GuidanceSnapshot.String,
	})
	if err != nil {
		return nil, ErrCrossCheckRefused
	}
	digest := sha256.Sum256(raw)
	return digest[:], nil
}
