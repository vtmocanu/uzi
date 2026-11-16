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
	body := funcBody(t, "hibernation_auto.go", "func (s *Service) autoAdmit(")
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

// TestSurplusDecisionReadsIdlenessOnce is the surplus-mode half (issue #4395): a surplus decision
// takes every member's non-terminal count and anchor from the ONE GetOwnerHibernationPoolInput
// statement read after both locks, never from GetWorkerIdleness or a separate count; and the
// pure decision reads exactly those two row fields. Its long-rule fallback goes through autoAdmit,
// which reads idleness once (TestAutoAdmitReadsIdlenessOnce), so the per-mode count is one.
func TestSurplusDecisionReadsIdlenessOnce(t *testing.T) {
	body := funcBody(t, "hibernation_pool.go", "func (s *Service) poolAttempt(")
	if n := strings.Count(body, ".GetOwnerHibernationPoolInput("); n != 1 {
		t.Fatalf("poolAttempt must read the owner pool exactly once, found %d", n)
	}
	for _, banned := range []string{".GetWorkerIdleness(", ".CountWorkerNonTerminalRuns(", ".ListWorkerNonTerminalRuns("} {
		if strings.Contains(body, banned) {
			t.Fatalf("poolAttempt must not read idleness through a separate query (%s)", banned)
		}
	}
	decide := funcBody(t, "hibernation_pool.go", "func decideSurplus(")
	for _, want := range []string{"c.Nonterminal", "c.Anchor"} {
		if !strings.Contains(decide, want) {
			t.Fatalf("decideSurplus must decide on %s from the pool input row", want)
		}
	}
}

// funcBody returns the source of the function that starts with header in file.
func funcBody(t *testing.T, file, header string) string {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	start := strings.Index(text, header)
	if start < 0 {
		t.Fatalf("%s not found in %s; update this test if it was renamed", header, file)
	}
	body := text[start:]
	if end := strings.Index(body[1:], "\nfunc "); end >= 0 {
		body = body[:end+1]
	}
	return body
}
