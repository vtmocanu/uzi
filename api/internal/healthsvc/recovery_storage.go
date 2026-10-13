package healthsvc

import (
	"context"
	"fmt"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const recoveryStorageReadBudget = 4 * time.Second
const recoveryStorageExamples = 8

// checkRecoveryStorage observes current persisted refusal markers, independently of custody.
func (s *Service) checkRecoveryStorage(ctx context.Context, now time.Time) apitypes.HealthCheckDTO {
	c := s.base("recovery.storage")
	ctx, cancel := context.WithTimeout(ctx, recoveryStorageReadBudget)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return degradeUnknown(c, c.ID, err)
	}
	rows, err := s.cfg.Store.RecoveryStorageHealth(ctx, store.RecoveryStorageHealthParams{
		Now: pgconv.Time(now), ExampleLimit: recoveryStorageExamples,
	})
	if err != nil {
		return degradeUnknown(c, c.ID, err)
	}
	if err := ctx.Err(); err != nil {
		return degradeUnknown(c, c.ID, err)
	}
	if len(rows) == 0 {
		return degradeUnknown(c, c.ID, fmt.Errorf("missing storage aggregate"))
	}
	t := rows[0]
	c.Severity = sevOK
	if t.RefusedCount > 0 {
		c.Severity = sevWarn
	}
	c.Summary = fmt.Sprintf("%d captures currently marked quota-refused.", t.RefusedCount)
	limit := func(n int64) string {
		if n <= 0 {
			return "disabled"
		}
		return fmt.Sprintf("%d bytes", n)
	}
	c.Evidence = []apitypes.HealthEvidenceDTO{
		{Label: "Available captures", Value: fmt.Sprintf("%d; %d bytes", t.AvailableCount, t.AvailableBytes)},
		{Label: "Preparing reservations", Value: fmt.Sprintf("%d; %d bytes", t.PreparingCount, t.PreparingBytes)},
		{Label: "Uploading reservations", Value: fmt.Sprintf("%d; %d bytes", t.UploadingCount, t.UploadingBytes)},
		{Label: "Non-expired job files", Value: fmt.Sprintf("%d; %d bytes", t.JobCount, t.JobBytes)},
		{Label: "Reclaimable job bytes", Value: fmt.Sprint(t.ReclaimableBytes)},
		{Label: "Quota-refused captures", Value: fmt.Sprint(t.RefusedCount)},
		{Label: "Owners", Value: fmt.Sprint(t.OwnerCount)},
		{Label: "Omitted owner examples", Value: fmt.Sprint(t.OmittedCount)},
		{Label: "Recovery bytes", Value: fmt.Sprint(t.AvailableBytes + t.PreparingBytes + t.UploadingBytes)},
		{Label: "Shared stored bytes", Value: fmt.Sprint(t.AvailableBytes + t.PreparingBytes + t.UploadingBytes + t.JobBytes)},
		{Label: "Recovery per-owner limit", Value: limit(s.cfg.RecoveryReadyPayloadPerOwner)},
		{Label: "Recovery instance limit", Value: limit(s.cfg.RecoveryInstanceBytes)},
		{Label: "Shared stored-files limit", Value: limit(s.cfg.StoredFilesBudgetBytes)},
	}
	// SQL bounds the examples to eight; one failed aggregate read aborts the entire check.
	for _, r := range rows {
		if !r.OwnerID.Valid {
			continue
		}
		c.Evidence = append(c.Evidence, apitypes.HealthEvidenceDTO{
			Label: "Owner " + safe(r.OwnerID.String()),
			Value: fmt.Sprintf("available %d / %d bytes; preparing %d / %d bytes; uploading %d / %d bytes; jobs %d / %d bytes; reclaimable %d bytes; refused %d",
				r.OwnerAvailableCount, r.OwnerAvailableBytes, r.OwnerPreparingCount, r.OwnerPreparingBytes,
				r.OwnerUploadingCount, r.OwnerUploadingBytes, r.OwnerJobCount, r.OwnerJobBytes,
				r.OwnerReclaimableBytes, r.OwnerRefusedCount),
		})
	}
	if c.Severity == sevWarn {
		c.Action = strPtr("Review currently quota-refused captures with their owners (uzi run recovery). This observes stored state; an admitted retry clears the marker before upload succeeds.")
	}
	return c
}
