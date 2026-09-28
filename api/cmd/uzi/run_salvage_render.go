package main

// run_salvage_render.go renders the failed-run checkpoint salvage block of `uzi run get`
// (PRD #1867 M4): the run-scoped archive copy of a failed run's last published checkpoint at
// refs/uzi-salvage/<run-id>. Every field is server data; the free-text ones are sanitized.

import (
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// salvageRefPrefix is the only ref namespace salvage writes; a salvage_ref outside it is never
// turned into a copy-paste fetch command.
const salvageRefPrefix = "refs/uzi-salvage/"

// salvageStateExplanation is the one-line meaning of each salvage state. The wording is
// deliberately honest: a promoted copy is the last PUBLISHED checkpoint, which may be behind
// the run's final local work, so it is "saved", never "recovered". An unknown state (a newer
// server) returns "" and prints bare.
func salvageStateExplanation(state string) string {
	switch state {
	case "pending":
		return "a salvage copy of the last published checkpoint is being made"
	case "promoted":
		return "checkpointed commits saved (last published checkpoint; may be behind the run's final local work)"
	case "unavailable":
		return "not saved: the published checkpoint was no longer at its recorded tip on the forge"
	case "refused":
		return "not saved: the salvage ref already pointed at a different commit"
	case "failed":
		return "not saved: making the salvage copy kept failing"
	case "skipped_secret":
		return "not saved: the run failed on a secret-scan block"
	case "expired":
		return "the salvage copy expired and was removed from the forge"
	case "disabled":
		return "not saved: salvage was turned off for this forge before a copy was made"
	default:
		return ""
	}
}

// salvageRefFor returns the run's salvage ref when it is exactly refs/uzi-salvage/<uuid>, the
// uuid in its canonical lower-case form, and (when the run carries an id) that uuid is the run's
// own id; "" otherwise. The server builds the ref from the run's UUID, so any other shape (a
// dash run, a foreign run's id, an upper-case or braced uuid) is never echoed.
func salvageRefFor(r apitypes.RunDTO) string {
	if r.SalvageRef == nil {
		return ""
	}
	ref := *r.SalvageRef
	id, ok := strings.CutPrefix(ref, salvageRefPrefix)
	if !ok {
		return ""
	}
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.String() != id {
		return ""
	}
	if r.ID != "" && r.ID != id {
		return ""
	}
	return ref
}

// salvageFetchLine is the command that fetches a promoted salvage copy, "" unless the run is
// promoted with a well-formed salvage ref of its own (salvageRefFor).
func salvageFetchLine(r apitypes.RunDTO) string {
	if strOr(r.SalvageState, "") != "promoted" {
		return ""
	}
	ref := salvageRefFor(r)
	if ref == "" {
		return ""
	}
	return "git fetch origin " + ref
}

// salvageRows is the SALVAGE block of `uzi run get`, emitted only when the server set
// salvage_state (a failed run with a salvage row); every other run prints no row.
func salvageRows(r apitypes.RunDTO) [][]string {
	if r.SalvageState == nil || *r.SalvageState == "" {
		return nil
	}
	state := cellText(*r.SalvageState)
	if why := salvageStateExplanation(*r.SalvageState); why != "" {
		state += ": " + why
	}
	rows := [][]string{{"SALVAGE", state}}
	if ref := salvageRefFor(r); ref != "" {
		rows = append(rows, []string{"SALVAGE_REF", ref})
	}
	if r.SalvageTip != nil && *r.SalvageTip != "" {
		rows = append(rows, []string{"SALVAGE_TIP", cellText(shortSHA(*r.SalvageTip))})
	}
	if r.SalvageExpiresAt != nil {
		rows = append(rows, []string{"SALVAGE_EXPIRES", r.SalvageExpiresAt.UTC().Format(time.RFC3339)})
	}
	if r.SalvageLastError != nil && *r.SalvageLastError != "" {
		rows = append(rows, []string{"SALVAGE_ERROR", cellText(*r.SalvageLastError)})
	}
	if line := salvageFetchLine(r); line != "" {
		rows = append(rows, []string{"SALVAGE_FETCH", line})
	}
	return rows
}
