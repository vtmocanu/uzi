package workersvc

import (
	"errors"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/secretscrub"
)

// invisibleSeparators are characters that render as nothing (or as a break) and that a hostile
// remote can slip into the middle of a credential to dodge a redactor.
var invisibleSeparators = map[string]string{
	"word-joiner U+2060":      "\u2060",
	"zero-width-space U+200B": "\u200b",
	"NUL":                     "\x00",
	"newline":                 "\n",
	"ESC CSI":                 "\x1b[0m",
}

// assertNoBodyPiece fails when out holds a contiguous run of minRun characters of body.
func assertNoBodyPiece(t *testing.T, out, body string, minRun int) {
	t.Helper()
	for i := 0; i+minRun <= len(body); i++ {
		if strings.Contains(out, body[i:i+minRun]) {
			t.Fatalf("output %q still holds a %d-character piece of the token body %q", out, minRun, body)
		}
	}
}

// A forge token split mid-body by an invisible character must not survive in either half.
// Assembled at runtime: no token-shaped literal in tracked source.
func TestHeldRedactSplitFamilyToken(t *testing.T) {
	body := strings.Repeat("Ab3x", 5)
	for name, sep := range invisibleSeparators {
		t.Run(name, func(t *testing.T) {
			split := "remote: glpat-" + body[:10] + sep + body[10:] + " rejected"
			for label, out := range map[string]string{
				"sanitizeHeldText": sanitizeHeldText(split),
				"heldErrText":      heldErrText(errors.New(split), ""),
			} {
				assertNoBodyPiece(t, out, body, 8)
				if strings.Contains(out, body) {
					t.Fatalf("%s: rejoined token body in %q", label, out)
				}
				if !strings.Contains(out, "rejected") {
					t.Fatalf("%s: surrounding text lost: %q", label, out)
				}
			}
		})
	}
}

// An opaque Forgejo-style PAT (40 hex, no scrubber family) is redacted by exact match, also when
// a separator splits it, and also when it is echoed unsplit.
func TestHeldRedactOpaquePAT(t *testing.T) {
	pat := strings.Repeat("9f3a1c7e", 5)
	if got := secretscrub.Scrub("x " + pat + " y"); !strings.Contains(got, pat) {
		t.Fatalf("fixture premise broken: secretscrub already redacts the PAT: %q", got)
	}
	for name, sep := range invisibleSeparators {
		t.Run("split/"+name, func(t *testing.T) {
			echoed := "refused: bad credential " + pat[:17] + sep + pat[17:] + " for user"
			out := heldErrText(errors.New(echoed), pat)
			assertNoBodyPiece(t, out, pat, 8)
			if !strings.Contains(out, "[redacted]") || !strings.Contains(out, "for user") {
				t.Fatalf("heldErrText = %q", out)
			}
		})
	}
	t.Run("unsplit", func(t *testing.T) {
		out := heldErrText(errors.New("refused: "+pat+" is not valid"), pat)
		if out != "refused: [redacted] is not valid" {
			t.Fatalf("heldErrText = %q", out)
		}
	})
	t.Run("pat itself carries an invisible character", func(t *testing.T) {
		out := heldErrText(errors.New("refused: "+pat+" no"), pat[:10]+"\u2060"+pat[10:])
		if strings.Contains(out, pat[:8]) {
			t.Fatalf("heldErrText = %q", out)
		}
	})
}
