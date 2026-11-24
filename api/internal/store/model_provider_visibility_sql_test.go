package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	// id is the one caller-id expression a recipient-read (classVIS) query may compare the owner
	// column to; any other expression (a self-comparison included) is a violation.
	id string
	// codexFrom, when set, marks where the statement's Codex part begins; that part is checked
	// separately: it must contain every codexMust substring and never the shared-scope text.
	codexFrom string
	codexMust []string
	// callers pins, for an identity-only query, the exact set of production files (relative to
	// api/internal) that call it: an identity query has no ownership predicate of its own, so its
	// authority is the caller's prior visibility check, and a new caller must be reviewed here.
	callers []string
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
	"GetProviderKeyForUpdate": {class: classVIS, id: "k.user_id"},
	"InsertProviderKeySecret": {class: classVIS, id: "@user_id::uuid"},
	"GetUserProviderSource": {class: classVIS, id: "u.id", codexFrom: " UNION ALL ",
		codexMust: []string{"p.owner_user_id = u.id", "@engine::text = 'codex'"}},
	"SetUserProviderSource": {class: classVIS, id: "u.id", codexFrom: "(@engine::text = 'codex' AND EXISTS",
		codexMust: []string{"p.owner_user_id = u.id", "p.engine = 'codex'"}},
	"LockModelProviderForShare":                    {class: classVIS, id: "@owner_user_id::uuid"},
	"GetModelProviderRevisionForOwner":             {class: classVIS, id: "@owner_user_id::uuid"},
	"LockProviderBindingRevisionForKeyShareNowait": {class: classVIS, id: "@owner_user_id::uuid"},
	"ListRunProviderNames":                         {class: classVIS, id: "r.user_id"},
	"ListVisibleProviders":                         {class: classVIS, id: "@user_id::uuid"},
	"GetVisibleProviderForUse":                     {class: classVIS, id: "@user_id::uuid"},
	"LockVisibleProviderForKeyMutation":            {class: classVIS, id: "@user_id::uuid"},
	"LockVisibleProviderForEvidence":               {class: classVIS, id: "@user_id::uuid"},

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
	"InsertModelProviderRevision": {class: classIdentity, must: []string{"WHERE p.id = @provider_id"},
		callers: []string{"handler/providers.go", "handler/providers_admin.go"}},
	"GetModelProviderRevision": {class: classIdentity, must: []string{"WHERE provider_id = $1 AND revision = $2"},
		callers: []string{"handler/providers.go", "handler/providers_admin.go"}},
	"GetProviderBindingKeyByID": {class: classIdentity, must: []string{"WHERE id = $1 AND user_id = $2"},
		callers: []string{"workersvc/provider_claim.go", "workersvc/provider_source.go"}},
	"LockProviderBindingKeyForShareNowait": {class: classIdentity, must: []string{"WHERE id = $1 AND user_id = $2"},
		callers: []string{"workersvc/provider_claim.go"}},
	// The evidence key lock runs only after LockVisibleProviderForEvidence in the same transaction
	// (TestIdentityQueryCallersArePinned checks that order in the caller).
	"LockProviderKeyForEvidence": {class: classIdentity, must: []string{"k.id = @key_id", "k.provider_id =", "k.user_id =", "k.enablement_rev =", "k.disabled_at IS NULL"},
		callers: []string{"handler/providers_probe.go"}},
	"UpsertProviderKeyTestResult": {class: classIdentity, must: []string{"ON CONFLICT (provider_id, user_id)"},
		callers: []string{"handler/providers_probe.go"}},
	"DeleteProviderKeySecret": {class: classIdentity, must: []string{"WHERE user_id = $1 AND provider_id = $2"},
		callers: []string{"handler/providers.go"}},
	"GetUserProviderLaneSources": {class: classIdentity, must: []string{"WHERE id = $1"},
		callers: []string{"workersvc/harness_resolver.go"}},
}

var (
	queryNameRE = regexp.MustCompile(`(?m)^-- name: (\w+) :\w+`)
	// The exact visibility predicate with the caller's id expression as the one free part.
	visRE = regexp.MustCompile(`\(p\.owner_user_id = ([^\s()]+) OR \(p\.owner_user_id IS NULL AND p\.credential_mode = 'per_user_key'\)\)`)
	// A predicate on the credential mode (a RETURNING column list is not one).
	// An OR attached to the owner column, either side.
	ownerOrRE            = regexp.MustCompile(`owner_user_id\s*=\s*\S+\s+OR\b|\bOR\s+(?:\w+\.)?owner_user_id\b`)
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
			if strings.Contains(stmt, "owner_user_id IS NULL") {
				out = append(out, name+": owner-only query matches owner_user_id IS NULL (it must not see shared rows)")
			}
			if ownerOrRE.MatchString(stmt) {
				out = append(out, name+": owner-only query attaches an OR to the owner arm")
			}
			if len(e.must) == 0 && !strings.Contains(stmt, "owner_user_id = ") {
				out = append(out, name+": owner-only query lacks the owner arm `owner_user_id = `")
			}
			if credentialModeTestRE.MatchString(stmt) {
				out = append(out, name+": owner-only query tests credential_mode (it must not see shared rows)")
			}
		case classVIS:
			ms := visRE.FindAllStringSubmatch(stmt, -1)
			if len(ms) == 0 {
				out = append(out, name+": recipient query lacks the exact visibility predicate (own OR shared per-user-key)")
			}
			for _, m := range ms {
				if m[1] != e.id {
					out = append(out, name+": recipient predicate compares the owner to "+m[1]+", want "+e.id)
				}
			}
			if e.codexFrom != "" {
				i := strings.Index(stmt, e.codexFrom)
				if i < 0 {
					out = append(out, name+": codex part marker "+e.codexFrom+" not found")
					break
				}
				part := stmt[i:]
				for _, m := range e.codexMust {
					if !strings.Contains(part, m) {
						out = append(out, name+": codex part lacks "+m)
					}
				}
				if ownerOrRE.MatchString(part) {
					out = append(out, name+": codex part attaches an OR to the owner arm")
				}
				if strings.Contains(part, "credential_mode") || strings.Contains(part, "owner_user_id IS NULL") {
					out = append(out, name+": codex part must stay owner-only (no credential_mode, no owner_user_id IS NULL)")
				}
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
			if len(e.callers) == 0 {
				out = append(out, name+": identity-only query has no authorized caller recorded")
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
			map[string]visEntry{"R": {class: classVIS, id: "@u::uuid"}}, "R: recipient query lacks the exact visibility predicate"},
		{"recipient with the predicate passes", "-- name: R :one\nSELECT 1 FROM model_providers p WHERE " + vis + ";",
			map[string]visEntry{"R": {class: classVIS, id: "@u::uuid"}}, ""},
		{"owner-only widened", "-- name: O :one\nSELECT 1 FROM model_providers p WHERE p.owner_user_id = @u OR p.credential_mode = 'per_user_key';",
			map[string]visEntry{"O": {class: classOwner}}, "O: owner-only query tests credential_mode"},
		{"owner-only widened with IS NULL", "-- name: O :one\nUPDATE model_providers SET name = @n WHERE id = @id AND (owner_user_id = @owner_user_id::uuid OR owner_user_id IS NULL);",
			map[string]visEntry{"O": {class: classOwner}}, "O: owner-only query matches owner_user_id IS NULL"},
		{"RenameModelProvider widened with OR IS NULL", "-- name: RenameModelProvider :one\nUPDATE model_providers SET name = @name WHERE id = @id AND owner_user_id = @owner_user_id::uuid OR owner_user_id IS NULL;",
			map[string]visEntry{"RenameModelProvider": {class: classOwner}}, "RenameModelProvider: owner-only query attaches an OR to the owner arm"},
		{"recipient self-comparison", "-- name: R :one\nSELECT 1 FROM model_providers p WHERE (p.owner_user_id = p.owner_user_id OR (p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key'));",
			map[string]visEntry{"R": {class: classVIS, id: "@u::uuid"}}, "R: recipient predicate compares the owner to p.owner_user_id"},
		{"GetUserProviderSource codex arm widened",
			"-- name: G :one\nSELECT 1 FROM users u LEFT JOIN model_providers p ON (p.owner_user_id = u.id OR (p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key')) WHERE @engine::text = 'claude' UNION ALL SELECT 1 FROM users u LEFT JOIN model_providers p ON p.owner_user_id = u.id OR p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key' WHERE @engine::text = 'codex';",
			map[string]visEntry{"G": {class: classVIS, id: "u.id", codexFrom: " UNION ALL ", codexMust: []string{"p.owner_user_id = u.id", "@engine::text = 'codex'"}}}, "G: codex part must stay owner-only"},
		{"SetUserProviderSource codex arm shared",
			"-- name: S :one\nUPDATE users u SET x = 1 WHERE (@engine::text = 'claude' AND EXISTS (SELECT 1 FROM model_providers p WHERE (p.owner_user_id = u.id OR (p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key')) AND p.engine = 'claude')) OR (@engine::text = 'codex' AND EXISTS (SELECT 1 FROM model_providers p WHERE p.credential_mode = 'per_user_key' AND p.engine = 'codex'));",
			map[string]visEntry{"S": {class: classVIS, id: "u.id", codexFrom: "(@engine::text = 'codex' AND EXISTS", codexMust: []string{"p.owner_user_id = u.id", "p.engine = 'codex'"}}}, "S: codex part lacks p.owner_user_id = u.id"},
		{"GetUserProviderSource codex arm ORs the engine",
			"-- name: G :one\nSELECT 1 FROM users u LEFT JOIN model_providers p ON (p.owner_user_id = u.id OR (p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key')) WHERE @engine::text = 'claude' UNION ALL SELECT 1 FROM users u LEFT JOIN model_providers p ON (p.owner_user_id = u.id OR p.engine = @engine) WHERE @engine::text = 'codex';",
			map[string]visEntry{"G": {class: classVIS, id: "u.id", codexFrom: " UNION ALL ", codexMust: []string{"p.owner_user_id = u.id", "@engine::text = 'codex'"}}}, "G: codex part attaches an OR to the owner arm"},
		{"SetUserProviderSource codex arm ORs TRUE",
			"-- name: S :one\nUPDATE users u SET x = 1 WHERE (@engine::text = 'claude' AND EXISTS (SELECT 1 FROM model_providers p WHERE (p.owner_user_id = u.id OR (p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key')) AND p.engine = 'claude')) OR (@engine::text = 'codex' AND EXISTS (SELECT 1 FROM model_providers p WHERE (p.owner_user_id = u.id OR TRUE) AND p.engine = 'codex'));",
			map[string]visEntry{"S": {class: classVIS, id: "u.id", codexFrom: "(@engine::text = 'codex' AND EXISTS", codexMust: []string{"p.owner_user_id = u.id", "p.engine = 'codex'"}}}, "S: codex part attaches an OR to the owner arm"},
		{"shared-admin lost scope", "-- name: S :one\nSELECT 1 FROM model_providers p WHERE p.id = @id;",
			map[string]visEntry{"S": {class: classShared, caller: "x"}}, "S: shared-admin query lacks"},
		{"shared-admin without caller", "-- name: S :one\nSELECT 1 FROM model_providers p WHERE p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key';",
			map[string]visEntry{"S": {class: classShared}}, "S: shared-admin query has no authorized caller"},
		{"identity-only without caller", "-- name: I :one\nSELECT 1 FROM user_secrets WHERE id = $1;",
			map[string]visEntry{"I": {class: classIdentity, must: []string{"WHERE id = $1"}}}, "I: identity-only query has no authorized caller recorded"},
		{"stale entry", "-- name: A :one\nSELECT 1;", map[string]visEntry{"A": {class: classIdentity, must: []string{"1"}, callers: []string{"x.go"}}, "Gone": {class: classOwner}}, "Gone: in visibilityTable but not in the query file"},
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

// TestIdentityQueryCallersArePinned: every identity-only query is called from exactly the
// production files its entry records, and the evidence key lock is preceded by the provider
// evidence fence and followed by the upsert in its one caller.
func TestIdentityQueryCallersArePinned(t *testing.T) {
	// Every production .go file under api/internal and api/cmd (test files, testdata and the
	// generated store files excluded), keyed by its path relative to api/internal.
	files := map[string]string{}
	for _, root := range []string{"..", filepath.Join("..", "..", "cmd")} {
		err := filepath.WalkDir(root, func(path string, de os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if de.IsDir() {
				if de.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			n := de.Name()
			if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") || strings.HasSuffix(n, ".sql.go") {
				return nil
			}
			b, err := os.ReadFile(path) //nolint:gosec // G304: a path WalkDir found under this module's own source tree, read-only.
			if err != nil {
				return err
			}
			rel, err := filepath.Rel("..", path)
			if err != nil {
				return err
			}
			files[filepath.ToSlash(rel)] = string(b)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for name, e := range visibilityTable {
		if e.class != classIdentity {
			continue
		}
		var got []string
		for path, src := range files {
			if strings.Contains(src, "."+name+"(") {
				got = append(got, path)
			}
		}
		sort.Strings(got)
		want := append([]string(nil), e.callers...)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: callers = %v, recorded %v (review the new caller's prior visibility check, then record it)", name, got, want)
		}
	}
	checkProviderEvidenceCalls(t, files["handler/providers_probe.go"], true)
}

// Read executable selectors in recordProviderProbeResult. Comments and calls
// outside this function cannot satisfy the order check.
func checkProviderEvidenceCalls(t *testing.T, source string, wantOrdered bool) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "providers_probe.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"LockVisibleProviderForEvidence", "LockProviderKeyForEvidence", "UpsertProviderKeyTestResult"}
	var calls []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "recordProviderProbeResult" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			method, ok := call.Fun.(*ast.SelectorExpr)
			if ok && slices.Contains(want, method.Sel.Name) {
				if id, ok := method.X.(*ast.Ident); !ok || id.Name != "q" {
					t.Errorf("evidence call %s must use the transaction queries q", method.Sel.Name)
				}
				calls = append(calls, method.Sel.Name)
			}
			return true
		})
	}
	if ordered := slices.Equal(calls, want); ordered != wantOrdered {
		t.Fatalf("evidence calls = %v; want ordered=%v for %v", calls, wantOrdered, want)
	}
}

func TestProviderEvidenceLockOrderIgnoresComments(t *testing.T) {
	checkProviderEvidenceCalls(t, `package handler
func recordProviderProbeResult() {
	// q.LockVisibleProviderForEvidence()
	q.LockProviderKeyForEvidence()
	q.UpsertProviderKeyTestResult()
	q.LockVisibleProviderForEvidence()
}`, false)
}
