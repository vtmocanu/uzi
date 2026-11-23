package store

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// PRD #4324: a shared definition has a NULL owner, so "who may see this provider row" is a SQL
// predicate each query carries, and a query that kept only the owner arm silently hides shared
// definitions from recipients (or, the other way round, a shared-admin query that lost its scope
// would touch personal rows). This is a non-DB pin over queries/model_providers.sql: every named
// query is classified into exactly one class, and each class has a text rule it must satisfy. A new
// or renamed query that is not in the table fails, so adding a query forces a visibility decision.

type visClass int

const (
	classOwner    visClass = iota // owner-only: the predicate stays the owner arm, never widened
	classVIS                      // recipient read: own definition OR shared per-user-key definition
	classShared                   // shared-admin: scoped to NULL owner and the per-user-key mode
	classIdentity                 // keyed by identity (row ids, key ids), no ownership predicate of its own
)

type visEntry struct {
	class visClass
	// must replaces the class's default text rule when the statement is not a plain WHERE (an INSERT
	// or an advisory lock): every substring must appear in the normalized, comment-free statement.
	must []string
	// caller records the authorized caller of a shared-admin query: reviewed here, not computed.
	caller string
}

const (
	nilScopeSQL = `'00000000-0000-0000-0000-000000000000'::uuid`
	adminCreate = "admin create (cookie + CSRF)"
	adminEdit   = "admin edit (cookie + CSRF)"
	adminDelete = "admin delete (cookie + CSRF)"
	adminRead   = "admin list/get (cookie, or the admin-read token)"
)

// visibilityTable is the classification. Keep it sorted by class, then name.
var visibilityTable = map[string]visEntry{
	// owner-only
	"CountModelProvidersForOwner":    {class: classOwner},
	"BumpModelProviderRevision":      {class: classOwner},
	"PruneModelProviderRevisions":    {class: classOwner},
	"RenameModelProvider":            {class: classOwner},
	"RetireModelProvider":            {class: classOwner},
	"DeleteModelProvider":            {class: classOwner},
	"ListModelProvidersForOwner":     {class: classOwner},
	"GetModelProviderForOwner":       {class: classOwner},
	"UpsertModelProviderProbeResult": {class: classOwner},
	"InsertModelProvider": {class: classOwner, must: []string{
		"VALUES (@owner_user_id::uuid, @name, @engine, 1, @owner_user_id::uuid)"}},
	"LockModelProviderOwner": {class: classOwner, must: []string{"hashtext(sqlc.arg(owner_user_id)::uuid::text)"}},

	// recipient reads and the recipient write path: the exact visibility predicate
	"GetProviderKeyForUpdate":                      {class: classVIS},
	"InsertProviderKeySecret":                      {class: classVIS},
	"GetUserProviderSource":                        {class: classVIS},
	"SetUserProviderSource":                        {class: classVIS},
	"LockModelProviderForShare":                    {class: classVIS},
	"GetModelProviderRevisionForOwner":             {class: classVIS},
	"LockProviderBindingRevisionForKeyShareNowait": {class: classVIS},
	"ListRunProviderNames":                         {class: classVIS},
	"ListVisibleProviders":                         {class: classVIS},
	"GetVisibleProviderForUse":                     {class: classVIS},
	"LockVisibleProviderForKeyMutation":            {class: classVIS},
	"LockVisibleProviderForEvidence":               {class: classVIS},

	// shared-admin: NULL owner and the per-user-key mode, with the authorized caller
	"LockSharedModelProviderCatalogue": {class: classShared, caller: adminCreate, must: []string{"hashtext('00000000-0000-0000-0000-000000000000')"}},
	"CountSharedModelProviders":        {class: classShared, caller: adminCreate},
	"InsertSharedModelProvider": {class: classShared, caller: adminCreate, must: []string{
		"VALUES (NULL, @name, 'claude', 1, 'per_user_key', " + nilScopeSQL + ")"}},
	"LockSharedModelProviderForEdit":    {class: classShared, caller: adminEdit},
	"BumpSharedModelProviderRevision":   {class: classShared, caller: adminEdit},
	"RenameSharedModelProvider":         {class: classShared, caller: adminEdit},
	"RetireSharedModelProvider":         {class: classShared, caller: adminEdit},
	"PruneSharedModelProviderRevisions": {class: classShared, caller: adminEdit},
	"LockSharedModelProviderForDelete":  {class: classShared, caller: adminDelete},
	"DeleteSharedModelProvider":         {class: classShared, caller: adminDelete},
	"DeleteSharedProviderKeys":          {class: classShared, caller: adminDelete, must: []string{"provider_scope_id = " + nilScopeSQL}},
	"CountSharedProviderSelections":     {class: classShared, caller: adminDelete, must: []string{"claude_provider_scope_id = " + nilScopeSQL}},
	"ListSharedModelProvidersAdmin":     {class: classShared, caller: adminRead},
	"GetSharedModelProviderAdmin":       {class: classShared, caller: adminRead},

	// identity-only: keyed by an id the caller already holds under a prior visibility check
	"InsertModelProviderRevision":          {class: classIdentity, must: []string{"WHERE p.id = @provider_id"}},
	"GetModelProviderRevision":             {class: classIdentity, must: []string{"WHERE provider_id = $1 AND revision = $2"}},
	"GetProviderBindingKeyByID":            {class: classIdentity, must: []string{"WHERE id = $1 AND user_id = $2"}},
	"LockProviderBindingKeyForShareNowait": {class: classIdentity, must: []string{"WHERE id = $1 AND user_id = $2"}},
	"LockProviderKeyForEvidence":           {class: classIdentity, must: []string{"k.provider_id ="}},
	"UpsertProviderKeyTestResult":          {class: classIdentity, must: []string{"ON CONFLICT (provider_id, user_id)"}},
	"DeleteProviderKeySecret":              {class: classIdentity, must: []string{"WHERE user_id = $1 AND provider_id = $2"}},
	"GetUserProviderLaneSources":           {class: classIdentity, must: []string{"WHERE id = $1"}},
}

var (
	queryNameRE = regexp.MustCompile(`(?m)^-- name: (\w+) :\w+`)
	// The exact visibility predicate with the caller's id expression as the one free part.
	visRE = regexp.MustCompile(`\(p\.owner_user_id = [^\s()]+ OR \(p\.owner_user_id IS NULL AND p\.credential_mode = 'per_user_key'\)\)`)
	// A predicate on the credential mode (a RETURNING column list is not one).
	credentialModeTestRE = regexp.MustCompile(`credential_mode\s*(?:=|<>|IN\b|IS\b)`)
	// A shared-admin scope: NULL owner and the per-user-key mode, with or without the alias.
	sharedScopeRE = regexp.MustCompile(`owner_user_id IS NULL AND (?:p\.)?credential_mode = 'per_user_key'`)
)

// statementsOf splits a query file into name -> normalized, comment-free statement text.
func statementsOf(sqlText string) map[string]string {
	out := map[string]string{}
	locs := queryNameRE.FindAllStringSubmatchIndex(sqlText, -1)
	for i, loc := range locs {
		end := len(sqlText)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		body := sqlText[loc[1]:end]
		body = sqlLineComment.ReplaceAllString(body, "")
		out[sqlText[loc[2]:loc[3]]] = strings.Join(strings.Fields(body), " ")
	}
	return out
}

// visibilityViolations classifies every statement of sqlText against table and returns one
// description per violation, sorted.
func visibilityViolations(sqlText string, table map[string]visEntry) []string {
	var out []string
	stmts := statementsOf(sqlText)
	for name, stmt := range stmts {
		e, ok := table[name]
		if !ok {
			out = append(out, name+": unclassified; add it to visibilityTable with a visibility decision")
			continue
		}
		for _, m := range e.must {
			if !strings.Contains(stmt, m) {
				out = append(out, name+": lacks required text "+m)
			}
		}
		switch e.class {
		case classOwner:
			if len(e.must) == 0 && !strings.Contains(stmt, "owner_user_id = ") {
				out = append(out, name+": owner-only query lacks the owner arm `owner_user_id = `")
			}
			if credentialModeTestRE.MatchString(stmt) {
				out = append(out, name+": owner-only query tests credential_mode (it must not see shared rows)")
			}
		case classVIS:
			if !visRE.MatchString(stmt) {
				out = append(out, name+": recipient query lacks the exact visibility predicate (own OR shared per-user-key)")
			}
		case classShared:
			if e.caller == "" {
				out = append(out, name+": shared-admin query has no authorized caller recorded")
			}
			if len(e.must) == 0 && !sharedScopeRE.MatchString(stmt) {
				out = append(out, name+": shared-admin query lacks `owner_user_id IS NULL AND credential_mode = 'per_user_key'`")
			}
			if strings.Contains(stmt, "owner_user_id = ") {
				out = append(out, name+": shared-admin query has an owner arm")
			}
			if strings.Contains(stmt, "ciphertext") || strings.Contains(stmt, "sealed_with") {
				out = append(out, name+": shared-admin query touches sealed key material")
			}
		case classIdentity:
			if len(e.must) == 0 {
				out = append(out, name+": identity-only query has no pinned key predicate")
			}
		}
	}
	for name := range table {
		if _, ok := stmts[name]; !ok {
			out = append(out, name+": in visibilityTable but not in the query file (stale entry)")
		}
	}
	// The admin list and get never join keys or per-user evidence.
	for _, name := range []string{"ListSharedModelProvidersAdmin", "GetSharedModelProviderAdmin"} {
		if s, ok := stmts[name]; ok && (strings.Contains(s, "model_provider_key_test_results") ||
			strings.Contains(s, "k.id") || strings.Contains(s, "key_secret_id")) {
			out = append(out, name+": the admin read must not expose keys or evidence")
		}
	}
	sort.Strings(out)
	return out
}

func TestModelProviderQueriesAreVisibilityClassified(t *testing.T) {
	raw, err := os.ReadFile("queries/model_providers.sql")
	if err != nil {
		t.Fatal(err)
	}
	if got := visibilityViolations(string(raw), visibilityTable); len(got) != 0 {
		t.Fatalf("visibility classification violations:\n  %s", strings.Join(got, "\n  "))
	}
}

// TestModelProviderVisibilityPinFiresOnItsCanaries proves the scanner is not vacuous: an
// unclassified query, a recipient query that kept only the owner arm, an owner-only query that was
// widened, a shared-admin query that lost its scope and a stale table entry each fail.
func TestModelProviderVisibilityPinFiresOnItsCanaries(t *testing.T) {
	vis := "(p.owner_user_id = @u::uuid OR (p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key'))"
	cases := []struct {
		name  string
		sql   string
		table map[string]visEntry
		want  string
	}{
		{"unclassified", "-- name: Brand :one\nSELECT 1;", map[string]visEntry{}, "Brand: unclassified"},
		{"recipient owner arm only", "-- name: R :one\nSELECT 1 FROM model_providers p WHERE p.owner_user_id = @u;",
			map[string]visEntry{"R": {class: classVIS}}, "R: recipient query lacks the exact visibility predicate"},
		{"recipient with the predicate passes", "-- name: R :one\nSELECT 1 FROM model_providers p WHERE " + vis + ";",
			map[string]visEntry{"R": {class: classVIS}}, ""},
		{"owner-only widened", "-- name: O :one\nSELECT 1 FROM model_providers p WHERE p.owner_user_id = @u OR p.credential_mode = 'per_user_key';",
			map[string]visEntry{"O": {class: classOwner}}, "O: owner-only query tests credential_mode"},
		{"shared-admin lost scope", "-- name: S :one\nSELECT 1 FROM model_providers p WHERE p.id = @id;",
			map[string]visEntry{"S": {class: classShared, caller: "x"}}, "S: shared-admin query lacks"},
		{"shared-admin without caller", "-- name: S :one\nSELECT 1 FROM model_providers p WHERE p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key';",
			map[string]visEntry{"S": {class: classShared}}, "S: shared-admin query has no authorized caller"},
		{"stale entry", "-- name: A :one\nSELECT 1;", map[string]visEntry{"A": {class: classIdentity, must: []string{"1"}}, "Gone": {class: classOwner}}, "Gone: in visibilityTable but not in the query file"},
	}
	for _, c := range cases {
		got := strings.Join(visibilityViolations(c.sql, c.table), "\n")
		if c.want == "" {
			if got != "" {
				t.Errorf("%s: unexpected violations: %s", c.name, got)
			}
			continue
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: violations %q do not contain %q", c.name, got, c.want)
		}
	}
}
