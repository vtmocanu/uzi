package apitypes

import "time"

// ProductSkillDTO is one skill of a product's skill set (PRD #1909 D9): the three things kept
// from a SKILL.md. Body is included: product skill bodies are visible to uzi admins, who approve
// them (an accepted cost, PRD D9).
type ProductSkillDTO struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
}

// ProductSkillDropDTO names a skill a sync did not stage and why. Reason is one of invalid,
// too_large, duplicate, over_limit, secret. Name is empty (and Count set) only for the one
// aggregated over_limit note of files past the read bound.
type ProductSkillDropDTO struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
	Count  int    `json:"count,omitempty"`
}

// ProductSkillsConfigDTO is the skills source of a product. The clone token is WRITE-ONLY:
// skills_token_set says whether one is stored and nothing else about it is ever returned.
// Enabled is the instance fact that UZI_PRODUCT_SKILLS_ALLOWED_BASE_URLS lists at least one base
// URL (an empty list turns the feature off).
type ProductSkillsConfigDTO struct {
	SkillsRepoURL  string `json:"skills_repo_url"`
	SkillsRef      string `json:"skills_ref"`
	SkillsTokenSet bool   `json:"skills_token_set"`
	Enabled        bool   `json:"enabled"`
}

// ProductSkillsAppliedDTO is the APPROVED set: what reaches the product's jobs. SHA is "" and
// AppliedAt/AppliedBy are null until a first apply.
type ProductSkillsAppliedDTO struct {
	SHA       string            `json:"sha"`
	AppliedAt *time.Time        `json:"applied_at"`
	AppliedBy *string           `json:"applied_by"`
	Skills    []ProductSkillDTO `json:"skills"`
}

// ProductSkillsDiffDTO compares a staged set with the applied one, by skill name: Added and
// Removed are names only in the staged / applied set, Changed are names in both whose description
// or body differ, Unchanged are identical. Every list is sorted and never null.
type ProductSkillsDiffDTO struct {
	Added     []string `json:"added"`
	Changed   []string `json:"changed"`
	Removed   []string `json:"removed"`
	Unchanged []string `json:"unchanged"`
}

// ProductSkillsStagedDTO is a synced snapshot awaiting admin approval. It reaches no job until
// POST /api/admin/products/{id}/skills/apply names its SHA.
type ProductSkillsStagedDTO struct {
	SHA      string                `json:"sha"`
	StagedAt time.Time             `json:"staged_at"`
	StagedBy *string               `json:"staged_by"`
	Skills   []ProductSkillDTO     `json:"skills"`
	Dropped  []ProductSkillDropDTO `json:"dropped"`
	Diff     ProductSkillsDiffDTO  `json:"diff"`
}

// ProductSkillsDTO is the admin view of one product's skill set, the response of GET
// /api/admin/products/{id}/skills and of the sync and apply writes. Staged is null when nothing
// is waiting for approval.
type ProductSkillsDTO struct {
	Config  ProductSkillsConfigDTO  `json:"config"`
	Applied ProductSkillsAppliedDTO `json:"applied"`
	Staged  *ProductSkillsStagedDTO `json:"staged"`
}
