package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/autoselect"
)

// PRD #111 M5 — the CLI half of D20: the run view names the MODE, not just the token.

// sptr/iptr rather than a generic `ptr`: commands_test.go already declares a
// string-only ptr in this package.
func sptr(v string) *string { return &v }
func iptr(v int) *int       { return &v }

// TestCredentialCellNamesTheMode is D20 stated as a test. The three shapes the PRD
// names must be distinguishable, and the fixture pins the exact thing that makes the
// label insufficient: ALL THREE ROWS NAME THE SAME TOKEN. A fixture using three
// different labels would pass against a renderer that ignored the reason entirely.
func TestCredentialCellNamesTheMode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reason   string
		headroom *int
		want     string
	}{
		{"auto pick carries its headroom", "auto", iptr(62), "console-key — auto, 62% headroom"},
		{"a plain default does not", "default", nil, "console-key — default"},
		{"nor does a pin", "pinned", nil, "console-key — pinned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := credentialCell(apitypes.RunDTO{
				AnthropicSecretID:     sptr("11111111-1111-4111-8111-111111111111"),
				AnthropicSecretLabel:  sptr("console-key"),
				AnthropicSelectReason: sptr(tc.reason),
				AnthropicHeadroomPct:  tc.headroom,
			})
			if got != tc.want {
				t.Fatalf("cell = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCredentialCellFallbacksSayWhy: an `auto` worker that did NOT get an auto pick
// must not read as an ordinary default. The worker is configured for auto and the run
// did not get it — a different situation, with a different fix, from a worker that was
// never auto. Three fallbacks, three different problems: nothing in the pool (a
// settings fix), nothing measurable (a poller fix), and the pick would not decrypt (a
// credential fix).
//
// MUTATION THIS CATCHES: rendering the three fallback reasons as bare "default" —
// every case collapses onto the plain-default string asserted above.
func TestCredentialCellFallbacksSayWhy(t *testing.T) {
	seen := map[string]string{}
	for _, reason := range []autoselect.Reason{
		autoselect.ReasonPoolEmpty, autoselect.ReasonPoolStale, autoselect.ReasonOpenFailed,
	} {
		got := credentialCell(apitypes.RunDTO{
			AnthropicSecretID:     sptr("11111111-1111-4111-8111-111111111111"),
			AnthropicSecretLabel:  sptr("default"),
			AnthropicSelectReason: sptr(string(reason)),
		})
		if !strings.Contains(got, "default (auto:") {
			t.Errorf("%s rendered %q; a fallback must say the worker is on AUTO and why it "+
				"did not get a pick, not read as an ordinary default", reason, got)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("%s and %s both render %q; they are different problems with different "+
				"fixes and a user cannot act on a shared string", prev, reason, got)
		}
		seen[got] = string(reason)
	}
}

// TestCredentialCellCoversEveryReason is the EXHAUSTIVE control the design note asked
// for, "not by sampling". A reason with no rendering is invisible until the one user
// who hits it reaches support, and Go's switch is NOT exhaustive over a string type —
// which is why this test exists at all, and why the web's equivalent is a typed Record
// that fails typecheck instead.
//
// 🔴 IT CANNOT CATCH EVERY DELETED ARM, and pretending otherwise is why this comment
// is longer than the test. An earlier version asserted `rendering != string(reason)`
// as a missing-arm signal. That is FALSE for three of the eight: `default`, `pinned`
// and `auto` render as their own wire word, so deleting their arm makes the switch
// fall through to `return string(reason)` and produce a BYTE-IDENTICAL result. The
// mutation is not merely undetectable, it is semantically null — there is no bug to
// catch. The three assertions below are the ones that hold for all eight:
// non-empty, distinct, and enumerated from AllReasons rather than from a list typed
// here.
//
// MUTATION THIS CATCHES, measured: deleting the `judge`, `best_of_pool`, `pool_empty`,
// `pool_stale` or `open_failed` arm — each falls through to its bare wire value, which
// collides with nothing but loses its words. Distinctness is what catches those,
// together with the fallback test above.
func TestCredentialCellCoversEveryReason(t *testing.T) {
	reasons := autoselect.AllReasons()
	if len(reasons) != 8 {
		t.Fatalf("AllReasons has %d entries, want 8 — this test enumerates the vocabulary and a "+
			"change to it must be deliberate here too", len(reasons))
	}
	seen := map[string]autoselect.Reason{}
	for _, r := range reasons {
		got := selectReasonText(r, nil)
		if got == "" {
			t.Errorf("%s renders as the empty string", r)
			continue
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("%s and %s both render %q; every reason needs its own words, or the "+
				"rendering exists and says nothing", prev, r, got)
		}
		seen[got] = r
	}
	// The five that carry real prose must not read as their bare wire value. Stated as
	// an explicit list rather than "all of them" precisely because the other three
	// legitimately do.
	for _, r := range []autoselect.Reason{
		autoselect.ReasonJudge, autoselect.ReasonBestOfPool,
		autoselect.ReasonPoolEmpty, autoselect.ReasonPoolStale, autoselect.ReasonOpenFailed,
	} {
		if got := selectReasonText(r, nil); got == string(r) {
			t.Errorf("%s fell through to the raw wire value %q — the shape of a deleted "+
				"switch arm", r, got)
		}
	}
}

// TestCredentialCellUnknownReasonPassesThrough: the CLI is versioned separately from
// the API, so a newer server can ship a ninth reason this binary has never heard of.
// Printing it as itself is the honest answer; dropping it or guessing a rendering
// would be worse, and inventing one is exactly the lie D21 exists to prevent one layer
// down.
func TestCredentialCellUnknownReasonPassesThrough(t *testing.T) {
	got := credentialCell(apitypes.RunDTO{
		AnthropicSecretID:     sptr("11111111-1111-4111-8111-111111111111"),
		AnthropicSecretLabel:  sptr("console-key"),
		AnthropicSelectReason: sptr("some_future_reason"),
	})
	if !strings.Contains(got, "some_future_reason") {
		t.Fatalf("cell = %q, want it to carry the unrecognised reason verbatim", got)
	}
}

// TestCredentialCellPreM1Run: a run claimed before M1 recorded no mode. The bare label
// is the truthful rendering; a guessed "default" would assert something nothing knows.
func TestCredentialCellPreM1Run(t *testing.T) {
	got := credentialCell(apitypes.RunDTO{
		AnthropicSecretID:    sptr("11111111-1111-4111-8111-111111111111"),
		AnthropicSecretLabel: sptr("console-key"),
	})
	if got != "console-key" {
		t.Fatalf("cell = %q, want the bare label for a run with no recorded reason", got)
	}
}

// TestCredentialCellDeletedToken is F8's CLI half, and the id/label pair is the whole
// point: 00086's SET NULL nulls the id when the token is deleted while the snapshotted
// label survives, so this is a NORMAL historical run rather than a corrupt one.
//
// MUTATION THIS CATCHES: gating the whole cell on the id being present — the run then
// shows no credential at all, which is the pre-M1 behaviour the snapshot column exists
// to fix.
func TestCredentialCellDeletedToken(t *testing.T) {
	got := credentialCell(apitypes.RunDTO{
		AnthropicSecretID:     nil, // deleted
		AnthropicSecretLabel:  sptr("retired-key"),
		AnthropicSelectReason: sptr("pinned"),
	})
	if !strings.Contains(got, "retired-key") {
		t.Fatalf("cell = %q, want it to still name the account the run billed", got)
	}
	if !strings.Contains(got, "deleted") {
		t.Fatalf("cell = %q, want it to say the credential is gone — otherwise it is "+
			"indistinguishable from one a user can still go and look at", got)
	}
}

// TestCredentialCellSanitizesTheLabel: the label is USER-AUTHORED and reaches a table
// cell. cellText folds newlines and tabs (which break the column rail) and caps the
// length; sanitizeTTY alone spares "\n" and would not. Rows written before PRD #111
// M2's validator landed are never re-validated, so a hostile label still arrives here
// through history.
func TestCredentialCellSanitizesTheLabel(t *testing.T) {
	got := credentialCell(apitypes.RunDTO{
		AnthropicSecretID:     sptr("11111111-1111-4111-8111-111111111111"),
		AnthropicSecretLabel:  sptr("safe‮dnetsop\x1b[31m\nnext\tcell"),
		AnthropicSelectReason: sptr("default"),
	})
	for _, bad := range []string{"‮", "\x1b", "\nnext", "\tcell"} {
		if strings.Contains(got, bad) {
			t.Errorf("hostile label reached the terminal carrying %q: %q", bad, got)
		}
	}
	if !strings.Contains(got, "safe") {
		t.Errorf("sanitizing dropped the printable text too: %q", got)
	}
}
