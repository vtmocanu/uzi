package workersvc

import (
	"os"
	"strings"
	"testing"
)

// TestAutoAdmitReadsIdlenessOnce pins the other half of the single-snapshot contract
// (issue #4207): autoAdmit must take the non-terminal count AND the anchor from the same
// GetWorkerIdleness row, read once after the worker lock. A refactor that reads the anchor
// and the count through separate queries (in either order) reintroduces the READ COMMITTED
// straddle that TestGetWorkerIdlenessIsOneStatement documents. LiveDB tests cannot interleave
// between two reads of one statement, so this is a source-shape regression, like
// sweeper_pass_order_test.go in cmd/server.
func TestAutoAdmitReadsIdlenessOnce(t *testing.T) {
	src, err := os.ReadFile("hibernation_auto.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	start := strings.Index(text, "func (s *Service) autoAdmit(")
	if start < 0 {
		t.Fatal("autoAdmit not found in hibernation_auto.go; update this test if it was renamed")
	}
	body := text[start:]
	if end := strings.Index(body[1:], "\nfunc "); end >= 0 {
		body = body[:end+1]
	}
	if n := strings.Count(body, ".GetWorkerIdleness("); n != 1 {
		t.Fatalf("autoAdmit must call GetWorkerIdleness exactly once, found %d", n)
	}
	for _, banned := range []string{".CountWorkerNonTerminalRuns(", ".ListWorkerNonTerminalRuns("} {
		if strings.Contains(body, banned) {
			t.Fatalf("autoAdmit must not read idleness through a separate query (%s)", banned)
		}
	}
	for _, want := range []string{"idle.Nonterminal", "idle.Anchor"} {
		if !strings.Contains(body, want) {
			t.Fatalf("autoAdmit must decide on %s from the single GetWorkerIdleness row", want)
		}
	}
}
