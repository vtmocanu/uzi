package issuetracker

import (
	"fmt"
	"strings"
	"unicode"
)

// closableNames are the normalised state names that mean the work is done. It
// is an allowlist on purpose: isResolved is true for "Won't fix" and
// "Duplicate" too, and a name this list does not know is refused rather than
// guessed at, so a write never closes an issue as something it was not.
var closableNames = map[string]bool{
	"fixed": true, "done": true, "resolved": true, "verified": true,
	"completed": true, "complete": true, "closed": true, "implemented": true,
	"shipped": true, "released": true, "delivered": true, "merged": true,
	"finished": true,
}

// ClosableState reports whether s may be written as the closed state: it is
// resolved, not archived, its name passes stateNameProblem, and its name is one
// of closableNames once normalised. Only Name decides. LocalizedName is display
// text that the same bundle administrator sets freely, so consulting it would
// let a won't-fix state relabelled "Fixed" through; a done state named only in
// another language is therefore refused.
func ClosableState(s State) bool {
	if !s.Resolved || s.Archived || stateNameProblem(s.Name) {
		return false
	}
	return closableNames[normaliseStateName(s.Name)]
}

// OpenableState reports whether s may be written as the open state or as a
// board column's state: it is unresolved, not archived, and its name passes
// stateNameProblem.
func OpenableState(s State) bool {
	return !s.Resolved && !s.Archived && !stateNameProblem(s.Name)
}

// normaliseStateName lower-cases name, trims it and collapses each run of
// whitespace, '-' and '_' to one space.
func normaliseStateName(name string) string {
	fields := strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return unicode.IsSpace(r) || r == '-' || r == '_'
	})
	return strings.Join(fields, " ")
}

// StateNameProblem is stateNameProblem for callers outside the package: a caller that
// stores a State name the driver listed checks it with the same rule.
func StateNameProblem(name string) bool { return stateNameProblem(name) }

// stateNameProblem reports whether a State name read from the issue tracker is
// one the driver will not pass on: blank, over maxLabelNameLen bytes, or
// carrying an invisible or control character, an ignorable code point, a
// variation selector or a blank filler (Hangul and Braille fillers). Any user
// who administers a bundle can name a state, and the name ends up in errors and
// in the board.
func stateNameProblem(name string) bool {
	if strings.TrimSpace(name) == "" || len(name) > maxLabelNameLen {
		return true
	}
	for _, c := range name {
		if InvisibleRune(c) || stateNameFiller(c) {
			return true
		}
	}
	return false
}

// stateNameFiller reports whether c is an Other_Default_Ignorable_Code_Point, a
// variation selector, or one of the fillers that render blank: U+115F, U+1160,
// U+2800, U+3164 and U+FFA0.
// InvisibleRune is shared with other callers and is left as it is.
func stateNameFiller(c rune) bool {
	switch c {
	case 0x115F, 0x1160, 0x2800, 0x3164, 0xFFA0:
		return true
	}
	return unicode.Is(unicode.Other_Default_Ignorable_Code_Point, c) || unicode.Is(unicode.Variation_Selector, c)
}

// maxQuotedNameLen bounds the name quoteName puts in an error.
const maxQuotedNameLen = 80

// quoteName renders a configured state name for an error: cut to
// maxQuotedNameLen bytes and quoted with %q, so no control character survives.
func quoteName(name string) string { return fmt.Sprintf("%q", truncate(name, maxQuotedNameLen)) }
