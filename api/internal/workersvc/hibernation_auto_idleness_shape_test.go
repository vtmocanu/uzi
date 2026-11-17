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
// statement read after both locks, never from GetWorkerIdleness or a separate idleness read; and
// the pure decision reads exactly those two row fields. The admission itself still goes through
// hibernateRefusal, which counts the candidate's non-terminal runs again under its row lock as the
// same refusal gate every hibernation uses; that count is a recheck, not a decision input. The
// pooled long rule reads idleness once through admitPooledLong
// (TestPooledLongAdmissionReadsIdlenessOnce) and an unpooled candidate goes through autoAdmit,
// which reads idleness once (TestAutoAdmitReadsIdlenessOnce).
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

// TestPooledLongAdmissionReadsIdlenessOnce pins the surplus-mode half of the single-snapshot
// contract for the long rule (issue #4395): a pooled worker's long recheck takes its non-terminal
// count, pooledness, long anchor and owner-active flag from ONE GetWorkerLongIdleness row, and
// decides on exactly those; it never mixes in GetWorkerIdleness (today's per-worker anchor) or a
// separate count. The warm decision likewise reads the owner's pool exactly once.
func TestPooledLongAdmissionReadsIdlenessOnce(t *testing.T) {
	body := funcBody(t, "hibernation_pool.go", "func (s *Service) admitPooledLong(")
	if n := strings.Count(body, ".GetWorkerLongIdleness("); n != 1 {
		t.Fatalf("admitPooledLong must call GetWorkerLongIdleness exactly once, found %d", n)
	}
	for _, banned := range []string{".GetWorkerIdleness(", ".CountWorkerNonTerminalRuns(", ".ListWorkerNonTerminalRuns("} {
		if strings.Contains(body, banned) {
			t.Fatalf("admitPooledLong must not read idleness through a separate query (%s)", banned)
		}
	}
	for _, want := range []string{"idle.Pooled", "idle.Nonterminal", "idle.OwnerActiveNow", "idle.Anchor"} {
		if !strings.Contains(body, want) {
			t.Fatalf("admitPooledLong must decide on %s from the single GetWorkerLongIdleness row", want)
		}
	}
	warm := funcBody(t, "hibernation_warm.go", "func (s *Service) warmAttempt(")
	if n := strings.Count(warm, ".GetOwnerHibernationPoolInput("); n != 1 {
		t.Fatalf("warmAttempt must read the owner pool exactly once, found %d", n)
	}
	for _, banned := range []string{".GetWorkerIdleness(", ".GetWorkerLongIdleness(", ".InsertHibernationWake(", ".WakeWorker(", ".HibernateWorker(", ".DemandWake("} {
		if strings.Contains(warm, banned) {
			t.Fatalf("warmAttempt must not call %s", banned)
		}
	}
}

// funcBody returns the source of the function that starts with header in file.
func funcBody(t *testing.T, file, header string) string {
	t.Helper()
	src, err := os.ReadFile(file) //nolint:gosec // G304: callers pass fixed repo-relative source file names in a test, not user input
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
