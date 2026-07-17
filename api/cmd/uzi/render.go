package main

import (
	"fmt"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
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
