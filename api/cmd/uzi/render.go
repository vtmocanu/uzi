package main

import (
	"fmt"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// runTitle picks a human display title for a run row. A chat carries Title; an
// issue/ci_fix run carries an IssueTitle. Both may be empty (a pre-title run),
// in which case the caller shows the id + kind instead.
func runTitle(r apitypes.RunDTO) string {
	if r.Title != nil && *r.Title != "" {
		return *r.Title
	}
	return r.IssueTitle
}

// strOr renders a *string, substituting fallback for nil/empty.
func strOr(p *string, fallback string) string {
	if p == nil || *p == "" {
		return fallback
	}
	return *p
}

// int64Or renders a *int64, substituting fallback for nil.
func int64Or(p *int64, fallback string) string {
	if p == nil {
		return fallback
	}
	return fmt.Sprintf("%d", *p)
}

// boolStr renders a bool as "true"/"false" for a table cell.
func boolStr(b bool) string { return fmt.Sprintf("%t", b) }

// triStateStr renders a tri-state *bool (PRD #841): nil → "inherit", true → "on",
// false → "off". Used by the MR_REWORK rows in `run get` / `schedule get`, where a nil
// pointer means "inherit the owner default" rather than a concrete on/off — unlike
// boolStr, which cannot express the inherit case.
func triStateStr(b *bool) string {
	if b == nil {
		return "inherit"
	}
	if *b {
		return "on"
	}
	return "off"
}

// mrAbbrev is the compact per-run label for the merge/pull request, forge-aware:
// GitLab "MR", Forgejo AND GitHub "PR" (PRD #65 D2, #238 D2). It is the CLI's twin of
// the web's mrAbbrev (web/src/lib/forgeNoun.ts) and slacksvc's forgeMrAbbrev — same
// mapping, kept in sync so `uzi run get` reads in the run's own forge vocabulary.
// Both PR-forges are named explicitly so a missing github arm never renders "MR".
// Any unknown/absent forge_type is GitLab's form (the default connection kind).
func mrAbbrev(forgeType string) string {
	if forgeType == "forgejo" || forgeType == "github" {
		return "PR"
	}
	return "MR"
}
