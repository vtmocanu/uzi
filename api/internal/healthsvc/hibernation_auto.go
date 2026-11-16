package healthsvc

import (
	"context"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

const (
	// hibernationEvidenceRows bounds the listed workers in total; the window total the query
	// returns stays exact whatever this is.
	hibernationEvidenceRows = 50
	// hibernationNameBytes is the worker-name budget in an evidence value. It shrinks when the
	// rest of the value is long, so the blocker text always survives maxEvidenceBytes.
	hibernationNameBytes = 32
	// hibernationReasonBytes bounds an unrecognised blocked-reason code shown verbatim.
	hibernationReasonBytes = 32
	// hibernationControllerReasonBytes bounds an unrecognised controller refusal code.
	hibernationControllerReasonBytes = 20
	// hibernationMinNameBytes is the least the worker-name part keeps once the suffix is bounded.
	hibernationMinNameBytes = 8
)

// controllerRefusalLabel renders the controller's refusal code (the worker's hibernation_reason
// after a controller_refused block). The codes mirror the refuse* constants in
// controller/internal/kube/hibernation.go; an unknown code shows sanitized and bounded.
func controllerRefusalLabel(code string) string {
	switch code {
	case "roll_reservation":
		return "roll reservation"
	case "roll_blocked":
		return "roll blocked"
	case "disk_pressure":
		return "disk pressure"
	case "dind_maintenance":
		return "DinD maintenance"
	case "busy":
		return "busy"
	case "layout_migration_held":
		return "layout migration held"
	case "roll_evidence_unavailable":
		return "roll evidence unavailable"
	}
	return cutBytes(orDash(safe(code)), hibernationControllerReasonBytes)
}

// hibernationBlockedLabel renders a recorded hibernation_blocked_reason for an admin. The
// codes are the workersvc.Blocked* closed set; an unknown code (a newer api wrote it) shows
// as its sanitized, bounded self.
func hibernationBlockedLabel(code string) string {
	switch code {
	case workersvc.BlockedCustodyHold:
		return "recovery custody hold"
	case workersvc.BlockedManualCordon:
		return "manual cordon"
	case workersvc.BlockedMaintenance:
		return "maintenance"
	case workersvc.BlockedRoll:
		return "image roll"
	case workersvc.BlockedDraining:
		return "draining"
	case workersvc.BlockedPendingWakeDemand:
		return "a queued run is waking it"
	case workersvc.BlockedControllerNotAdvertise:
		return "controller not advertising hibernation"
	case workersvc.BlockedNoPodIdentity:
		return "no pod identity"
	case workersvc.BlockedControllerRefused:
		return "controller refused"
	case workersvc.BlockedIntakeFenced:
		return "intake fenced"
	}
	return cutBytes(orDash(safe(code)), hibernationReasonBytes)
}

// cutBytes truncates s to at most n bytes without splitting a rune.
func cutBytes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// hibernationEvidenceValue joins prefix, the worker name and suffix within maxEvidenceBytes.
// The suffix (the blocker) is bounded first so the name always keeps at least
// hibernationMinNameBytes; the name then takes what remains, up to hibernationNameBytes.
func hibernationEvidenceValue(name, suffix string) string {
	const prefix = "worker "
	suffix = cutBytes(suffix, maxEvidenceBytes-len(prefix)-hibernationMinNameBytes)
	budget := maxEvidenceBytes - len(prefix) - len(suffix)
	if budget > hibernationNameBytes {
		budget = hibernationNameBytes
	}
	return prefix + cutBytes(orDash(safe(name)), budget) + suffix
}

// hibernationIdleFor renders how long a worker has been idle: whole days, or hours under a day.
func hibernationIdleFor(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// checkFleetHibernation (issue #4207) warns when an idle persistent hosted worker cannot be
// hibernated by the automatic pass: either the pass recorded a blocker for it (a recovery
// custody hold, cordon, maintenance, roll, ...) or every run assigned to it is parked and the
// oldest parked run is older than the idle threshold. `na` when no hosted workers are
// configured or automatic hibernation is off (hibernation enabled AND automatic both on);
// `unknown` when a setting or the query cannot be read. The summary is fixed text. Evidence
// is the exact worker count (one statement over both disjoint sources), then at most
// hibernationEvidenceRows rows in total (idle workers first), then an explicit More row when the
// listing is cut. Since is the oldest blocker timestamp across every matching worker.
func (s *Service) checkFleetHibernation(ctx context.Context, now time.Time, workers []store.ListAllWorkersRow) apitypes.HealthCheckDTO {
	c := s.base("fleet.hibernation")
	if !s.hostedConfigured(workers) {
		c.Severity = sevNA
		c.Summary = "No hosted workers are configured on this deployment."
		return c
	}
	unknown := func(what string, err error) apitypes.HealthCheckDTO {
		slog.Warn("healthsvc: fleet.hibernation read failed", "what", what, "error", err)
		c.Severity = sevUnknown
		c.Summary = "Automatic hibernation status could not be read."
		return c
	}
	if s.cfg.Settings == nil {
		c.Severity = sevNA
		c.Summary = "Automatic hibernation is off."
		return c
	}
	enabled, err := s.cfg.Settings.WorkerHibernationEnabled(ctx)
	if err != nil {
		return unknown("worker_hibernation_enabled", err)
	}
	auto, err := s.cfg.Settings.WorkerHibernationAuto(ctx)
	if err != nil {
		return unknown("worker_hibernation_auto", err)
	}
	if !enabled || !auto {
		c.Severity = sevNA
		c.Summary = "Automatic hibernation is off."
		return c
	}
	idleAfter, err := s.cfg.Settings.WorkerHibernationIdleAfter(ctx)
	if err != nil {
		return unknown("worker_hibernation_idle_after", err)
	}
	cutoff := pgconv.Time(now.Add(-idleAfter))
	rows, err := s.cfg.Store.ListAutoHibernationBlockers(ctx, store.ListAutoHibernationBlockersParams{Cutoff: cutoff, MaxRows: hibernationEvidenceRows})
	if err != nil {
		return unknown("blockers", err)
	}
	if len(rows) == 0 {
		c.Severity = sevOK
		c.Summary = "No idle hosted worker is blocked from automatic hibernation."
		return c
	}

	// Every row repeats the exact window total and the true oldest since (window functions
	// over the whole matching set, one snapshot), so they are read from the first row.
	total := rows[0].Total
	c.Severity = sevWarn
	c.Summary = "Some idle hosted workers cannot be hibernated automatically."
	c.Evidence = append(c.Evidence, apitypes.HealthEvidenceDTO{Label: "Workers", Value: fmt.Sprintf("%d", total)})
	for _, r := range rows {
		var suffix string
		if r.Kind == "parked" {
			runRef := "-"
			if r.RunID.Valid {
				// A canonical UUID string is 36 bytes, so the 8-byte prefix always exists.
				runRef = uuid.UUID(r.RunID.Bytes).String()[:8]
			}
			suffix = fmt.Sprintf(" blocked: assigned run parked (%s, %s)", runRef, cutBytes(safe(r.Status.String), 16))
		} else {
			label := hibernationBlockedLabel(r.Reason)
			if r.Reason == workersvc.BlockedControllerRefused && r.ControllerReason != "" {
				label += " (" + controllerRefusalLabel(r.ControllerReason) + ")"
			}
			suffix = fmt.Sprintf(" idle %s, hibernation blocked: %s", hibernationIdleFor(now.Sub(r.Anchor.Time)), label)
		}
		c.Evidence = append(c.Evidence, apitypes.HealthEvidenceDTO{Label: "Worker", Value: hibernationEvidenceValue(r.Name, suffix)})
	}
	if more := total - int64(len(rows)); more > 0 {
		c.Evidence = append(c.Evidence, apitypes.HealthEvidenceDTO{Label: "More", Value: fmt.Sprintf("%d more not listed", more)})
	}
	c.Action = strPtr("Clear the blocker so the worker can hibernate: release its recovery custody hold, lift the manual cordon or maintenance, or resume or cancel the parked run. The pass retries on its own; see Automatic hibernation in the hosted workers doc.")
	if rows[0].OldestSince.Valid {
		c.Since = sincePtr(rows[0].OldestSince.Time)
	}
	return c
}
