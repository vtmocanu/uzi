package handler

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/runactivity"
)

// TestTrimPayloadCutRunes covers the rune-aligned prefix helper directly: a cut that
// lands mid-rune drops the straddling rune whole, so the output is always valid UTF-8.
func TestTrimPayloadCutRunes(t *testing.T) {
	// "héllo" — é is 2 bytes (0xC3 0xA9), so byte offsets: h=0, é=1..2, l=3, l=4, o=5.
	cases := []struct {
		name    string
		s       string
		max     int
		want    string
		wantCut bool
	}{
		{"under max no cut", "abc", 10, "abc", false},
		{"exact fit no cut", "abc", 3, "abc", false},
		{"ascii cut", "abcdef", 3, "abc", true},
		{"max zero", "abc", 0, "", true},
		{"multibyte straddle dropped", "héllo", 2, "h", true}, // cut at byte 2 is mid-é -> drop é
		{"multibyte boundary kept", "héllo", 3, "hé", true},   // byte 3 is a rune start (l)
		{"emoji straddle dropped", "a😀b", 3, "a", true},       // 😀 is 4 bytes at offset 1..4
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, cut := cutRunes(tc.s, tc.max)
			if got != tc.want || cut != tc.wantCut {
				t.Fatalf("cutRunes(%q, %d) = (%q, %v), want (%q, %v)", tc.s, tc.max, got, cut, tc.want, tc.wantCut)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("cutRunes(%q, %d) produced invalid UTF-8 %q", tc.s, tc.max, got)
			}
		})
	}
}

// TestTrimPayload is the table-driven core: each case names a kind, an input payload,
// a max, whether trimming is expected, and an assertion over the output. Every output
// must be valid JSON and valid UTF-8.
func TestTrimPayload(t *testing.T) {
	const max = 8

	cases := []struct {
		name          string
		kind          string
		in            string
		max           int
		wantTruncated bool
		// wantVerbatim asserts the output bytes equal the input bytes (nothing trimmed).
		wantVerbatim bool
		check        func(t *testing.T, obj map[string]json.RawMessage)
	}{
		{
			name:          "tool_result string content cut",
			kind:          "tool_result",
			in:            `{"content":"abcdefghijklmnop","tool_use_id":"tu_1","is_error":false}`,
			max:           max,
			wantTruncated: true,
			check: func(t *testing.T, obj map[string]json.RawMessage) {
				var content string
				mustUnmarshal(t, obj["content"], &content)
				if content != "abcdefgh…" {
					t.Fatalf("content = %q, want %q", content, "abcdefgh…")
				}
				// Other keys verbatim.
				assertRawEquals(t, obj["tool_use_id"], `"tu_1"`)
				assertRawEquals(t, obj["is_error"], `false`)
			},
		},
		{
			name:          "tool_result block array drops image keeps text cumulative",
			kind:          "tool_result",
			in:            `{"content":[{"type":"text","text":"hello world"},{"type":"image","source":{"data":"AAAABBBBCCCC"}},{"type":"text","text":"more text here"}],"tool_use_id":"tu_2"}`,
			max:           max,
			wantTruncated: true,
			check: func(t *testing.T, obj map[string]json.RawMessage) {
				var blocks []map[string]json.RawMessage
				mustUnmarshal(t, obj["content"], &blocks)
				if len(blocks) != 2 {
					t.Fatalf("kept %d blocks, want 2 (image dropped)", len(blocks))
				}
				for _, b := range blocks {
					var typ string
					mustUnmarshal(t, b["type"], &typ)
					if typ != "text" {
						t.Fatalf("kept a non-text block: %s", b["type"])
					}
				}
				// First block: "hello world" cut to 8 bytes at a rune boundary + ellipsis.
				var t0 string
				mustUnmarshal(t, blocks[0]["text"], &t0)
				if t0 != "hello wo…" {
					t.Fatalf("block0 text = %q, want %q", t0, "hello wo…")
				}
				assertRawEquals(t, obj["tool_use_id"], `"tu_2"`)
			},
		},
		{
			// A malformed text block (text is not a JSON string) must be preserved
			// verbatim even when a sibling block triggers a re-marshal of the array —
			// the trim only ever rewrites valid string content, never corrupts it to "".
			name:          "tool_result block array preserves non-string text block",
			kind:          "tool_result",
			in:            `{"content":[{"type":"text","text":42},{"type":"image","source":"x"}],"tool_use_id":"tu_5"}`,
			max:           max,
			wantTruncated: true, // the image block is dropped, so the array is re-marshalled
			check: func(t *testing.T, obj map[string]json.RawMessage) {
				var blocks []map[string]json.RawMessage
				mustUnmarshal(t, obj["content"], &blocks)
				if len(blocks) != 1 {
					t.Fatalf("kept %d blocks, want 1 (image dropped, non-string text kept)", len(blocks))
				}
				assertRawEquals(t, blocks[0]["text"], `42`)
			},
		},
		{
			name:          "tool_result content already short not trimmed",
			kind:          "tool_result",
			in:            `{"content":"short","tool_use_id":"tu_3"}`,
			max:           max,
			wantTruncated: false,
			wantVerbatim:  true,
		},
		{
			name:          "tool_use input string cut at rune boundary multibyte",
			kind:          "tool_use",
			in:            `{"id":"t1","name":"Bash","input":{"command":"héllo wörld here"},"usage":{"input_tokens":5}}`,
			max:           4, // "héllo..." -> h(1)é(2)l... cut at 4 bytes lands after 'l'? h=0,é=1-2,l=3 -> byte4 is second l -> "hél"
			wantTruncated: true,
			check: func(t *testing.T, obj map[string]json.RawMessage) {
				var input map[string]json.RawMessage
				mustUnmarshal(t, obj["input"], &input)
				var cmd string
				mustUnmarshal(t, input["command"], &cmd)
				if !utf8.ValidString(cmd) {
					t.Fatalf("command not valid UTF-8: %q", cmd)
				}
				if !strings.HasSuffix(cmd, "…") {
					t.Fatalf("command %q missing ellipsis", cmd)
				}
				// id, name, usage verbatim.
				assertRawEquals(t, obj["id"], `"t1"`)
				assertRawEquals(t, obj["name"], `"Bash"`)
				assertRawEquals(t, obj["usage"], `{"input_tokens":5}`)
			},
		},
		{
			name:          "tool_use identity keys never cut",
			kind:          "tool_use",
			in:            `{"name":"Agent","input":{"subagent_type":"a-really-long-subagent-type","description":"a really long description string","file_path":"a/really/long/file/path/here.go","prompt":"a really long prompt string"}}`,
			max:           4,
			wantTruncated: true, // prompt is cut
			check: func(t *testing.T, obj map[string]json.RawMessage) {
				var input map[string]json.RawMessage
				mustUnmarshal(t, obj["input"], &input)
				assertRawEquals(t, input["subagent_type"], `"a-really-long-subagent-type"`)
				assertRawEquals(t, input["description"], `"a really long description string"`)
				assertRawEquals(t, input["file_path"], `"a/really/long/file/path/here.go"`)
				var prompt string
				mustUnmarshal(t, input["prompt"], &prompt)
				if !strings.HasSuffix(prompt, "…") {
					t.Fatalf("prompt %q should have been cut", prompt)
				}
			},
		},
		{
			name:          "tool_use non-string input values untouched",
			kind:          "tool_use",
			in:            `{"name":"Foo","input":{"count":123456789,"flag":true,"obj":{"deep":"a really long nested string value"},"arr":[1,2,3,4,5,6,7,8,9]}}`,
			max:           4,
			wantTruncated: false,
			wantVerbatim:  true,
		},
		{
			name:          "tool_use context object preserved",
			kind:          "tool_use",
			in:            `{"name":"Foo","input":{"x":"a very long string that is trimmed"},"context":{"window":200000,"used":1234}}`,
			max:           4,
			wantTruncated: true,
			check: func(t *testing.T, obj map[string]json.RawMessage) {
				assertRawEquals(t, obj["context"], `{"window":200000,"used":1234}`)
			},
		},
		{
			name:          "text kind never trimmed",
			kind:          "text",
			in:            `{"text":"a very long body of text that would exceed the cap but is drawn in full","usage":{"input_tokens":9}}`,
			max:           4,
			wantTruncated: false,
			wantVerbatim:  true,
		},
		{
			name:          "thinking kind never trimmed",
			kind:          "thinking",
			in:            `{"text":"a very long thinking body that exceeds the cap"}`,
			max:           4,
			wantTruncated: false,
			wantVerbatim:  true,
		},
		{
			name:          "plan kind never trimmed",
			kind:          "plan",
			in:            `{"plan":"a very long plan body that exceeds the cap by a lot"}`,
			max:           4,
			wantTruncated: false,
			wantVerbatim:  true,
		},
		{
			name:          "status kind preserves usage never trimmed",
			kind:          "status",
			in:            `{"status":"running","usage":{"input_tokens":11}}`,
			max:           4,
			wantTruncated: false,
			wantVerbatim:  true,
		},
		{
			name:          "finding kind never trimmed",
			kind:          "finding",
			in:            `{"summary":"a very long finding summary that exceeds the cap"}`,
			max:           4,
			wantTruncated: false,
			wantVerbatim:  true,
		},
		{
			name:          "malformed payload returned verbatim",
			kind:          "tool_result",
			in:            `not json at all {{{`,
			max:           4,
			wantTruncated: false,
			wantVerbatim:  true,
		},
		{
			name:          "non-object payload returned verbatim",
			kind:          "tool_use",
			in:            `["an","array","payload"]`,
			max:           4,
			wantTruncated: false,
			wantVerbatim:  true,
		},
		{
			name:          "tool_result no content key untouched",
			kind:          "tool_result",
			in:            `{"tool_use_id":"tu_4","is_error":true}`,
			max:           4,
			wantTruncated: false,
			wantVerbatim:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.max
			if m == 0 {
				m = max
			}
			out, truncated := trimPayload(json.RawMessage(tc.in), tc.kind, m)
			if truncated != tc.wantTruncated {
				t.Fatalf("truncated = %v, want %v (out=%s)", truncated, tc.wantTruncated, out)
			}
			if tc.wantVerbatim {
				if string(out) != tc.in {
					t.Fatalf("expected verbatim output; got %s want %s", out, tc.in)
				}
			}
			// The "always valid JSON" invariant holds for any well-formed input; a
			// malformed payload is returned verbatim and stays malformed by design, so
			// only assert validity when the input itself was valid JSON.
			if json.Valid([]byte(tc.in)) && !json.Valid(out) {
				t.Fatalf("output is not valid JSON: %s", out)
			}
			if !utf8.Valid(out) {
				t.Fatalf("output is not valid UTF-8: %s", out)
			}
			if tc.check != nil {
				var obj map[string]json.RawMessage
				if err := json.Unmarshal(out, &obj); err != nil {
					t.Fatalf("output not an object: %v (%s)", err, out)
				}
				tc.check(t, obj)
			}
		})
	}
}

// TestTrimPayloadOtherKinds asserts every non tool_result / tool_use kind is returned
// byte-identical regardless of size.
func TestTrimPayloadOtherKinds(t *testing.T) {
	for _, kind := range []string{"text", "thinking", "plan", "status", "finding", "answer", ""} {
		in := json.RawMessage(`{"body":"a very long body that vastly exceeds any cap we would set here"}`)
		out, truncated := trimPayload(in, kind, 4)
		if truncated {
			t.Fatalf("kind %q was truncated", kind)
		}
		if string(out) != string(in) {
			t.Fatalf("kind %q not verbatim: %s", kind, out)
		}
	}
}

// TestTrimPayloadNowLineParity is the invariant the exempt identity keys exist for: a
// trimmed tool_use fed to runactivity.FromFrame yields the same Agent/AgentLabel/Detail
// as the untrimmed one, because subagent_type/description/file_path are never cut.
func TestTrimPayloadNowLineParity(t *testing.T) {
	longDesc := "dispatch a subagent to do a very long task described at great length here"
	full := json.RawMessage(`{"name":"Agent","input":{` +
		`"subagent_type":"reviewer",` +
		`"description":"` + longDesc + `",` +
		`"prompt":"a very long prompt body that the trim will shorten dramatically"}}`)

	trimmed, truncated := trimPayload(full, "tool_use", 8)
	if !truncated {
		t.Fatalf("expected the tool_use prompt to be trimmed")
	}

	agent := "lead"
	label := "orig-label"
	at := time.Unix(1700000000, 0).UTC()
	fromFull := runactivity.FromFrame("tool_use", &agent, &label, full, at, 1)
	fromTrimmed := runactivity.FromFrame("tool_use", &agent, &label, trimmed, at, 1)

	if fromFull.Agent != fromTrimmed.Agent {
		t.Fatalf("Agent differs: full=%q trimmed=%q", fromFull.Agent, fromTrimmed.Agent)
	}
	if fromFull.AgentLabel != fromTrimmed.AgentLabel {
		t.Fatalf("AgentLabel differs: full=%q trimmed=%q", fromFull.AgentLabel, fromTrimmed.AgentLabel)
	}
	if fromFull.Detail != fromTrimmed.Detail {
		t.Fatalf("Detail differs: full=%q trimmed=%q", fromFull.Detail, fromTrimmed.Detail)
	}
	// Sanity: the exempt keys really are the ones FromFrame folded in (subagent_type
	// -> Agent, description -> Detail), so the parity above is meaningful, not vacuous.
	if fromFull.Agent != "reviewer" || fromFull.Detail != longDesc {
		t.Fatalf("unexpected now-line fold: agent=%q detail=%q", fromFull.Agent, fromFull.Detail)
	}
}

func mustUnmarshal(t *testing.T, raw json.RawMessage, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
}

func assertRawEquals(t *testing.T, raw json.RawMessage, want string) {
	t.Helper()
	if strings.TrimSpace(string(raw)) != want {
		t.Fatalf("raw = %s, want %s", raw, want)
	}
}
