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
	// The shared catalogue lock is the same class keyed by the nil UUID, which no real owner can use
	// (model_providers_owner_not_nil_check), so it never collides with an owner's create lock.
	wantShared := "pg_advisory_xact_lock(\n    " + want + ",\n    hashtext('00000000-0000-0000-0000-000000000000')\n)"
	if !strings.Contains(lockSharedModelProviderCatalogue, wantShared) {
		t.Fatalf("LockSharedModelProviderCatalogue does not lock class %s keyed by the nil UUID:\n%s", want, lockSharedModelProviderCatalogue)
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
	// Any UPDATE ... SET, or an upsert's DO UPDATE SET, up to the end of the assignment list.
	anyUpdateSetRE = regexp.MustCompile(`(?is)(?:\bUPDATE\s+(?:ONLY\s+)?\w+(?:\s+(?:AS\s+)?\w+)?\s+SET\b|\bDO\s+UPDATE\s+SET\b)(.*?)(?:\bWHERE\b|\bFROM\b|\bRETURNING\b|$)`)
	// Assignment to a column that ties a definition or a key to its scope (PRD #4324): set at insert,
	// never reassigned, so a definition can never move between owners or into the shared catalogue.
	scopeAssignRE          = regexp.MustCompile(`(?i)\b(owner_user_id|scope_owner_id|credential_mode|provider_scope_id|provider_id)\s*=`)
	scopeTupleAssignRE     = regexp.MustCompile(`(?i)\(([a-z_][a-z_0-9]*(?:\s*,\s*[a-z_][a-z_0-9]*)*)\)\s*=`)
	immutableScopeColumnRE = regexp.MustCompile(`(?i)\b(owner_user_id|scope_owner_id|credential_mode|provider_scope_id|provider_id)\b`)
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
		for _, m := range anyUpdateSetRE.FindAllStringSubmatch(stmt, -1) {
			if col := scopeAssignRE.FindStringSubmatch(m[1]); col != nil {
				out = append(out, "UPDATE assigns "+col[1]+": "+stmt)
			}
			for _, tuple := range scopeTupleAssignRE.FindAllStringSubmatch(m[1], -1) {
				for _, col := range immutableScopeColumnRE.FindAllString(tuple[1], -1) {
					out = append(out, "UPDATE assigns "+col+": "+stmt)
				}
			}
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
		"UPDATE model_providers SET owner_user_id = $1 WHERE id = $2;",
		"UPDATE model_providers p SET scope_owner_id = $1, name = $3 WHERE id = $2;",
		"UPDATE model_providers SET credential_mode = 'per_user_key' WHERE id = $1;",
		"UPDATE user_secrets SET label = $1, provider_scope_id = $2 WHERE id = $3;",
		"INSERT INTO model_providers (id) VALUES ($1) ON CONFLICT (id) DO UPDATE SET owner_user_id = NULL;",
	}
	for _, c := range canaries {
		if len(providerImmutabilityViolations(c)) == 0 {
			t.Fatalf("the scanner missed its canary:\n%s", c)
		}
	}
	tuple := "UPDATE model_providers SET (name, owner_user_id, scope_owner_id) = ($1, $2, $3) WHERE id = $4;"
	got := strings.Join(providerImmutabilityViolations(tuple), "\n")
	for _, column := range []string{"owner_user_id", "scope_owner_id"} {
		if !strings.Contains(got, "UPDATE assigns "+column+":") {
			t.Errorf("tuple assignment missed %s: %q", column, got)
		}
	}
	clean := "UPDATE user_secrets SET label = $1 WHERE user_id = $2 AND provider_id IS NULL RETURNING id;\n-- UPDATE model_provider_revisions SET x = 1;\n" +
		"UPDATE users u SET claude_provider_id = $1, claude_provider_scope_id = (SELECT sp.scope_owner_id FROM model_providers sp WHERE sp.id = $1) WHERE u.id = $2 AND u.id = $3;"
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
