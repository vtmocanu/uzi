package store

import (
	"strings"
	"testing"
)

// Source-level pins of the weekly usage reserve's SQL (PRD #3522 M3). The behaviour is pinned
// against a real Postgres in workersvc (usage_reserve_*_livedb_test.go); these catch a clause
// that silently disappears from a query the live tests cannot all reach.

func TestEphemeralDemandQueriesSkipReserveHeldRuns(t *testing.T) {
	for name, sql := range map[string]string{
		"ListUnplaceableQueuedRunsForEphemeral": listUnplaceableQueuedRunsForEphemeral,
		"ListSaturationQueuedRunsForEphemeral":  listSaturationQueuedRunsForEphemeral,
		"ListIsolatedQueuedRunsForEphemeral":    listIsolatedQueuedRunsForEphemeral,
	} {
		if !strings.Contains(sql, "AND r.reserve_hold_reason IS NULL") {
			t.Errorf("%s does not drop reserve-held runs from ephemeral demand", name)
		}
	}
}

func TestClaimRunReserveClauseShape(t *testing.T) {
	// sqlc renders named parameters positionally, so the pins are on the column logic around them.
	for _, want := range []string{
		"COALESCE(r.reserve_exempt_until >",
		"r.first_started_at IS NOT NULL AND NOT $",
		"r.harness = 'codex'",
		"r.kind IN ('judge', 'self_improve')",
		"r.credential_override_mode = 'pinned' AND r.credential_override_secret_id IS NOT NULL",
		"r.credential_override_mode = 'default'",
		"p.id = ANY($",
		"(r.reserve_hold_reason IS NOT NULL) ASC",
		"reserve_hold_reason = NULL,",
	} {
		if !strings.Contains(claimRun, want) {
			t.Errorf("ClaimRun lost the reserve fragment %q", want)
		}
	}
	// The reserve gate sits in the target selection, before the claim UPDATE: a held run is
	// never locked, never claimed and never opens a custody hold.
	gate := strings.Index(claimRun, "PRD #3522 M3: the weekly usage reserve admission gate")
	update := strings.Index(claimRun, "UPDATE runs SET")
	hold := strings.Index(claimRun, "hold AS (")
	if gate < 0 || update < 0 || hold < 0 || gate > hold || hold > update {
		t.Errorf("reserve gate at %d, hold CTE at %d, claim UPDATE at %d: the gate must filter the target", gate, hold, update)
	}
}

// TestClaimableCountStaysReserveAgnostic: the health resolver's "who could claim this run" count is
// lane-agnostic on purpose (a reserve hold is the run's credential, not any worker's availability).
func TestClaimableCountStaysReserveAgnostic(t *testing.T) {
	if strings.Contains(countOnlineWorkersClaimableForRun, "reserve_hold") || strings.Contains(countOnlineWorkersClaimableForRun, "reserve_exempt") {
		t.Error("CountOnlineWorkersClaimableForRun takes a reserve parameter; the reserve is reported through its own health reason")
	}
}

func TestRequeueClaimAssemblyExactPersistsReserveReasonOnlyWhenSupplied(t *testing.T) {
	if !strings.Contains(requeueClaimAssemblyExact, "reserve_hold_reason = COALESCE(") {
		t.Error("RequeueClaimAssemblyExact must COALESCE the reserve reason so a NULL parameter leaves the column alone")
	}
	for _, forbidden := range []string{"limit_wait_count =", "limit_resets_at =", "retry_not_before =", "rate_limit_type ="} {
		if strings.Contains(requeueClaimAssemblyExact, forbidden) {
			t.Errorf("RequeueClaimAssemblyExact writes %q; a reserve hold must not touch the usage-park state", forbidden)
		}
	}
}
