package workersvc

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// queryBlock returns the text of one sqlc query from `-- name: <name>` up to the next `-- name:`,
// so an assertion is anchored to its own query rather than matching anywhere in the file.
func queryBlock(t *testing.T, sql, name string) string {
	t.Helper()
	start := strings.Index(sql, "-- name: "+name+" ")
	if start < 0 {
		t.Fatalf("runtime.sql has no query %q", name)
	}
	block := sql[start+len("-- name: "):]
	if end := strings.Index(block, "-- name:"); end >= 0 {
		block = block[:end]
	}
	return block
}

// The ListWaitingWorkerRuns (queue.waiting) and ListOwnersWaitingNoCapacity (fleet.capacity)
// exclusions are written in SQL as a literal LIKE prefix. It must stay equal to
// reasonStaleRequeuePinPrefix, or an alarm silently starts counting pinned runs (a prefix drift)
// without any other test noticing. Each check is anchored to its own query block.
func TestStaleRequeuePinPrefixMatchesListWaitingWorkerRunsSQL(t *testing.T) {
	raw, err := os.ReadFile("../store/queries/runtime.sql") // #nosec G304 -- fixed repository query path
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ query, want string }{
		{"ListWaitingWorkerRuns", "NOT COALESCE(health_reason LIKE '" + reasonStaleRequeuePinPrefix + "%', false)"},
		{"ListOwnersWaitingNoCapacity", "NOT COALESCE(r.health_reason LIKE '" + reasonStaleRequeuePinPrefix + "%', false)"},
	} {
		if block := queryBlock(t, string(raw), c.query); !strings.Contains(block, c.want) {
			t.Errorf("runtime.sql %s does not contain %q", c.query, c.want)
		}
	}
	if !isStaleRequeuePinReason(staleRequeuePinReason(t0, "w")) ||
		!isStaleRequeuePinReason(ownerHoldReason(t0, "w")) {
		t.Fatal("the rendered pin reason does not match its own prefix")
	}
}

// Normalize executable SQL only: prose cannot satisfy a predicate assertion.
func executableSQL(s string) string {
	s = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(s, "")
	s = regexp.MustCompile(`(?m)--[^\n]*`).ReplaceAllString(s, "")
	return strings.Join(strings.Fields(s), "")
}

func TestOwnerHoldSQLMirrorsClaimRun(t *testing.T) {
	raw, err := os.ReadFile("../store/queries/runtime.sql") // #nosec G304 -- fixed repository query path
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	claim := executableSQL(queryBlock(t, sql, "ClaimRun"))
	if !strings.Contains(claim, "AND(r.kind='cross_check'ORr.worker_idISNULL") {
		t.Fatal("ClaimRun affinity predicate lacks cross_check exemption")
	}
	if !strings.Contains(claim, "ORr.updated_at<@affinity_cutoff)") {
		t.Fatal("ClaimRun affinity predicate lacks strict ceiling")
	}
	start := strings.Index(claim, "(ow.draining_sinceISNOTNULLOR")
	if start < 0 {
		t.Fatal("ClaimRun owner predicate missing")
	}
	tail := claim[start+1:]
	end := strings.Index(tail, "OR(ow.last_heartbeat_at")
	if end < 0 {
		t.Fatal("ClaimRun heartbeat arm missing")
	}
	owner := tail[:end]
	for _, name := range []string{"ListUnplaceableQueuedRunsForEphemeral", "ListSaturationQueuedRunsForEphemeral", "ListActiveRunsForHealth"} {
		t.Run(name, func(t *testing.T) {
			block := executableSQL(queryBlock(t, sql, name))
			want := "ANDNOTCOALESCE(r.worker_idISNOTNULLANDr.kind<>'cross_check'ANDr.updated_at>=@affinity_cutoff::timestamptzANDEXISTS(SELECT1FROMworkersowWHEREow.id=r.worker_idAND(" + owner + ")),false)"
			if name == "ListActiveRunsForHealth" {
				want = "COALESCE(runs.status='queued'ANDruns.kind<>'cross_check'ANDruns.worker_idISNOTNULLANDEXISTS(SELECT1FROMworkersowWHEREow.id=runs.worker_idAND(" + owner + ")),false)::booleanASowner_hold_pinnable"
			}
			if !strings.Contains(block, want) {
				t.Fatalf("%s lacks complete owner hold predicate matching executable ClaimRun: %s", name, want)
			}
		})
	}
}
