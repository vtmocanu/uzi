package forge

import (
	"errors"
	"strings"
)

// redactPlaceholder replaces any occurrence of a secret in a string.
const redactPlaceholder = "[REDACTED]"

// redactor scrubs known secret values (the PAT, and by extension the
// Authorization/PRIVATE-TOKEN header content) out of any string before it can
// reach a log line or a returned error. A driver wraps every error it surfaces
// through redact, so even if the underlying client ever embedded the token in a
// message, it would not escape the package.
type redactor struct {
	secrets []string
}

// newRedactor builds a redactor for the given secrets. Empty and very short
// secrets are ignored: redacting a 1-2 char string would mangle unrelated
// output, and a real PAT is always long.
func newRedactor(secrets ...string) redactor {
	var kept []string
	for _, s := range secrets {
		if len(s) >= 8 {
			kept = append(kept, s)
		}
	}
	return redactor{secrets: kept}
}

// string returns s with every known secret replaced by the placeholder.
func (r redactor) string(s string) string {
	for _, secret := range r.secrets {
		s = strings.ReplaceAll(s, secret, redactPlaceholder)
	}
	return s
}

// error wraps err so its message (and Unwrap chain via the original) carries no
// secret. Returns nil for a nil error. The wrapped error deliberately does NOT
// implement Unwrap: the whole point is to sever access to the original message,
// which might contain the token; callers get a clean string and the redacted
// text is authoritative.
func (r redactor) error(err error) error {
	if err == nil {
		return nil
	}
	msg := r.string(err.Error())
	return errors.New(msg)
}
