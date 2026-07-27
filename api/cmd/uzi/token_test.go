package main

import (
	"strings"
	"testing"
	"time"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
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
	// undetected. Rows are ID / LABEL / DEFAULT / POOL / CREATED.
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
		if len(cells) < 6 {
			t.Errorf("row for %q has %d cells, want 6: %q", tc.label, len(cells), row)
			continue
		}
		if cells[2] != tc.wantDefault || cells[3] != tc.wantPool {
			t.Errorf("row for %q rendered DEFAULT=%q POOL=%q, want DEFAULT=%q POOL=%q — "+
				"the two columns are separate facts and rendering one in the other's place "+
				"tells a user the wrong credential is pooled: %q",
				tc.label, cells[2], cells[3], tc.wantDefault, tc.wantPool, row)
		}
	}
}

// The label reaches a terminal through cellText, like every other user-authored
// cell, because uzicli.Printer.Table does not sanitize what it is handed.
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
		{ID: "s1", Kind: "anthropic_token", Label: "safe‮dnetsop\x1b[31m\nnext\tcell", CreatedAt: time.Unix(1784000000, 0)},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "token", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	// The first two are the shared floor (either helper satisfies them); the last two
	// are what tell cellText and sanitizeTTY apart.
	for _, bad := range []string{"‮", "\x1b", "\nnext", "\tcell"} {
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
	if got := rowOf("console-key")[4]; got != "no_reading" {
		t.Errorf("pooled token's ELIGIBLE = %q, want the server's no_reading — a pooled "+
			"token that can never be picked must SAY so", got)
	}
	// An un-pooled token reads "-": the POOL column beside it already says it is out,
	// and repeating "not in pool" would be noise on every row.
	if got := rowOf("default")[4]; got != "-" {
		t.Errorf("un-pooled token's ELIGIBLE = %q, want -", got)
	}
	// A pooled token the meters did not mention reads "?" — NOT "-" and not blank.
	// "Unknown" and "fine" must not look the same, which is the failure the column
	// exists to remove.
	if got := rowOf("spare-key")[4]; got != "-" {
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
			if got := strings.Fields(line)[4]; got != "?" {
				t.Errorf("pooled token with no meter reads %q, want ? — unknown must not "+
					"look the same as eligible", got)
			}
		}
	}
}
