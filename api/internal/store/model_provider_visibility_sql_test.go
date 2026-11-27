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
	// classOperatorCustody is an organization key's custody or Test evidence statement (PRD #4540): it
	// names model_provider_operator_keys or its evidence table, or reads an organization-funded
	// definition. It has its own text rules, and only the three statements flagged ciphertext may name
	// the sealed key.
	classOperatorCustody
)

type visEntry struct {
	class visClass
	// must replaces the class's default text rule when the statement is not a plain WHERE (an INSERT
	// or an advisory lock): every substring must appear in the normalized, comment-free statement.
	must []string
	// caller records the authorized caller of a shared-admin query: reviewed here. The files that
	// actually call it are computed and pinned by callers (TestIdentityQueryCallersArePinned).
	caller string
	// exact is the statement's full normalized, comment-free text (statementsOf). Every classVIS,
	// classShared and classOperatorCustody entry carries it (exactSQL): a fragment rule cannot see a
	// widening that keeps the fragment, so any edit to the statement must be an edit here too.
	exact string
	// ciphertext marks the only statements that may name the sealed organization key: its insert, its
	// replace and the Test sink.
	ciphertext bool
	// id is the one caller-id expression a recipient-read (classVIS) query may compare the owner
	// column to; any other expression (a self-comparison included) is a violation.
	id string
	// codexFrom, when set, marks where the statement's Codex part begins. Each engine part (before and
	// from the marker) is checked separately: it must carry the exact owner-or-shared predicate with
	// the caller's id, every claudeMust / codexMust substring (its engine guard), and no OR attached
	// to the owner arm beyond that predicate.
	codexFrom  string
	claudeMust []string
	codexMust  []string
	// callers pins, for an identity-only, shared-admin or organization-key custody query, the exact set
	// of production files (relative to api/internal) that call it; an empty set means no production
	// caller. An identity query has no ownership predicate of its own, so its authority is the caller's
	// prior visibility check; a shared-admin or custody statement's authority is the admin route. Either
	// way a new caller must be reviewed here.
	callers []string
}

const (
	nilScopeSQL = `'00000000-0000-0000-0000-000000000000'::uuid`
	adminCreate = "admin create (cookie + CSRF)"
	adminEdit   = "admin edit (cookie + CSRF)"
	adminDelete = "admin delete (cookie + CSRF)"
	adminRead   = "admin list/get (cookie, or the admin-read token)"
	adminKey    = "admin key write or Test (cookie + CSRF)"

	// sharedBothModes is the credential-mode predicate of every admin statement: a shared definition is
	// per-user-key or organization-funded, and the admin catalogue, cap and edits cover both.
	sharedBothModes = "credential_mode IN ('per_user_key', 'operator_key')"
	// custodyKeyRevNextval is the only value a custody row's key_rev is ever assigned.
	custodyKeyRevNextval = "key_rev = nextval('model_provider_operator_key_rev_seq')"
	// operatorScopeSQL is the scope of every statement that reads an organization-funded definition.
	operatorScopeSQL = "p.owner_user_id IS NULL AND p.credential_mode = 'operator_key'"
)

// bothSelectionArms pins that a shared definition's selection count covers both engines' nil-scope
// selections; dropping either arm would let a delete pass over a selected definition's users.
var bothSelectionArms = []string{
	"claude_provider_scope_id = " + nilScopeSQL,
	"codex_provider_scope_id = " + nilScopeSQL,
}

// adminCountMust is bothSelectionArms plus the shared scope the admin reads select on, in both modes.
var adminCountMust = append([]string{"p.owner_user_id IS NULL AND p." + sharedBothModes}, bothSelectionArms...)

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

	// recipient reads and the recipient write path: the exact visibility predicate. Their text is the
	// text at the base of PRD #4540 M1 (exactSQL): an organization-funded definition is invisible here.
	"GetProviderKeyForUpdate": {class: classVIS, id: "k.user_id", exact: exactSQL["GetProviderKeyForUpdate"]},
	"InsertProviderKeySecret": {class: classVIS, id: "@user_id::uuid", exact: exactSQL["InsertProviderKeySecret"]},
	"GetUserProviderSource": {class: classVIS, id: "u.id", codexFrom: " UNION ALL ", exact: exactSQL["GetUserProviderSource"],
		claudeMust: []string{"p.engine = @engine", "@engine::text = 'claude'"},
		codexMust:  []string{"p.engine = @engine", "@engine::text = 'codex'"}},
	"SetUserProviderSource": {class: classVIS, id: "u.id", codexFrom: "(@engine::text = 'codex' AND EXISTS", exact: exactSQL["SetUserProviderSource"],
		claudeMust: []string{"p.engine = 'claude'"},
		codexMust:  []string{"p.engine = 'codex'"}},
	"LockModelProviderForShare":                    {class: classVIS, id: "@owner_user_id::uuid", exact: exactSQL["LockModelProviderForShare"]},
	"GetModelProviderRevisionForOwner":             {class: classVIS, id: "@owner_user_id::uuid", exact: exactSQL["GetModelProviderRevisionForOwner"]},
	"LockProviderBindingRevisionForKeyShareNowait": {class: classVIS, id: "@owner_user_id::uuid", exact: exactSQL["LockProviderBindingRevisionForKeyShareNowait"]},
	"ListRunProviderNames":                         {class: classVIS, id: "r.user_id", exact: exactSQL["ListRunProviderNames"]},
	"ListVisibleProviders":                         {class: classVIS, id: "@user_id::uuid", exact: exactSQL["ListVisibleProviders"]},
	"GetVisibleProviderForUse":                     {class: classVIS, id: "@user_id::uuid", exact: exactSQL["GetVisibleProviderForUse"]},
	"LockVisibleProviderForKeyMutation":            {class: classVIS, id: "@user_id::uuid", exact: exactSQL["LockVisibleProviderForKeyMutation"]},
	"LockVisibleProviderForEvidence":               {class: classVIS, id: "@user_id::uuid", exact: exactSQL["LockVisibleProviderForEvidence"]},

	// shared-admin: NULL owner and either credential mode, with the authorized caller and the exact set
	// of production files that call it (all in the admin handler, which is where the route's cookie +
	// CSRF check lives; PRD #4540 M2 adds the key and Test files).
	"LockSharedModelProviderCatalogue": {class: classShared, caller: adminCreate, exact: exactSQL["LockSharedModelProviderCatalogue"],
		must: []string{"hashtext('00000000-0000-0000-0000-000000000000')"}, callers: []string{"handler/providers_admin.go"}},
	"CountSharedModelProviders": {class: classShared, caller: adminCreate, exact: exactSQL["CountSharedModelProviders"],
		callers: []string{"handler/providers_admin.go"}},
	"InsertSharedModelProvider": {class: classShared, caller: adminCreate, exact: exactSQL["InsertSharedModelProvider"], must: []string{
		"VALUES (NULL, @name, @engine, 1, 'per_user_key', " + nilScopeSQL + ")"}, callers: []string{"handler/providers_admin.go"}},
	// No production caller in M1: PRD #4540 M2 wires the create route.
	"InsertSharedOperatorModelProvider": {class: classShared, caller: adminCreate, exact: exactSQL["InsertSharedOperatorModelProvider"], must: []string{
		"VALUES (NULL, @name, 'claude', 1, 'operator_key', " + nilScopeSQL + ")"}},
	"LockSharedModelProviderForEdit":    {class: classShared, caller: adminEdit, exact: exactSQL["LockSharedModelProviderForEdit"], callers: []string{"handler/providers_admin.go"}},
	"BumpSharedModelProviderRevision":   {class: classShared, caller: adminEdit, exact: exactSQL["BumpSharedModelProviderRevision"], callers: []string{"handler/providers_admin.go"}},
	"RenameSharedModelProvider":         {class: classShared, caller: adminEdit, exact: exactSQL["RenameSharedModelProvider"], callers: []string{"handler/providers_admin.go"}},
	"RetireSharedModelProvider":         {class: classShared, caller: adminEdit, exact: exactSQL["RetireSharedModelProvider"], callers: []string{"handler/providers_admin.go"}},
	"PruneSharedModelProviderRevisions": {class: classShared, caller: adminEdit, exact: exactSQL["PruneSharedModelProviderRevisions"], callers: []string{"handler/providers_admin.go"}},
	"LockSharedModelProviderForDelete":  {class: classShared, caller: adminDelete, exact: exactSQL["LockSharedModelProviderForDelete"], callers: []string{"handler/providers_admin.go"}},
	"DeleteSharedModelProvider":         {class: classShared, caller: adminDelete, exact: exactSQL["DeleteSharedModelProvider"], callers: []string{"handler/providers_admin.go"}},
	"DeleteSharedProviderKeys": {class: classShared, caller: adminDelete, exact: exactSQL["DeleteSharedProviderKeys"],
		must: []string{"provider_scope_id = " + nilScopeSQL}, callers: []string{"handler/providers_admin.go"}},
	"CountSharedProviderSelections": {class: classShared, caller: adminDelete, exact: exactSQL["CountSharedProviderSelections"],
		must: bothSelectionArms, callers: []string{"handler/providers_admin.go"}},
	"ListSharedModelProvidersAdmin": {class: classShared, caller: adminRead, exact: exactSQL["ListSharedModelProvidersAdmin"],
		must: adminCountMust, callers: []string{"handler/providers_admin.go"}},
	"GetSharedModelProviderAdmin": {class: classShared, caller: adminRead, exact: exactSQL["GetSharedModelProviderAdmin"],
		must: adminCountMust, callers: []string{"handler/providers_admin.go"}},

	// organization key custody and the admin Test evidence (PRD #4540). None has a production caller in
	// M1; M2's key routes and Test probe set the callers below, and the key and probe files are the only
	// ones that may ever call them. Each must name the operator scope or its own key identity.
	"LockOperatorProviderKeyForWrite": {class: classOperatorCustody, caller: adminKey, exact: exactSQL["LockOperatorProviderKeyForWrite"],
		must: []string{"WHERE provider_id = @provider_id FOR UPDATE"}},
	"InsertOperatorProviderKey": {class: classOperatorCustody, caller: adminKey, exact: exactSQL["InsertOperatorProviderKey"], ciphertext: true,
		must: []string{"INSERT INTO model_provider_operator_keys (provider_id, ciphertext, set_by)"}},
	"ReplaceOperatorProviderKey": {class: classOperatorCustody, caller: adminKey, exact: exactSQL["ReplaceOperatorProviderKey"], ciphertext: true,
		must: []string{"WHERE provider_id = @provider_id"}},
	"DisableOperatorProviderKey": {class: classOperatorCustody, caller: adminKey, exact: exactSQL["DisableOperatorProviderKey"],
		must: []string{"WHERE provider_id = @provider_id AND disabled_at IS NULL"}},
	"EnableOperatorProviderKey": {class: classOperatorCustody, caller: adminKey, exact: exactSQL["EnableOperatorProviderKey"],
		must: []string{"WHERE provider_id = @provider_id AND disabled_at IS NOT NULL"}},
	"DeleteOperatorProviderKey": {class: classOperatorCustody, caller: adminKey, exact: exactSQL["DeleteOperatorProviderKey"],
		must: []string{"WHERE provider_id = @provider_id"}},
	"GetOperatorProviderKeyForTest": {class: classOperatorCustody, caller: adminKey, exact: exactSQL["GetOperatorProviderKeyForTest"], ciphertext: true,
		must: []string{"WHERE id = @id AND provider_id = @provider_id AND key_rev = @key_rev AND disabled_at IS NULL"}},
	"GetOperatorProviderForTest": {class: classOperatorCustody, caller: adminKey, exact: exactSQL["GetOperatorProviderForTest"],
		must: []string{operatorScopeSQL}},
	"LockOperatorProviderForEvidence": {class: classOperatorCustody, caller: adminKey, exact: exactSQL["LockOperatorProviderForEvidence"],
		must: []string{operatorScopeSQL, "p.retired_at IS NULL", "FOR SHARE OF p"}},
	"LockOperatorProviderKeyForEvidence": {class: classOperatorCustody, caller: adminKey, exact: exactSQL["LockOperatorProviderKeyForEvidence"],
		must: []string{"k.id = @key_id", "k.provider_id = @provider_id", "k.key_rev = @key_rev", "k.disabled_at IS NULL", "FOR SHARE"}},
	"UpsertOperatorProviderKeyTestResult": {class: classOperatorCustody, caller: adminKey, exact: exactSQL["UpsertOperatorProviderKeyTestResult"],
		must: []string{"ON CONFLICT (provider_id)"}},

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
	"GetUserJudgeLanePolicy": {class: classIdentity, must: []string{"WHERE id = $1"},
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
	// A shared-admin scope: NULL owner and either the per-user-key mode alone (the creates of that mode)
	// or both modes (every admin statement), with or without the alias. The statements without an alias
	// (the count, bump, rename, retire and delete) pin their own predicate by exact text.
	sharedScopeRE = regexp.MustCompile(`owner_user_id IS NULL AND (?:p\.)?credential_mode (?:= 'per_user_key'|IN \('per_user_key', 'operator_key'\))`)
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
		if e.exact != "" && stmt != e.exact {
			out = append(out, name+": statement text differs from its pinned exact text (a visibility, scope or custody edit must be reviewed in exactSQL)")
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
					out = append(out, name+": engine part marker "+e.codexFrom+" not found")
					break
				}
				for _, arm := range []struct {
					label, part string
					must        []string
				}{{"claude", stmt[:i], e.claudeMust}, {"codex", stmt[i:], e.codexMust}} {
					ams := visRE.FindAllStringSubmatch(arm.part, -1)
					if len(ams) == 0 {
						out = append(out, name+": "+arm.label+" part lacks the exact owner-or-shared predicate")
					}
					for _, m := range ams {
						if m[1] != e.id {
							out = append(out, name+": "+arm.label+" part compares the owner to "+m[1]+", want "+e.id)
						}
					}
					for _, m := range arm.must {
						if !strings.Contains(arm.part, m) {
							out = append(out, name+": "+arm.label+" part lacks "+m)
						}
					}
					if ownerOrRE.MatchString(visRE.ReplaceAllString(arm.part, "")) {
						out = append(out, name+": "+arm.label+" part attaches an OR to the owner arm")
					}
				}
			}
		case classShared:
			if e.caller == "" {
				out = append(out, name+": shared-admin query has no authorized caller recorded")
			}
			if len(e.must) == 0 && !sharedScopeRE.MatchString(stmt) {
				out = append(out, name+": shared-admin query lacks `owner_user_id IS NULL AND credential_mode IN ('per_user_key', 'operator_key')` (or the per-user-key literal)")
			}
			if strings.Contains(stmt, "owner_user_id = ") {
				out = append(out, name+": shared-admin query has an owner arm")
			}
			if strings.Contains(stmt, "ciphertext") || strings.Contains(stmt, "sealed_with") {
				out = append(out, name+": shared-admin query touches sealed key material")
			}
		case classOperatorCustody:
			out = append(out, custodyViolations(name, stmt, e)...)
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
	// The admin list and get never join per-user keys or evidence, and project no custody internals.
	for _, name := range []string{"ListSharedModelProvidersAdmin", "GetSharedModelProviderAdmin"} {
		s, ok := stmts[name]
		if !ok {
			continue
		}
		if strings.Contains(s, "model_provider_key_test_results") || strings.Contains(s, "k.id") || strings.Contains(s, "key_secret_id") {
			out = append(out, name+": the admin read must not expose keys or evidence")
		}
		if bad := adminProjectionLeaks(s); len(bad) != 0 {
			out = append(out, name+": the admin read projects custody internals: "+strings.Join(bad, ", "))
		}
	}
	sort.Strings(out)
	return out
}

// custodyViolations are the text rules of an organization-key custody or evidence statement.
func custodyViolations(name, stmt string, e visEntry) []string {
	var out []string
	if e.caller == "" {
		out = append(out, name+": custody statement has no authorized caller recorded")
	}
	if len(e.must) == 0 {
		out = append(out, name+": custody statement has no pinned predicate")
	}
	if !e.ciphertext && (strings.Contains(stmt, "ciphertext") || strings.Contains(stmt, "sealed_with")) {
		out = append(out, name+": custody statement names the sealed key (only the insert, the replace and the Test sink may)")
	}
	if strings.Contains(stmt, "model_providers ") && !strings.Contains(stmt, operatorScopeSQL) {
		out = append(out, name+": custody statement reads a definition without the operator scope `"+operatorScopeSQL+"`")
	}
	if strings.HasPrefix(stmt, "UPDATE ") && !strings.Contains(stmt, custodyKeyRevNextval) {
		out = append(out, name+": custody UPDATE does not assign `"+custodyKeyRevNextval+"`")
	}
	return out
}

// adminProjectionLeaks returns the custody internals an admin read's SELECT list projects: the key
// revision, the key's row id, the setter, any ciphertext, or a whole-row expansion. The joins may name
// them (the evidence join compares key id and revision); the projection may not.
func adminProjectionLeaks(stmt string) []string {
	proj := stmt
	if i := strings.Index(proj, " FROM "); i >= 0 {
		proj = proj[:i]
	}
	var out []string
	for _, bad := range []string{"cust.key_rev", "cust.id", "cust.set_by", "cust.ciphertext", "cev.key_id", "cev.key_rev", "cev.probed_by", "ciphertext", "to_jsonb(", "row_to_json(", "json_build_object("} {
		if strings.Contains(proj, bad) {
			out = append(out, bad)
		}
	}
	if strings.Contains(proj, ".*") {
		out = append(out, ".*")
	}
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
// adminCountCodexArmMissingSQL is an admin count in the two-mode form with the Claude selection arm
// only: it must raise exactly the missing Codex arm and nothing else, so the canary keeps isolating the
// arm after the single-mode literal was replaced (TestAdminCountMissingArmRaisesOnlyThatArm).
const adminCountCodexArmMissingSQL = "-- name: C :one\nSELECT 1 FROM model_providers p WHERE p.owner_user_id IS NULL AND p.credential_mode IN ('per_user_key', 'operator_key') AND claude_provider_scope_id = " + nilScopeSQL + ";"

func TestAdminCountMissingArmRaisesOnlyThatArm(t *testing.T) {
	table := map[string]visEntry{"C": {class: classShared, caller: "x", must: adminCountMust}}
	got := visibilityViolations(adminCountCodexArmMissingSQL, table)
	if len(got) != 1 || !strings.Contains(got[0], "C: lacks required text codex_provider_scope_id") {
		t.Fatalf("violations = %q, want exactly the missing codex arm", got)
	}
	// The control with both arms is clean.
	both := "-- name: C :one\nSELECT 1 FROM model_providers p WHERE p.owner_user_id IS NULL AND p.credential_mode IN ('per_user_key', 'operator_key') AND claude_provider_scope_id = " + nilScopeSQL + " AND codex_provider_scope_id = " + nilScopeSQL + ";"
	if got := visibilityViolations(both, table); len(got) != 0 {
		t.Fatalf("both arms: %q, want none", got)
	}
}

func TestModelProviderVisibilityPinFiresOnItsCanaries(t *testing.T) {
	vis := "(p.owner_user_id = @u::uuid OR (p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key'))"
	vis2 := "(p.owner_user_id = u.id OR (p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key'))"
	getEntry := map[string]visEntry{"G": {class: classVIS, id: "u.id", codexFrom: " UNION ALL ",
		claudeMust: []string{"p.engine = @engine", "@engine::text = 'claude'"}, codexMust: []string{"p.engine = @engine", "@engine::text = 'codex'"}}}
	setEntry := map[string]visEntry{"S": {class: classVIS, id: "u.id", codexFrom: "(@engine::text = 'codex' AND EXISTS",
		claudeMust: []string{"p.engine = 'claude'"}, codexMust: []string{"p.engine = 'codex'"}}}
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
		{"GetUserProviderSource codex arm missing the shared arm",
			"-- name: G :one\nSELECT 1 FROM users u LEFT JOIN model_providers p ON " + vis2 + " AND p.engine = @engine WHERE @engine::text = 'claude' UNION ALL SELECT 1 FROM users u LEFT JOIN model_providers p ON p.owner_user_id = u.id AND p.engine = @engine WHERE @engine::text = 'codex';",
			getEntry, "G: codex part lacks the exact owner-or-shared predicate"},
		{"GetUserProviderSource codex arm missing the engine guard",
			"-- name: G :one\nSELECT 1 FROM users u LEFT JOIN model_providers p ON " + vis2 + " AND p.engine = @engine WHERE @engine::text = 'claude' UNION ALL SELECT 1 FROM users u LEFT JOIN model_providers p ON " + vis2 + " WHERE @engine::text = 'codex';",
			getEntry, "G: codex part lacks p.engine = @engine"},
		{"GetUserProviderSource codex arm with an OR attached to the owner arm",
			"-- name: G :one\nSELECT 1 FROM users u LEFT JOIN model_providers p ON " + vis2 + " AND p.engine = @engine WHERE @engine::text = 'claude' UNION ALL SELECT 1 FROM users u LEFT JOIN model_providers p ON (p.owner_user_id = u.id OR TRUE) AND p.engine = @engine WHERE @engine::text = 'codex';",
			getEntry, "G: codex part attaches an OR to the owner arm"},
		{"GetUserProviderSource passes with both arms and guards",
			"-- name: G :one\nSELECT 1 FROM users u LEFT JOIN model_providers p ON " + vis2 + " AND p.engine = @engine WHERE @engine::text = 'claude' UNION ALL SELECT 1 FROM users u LEFT JOIN model_providers p ON " + vis2 + " AND p.engine = @engine WHERE @engine::text = 'codex';",
			getEntry, ""},
		{"SetUserProviderSource codex arm missing the shared arm",
			"-- name: S :one\nUPDATE users u SET x = 1 WHERE (@engine::text = 'claude' AND EXISTS (SELECT 1 FROM model_providers p WHERE " + vis2 + " AND p.engine = 'claude')) OR (@engine::text = 'codex' AND EXISTS (SELECT 1 FROM model_providers p WHERE p.owner_user_id = u.id AND p.engine = 'codex'));",
			setEntry, "S: codex part lacks the exact owner-or-shared predicate"},
		{"SetUserProviderSource codex arm missing the engine guard",
			"-- name: S :one\nUPDATE users u SET x = 1 WHERE (@engine::text = 'claude' AND EXISTS (SELECT 1 FROM model_providers p WHERE " + vis2 + " AND p.engine = 'claude')) OR (@engine::text = 'codex' AND EXISTS (SELECT 1 FROM model_providers p WHERE " + vis2 + "));",
			setEntry, "S: codex part lacks p.engine = 'codex'"},
		{"SetUserProviderSource codex arm ORs TRUE",
			"-- name: S :one\nUPDATE users u SET x = 1 WHERE (@engine::text = 'claude' AND EXISTS (SELECT 1 FROM model_providers p WHERE " + vis2 + " AND p.engine = 'claude')) OR (@engine::text = 'codex' AND EXISTS (SELECT 1 FROM model_providers p WHERE (p.owner_user_id = u.id OR TRUE) AND p.engine = 'codex'));",
			setEntry, "S: codex part attaches an OR to the owner arm"},
		{"SetUserProviderSource claude arm missing the engine guard",
			"-- name: S :one\nUPDATE users u SET x = 1 WHERE (@engine::text = 'claude' AND EXISTS (SELECT 1 FROM model_providers p WHERE " + vis2 + ")) OR (@engine::text = 'codex' AND EXISTS (SELECT 1 FROM model_providers p WHERE " + vis2 + " AND p.engine = 'codex'));",
			setEntry, "S: claude part lacks p.engine = 'claude'"},
		{"SetUserProviderSource passes with both arms and guards",
			"-- name: S :one\nUPDATE users u SET x = 1 WHERE (@engine::text = 'claude' AND EXISTS (SELECT 1 FROM model_providers p WHERE " + vis2 + " AND p.engine = 'claude')) OR (@engine::text = 'codex' AND EXISTS (SELECT 1 FROM model_providers p WHERE " + vis2 + " AND p.engine = 'codex'));",
			setEntry, ""},
		{"selection count missing the codex arm",
			"-- name: C :one\nSELECT count(*) FROM users WHERE claude_provider_id = @id AND claude_provider_scope_id = " + nilScopeSQL + ";",
			map[string]visEntry{"C": {class: classShared, caller: "x", must: bothSelectionArms}}, "C: lacks required text codex_provider_scope_id"},
		{"selection count missing the claude arm",
			"-- name: C :one\nSELECT count(*) FROM users WHERE codex_provider_id = @id AND codex_provider_scope_id = " + nilScopeSQL + ";",
			map[string]visEntry{"C": {class: classShared, caller: "x", must: bothSelectionArms}}, "C: lacks required text claude_provider_scope_id"},
		{"admin count missing the codex arm",
			adminCountCodexArmMissingSQL,
			map[string]visEntry{"C": {class: classShared, caller: "x", must: adminCountMust}}, "C: lacks required text codex_provider_scope_id"},
		{"InsertSharedOperatorModelProvider with a Codex engine",
			"-- name: I :one\nINSERT INTO model_providers VALUES (NULL, @name, 'codex', 1, 'operator_key', " + nilScopeSQL + ");",
			map[string]visEntry{"I": {class: classShared, caller: "x", must: []string{"VALUES (NULL, @name, 'claude', 1, 'operator_key', " + nilScopeSQL + ")"}}}, "I: lacks required text VALUES (NULL, @name, 'claude'"},
		{"InsertSharedModelProvider hardcoding the engine",
			"-- name: I :one\nINSERT INTO model_providers VALUES (NULL, @name, 'claude', 1, 'per_user_key', " + nilScopeSQL + ");",
			map[string]visEntry{"I": {class: classShared, caller: "x", must: []string{"VALUES (NULL, @name, @engine, 1, 'per_user_key', " + nilScopeSQL + ")"}}}, "I: lacks required text VALUES (NULL, @name, @engine"},
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

// TestIdentityQueryCallersArePinned: every identity-only, shared-admin and organization-key custody
// query is called from exactly the production files its entry records (computed, not reviewed by eye),
// operatorkey.Seal and Open from exactly their recorded files, and the evidence key lock is preceded by
// the provider evidence fence and followed by the upsert in its one caller.
func TestIdentityQueryCallersArePinned(t *testing.T) {
	files := productionGoFiles(t)
	for name, e := range visibilityTable {
		// Identity-only, shared-admin and custody statements carry no ownership predicate of the
		// caller's own (or, for the shared and custody ones, one only the admin route makes safe), so
		// the exact set of production files that call each is pinned; an empty set means no caller.
		if e.class != classIdentity && e.class != classShared && e.class != classOperatorCustody {
			continue
		}
		got := callersOf(files, "."+name+"(")
		want := append([]string(nil), e.callers...)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: callers = %v, recorded %v (review the new caller's authorization, then record it)", name, got, want)
		}
	}
	// operatorkey.Seal and Open are the only way the sealed organization key is made or read; M1 has no
	// production caller (the key routes and the Test probe, in M2, are the only files that may).
	for _, fn := range []string{"Seal", "Open"} {
		if got := callersOf(files, "operatorkey."+fn+"("); len(got) != len(operatorKeySealOpenCallers) ||
			(len(got) > 0 && strings.Join(got, ",") != strings.Join(operatorKeySealOpenCallers, ",")) {
			t.Errorf("operatorkey.%s callers = %v, recorded %v", fn, got, operatorKeySealOpenCallers)
		}
	}
	checkProviderEvidenceCalls(t, files["handler/providers_probe.go"], true)
}

// operatorKeySealOpenCallers is the exact set of production files that call operatorkey.Seal or Open.
var operatorKeySealOpenCallers = []string{}

func callersOf(files map[string]string, needle string) []string {
	var got []string
	for path, src := range files {
		if strings.Contains(src, needle) {
			got = append(got, path)
		}
	}
	sort.Strings(got)
	return got
}

// isProductionGoFile reports whether a Go file name is a production source: not a test, and not a
// sqlc-generated query file (whose text embeds every statement).
func isProductionGoFile(name string) bool {
	return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") && !strings.HasSuffix(name, ".sql.go")
}

// productionGoFiles reads every production .go file under api/internal and api/cmd (test files,
// testdata and the generated store files excluded), keyed by its path relative to api/internal.
func productionGoFiles(t *testing.T) map[string]string {
	t.Helper()
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
			if !isProductionGoFile(de.Name()) {
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
	return files
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

// TestPinnedClassesCarryExactText: a recipient, shared-admin or custody entry without its exact text
// would only be held to the fragment rules, which a widening that keeps the fragment passes.
func TestPinnedClassesCarryExactText(t *testing.T) {
	stmts := statementsOfFile(t)
	for name, e := range visibilityTable {
		switch e.class {
		case classVIS, classShared, classOperatorCustody:
			if e.exact == "" {
				t.Errorf("%s: no exact text pinned", name)
			} else if e.exact != stmts[name] {
				t.Errorf("%s: pinned exact text differs from queries/model_providers.sql", name)
			}
		}
	}
	for name := range exactSQL {
		if _, ok := visibilityTable[name]; !ok {
			t.Errorf("exactSQL[%s] has no visibilityTable entry", name)
		}
	}
}

func statementsOfFile(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile("queries/model_providers.sql")
	if err != nil {
		t.Fatal(err)
	}
	return statementsOf(string(raw))
}

// fileWith rebuilds the query file from its normalized statements with one statement edited, so a
// canary mutates the real text. The edit must change the statement.
func fileWith(t *testing.T, name string, edit func(string) string) string {
	t.Helper()
	stmts := statementsOfFile(t)
	names := make([]string, 0, len(stmts))
	for n := range stmts {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		text := stmts[n]
		if n == name {
			mutated := edit(text)
			if mutated == text {
				t.Fatalf("canary edit left %s unchanged", name)
			}
			text = mutated
		}
		b.WriteString("-- name: " + n + " :one\n" + text + "\n\n")
	}
	if _, ok := stmts[name]; !ok {
		t.Fatalf("canary names unknown statement %s", name)
	}
	return b.String()
}

// TestOperatorCustodyPinFiresOnItsCanaries mutates the REAL statements, one defect each, and requires
// the classifier to name it. The control (the unmutated file) is clean in
// TestModelProviderQueriesAreVisibilityClassified, so a pass here cannot mean the scan matched nothing.
func TestOperatorCustodyPinFiresOnItsCanaries(t *testing.T) {
	const exact = "statement text differs from its pinned exact text"
	repl := func(old, new string) func(string) string {
		return func(s string) string { return strings.Replace(s, old, new, 1) }
	}
	cases := []struct {
		name, stmt string
		edit       func(string) string
		want       string
	}{
		{"LockModelProviderForShare ORs true", "LockModelProviderForShare", repl(" FOR SHARE OF p", " OR true FOR SHARE OF p"), exact},
		{"ListVisibleProviders predicate made optional", "ListVisibleProviders", repl("WHERE (p.owner_user_id", "WHERE @everyone::bool OR (p.owner_user_id"), exact},
		{"GetVisibleProviderForUse widened to operator mode", "GetVisibleProviderForUse", repl("p.credential_mode = 'per_user_key'", "p.credential_mode IN ('per_user_key', 'operator_key')"), exact},
		{"LockSharedModelProviderForEdit ORs true", "LockSharedModelProviderForEdit", repl(" FOR NO KEY UPDATE", " OR true FOR NO KEY UPDATE"), exact},
		{"CountSharedModelProviders ORs true", "CountSharedModelProviders", repl("'operator_key')", "'operator_key') OR true"), exact},
		{"the sink admits a disabled key", "GetOperatorProviderKeyForTest", repl("AND disabled_at IS NULL", "AND disabled_at IS NULL OR disabled_at IS NOT NULL"), exact},
		{"the sink drops the provider_id bind", "GetOperatorProviderKeyForTest", repl(" AND provider_id = @provider_id", ""), "lacks required text"},
		{"the write lock projects ciphertext", "LockOperatorProviderKeyForWrite", repl("SELECT id, key_rev", "SELECT id, ciphertext, key_rev"), "names the sealed key"},
		{"the evidence lookup names the sealed key", "GetOperatorProviderForTest", repl("cust.id AS key_id", "cust.ciphertext AS key_id"), "names the sealed key"},
		{"admin get projects to_jsonb(cust)", "GetSharedModelProviderAdmin", repl("cust.set_at AS operator_key_set_at", "to_jsonb(cust) AS operator_key_set_at"), "projects custody internals"},
		{"admin get projects cust.*", "GetSharedModelProviderAdmin", repl("cust.set_at AS operator_key_set_at", "cust.*"), "projects custody internals"},
		{"admin list projects cust.key_rev", "ListSharedModelProvidersAdmin", repl("cust.set_at AS operator_key_set_at", "cust.key_rev AS operator_key_set_at"), "projects custody internals"},
		{"admin get drops the evidence retirement predicate", "GetSharedModelProviderAdmin", repl(" AND p.retired_at IS NULL LEFT JOIN", " LEFT JOIN"), exact},
		{"admin list drops the evidence retirement predicate", "ListSharedModelProvidersAdmin", repl(" AND p.retired_at IS NULL LEFT JOIN", " LEFT JOIN"), exact},
		{"Enable without nextval", "EnableOperatorProviderKey", repl("SET disabled_at = NULL, key_rev = nextval('model_provider_operator_key_rev_seq')", "SET disabled_at = NULL"), "custody UPDATE does not assign"},
		{"Disable without nextval", "DisableOperatorProviderKey", repl(", key_rev = nextval('model_provider_operator_key_rev_seq')", ""), "custody UPDATE does not assign"},
		{"the Test lookup loses the operator scope", "GetOperatorProviderForTest", repl("p.owner_user_id IS NULL AND p.credential_mode = 'operator_key'", "p.id IS NOT NULL"), "lacks required text"},
		{"a recipient reader widened to both modes", "LockVisibleProviderForEvidence", repl("p.credential_mode = 'per_user_key'", "p.credential_mode IN ('per_user_key', 'operator_key')"), exact},
	}
	for _, c := range cases {
		got := strings.Join(visibilityViolations(fileWith(t, c.stmt, c.edit), visibilityTable), "\n")
		if !strings.Contains(got, c.stmt+": ") || !strings.Contains(got, c.want) {
			t.Errorf("%s: violations %q do not name %s with %q", c.name, got, c.stmt, c.want)
		}
	}
	// An adminCountMust entry that keeps the old single-mode literal no longer matches an admin
	// statement: the two-mode string replaced it, and no entry keeps both.
	oldMust := append([]string{"p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key'"}, bothSelectionArms...)
	table := map[string]visEntry{}
	for k, v := range visibilityTable {
		table[k] = v
	}
	e := table["GetSharedModelProviderAdmin"]
	e.must = oldMust
	table["GetSharedModelProviderAdmin"] = e
	if got := strings.Join(visibilityViolations(fileWith(t, "GetSharedModelProviderAdmin", func(s string) string { return s + " " }), table), "\n"); !strings.Contains(got, "GetSharedModelProviderAdmin: lacks required text p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key'") {
		t.Errorf("an admin entry keeping the single-mode literal was not refused: %q", got)
	}
	for _, m := range adminCountMust {
		if strings.Contains(m, "credential_mode = 'per_user_key'") {
			t.Errorf("adminCountMust keeps the single-mode literal: %q", m)
		}
	}
}

// custodyTableMarker is the stem of both organization-key tables and their sequence. Only the reviewed
// custody statements in queries/model_providers.sql (and the migrations) may name it.
const custodyTableMarker = "model_provider_operator_key"

// custodyOutOfFileViolations returns each file that names the custody tables: the production Go files
// and every other queries/*.sql. It is a pure function over already-filtered inputs so its negative
// test can feed it a synthetic offender; the production walk is TestOperatorCustodyIsNamedOnlyInItsQueryFile.
func custodyOutOfFileViolations(goFiles, otherQueries map[string]string) []string {
	var out []string
	for path, src := range goFiles {
		if strings.Contains(src, custodyTableMarker) {
			out = append(out, path+": production Go names an organization key table")
		}
	}
	for path, src := range otherQueries {
		if strings.Contains(src, custodyTableMarker) {
			out = append(out, path+": a query file other than model_providers.sql names an organization key table")
		}
	}
	sort.Strings(out)
	return out
}

func TestOperatorCustodyIsNamedOnlyInItsQueryFile(t *testing.T) {
	other := map[string]string{}
	paths, err := filepath.Glob("queries/*.sql")
	if err != nil || len(paths) == 0 {
		t.Fatalf("glob queries: %v (%d files)", err, len(paths))
	}
	for _, f := range paths {
		if filepath.Base(f) == "model_providers.sql" {
			continue
		}
		raw, err := os.ReadFile(f) //nolint:gosec // G304: f comes from a Glob over this package's own queries dir, not from input.
		if err != nil {
			t.Fatal(err)
		}
		other[f] = string(raw)
	}
	if got := custodyOutOfFileViolations(productionGoFiles(t), other); len(got) != 0 {
		t.Fatalf("organization key tables named outside queries/model_providers.sql:\n  %s", strings.Join(got, "\n  "))
	}
}

// TestCustodyOutOfFileScanFiresAndIgnoresTestsAndGeneratedFiles: a synthetic production file and a
// synthetic other query file each fail, and the production filter drops tests and generated files, so
// a green walk cannot mean it saw nothing.
func TestCustodyOutOfFileScanFiresAndIgnoresTestsAndGeneratedFiles(t *testing.T) {
	got := custodyOutOfFileViolations(
		map[string]string{"handler/leak.go": `q := "SELECT ciphertext FROM model_provider_operator_keys"`, "handler/ok.go": "package handler"},
		map[string]string{"queries/other.sql": "SELECT 1 FROM model_provider_operator_key_test_results;", "queries/fine.sql": "SELECT 1;"})
	if len(got) != 2 || !strings.HasPrefix(got[0], "handler/leak.go: ") || !strings.HasPrefix(got[1], "queries/other.sql: ") {
		t.Fatalf("violations = %q, want the two synthetic offenders only", got)
	}
	for name, want := range map[string]bool{
		"providers_admin.go":      true,
		"providers_admin_test.go": false,
		"model_providers.sql.go":  false,
		"notes.txt":               false,
	} {
		if isProductionGoFile(name) != want {
			t.Errorf("isProductionGoFile(%q) = %t, want %t", name, !want, want)
		}
	}
}
