package issuetracker

import (
	"encoding/json"
	"errors"
	"io"
)

// maxNestedItems caps the tags of one issue or one activity.
const maxNestedItems = 1000

// errNestedCeiling is returned when one object carries more than maxNestedItems
// nested entries.
var errNestedCeiling = errors.New("issuetracker: an item carries more nested entries than the ceiling allows")

// Structural budgets, applied by a byte scan before any response is handed to
// json.Unmarshal. A value is counted as one comma or one opening bracket
// outside a string, which approximates the number of JSON values (at most
// 2*count+1). Real YouTrack adds a "$type" member to every entity, so a tag
// with $type, id, name and one more member such as "color" costs 5 (its brace,
// the commas between its four members and the comma after it). An activity
// carries up to maxNestedItems such tags in each of "added" and "removed"
// (10*maxNestedItems values), so one item is allowed 10*maxNestedItems+256. A
// single object (an issue holds one tags array, plus a few members of its own)
// is allowed 6*maxNestedItems+256, which holds 5*maxNestedItems for the tags.
// The real-shaped tests pin that headroom. Three totals are enforced:
// pageValueBudget for a whole list page, itemValueBudget for each element of
// it and objectValueBudget for one single-object response. The budgets bound
// the structure, not the cost of a type error: 13 bytes of bad scalar fit many
// times inside a page budget, and encoding/json keeps decoding after a type
// error, building one error (about 150 bytes) per bad value. So decodePage
// also decodes element by element and stops at the first error, which leaves
// one element, at most itemValueBudget values, to amplify.
const (
	itemValueBudget   = 10*maxNestedItems + 256
	objectValueBudget = 6*maxNestedItems + 256
	pageValueBudget   = pageSize * itemValueBudget
)

// errValueCeiling is returned when a response holds more JSON values than its
// structural budget allows.
var errValueCeiling = errors.New("issuetracker: response holds more values than the budget allows")

// errNotArray is returned when a list response is not a JSON array.
var errNotArray = errors.New("issuetracker: response is not a JSON array")

// errTrailingData is returned when a list response has data after its array.
var errTrailingData = errors.New("issuetracker: response has data after the JSON array")

// errTagNotObject is returned when a tags array holds an element that is not an
// object, refused before decoding so a bad element costs no error value.
var errTagNotObject = errors.New("issuetracker: a tag is not a JSON object")

// ytTags is a nested tag array. Its UnmarshalJSON counts the elements with a
// byte scan and refuses above maxNestedItems before anything is decoded, so one
// object cannot make the decoder allocate a slice as large as the response. It
// builds no json.Decoder: encoding/json calls it once per repeated "tags" key,
// and a per-call Decoder would multiply across a response of repeated keys.
type ytTags []ytTag

// UnmarshalJSON implements json.Unmarshaler. b is already-validated JSON.
func (t *ytTags) UnmarshalJSON(b []byte) error {
	// A repeated "tags" key reuses the backing array, so a later key never
	// forces a reallocation per call. encoding/json does not zero reused
	// elements, so clear them first: a field a later key omits must not keep
	// the value an earlier key gave it.
	clear((*t)[:cap(*t)])
	if trimmed := trimJSONSpace(b); string(trimmed) == "null" {
		// null is an empty list.
		*t = (*t)[:0]
		return nil
	}
	if n, _ := topLevelCount(b); n > maxNestedItems {
		return errNestedCeiling
	}
	if !onlyObjects(b) {
		return errTagNotObject
	}
	return json.Unmarshal(b, (*[]ytTag)(t))
}

// topLevelCount scans b, a JSON value, and returns the number of elements of
// the array it starts with (0 when it does not start with an array) and the
// offset just past that array (0 when it does not start with one, or never
// closes). It tracks bracket depth and string state only, so it neither
// validates nor allocates; a caller that needs validity gets it from
// json.Unmarshal afterwards.
func topLevelCount(b []byte) (count, end int) {
	i := 0
	for i < len(b) && isJSONSpace(b[i]) {
		i++
	}
	if i >= len(b) || b[i] != '[' {
		return 0, 0
	}
	depth, seen, inStr, esc := 0, false, false, false
	for ; i < len(b); i++ {
		c := b[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '[', '{':
			if depth == 1 {
				seen = true
			}
			depth++
		case ']', '}':
			depth--
			if depth == 0 {
				if seen {
					count++
				}
				return count, i + 1
			}
		case ',':
			if depth == 1 {
				count++
			}
		case '"':
			if depth == 1 {
				seen = true
			}
			inStr = true
		default:
			if depth == 1 && !isJSONSpace(c) {
				seen = true
			}
		}
	}
	return count, 0
}

// onlyObjects reports whether b, a JSON value, is null or an array whose every
// element is an object or null. It looks only at the first byte of each
// element, so it is meaningful on already-validated JSON; any other first byte
// (a number, string, array, true, false) is refused.
func onlyObjects(b []byte) bool {
	i := 0
	for i < len(b) && isJSONSpace(b[i]) {
		i++
	}
	if i >= len(b) {
		return true
	}
	if b[i] == 'n' {
		return true
	}
	if b[i] != '[' {
		return false
	}
	depth, inStr, esc, want := 0, false, false, false
	for ; i < len(b); i++ {
		c := b[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		if want && !isJSONSpace(c) {
			if c != '{' && c != 'n' && c != ']' {
				return false
			}
			want = false
		}
		switch c {
		case '[', '{':
			depth++
			want = depth == 1
		case ']', '}':
			depth--
			if depth == 0 {
				return true
			}
		case ',':
			want = depth == 1
		case '"':
			inStr = true
		}
	}
	return true
}

// valueCount returns the number of commas and opening brackets outside strings
// in b, at every depth: approximately the number of JSON values (at most
// 2*count+1).
func valueCount(b []byte) int {
	n, inStr, esc := 0, false, false
	for _, c := range b {
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case ',', '[', '{':
			n++
		}
	}
	return n
}

// trimJSONSpace trims JSON's own whitespace (space, tab, CR, LF) from both ends.
func trimJSONSpace(b []byte) []byte {
	for len(b) > 0 && isJSONSpace(b[0]) {
		b = b[1:]
	}
	for len(b) > 0 && isJSONSpace(b[len(b)-1]) {
		b = b[:len(b)-1]
	}
	return b
}

func isJSONSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// decodePage decodes one list page. The driver asked for pageSize items, so a
// longer array is a protocol violation: the elements and the structural values
// are counted by byte scans before any element is decoded, so an oversized page
// allocates nothing for its items. A JSON null is an empty page. Whitespace is
// JSON's own, so a form feed is refused (errNotArray, or errTrailingData after
// the array) rather than skipped as padding, and a form feed inside the array
// is a syntax error. The whole page is validated first,
// then each element is decoded on its own and the first error ends the decode:
// encoding/json would otherwise go on past a type error and build an error per
// bad value, allocating in proportion to the page's bytes.
func decodePage[T any](body []byte) ([]T, error) {
	trimmed := trimJSONSpace(body)
	if len(trimmed) >= 4 && string(trimmed[:4]) == "null" {
		switch {
		case len(trimmed) == 4:
			return nil, nil
		case isJSONSpace(trimmed[4]):
			return nil, errTrailingData
		}
	}
	n, end := topLevelCount(body)
	if end == 0 {
		if len(trimmed) > 0 && trimmed[0] == '[' {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, errNotArray
	}
	if n > pageSize {
		return nil, errItemCeiling
	}
	if len(trimJSONSpace(body[end:])) > 0 {
		return nil, errTrailingData
	}
	if valueCount(body) > pageValueBudget {
		return nil, errValueCeiling
	}
	if !json.Valid(body) {
		// Unmarshal reports the syntax error; it decodes nothing on invalid input.
		var discard []T
		return nil, json.Unmarshal(body, &discard)
	}
	var items []T
	var elemErr error
	forEachElement(body, func(elem []byte) bool {
		if valueCount(elem) > itemValueBudget {
			elemErr = errValueCeiling
			return false
		}
		var item T
		if elemErr = json.Unmarshal(elem, &item); elemErr != nil {
			return false
		}
		items = append(items, item)
		return true
	})
	if elemErr != nil {
		return nil, elemErr
	}
	return items, nil
}

// forEachElement calls f with each top-level element of b, a valid JSON array
// (possibly padded with JSON space), stopping when f returns false. It tracks
// bracket depth and string state only and allocates nothing.
func forEachElement(b []byte, f func(elem []byte) bool) {
	depth, start, inStr, esc := 0, -1, false, false
	emit := func(end int) bool {
		if start < 0 {
			return true
		}
		elem := trimJSONSpace(b[start:end])
		start = -1
		return len(elem) == 0 || f(elem)
	}
	for i, c := range b {
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '[', '{':
			depth++
			if depth == 2 && start < 0 {
				start = i
			}
		case ']', '}':
			depth--
			if depth == 0 {
				emit(i)
				return
			}
		case ',':
			if depth == 1 && !emit(i) {
				return
			}
		case '"':
			inStr = true
			if depth == 1 && start < 0 {
				start = i
			}
		default:
			if depth == 1 && start < 0 && !isJSONSpace(c) {
				start = i
			}
		}
	}
}
