package workersvc

import (
	"encoding/json"
	"log/slog"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// ClaimPayload is the complete, self-contained handoff a worker receives when it
// claims a run: everything it needs to execute without a second server round
// trip. It is returned only over the claim response (the sole secret-delivery
// channel) and must never be logged — it carries the decrypted forge PAT and
// Anthropic token.
//
// Wire shape reconciled with the M2 worker: run fields are flat at the top
// level; repo/secrets/config are nested; secret field names are forge_pat and
// anthropic_oauth_token. Deviation from M2's assumption: agents is an ARRAY of
// structured templates (PRD #3 provides several subagents that map to
// programmatic SDK AgentDefinitions), not a single `template`.
type ClaimPayload struct {
	RunID            string  `json:"run_id"`
	IssueIID         int64   `json:"issue_iid"`
	IssueTitle       string  `json:"issue_title"`
	IssueDescription string  `json:"issue_description"`
	Status           string  `json:"status"`
	Branch           *string `json:"branch"`     // resume: attach existing branch
	SessionID        *string `json:"session_id"` // resume: continue SDK session
	LastSeq          int32   `json:"last_seq"`   // resume: continue message numbering
	IterationCount   int32   `json:"iteration_count"`
	RequeueCount     int32   `json:"requeue_count"`
	PlanMd           *string `json:"plan_md"` // resume: plan already captured

	Repo    ClaimRepo    `json:"repo"`
	Secrets ClaimSecrets `json:"secrets"`
	Agents  []ClaimAgent `json:"agents"`
	Config  ClaimConfig  `json:"config"`

	// Skills is the deduplicated per-run union of every skill allocated to any
	// template for this run's owner (shared allocations ∪ the owner's overlay),
	// after name-collision precedence (user > global > builtin) and the
	// SKILLS_MAX_PER_RUN cap. The worker synthesizes a local plugin dir from this
	// and passes the names as the SDK's explicit top-level enable-list (M4). Always
	// present (possibly empty), never null — the worker always passes an explicit
	// list. Repo-borne skills (opt-in) are resolved worker-side and are not here.
	Skills []ClaimSkill `json:"skills"`
	// SkillsDropped records every skill dropped during assembly — shadowed by a
	// higher-precedence skill of the same name, or over the per-run cap — so the
	// worker can emit the run-message log lines (the worker owns the gapless per-run
	// seq; the server never writes run_messages). Always present (possibly empty).
	SkillsDropped []ClaimSkillDrop `json:"skills_dropped"`
}

// ClaimSkill is one delivered skill in the per-run union: the name+description the
// model routes on and the body it loads on demand. The SKILL.md frontmatter is
// synthesized worker-side from name+description (never stored/sent as frontmatter).
type ClaimSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
}

// ClaimSkillDrop names a skill that assembly dropped and why. Reason is a stable
// code the worker maps to a run-message log line: DropShadowed or DropOverLimit.
type ClaimSkillDrop struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// ClaimRepo carries the repo facts the worker needs. CloneURL is the https clone
// target the worker authenticates with the PAT (via a per-invocation
// http.extraHeader, never writing the PAT into git config); URL is the GitLab
// web URL. Clone from CloneURL, not URL.
type ClaimRepo struct {
	ID            string  `json:"id"`
	URL           string  `json:"url"`
	CloneURL      string  `json:"clone_url"`
	DefaultBranch *string `json:"default_branch"`
	// SkillsEnabled is the repo owner's opt-in to load skills from the repo's own
	// .claude/skills at run time (PRD #16). Default false. When true the worker
	// enumerates repo skills after checkout, applies the same caps, and ranks them
	// below every delivered skill (M6). Skills only — the repo's hooks/settings/
	// commands are never loaded, flag or no flag.
	SkillsEnabled bool `json:"skills_enabled"`
}

// ClaimSecrets are the decrypted secrets for this run only. The worker holds the
// PAT (the agent subprocess never sees it) and uses the Anthropic token as the
// SDK's OAuth credential. Never logged; never persisted on the worker beyond the
// run. ForgeUsername is the bot login (not sensitive; travels with the PAT for
// git identity / MR authorship).
type ClaimSecrets struct {
	ForgeUsername       string `json:"forge_username"`
	ForgePAT            string `json:"forge_pat"`
	AnthropicOAuthToken string `json:"anthropic_oauth_token"`
}

// ClaimAgent is a PRD #3 agent template as structured fields, ready to map onto
// a programmatic SDK AgentDefinition (not a .claude/agents/*.md file — the
// worker runs with settingSources off, so those files would never load). Tools
// nil means inherit-all; Model nil means inherit.
type ClaimAgent struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	PromptBody  string   `json:"prompt_body"`
	Tools       []string `json:"tools"`
	Model       *string  `json:"model"`
	// Skills is this template's allocated skill names (shared ∪ owner's overlay),
	// restricted to names that survived in the per-run union (a name dropped over
	// the cap is removed; a shadowed name stays because the name is still delivered,
	// backed by the higher-precedence body). The worker maps this onto the
	// subagent's AgentDefinition.skills (M4). Always present (possibly empty).
	Skills []string `json:"skills"`
}

// ClaimConfig are the caps the worker enforces (wall clock, idle, loop count)
// plus the run owner's per-user default model. DefaultModel is the SDK model the
// worker applies to the lead/main thread, taking precedence over the lead
// template's model (PRD #17 Decision 6); it is omitted when the owner has no
// default set (NULL), so the worker falls back to the lead template's model.
type ClaimConfig struct {
	RunTimeoutSeconds  int     `json:"run_timeout_seconds"`
	IdleTimeoutSeconds int     `json:"idle_timeout_seconds"`
	MaxIterations      int     `json:"max_iterations"`
	DefaultModel       *string `json:"default_model,omitempty"`
	// SkillMaxBytes and SkillsMaxPerRun are the skill caps the server configured,
	// delivered so the worker enforces the same limits (no hardcoded drift): the
	// worker applies SkillMaxBytes to repo-borne skills and re-enforces
	// SkillsMaxPerRun over the combined delivered ∪ repo set (M4/M6). The server
	// already applied SkillMaxBytes at save and SkillsMaxPerRun at this assembly.
	SkillMaxBytes   int `json:"skill_max_bytes"`
	SkillsMaxPerRun int `json:"skills_max_per_run"`
}

// agentsFromTemplates maps stored templates to claim-payload agents, decoding
// the jsonb tools allowlist (NULL/empty ⇒ inherit-all ⇒ nil) and attaching each
// template's surviving skill names from perTemplateSkills (nil ⇒ empty list, so
// the JSON carries `skills: []`, never null).
func agentsFromTemplates(rows []store.AgentTemplate, perTemplateSkills map[string][]string) []ClaimAgent {
	out := make([]ClaimAgent, 0, len(rows))
	for _, t := range rows {
		skills := perTemplateSkills[t.Name]
		if skills == nil {
			skills = []string{}
		}
		a := ClaimAgent{
			Name:        t.Name,
			Description: t.Description,
			PromptBody:  t.PromptBody,
			Tools:       decodeTools(t.Tools),
			Skills:      skills,
		}
		if t.Model.Valid {
			m := t.Model.String
			a.Model = &m
		}
		out = append(out, a)
	}
	return out
}

// decodeTools turns the stored jsonb allowlist into a slice; a NULL/empty column
// (inherit-all) yields nil. A malformed value is logged and treated as
// inherit-all rather than failing the whole claim.
func decodeTools(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		slog.Error("workersvc: decode template tools", "error", err)
		return nil
	}
	return out
}
