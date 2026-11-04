package store_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// namedQueryBodies returns every named query across queries/*.sql as name -> SQL with comment
// lines removed, so a guard cannot be satisfied by editing prose.
func namedQueryBodies(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir("queries")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("queries", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, block := range strings.Split(string(raw), "-- name: ")[1:] {
			name, _, _ := strings.Cut(block, " ")
			var sql strings.Builder
			for _, line := range strings.Split(block, "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "--") {
					continue
				}
				sql.WriteString(line + "\n")
			}
			out[name] = sql.String()
		}
	}
	if len(out) < 100 {
		t.Fatalf("scanned %d named queries: the scan did not run", len(out))
	}
	return out
}

// TestOnlyOneQueryInsertsAnswerRows pins the writer inventory the owner-steering snapshot relies
// on (issue #3841): an answer row carries its question_id column only because exactly one
// query creates answer rows and writes that column. Like its revise_plan sibling it sees SQL, not
// Go: a writer reusing the generic CreateRunInput with kind "answer" adds no literal here.
func TestOnlyOneQueryInsertsAnswerRows(t *testing.T) {
	var inserters []string
	for name, body := range namedQueryBodies(t) {
		if strings.Contains(body, "INSERT INTO run_user_inputs") && strings.Contains(body, "'answer'") {
			inserters = append(inserters, name)
		}
	}
	sort.Strings(inserters)
	if len(inserters) != 1 || inserters[0] != "CreateRunAnswerInput" {
		t.Fatalf("queries inserting an 'answer' row = %v, want exactly [CreateRunAnswerInput]", inserters)
	}
}

// TestOwnerSteeringClauseParity fails when a claim, placement, spread or health query carries the
// semantic plan-checker clause but not the owner-steering one: every site that gates a plan child
// on cross_check_semantic_plan_v1 must gate a steering-bearing child on cross_check_owner_steering_v1.
func TestOwnerSteeringClauseParity(t *testing.T) {
	const semantic, steering = "'cross_check_semantic_plan_v1'", "'cross_check_owner_steering_v1'"
	var seen int
	for name, body := range namedQueryBodies(t) {
		if !strings.Contains(body, semantic) {
			continue
		}
		seen++
		if strings.Count(body, steering) < 1 {
			t.Errorf("%s carries %s but not %s", name, semantic, steering)
		}
		// Claim copies must gate on the row's existence, not on a bare capability mention.
		if name != "GetRunSemanticHealth" && !strings.Contains(body, "JOIN cross_check_owner_steering s ON s.cross_check_id = pc.id") {
			t.Errorf("%s mentions %s without the steering-row EXISTS clause", name, steering)
		}
	}
	if seen < 5 {
		t.Fatalf("found only %d queries carrying the semantic clause: the scan did not run", seen)
	}
}
