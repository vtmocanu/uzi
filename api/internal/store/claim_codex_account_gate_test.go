package store_test

import (
	"os"
	"strings"
	"testing"
)

// codexAccountGateAnchor opens PRD #1590's shared Codex account-hold predicate. The predicate
// is this anchor plus its balanced EXISTS (...) body.
const codexAccountGateAnchor = "r.harness = 'codex' AND r.codex_auth_mode = 'subscription' " +
	"AND r.kind IN ('issue', 'ci_fix', 'self_improve', 'prompt', 'task', 'mr_rework') AND EXISTS ("

// generatedQuery returns the SQL text of one generated sqlc constant, comment lines dropped and
// whitespace collapsed to single spaces.
func generatedQuery(t *testing.T, generated, constName string) string {
	t.Helper()
	parts := strings.SplitN(generated, "const "+constName+" = `", 2)
	if len(parts) != 2 {
		t.Fatalf("generated constant %s not found", constName)
	}
	// The constant ends at a lone backtick line; an inline backtick is spliced by sqlc.
	raw := strings.SplitN(parts[1], "\n`\n", 2)[0]
	var sql strings.Builder
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			sql.WriteString(line)
			sql.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(sql.String()), " ")
}

// accountGateCopies extracts every copy of the shared predicate from a normalised query.
func accountGateCopies(t *testing.T, name, body string) []string {
	t.Helper()
	var copies []string
	for _, tail := range strings.Split(body, codexAccountGateAnchor)[1:] {
		depth, finish := 1, -1 // the anchor ends inside EXISTS (
		for i, ch := range tail {
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
			t.Fatalf("%s: unbalanced account gate", name)
		}
		copies = append(copies, codexAccountGateAnchor+tail[:finish])
	}
	return copies
}

// TestCodexAccountGateCopiesIdentical pins PRD #1590 M2's single hold predicate across its four
// textual copies in the generated runtime.sql.go (sqlc cannot share a fragment and a SQL function
// would need a migration): ClaimRun's claimant gate and its peer mirror, the
// park_codex_account_unavailable page, and the health projection. The copies must be
// byte-identical after whitespace normalisation, and the claim gate must precede the
// custody-opening hold CTE. Behaviour is pinned by the LiveDB tests in workersvc
// (codex_account_gate_livedb_test.go).
func TestCodexAccountGateCopiesIdentical(t *testing.T) {
	raw, err := os.ReadFile("runtime.sql.go")
	if err != nil {
		t.Fatal(err)
	}
	generated := string(raw)
	var canonical string
	for _, q := range []struct {
		constName string
		want      int
	}{
		{"claimRun", 2},
		{"parkQueuedCodexAccountUnavailablePage", 1},
		{"listActiveRunsForHealth", 1},
	} {
		body := generatedQuery(t, generated, q.constName)
		copies := accountGateCopies(t, q.constName, body)
		if len(copies) != q.want {
			t.Fatalf("%s: %d copies of the account gate, want %d", q.constName, len(copies), q.want)
		}
		for _, c := range copies {
			if canonical == "" {
				canonical = c
			}
			if c != canonical {
				t.Fatalf("%s: account gate differs from ClaimRun's:\n got %s\nwant %s", q.constName, c, canonical)
			}
		}
		if q.constName == "claimRun" && strings.Index(body, codexAccountGateAnchor) > strings.Index(body, "hold AS (") {
			t.Fatal("claimRun: account gate follows the custody hold CTE")
		}
	}
	for _, required := range []string{
		"cpa.coord_state = 'quarantined'",
		"ccs.status IN ('staging', 'failed')",
		"ccs.status = 'linked'",
		"ccs.material_revision > r.codex_material_revision",
		"cpa.credential_revision = r.codex_account_revision",
		"jsonb_build_array(cpa.provider_user_id, cpa.workspace_account_id)",
	} {
		if !strings.Contains(canonical, required) {
			t.Errorf("account gate is missing %q", required)
		}
	}
}
