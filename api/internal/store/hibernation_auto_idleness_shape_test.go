package store

import (
	"strings"
	"testing"
)

// TestGetWorkerIdlenessIsOneStatement pins the single-snapshot contract of the automatic
// hibernation idle re-check (issue #4207). A run finishing takes no worker lock, so under READ
// COMMITTED an anchor read and a separate non-terminal count read could straddle that finish
// (old anchor, count 0) and report a just-active worker as idle. Both values must therefore
// come from ONE statement. A behavioural LiveDB interleaving cannot reach between the two
// reads of a single statement, so this structural regression is the guard: splitting the
// query (or dropping either column) fails here, and dropping a field from
// GetWorkerIdlenessRow fails to compile.
func TestGetWorkerIdlenessIsOneStatement(t *testing.T) {
	q := getWorkerIdleness
	body := strings.TrimSpace(q[strings.Index(q, "\n"):]) // drop the "-- name:" header line
	if strings.Contains(strings.TrimRight(body, "; \n\t"), ";") {
		t.Fatalf("GetWorkerIdleness must be one statement, found a statement separator:\n%s", q)
	}
	if n := strings.Count(strings.ToUpper(body), "FROM WORKERS"); n != 1 {
		t.Fatalf("GetWorkerIdleness must read the worker once (one outer SELECT), FROM workers appears %d times:\n%s", n, q)
	}
	for _, want := range []string{"AS nonterminal", "AS anchor", "GREATEST("} {
		if !strings.Contains(body, want) {
			t.Fatalf("GetWorkerIdleness must compute %q in the same statement:\n%s", want, q)
		}
	}
	// Compile-time: both values arrive in one row.
	var row GetWorkerIdlenessRow
	_, _ = row.Nonterminal, row.Anchor
}
