package workersvc

import (
	"os"
	"strings"
	"testing"
)

// The ListWaitingWorkerRuns exclusion is written in SQL as a literal LIKE prefix. It must stay
// equal to reasonStaleRequeuePinPrefix, or the queue.waiting alarm silently starts counting pinned
// runs (a prefix drift) without any other test noticing.
func TestStaleRequeuePinPrefixMatchesListWaitingWorkerRunsSQL(t *testing.T) {
	raw, err := os.ReadFile("../store/queries/runtime.sql") // #nosec G304 -- fixed repository query path
	if err != nil {
		t.Fatal(err)
	}
	want := "NOT COALESCE(health_reason LIKE '" + reasonStaleRequeuePinPrefix + "%', false)"
	if !strings.Contains(string(raw), want) {
		t.Fatalf("runtime.sql ListWaitingWorkerRuns does not contain %q", want)
	}
	if !isStaleRequeuePinReason(staleRequeuePinReason(t0, "w")) {
		t.Fatal("the rendered pin reason does not match its own prefix")
	}
}
