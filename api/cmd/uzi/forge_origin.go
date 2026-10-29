package main

import (
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// forgeOriginLines renders an advisory snapshot, never a reachability verdict.
func forgeOriginLines(c *apitypes.ForgeOriginCooldownDTO) []string {
	if c == nil {
		return nil
	}
	expired := !c.CooledUntil.IsZero() && !time.Now().Before(c.CooledUntil)
	state := "state unknown"
	switch c.State {
	case "cooling":
		state = "cooling"
		if expired {
			state = "retry eligible; recovery unverified"
		}
	case "half_open":
		state = "half-open attempt"
	case "retry_eligible_unverified":
		state = "retry eligible; recovery unverified"
	case "legacy_serialized_until_upgrade":
		state = "legacy serialized for the active flight"
	case "reserved_unverified":
		state = "reserved; attempt unverified"
	}
	until := "unknown"
	if !c.CooledUntil.IsZero() {
		until = c.CooledUntil.UTC().Format(time.RFC3339)
	}
	lines := []string{
		"forge origin: " + cellText(c.Origin),
		state,
		"cooled until " + until,
	}
	if c.HalfOpenRunID != nil {
		lines = append(lines, "half-open run: "+cellText(*c.HalfOpenRunID))
	}
	if c.State == "legacy_serialized_until_upgrade" {
		lines = append(lines, "Serialization ends when this flight finishes or qualifying direct proof clears it.", "Upgrade enables direct recovery reporting.")
	}
	if c.Detail != "" {
		lines = append(lines, "last reported detail: "+cellText(c.Detail))
	}
	return append(lines, "Expiry permits retry; recovery unverified.", "Worker heartbeat online does not verify forge reachability.")
}

func workerForgeLines(w apitypes.WorkerDTO) []string {
	var lines []string
	for i := range w.ForgeOriginCooldowns {
		lines = append(lines, forgeOriginLines(&w.ForgeOriginCooldowns[i])...)
	}
	return lines
}

func isForgeOwnerHold(r apitypes.RunDTO) bool {
	return isForgePark(r) && r.RecoveryRetryNotBeforePresent && r.RecoveryRetryNotBefore == nil
}

func forgeHoldLines(r apitypes.RunDTO) []string {
	if !isForgeOwnerHold(r) {
		return nil
	}
	id := providerCommandID(r.ID)
	return []string{
		"Forge automatic retries exhausted; owner-held.",
		"Owner Resume or Cancel; no automatic resume.",
		"Resume: uzi run resume " + id,
		"Cancel: uzi run cancel " + id + " (terminal)",
		"Resume requests one ordinary bounded attempt.",
		"Counters, cooldown, custody and grants preserved.",
		"Admission can still block: limit 1 or saturation.",
		"Source/archive availability and latest edits unverified.",
		"Local-only work remains local; no source authority change.",
		"Export only an available archive; it may lack latest edits.",
	}
}

func forgeRunLines(r apitypes.RunDTO) []string {
	var lines []string
	if isForgePark(r) {
		lines = append(lines, forgeParkLine(r))
	}
	lines = append(lines, forgeHoldLines(r)...)
	return append(lines, forgeOriginLines(r.ForgeOriginCooldown)...)
}
