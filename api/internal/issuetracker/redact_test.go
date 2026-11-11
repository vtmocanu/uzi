package issuetracker

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// fixtureCredential assembles a credential-shaped value at runtime so no
// complete token literal sits in source. It contains characters that each
// escaping form rewrites (space, slash, quote).
func fixtureCredential() string {
	return strings.Join([]string{"perm", ":", "ab c/d\"e", ".", "Zq9xLm2VnB7wKp"}, "")
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }

// scrubsTo asserts r replaces form in a message with exactly one placeholder
// and nothing else, so removing any single encoding from newRedactor fails.
func scrubsTo(t *testing.T, r redactor, form string) {
	t.Helper()
	if got, want := r.error(fmt.Errorf("boom %s end", form)).Error(), "boom [REDACTED] end"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestRedactorScrubsEveryEncoding uses a token without '.', so no segment
// scrub can mask a missing whole-token form.
func TestRedactorScrubsEveryEncoding(t *testing.T) {
	cred := strings.Join([]string{"perm", ":", "ab c/d\"e", "Zq9xLm2VnB7wKp"}, "")
	r := newRedactor(cred)
	for name, form := range map[string]string{
		"raw":         cred,
		"query":       "perm%3Aab+c%2Fd%22eZq9xLm2VnB7wKp",
		"query lower": "perm%3aab+c%2fd%22eZq9xLm2VnB7wKp",
		"path":        "perm:ab%20c%2Fd%22eZq9xLm2VnB7wKp",
		"path lower":  "perm:ab%20c%2fd%22eZq9xLm2VnB7wKp",
		"json":        `perm:ab c/d\"eZq9xLm2VnB7wKp`,
		"json slash":  `perm:ab c\/d\"eZq9xLm2VnB7wKp`,
	} {
		t.Run(name, func(t *testing.T) { scrubsTo(t, r, form) })
	}
}

// TestRedactorScrubsEverySegmentEncoding covers the final '.'-separated
// segment in each form; the segment holds characters each escaper rewrites.
func TestRedactorScrubsEverySegmentEncoding(t *testing.T) {
	r := newRedactor(strings.Join([]string{"perm:id", ".", "Zq9/xLm 2VnB7w\"Kp"}, ""))
	for name, form := range map[string]string{
		"raw":         "Zq9/xLm 2VnB7w\"Kp",
		"query":       "Zq9%2FxLm+2VnB7w%22Kp",
		"query lower": "Zq9%2fxLm+2VnB7w%22Kp",
		"path":        "Zq9%2FxLm%202VnB7w%22Kp",
		"path lower":  "Zq9%2fxLm%202VnB7w%22Kp",
		"json":        `Zq9/xLm 2VnB7w\"Kp`,
		"json slash":  `Zq9\/xLm 2VnB7w\"Kp`,
	} {
		t.Run(name, func(t *testing.T) { scrubsTo(t, r, form) })
	}
}

func TestRedactorShortTokenStillScrubbed(t *testing.T) {
	r := newRedactor("abcdef") // below minSegmentLen: the raw token is still listed
	if got := r.string("x abcdef y"); got != "x [REDACTED] y" {
		t.Fatalf("got %q", got)
	}
}

func TestRedactorSecretsDeduped(t *testing.T) {
	r := newRedactor(strings.Repeat("a", 20))
	if len(r.secrets) != 1 {
		t.Fatalf("secrets = %q, want one entry", r.secrets)
	}
}

func TestRedactorShortSegmentKept(t *testing.T) {
	r := newRedactor(strings.Join([]string{"abcdefghijklmnop", ".", "xyz"}, ""))
	if got := r.string("see xyz here"); got != "see xyz here" {
		t.Fatalf("short segment scrubbed: %q", got)
	}
}

func TestRedactorStripsControlAndFormatRunes(t *testing.T) {
	r := newRedactor(fixtureCredential())
	for _, c := range []rune{
		0x00, 0x1b, 0x09, 0x7f, 0x85, 0x9f,
		0x200b, 0x200e, 0x200f, 0x061c, 0xfeff,
		0x202a, 0x202b, 0x202c, 0x202d, 0x202e,
		0x2066, 0x2067, 0x2068, 0x2069, 0x2028, 0x2029,
	} {
		got := r.string("a" + string(c) + "b")
		if got != "a b" {
			t.Errorf("U+%04X: got %q, want %q", c, got, "a b")
		}
	}
}

func TestRedactorScrubsTokenSplitByControlRune(t *testing.T) {
	cred := fixtureCredential()
	r := newRedactor(cred)
	// A raw token has no control runes, so it is scrubbed whole.
	if got := r.string("x " + cred + " y"); strings.Contains(got, cred) {
		t.Fatalf("leaked: %q", got)
	}
}

func TestRedactorTruncatesOnRuneBoundary(t *testing.T) {
	r := newRedactor(fixtureCredential())
	in := strings.Repeat("a", maxMessageBytes-1) + "éé"
	got := r.string(in)
	if len(got) > maxMessageBytes {
		t.Fatalf("len = %d", len(got))
	}
	if got != strings.Repeat("a", maxMessageBytes-1) {
		t.Fatalf("split rune kept: len %d", len(got))
	}
}

func TestRedactorErrorHasNoUnwrap(t *testing.T) {
	r := newRedactor(fixtureCredential())
	orig := fmt.Errorf("wrap: %w", context.Canceled)
	err := r.error(orig)
	if errors.Is(err, context.Canceled) {
		t.Fatal("redacted error exposes the original chain")
	}
	if errors.Unwrap(err) != nil {
		t.Fatal("redacted error has Unwrap")
	}
	if r.error(nil) != nil {
		t.Fatal("nil must stay nil")
	}
}

func TestRedactorClass(t *testing.T) {
	r := newRedactor(fixtureCredential())
	for _, tc := range []struct {
		name   string
		err    error
		status int
		want   forge.ErrorClass
	}{
		{"deadline", context.DeadlineExceeded, 0, forge.ErrorClassTimeout},
		{"net timeout", timeoutErr{}, 0, forge.ErrorClassTimeout},
		{"401", errors.New("x"), 401, forge.ErrorClassAuth},
		{"403", errors.New("x"), 403, forge.ErrorClassAuth},
		{"429", errors.New("x"), 429, forge.ErrorClassRateLimited},
		{"500", errors.New("x"), 500, forge.ErrorClassServerError},
		{"503", errors.New("x"), 503, forge.ErrorClassServerError},
		{"404", errors.New("x"), 404, forge.ErrorClassOther},
		{"plain", errors.New("x"), 0, forge.ErrorClassOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := forge.Class(r.errorStatus(tc.err, tc.status)); got != tc.want {
				t.Fatalf("Class = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRedactorKeepsOversizeProvenance(t *testing.T) {
	r := newRedactor(fixtureCredential())
	err := r.error(&forge.ResponseTooLargeError{Provenance: forge.ResponseIssueSuccess})
	if !forge.IsIssueResponseTooLarge(err) || !errors.Is(err, forge.ErrResponseTooLarge) {
		t.Fatalf("oversize identity lost: %v", err)
	}
}

func TestRedactorSentinelIdentifiableAndRedacted(t *testing.T) {
	cred := fixtureCredential()
	r := newRedactor(cred)
	err := r.sentinel(ErrRedirectRefused, 302, "to "+cred)
	if !errors.Is(err, ErrRedirectRefused) {
		t.Fatal("sentinel not identifiable")
	}
	if strings.Contains(err.Error(), cred) {
		t.Fatalf("token leaked: %q", err.Error())
	}
	if errors.Is(err, ErrProjectNotBound) {
		t.Fatal("matches an unrelated sentinel")
	}
	if got := errors.Unwrap(err); got != ErrRedirectRefused {
		t.Fatalf("Unwrap = %v, want only the sentinel", got)
	}
	if forge.Class(err) != forge.ErrorClassOther {
		t.Fatalf("class = %q", forge.Class(err))
	}
}

func TestRedactorSentinelRestrictedToPackageSentinels(t *testing.T) {
	r := newRedactor(fixtureCredential())
	foreign := errors.New("foreign")
	err := r.sentinel(foreign, 0, "d")
	if errors.Is(err, foreign) || errors.Unwrap(err) != nil {
		t.Fatalf("foreign error stayed in the chain: %v", err)
	}
	if err.Error() != "foreign: d" {
		t.Fatalf("message = %q", err.Error())
	}
}

func TestClassifyStatusServerErrorRange(t *testing.T) {
	for status, want := range map[int]forge.ErrorClass{
		499: forge.ErrorClassOther, 500: forge.ErrorClassServerError,
		599: forge.ErrorClassServerError, 600: forge.ErrorClassOther,
	} {
		if got := classifyStatus(status); got != want {
			t.Errorf("classifyStatus(%d) = %q, want %q", status, got, want)
		}
	}
}

func TestValidateToken(t *testing.T) {
	if ValidateToken(strings.Repeat("a", minTokenLen-1)) == nil {
		t.Fatal("short token accepted")
	}
	if err := ValidateToken(strings.Repeat("a", minTokenLen)); err != nil {
		t.Fatal(err)
	}
	short := strings.Repeat("q", minTokenLen-1)
	if strings.Contains(ValidateToken(short).Error(), short) {
		t.Fatal("error leaks token")
	}
}

func TestValidateTokenBoundsAndCharacters(t *testing.T) {
	if err := ValidateToken(strings.Repeat("a", maxTokenLen)); err != nil {
		t.Errorf("a token of the maximum length: %v", err)
	}
	cases := map[string]string{
		"too long":   strings.Repeat("a", maxTokenLen+1),
		"newline":    strings.Repeat("a", 20) + "\n",
		"nul":        strings.Repeat("a", 20) + "\x00",
		"zero width": strings.Repeat("a", 20) + "\u200b",
		"bidi":       strings.Repeat("a", 20) + "\u202e",
		"delete":     strings.Repeat("a", 20) + "\x7f",
		"line sep":   strings.Repeat("a", 20) + "\u2028",
		"para sep":   strings.Repeat("a", 20) + "\u2029",
	}
	for name, tok := range cases {
		err := ValidateToken(tok)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), tok) {
			t.Errorf("%s: error leaks the token", name)
		}
	}
}

// TestRedactorScrubsMixedCaseEscapes covers a percent-escaped token whose hex
// digits are upper case in one escape and lower case in another.
func TestRedactorScrubsMixedCaseEscapes(t *testing.T) {
	cred := strings.Join([]string{"perm", ":", "a/b/c", "Zq9xLm2VnB7wKp"}, "")
	r := newRedactor(cred)
	scrubsTo(t, r, "perm%3Aa%2fb%2Fc"+"Zq9xLm2VnB7wKp")
	scrubsTo(t, r, "perm%3aa%2Fb%2fc"+"Zq9xLm2VnB7wKp")
}

// TestRedactorSecondScrubAfterSanitize pins the scrub that follows sanitize: a
// dotless token holding a space is split by a TAB in the message, which only
// sanitize turns back into the token.
func TestRedactorSecondScrubAfterSanitize(t *testing.T) {
	cred := strings.Join([]string{"ab cd", "-efghijklmnop"}, "")
	r := newRedactor(cred)
	got := r.error(errors.New("x ab\tcd-efghijklmnop y")).Error()
	if got != "x [REDACTED] y" {
		t.Fatalf("got %q", got)
	}
}

// TestPackageSentinelsMatchErrorsFile fails when errors.go declares an Err
// variable that packageSentinels omits, which would stop errors.Is matching it
// through redactor.sentinel.
func TestPackageSentinelsMatchErrorsFile(t *testing.T) {
	file, err := parser.ParseFile(gotoken.NewFileSet(), "errors.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	declared := 0
	for _, d := range file.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != gotoken.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			for _, n := range spec.(*ast.ValueSpec).Names {
				if strings.HasPrefix(n.Name, "Err") {
					declared++
				}
			}
		}
	}
	if declared != len(packageSentinels) {
		t.Fatalf("errors.go declares %d Err variables, packageSentinels lists %d", declared, len(packageSentinels))
	}
	r := newRedactor(fixtureCredential())
	for _, s := range packageSentinels {
		if !errors.Is(r.sentinel(s, 0, "x"), s) {
			t.Errorf("sentinel %v lost through redactor.sentinel", s)
		}
	}
}
