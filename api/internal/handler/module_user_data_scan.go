package handler

import (
	"bytes"
	"encoding/json"
	"strconv"
	"unicode/utf8"
)

// maxModuleDocDepth is the deepest array/object nesting a stored value may have,
// counted relative to the value (the wrapper object is not counted). It matches the
// stdlib's own limit, so json.Valid on a value span accepts exactly what we allow.
const maxModuleDocDepth = 10000

// moduleDocError is a client-input failure; the handler answers it 400 with Msg.
type moduleDocError struct{ Msg string }

func (e *moduleDocError) Error() string { return e.Msg }

func errBody() error { return &moduleDocError{"invalid request body"} }
func errData() error { return &moduleDocError{"invalid data"} }

// span is a half-open byte range of the original request body.
type span struct{ start, end int }

// docMember is one member of the wrapper object: its key span (quotes included),
// its value span, and every number literal found in the value outside strings.
type docMember struct {
	key, val span
	nums     []span
}

// scannedModuleDoc is a fully validated wrapper.
type scannedModuleDoc struct {
	// data is the outgoing document: the winning data value with zero-valued number
	// literals rewritten to 0.
	data []byte
	// expectedRevision is nil when expected_revision is null.
	expectedRevision *string
}

func isJSONWS(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func skipWS(b []byte, i int) int {
	for i < len(b) && isJSONWS(b[i]) {
		i++
	}
	return i
}

// skipString returns the index just past the closing quote of the string opening at
// b[i]. Escapes are skipped as a pair; validity of the contents is left to json.Valid.
func skipString(b []byte, i int) (int, bool) {
	for j := i + 1; j < len(b); {
		switch b[j] {
		case '\\':
			j += 2
		case '"':
			return j + 1, true
		default:
			j++
		}
	}
	return 0, false
}

func isScalarByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '+' || c == '-' || c == '.'
}

func isNumberByte(c byte) bool {
	return c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.' || c == 'e' || c == 'E'
}

// scanValue finds the value starting at b[i]: its end offset and the number
// literals inside it. One linear pass, an explicit depth counter, no recursion.
func scanValue(b []byte, i int) (end int, nums []span, err error) {
	if i >= len(b) {
		return 0, nil, errBody()
	}
	switch c := b[i]; {
	case c == '"':
		j, ok := skipString(b, i)
		if !ok {
			return 0, nil, errBody()
		}
		return j, nil, nil
	case c == '[' || c == '{':
		depth := 0
		for j := i; j < len(b); {
			switch ch := b[j]; {
			case ch == '"':
				k, ok := skipString(b, j)
				if !ok {
					return 0, nil, errBody()
				}
				j = k
			case ch == '[' || ch == '{':
				depth++
				if depth > maxModuleDocDepth {
					return 0, nil, errData()
				}
				j++
			case ch == ']' || ch == '}':
				depth--
				j++
				if depth == 0 {
					return j, nums, nil
				}
			case ch == '-' || ch >= '0' && ch <= '9':
				k := j
				for k < len(b) && isNumberByte(b[k]) {
					k++
				}
				nums = append(nums, span{j, k})
				j = k
			default:
				j++
			}
		}
		return 0, nil, errBody()
	case isScalarByte(c):
		j := i
		for j < len(b) && isScalarByte(b[j]) {
			j++
		}
		if c == '-' || c >= '0' && c <= '9' {
			nums = append(nums, span{i, j})
		}
		return j, nums, nil
	}
	return 0, nil, errBody()
}

// scanWrapper tokenizes the wrapper object {"key":value,...} and validates every key
// and value span with the stdlib. It accepts exactly the bodies json.Valid accepts
// whose top level is an object (the depth limit applies to the value, not the wrapper).
func scanWrapper(b []byte) ([]docMember, error) {
	i := skipWS(b, 0)
	if i >= len(b) || b[i] != '{' {
		return nil, errBody()
	}
	i = skipWS(b, i+1)
	var members []docMember
	if i < len(b) && b[i] == '}' {
		i++
	} else {
		for {
			if i >= len(b) || b[i] != '"' {
				return nil, errBody()
			}
			ke, ok := skipString(b, i)
			if !ok {
				return nil, errBody()
			}
			m := docMember{key: span{i, ke}}
			i = skipWS(b, ke)
			if i >= len(b) || b[i] != ':' {
				return nil, errBody()
			}
			i = skipWS(b, i+1)
			ve, nums, err := scanValue(b, i)
			if err != nil {
				return nil, err
			}
			m.val, m.nums = span{i, ve}, nums
			members = append(members, m)
			i = skipWS(b, ve)
			if i >= len(b) {
				return nil, errBody()
			}
			if b[i] == ',' {
				i = skipWS(b, i+1)
				continue
			}
			if b[i] == '}' {
				i++
				break
			}
			return nil, errBody()
		}
	}
	if skipWS(b, i) != len(b) {
		return nil, errBody()
	}
	for _, m := range members {
		if !json.Valid(b[m.key.start:m.key.end]) || !json.Valid(b[m.val.start:m.val.end]) {
			return nil, errBody()
		}
	}
	return members, nil
}

// decodeKey decodes a key span into its string value.
func decodeKey(b []byte, s span) (string, error) {
	var k string
	if err := json.Unmarshal(b[s.start:s.end], &k); err != nil {
		return "", errBody()
	}
	return k, nil
}

// scanModuleDoc validates a PUT body {"data":<json>,"expected_revision":<string|null>}
// and builds the outgoing data bytes.
func scanModuleDoc(b []byte) (*scannedModuleDoc, error) {
	// json.Valid accepts invalid UTF-8 inside strings and Unmarshal would silently
	// replace it in expected_revision, so reject it anywhere in the body up front.
	if !utf8.Valid(b) {
		return nil, errBody()
	}
	members, err := scanWrapper(b)
	if err != nil {
		return nil, err
	}
	dataIdx, revIdx := -1, -1
	for idx, m := range members {
		k, err := decodeKey(b, m.key)
		if err != nil {
			return nil, err
		}
		switch k {
		case "data":
			dataIdx = idx
		case "expected_revision":
			revIdx = idx
		default:
			return nil, errBody()
		}
	}
	if dataIdx < 0 || revIdx < 0 {
		return nil, errBody()
	}
	d := members[dataIdx]
	dataSpan := b[d.val.start:d.val.end]
	if bytes.Equal(dataSpan, []byte("null")) {
		return nil, errData()
	}
	out := &scannedModuleDoc{}
	rs := b[members[revIdx].val.start:members[revIdx].val.end]
	if !bytes.Equal(rs, []byte("null")) {
		var s string
		if err := json.Unmarshal(rs, &s); err != nil {
			return nil, errBody()
		}
		out.expectedRevision = &s
	}

	// Only the winning data value's numbers are checked.
	var zeros []span
	for _, n := range d.nums {
		lit := string(b[n.start:n.end])
		f, err := strconv.ParseFloat(lit, 64)
		if err != nil {
			return nil, errData() // out of float64 range
		}
		if f != 0 {
			continue
		}
		mant := lit
		if e := bytes.IndexAny([]byte(lit), "eE"); e >= 0 {
			mant = lit[:e]
		}
		if bytes.ContainsAny([]byte(mant), "123456789") {
			return nil, errData() // underflow to zero
		}
		if lit != "0" {
			zeros = append(zeros, n)
		}
	}
	buf := make([]byte, 0, len(dataSpan))
	pos := d.val.start
	for _, z := range zeros {
		buf = append(buf, b[pos:z.start]...)
		buf = append(buf, '0')
		pos = z.end
	}
	buf = append(buf, b[pos:d.val.end]...)
	out.data = buf
	return out, nil
}
