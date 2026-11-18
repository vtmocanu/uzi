package handler

// hasLoneSurrogateEscape reports whether a JSON body holds a \uD800-\uDFFF escape that
// is not part of a high-then-low pair. encoding/json turns such an escape into U+FFFD,
// so the stored value would differ from what the caller sent. The body must already be
// valid JSON: every backslash then begins an escape, so a single linear walk that skips
// the byte after a non-u backslash never mistakes an escaped backslash for an escape.
func hasLoneSurrogateEscape(raw []byte) bool {
	// u16 reads the 4 hex digits of a \uXXXX escape whose backslash is at i.
	u16 := func(i int) (uint16, bool) {
		if i+6 > len(raw) || raw[i] != '\\' || raw[i+1] != 'u' {
			return 0, false
		}
		var v uint16
		for _, c := range raw[i+2 : i+6] {
			switch {
			case c >= '0' && c <= '9':
				v = v<<4 | uint16(c-'0')
			case c >= 'a' && c <= 'f':
				v = v<<4 | uint16(c-'a'+10)
			case c >= 'A' && c <= 'F':
				v = v<<4 | uint16(c-'A'+10)
			default:
				return 0, false
			}
		}
		return v, true
	}
	for i := 0; i < len(raw); {
		if raw[i] != '\\' {
			i++
			continue
		}
		if i+1 >= len(raw) || raw[i+1] != 'u' {
			i += 2
			continue
		}
		v, ok := u16(i)
		if !ok {
			i += 2
			continue
		}
		switch {
		case v >= 0xDC00 && v <= 0xDFFF:
			return true // a low surrogate with no high one before it
		case v >= 0xD800 && v <= 0xDBFF:
			lo, ok := u16(i + 6)
			if !ok || lo < 0xDC00 || lo > 0xDFFF {
				return true
			}
			i += 12
		default:
			i += 6
		}
	}
	return false
}
