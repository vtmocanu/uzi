package uzicli

import (
	"context"
	"net/url"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// client_admin.go holds the admin verbs (uzi admin) of the
// Client/HTTPClient split out of client.go (PRD #1017).

func (c *HTTPClient) AdminListUsers(ctx context.Context) ([]apitypes.UserDTO, error) {
	var env struct {
		Users []apitypes.UserDTO `json:"users"`
	}
	if err := c.get(ctx, "/api/admin/users", &env); err != nil {
		return nil, err
	}
	return env.Users, nil
}

func (c *HTTPClient) AdminListRuns(ctx context.Context) ([]apitypes.RunListItemDTO, error) {
	var env struct {
		Runs []apitypes.RunListItemDTO `json:"runs"`
	}
	if err := c.get(ctx, "/api/admin/runs", &env); err != nil {
		return nil, err
	}
	return env.Runs, nil
}

func (c *HTTPClient) AdminListWorkers(ctx context.Context) ([]apitypes.AdminWorkerDTO, error) {
	var env struct {
		Workers []apitypes.AdminWorkerDTO `json:"workers"`
	}
	if err := c.get(ctx, "/api/admin/workers", &env); err != nil {
		return nil, err
	}
	return env.Workers, nil
}

// AdminListCLITokens reads the factory-wide standing-credential inventory. The
// response carries no token value and no hash — see apitypes.AdminCLITokenDTO.
func (c *HTTPClient) AdminListCLITokens(ctx context.Context) ([]apitypes.AdminCLITokenDTO, error) {
	var env struct {
		Tokens []apitypes.AdminCLITokenDTO `json:"tokens"`
	}
	if err := c.get(ctx, "/api/admin/cli-tokens", &env); err != nil {
		return nil, err
	}
	return env.Tokens, nil
}

// GuardrailImpact reads the live guardrail pre-flight impact count. The endpoint
// persists nothing; it re-sweeps the forge and returns how many enabled repos
// would be refused under the new guardrail (PRD #66 M3).
func (c *HTTPClient) GuardrailImpact(ctx context.Context) (apitypes.GuardrailImpactDTO, error) {
	var out apitypes.GuardrailImpactDTO
	if err := c.get(ctx, "/api/admin/guardrail-impact", &out); err != nil {
		return apitypes.GuardrailImpactDTO{}, err
	}
	return out, nil
}

// AdminBlockedRepos reads the admin cross-user blocked-repos list from the STORED
// privilege report (PRD #66 M9). ChecksUnknown on the envelope is true when a
// connection was never checked — an empty list is then "unknown", not "none blocked".
func (c *HTTPClient) AdminBlockedRepos(ctx context.Context) (apitypes.AdminBlockedReposDTO, error) {
	var out apitypes.AdminBlockedReposDTO
	if err := c.get(ctx, "/api/admin/blocked-repos", &out); err != nil {
		return apitypes.AdminBlockedReposDTO{}, err
	}
	return out, nil
}

// AdminAgentSource reads the agent-source config + sync status + staged snapshot
// (PRD #602 M6): GET /api/admin/agent-source. Read-only — the "Sync now" and
// approve-and-apply writes are cookie-only, so there is nothing here for a uza_
// (admin_ro) token to trigger. The response envelope is {"agent_source": {...}}.
func (c *HTTPClient) AdminAgentSource(ctx context.Context) (apitypes.AgentSourceDTO, error) {
	var env struct {
		AgentSource apitypes.AgentSourceDTO `json:"agent_source"`
	}
	if err := c.get(ctx, "/api/admin/agent-source", &env); err != nil {
		return apitypes.AgentSourceDTO{}, err
	}
	return env.AgentSource, nil
}

func (c *HTTPClient) AdminUsage(ctx context.Context) (apitypes.AdminUsageDTO, error) {
	var out apitypes.AdminUsageDTO
	if err := c.get(ctx, "/api/admin/usage", &out); err != nil {
		return apitypes.AdminUsageDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) AdminRateLimits(ctx context.Context) ([]apitypes.AdminRateLimitRowDTO, error) {
	var env struct {
		Users []apitypes.AdminRateLimitRowDTO `json:"users"`
	}
	if err := c.get(ctx, "/api/admin/rate-limits", &env); err != nil {
		return nil, err
	}
	return env.Users, nil
}

// AdminJudgeBacklog reads the admin "All users" aggregate backlog (PRD #1184 M5): GET
// /api/admin/judge/recommendations. The handler serves an UNENVELOPED JudgeAdminBacklogDTO
// (httpx.JSON of the DTO directly), so decode straight into it — no {"backlog": …} wrapper,
// matching the owner JudgeBacklog. bucket and category are omitted when empty (the handler's
// default branches apply the server defaults) and escaped because both are user input off a
// flag; there is no ?run= anchor on the admin path.
func (c *HTTPClient) AdminJudgeBacklog(ctx context.Context, bucket, category string) (apitypes.JudgeAdminBacklogDTO, error) {
	path := "/api/admin/judge/recommendations"
	q := url.Values{}
	if bucket != "" {
		q.Set("bucket", bucket)
	}
	if category != "" {
		// Forwarded verbatim as the comma-separated `?category=a,b` list the server splits
		// and validates; the CLI never parses it into labels of its own.
		q.Set("category", category)
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out apitypes.JudgeAdminBacklogDTO
	if err := c.get(ctx, path, &out); err != nil {
		return apitypes.JudgeAdminBacklogDTO{}, err
	}
	return out, nil
}

// AdminJudgeStats reads the admin "All users" triage tally (PRD #1184 M5): GET
// /api/admin/judge/stats. The reply is an unenveloped TriageDTO, exactly like JudgeStats.
func (c *HTTPClient) AdminJudgeStats(ctx context.Context) (apitypes.TriageDTO, error) {
	var out apitypes.TriageDTO
	if err := c.get(ctx, "/api/admin/judge/stats", &out); err != nil {
		return apitypes.TriageDTO{}, err
	}
	return out, nil
}
