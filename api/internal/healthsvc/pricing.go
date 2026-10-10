package healthsvc

import (
	"context"
	"fmt"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/codexprice"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const pricingCacheTTL = 10 * time.Minute

func (s *Service) checkCodexPricing(ctx context.Context, now time.Time) apitypes.HealthCheckDTO {
	s.pricingMu.Lock()
	defer s.pricingMu.Unlock()
	if s.pricingInitialized && now.Sub(s.pricingRefreshedAt) < pricingCacheTTL {
		return copyCheck(s.pricingResult)
	}
	c := s.base("pricing.codex")
	// A refresh makes one query with a five-second deadline. Its failure replaces
	// the expired result and is cached for the same interval as a successful read.
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := s.cfg.Store.ListRecentUnpricedCodexModels(queryCtx, store.ListRecentUnpricedCodexModelsParams{
		Cutoff: pgconv.Time(now.Add(-7 * 24 * time.Hour)),
		Priced: codexprice.PricedModels(now),
	})
	if err != nil {
		c = degradeUnknown(c, "pricing.codex", err)
	} else if len(rows) == 0 {
		c.Severity = sevOK
		c.Summary = "no recent Codex usage on unpriced models"
	} else {
		c.Severity = sevWarn
		c.Summary = "recent Codex usage on unpriced models"
		for i, row := range rows {
			if i == 10 {
				c.Evidence = append(c.Evidence, apitypes.HealthEvidenceDTO{Label: safe("and more"), Value: safe("additional unpriced models")})
				break
			}
			reason := "no price"
			if codexprice.Coverage(row.Model, now) == codexprice.PromoExpired {
				reason = "promotional price expired"
			}
			c.Evidence = append(c.Evidence, apitypes.HealthEvidenceDTO{
				Label: safe(row.Model),
				Value: safe(fmt.Sprintf("%d runs, %s", row.Runs, reason)),
			})
		}
	}
	s.pricingResult = c
	s.pricingRefreshedAt = now
	s.pricingInitialized = true
	return copyCheck(c)
}

// copyCheck keeps all mutable DTO storage private to each caller.
func copyCheck(c apitypes.HealthCheckDTO) apitypes.HealthCheckDTO {
	evidence := make([]apitypes.HealthEvidenceDTO, len(c.Evidence))
	copy(evidence, c.Evidence)
	c.Evidence = evidence
	for _, p := range []**string{&c.Doc, &c.Since, &c.Action, &c.Command} {
		if *p != nil {
			*p = strPtr(**p)
		}
	}
	return c
}
