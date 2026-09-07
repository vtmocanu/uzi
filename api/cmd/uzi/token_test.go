package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestTokenList(t *testing.T) {
	fc := &uzicli.FakeClient{Secrets: []apitypes.SecretDTO{
		{ID: "s1", Kind: "anthropic_token", Label: "default", IsDefault: true, CreatedAt: time.Unix(1784000000, 0)},
		{ID: "s2", Kind: "anthropic_token", Label: "console-key", IsDefault: false, CreatedAt: time.Unix(1784000000, 0)},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "token", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "default") || !strings.Contains(out, "console-key") {
		t.Errorf("token list output missing labels: %q", out)
	}
	// The table marks the default.
	if !strings.Contains(out, "true") {
		t.Errorf("token list should show the default flag: %q", out)
	}
}

func TestTokenListJSON(t *testing.T) {
	fc := &uzicli.FakeClient{Secrets: []apitypes.SecretDTO{
		{ID: "s1", Kind: "anthropic_token", Label: "default", IsDefault: true, CreatedAt: time.Unix(1784000000, 0)},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "token", "list", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"label": "default"`) || !strings.Contains(out, `"is_default": true`) {
		t.Errorf("token list --json missing fields: %q", out)
	}
	// The value must never appear in any CLI output — there is no value field at all.
	if strings.Contains(out, "ciphertext") || strings.Contains(out, "sealed") {
		t.Fatalf("token list leaked a value-ish field: %q", out)
	}
}

// The token command tree carries `list` and `pool`, and NOTHING that mints or
// replaces a credential: add/rename/set-default/rm are cookie-only web actions
// (PRD #104 D8) and must not exist as CLI commands, the same way `uzi worker` has
// no `create`.
//
// This test used to be named TestTokenHasOnlyList and its comment said the tree
// carried "ONLY list". That was true and is not any more — `pool` (PRD #111 M2,
// D13) is a write, and it is legitimate here for a reason the old wording could not
// express: the rule was never "no writes", it was "nothing that MINTS or REPLACES a
// credential". Toggling pool membership re-points spend among tokens the caller
// already holds; a stolen uzc_ gains nothing from it that it did not already have.
// The forbidden list below is the durable half and is unchanged.
func TestTokenSubcommands(t *testing.T) {
	root := newRootCmd(fakeEnv(&uzicli.FakeClient{}))
	tok := findCmd(root, "token")
	if tok == nil {
		t.Fatal("missing `uzi token` command")
	}
	subs := map[string]bool{}
	for _, c := range tok.Commands() {
		subs[c.Name()] = true
	}
	for _, want := range []string{"list", "pool"} {
		if !subs[want] {
			t.Errorf("`uzi token %s` is missing", want)
		}
	}
	for _, forbidden := range []string{"add", "rename", "set-default", "rm", "create", "delete"} {
		if subs[forbidden] {
			t.Errorf("`uzi token %s` exists, but minting or replacing a credential is web-only (D8)", forbidden)
		}
	}
}

// --- PRD #111 M2: the auto-selection pool toggle ---------------------------

// poolFake stages THREE tokens, and the third one is the point (B1).
//
// With only the first two — (default=true, pool=false) and (default=false, pool=true)
// — the DEFAULT and POOL columns are exact inverses, so they render the same multiset
// of strings in either direction. Measured: swapping boolStr(s.AutoEligible) for
// boolStr(s.IsDefault), the plausible copy-paste in a row with two boolean columns,
// left the entire `uzi token list` suite green including the column test. The failure
// it hides is direct — the CLI names the wrong tokens as pooled and a user opts the
// wrong credential in.
//
// The third token is false in BOTH columns, which breaks the symmetry: (true,false),
// (false,true), (false,false) is not the same multiset as its transpose.
func poolFake() *uzicli.FakeClient {
	return &uzicli.FakeClient{
		Secrets: []apitypes.SecretDTO{
			{ID: "s1", Kind: "anthropic_token", Label: "default", IsDefault: true, AutoEligible: false, CreatedAt: time.Unix(1784000000, 0)},
			{ID: "s2", Kind: "anthropic_token", Label: "console-key", IsDefault: false, AutoEligible: true, CreatedAt: time.Unix(1784000000, 0)},
			{ID: "s3", Kind: "anthropic_token", Label: "spare-key", IsDefault: false, AutoEligible: false, CreatedAt: time.Unix(1784000000, 0)},
		},
		PoolSecret: apitypes.SecretDTO{ID: "s2", Kind: "anthropic_token", Label: "console-key", AutoEligible: true},
	}
}

// TestTokenPoolResolvesLabelToID pins that the LABEL is resolved client-side, to an
// id, before anything is sent. That is the whole shape of the command: the server
// route is keyed on the secret id, and a label posted to it would be a second,
// server-side resolution nobody wrote.
func TestTokenPoolResolvesLabelToID(t *testing.T) {
	fc := poolFake()
	_, _, code := runCLI(t, fakeEnv(fc), "token", "pool", "console-key", "--on")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastPoolSecretID != "s2" {
		t.Errorf("sent secret id %q, want s2 (the label must be resolved to an id client-side)", fc.LastPoolSecretID)
	}
	if !fc.LastPoolValue {
		t.Error("--on must send auto_eligible=true")
	}
}

// Case-insensitively, matching the unique index 00077 put on
// (user_id, kind, lower(label)) — `Console-Key` and `console-key` are the same
// token everywhere else, so they must be here too.
func TestTokenPoolLabelIsCaseInsensitive(t *testing.T) {
	fc := poolFake()
	if _, _, code := runCLI(t, fakeEnv(fc), "token", "pool", "CONSOLE-KEY", "--off"); code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastPoolSecretID != "s2" {
		t.Errorf("sent %q, want s2 — label matching must be case-insensitive", fc.LastPoolSecretID)
	}
	if fc.LastPoolValue {
		t.Error("--off must send auto_eligible=false")
	}
}

// TestTokenPoolRequiresExactlyOneDirection: neither flag and both flags are usage
// errors, and NOTHING is sent in either case. Defaulting one way would make a bare
// `uzi token pool console-key` a spend decision the user never expressed.
func TestTokenPoolRequiresExactlyOneDirection(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"neither", []string{"token", "pool", "console-key"}},
		{"both", []string{"token", "pool", "console-key", "--on", "--off"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := poolFake()
			_, _, code := runCLI(t, fakeEnv(fc), tc.args...)
			if code != uzicli.ExitUsage {
				t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
			}
			if fc.LastPoolSecretID != "" {
				t.Errorf("a usage error still sent a write for %q", fc.LastPoolSecretID)
			}
		})
	}
}

// TestTokenPoolUnknownLabelIsUsageError also EXECUTES the `uzi token list` remedy
// this command prints — the instruction knownInstructions attributes to this test.
// The assertion is the outcome pair: exit 3 (usage, not a 404 — the label never
// reached the server) and no write attempted.
func TestTokenPoolUnknownLabelIsUsageError(t *testing.T) {
	fc := poolFake()
	_, errOut, code := runCLI(t, fakeEnv(fc), "token", "pool", "no-such-token", "--on")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if !strings.Contains(errOut, "uzi token list") {
		t.Errorf("the error should name the read that lists the valid labels, got: %q", errOut)
	}
	if fc.LastPoolSecretID != "" {
		t.Errorf("an unresolvable label still sent a write for %q", fc.LastPoolSecretID)
	}
}

// The POOL column is the OPT-IN, and `list` must show both values — a column that
// only ever printed one would be indistinguishable from a broken one.
func TestTokenListShowsPoolColumn(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(poolFake()), "token", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "POOL") {
		t.Errorf("token list is missing the POOL column: %q", out)
	}
	// POSITIONAL, not "the word true appears somewhere". Each row's POOL cell is
	// asserted against that row's own AutoEligible, which is what a
	// Contains-anywhere check cannot do and what lets the wrong column render
	// undetected. Rows are ID / KIND / LABEL / DEFAULT / POOL / ELIGIBLE / STATUS / CREATED.
	for _, tc := range []struct {
		label       string
		wantPool    string
		wantDefault string
	}{
		{"default", "false", "true"},
		{"console-key", "true", "false"},
		{"spare-key", "false", "false"},
	} {
		var row string
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, tc.label) {
				row = line
				break
			}
		}
		if row == "" {
			t.Errorf("no row for %q in:\n%s", tc.label, out)
			continue
		}
		cells := strings.Fields(row)
		if len(cells) < 8 {
			t.Errorf("row for %q has %d cells, want 8: %q", tc.label, len(cells), row)
			continue
		}
		if cells[3] != tc.wantDefault || cells[4] != tc.wantPool {
			t.Errorf("row for %q rendered DEFAULT=%q POOL=%q, want DEFAULT=%q POOL=%q — "+
				"the two columns are separate facts and rendering one in the other's place "+
				"tells a user the wrong credential is pooled: %q",
				tc.label, cells[3], cells[4], tc.wantDefault, tc.wantPool, row)
		}
	}
}

// The label reaches a terminal through cellText, like every other user-authored cell.
// This sentence used to end "because uzicli.Printer.Table does not sanitize what it is
// handed"; #180 made that false (Table now runs CellText over every cell), so this test
// now pins TWO independent defences rather than the only one — which is what it should
// assert anyway, since it drives the real render path and does not care which layer
// stripped the bytes.
//
// 🔴 THE FIXTURE BELOW IS DELIBERATELY UN-STORABLE THROUGH THE API. This comment
// used to say "validateSecretLabel permits unicode.Cf", which was true when written
// and stopped being true in this same milestone: the validator now rejects both
// unicode.IsControl and unicode.Cf, so no handler will accept this label.
//
// The test still earns its keep, and the reasoning is recorded here so nobody has to
// re-derive it before deleting it: the validator is a statement about what the
// SERVER accepts, this is a statement about what the RENDERER does with what it is
// given, and they sit on opposite sides of a trust boundary. A pre-M2 stored label,
// a future write path that skips validation, or a row written directly to the
// database all reach this code without passing that check. Keep the fixture hostile
// and keep the cellText routing.
func TestTokenListSanitizesLabel(t *testing.T) {
	// 🔴 THE FIXTURE NEEDS A NEWLINE AND A TAB, AND DID NOT HAVE THEM. Bidi and CSI
	// are stripped by sanitizeTTY ALONE, so the original fixture passed under either
	// helper and pinned nothing about which one is used — measured: cellText →
	// sanitizeTTY passed, only cellText → raw failed.
	//
	// cellText's distinguishing behaviour is newline folding, tab folding and the
	// length cap. A newline or tab in a table cell breaks the rail the column
	// alignment depends on, which is the whole reason this goes through the cell
	// wrapper rather than the text sanitizer.
	//
	// SECOND INSTANCE OF THIS SHAPE in one PRD (render_test.go was the first): a
	// fixture on which the broken and the correct implementation AGREE, reading as
	// proof of something it never tested. Worth expecting a third.
	fc := &uzicli.FakeClient{Secrets: []apitypes.SecretDTO{
		{ID: "s1", Kind: "anthropic_token", Label: "safe\u202ednetsop\x1b[31m\nnext\tcell", CreatedAt: time.Unix(1784000000, 0)},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "token", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	// The first two are the shared floor (either helper satisfies them); the last two
	// are what tell cellText and sanitizeTTY apart.
	for _, bad := range []string{"\u202e", "\x1b", "\nnext", "\tcell"} {
		if strings.Contains(out, bad) {
			t.Errorf("hostile label reached the terminal carrying %q: %q", bad, out)
		}
	}
	if !strings.Contains(out, "safe") {
		t.Errorf("sanitizing dropped the printable text too: %q", out)
	}
}

// --- PRD #111 D23: the ELIGIBLE column ---------------------------------------

// TestTokenListShowsLiveEligibility is the CLI half of D23. Before the auth move
// `uzi token pool x --on` could opt a token in and give a script NO WAY to learn
// that x can never be picked — R7's silent no-op surviving on the CLI side. The
// column closes it, and this pins that it renders the SERVER's word per row.
func TestTokenListShowsLiveEligibility(t *testing.T) {
	fc := poolFake()
	fc.SelfMeters = []apitypes.TokenRateLimitDTO{
		{SecretID: "s2", Label: "console-key", AutoEligible: true, AutoStatus: "no_reading"},
	}
	out, _, code := runCLI(t, fakeEnv(fc), "token", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "ELIGIBLE") {
		t.Errorf("token list is missing the ELIGIBLE column: %q", out)
	}
	rowOf := func(label string) []string {
		t.Helper()
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, label) {
				return strings.Fields(line)
			}
		}
		t.Fatalf("no row for %q in:\n%s", label, out)
		return nil
	}
	// The pooled token carries the server's status verbatim — this is the whole
	// point of the column, and of D21: the string is autoselect.Classify's answer,
	// not something the CLI re-derived from percentages it does not have.
	if got := rowOf("console-key")[5]; got != "no_reading" {
		t.Errorf("pooled token's ELIGIBLE = %q, want the server's no_reading — a pooled "+
			"token that can never be picked must SAY so", got)
	}
	// An un-pooled token reads "-": the POOL column beside it already says it is out,
	// and repeating "not in pool" would be noise on every row.
	if got := rowOf("default")[5]; got != "-" {
		t.Errorf("un-pooled token's ELIGIBLE = %q, want -", got)
	}
	// A pooled token the meters did not mention reads "?" — NOT "-" and not blank.
	// "Unknown" and "fine" must not look the same, which is the failure the column
	// exists to remove.
	if got := rowOf("spare-key")[5]; got != "-" {
		t.Errorf("un-pooled spare-key's ELIGIBLE = %q, want -", got)
	}
}

// A failed meters read must not fail the listing: a user asking which tokens they
// hold still gets the answer, with eligibility honestly unknown rather than absent.
func TestTokenListSurvivesAMetersFailure(t *testing.T) {
	fc := poolFake()
	fc.SelfMeters = nil // no meters staged, and the fake returns no error either
	out, _, code := runCLI(t, fakeEnv(fc), "token", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 — a secondary read must not fail the listing", code)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "console-key") {
			if got := strings.Fields(line)[5]; got != "?" {
				t.Errorf("pooled token with no meter reads %q, want ? — unknown must not "+
					"look the same as eligible", got)
			}
		}
	}
}

// TestTokenListJSONCarriesLiveEligibility is D23-JSON. `uzi token list` computed the
// eligibility map and then DISCARDED it on the --json branch, so the human table
// gained an ELIGIBLE column while a script still saw only the opt-in flag and could
// not learn that a pooled token will never be picked — R7's silent no-op surviving on
// exactly the surface this CLI exists to serve (its audience is agents). The
// endpoint's one CLI call site is that listing, so there was no other command a
// script could reach it through.
//
// The three states the table distinguishes must survive into JSON, and they must not
// collapse: a pooled token with a status carries it, a token the meters did not
// mention carries null (unknown, NOT "not eligible"), and the field is always
// PRESENT so a consumer can tell null from absent.
//
// MUTATIONS THIS CATCHES, both measured: reverting to `p.JSON(secrets)` (no
// auto_status key at all), and emitting "" instead of null for the unknown case.
func TestTokenListJSONCarriesLiveEligibility(t *testing.T) {
	fc := poolFake()
	fc.SelfMeters = []apitypes.TokenRateLimitDTO{
		{SecretID: "s2", Label: "console-key", AutoEligible: true, AutoStatus: "below_threshold"},
		{SecretID: "s1", Label: "default", AutoEligible: false, AutoStatus: "not_pooled"},
		// s3 (spare-key) is deliberately absent: the "unknown" case.
	}
	out, _, code := runCLI(t, fakeEnv(fc), "token", "list", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}

	var items []map[string]any
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		t.Fatalf("token list --json is not a JSON array: %v\n%s", err, out)
	}
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3", len(items))
	}
	byLabel := map[string]map[string]any{}
	for _, it := range items {
		byLabel[it["label"].(string)] = it
	}
	// The embedded SecretDTO's keys are unchanged — a wrapper object would break
	// every script reading `.[].label` today, which this follow-up is not entitled
	// to do.
	for _, k := range []string{"id", "kind", "label", "is_default", "auto_eligible", "created_at", "updated_at"} {
		if _, ok := byLabel["console-key"][k]; !ok {
			t.Errorf("the JSON item lost SecretDTO's %q key", k)
		}
	}
	if got := byLabel["console-key"]["auto_status"]; got != "below_threshold" {
		t.Errorf("pooled token's auto_status = %v, want the server's below_threshold — a script must "+
			"be able to learn a pooled token will never be picked", got)
	}
	if got := byLabel["default"]["auto_status"]; got != "not_pooled" {
		t.Errorf("un-pooled token's auto_status = %v, want not_pooled — the table can lean on the POOL "+
			"column beside it, a JSON consumer wants the answer", got)
	}
	// Present-and-null, never absent and never "". A pooled token whose eligibility
	// is unknown must not be indistinguishable from one that is fine.
	raw, ok := byLabel["spare-key"]["auto_status"]
	if !ok {
		t.Fatal("auto_status is ABSENT for the token the meters did not mention; null and absent are " +
			"different answers and a consumer cannot tell an omitted field from an old CLI")
	}
	if raw != nil {
		t.Errorf("unmentioned token's auto_status = %#v, want null — \"\" would read as a status the "+
			"server never sent", raw)
	}
}

// --- PRD #1147 M3: kind + status distinction, and the pool kind guard ---------

// codexFake stages one anthropic row and two non-anthropic rows: a codex_auth with a
// link status and a standalone openai_api_key. ListSecrets returns all kinds together
// (M1), so `uzi token list` must render the kind and status distinction M3 adds.
func codexFake() *uzicli.FakeClient {
	return &uzicli.FakeClient{
		Secrets: []apitypes.SecretDTO{
			{ID: "s1", Kind: "anthropic_token", Label: "default", IsDefault: true, AutoEligible: true, CreatedAt: time.Unix(1784000000, 0)},
			{ID: "c1", Kind: "codex_auth", Label: "my-codex", CodexStatus: "linked", CreatedAt: time.Unix(1784000000, 0)},
			{ID: "o1", Kind: "openai_api_key", Label: "my-openai", CodexStatus: "static", CreatedAt: time.Unix(1784000000, 0)},
		},
	}
}

// TestTokenListShowsKindAndStatus pins the M3 human-table shape: a KIND column, a
// STATUS column, and — critically — that POOL and ELIGIBLE read "-" (not applicable)
// on a codex row rather than a misleading "false". There is no Codex auto-selection
// pool server-side, so "false" would imply an opt-in a user could flip.
func TestTokenListShowsKindAndStatus(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(codexFake()), "token", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, col := range []string{"KIND", "STATUS"} {
		if !strings.Contains(out, col) {
			t.Errorf("token list is missing the %s column: %q", col, out)
		}
	}
	rowOf := func(label string) []string {
		t.Helper()
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, label) {
				return strings.Fields(line)
			}
		}
		t.Fatalf("no row for %q in:\n%s", label, out)
		return nil
	}
	// Rows are ID / KIND / LABEL / DEFAULT / POOL / ELIGIBLE / STATUS / CREATED.
	for _, tc := range []struct {
		label                                        string
		wantKind, wantPool, wantEligible, wantStatus string
	}{
		// The anthropic row keeps its POOL/ELIGIBLE facts and carries no status ("-").
		{"default", "anthropic", "true", "?", "-"},
		// The codex row shows its kind and link status; POOL and ELIGIBLE are N/A ("-").
		{"my-codex", "codex", "-", "-", "linked"},
		// The openai_api_key row is a codex-family credential too: kind alias + status.
		{"my-openai", "openai-key", "-", "-", "static"},
	} {
		cells := rowOf(tc.label)
		if len(cells) < 8 {
			t.Errorf("row for %q has %d cells, want 8: %v", tc.label, len(cells), cells)
			continue
		}
		if cells[1] != tc.wantKind {
			t.Errorf("row for %q KIND = %q, want %q", tc.label, cells[1], tc.wantKind)
		}
		// The anthropic row's ELIGIBLE depends on the meters read (unstaged here -> "?"),
		// so only assert POOL and STATUS positionally there; the codex rows assert all.
		if cells[4] != tc.wantPool {
			t.Errorf("row for %q POOL = %q, want %q — a codex row's POOL must be N/A, not a misleading boolean", tc.label, cells[4], tc.wantPool)
		}
		if cells[6] != tc.wantStatus {
			t.Errorf("row for %q STATUS = %q, want %q", tc.label, cells[6], tc.wantStatus)
		}
		if tc.label != "default" && cells[5] != tc.wantEligible {
			t.Errorf("row for %q ELIGIBLE = %q, want %q — no Codex pool exists, so ELIGIBLE is N/A", tc.label, cells[5], tc.wantEligible)
		}
	}
}

// TestTokenListJSONCarriesKindAndStatus confirms the embedded SecretDTO already emits
// `kind` and `codex_status` for a codex row (added in M1), so the JSON surface needs
// no new field — M3's job is only to confirm they are legible per row.
func TestTokenListJSONCarriesKindAndStatus(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(codexFake()), "token", "list", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		t.Fatalf("token list --json is not a JSON array: %v\n%s", err, out)
	}
	byLabel := map[string]map[string]any{}
	for _, it := range items {
		byLabel[it["label"].(string)] = it
	}
	if got := byLabel["my-codex"]["kind"]; got != "codex_auth" {
		t.Errorf("codex row kind = %v, want codex_auth", got)
	}
	if got := byLabel["my-codex"]["codex_status"]; got != "linked" {
		t.Errorf("codex row codex_status = %v, want linked", got)
	}
	// codex_status is `omitempty`, so an anthropic row (empty status) omits the key
	// entirely — presence of the key IS the "this is a Codex credential" signal.
	if _, present := byLabel["default"]["codex_status"]; present {
		t.Errorf("anthropic row should omit codex_status (omitempty), got %v", byLabel["default"]["codex_status"])
	}
	if got := byLabel["default"]["kind"]; got != "anthropic_token" {
		t.Errorf("anthropic row kind = %v, want anthropic_token", got)
	}
}

// TestTokenPoolRejectsCodexLabel is the M3 kind guard: `uzi token pool <label>` must
// refuse a codex-kind label and send NO write, because there is no Codex auto-selection
// pool server-side. The error names the kind so the refusal is legible.
func TestTokenPoolRejectsCodexLabel(t *testing.T) {
	for _, tc := range []struct{ label, wantKind string }{
		{"my-codex", "codex"},
		{"my-openai", "openai-key"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			fc := codexFake()
			_, errOut, code := runCLI(t, fakeEnv(fc), "token", "pool", tc.label, "--on")
			if code != uzicli.ExitUsage {
				t.Fatalf("exit = %d, want %d (usage) — pool is Anthropic-only", code, uzicli.ExitUsage)
			}
			if !strings.Contains(errOut, "Anthropic") || !strings.Contains(errOut, tc.wantKind) {
				t.Errorf("error should say pool is Anthropic-only and name the %s kind, got: %q", tc.wantKind, errOut)
			}
			if fc.LastPoolSecretID != "" {
				t.Errorf("a codex label still sent a write for %q", fc.LastPoolSecretID)
			}
		})
	}
}

// TestTokenPoolPrefersAnthropicOnLabelCollision pins that when a codex and an anthropic
// secret share a label, pool resolves the ANTHROPIC one (the kind filter), rather than
// cross-matching the codex row that happens to appear first.
func TestTokenPoolPrefersAnthropicOnLabelCollision(t *testing.T) {
	fc := &uzicli.FakeClient{
		Secrets: []apitypes.SecretDTO{
			// The codex row is listed FIRST, so an unfiltered match would pick it.
			{ID: "c1", Kind: "codex_auth", Label: "shared", CodexStatus: "linked", CreatedAt: time.Unix(1784000000, 0)},
			{ID: "s1", Kind: "anthropic_token", Label: "shared", AutoEligible: false, CreatedAt: time.Unix(1784000000, 0)},
		},
		PoolSecret: apitypes.SecretDTO{ID: "s1", Kind: "anthropic_token", Label: "shared", AutoEligible: true},
	}
	if _, _, code := runCLI(t, fakeEnv(fc), "token", "pool", "shared", "--on"); code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastPoolSecretID != "s1" {
		t.Errorf("sent secret id %q, want s1 — the kind filter must resolve the anthropic row, not the codex one sharing the label", fc.LastPoolSecretID)
	}
}
