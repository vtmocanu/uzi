package main

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// Recovery metadata is a recorded observation, never a fresh forge probe.
// Only a canonical capture UUID can identify its fixed prerequisite ref.
func recoveryPinRef(a apitypes.RecoveryArchiveDTO) (string, bool) {
	id, err := uuid.Parse(a.ID)
	if err != nil || id.String() != a.ID {
		return "", false
	}
	ref := "refs/uzi-prereq/" + id.String()
	return ref, a.PrerequisiteClosure != nil && a.PrerequisiteClosure.PinRef == ref
}

func recoveryObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

const recoveryRestoreGuide = "https://github.com/vtmocanu/uzi/blob/main/docs/run-recovery.md#prerequisite-pins-lifecycle-and-custody-release"

// prerequisiteLines keeps flags independent of state, including historical deletion.
// No untrusted field is inserted into a command or interpreted as Markdown.
func prerequisiteLines(a apitypes.RecoveryArchiveDTO) []string {
	lines := []string{}
	if a.ExpiresAt != nil {
		lines = append(lines, "archive expiry: "+a.ExpiresAt.UTC().Format(time.RFC3339))
	} else {
		lines = append(lines, "archive expiry: not recorded")
	}
	if len(a.PrerequisiteShas) == 0 {
		lines = append(lines, "prerequisites: self_contained (worker manifest reports no prerequisites)")
	} else {
		lines = append(lines, fmt.Sprintf("prerequisites: %d required commit(s); worker manifest is not import proof", len(a.PrerequisiteShas)))
	}
	c := a.PrerequisiteClosure
	if c == nil {
		if len(a.PrerequisiteShas) != 0 {
			lines = append(lines, "pin protection: uncertain; no owner projection (older server or no pin row); prerequisite availability unknown")
		}
	} else {
		ref, validRef := recoveryPinRef(a)
		validTip := c.PinTip != nil && len(*c.PinTip) == 40 && *c.PinTip == strings.ToLower(*c.PinTip) && *c.PinTip != strings.Repeat("0", 40) && recoveryObjectID(*c.PinTip)
		validRecord := validRef && (c.PinTip == nil || validTip) && !c.UpdatedAt.IsZero()
		protection, explanation := "uncertain", "unrecognized or malformed recorded observation"
		switch c.State {
		case "pending", "invoked", "create_unknown":
			explanation = "protection not established; may remain unresolved indefinitely"
		case "pinned":
			if validRecord && validTip && !c.AbsenceUnconfirmed {
				protection = "protected"
				explanation = "recorded server observation only, conditional on external refs surviving; no fresh probe or complete-bundle guarantee"
			} else {
				explanation = "recorded pin does not establish protection; absence or metadata is uncertain"
			}
		case "refused":
			protection, explanation = "unprotected", "pin creation refused"
		case "foreign":
			protection, explanation = "unprotected", "ref occupied by foreign content"
		case "lost":
			protection, explanation = "unprotected", "recorded pin lost"
		case "deleted":
			protection, explanation = "unprotected", "pin deletion recorded; historical observation"
		}
		lines = append(lines, "pin state: "+cellText(c.State), "pin protection: "+protection+"; "+explanation)
		if validRef {
			lines = append(lines, "pin ref: "+ref)
		} else {
			lines = append(lines, "pin ref: invalid metadata; no safe ref")
		}
		if validTip {
			lines = append(lines, "pin tip: "+*c.PinTip)
		} else if c.PinTip != nil {
			lines = append(lines, "pin tip: invalid metadata")
		}
		if c.Reason != "" {
			lines = append(lines, "pin reason (recorded): "+cellText(c.Reason))
		}
		if c.UpdatedAt.IsZero() {
			lines = append(lines, "pin record updated: unknown")
		} else {
			lines = append(lines, "pin record updated: "+c.UpdatedAt.UTC().Format(time.RFC3339)+" (record update, not a fresh probe)")
		}
		lines = append(lines, fmt.Sprintf("absence_unconfirmed: %t; cleanup_exhausted: %t", c.AbsenceUnconfirmed, c.CleanupExhausted))
		if c.AbsenceUnconfirmed {
			lines = append(lines, "pin absence unconfirmed; do not mint a replacement from this observation")
		}
		switch {
		case c.CleanupExhausted:
			lines = append(lines, "pin cleanup: exhausted; automatic deletes stopped; this grants no manual deletion permission")
		case c.State == "deleted":
			lines = append(lines, "pin cleanup: complete (recorded deletion)")
		case a.State == "expired" || a.State == "discarded":
			lines = append(lines, "pin cleanup: pending; completion not confirmed")
		}
	}
	lines = append(lines, "pin metadata does not release custody or verify latest local work; download/discard follow archive rules", "restore guide: "+recoveryRestoreGuide)
	return lines
}

func captureDetailLines(a apitypes.RecoveryArchiveDTO) []string {
	lines := []string{captureListLine(a)}
	for _, line := range prerequisiteLines(a) {
		lines = append(lines, "  "+strings.TrimSpace(line))
	}
	return lines
}
