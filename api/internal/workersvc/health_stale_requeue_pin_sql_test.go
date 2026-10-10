package workersvc

import (
	"os"
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
	if !isStaleRequeuePinReason(staleRequeuePinReason(t0, "w")) {
		t.Fatal("the rendered pin reason does not match its own prefix")
	}
}
