package runkind

import (
	"os"
	"strings"
	"testing"
)

// Inventory the combined contract in each scheduling query. Behavioral LiveDB
// tests cover claims and placement; this prevents a mirror losing either side
// when the deliberately duplicated predicates change.
func TestCrossCheckCombinedSQLMirrorParity(t *testing.T) {
	raw, err := os.ReadFile("../store/queries/runtime.sql")
	if err != nil {
		t.Fatal(err)
	}
	blocks := namedQueryBlocks(string(raw))
	for _, site := range []struct {
		name               string
		helpers, protocols int
	}{
		{"ClaimRun", 2, 2},                          // claimant and legacy spread peer
		{"CountOnlineWorkersClaimableForRun", 2, 1}, // availability and strict health
		{"ListUnplaceableQueuedRunsForEphemeral", 1, 1},
		{"ListSaturationQueuedRunsForEphemeral", 2, 2}, // eligible and available workers
	} {
		t.Run(site.name, func(t *testing.T) {
			body, ok := blocks[site.name]
			if !ok {
				t.Fatal("scheduling query missing")
			}
			if got := strings.Count(body, "fn_cross_check_child_eligible("); got != site.helpers {
				t.Errorf("lane eligibility copies=%d want=%d", got, site.helpers)
			}
			for _, predicate := range []string{
				"'cross_check_rounds_v1'",
				"'cross_check_pins_v1'",
				"protocol_check.round = (SELECT max(latest.round)",
				"COALESCE((SELECT pin.model FROM user_cross_check_pins",
			} {
				if got := strings.Count(body, predicate); got != site.protocols {
					t.Errorf("%s copies=%d want=%d", predicate, got, site.protocols)
				}
			}
		})
	}
	for _, name := range []string{
		"ListUnplaceableQueuedRunsForEphemeral",
		"ListSaturationQueuedRunsForEphemeral",
		"ListIsolatedQueuedRunsForEphemeral",
	} {
		t.Run(name+"/priority", func(t *testing.T) {
			body := blocks[name]
			order := strings.LastIndex(body, "ORDER BY fn_run_priority(")
			limit := strings.LastIndex(body, "LIMIT @max_rows")
			if order < 0 || limit < 0 || order >= limit {
				t.Fatal("priority must be applied before the candidate limit")
			}
		})
	}
}
