package main

import "sort"

// side identifies which roster a divergent role appears in.
type side string

const (
	sideProductOnly side = "product-only"
	sideDevteamOnly side = "devteam-only"
)

// divergence is one role present in one roster and absent from the other, after
// the accepted-divergence allowlist is applied. msg is the actionable nudge text.
type divergence struct {
	side side
	role string
	msg  string
}

// accepted carries the allowlisted divergences per side, keyed by role. A role
// present here for its side is a known-and-recorded difference the nudge stays
// quiet about (issue #63: lead is product-only by design and must never be
// flagged).
type accepted struct {
	productOnly map[string]bool
	devteamOnly map[string]bool
}

// roleParity compares the product roster against the dev-team roster and returns
// the un-accepted divergences: roles on the dev team but not in the product, and
// roles in the product but not on the dev team, each minus the accepted side.
// Role identity is the caller's string (the filename stem). It is pure — no I/O,
// deterministic, output sorted by (side, role) — so it can be unit-tested with
// fixture rosters, which is where the tool's own logic is guarded (not against
// the live roster, which would re-create the coupling issue #63 removes).
func roleParity(product, devteam []string, acc accepted) []divergence {
	productSet := toSet(product)
	devteamSet := toSet(devteam)

	var out []divergence
	for _, role := range devteam {
		if !productSet[role] && !acc.devteamOnly[role] {
			out = append(out, divergence{
				side: sideDevteamOnly,
				role: role,
				msg:  "on the dev team, not in the product: consider promoting it into api/internal/agenttmpl/builtins/, or record it in scripts/role-parity-accepted.tsv with a reason",
			})
		}
	}
	for _, role := range product {
		if !devteamSet[role] && !acc.productOnly[role] {
			out = append(out, divergence{
				side: sideProductOnly,
				role: role,
				msg:  "in the product, not on the dev team: consider adding it to .claude/agents/, or record it in scripts/role-parity-accepted.tsv with a reason",
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].side != out[j].side {
			return out[i].side < out[j].side
		}
		return out[i].role < out[j].role
	})
	return out
}

// toSet builds a membership set from a role list.
func toSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}
