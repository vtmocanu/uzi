package forge

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestWrapErrByteIdentical pins the gitlab and forgejo wrapErr helpers to the
// EXACT string the pre-migration inline idiom produced, so a dropped op-context
// (which the redaction-focused suite would pass green) is caught here. The
// pre-migration idiom for these two drivers was, verbatim:
//
//	g.redact.error(fmt.Errorf("gitlab: %s: %w", op, err))
//	f.redact.error(fmt.Errorf("forgejo: %s: %w", op, err))
//
// so wrapErr(op, err) must render "gitlab: <op>: <err>" / "forgejo: <op>: <err>".
func TestWrapErrByteIdentical(t *testing.T) {
	const op = "some op"
	boom := errors.New("boom")

	cases := []struct {
		name    string
		got     error
		want    string
		wantOld string // the exact pre-migration rendering, recomputed here
	}{
		{
			name:    "gitlab",
			got:     (&gitLab{redact: newRedactor()}).wrapErr(op, boom),
			want:    "gitlab: some op: boom",
			wantOld: newRedactor().error(fmt.Errorf("gitlab: %s: %w", op, boom)).Error(),
		},
		{
			name:    "forgejo",
			got:     (&forgejo{redact: newRedactor()}).wrapErr(op, boom),
			want:    "forgejo: some op: boom",
			wantOld: newRedactor().error(fmt.Errorf("forgejo: %s: %w", op, boom)).Error(),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.got.Error() != c.want {
				t.Fatalf("wrapErr rendered %q, want %q", c.got.Error(), c.want)
			}
			// And byte-identical to the exact idiom it replaced.
			if c.got.Error() != c.wantOld {
				t.Fatalf("wrapErr %q diverged from pre-migration idiom %q", c.got.Error(), c.wantOld)
			}
		})
	}
}

// TestWrapErrNilPassthrough pins the nil short-circuit both helpers share with
// github's wrapErr: a nil error must return a nil error, never a wrapped one.
func TestWrapErrNilPassthrough(t *testing.T) {
	if got := (&gitLab{redact: newRedactor()}).wrapErr("x", nil); got != nil {
		t.Fatalf("gitLab.wrapErr(nil) = %v, want nil", got)
	}
	if got := (&forgejo{redact: newRedactor()}).wrapErr("x", nil); got != nil {
		t.Fatalf("forgejo.wrapErr(nil) = %v, want nil", got)
	}
}

// TestWrapErrRedacts proves the whole point of routing through wrapErr: a secret
// that leaks into the underlying error text is scrubbed, while the driver prefix
// and op-context survive intact.
func TestWrapErrRedacts(t *testing.T) {
	const secret = "glpat-supersecrettoken1234" //nolint:gosec // G101: fake fixture PAT; this test's whole point is asserting it gets redacted

	t.Run("gitlab", func(t *testing.T) {
		g := &gitLab{redact: newRedactor(secret)}
		err := g.wrapErr("verify token", fmt.Errorf("401 unauthorized token=%s", secret))
		msg := err.Error()
		if strings.Contains(msg, secret) {
			t.Fatalf("secret leaked through wrapErr: %q", msg)
		}
		if !strings.Contains(msg, redactPlaceholder) {
			t.Fatalf("expected %q in %q", redactPlaceholder, msg)
		}
		if !strings.HasPrefix(msg, "gitlab: verify token: ") {
			t.Fatalf("op/prefix context lost: %q", msg)
		}
	})

	t.Run("forgejo", func(t *testing.T) {
		f := &forgejo{redact: newRedactor(secret)}
		err := f.wrapErr("verify token", fmt.Errorf("401 unauthorized token=%s", secret))
		msg := err.Error()
		if strings.Contains(msg, secret) {
			t.Fatalf("secret leaked through wrapErr: %q", msg)
		}
		if !strings.Contains(msg, redactPlaceholder) {
			t.Fatalf("expected %q in %q", redactPlaceholder, msg)
		}
		if !strings.HasPrefix(msg, "forgejo: verify token: ") {
			t.Fatalf("op/prefix context lost: %q", msg)
		}
	})
}
