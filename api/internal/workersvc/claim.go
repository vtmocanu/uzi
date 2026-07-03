package workersvc

import (
	"encoding/json"
	"log/slog"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// ClaimPayload is the complete, self-contained handoff a worker receives when it
// claims a run: everything it needs to execute without a second server round
// trip. It is returned only over the claim response (the sole secret-delivery
// channel) and must never be logged — it carries the decrypted bot PAT and
// Anthropic token.
type ClaimPayload struct {
	Run         ClaimRun         `json:"run"`
	Repo        ClaimRepo        `json:"repo"`
	Credentials ClaimCredentials `json:"credentials"`
	Agents      []ClaimAgent     `json:"agents"`
	Config      ClaimConfig      `json:"config"`
}

// ClaimRun is the run snapshot. SessionID/Branch/PlanMd are populated on a
// resume (a re-queued run that already has a session, branch, or captured plan);
// LastSeq lets the worker continue message numbering without a seq collision.
type ClaimRun struct {
	ID               string  `json:"id"`
	IssueIID         int64   `json:"issue_iid"`
	IssueTitle       string  `json:"issue_title"`
	IssueDescription string  `json:"issue_description"`
	Status           string  `json:"status"`
	SessionID        *string `json:"session_id"`
	LastSeq          int32   `json:"last_seq"`
	IterationCount   int32   `json:"iteration_count"`
	RequeueCount     int32   `json:"requeue_count"`
	Branch           *string `json:"branch"`
	PlanMd           *string `json:"plan_md"`
}

// ClaimRepo carries the repo facts the worker needs to clone and push. CloneURL
// is the https clone URL the worker authenticates with the bot PAT (via a
// per-invocation http.extraHeader), never writing the PAT into git config.
type ClaimRepo struct {
	WebURL            string  `json:"web_url"`
	PathWithNamespace string  `json:"path_with_namespace"`
	DefaultBranch     *string `json:"default_branch"`
	CloneURL          string  `json:"clone_url"`
}

// ClaimCredentials are the decrypted secrets for this run only. The worker holds
// the PAT (the agent subprocess never sees it) and uses the Anthropic token as
// the SDK's OAuth credential. Never logged; never persisted on the worker beyond
// the run.
type ClaimCredentials struct {
	BotUsername    string `json:"bot_username"`
	BotPAT         string `json:"bot_pat"`
	AnthropicToken string `json:"anthropic_token"`
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
