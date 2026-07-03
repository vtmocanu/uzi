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
}

// ClaimConfig are the caps the worker enforces (wall clock, idle, loop count).
type ClaimConfig struct {
	RunTimeoutSeconds  int `json:"run_timeout_seconds"`
	IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
	MaxIterations      int `json:"max_iterations"`
}

// agentsFromTemplates maps stored templates to claim-payload agents, decoding
// the jsonb tools allowlist (NULL/empty ⇒ inherit-all ⇒ nil).
func agentsFromTemplates(rows []store.AgentTemplate) []ClaimAgent {
	out := make([]ClaimAgent, 0, len(rows))
	for _, t := range rows {
		a := ClaimAgent{
			Name:        t.Name,
			Description: t.Description,
			PromptBody:  t.PromptBody,
			Tools:       decodeTools(t.Tools),
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
