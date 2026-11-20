package store_test

import (
	"os"
	"strings"
	"testing"
)

// provSQL returns the executable SQL of file with comments stripped and all whitespace removed,
// so each pin compares text and not layout.
func provSQL(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is a fixed file inside this package's queries/migrations dirs.
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(strings.Fields(stripSQLComments(string(raw))), "")
}

// provBlock returns the named query's text from a queries file, whitespace-stripped.
func provBlock(t *testing.T, path, name string) string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is a fixed file inside this package's queries dir.
	if err != nil {
		t.Fatal(err)
	}
	for _, block := range strings.Split(string(raw), "\n-- name: ")[1:] {
		if strings.Fields(block)[0] == name {
			return strings.Join(strings.Fields(stripSQLComments(block)), "")
		}
	}
	t.Fatalf("query %s not found in %s", name, path)
	return ""
}

// TestClaimProvenanceClausesPinnedPerQuery pins issue #4476's claim-provenance gate to the
// claim and its per-worker mirrors, and its absence from the isolated-lane demand query.
func TestClaimProvenanceClausesPinnedPerQuery(t *testing.T) {
	const rt, hib = "queries/runtime.sql", "queries/hibernation.sql"
	codex := func(a string) string {
		return "(" + a + ".harness='codex'OR" + a + ".codex_material_revisionISNOTNULLOR" + a + ".codex_secret_idISNOTNULL)"
	}
	claim := provBlock(t, rt, "ClaimRun")
	for _, c := range []struct {
		name, text string
		want       int
	}{
		{"claimant gate", "fn_codex_claim_provenance_admits(r,@worker_id", 1},
		{"peer gate", "fn_codex_claim_provenance_admits(r,p.id)", 1},
		{"worker assignment", "claim_provenance_worker_id=CASEWHEN" + codex("runs") + "THEN@worker_id::uuidELSEruns.claim_provenance_worker_idEND", 1},
		{"generation assignment", "claim_provenance_generation=CASEWHEN" + codex("runs") + "THENruns.claim_generation+1ELSEruns.claim_provenance_generationEND", 1},
	} {
		if n := strings.Count(claim, c.text); n != c.want {
			t.Errorf("ClaimRun %s: %d occurrences, want %d", c.name, n, c.want)
		}
	}
	if n := strings.Count(claim, "fn_codex_claim_provenance_admits("); n != 2 {
		t.Errorf("ClaimRun carries %d gate calls, want exactly 2", n)
	}
	for _, c := range []struct{ file, query, text string }{
		{rt, "CountOnlineWorkersClaimableForRun", "fn_codex_claim_provenance_admits(run,w.id)"},
		{rt, "ListUnplaceableQueuedRunsForEphemeral", "fn_codex_claim_provenance_admits(r,NULL::uuid)"},
		{rt, "ListSaturationQueuedRunsForEphemeral", "fn_codex_claim_provenance_admits(r,NULL::uuid)"},
		{hib, "ListHibernationWakeDemand", "fn_codex_claim_provenance_admits(r,w.id)"},
	} {
		if n := strings.Count(provBlock(t, c.file, c.query), c.text); n != 1 {
			t.Errorf("%s: %d occurrences of %s, want 1", c.query, n, c.text)
		}
	}
	iso := provBlock(t, rt, "ListIsolatedQueuedRunsForEphemeral")
	if !strings.Contains(iso, "ANDNOT"+codex("r")) {
		t.Error("ListIsolatedQueuedRunsForEphemeral lost its Codex exclusion")
	}
	if strings.Contains(iso, "fn_codex_claim_provenance_admits") {
		t.Error("ListIsolatedQueuedRunsForEphemeral must not carry the provenance gate")
	}
}

// TestClaimProvenanceMigrationText pins the gate function body and the guard conditions.
func TestClaimProvenanceMigrationText(t *testing.T) {
	sql := provSQL(t, "migrations/00493_run_claim_provenance.sql")
	body := "SELECTNOT(r.harness='codex'ORr.codex_material_revisionISNOTNULLORr.codex_secret_idISNOTNULL)" +
		"ORr.claim_generation<1" +
		"ORCOALESCE(r.claim_provenance_worker_id=claimantANDr.claim_provenance_generation=r.claim_generation,false)"
	if !strings.Contains(sql, "$$"+body+"$$") {
		t.Error("gate function body differs from the expected text")
	}
	for _, c := range []string{
		"NEW.claim_generation=OLD.claim_generation+1",
		"((OLD.claim_generation<1ANDOLD.claim_provenance_worker_idISNULLANDOLD.claim_provenance_generationISNULL)OR(OLD.claim_generation>=1ANDCOALESCE(OLD.claim_provenance_worker_id=NEW.worker_idANDOLD.claim_provenance_generation=OLD.claim_generation,false)))",
		"(OLD.claim_provenance_worker_idISNULL)=(OLD.claim_provenance_generationISNULL)",
		"(NEW.claim_provenance_worker_idISNULL)=(NEW.claim_provenance_generationISNULL)",
		"NEW.claim_provenance_worker_idISNOTNULLANDNEW.claim_provenance_worker_id=NEW.worker_idANDNEW.claim_provenance_generation=NEW.claim_generation)ISNOTTRUETHEN",
		"CREATETRIGGERtrg_runs_claim_provenance_guardBEFOREUPDATEONruns",
	} {
		if !strings.Contains(sql, c) {
			t.Errorf("migration lacks %q", c)
		}
	}
}
