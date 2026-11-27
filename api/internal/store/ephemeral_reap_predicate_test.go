package store_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reapPredicateText returns the reap predicate of the named statement in queries/hosted_workers.sql,
// normalized so the lock statement and the delete statement compare equal when their predicates
// are the same text: from the `w.ephemeral` conjunct through the end of the statement, without the
// leading WHERE/AND keyword, the delete's `w.id = ANY(@ids::uuid[])` line, the lock's
// `FOR UPDATE OF w SKIP LOCKED` tail and the statement terminator.
func reapPredicateText(t *testing.T, sql, name string) string {
	t.Helper()
	marker := "-- name: " + name + " "
	start := strings.Index(sql, marker)
	if start < 0 {
		t.Fatalf("statement %s not found in queries/hosted_workers.sql", name)
	}
	body := sql[start:]
	if next := strings.Index(body[len(marker):], "\n-- name: "); next >= 0 {
		body = body[:len(marker)+next]
	}
	var out []string
	in := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !in {
			switch trimmed {
			case "WHERE w.ephemeral", "AND w.ephemeral":
				in = true
				out = append(out, "w.ephemeral")
			}
			continue
		}
		if trimmed == "FOR UPDATE OF w SKIP LOCKED;" {
			break
		}
		out = append(out, strings.TrimRight(line, " \t"))
	}
	if !in {
		t.Fatalf("statement %s: no `w.ephemeral` predicate line found", name)
	}
	return strings.TrimSuffix(strings.TrimRight(strings.Join(out, "\n"), " \t\n"), ";")
}

// LockReapableEphemeralWorkers and DeleteLockedEphemeralWorkers must carry the SAME reap
// predicate: the delete re-checks, on a fresh snapshot, exactly what the lock statement selected.
// No database needed; the statements are compared as text.
func TestEphemeralReapPredicateIdenticalAcrossLockAndDelete(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("queries", "hosted_workers.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	lock := reapPredicateText(t, sql, "LockReapableEphemeralWorkers")
	del := reapPredicateText(t, sql, "DeleteLockedEphemeralWorkers")
	if lock != del {
		la, da := strings.Split(lock, "\n"), strings.Split(del, "\n")
		for i := 0; i < len(la) || i < len(da); i++ {
			var l, d string
			if i < len(la) {
				l = la[i]
			}
			if i < len(da) {
				d = da[i]
			}
			if l != d {
				t.Fatalf("reap predicates diverge at normalized line %d:\n  lock:   %q\n  delete: %q", i+1, l, d)
			}
		}
	}
	for name, p := range map[string]string{"lock": lock, "delete": del} {
		normalized := strings.Join(strings.Fields(p), " ")
		for _, clause := range []string{
			"(r.harness = 'claude' AND 'claude_provider_v1' = ANY(w.protocol_capabilities) AND (r.kind <> 'judge' OR 'claude_provider_judge_v1' = ANY(w.protocol_capabilities)))",
			"(r.harness = 'codex' AND 'codex_provider_v1' = ANY(w.protocol_capabilities) AND 'codex_harness_v1' = ANY(w.protocol_capabilities) AND 'codex_runtime_v2' = ANY(w.protocol_capabilities) AND (r.kind <> 'judge' OR 'codex_provider_judge_v1' = ANY(w.protocol_capabilities)))",
		} {
			if !strings.Contains(normalized, clause) {
				t.Errorf("%s predicate lacks engine-specific provider arm %q", name, clause)
			}
		}
		if !strings.Contains(p, "isolated_fetch_v1") {
			t.Errorf("%s predicate lost the isolated_fetch_v1 arm (e)", name)
		}
	}
}
