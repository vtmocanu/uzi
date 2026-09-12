package workersvc

import (
	"context"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// AdminNewestOpenOccurrence resolves the SINGLE newest OPEN occurrence of a (category, target)
// coordinate across ALL users (PRD #1184 M3) — the row the admin issue DRAFT renders from and
// the admin FILER claims against. It is a thin pass-through over NewestOpenOccurrenceForCoord: the
// query has NO user predicate (the aggregate is cross-user by design) and NO authorization of its
// own — admin-ness is enforced on the route (RequireAdminRO for the draft read, RequireAdmin for
// the file write), never here.
//
// "Open" is the bottom rung of the bucket ladder inlined in SQL as a plain predicate (no
// disposition, not filed); see the query header for why that is not the §332-forbidden second
// ladder. A coordinate with no open occurrence returns pgx.ErrNoRows, which the handlers map to a
// 404 — the draft and the file each resolve INDEPENDENTLY, so a fresher review that landed between
// the two requests moves the filed link to that newer occurrence (decision log 2026-09-07).
func (s *Service) AdminNewestOpenOccurrence(ctx context.Context, category, target string) (store.NewestOpenOccurrenceForCoordRow, error) {
	return s.q.NewestOpenOccurrenceForCoord(ctx, store.NewestOpenOccurrenceForCoordParams{
		Category: category,
		Target:   target,
	})
}
