package healthsvc

import (
	"context"
	"fmt"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/agentsource"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// checkAgentSourceUpdate reads persisted discovery facts only; sync outcomes and
// staged snapshots do not affect this advisory check.
func (s *Service) checkAgentSourceUpdate(ctx context.Context) apitypes.HealthCheckDTO {
	c := s.base("agentsource.update")
	c.Action = strPtr("Review agent source updates at /admin/settings#agent-source.")
	c.Severity, c.Summary = sevUnknown, "Agent source update status unavailable."
	if s.cfg.Settings == nil {
		return c
	}
	enabled, enabledErr := s.cfg.Settings.AgentSourceEnabled(ctx)
	url, urlErr := s.cfg.Settings.AgentSourceRepoURL(ctx)
	ref, refErr := s.cfg.Settings.AgentSourceRef(ctx)
	status, statusErr := s.cfg.Settings.AgentSourceStatus(ctx)
	if enabledErr != nil || urlErr != nil || refErr != nil || statusErr != nil {
		return c
	}
	if !enabled || strings.TrimSpace(url) == "" {
		c.Severity, c.Summary = sevNA, "Agent source disabled or unconfigured."
		return c
	}
	if agentsource.IsSHAPin(strings.TrimSpace(ref)) {
		c.Severity, c.Summary = sevNA, "Agent source is pinned to an immutable SHA."
		return c
	}
	if strings.TrimSpace(status.UpdateCheckedAt) == "" {
		c.Summary = "Agent source not checked yet."
		return c
	}
	available, latest := agentsource.DeriveUpdate(ref, status.LastAppliedSHA, status.LatestRef, status.RemoteTipSHA)
	c.Severity, c.Summary = sevOK, "Agent source is up to date."
	if available {
		c.Severity = sevWarn
		c.Summary = "Agent source tracked branch moved."
		if latest != "" {
			c.Summary = fmt.Sprintf("Agent source newer tag %s is available.", safe(latest))
		}
	}
	return c
}
