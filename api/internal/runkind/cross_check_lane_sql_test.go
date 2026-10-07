package runkind

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every non-chat predicate site has a disposition. Load predicates must use the
// per-claim marker; excluding every child would lose legacy run-slot accounting.
func TestCrossCheckLaneSQLExclusionParity(t *testing.T) {
	type disposition struct {
		file, name string
		sites      int
		reason     string
	}
	inventory := []disposition{
		{"runtime", "ListWorkersByUser", 1, ""},
		{"runtime", "CountInProgressRunsForUser", 1, ""},
		{"runtime", "ListRunsForUser", 1, ""},
		{"runtime", "ListActiveRunsAll", 1, ""},
		{"runtime", "ListAllWorkers", 1, ""},
		{"runtime", "ClaimRun", 5, "selection and harness gates retain legacy children; load sites are separately pinned"},
		{"runtime", "RunHasPendingOutcomeLease", 1, "pending outcome fence applies to children"},
		{"runtime", "CancelRunServerSideWithPendingOutcome", 2, "cancellation fences retain children"},
		{"runtime", "RequestWallParks", 1, ""},
		{"runtime", "ParkRunsAtWall", 2, ""},
		{"runtime", "StampCompletionBudgetExhausted", 1, ""},
		{"runtime", "RequeueAttestedFinalizeRuns", 3, "lifecycle recovery retains children"},
		{"runtime", "LockFailAttestedFinalizeRunsOverCap", 2, "lifecycle locks retain children"},
		{"runtime", "failAttestedFinalizeRunsOverCapLocked", 4, "lifecycle failure retains children"},
		{"runtime", "ReadoptRunsFromSnapshot", 2, "snapshot recovery retains claimed children"},
		{"runtime", "LockFailRunsMissingFromSnapshot", 4, "snapshot fences and lifecycle locks retain children"},
		{"runtime", "failRunsMissingFromSnapshotLocked", 8, "snapshot fences and lifecycle failure retain children"},
		{"runtime", "RequeueRunsMissingFromSnapshot", 6, "snapshot fences and lifecycle recovery retain children"},
		{"runtime", "SelfUsage", 1, "usage measures actual billed work, including children"},
		{"runtime", "AdminUsageTotals", 1, "usage measures actual billed work, including children"},
		{"runtime", "AdminUsagePerUser", 1, "usage measures actual billed work, including children"},
		{"runtime", "SelfRunOutcomes", 5, ""},
		{"runtime", "AdminRunOutcomes", 5, ""},
		{"runtime", "AdminRunOutcomesPerUser", 5, ""},
		{"runtime", "CreateGateVerdictInput", 1, "gate content identity, not lane occupancy"},
		{"runtime", "CreateRunReviseInputIfUnderCap", 1, "gate content identity, not lane occupancy"},
		{"runtime", "CreateApprovePlanInput", 1, "gate content identity, not lane occupancy"},
		{"runtime", "CreateStopVerdictInput", 1, "gate content identity, not lane occupancy"},
		{"runtime", "CreateExtendInput", 1, ""},
		{"runtime", "ListActiveRunsForHealth", 2, "health must retain children to report cross-check slot reasons"},
		{"runtime", "CountOnlineWorkersClaimableForRun", 2, "harness selection retains children; load site separately pinned"},
		{"runtime", "CountOnlineWorkersWithFreeSlotForUser", 1, ""},
		{"runtime", "ListUnplaceableQueuedRunsForEphemeral", 2, "provisioning candidate and harness gates retain children"},
		{"runtime", "ListSaturationQueuedRunsForEphemeral", 4, "provisioning candidate and harness gates retain children; load separately pinned"},
		{"runtime", "ListIsolatedQueuedRunsForEphemeral", 1, "isolated profile candidate selection, not occupancy"},
		{"credential_disabled", "SettleCredentialDisabledSpentBudget", 1, "credential lifecycle settlement retains children"},
	}
	predicate := regexp.MustCompile(`(?i)\b(?:\w+\.)?(?:run_)?kind\s*(?:<>\s*'chat'|NOT\s+IN\s*\([^)]*'chat'[^)]*\))`)
	comments := regexp.MustCompile(`--[^\n]*`)
	loadSites := map[string]map[int]bool{
		"ListWorkersByUser": {0: true}, "ListAllWorkers": {0: true},
		"ClaimRun":                              {0: true, 2: true, 4: true},
		"CountOnlineWorkersClaimableForRun":     {0: true},
		"CountOnlineWorkersWithFreeSlotForUser": {0: true},
		"ListSaturationQueuedRunsForEphemeral":  {3: true},
	}
	known := map[string]disposition{}
	for _, d := range inventory {
		known[d.file+"/"+d.name] = d
	}
	seen := map[string]bool{}
	for file, path := range map[string]string{ // #nosec G101 -- repository SQL paths, not credential values
		"runtime":             "../store/queries/runtime.sql",
		"credential_disabled": "../store/queries/credential_disabled.sql",
	} {
		raw, err := os.ReadFile(path) // #nosec G304 -- fixed repository query paths above
		if err != nil {
			t.Fatal(err)
		}
		for name, body := range namedQueryBlocks(string(raw)) {
			body = comments.ReplaceAllString(body, "")
			sites := predicate.FindAllStringIndex(body, -1)
			if len(sites) == 0 {
				continue
			}
			key := file + "/" + name
			d, ok := known[key]
			if !ok {
				t.Errorf("unaudited non-chat predicate: %s", key)
				continue
			}
			seen[key] = true
			if len(sites) != d.sites {
				t.Errorf("%s predicate sites=%d want=%d", key, len(sites), d.sites)
			}
			for i, loc := range sites {
				tail := body[loc[1]:]
				if end := strings.IndexByte(tail, '\n'); end >= 0 {
					tail = tail[:end]
				}
				if loadSites[name][i] {
					alias := strings.TrimSuffix(strings.Split(body[loc[0]:loc[1]], "kind")[0], ".")
					guard := "AND NOT " + alias + ".cross_check_lane"
					if !strings.Contains(tail, guard) {
						t.Errorf("%s load site %d missing %s", key, i, guard)
					}
				} else if !strings.Contains(body[loc[0]:loc[1]], "'cross_check'") && d.reason == "" {
					t.Errorf("%s site %d needs lane exclusion or semantic allowlist", key, i)
				}
			}
		}
	}
	for key := range known {
		if !seen[key] {
			t.Errorf("audited predicate removed: %s", key)
		}
	}
}
