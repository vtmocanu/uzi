package oauthsrv

import (
	"errors"
	"fmt"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/producttoken"
)

// ParseScopes parses an OAuth scope parameter (RFC 6749 section 3.3): scope-token values
// separated by single spaces. It returns them in order, and fails closed on an empty string, an
// empty element (leading, trailing or doubled space), an unknown scope or a duplicate. Known
// scopes are producttoken.Scopes: a client can never be granted a scope a product token could
// not carry.
func ParseScopes(s string) ([]string, error) {
	if s == "" {
		return nil, errors.New("scope must not be empty")
	}
	return ValidateScopes(strings.Split(s, " "))
}

// ValidateScopes checks a scope list the same way ParseScopes does and returns a copy:
// non-empty, no empty element, every element a known scope, no duplicates.
func ValidateScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		return nil, errors.New("scope must name at least one scope")
	}
	out := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		if sc == "" {
			return nil, errors.New("scope must not contain an empty element")
		}
		if !producttoken.ValidScope(sc) {
			return nil, fmt.Errorf("unknown scope %q (known: %s)", truncateForError(sc), strings.Join(producttoken.Scopes, ", "))
		}
		for _, have := range out {
			if have == sc {
				return nil, fmt.Errorf("duplicate scope %q", sc)
			}
		}
		out = append(out, sc)
	}
	return out, nil
}

// truncateForError bounds an untrusted value echoed into an error message.
func truncateForError(s string) string {
	const max = 64
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
