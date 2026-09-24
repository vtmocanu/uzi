package store_test

import (
	"os"
	"strings"
	"testing"
)

// The claimant and peer must apply identical account admission before ClaimRun's hold CTE.
func TestClaimRunCodexAccountGate(t *testing.T) {
	source, err := os.ReadFile("queries/runtime.sql")
	if err != nil {
		t.Fatal(err)
	}
	generated, err := os.ReadFile("runtime.sql.go")
	if err != nil {
		t.Fatal(err)
	}
	const anchor = "AND NOT ( r.harness = 'codex' AND r.codex_auth_mode = 'subscription'"
	var sourceClause string
	for _, input := range []struct {
		name string
		raw  []byte
	}{{"source", source}, {"generated", generated}} {
		query := string(input.raw)
		if input.name == "source" {
			query = strings.SplitN(query, "-- name: ClaimRun :one", 2)[1]
			query = strings.SplitN(query, "-- name: ", 2)[0]
		} else {
			query = strings.SplitN(query, "const claimRun = `", 2)[1]
			query = strings.SplitN(query, "type ClaimRunParams struct", 2)[0]
		}
		var sql strings.Builder
		for _, line := range strings.Split(query, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "--") {
				sql.WriteString(line)
				sql.WriteByte(' ')
			}
		}
		body := strings.Join(strings.Fields(sql.String()), " ")
		if strings.Count(body, anchor) != 2 {
			t.Fatalf("%s: want claimant and peer gates, found %d", input.name, strings.Count(body, anchor))
		}
		clauses := make([]string, 0, 2)
		for _, tail := range strings.Split(body, anchor)[1:] {
			clause := anchor + tail
			depth, finish := 0, -1
			for i, ch := range clause {
				if ch == '(' {
					depth++
				}
				if ch == ')' {
					depth--
					if depth == 0 {
						finish = i + 1
						break
					}
				}
			}
			if finish < 0 {
				t.Fatalf("%s: unbalanced account gate", input.name)
			}
			clauses = append(clauses, clause[:finish])
		}
		if clauses[0] != clauses[1] {
			t.Fatalf("%s: claimant and peer predicates differ", input.name)
		}
		if sourceClause != "" && sourceClause != clauses[0] {
			t.Fatal("generated gate differs from source")
		}
		sourceClause = clauses[0]
		for _, required := range []string{
			"r.kind IN ('issue', 'ci_fix', 'self_improve', 'prompt', 'task', 'mr_rework')",
			"EXISTS ( SELECT 1 FROM codex_credential_state ccs LEFT JOIN codex_provider_account cpa",
			"cpa.id = ccs.provider_account_id AND cpa.user_id = r.user_id",
			"ccs.user_secret_id = r.codex_secret_id AND ccs.user_id = r.user_id",
			"cpa.coord_state = 'quarantined'",
			"r.codex_account_key IS NOT NULL",
			"ccs.status IN ('staging', 'failed')",
			"ccs.material_revision > r.codex_material_revision",
		} {
			if !strings.Contains(clauses[0], required) {
				t.Errorf("%s: missing %q", input.name, required)
			}
		}
		if strings.Index(body, anchor) > strings.Index(body, "hold AS (") {
			t.Fatalf("%s: account gate follows custody hold", input.name)
		}
	}
}
