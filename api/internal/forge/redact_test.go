package forge

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

const fakePAT = "glpat-AbCdEf0123456789xyz"

func TestRedactorScrubsSecretFromString(t *testing.T) {
	r := newRedactor(fakePAT)
	in := "GET https://gitlab.example.com/api/v4/user with PRIVATE-TOKEN: " + fakePAT + " failed"
	got := r.string(in)
	if strings.Contains(got, fakePAT) {
		t.Fatalf("redacted string still contains the PAT: %q", got)
	}
	if !strings.Contains(got, redactPlaceholder) {
		t.Fatalf("expected placeholder in %q", got)
	}
}

func TestRedactorScrubsSecretFromError(t *testing.T) {
	r := newRedactor(fakePAT)
	// Simulate an error whose message embeds the token (worst case).
	orig := fmt.Errorf("request failed: token=%s", fakePAT)
	red := r.error(orig)
	if strings.Contains(red.Error(), fakePAT) {
		t.Fatalf("redacted error still leaks the PAT: %q", red.Error())
	}
	if !strings.Contains(red.Error(), redactPlaceholder) {
		t.Fatalf("expected placeholder in %q", red.Error())
	}
}

func TestRedactorErrorNilPassthrough(t *testing.T) {
	if err := newRedactor(fakePAT).error(nil); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestRedactorIgnoresShortSecrets(t *testing.T) {
	// A 3-char "secret" must not be redacted, or it would mangle ordinary text.
	r := newRedactor("abc")
	const s = "the abc quick brown fox"
	if got := r.string(s); got != s {
		t.Fatalf("short secret should be ignored, got %q", got)
	}
}

func TestRedactorDoesNotExposeOriginalViaUnwrap(t *testing.T) {
	r := newRedactor(fakePAT)
	orig := fmt.Errorf("leak %s", fakePAT)
	red := r.error(orig)
	// Unwrapping must not reach the original (which still holds the token).
	if unwrapped := errors.Unwrap(red); unwrapped != nil && strings.Contains(unwrapped.Error(), fakePAT) {
		t.Fatal("Unwrap exposed the original error carrying the PAT")
	}
}
