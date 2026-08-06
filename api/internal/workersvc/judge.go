package workersvc

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/toolprofile"
)

const (
	// RunKindJudge is the run-judge kind (PRD #46 Decision 1): a worker-executed
	// retrospective of another finished run. It has no repo, no issue, no branch, no
	// MR — it rides the run machinery only for token delivery and message
	// persistence, and points at the reviewed run via target_run_id.
	RunKindJudge = "judge"
	// RunKindSelfImprove is the self-improvement kind (PRD #46 Decision 10): an
	// autonomous improvement run against uzi's own repo. It is issue-shaped and flows
	// through the ordinary repo-ful claim path — no fork needed here.
	RunKindSelfImprove = "self_improve"
)

// Judge scan caps (PRD #46 Decision 4 + audit L1): the command-not-found pre-scan
// runs off the hot request path (at claim assembly) and bounds what it inspects so a
// pathological run can't make it expensive.
const (
	// judgeToolTraceRowCap bounds the trace fetch. It was 2000 while the query
	// returned tool_result ONLY; PRD #121 M3 widened it to both kinds, which
	// interleave roughly 1:1, so leaving it at 2000 would have HALVED the existing
	// detection's reach — the widening would have made the scan worse. The extra
	// rows are short tool_use command payloads; the fetch was never byte-bounded
	// (judgeScanByteBudget bounds SCANNING, not fetching).
	judgeToolTraceRowCap int32 = 4000
	// judgeScanByteBudget counts tool_result text ONLY, exactly as before the
	// widening, so the existing detection's per-byte reach is unchanged.
	judgeScanByteBudget = 512 * 1024
	// judgeCommandByteBudget bounds the tool_use command index that feeds
	// suppression. Its own, much smaller budget: commands are short, and spending
	// the scan budget on them would shrink the detection it exists to refine.
	judgeCommandByteBudget = 128 * 1024
	judgeMaxMissingTools   = 20  // distinct missing tools reported
	judgeEvidenceMaxLen    = 200 // per-hit evidence cap (chars)
	// judgeMissCandidateCap is how many candidates the scan collects BEFORE
	// suppression. judgeMaxMissingTools both caps and short-circuits the scan, so
	// collecting only 20 would let suppression shrink the reported set below what
	// the cap was ever meant to allow (20 candidates, 15 suppressed → 5 reported,
	// while 5 further genuine misses went unscanned). Collect double, then truncate.
	//
	// This NARROWS that gap; it does not close it, and the truncation is SILENT
	// either way — nothing in JudgeSignal marks a run whose misses were cut off.
	// With 25 of 40 candidates suppressed you report 15 while further genuine misses
	// went unscanned. Measured: 60 distinct missing tools → 40 candidates → 20
	// reported, no marker anywhere. The 20-cap silence is pre-existing behaviour
	// (the old scan capped identically); doubling only makes it rarer. A run
	// legitimately missing 40 tools is broken in a way one signal cannot convey.
	judgeMissCandidateCap = 2 * judgeMaxMissingTools
)

// judgeKnownTargetsCap bounds the improve_uzi target menu delivered on a judge claim
// (issue #232). Bounded so a user with a large backlog cannot bloat the claim/prompt.
const judgeKnownTargetsCap int32 = 50

// JudgeSignal is the API-side deterministic pre-scan the judge claim carries (PRD #46
// Decision 4): command-not-found / missing-executable hits found in the reviewed
// run's tool_result output. The judge interprets them (which tool, which agent) and,
// if the model call fails, they are the deterministic fallback recommendation.
type JudgeSignal struct {
	MissingTools []ToolMiss `json:"missing_tools"`
}

// ToolMiss is one deterministic command-not-found hit: the executable a shell could
// not find and the trimmed line that flagged it (bounded — never raw output wholesale).
type ToolMiss struct {
	Command  string `json:"command"`
	Evidence string `json:"evidence"`
}

// missCandidate is a command-not-found hit BEFORE suppression: the ToolMiss fields
// plus the seq of the trace row that produced it (PRD #121 M3). The seq is what makes
// "the same run LATER ran this tool green" an ordering claim the code can state and a
// test can pin. Before the trace widened, "later" was expressible only as "a larger
// slice index" — correct solely by virtue of the query's ORDER BY, which the pure
// scan function could neither see nor assert.
type missCandidate struct {
	Command  string
	Evidence string
	Seq      int32
}

// Command-not-found patterns, ordered high- to lower-confidence. They only FLAG
// missing-executable evidence for the judge to interpret; none decides anything.
var (
	reCmdNotFound    = regexp.MustCompile(`([A-Za-z0-9_.+-]+): command not found`)                    // bash: `foo: command not found`
	reCmdNotFoundZsh = regexp.MustCompile(`command not found: ([A-Za-z0-9_.+-]+)`)                    // zsh: `command not found: foo`
	reExecNotFound   = regexp.MustCompile(`exec: "?([A-Za-z0-9_./+-]+)"?: executable file not found`) // Go exec.LookPath
	reShNotFound     = regexp.MustCompile(`([A-Za-z0-9_.+-]+): not found\b`)                          // dash/busybox: `foo: not found`
)

// shellNames are the interpreters that REPORT a missing command; they are never the
// missing command themselves, so the bash/zsh forms (`zsh: command not found: foo`)
// would otherwise flag the shell prefix. Filtered out of the results.
//
// The path entries (`/bin/sh`, `/bin/bash`, `/usr/bin/env`) are now UNREACHABLE, since
// normalizeCommandToken basenames before this map is consulted and the bare names below
// already cover them. Kept rather than deleted: harmless, and removing them would make
// this map silently wrong again if the basenaming were ever reverted.
var shellNames = map[string]bool{
	"bash": true, "zsh": true, "sh": true, "dash": true, "ash": true, "ksh": true,
	"fish": true, "/bin/sh": true, "/bin/bash": true, "/usr/bin/env": true, "env": true,
}

// noisyShToken matches tokens the low-confidence `X: not found` (sh/busybox) form
// commonly mis-flags but that are never a missing WORKER TOOL: HTTP status / line
// numbers (404, 1) and shared-object / archive / header files (libssl.so.1, foo.o,
// bar.h). Applied ONLY to reShNotFound; the explicit "command not found" forms are
// high-confidence and unfiltered. A generic English word ("key: not found") can still
// slip through — distinguishing it needs context this flag-only scan lacks, so the
// judge interprets it.
var noisyShToken = regexp.MustCompile(`(?i)^\d+$|\.(so|a|o|h|dll|dylib|la|lo)(\.\d+)*$`)

// forEachNotFound applies every command-not-found pattern to one payload's text,
// calling fn(command, evidence) per hit with the noise filter already applied to the
// low-confidence sh form. Single implementation so the scan and the same-result veto
// (observedGreenTools) can never disagree about what "reports X missing" means.
func forEachNotFound(text string, fn func(cmd, evidence string)) {
	for _, m := range reCmdNotFound.FindAllStringSubmatch(text, -1) {
		fn(m[1], m[0])
	}
	for _, m := range reCmdNotFoundZsh.FindAllStringSubmatch(text, -1) {
		fn(m[1], m[0])
	}
	for _, m := range reExecNotFound.FindAllStringSubmatch(text, -1) {
		fn(m[1], m[0])
	}
	for _, m := range reShNotFound.FindAllStringSubmatch(text, -1) {
		if noisyShToken.MatchString(m[1]) {
			continue
		}
		fn(m[1], m[0])
	}
}

// payloadText renders a raw jsonb payload for pattern matching. tool_result payloads
// are jsonb, so embedded quotes arrive escaped as \" — unescape them so the
// exec: "cmd" form (Go's exec.LookPath error) matches.
func payloadText(p []byte) string {
	return strings.ReplaceAll(string(p), `\"`, `"`)
}

// normalizeCommandToken trims whitespace and surrounding quotes off a flagged
// executable name, then reduces it to its BASENAME.
//
// The basename is not cosmetic. reExecNotFound captures `/` and `.`, so Go's
// exec.LookPath error arrives as a full path, and without this the scan carried it
// through in two damaging ways: the recommendation's target became `/usr/bin/jq`
// rather than `jq` — and `(category, target)` is the coordinate the cross-run backlog
// dedupes on, so the same tool split into separate rows per invocation path — and the
// `seen` map keyed on the raw token, so one run hitting both `jq: command not found`
// and `exec: "/usr/bin/jq"` produced TWO candidates for one tool, consuming two of the
// judgeMissCandidateCap slots.
//
// It also repairs a THIRD defect that predates the denylist work: executablesIn already
// basenames (basenameToken), so the `green` index and the same-result veto have always
// been keyed on basenames. A path-form candidate could therefore never match its own
// green entry — `/usr/bin/helm` against `helm` — which meant suppressResolved could not
// suppress it and observedGreenTools could not veto it. All three now agree.
func normalizeCommandToken(cmd string) string {
	trimmed := strings.Trim(strings.TrimSpace(cmd), `"'`)
	if trimmed == "" {
		return ""
	}
	return path.Base(trimmed)
}

// scanCommandNotFound flags missing-executable evidence in a run's tool_result
// payloads (PRD #46 Decision 4). It bounds the bytes inspected (judgeScanByteBudget),
// dedupes by command keeping the first evidence line AND its seq, and caps the
// candidate count. Pure over its input so it is unit-testable without a DB.
//
// 🔴 THE REGEXES RUN OVER tool_result ROWS ONLY. That is load-bearing, not an
// optimization. Since PRD #121 M3 the trace carries both kinds; a tool_use payload
// holds the command the agent TYPED, which by definition never ran. Feeding those to
// these patterns would make an agent typing `echo "foo: command not found"` report
// `foo` as a missing tool — a brand-new false positive the widening itself would
// introduce, in the function whose whole job this milestone is to make more accurate.
func scanCommandNotFound(rows []store.ListToolTraceForRunRow) []missCandidate {
	var out []missCandidate
	seen := map[string]bool{}
	scanned := 0

	for _, row := range rows {
		if row.Kind != "tool_result" {
			continue
		}
		if scanned >= judgeScanByteBudget || len(out) >= judgeMissCandidateCap {
			break
		}
		text := string(row.Payload)
		if scanned+len(text) > judgeScanByteBudget {
			text = text[:judgeScanByteBudget-scanned]
		}
		scanned += len(text)
		text = payloadText([]byte(text))

		forEachNotFound(text, func(cmd, evidence string) {
			cmd = normalizeCommandToken(cmd)
			// A denylisted credential-bearing CLI (glab/gh/aws/az/…) is not a gap: it is
			// barred by Decision 6 and rejected when an admin allowlists it
			// (toolprofile.Denied). Recommending its installation can never be actioned,
			// so observing it absent is the policy working rather than a finding.
			//
			// FILTERED HERE, at collection, and that placement is load-bearing rather than
			// incidental: judgeMissCandidateCap short-circuits this loop, so a denied CLI
			// caught later (in suppressResolved, or downstream at recommendation time)
			// would still consume a candidate slot and crowd out a genuine miss — the
			// precise failure the cap's own comment above documents at length.
			//
			// SCOPE: this covers the DETERMINISTIC path only. The judge MODEL also reads
			// the sampled trace and can emit the same recommendation on its own; both
			// observed `glab` recommendations (runs b64b98f3, 1dfc65b4) came from a
			// `complete` model review, not from fallbackReview. JUDGE_SYSTEM_PROMPT names
			// the barred class so the model reclassifies to adjust_template/improve_agent,
			// which is the actionable category — that prompt line, not this filter, is
			// what addresses those two.
			if cmd == "" || shellNames[cmd] || toolprofile.DeniedExecutable(cmd) || seen[cmd] || len(out) >= judgeMissCandidateCap {
				return
			}
			seen[cmd] = true
			evidence = strings.TrimSpace(evidence)
			if len(evidence) > judgeEvidenceMaxLen {
				evidence = evidence[:judgeEvidenceMaxLen]
			}
			out = append(out, missCandidate{Command: cmd, Evidence: evidence, Seq: row.Seq})
		})
	}
	return out
}

// suppressResolved drops a candidate whose tool the SAME run demonstrably ran green
// at a LATER seq, then truncates to judgeMaxMissingTools (PRD #121 M3). This is the
// fix for the tsc/vitest/eslint false positives: a single command-not-found text hit
// used to be enough to report a tool missing even when `npm run typecheck` went on to
// succeed with it (run e7c31999, #115).
//
// Suppression is safe in the direction that matters: a genuinely absent tool cannot
// later run green, so a real miss survives. The strictly-later requirement is what
// keeps `helm` green early then absent later reported — an absence that arrives
// during a run is exactly the kind worth telling the judge about.
func suppressResolved(cands []missCandidate, green map[string]int32) []ToolMiss {
	var out []ToolMiss
	for _, c := range cands {
		if len(out) >= judgeMaxMissingTools {
			break
		}
		if at, ok := green[c.Command]; ok && at > c.Seq {
			continue
		}
		out = append(out, ToolMiss{Command: c.Command, Evidence: c.Evidence})
	}
	return out
}

// observedGreenTools indexes the executables a run demonstrably RAN without error,
// mapping each to the LATEST seq at which it did (PRD #121 M3). It walks the trace
// once, joining tool_result rows back to their originating tool_use by tool_use_id —
// never by "some later row mentions X", which an invocation with no result would
// satisfy without anything having executed.
//
// The predicate is deliberately "ran GREEN", not "ran". The semantically correct test
// for a MISSING-tool flag is "later executed" — a tsc exiting 1 on real type errors is
// present, not missing — but PRD #121's risk section asks for the conservative
// direction and the motivating trace (`npm run typecheck` SUCCEEDED) costs nothing
// under the stricter rule. RECORDED SO THE NEXT READER KNOWS IT WAS A CHOICE: a run
// where the tool exists but its suite legitimately fails keeps a false missing-tool
// flag. Relaxing this is a one-line change — drop the isErr check — and the fixture's
// "green requirement" case is the test that would flip.
func observedGreenTools(rows []store.ListToolTraceForRunRow) map[string]int32 {
	commands := make(map[string]string) // tool_use id -> Bash command text
	green := make(map[string]int32)
	cmdBytes := 0

	for _, row := range rows {
		switch row.Kind {
		case "tool_use":
			id, cmd := toolUseCommand(row.Payload)
			if id == "" || cmd == "" {
				continue
			}
			// Checked BEFORE the add, against this command's own length, so the
			// constant is a true bound. Testing the running total first made the real
			// bound "the budget plus one command": a single 384 KB command was indexed
			// in full and every later invocation dropped. Skipping the oversized one
			// instead keeps the rest of the run observable, and dropping an invocation
			// only ever under-suppresses.
			if cmdBytes+len(cmd) > judgeCommandByteBudget {
				continue
			}
			cmdBytes += len(cmd)
			commands[id] = cmd
		case "tool_result":
			id, isErr := toolResultOutcome(row.Payload)
			if isErr || id == "" {
				continue
			}
			cmd, ok := commands[id]
			if !ok {
				// No originating tool_use in the fetched window (or a non-Bash tool):
				// nothing to attribute the green to. Silence, never a guess.
				continue
			}
			// Same-result veto: an exit-0 `npm run lint` whose output says
			// `eslint: not found` is not evidence that eslint ran. Checked against
			// THIS payload, so it cannot reach across to an unrelated result.
			vetoed := map[string]bool{}
			forEachNotFound(payloadText(row.Payload), func(cmd, _ string) {
				vetoed[normalizeCommandToken(cmd)] = true
			})
			for _, tool := range observedTools(cmd, row.Payload) {
				if vetoed[tool] {
					continue
				}
				// LATEST, not earliest. suppressResolved asks an EXISTENTIAL — "is
				// there a green at a seq above this candidate's?" — and max is that
				// question's reduction; min answers the stricter "is the FIRST green
				// above it?", which is wrong for green → miss → green. Measured:
				// helm green @10, miss @30, helm green @50 kept reporting helm under
				// min, though helm demonstrably ran after the miss. The two agree
				// everywhere except that shape, which is why fixture case 10 is the
				// gate here and not a unit assertion on the map.
				if at, ok := green[tool]; !ok || row.Seq > at {
					green[tool] = row.Seq
				}
			}
		}
	}
	return green
}

// observedTools derives the executables a green tool_result proves ran, from two
// channels: the originating command's executable positions, and — only for an
// npm/pnpm/yarn/bun `run` invocation — the script lines the package manager echoes
// into its own output.
func observedTools(command string, payload []byte) []string {
	out := executablesIn(command)
	if isScriptRunnerCommand(command) {
		out = append(out, scriptEchoTools(toolResultText(payload))...)
	}
	return out
}

// scriptRunners are the package managers whose `run` subcommand echoes the wrapped
// command into stdout, which is what makes the script-echo channel possible at all.
var scriptRunners = map[string]bool{"npm": true, "pnpm": true, "yarn": true, "bun": true}

// execWrappers run their next argument AS the executable, so that argument is itself
// in executable position (`npx vitest`, `bunx tsc`).
var execWrappers = map[string]bool{"npx": true, "bunx": true}

// envAssignment matches a `FOO=bar` prefix, which precedes the executable rather than
// being one.
var envAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// reScriptEcho matches a package manager's echo of the command it is about to run.
// npm and pnpm prefix with `> `, yarn 1 and bun with `$ `. The `> pkg@1.0.0 typecheck`
// header yields a junk token that can never match a miss candidate — harmless, and
// cheaper than trying to pick the right line out of the pair.
//
// Applied per line inside the header block only — see judgeScriptEchoMaxLines.
var reScriptEcho = regexp.MustCompile(`^[ \t]*[>$][ \t]+(\S+)`)

// judgeScriptEchoMaxLines bounds how far into a result the script-echo channel reads.
//
// 🔴 SCANNING THE WHOLE RESULT LETS A TEST'S OWN OUTPUT FAKE A GREEN. Measured: an
// `npm run test` whose vitest output contains a docs golden printing
// `    $ shellcheck x.sh` marked shellcheck green and suppressed a real miss. Any
// suite printing a shell transcript does this — CLI usage goldens, README-driven
// tests, markdown snapshots — and uzi's own repo has a CLI with docs, so this is live
// rather than hypothetical.
//
// A package manager's echo lives in the first few lines and nowhere else: npm and pnpm
// emit a blank line, the `> pkg@1.0.0 script` header, the `> command` line, then a
// blank line; yarn 1 emits `yarn run v1.x` then `$ command`; bun emits `$ command`
// first. Five lines covers all four with room to spare, and the blank-line stop ends
// the block earlier still for npm/pnpm — yarn is the one that needs the line cap,
// since its command output follows with no blank line between.
//
// Residual: a nested script (`npm run build` whose script runs `npm run compile`)
// pushes the inner echo past this window, so its tool is not observed. That
// under-suppresses, which is the safe direction.
const judgeScriptEchoMaxLines = 5

// executablesIn returns the basenamed executables in EXECUTABLE POSITION of a shell
// command: the first token, tokens after a command separator, and the token an exec
// wrapper wraps.
//
// 🔴 NOT strings.Contains, and that distinction is the point. `grep tsc package.json`
// mentions tsc without running it; counting that as "tsc ran green" would suppress a
// genuine miss, which is the over-suppression PRD #121's risk section names.
func executablesIn(command string) []string {
	var out []string
	for _, pair := range execPositions(command) {
		if name := basenameToken(pair[0]); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// isScriptRunnerCommand reports whether the command is an `npm|pnpm|yarn|bun run …`
// invocation — the only shape whose output the script-echo channel may be read from.
// The runner must itself be in executable position, so `grep "npm run" Makefile` does
// not unlock the channel for grep's output.
//
// Residual, stated rather than discovered later: the no-`run` aliases (`npm test`,
// `npm start`) echo identically and are NOT matched here. That is the conservative
// direction — it can only leave a miss reported, never suppress a real one — and it
// follows the shape the design note prescribed.
func isScriptRunnerCommand(command string) bool {
	for _, pair := range execPositions(command) {
		if scriptRunners[basenameToken(pair[0])] && pair[1] == "run" {
			return true
		}
	}
	return false
}

// scriptEchoTools pulls the wrapped executables out of a package manager's echoed
// script lines. Needs REAL line boundaries, which is why it takes decoded text rather
// than the raw jsonb payload the regex scan reads (there, a newline is a literal \n).
func scriptEchoTools(text string) []string {
	var out []string
	seenContent := false
	for i, line := range strings.SplitN(text, "\n", judgeScriptEchoMaxLines+1) {
		if i >= judgeScriptEchoMaxLines {
			break // the final piece is the unsplit remainder; never inspect it
		}
		if strings.TrimSpace(line) == "" {
			if seenContent {
				break // the header block ended; the rest is the command's own output
			}
			continue // npm and pnpm open with a blank line
		}
		seenContent = true
		if m := reScriptEcho.FindStringSubmatch(line); m != nil {
			if name := basenameToken(m[1]); name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}

// execPositions returns each executable-position token of a shell command paired with
// the token that immediately follows it (or "" at the end) — the follower is what
// distinguishes `npm run lint` from `npm ci`.
func execPositions(command string) [][2]string {
	toks := shellTokens(command)
	var out [][2]string
	execNext := true
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if shellSeparators[t] {
			execNext = true
			continue
		}
		if !execNext {
			continue
		}
		if envAssignment.MatchString(t) {
			continue // a FOO=bar prefix; the executable is still ahead
		}
		execNext = false
		next := ""
		if i+1 < len(toks) && !shellSeparators[toks[i+1]] {
			next = toks[i+1]
		}
		out = append(out, [2]string{t, next})

		switch name := basenameToken(t); {
		case execWrappers[name]:
			execNext = true
		case scriptRunners[name] && (next == "exec" || next == "dlx"):
			// `dlx` is pnpm's and Berry's npx, so it wraps exactly like `exec`.
			// The set is deliberately narrow otherwise: `time tsc`, `sudo helm`,
			// `xargs -n1 tsc`, `sh -c "tsc"` all leave the tool unobserved, which
			// under-suppresses — the safe direction this parser commits to.
			i++ // consume the subcommand; `pnpm exec tsc` puts tsc in exec position
			execNext = true
		}
	}
	return out
}

// shellSeparators are the tokens that begin a new command within one Bash invocation,
// so whatever follows is back in executable position. `(` and `)` are included
// because a subshell or command substitution also starts one.
var shellSeparators = map[string]bool{
	"|": true, "||": true, "&": true, "&&": true, "|&": true,
	";": true, ";;": true, "\n": true, "(": true, ")": true,
}

// shellTokens splits a command into tokens, emitting the separators above as tokens
// of their own even when unspaced, and treating a quoted region as opaque (never a
// separator, never a split point).
//
// Deliberately a POSITION parser, not a shell. Anything it cannot resolve degrades to
// "not in executable position", which leaves a miss REPORTED — the conservative
// direction, since over-suppression is the risk PRD #121 names and under-suppression
// only preserves today's behaviour.
//
// 🔴 A HEREDOC LATCHES OFF EVERY SEPARATOR FOR THE REST OF THE COMMAND, and that is a
// correctness fix, not tidiness. `\n` is a genuine command separator, so without this
// every line of a heredoc BODY put its first word in executable position:
//
//	cat > build.sh <<'EOF'
//	tsc --noEmit
//	EOF
//
// parsed as [cat tsc EOF], the write succeeded, and a GENUINE `tsc` miss was suppressed
// by an agent merely writing tsc into a file. Silent, and strictly worse than the false
// positive this milestone removes. The two events are correlated rather than
// independent: the tool an agent just failed to invoke is exactly the tool likely to
// appear in the wrapper script it writes next.
//
// Latching off `; | & ( )` too, not just `\n`, is deliberate — a body line reading
// `foo; tsc` would otherwise reopen the same hole one level down. Everything after a
// `<<` is data as far as this scan is concerned, so no later token on that command can
// be an executable. That under-suppresses a real command following a heredoc, which is
// the safe direction and the philosophy stated above.
func shellTokens(command string) []string {
	var toks []string
	var cur strings.Builder
	// heredoc latches true at the first `<<` (or `<<-`) and never clears: from there
	// the remainder of this command yields data tokens only.
	heredoc := false
	flush := func() {
		if cur.Len() > 0 {
			toks = append(toks, cur.String())
			cur.Reset()
		}
	}
	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		switch c := runes[i]; c {
		case '\'', '"':
			for i++; i < len(runes) && runes[i] != c; i++ {
				cur.WriteRune(runes[i])
			}
		case '\\':
			if i+1 < len(runes) {
				i++
				if runes[i] != '\n' {
					cur.WriteRune(runes[i])
				}
			}
		case '<':
			cur.WriteRune(c)
			// `<<` and `<<-` open a heredoc; a single `<` is an ordinary redirect and
			// `<<<` (herestring) latches too, which costs only under-suppression.
			if i+1 < len(runes) && runes[i+1] == '<' {
				cur.WriteRune('<')
				i++
				heredoc = true
			}
		case ' ', '\t', '\r':
			flush()
		case '\n', ';', '|', '&', '(', ')':
			// `2>&1` is a redirect, not a separator. Treating it as one flushed `2>`
			// and put the trailing `1` in executable position — a phantom executable
			// per redirect. Harmless downstream (noisyShToken drops pure digits, so it
			// could never match a candidate) but the parser should not invent it.
			if c == '&' && strings.HasSuffix(cur.String(), ">") {
				cur.WriteRune(c)
				continue
			}
			flush()
			if heredoc {
				continue // structure inside heredoc data is not structure
			}
			sep := string(c)
			if i+1 < len(runes) && (c == ';' || c == '|' || c == '&') &&
				(runes[i+1] == '|' || runes[i+1] == '&' || (c == ';' && runes[i+1] == ';')) {
				sep += string(runes[i+1])
				i++
			}
			toks = append(toks, sep)
		default:
			cur.WriteRune(c)
		}
	}
	flush()
	return toks
}

// basenameToken reduces a path form to the executable name (`./node_modules/.bin/tsc`
// → `tsc`) and rejects anything that is plainly not one (a flag, an empty token).
func basenameToken(tok string) string {
	tok = normalizeCommandToken(tok)
	if tok == "" || strings.HasPrefix(tok, "-") {
		return ""
	}
	if i := strings.LastIndexByte(tok, '/'); i >= 0 {
		tok = tok[i+1:]
	}
	return tok
}

// toolUseCommand extracts (block id, Bash command) from a tool_use payload. The
// shapes come from the worker's sdk-messages mapping (tool_use carries {id, name,
// input}); a non-Bash tool has no input.command and yields "". JSON-parsed, never
// regexed over raw bytes — the toolCallHash / toolUseID precedent in health.go.
func toolUseCommand(payload []byte) (string, string) {
	var p struct {
		ID    string `json:"id"`
		Input struct {
			Command string `json:"command"`
		} `json:"input"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", ""
	}
	return p.ID, p.Input.Command
}

// toolResultOutcome extracts (originating tool_use id, is_error) from a tool_result
// payload. is_error is normalized worker-side to a strict boolean, but ABSENT must
// read as false for rows written by any other producer. An unparseable payload
// reports an error so it can never be counted as green.
func toolResultOutcome(payload []byte) (string, bool) {
	var p struct {
		ToolUseID string `json:"tool_use_id"`
		IsError   bool   `json:"is_error"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", true
	}
	return p.ToolUseID, p.IsError
}

// toolResultText decodes a tool_result payload's content into plain text. The SDK
// passes content through as-is, so it is either a string or an array of content
// blocks; both shapes are handled and anything else yields "".
func toolResultText(payload []byte) string {
	var p struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(payload, &p); err != nil || len(p.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(p.Content, &s); err == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(p.Content, &blocks); err != nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(blk.Text)
	}
	return b.String()
}

// judgeSecretID reads the user's judge-lane Anthropic binding as openAnthropic's
// override: nil when they have named none, which resolves their default token
// (PRD #104 M4).
//
// A lookup error is propagated, never swallowed into "use the default": treating a
// failed read as "unbound" would silently spend the wrong account, which is R4's
// failure mode — a resolution bug that costs money and raises nothing. The claim
// fails instead, and the run is retried.
func (s *Service) judgeSecretID(ctx context.Context, userID uuid.UUID) (*uuid.UUID, error) {
	bound, err := s.q.GetUserJudgeAnthropicSecret(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("judge token binding lookup: %w", err)
	}
	if !bound.Valid {
		return nil, nil
	}
	id := uuid.UUID(bound.Bytes)
	return &id, nil
}

// assembleJudgeClaim builds the claim payload for a judge run (PRD #46 Decisions 1, 3
// & 4). Unlike the ordinary run lane it does NOT call GetRunClaimContext (no repo, no
// forge connection) and NEVER opens the bot PAT: least privilege, and a judge must
// not spuriously fail when the reviewed run's forge connection is gone (audit H2). It
// delivers the run owner's Anthropic token (vault-aware openAnthropic), the reviewed
// run's id (its trace is fetched out-of-band), the judge model, and the deterministic
// command-not-found pre-scan. The trace itself never rides the claim (it can be MB).
func (s *Service) assembleJudgeClaim(ctx context.Context, run store.Run) (*ClaimPayload, error) {
	// The run owner's JUDGE binding, falling back to their default — and deliberately
	// NOT the claiming worker's binding, even though a judge run is claimed by an
	// ordinary worker through the same ClaimRun lane. Under PRD #104 D1 the judge
	// lane is bound per USER: which credential reviews your work is a property of
	// you, not of whichever worker happened to pick the retrospective up, which would
	// otherwise bill the same retrospective to different accounts run to run for no
	// reason a user could see. Self-improve runs ride this same path and so follow
	// the same binding.
	judgeSecret, err := s.judgeSecretID(ctx, run.UserID)
	if err != nil {
		return nil, err
	}
	cred, err := s.openAnthropic(ctx, run.UserID, judgeSecret)
	if err != nil {
		return nil, err
	}
	// The judge lane records what it spent exactly as the run lane does (PRD #111
	// M1). It is the lane where this matters MOST: the judge binding exists so
	// retrospectives can be billed to a different account, and until now nothing in
	// the data said whether they actually were.
	//
	// Its reason is `judge`, not `pinned` (M4 closed the vocabulary). D20 makes the
	// run view name the MODE, and "pinned" would send a user looking for a worker
	// binding that does not exist — the choice was made by their judge setting, on a
	// different page. An UNBOUND judge lane records `default`, honestly: the owner's
	// default really did pay.
	if err := s.recordRunCredential(ctx, run, cred, staticChoice(judgeSecret, selectReasonJudge)); err != nil {
		return nil, err
	}
	anthropic := cred.Token

	var targetRunID *string
	var signal *JudgeSignal
	if run.TargetRunID.Valid {
		id := uuid.UUID(run.TargetRunID.Bytes).String()
		targetRunID = &id
		signal = s.judgeSignal(ctx, uuid.UUID(run.TargetRunID.Bytes))
	}

	var judgeModel *string
	if s.settings != nil {
		if m, err := s.settings.JudgeModel(ctx); err == nil && strings.TrimSpace(m) != "" {
			judgeModel = &m
		}
	}

	// The owner's existing improve_uzi target coordinates (issue #232), read best-effort:
	// a lookup failure must NOT fail the claim — the menu is an optimization that helps the
	// judge reuse an exact target string, and the review still lands without it. Warn and
	// proceed with a nil menu, mirroring judgeSignal's best-effort posture above. Owner-
	// scoped (run.UserID), so it can never carry another user's targets.
	var knownTargets []string
	if kt, err := s.q.ListKnownImproveUziTargetsForUser(ctx, store.ListKnownImproveUziTargetsForUserParams{
		UserID: run.UserID,
		Lim:    judgeKnownTargetsCap,
	}); err != nil {
		slog.Warn("judge claim: list known improve_uzi targets", "user", run.UserID.String(), "error", err)
	} else {
		knownTargets = kt
	}

	return &ClaimPayload{
		RunID:                  run.ID.String(),
		Kind:                   run.Kind,
		IssueTitle:             run.IssueTitle,
		IssueDescription:       run.IssueDescription,
		Status:                 run.Status,
		TargetRunID:            targetRunID,
		JudgeModel:             judgeModel,
		JudgeSignal:            signal,
		KnownImproveUziTargets: knownTargets,
		SessionID:              textPtr(run.SessionID),
		LastSeq:                run.LastSeq,
		IterationCount:         run.IterationCount,
		RequeueCount:           run.RequeueCount,
		Secrets: ClaimSecrets{
			// ForgeUsername/ForgePAT are left empty by design: a judge never touches a
			// repo. The wire still carries the (empty) forge_pat key because a judge run
			// rides the ordinary ClaimPayload; the no-PAT guarantee is that assembly
			// never decrypts one, asserted in judge_test.go.
			AnthropicOAuthToken: string(anthropic),
		},
		Agents:        []ClaimAgent{},
		Skills:        []ClaimSkill{},
		SkillsDropped: []ClaimSkillDrop{},
		Config: ClaimConfig{
			RunTimeoutSeconds:      int(s.p.RunTimeout.Seconds()),
			IdleTimeoutSeconds:     int(s.p.RunIdleTimeout.Seconds()),
			MaxIterations:          s.p.RunMaxIterations,
			PlanMaxRevisions:       s.p.PlanMaxRevisions,
			QuestionMax:            s.p.QuestionMax,
			QuestionTimeoutSeconds: s.p.QuestionTimeoutSeconds,
			ToolPackages:           []string{},
		},
	}, nil
}

// judgeSignal runs the deterministic command-not-found scan over the reviewed run's
// tool trace (PRD #46 Decision 4), off the hot request path (it runs at claim
// assembly, a separate worker poll). Best-effort — a scan error never fails the claim;
// the judge still runs, it just loses the deterministic hint. Returns nil when there
// is nothing to report so the claim omits the signal entirely.
//
// Two passes over one fetch (PRD #121 M3): the not-found scan reads tool_result rows
// only, while the green index reads both kinds and joins them by tool_use_id. The
// order is scan → index → suppress, and the wire contract is unchanged — a suppressed
// miss is simply absent from MissingTools.
func (s *Service) judgeSignal(ctx context.Context, targetID uuid.UUID) *JudgeSignal {
	rows, err := s.q.ListToolTraceForRun(ctx, store.ListToolTraceForRunParams{
		RunID: targetID,
		Lim:   judgeToolTraceRowCap,
	})
	if err != nil {
		slog.Warn("judge signal: list tool trace", "target", targetID, "error", err)
		return nil
	}
	misses := suppressResolved(scanCommandNotFound(rows), observedGreenTools(rows))
	if len(misses) == 0 {
		return nil
	}
	return &JudgeSignal{MissingTools: misses}
}
