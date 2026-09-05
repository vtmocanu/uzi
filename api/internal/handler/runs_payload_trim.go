package handler

import (
	"encoding/json"
	"unicode/utf8"
)

// runs_payload_trim.go holds trimPayload, the pure ?payload_max= trim applied per
// message on GET /api/runs/{id}/messages (PRD #1137 M2). It shrinks the two bulky
// payload fields the TUI never draws in full — tool_result.content and tool_use.input
// string values — while preserving every other key byte-for-byte and leaving the
// three identity keys runactivity.FromFrame reads (subagent_type, description,
// file_path) untouched. Output is always valid JSON; a malformed payload is returned
// verbatim so a model-authored payload never fails the request.

// trimIdentityKeys are the tool_use.input string keys that are NEVER cut:
// runactivity.FromFrame reads them verbatim for the "now" line (see
// api/internal/runactivity/runactivity.go), so cutting them would corrupt the TUI's
// now-line agent/detail.
var trimIdentityKeys = map[string]bool{
	"subagent_type": true,
	"description":   true,
	"file_path":     true,
}

// trimPayload trims the bulky payload fields of a tool_result / tool_use message to
// at most max bytes each, returning the (possibly rewritten) payload and whether any
// bytes were actually removed. Every other kind, and any payload that fails to
// unmarshal as a JSON object, is returned unchanged with truncated=false. When
// nothing is trimmed the ORIGINAL raw is returned byte-identical.
func trimPayload(raw json.RawMessage, kind string, max int) (out json.RawMessage, truncated bool) {
	if kind != "tool_result" && kind != "tool_use" {
		return raw, false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		// Malformed or non-object payload: leave it exactly as the worker persisted it.
		return raw, false
	}

	var changed bool
	switch kind {
	case "tool_result":
		changed = trimToolResult(obj, max)
	case "tool_use":
		changed = trimToolUse(obj, max)
	}
	if !changed {
		return raw, false
	}
	// Re-marshal the map. map[string]json.RawMessage re-marshals with sorted keys,
	// which is valid JSON — the contract is shape-not-order.
	reencoded, err := json.Marshal(obj)
	if err != nil {
		// Defensive: a RawMessage we assembled ourselves should always re-marshal.
		return raw, false
	}
	return reencoded, true
}

// trimToolResult trims the "content" key of a tool_result payload in place. It reports
// whether any bytes were removed. Every other key is left untouched.
func trimToolResult(obj map[string]json.RawMessage, max int) bool {
	rawContent, ok := obj["content"]
	if !ok {
		return false
	}
	// content as a JSON string.
	var s string
	if err := json.Unmarshal(rawContent, &s); err == nil {
		cut, did := cutRunes(s, max)
		if !did {
			return false
		}
		obj["content"] = mustMarshalString(cut + "…")
		return true
	}
	// content as a JSON array of SDK content blocks.
	var blocks []json.RawMessage
	if err := json.Unmarshal(rawContent, &blocks); err == nil {
		trimmed, did := trimContentBlocks(blocks, max)
		if !did {
			return false
		}
		obj["content"] = trimmed
		return true
	}
	// content is neither string nor array (or some other shape): leave it.
	return false
}

// trimContentBlocks keeps only {"type":"text"} blocks and cuts the cumulative text
// across kept blocks to at most max bytes total. It returns the re-marshalled array
// and whether any block was dropped or any text cut.
func trimContentBlocks(blocks []json.RawMessage, max int) (json.RawMessage, bool) {
	kept := make([]json.RawMessage, 0, len(blocks))
	dropped := false
	remaining := max
	cutAny := false
	for _, b := range blocks {
		var block map[string]json.RawMessage
		if err := json.Unmarshal(b, &block); err != nil {
			// A non-object block is not a text block: drop it.
			dropped = true
			continue
		}
		var typ string
		if rawType, ok := block["type"]; ok {
			_ = json.Unmarshal(rawType, &typ)
		}
		if typ != "text" {
			dropped = true
			continue
		}
		var text string
		if rawText, ok := block["text"]; ok {
			if err := json.Unmarshal(rawText, &text); err != nil {
				text = ""
			}
		}
		cut, did := cutRunes(text, remaining)
		if did {
			cutAny = true
			cut += "…"
		}
		remaining -= len(cut)
		if remaining < 0 {
			remaining = 0
		}
		block["text"] = mustMarshalString(cut)
		reblock, err := json.Marshal(block)
		if err != nil {
			return nil, false
		}
		kept = append(kept, reblock)
	}
	if !dropped && !cutAny {
		return nil, false
	}
	out, err := json.Marshal(kept)
	if err != nil {
		return nil, false
	}
	return out, true
}

// trimToolUse trims the string values of a tool_use payload's "input" object in place,
// except the three identity keys. It reports whether any bytes were removed. Every
// other top-level key of the payload is left untouched.
func trimToolUse(obj map[string]json.RawMessage, max int) bool {
	rawInput, ok := obj["input"]
	if !ok {
		return false
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal(rawInput, &input); err != nil {
		// input is not an object: leave it.
		return false
	}
	changed := false
	for k, v := range input {
		if trimIdentityKeys[k] {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			// Non-string value (number, object, array, bool): verbatim.
			continue
		}
		cut, did := cutRunes(s, max)
		if !did {
			continue
		}
		input[k] = mustMarshalString(cut + "…")
		changed = true
	}
	if !changed {
		return false
	}
	reinput, err := json.Marshal(input)
	if err != nil {
		return false
	}
	obj["input"] = reinput
	return true
}

// cutRunes returns the rune-aligned prefix of s that is at most max bytes, and whether
// it cut anything. A multibyte rune straddling the byte cut is dropped whole so the
// result is always valid UTF-8.
func cutRunes(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	if max <= 0 {
		return "", true
	}
	// Walk back from max to the nearest rune boundary so a straddling rune is dropped.
	end := max
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end], true
}

// mustMarshalString marshals a Go string to a JSON string RawMessage. json.Marshal of
// a string cannot fail, so the error is discarded.
func mustMarshalString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}
