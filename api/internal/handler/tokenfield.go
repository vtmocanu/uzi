package handler

import (
	"encoding/json"
	"strings"
)

// tokenField decodes the three-way "which Anthropic token" body field shared by
// PATCH /api/workers/{id} and PUT /api/me/judge (PRD #104 M3/M4).
//
// A plain *string cannot express what these routes need. JSON `null` and an omitted
// key both decode to a nil *string, so "clear the binding" and "leave it alone"
// become the same request — and they must not be. Every pre-M4 client sends
// {"enabled":true} with no token key at all; if that meant "clear", enabling the
// judge would silently unbind the user's judge credential. json.RawMessage keeps
// absence observable, because an omitted key leaves it empty while an explicit null
// arrives as the four bytes "null".
//
//	omitted        → leave the binding exactly as it is
//	null or ""     → clear it; the user falls back to their default token
//	"some-label"   → bind to the token with that label
//
// An all-whitespace label is a clear too, not a lookup: "" is what a shell or a form
// sends for an omitted value, and 400-ing on it would reject something the caller
// plainly meant as "none".
type tokenField struct {
	// present is false when the key was omitted. Callers MUST check it before acting
	// — that check is the whole reason this type exists.
	present bool
	// label is "" for the clear form, else the trimmed label to resolve.
	label string
}

// parseTokenField interprets one raw JSON value as the three-way field. A malformed
// value (a number, an object) reports ok=false so the handler can 400 rather than
// silently treating it as absent.
func parseTokenField(raw json.RawMessage) (tokenField, bool) {
	if len(raw) == 0 {
		return tokenField{}, true // omitted
	}
	if string(raw) == "null" {
		return tokenField{present: true}, true // explicit clear
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return tokenField{}, false
	}
	return tokenField{present: true, label: strings.TrimSpace(s)}, true
}
