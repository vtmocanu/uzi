package issuetracker

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

const (
	// redactPlaceholder replaces any occurrence of the token in a message.
	redactPlaceholder = "[REDACTED]"
	// maxMessageBytes bounds a returned error message.
	maxMessageBytes = 512
	// minTokenLen is the shortest token a constructor accepts. A shorter secret
	// could not be scrubbed without mangling unrelated text.
	minTokenLen = 16
	// maxTokenLen is the longest token a constructor accepts.
	maxTokenLen = 4096
	// minSegmentLen is the shortest dot-separated token segment scrubbed alone.
	minSegmentLen = 8
)

// redactor scrubs one token out of every error a driver returns. Build it from
// the token once, in the constructor.
type redactor struct {
	secrets []string
}

// newRedactor lists the token's raw, query-escaped, path-escaped and
// JSON-escaped forms (percent-hex in both cases, JSON with and without "\/"),
// plus the same escaped forms of its final '.'-separated segment when long
// enough (a permanent token has the shape prefix:id.secret). The token's own
// forms are always listed whatever its length; minSegmentLen applies to the
// segment only. The list has no duplicates.
func newRedactor(token string) redactor {
	var secrets []string
	seen := map[string]bool{}
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			secrets = append(secrets, s)
		}
	}
	addForms := func(v string) {
		add(v)
		q, p := url.QueryEscape(v), url.PathEscape(v)
		add(q)
		add(lowerPercent(q))
		add(p)
		add(lowerPercent(p))
		if b, err := json.Marshal(v); err == nil && len(b) >= 2 {
			j := string(b[1 : len(b)-1])
			add(j)
			add(strings.ReplaceAll(j, "/", `\/`))
		}
	}
	addForms(token)
	if i := strings.LastIndexByte(token, '.'); i >= 0 && len(token)-i-1 >= minSegmentLen {
		addForms(token[i+1:])
	}
	return redactor{secrets: secrets}
}

// lowerPercent lower-cases the hex digits of every %XX escape in s.
func lowerPercent(s string) string {
	b := []byte(s)
	for i := 0; i+2 < len(b); i++ {
		if b[i] == '%' {
			b[i+1] = lowerHex(b[i+1])
			b[i+2] = lowerHex(b[i+2])
			i += 2
		}
	}
	return string(b)
}

// upperPercent upper-cases the hex digits of every %XX escape in s.
func upperPercent(s string) string {
	b := []byte(s)
	for i := 0; i+2 < len(b); i++ {
		if b[i] == '%' {
			b[i+1] = upperHex(b[i+1])
			b[i+2] = upperHex(b[i+2])
			i += 2
		}
	}
	return string(b)
}

func upperHex(c byte) byte {
	if c >= 'a' && c <= 'f' {
		return c - ('a' - 'A')
	}
	return c
}

func lowerHex(c byte) byte {
	if c >= 'A' && c <= 'F' {
		return c + ('a' - 'A')
	}
	return c
}

// scrub replaces every listed secret form in s. A second pass runs over a copy
// whose %XX escapes are upper-cased, so a mixed-case escape such as one that
// reads "%2f...%2F" is caught too; the copy is used only when it matched.
func (r redactor) scrub(s string) string {
	out := r.scrubForms(s)
	canon := upperPercent(out)
	if c := r.scrubForms(canon); c != canon {
		return c
	}
	return out
}

func (r redactor) scrubForms(s string) string {
	for _, secret := range r.secrets {
		s = strings.ReplaceAll(s, secret, redactPlaceholder)
	}
	return s
}

// string scrubs secrets, strips control and format characters, scrubs again
// (stripping can join fragments of a secret), and truncates.
func (r redactor) string(s string) string {
	return truncate(r.scrub(sanitize(r.scrub(s))), maxMessageBytes)
}

// sanitize replaces control characters, Unicode format characters (bidi
// controls, zero-width characters, BOM) and the line and paragraph separators
// with a space. It uses the predicate of issueinput.AuthorName.
func sanitize(s string) string {
	return strings.Map(func(c rune) rune {
		if InvisibleRune(c) {
			return ' '
		}
		return c
	}, s)
}

// truncate cuts s to at most limit bytes on a UTF-8 boundary.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	s = s[:limit]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// classifiedError carries a scrubbed message and a normalized class. It has no
// Unwrap on purpose: the original error may contain the token.
type classifiedError struct {
	message string
	class   forge.ErrorClass
}

func (e *classifiedError) Error() string           { return e.message }
func (e *classifiedError) Class() forge.ErrorClass { return e.class }

// sentinelError is a redacted error that stays identifiable: errors.Is matches
// the fixed sentinel and nothing else, because Unwrap returns only the
// sentinel. Use it only for a sentinel that carries no secret.
type sentinelError struct {
	classifiedError
	sentinel error
}

func (e *sentinelError) Unwrap() error { return e.sentinel }

// error returns err as a scrubbed, classified error, or nil for nil. A forge
// oversize error passes through as a fresh copy so its provenance survives.
func (r redactor) error(err error) error {
	return r.errorStatus(err, 0)
}

// errorStatus is error with the HTTP status of the response, when known.
func (r redactor) errorStatus(err error, status int) error {
	if err == nil {
		return nil
	}
	var large *forge.ResponseTooLargeError
	if errors.As(err, &large) {
		outcome := large.Class()
		if outcome == forge.ErrorClassOther {
			outcome = classifyStatus(status)
		}
		return &forge.ResponseTooLargeError{Provenance: large.Provenance, Outcome: outcome}
	}
	if errors.Is(err, forge.ErrResponseTooLarge) {
		return &forge.ResponseTooLargeError{Outcome: classifyStatus(status)}
	}
	return &classifiedError{message: r.string(err.Error()), class: classify(err, status)}
}

// sentinel returns a redacted error for sentinel with extra context in its
// message. errors.Is(result, sentinel) holds; the chain exposes nothing else.
func (r redactor) sentinel(sentinel error, status int, detail string) error {
	if !isPackageSentinel(sentinel) {
		return &classifiedError{message: r.string(sentinel.Error() + detailSuffix(detail)), class: classifyStatus(status)}
	}
	msg := sentinel.Error() + detailSuffix(detail)
	return &sentinelError{
		classifiedError: classifiedError{message: r.string(msg), class: classifyStatus(status)},
		sentinel:        sentinel,
	}
}

func detailSuffix(detail string) string {
	if detail == "" {
		return ""
	}
	return ": " + detail
}

// isPackageSentinel reports whether err is one of this package's fixed
// sentinels; only those may stay in an error chain.
func isPackageSentinel(err error) bool {
	for _, s := range packageSentinels {
		if err == s {
			return true
		}
	}
	return false
}

func classify(err error, status int) forge.ErrorClass {
	if errors.Is(err, context.DeadlineExceeded) {
		return forge.ErrorClassTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return forge.ErrorClassTimeout
	}
	if c := classifyStatus(status); c != forge.ErrorClassOther {
		return c
	}
	return forge.Class(err)
}

func classifyStatus(status int) forge.ErrorClass {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return forge.ErrorClassAuth
	case status == http.StatusTooManyRequests:
		return forge.ErrorClassRateLimited
	case status >= 500 && status < 600:
		return forge.ErrorClassServerError
	}
	return forge.ErrorClassOther
}
