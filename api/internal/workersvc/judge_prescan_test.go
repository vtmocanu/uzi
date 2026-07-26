package workersvc

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// ---------------------------------------------------------------------------------
// PRD #121 M3 — pre-scan accuracy: suppress a ToolMiss the same run later ran green.
//
// 🔴 THE FIXTURE IS AUTHORED, NOT SNAPSHOTTED, AND THAT IS THE POINT. After M1/M2
// land, node_modules is present before the agent's first turn and real traces largely
// stop containing fumbles at all. A fixture captured from a post-M2 run would have
// NOTHING to suppress: it would agree with the implementation on every case it
// covers, read as full coverage, and discriminate nothing. Each case below exists to
// redden for one named reason, and three of them were verified by folding the
// implementation and watching the named case go red.
// ---------------------------------------------------------------------------------

// traceUse builds a tool_use row in the shape the worker's sdk-messages mapper
// persists: {id, name, input}, with the Bash command under input.command.
func traceUse(seq int32, id, command string) store.ListToolTraceForRunRow {
	p, err := json.Marshal(map[string]any{
		"id": id, "name": "Bash", "input": map[string]any{"command": command},
	})
	if err != nil {
		panic(err)
	}
	return store.ListToolTraceForRunRow{Seq: seq, Kind: "tool_use", Payload: p}
}

// traceResult builds a tool_result row: {tool_use_id, content, is_error}. Marshalled
// rather than hand-written so newlines land as the escaped \n a jsonb column really
// holds — the distinction the script-echo channel depends on (it needs decoded text;
// the not-found regexes read the raw payload).
func traceResult(seq int32, toolUseID, content string, isErr bool) store.ListToolTraceForRunRow {
	p, err := json.Marshal(map[string]any{
		"tool_use_id": toolUseID, "content": content, "is_error": isErr,
	})
	if err != nil {
		panic(err)
	}
	return store.ListToolTraceForRunRow{Seq: seq, Kind: "tool_result", Payload: p}
}

// rawResultRows wraps bare payload strings as tool_result rows at seq 1..n — the
// shape the pre-#121 tests passed as [][]byte, kept so those tests still say what
// they always said.
func rawResultRows(payloads ...string) []store.ListToolTraceForRunRow {
	rows := make([]store.ListToolTraceForRunRow, 0, len(payloads))
	for i, p := range payloads {
		rows = append(rows, store.ListToolTraceForRunRow{
			Seq: int32(i + 1), Kind: "tool_result", Payload: []byte(p),
		})
	}
	return rows
}

// npmScriptOutput is what npm echoes before running a script — the header line plus
// the wrapped command. Measured on npm 11.17.0 / node 26.4.0. The header's
// `pkg@1.0.0` token is junk that can never match a miss candidate.
func npmScriptOutput(script, wrapped, tail string) string {
	return "\n> uzi-fixture@1.0.0 " + script + "\n> " + wrapped + "\n\n" + tail
}

// prescan is the production pipeline exactly as judgeSignal composes it.
func prescan(rows []store.ListToolTraceForRunRow) []ToolMiss {
	return suppressResolved(scanCommandNotFound(rows), observedGreenTools(rows))
}

func reportedCommands(misses []ToolMiss) []string {
	out := make([]string, 0, len(misses))
	for _, m := range misses {
		out = append(out, m.Command)
	}
	sort.Strings(out)
	return out
}

func assertReported(t *testing.T, rows []store.ListToolTraceForRunRow, want []string) {
	t.Helper()
	got := reportedCommands(prescan(rows))
	if len(got) != len(want) {
		t.Fatalf("reported %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("reported %v, want exactly %v", got, want)
		}
	}
}

// --- the eight cases, each in its own trace ------------------------------------
//
// Isolated per case rather than concatenated into one, because the design note's own
// table cannot be run as a single trace: case 1 (tsc suppressed via script echo) and
// case 7 (tsc reported because the wrapper errored) name the SAME tool with OPPOSITE
// expected outcomes, and its stated totals — suppressed {tsc,vitest}, reported
// {kubectl,helm,eslint,jq,terraform} — cover seven tools across eight cases, which is
// where the collision shows. Isolation is also strictly more discriminating: a
// regression in one case cannot be masked by another case's rows.
// TestPrescanSuppressionFixtureCombined below runs the seven non-colliding cases as
// one trace and asserts those exact sets, so the aggregate claim is pinned too.

func TestPrescanSuppressionFixture(t *testing.T) {
	cases := []struct {
		name string
		why  string
		rows []store.ListToolTraceForRunRow
		want []string
	}{
		{
			name: "1_script_echo_suppresses",
			why:  "npm run typecheck went green and echoed `> tsc --noEmit`",
			rows: []store.ListToolTraceForRunRow{
				traceUse(9, "u1", "tsc --noEmit"),
				traceResult(10, "u1", "sh: 1: tsc: not found", true),
				traceUse(29, "u2", "npm run typecheck"),
				traceResult(30, "u2", npmScriptOutput("typecheck", "tsc --noEmit", ""), false),
			},
			want: []string{},
		},
		{
			name: "2_direct_invocation_suppresses",
			why:  "a successful direct `./node_modules/.bin/vitest run` prints nothing about vitest",
			rows: []store.ListToolTraceForRunRow{
				traceUse(9, "u1", "vitest run"),
				traceResult(10, "u1", "sh: 1: vitest: not found", true),
				traceUse(29, "u2", "./node_modules/.bin/vitest run"),
				traceResult(30, "u2", "Test Files  3 passed (3)\nTests  61 passed (61)", false),
			},
			want: []string{},
		},
		{
			name: "3_genuinely_absent_is_reported",
			why:  "the guarantee suppression must not break: a tool that never runs again stays flagged",
			rows: []store.ListToolTraceForRunRow{
				traceUse(9, "u1", "kubectl get pods"),
				traceResult(10, "u1", "bash: kubectl: command not found", true),
			},
			want: []string{"kubectl"},
		},
		{
			name: "4_green_before_the_miss_does_not_suppress",
			why:  "ORDERING. helm ran green at 10 and was absent at 30; a tool can disappear mid-run",
			rows: []store.ListToolTraceForRunRow{
				traceUse(9, "u1", "helm version"),
				traceResult(10, "u1", "version.BuildInfo{Version:\"v3.16.2\"}", false),
				traceUse(29, "u2", "helm template ."),
				traceResult(30, "u2", "bash: helm: command not found", true),
			},
			want: []string{"helm"},
		},
		{
			name: "5_same_result_veto",
			why:  "`npm run lint` exiting 0 while its own output says `eslint: not found` is not evidence eslint ran",
			rows: []store.ListToolTraceForRunRow{
				traceUse(9, "u1", "eslint ."),
				traceResult(10, "u1", "sh: 1: eslint: not found", true),
				traceUse(29, "u2", "npm run lint"),
				traceResult(30, "u2", npmScriptOutput("lint", "eslint .", "sh: 1: eslint: not found"), false),
			},
			want: []string{"eslint"},
		},
		{
			name: "6_non_executable_position",
			why:  "`grep jq package.json` MENTIONS jq without running it — the strings.Contains trap",
			rows: []store.ListToolTraceForRunRow{
				traceUse(9, "u1", "jq -r .name package.json"),
				traceResult(10, "u1", "sh: 1: jq: not found", true),
				traceUse(29, "u2", "grep jq package.json"),
				traceResult(30, "u2", "  \"scripts\": { \"x\": \"jq .\" }", false),
			},
			want: []string{"jq"},
		},
		{
			name: "7_green_requirement",
			why:  "the wrapper errored, so nothing is proven about tsc (the stricter PRD predicate)",
			rows: []store.ListToolTraceForRunRow{
				traceUse(9, "u1", "tsc --noEmit"),
				traceResult(10, "u1", "sh: 1: tsc: not found", true),
				traceUse(29, "u2", "npm run typecheck"),
				traceResult(30, "u2", npmScriptOutput("typecheck", "tsc --noEmit", "src/a.ts(3,1): error TS2304"), true),
			},
			want: []string{"tsc"},
		},
		{
			name: "8_invocation_without_result",
			why:  "a tool_use with NO matching tool_result proves an attempt, never an execution",
			// No tool_use for the MISS deliberately: the row window can start mid-run,
			// and an earlier terraform invocation would put a green at a seq below the
			// miss, masking the very mutation this case exists to catch (an
			// invocation-counts-as-execution fold records min-seq too).
			rows: []store.ListToolTraceForRunRow{
				traceResult(10, "u1", "exec: \"terraform\": executable file not found in $PATH", true),
				traceUse(30, "u2", "terraform plan -out tf.plan"),
			},
			want: []string{"terraform"},
		},
		{
			name: "9_heredoc_body_is_data_not_invocation",
			why: "writing a wrapper script CONTAINING tsc must not suppress the tsc miss — " +
				"the write succeeds, tsc never runs, and the failure would be silent",
			rows: []store.ListToolTraceForRunRow{
				traceUse(9, "u1", "tsc --noEmit"),
				traceResult(10, "u1", "sh: 1: tsc: not found", true),
				traceUse(29, "u2", "cat > build.sh <<'EOF'\nnpx tsc --noEmit\nEOF"),
				traceResult(30, "u2", "", false), // writing a file succeeds
			},
			want: []string{"tsc"},
		},
		{
			name: "10_green_miss_green_suppresses",
			why: "helm demonstrably ran green AFTER the miss; recording the EARLIEST green " +
				"answered the wrong question and kept reporting it",
			// Isolated from the combined trace for the same reason as case 7: it reuses
			// helm, which case 4 expects REPORTED. green → miss → green and
			// green → miss are opposite expectations for one tool.
			rows: []store.ListToolTraceForRunRow{
				traceUse(9, "g1", "helm version"),
				traceResult(10, "g1", "version.BuildInfo{Version:\"v3.16.2\"}", false),
				traceUse(29, "m1", "helm template ."),
				traceResult(30, "m1", "bash: helm: command not found", true),
				traceUse(49, "g2", "helm version"),
				traceResult(50, "g2", "version.BuildInfo{Version:\"v3.16.2\"}", false),
			},
			want: []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reportedCommands(prescan(tc.rows))
			if len(got) != len(tc.want) {
				t.Fatalf("%s\nreported %v, want exactly %v", tc.why, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("%s\nreported %v, want exactly %v", tc.why, got, tc.want)
				}
			}
		})
	}
}

// TestPrescanSuppressionFixtureCombined runs the seven non-colliding cases as ONE
// trace and asserts the exact reported set — sizes 2 suppressed / 5 reported, per the
// design note. Case 7 is excluded by construction: it reuses tsc, which case 1
// suppresses, so no single trace can hold both expectations.
//
// The seqs are the per-case 10/30 offset by case index so the run's seq stays
// monotonic, as run_messages guarantees per run.
func TestPrescanSuppressionFixtureCombined(t *testing.T) {
	rows := []store.ListToolTraceForRunRow{
		// 1 — tsc, suppressed via script echo
		traceUse(109, "c1a", "tsc --noEmit"),
		traceResult(110, "c1a", "sh: 1: tsc: not found", true),
		traceUse(129, "c1b", "npm run typecheck"),
		traceResult(130, "c1b", npmScriptOutput("typecheck", "tsc --noEmit", ""), false),
		// 2 — vitest, suppressed via direct invocation
		traceUse(209, "c2a", "vitest run"),
		traceResult(210, "c2a", "sh: 1: vitest: not found", true),
		traceUse(229, "c2b", "./node_modules/.bin/vitest run"),
		traceResult(230, "c2b", "Tests  61 passed (61)", false),
		// 3 — kubectl, never runs again
		traceUse(309, "c3a", "kubectl get pods"),
		traceResult(310, "c3a", "bash: kubectl: command not found", true),
		// 4 — helm, green BEFORE the miss
		traceUse(409, "c4a", "helm version"),
		traceResult(410, "c4a", "version.BuildInfo{Version:\"v3.16.2\"}", false),
		traceUse(429, "c4b", "helm template ."),
		traceResult(430, "c4b", "bash: helm: command not found", true),
		// 5 — eslint, same-result veto
		traceUse(509, "c5a", "eslint ."),
		traceResult(510, "c5a", "sh: 1: eslint: not found", true),
		traceUse(529, "c5b", "npm run lint"),
		traceResult(530, "c5b", npmScriptOutput("lint", "eslint .", "sh: 1: eslint: not found"), false),
		// 6 — jq, mentioned in non-executable position
		traceUse(609, "c6a", "jq -r .name package.json"),
		traceResult(610, "c6a", "sh: 1: jq: not found", true),
		traceUse(629, "c6b", "grep jq package.json"),
		traceResult(630, "c6b", "  \"scripts\": { \"x\": \"jq .\" }", false),
		// 8 — terraform, invoked again with no result (see the case note above for why
		// the miss deliberately has no originating tool_use in the window)
		traceResult(810, "c8a", "exec: \"terraform\": executable file not found in $PATH", true),
		traceUse(830, "c8b", "terraform plan -out tf.plan"),
	}
	assertReported(t, rows, []string{"eslint", "helm", "jq", "kubectl", "terraform"})
}

// TestExecutablesInParsesExecutablePosition pins the matcher directly, so a
// regression here names the parser rather than surfacing as a mysterious suppression.
func TestExecutablesInParsesExecutablePosition(t *testing.T) {
	cases := []struct {
		command string
		want    []string
	}{
		{"tsc --noEmit", []string{"tsc"}},
		{"./node_modules/.bin/vitest run", []string{"vitest"}},
		{"cd web && npm run typecheck", []string{"cd", "npm"}},
		{"npx vitest run", []string{"npx", "vitest"}},
		{"pnpm exec tsc --noEmit", []string{"pnpm", "tsc"}},
		{"bunx eslint .", []string{"bunx", "eslint"}},
		{"pnpm dlx tsc --noEmit", []string{"pnpm", "tsc"}},
		{"yarn dlx eslint .", []string{"yarn", "eslint"}},
		// `2>&1` is a redirect: the trailing 1 must not become a phantom executable.
		{"tsc > out.txt 2>&1", []string{"tsc"}},
		{"CI=1 NODE_ENV=test tsc", []string{"tsc"}},
		{"cat x | jq .name", []string{"cat", "jq"}},
		{"make build; helm lint", []string{"make", "helm"}},
		{"(cd api && go build ./...)", []string{"cd", "go"}},
		{"tsc 2>/dev/null || true", []string{"tsc", "true"}},
		// The trap this parser exists for: a MENTION is not an invocation.
		{"grep tsc package.json", []string{"grep"}},
		{"echo 'run vitest later'", []string{"echo"}},
		{"rg --files-with-matches eslint", []string{"rg"}},
		// A heredoc BODY is data. `\n` is a real separator, so without the latch every
		// body line's first word became an executable and writing a wrapper script
		// suppressed a genuine miss.
		{"cat > build.sh <<'EOF'\ntsc --noEmit\nEOF", []string{"cat"}},
		{"cat > check.sh <<'EOF'\nnpx tsc --noEmit\nEOF", []string{"cat"}},
		{"cat <<-EOF > f\nvitest run\nEOF", []string{"cat"}},
		// The latch covers non-newline separators too — a body line can carry its own.
		{"cat > s.sh <<'EOF'\nfoo; eslint .\nEOF", []string{"cat"}},
		// A single `<` is an ordinary redirect and must NOT latch.
		{"tsc < input.ts && vitest run", []string{"tsc", "vitest"}},
	}
	for _, tc := range cases {
		t.Run(tc.command, func(t *testing.T) {
			got := executablesIn(tc.command)
			if len(got) != len(tc.want) {
				t.Fatalf("executablesIn(%q) = %v, want %v", tc.command, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("executablesIn(%q) = %v, want %v", tc.command, got, tc.want)
				}
			}
		})
	}
}

// TestScriptEchoIgnoresOutputBelowTheHeaderBlock pins the bound on the script-echo
// channel. A suite that prints a shell transcript — CLI usage goldens, README-driven
// tests, markdown snapshots — must not fake a green for the tool in that transcript.
// Live in this repo, which has a CLI with docs, not a hypothetical.
func TestScriptEchoIgnoresOutputBelowTheHeaderBlock(t *testing.T) {
	vitestOutput := "\n> uzi@1.0.0 test\n> vitest run\n\n" +
		" ✓ src/docs.test.ts (3)\n" +
		"   golden output:\n" +
		"    $ shellcheck x.sh\n" + // a docs golden, NOT an invocation
		"    (no issues found)\n" +
		" Test Files  1 passed (1)\n"
	rows := []store.ListToolTraceForRunRow{
		traceUse(9, "m1", "shellcheck x.sh"),
		traceResult(10, "m1", "sh: 1: shellcheck: not found", true),
		traceUse(29, "g1", "npm run test"),
		traceResult(30, "g1", vitestOutput, false),
	}
	assertReported(t, rows, []string{"shellcheck"})

	// The real echo, two lines up in the same output, still registers.
	if _, ok := observedGreenTools(rows)["vitest"]; !ok {
		t.Error("the package manager's own echo must still be read — bounding the " +
			"channel must not disable it")
	}
}

// TestScriptEchoReadsYarnAndBunHeaders: yarn 1 and bun prefix with `$ ` and, unlike
// npm, emit no blank line before the command's output. They are why the channel needs
// a LINE CAP and not only a blank-line stop.
func TestScriptEchoReadsYarnAndBunHeaders(t *testing.T) {
	for _, tc := range []struct{ name, output string }{
		{"yarn1", "yarn run v1.22.19\n$ tsc --noEmit\nDone in 1.20s.\n"},
		{"bun", "$ tsc --noEmit\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := scriptEchoTools(tc.output); len(got) != 1 || got[0] != "tsc" {
				t.Fatalf("scriptEchoTools = %v, want [tsc]", got)
			}
		})
	}
}

// TestCommandIndexBudgetIsATrueBound: an oversized command is skipped rather than
// admitted and then blinding every later invocation.
func TestCommandIndexBudgetIsATrueBound(t *testing.T) {
	huge := "tsc " + strings.Repeat("x", judgeCommandByteBudget*3)
	rows := []store.ListToolTraceForRunRow{
		traceUse(1, "big", huge),
		traceResult(2, "big", "", false),
		traceUse(3, "small", "vitest run"),
		traceResult(4, "small", "", false),
	}
	green := observedGreenTools(rows)
	if _, ok := green["tsc"]; ok {
		t.Error("a command exceeding the index budget must be skipped, not admitted in full")
	}
	if _, ok := green["vitest"]; !ok {
		t.Error("an oversized command must not blind the index to every LATER invocation")
	}
}

// TestObservedGreenToolsRecordsLatestSeq is a companion to fixture case 10, NOT the
// gate for it. It asserts the map's shape, which is an implementation choice; case 10
// asserts the behaviour that choice exists to produce, and that is what must redden.
//
// Recorded because the distinction was earned: while this map held the EARLIEST green,
// the min→max fold reddened only this test and moved no fixture case, since no fixture
// then had two greens straddling a miss. A test that pins the choice rather than the
// consequence is exactly the test that goes green on a broken rewrite.
func TestObservedGreenToolsRecordsLatestSeq(t *testing.T) {
	rows := []store.ListToolTraceForRunRow{
		traceUse(1, "a", "tsc --noEmit"),
		traceResult(2, "a", "", false),
		traceUse(3, "b", "tsc --noEmit"),
		traceResult(4, "b", "", false),
	}
	green := observedGreenTools(rows)
	if at, ok := green["tsc"]; !ok || at != 4 {
		t.Fatalf("green[tsc] = %v (present=%v), want the LATEST green seq 4 — suppression "+
			"asks whether ANY green sits above the miss, and max is that existential", at, ok)
	}
}

// TestToolResultTextHandlesBothContentShapes: the SDK passes tool_result content
// through as-is, so it is a string on some frames and an array of content blocks on
// others. The script-echo channel reads this, and a shape it cannot decode silently
// disables suppression for that result.
func TestToolResultTextHandlesBothContentShapes(t *testing.T) {
	stringForm := traceResult(1, "a", "> tsc --noEmit\nok", false)
	if got := toolResultText(stringForm.Payload); got != "> tsc --noEmit\nok" {
		t.Errorf("string content = %q", got)
	}

	blockForm, err := json.Marshal(map[string]any{
		"tool_use_id": "a",
		"content":     []any{map[string]any{"type": "text", "text": "> tsc --noEmit"}, map[string]any{"type": "text", "text": "ok"}},
		"is_error":    false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := toolResultText(blockForm); got != "> tsc --noEmit\nok" {
		t.Errorf("block content = %q, want the texts joined by newline", got)
	}
}

// TestObservedGreenToolsTreatsAbsentIsErrorAsGreen: is_error is normalized to a strict
// boolean worker-side, but a row written by any other producer may omit it. Absent
// must read as "not an error", per the SDK's `is_error?: boolean`.
func TestObservedGreenToolsTreatsAbsentIsErrorAsGreen(t *testing.T) {
	rows := []store.ListToolTraceForRunRow{
		traceUse(1, "a", "tsc --noEmit"),
		{Seq: 2, Kind: "tool_result", Payload: []byte(`{"tool_use_id":"a","content":"ok"}`)},
	}
	if _, ok := observedGreenTools(rows)["tsc"]; !ok {
		t.Fatal("a tool_result with no is_error key must count as green")
	}
}

// TestSuppressResolvedTruncatesAfterSuppression pins why the scan collects
// judgeMissCandidateCap rather than judgeMaxMissingTools: suppression runs BEFORE the
// truncation, so a run whose early misses are all resolved still reports a full 20.
func TestSuppressResolvedTruncatesAfterSuppression(t *testing.T) {
	var cands []missCandidate
	green := map[string]int32{}
	for i := 0; i < judgeMissCandidateCap; i++ {
		name := string(rune('a'+i/26)) + string(rune('a'+i%26))
		cands = append(cands, missCandidate{Command: name, Evidence: "e", Seq: 1})
		if i < judgeMaxMissingTools {
			green[name] = 2 // the first half all later ran green
		}
	}
	got := suppressResolved(cands, green)
	if len(got) != judgeMaxMissingTools {
		t.Fatalf("reported %d misses, want the full %d — suppressing the first %d must not "+
			"shrink the report below the cap", len(got), judgeMaxMissingTools, judgeMaxMissingTools)
	}
	if got[0].Command != cands[judgeMaxMissingTools].Command {
		t.Errorf("first reported miss = %q, want the first UNsuppressed candidate %q",
			got[0].Command, cands[judgeMaxMissingTools].Command)
	}
}

// traceUseUsageAttached builds the SAME tool_use row as traceUse, plus the two keys
// the worker's executor attaches to whichever mapped message survives its signal
// filter: `model` and `usage`. Deliberately a separate helper rather than a parameter
// on traceUse, so the two arms of the test below differ in exactly one thing and a
// future edit to the base shape lands on both.
func traceUseUsageAttached(seq int32, id, command string) store.ListToolTraceForRunRow {
	p, err := json.Marshal(map[string]any{
		"id": id, "name": "Bash", "input": map[string]any{"command": command},
		// sdk-executor.ts: `em.payload["usage"] = frameUsage` and, co-gated under the
		// same latch, `em.payload["model"] = frameModel`.
		"model": "claude-opus-4-8",
		"usage": map[string]any{"input_tokens": 123, "output_tokens": 45},
	})
	if err != nil {
		panic(err)
	}
	return store.ListToolTraceForRunRow{Seq: seq, Kind: "tool_use", Payload: p}
}

// TestPrescanIgnoresFrameKeysOnToolUse pins a CROSS-PRD contract that neither PRD
// guards on its own: keys another feature attaches to a run_message payload are
// ADDITIVE, and the pre-scan's verdict must not move when they appear.
//
// The collision is real, not hypothetical. PRD #93 (per-agent Model column) attaches
// `model` alongside `usage` to the surviving mapped message, and for an assistant
// frame that message can be a tool_use BLOCK — which is exactly the payload this
// milestone's `toolUseCommand` decodes to recover the command the agent typed. The two
// PRDs were developed on separate branches and never saw each other; reviewed alone,
// neither shows the interaction. It is currently inert because `toolUseCommand`
// decodes into a narrow struct and encoding/json ignores unknown fields by default —
// an implicit property nothing else asserts, which is what makes it worth a test. The
// next feature to attach a key at that seam will have no idea this constraint exists.
//
// 🔴 HOW THIS TEST WAS SHOWN TO DISCRIMINATE, because a green here is otherwise
// indistinguishable from a test that compares two things it never varied. The control
// is a fold on `toolUseCommand`, and the OBVIOUS fold is the wrong one:
//
//   - Plain `DisallowUnknownFields()` reddens BOTH arms — `name` is absent from the
//     struct too, so the no-keys arm fails first on the precondition. That proves only
//     "the pipeline is sensitive to decoding in general", which is not this test's
//     claim, and a reader who stopped there would believe the control passed.
//   - The DISCRIMINATING fold adds `Name string` to the struct AND
//     `DisallowUnknownFields()`, leaving ONLY PRD #93's keys unknown. Measured: the
//     precondition still holds at [helm], and the interaction assertion then fires with
//     [helm] -> [helm tsc].
//
// Keep that distinction if this test is ever revised. The first fold is the one that
// gets reached for, and it silently tests something else.
func TestPrescanIgnoresFrameKeysOnToolUse(t *testing.T) {
	// A run that fumbles a bare `tsc`, fumbles `helm` (never resolved), then runs tsc
	// green through an npm script. Only helm should be reported. The npm output comes
	// from npmScriptOutput because the script-echo channel is what suppresses tsc at
	// all: a hand-written empty result answers [helm tsc] and would make this test
	// agree with a broken implementation for the wrong reason.
	build := func(use func(int32, string, string) store.ListToolTraceForRunRow) []store.ListToolTraceForRunRow {
		return []store.ListToolTraceForRunRow{
			use(1, "a", "tsc --noEmit"),
			traceResult(2, "a", "sh: tsc: command not found", true),
			use(3, "b", "helm lint ./deploy"),
			traceResult(4, "b", "bash: helm: command not found", true),
			use(5, "c", "npm run typecheck"),
			traceResult(6, "c", npmScriptOutput("typecheck", "tsc --noEmit", ""), false),
		}
	}

	bare := reportedCommands(prescan(build(traceUse)))
	attached := reportedCommands(prescan(build(traceUseUsageAttached)))

	// Precondition FIRST: if the bare trace does not already discriminate, the
	// comparison below is two identical wrong answers agreeing with each other.
	if len(bare) != 1 || bare[0] != "helm" {
		t.Fatalf("precondition failed: the trace WITHOUT frame keys reported %v, want exactly "+
			"[helm] (tsc suppressed by the npm script echo, helm never resolved). This test "+
			"cannot detect an interaction until the trace itself discriminates", bare)
	}

	if !slices.Equal(bare, attached) {
		t.Fatalf("PRD #93's model/usage keys CHANGED the M3 verdict: %v -> %v. Keys another "+
			"feature attaches to a run_message payload must be inert here — toolUseCommand "+
			"decodes tool_use into a narrow struct, so an unknown key must be IGNORED, never "+
			"fail the decode. A tool the run demonstrably ran green is now being reported "+
			"missing to the judge because an unrelated payload field appeared next to it",
			bare, attached)
	}
}
