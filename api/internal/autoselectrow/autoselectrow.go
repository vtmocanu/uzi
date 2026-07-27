// Package autoselectrow is the ONE conversion from a store row to an
// autoselect.Candidate (PRD #111 D21).
//
// It exists as its own package for a structural reason, not a stylistic one. The
// ranker is fed by THREE different queries — the settings list, the admin list, and
// M4's ranking query — read by two different layers: `handler` renders eligibility,
// `workersvc` ranks on it. Neither layer may own the conversion. Leaving it in
// `handler` would make the claim path import the HTTP layer; duplicating it into
// `workersvc` would give the two surfaces separate row→Candidate implementations,
// and the D21 differential test that is supposed to compare the two QUERIES would
// instead be comparing two conversions one person wrote the same afternoon, which is
// exactly what D21 exists to prevent one level up.
//
// This package is the impure adapter: it knows pgtype and it knows store. The ranker
// itself stays pure (`autoselect` imports only time + uuid), and the line between
// them is precisely this file.
package autoselectrow

import (
	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/autoselect"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// FromRateLimitRow lifts one per-user rate-limit row (GET /api/me/rate-limits) into
// a Candidate. It fills EVERY field the row carries, which is the point.
//
// Classify reads only a few of them today. The rest are populated anyway because the
// contract is "this row, as the ranker sees it", not "the subset the current gate
// happens to consult": a partly-zeroed Candidate is indistinguishable from one whose
// data really is absent, and the differential test that compares this row against
// M4's ranking row can only be as strong as the weaker conversion.
//
// InFlight stays zero deliberately: only M4's ranking query counts concurrent runs
// per credential, and it is NOT part of the gate — which is precisely why the
// differential test can require the two queries to agree on everything else.
func FromRateLimitRow(row store.ListRateLimitsForUserRow) autoselect.Candidate {
	c := autoselect.Candidate{
		SecretID:     row.UserSecretID,
		Label:        row.Label,
		AutoEligible: row.AutoEligible,
		// HasReading comes from synced_at's validity, which is the LEFT JOIN's own miss
		// signal: the column is NOT NULL in the table, so an invalid one can only mean
		// the join matched nothing. That distinction is the whole of D16 — never polled
		// vs. aged out — and it is why both queries must keep LEFT JOINing.
		HasReading: row.SyncedAt.Valid,
	}
	if row.SyncedAt.Valid {
		t := row.SyncedAt.Time
		c.SyncedAt = &t
	}
	if row.FiveHourPct.Valid {
		v := row.FiveHourPct.Int16
		c.FiveHourPct = &v
	}
	if row.SevenDayPct.Valid {
		v := row.SevenDayPct.Int16
		c.SevenDayPct = &v
	}
	if row.FiveHourResetsAt.Valid {
		t := row.FiveHourResetsAt.Time
		c.FiveResetsAt = &t
	}
	if row.SevenDayResetsAt.Valid {
		t := row.SevenDayResetsAt.Time
		c.SevenResetsAt = &t
	}
	return c
}

// FromAdminRateLimitRow is the admin twin. Its row nullable-wraps the user_secrets
// columns (that query LEFT JOINs users → user_secrets, so a token-less user yields a
// row with no secret at all); callers skip that row before reaching here, which is
// why .Bool and .String are the real values by this point.
func FromAdminRateLimitRow(row store.ListRateLimitsRow) autoselect.Candidate {
	return FromRateLimitRow(store.ListRateLimitsForUserRow{
		UserSecretID:     uuid.UUID(row.UserSecretID.Bytes),
		Label:            row.Label.String,
		AutoEligible:     row.AutoEligible.Bool,
		FiveHourPct:      row.FiveHourPct,
		FiveHourResetsAt: row.FiveHourResetsAt,
		SevenDayPct:      row.SevenDayPct,
		SevenDayResetsAt: row.SevenDayResetsAt,
		Source:           row.Source,
		SyncedAt:         row.SyncedAt,
	})
}

// FromCandidateRow lifts one row of M4's ranking query (ListAutoSelectCandidates)
// into a Candidate.
//
// It DELEGATES rather than converting independently, and the delegation is the
// design: the two queries differ in exactly one Candidate field, InFlight, and
// routing both through one body is what makes that claim checkable instead of
// aspirational. If this function ever grows a second `if row.FiveHourPct.Valid`, the
// differential test has quietly become a test of two conversions.
//
// in_flight_runs is a COALESCEd bigint (never NULL — the LEFT JOIN's miss is 0, not
// absent), narrowed to int here because it counts a user's concurrently-held runs and
// the ranker multiplies it by a points-per-run penalty. A count that could overflow
// an int is a different emergency.
func FromCandidateRow(row store.ListAutoSelectCandidatesRow) autoselect.Candidate {
	c := FromRateLimitRow(store.ListRateLimitsForUserRow{
		UserSecretID:     row.UserSecretID,
		Label:            row.Label,
		AutoEligible:     row.AutoEligible,
		FiveHourPct:      row.FiveHourPct,
		FiveHourResetsAt: row.FiveHourResetsAt,
		SevenDayPct:      row.SevenDayPct,
		SevenDayResetsAt: row.SevenDayResetsAt,
		SyncedAt:         row.SyncedAt,
	})
	c.InFlight = int(row.InFlightRuns)
	return c
}
