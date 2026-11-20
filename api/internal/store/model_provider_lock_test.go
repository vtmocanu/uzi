package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The owner lock's class is a literal in queries/model_providers.sql (sqlc cannot bind a Go
// constant into the statement). This pins the generated statement, which is what executes, to the
// named constant in migrate.go, and proves the class is among the enumerated *LockClass constants
// whose pairwise distinctness TestProductTokenMintLockClassMatchesSQL asserts.
func TestModelProviderOwnerLockClassMatchesSQL(t *testing.T) {
	want := strconv.FormatInt(int64(ModelProviderOwnerLockClass), 10)
	if !strings.Contains(lockModelProviderOwner, "pg_advisory_xact_lock(\n    "+want+",") {
		t.Fatalf("LockModelProviderOwner does not lock class %s (ModelProviderOwnerLockClass):\n%s", want, lockModelProviderOwner)
	}
	classes := lockClassesFromSource(t)
	if got, ok := classes["ModelProviderOwnerLockClass"]; !ok || got != int64(ModelProviderOwnerLockClass) {
		t.Fatalf("source parse gave ModelProviderOwnerLockClass = %d (found %t), compiled value %d", got, ok, ModelProviderOwnerLockClass)
	}
	for name, v := range classes {
		if name != "ModelProviderOwnerLockClass" && v == int64(ModelProviderOwnerLockClass) {
			t.Errorf("ModelProviderOwnerLockClass collides with %s = %d", name, v)
		}
	}
}

var (
	sqlLineComment = regexp.MustCompile(`--[^\n]*`)
	// An UPDATE of model_provider_revisions, anywhere (a revision is immutable).
	updateRevisionsRE = regexp.MustCompile(`(?is)\bUPDATE\s+(?:ONLY\s+)?model_provider_revisions\b`)
	// An UPDATE of user_secrets whose SET list assigns provider_id, or an upsert on it.
	updateSecretsRE = regexp.MustCompile(`(?is)\bUPDATE\s+(?:ONLY\s+)?user_secrets\b.*?\bSET\b(.*?)(?:\bWHERE\b|\bFROM\b|\bRETURNING\b|$)`)
	doUpdateSetRE   = regexp.MustCompile(`(?is)\bINSERT\s+INTO\s+user_secrets\b.*?\bDO\s+UPDATE\s+SET\b(.*)$`)
	providerIDRE    = regexp.MustCompile(`(?i)\bprovider_id\b`)
)

// providerImmutabilityViolations returns a description of each statement in sqlText that updates
// model_provider_revisions or assigns user_secrets.provider_id after insert.
func providerImmutabilityViolations(sqlText string) []string {
	var out []string
	for _, stmt := range strings.Split(sqlLineComment.ReplaceAllString(sqlText, ""), ";") {
		stmt = strings.Join(strings.Fields(stmt), " ")
		if updateRevisionsRE.MatchString(stmt) {
			out = append(out, "UPDATE model_provider_revisions: "+stmt)
		}
		if m := updateSecretsRE.FindStringSubmatch(stmt); m != nil && providerIDRE.MatchString(m[1]) {
			out = append(out, "UPDATE user_secrets SET provider_id: "+stmt)
		}
		if m := doUpdateSetRE.FindStringSubmatch(stmt); m != nil && providerIDRE.MatchString(m[1]) {
			out = append(out, "upsert user_secrets SET provider_id: "+stmt)
		}
	}
	return out
}

// TestNoModelProviderRevisionOrKeyAssociationUpdate pins two immutability rules over every query
// file (PRD #4320): a provider revision is only ever inserted, and a provider key's provider_id is
// set at insert and never reassigned. The canary proves the scanner fires, so a green run cannot
// mean it matched nothing.
func TestNoModelProviderRevisionOrKeyAssociationUpdate(t *testing.T) {
	canaries := []string{
		"-- x\nUPDATE model_provider_revisions SET base_url = $1 WHERE provider_id = $2;",
		"UPDATE user_secrets\n   SET label = $1, provider_id = $2\n WHERE id = $3;",
		"INSERT INTO user_secrets (user_id) VALUES ($1) ON CONFLICT (user_id) DO UPDATE SET provider_id = $2;",
	}
	for _, c := range canaries {
		if len(providerImmutabilityViolations(c)) != 1 {
			t.Fatalf("the scanner missed its canary:\n%s", c)
		}
	}
	clean := "UPDATE user_secrets SET label = $1 WHERE user_id = $2 AND provider_id IS NULL RETURNING id;\n-- UPDATE model_provider_revisions SET x = 1;"
	if v := providerImmutabilityViolations(clean); len(v) != 0 {
		t.Fatalf("the scanner flagged a clean statement: %v", v)
	}

	files, err := filepath.Glob("queries/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob queries: %v (%d files)", err, len(files))
	}
	for _, f := range files {
		raw, err := os.ReadFile(f) //nolint:gosec // G304: f comes from a Glob over this package's own queries dir, not from input.
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range providerImmutabilityViolations(string(raw)) {
			t.Errorf("%s: %s", f, v)
		}
	}
}
