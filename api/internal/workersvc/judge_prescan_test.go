package workersvc

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
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
// {kubectl,helm,eslint,jq,tofu} — cover seven tools across eight cases, which is
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
			// and an earlier tofu invocation would put a green at a seq below the
			// miss, masking the very mutation this case exists to catch (an
			// invocation-counts-as-execution fold records min-seq too).
			rows: []store.ListToolTraceForRunRow{
				traceResult(10, "u1", "exec: \"tofu\": executable file not found in $PATH", true),
				traceUse(30, "u2", "tofu plan -out tf.plan"),
			},
			want: []string{"tofu"},
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
		// 8 — tofu, invoked again with no result (see the case note above for why
		// the miss deliberately has no originating tool_use in the window)
		traceResult(810, "c8a", "exec: \"tofu\": executable file not found in $PATH", true),
		traceUse(830, "c8b", "tofu plan -out tf.plan"),
	}
	assertReported(t, rows, []string{"eslint", "helm", "jq", "kubectl", "tofu"})
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

// TestGoRunPinnedNamesParsesPinnedRefs pins the go-run parser directly, so a regression
// names the parser rather than surfacing as a mysterious suppression (the same reason
// TestExecutablesInParsesExecutablePosition exists). A tool reached via
// `go run <module>@<version>` needs no bare executable, so its name is what the scan
// suppresses.
func TestGoRunPinnedNamesParsesPinnedRefs(t *testing.T) {
	cases := []struct {
		command string
		want    []string
	}{
		// The motivating shape (judge rec run eaa26abe): golangci-lint via a pinned ref.
		{"go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run ./...", []string{"golangci-lint"}},
		{"go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...", []string{"govulncheck"}},
		{"go run golang.org/x/tools/cmd/deadcode@latest -test ./...", []string{"deadcode"}},
		// A build flag before the pinned package is skipped; the FIRST non-flag token is
		// the package, per `go run [build flags] <package> [args…]`.
		{"go run -mod=mod example.com/cmd/tool@v1.2.3", []string{"tool"}},
		// `go` must be in executable position — after a separator it still is.
		{"cd api && go run rsc.io/sqlc@v1.0.0 generate", []string{"sqlc"}},
		// UNPINNED shapes yield nothing: the version pin is the signal.
		{"go run .", nil},
		{"go run ./cmd/foo", nil},
		{"go run main.go", nil},
		// A `go` subcommand other than `run` provides nothing.
		{"go build ./...", nil},
		{"go test ./...", nil},
		// A MENTION is not an invocation — the strings.Contains trap this parser avoids.
		{"echo go run tool@v1 later", nil},
		{"grep 'go run' Makefile", nil},
		// `go` not in executable position (an argument to another command).
		{"which go run", nil},
	}
	for _, tc := range cases {
		t.Run(tc.command, func(t *testing.T) {
			got := goRunPinnedNames(tc.command)
			if len(got) != len(tc.want) {
				t.Fatalf("goRunPinnedNames(%q) = %v, want %v", tc.command, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("goRunPinnedNames(%q) = %v, want %v", tc.command, got, tc.want)
				}
			}
		})
	}
}

// TestPrescanSuppressesGoRunPinnedTool pins the behaviour (judge rec run eaa26abe): a
// tool the repo invokes via `go run <module>@<version>` is reachable through that pinned
// ref, so a `command not found` for its bare name must NOT surface as a missing worker
// tool. Suppression is presence-based, so it holds whichever order the two rows arrive.
func TestPrescanSuppressesGoRunPinnedTool(t *testing.T) {
	const pinnedLint = "go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run ./..."

	// The agent probes the bare name (not found), and the repo lints via the pinned ref.
	assertReported(t, []store.ListToolTraceForRunRow{
		traceUse(9, "u1", "command -v golangci-lint"),
		traceResult(10, "u1", "bash: golangci-lint: command not found", true),
		traceUse(29, "u2", pinnedLint),
		traceResult(30, "u2", "0 issues.", false),
	}, []string{})

	// Order-independent: the not-found probe often comes AFTER the successful `go run`,
	// which suppressResolved's strictly-later green rule would NOT catch — presence does.
	assertReported(t, []store.ListToolTraceForRunRow{
		traceUse(9, "u1", pinnedLint),
		traceResult(10, "u1", "0 issues.", false),
		traceUse(29, "u2", "command -v golangci-lint"),
		traceResult(30, "u2", "bash: golangci-lint: command not found", true),
	}, []string{})
}

// TestPrescanGoRunSuppressionSparesGenuineMiss is the guarantee the suppression must not
// break: a DIFFERENT tool that is genuinely absent stays reported even when a pinned
// go-run tool is suppressed in the same trace, and an UNPINNED `go run` suppresses
// nothing. Asserting both in one place is the control that the filter is name-specific
// and pin-specific, not a blanket "saw go run, drop everything".
func TestPrescanGoRunSuppressionSparesGenuineMiss(t *testing.T) {
	// golangci-lint suppressed (pinned ref present), kubectl still reported.
	assertReported(t, []store.ListToolTraceForRunRow{
		traceUse(9, "u1", "golangci-lint run"),
		traceResult(10, "u1", "bash: golangci-lint: command not found", true),
		traceUse(19, "u3", "kubectl get pods"),
		traceResult(20, "u3", "bash: kubectl: command not found", true),
		traceUse(29, "u2", "go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run ./..."),
		traceResult(30, "u2", "0 issues.", false),
	}, []string{"kubectl"})

	// A bare `go run .` pins no tool, so a real miss in the same run is untouched.
	assertReported(t, []store.ListToolTraceForRunRow{
		traceUse(9, "u1", "kubectl get pods"),
		traceResult(10, "u1", "bash: kubectl: command not found", true),
		traceUse(29, "u2", "go run ."),
		traceResult(30, "u2", "", false),
	}, []string{"kubectl"})
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

// TestScanSuppressesDenylistedCredentialCLIs pins the deterministic half of the fix.
// A credential-bearing CLI is barred by toolprofile's Decision 6 denylist, so a run
// whose agent reaches for one always sees command-not-found; the scan keys on that
// string rather than on whether the tool could ever be installed, so it kept producing
// `install_worker_tool` recommendations nobody could action.
//
// SCOPE, stated because the commit that added this initially overclaimed: the two
// observed glab recommendations (runs b64b98f3, 1dfc65b4) came from `complete` MODEL
// reviews, not from the deterministic fallback — a target of "file, glab" is free text
// no fallback emits. This filter closes the deterministic path; JUDGE_SYSTEM_PROMPT
// closes the model path. Neither alone would have prevented those two.
//
// The `aws` case is the one a name-equality check against the denylist would miss: the
// denylisted PACKAGE is `awscli` and the command a shell reports is `aws`.
func TestScanSuppressesDenylistedCredentialCLIs(t *testing.T) {
	rows := rawResultRows(
		`{"content":"glab: command not found"}`,
		`{"content":"aws: command not found"}`,
		`{"content":"az: command not found"}`,
		`{"content":"jq: command not found"}`,
	)

	got := scanCommandNotFound(rows)

	var cmds []string
	for _, c := range got {
		cmds = append(cmds, c.Command)
	}
	// jq is an ordinary tool and MUST survive: a suppression that swallows real
	// findings would be a worse defect than the one being fixed.
	if !slices.Contains(cmds, "jq") {
		t.Fatalf("jq should still be reported as missing, got %v", cmds)
	}
	for _, denied := range []string{"glab", "aws", "az"} {
		if slices.Contains(cmds, denied) {
			t.Errorf("%q is denylisted and can never be installed; it must not surface as a missing tool (got %v)", denied, cmds)
		}
	}
}

// TestScanSuppressesDenylistedCLIsInPathForm covers the bypass a bare map lookup left
// open. reExecNotFound captures `/` and `.`, so Go's exec.LookPath error arrives as a
// FULL PATH — measured leaking `/usr/local/bin/glab` and `./glab` past the filter
// before DeniedExecutable basenamed its input. The sh/bash forms exclude `/`, which is
// why only the exec: form leaked and why a single fixture would have missed it.
func TestScanSuppressesDenylistedCLIsInPathForm(t *testing.T) {
	rows := rawResultRows(
		`{"content":"exec: \"/usr/local/bin/glab\": executable file not found in $PATH"}`,
		`{"content":"exec: \"./glab\": executable file not found in $PATH"}`,
		`{"content":"exec: \"/usr/bin/jq\": executable file not found in $PATH"}`,
	)

	var cmds []string
	for _, c := range scanCommandNotFound(rows) {
		cmds = append(cmds, c.Command)
	}
	for _, c := range cmds {
		if strings.Contains(c, "glab") {
			t.Errorf("path-form denied CLI leaked through as %q (got %v)", c, cmds)
		}
	}
	// The ordinary tool must still be reported — and reported as `jq`, NOT `/usr/bin/jq`.
	// Asserting the VALUE rather than the count is deliberate: a count-only assertion
	// passed while the candidate carried the full path, which put a path in the
	// recommendation target (the coordinate the cross-run backlog dedupes on) and let one
	// tool occupy two candidate slots when a run hit both the bare and path forms.
	if !slices.Contains(cmds, "jq") {
		t.Errorf("the non-denied path-form miss must be reported basenamed as \"jq\", got %v", cmds)
	}
}

// TestPrescanDropsGenericNotFoundWordNeverInvoked pins the fix (issue #263): a generic
// English word in tool output that matches the low-confidence `X: not found`
// (dash/busybox) form but was never invoked as a command is NOT a missing worker tool
// and must produce no ToolMiss. Without corroboration these each became a false
// `install_worker_tool` recommendation downstream.
func TestPrescanDropsGenericNotFoundWordNeverInvoked(t *testing.T) {
	// A lone generic line, no tool_use anywhere: the token was never invoked.
	assertReported(t, []store.ListToolTraceForRunRow{
		traceResult(10, "u1", "sh: 1: key: not found", true),
	}, []string{})

	assertReported(t, []store.ListToolTraceForRunRow{
		traceResult(10, "u1", "foo: not found", true),
	}, []string{})

	// Two generic words in one trace, neither invoked: both dropped.
	assertReported(t, []store.ListToolTraceForRunRow{
		traceResult(10, "u1", "X: not found", true),
		traceResult(20, "u2", "sh: 1: key: not found", true),
	}, []string{})
}

// TestPrescanMentionDoesNotCorroborate: corroboration reuses tool_use command text as
// an allow-signal via executablesIn, which parses EXECUTABLE POSITION — not
// strings.Contains. A token that appears only as an ARGUMENT (`grep key package.json`
// mentions key) was never invoked as a command, so the low-confidence `key: not found`
// line stays dropped.
func TestPrescanMentionDoesNotCorroborate(t *testing.T) {
	rows := []store.ListToolTraceForRunRow{
		traceUse(9, "u1", "grep key package.json"),
		traceResult(10, "u1", "sh: 1: key: not found", true),
	}
	assertReported(t, rows, []string{})
}

// TestPrescanHighConfidenceFormsSurviveWithoutInvocation: corroboration applies to the
// low-confidence reShNotFound form ONLY. The three explicit forms (`command not found`
// bash/zsh, exec: executable-file-not-found) are high-confidence and must report even
// with NO tool_use in the trace to corroborate against.
func TestPrescanHighConfidenceFormsSurviveWithoutInvocation(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{"bash", "foo: command not found"},
		{"zsh", "command not found: foo"},
		// Watch the quote escaping, as case-8's tofu fixture does: the jsonb
		// payload carries \" and payloadText unescapes it before reExecNotFound runs.
		{"exec", "exec: \"foo\": executable file not found in $PATH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertReported(t, []store.ListToolTraceForRunRow{
				traceResult(10, "u1", tc.content, true),
			}, []string{"foo"})
		})
	}
}

// TestPrescanGenuineBusyboxMissPreserved is the corroboration positive path: a real
// dash/busybox miss whose tool WAS invoked directly (`foo --version` puts foo in
// executable position) is corroborated and still reported. The fix must not swallow
// genuine low-confidence misses.
func TestPrescanGenuineBusyboxMissPreserved(t *testing.T) {
	rows := []store.ListToolTraceForRunRow{
		traceUse(9, "u1", "foo --version"),
		traceResult(10, "u1", "sh: 1: foo: not found", true),
	}
	assertReported(t, rows, []string{"foo"})
}

// TestPrescanSuppressesShellFunctionDefinition pins PRD #499: a name the run DEFINES as
// a shell function (here `db_psql`) must NOT be reported as a missing worker tool even
// when a subshell that could not see the function emits `db_psql: command not found`.
// A genuine miss for a name defined NOWHERE (`jq`) is the negative control proving the
// suppression does not over-fire.
//
// 🔴 The fixture is built with traceResult, which marshals the content to jsonb so the
// `\n` newlines land ESCAPED exactly as a real payload holds them. shellFunctionNames
// runs its anchored def regexes on DECODED text (toolResultText), so a literal-newline
// fixture would pass even against a wrong raw-payloadText implementation, making the
// test vacuous w.r.t. the decode path.
func TestPrescanSuppressesShellFunctionDefinition(t *testing.T) {
	// One tool_result carrying BOTH the function definition and the not-found line, plus
	// a second row with a genuine miss for a name defined nowhere. The not-found line
	// carries bash's real `bash: ` prefix so the space before the name breaks the raw
	// scan's `\n`-bleed (the scan reads raw payloadText, where the preceding newline is
	// the two chars `\n`); shellFunctionNames indexes the def off DECODED text.
	rows := []store.ListToolTraceForRunRow{
		traceResult(10, "u1", "db_psql() {\n  psql \"$DATABASE_URL\" \"$@\"\n}\nbash: db_psql: command not found", true),
		traceResult(20, "u2", "bash: jq: command not found", true),
	}
	// db_psql suppressed (defined as a function), jq reported (never defined).
	assertReported(t, rows, []string{"jq"})
}

// TestPrescanCallSiteAloneDoesNotSuppress confirms a mere INVOCATION of a name (no
// definition anywhere in the trace) does NOT suppress its miss: the def regexes are
// anchored on a real function-definition shape, so a call site alone leaves the miss
// reported. This is the guard against the anchoring regressing to match call sites.
func TestPrescanCallSiteAloneDoesNotSuppress(t *testing.T) {
	rows := []store.ListToolTraceForRunRow{
		traceUse(9, "u1", "db_psql --version"),
		traceResult(10, "u1", "db_psql: command not found", false),
	}
	assertReported(t, rows, []string{"db_psql"})
}

// TestPrescanSuppressesBashKeywordFunctionForm guards the ksh/bash keyword form
// (`function name { … }`) specifically — the original suppression test exercised only the
// POSIX `name() {` form, so a regression in reShellFuncKeyword would go unseen. A genuine
// miss (`jq`) in the same trace is the negative control.
func TestPrescanSuppressesBashKeywordFunctionForm(t *testing.T) {
	rows := []store.ListToolTraceForRunRow{
		traceResult(10, "u1", "function db_psql {\n  psql \"$DATABASE_URL\" \"$@\"\n}\nbash: db_psql: command not found", true),
		traceResult(20, "u2", "bash: jq: command not found", true),
	}
	assertReported(t, rows, []string{"jq"})
}

// TestPrescanDoesNotSuppressForeignLanguageFunctionDef pins the regex-scope tightening:
// a function DEFINITION in another language printed into a tool_result (source code under
// review) must NOT read as a shell-function def and suppress a same-named genuine miss.
// JS `function serve(opts) {` (non-empty parens defeats reShellFuncKeyword's `{` clause),
// a ZERO-ARG `export function watch() {` (not statement-leading defeats reShellFuncKeyword's
// boundary anchor), and Go `func build() {` (name not statement-leading defeats
// reShellFuncPosix's boundary anchor) are all real cases: judge traces routinely carry
// cat/grep'd source. Each is its own negative control — the asserted name being REPORTED is
// the no-suppression behavior, so these stay green whether or not the `|| funcs[cmd]` clause
// is present (they prove the regex does not over-index, not that suppression fires).
//
// Two of the cases pin the anchor TIGHTENING specifically (they would be wrongly indexed by
// a boundary of `^[ \t]*` / `[;&|(]`, and are only excluded because it is now `^` / `[;&|]`):
// an INDENTED zero-arg method `  poll() {` inside a cat'd class body (indentation is not a
// boundary), and a `(`-preceded expression `setTimeout(function tick() {` (a JS IIFE/callback;
// `(` is not a boundary). Both names stay REPORTED.
func TestPrescanDoesNotSuppressForeignLanguageFunctionDef(t *testing.T) {
	rows := []store.ListToolTraceForRunRow{
		traceResult(10, "u1", "function serve(opts) {\n  return http.listen();\n}", false),
		traceResult(20, "u2", "bash: serve: command not found", true),
		traceResult(30, "u3", "func build() {\n  return\n}", false),
		traceResult(40, "u4", "bash: build: command not found", true),
		traceResult(50, "u5", "export function watch() {\n  fs.watch(dir);\n}", false),
		traceResult(60, "u6", "bash: watch: command not found", true),
		traceResult(70, "u7", "class Server {\n  poll() {\n    return this.q.shift();\n  }\n}", false),
		traceResult(80, "u8", "bash: poll: command not found", true),
		traceResult(90, "u9", "setTimeout(function tick() {\n  render();\n}, 16)", false),
		traceResult(100, "u10", "bash: tick: command not found", true),
	}
	// All source-def names still reported — no foreign def (with-args, zero-arg, Go, an
	// INDENTED method, or a `(`-preceded IIFE) is indexed as a shell func.
	assertReported(t, rows, []string{"build", "poll", "serve", "tick", "watch"})
}
