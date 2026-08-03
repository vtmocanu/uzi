package agenttmpl

import (
	"regexp"
	"strings"
	"testing"
)

// This file exists SEPARATELY from render_test.go on purpose. Issue #205 rewrites
// that file's phrase-pin region model wholesale, and these assertions are about a
// different property entirely (who a subagent addresses, not which behaviours the
// lead's prose retains). Keeping them apart makes the two changes disjoint by file
// rather than by luck, and means nothing here depends on render_test.go's helpers.

// reachableRecipient is the ONLY agent name a builtin subagent can address.
//
// `assembleAgents` (agent/src/agents.ts) routes the template matching
// /^(lead|orchestrator)$/i onto the MAIN THREAD and never registers it as an
// invokable subagent — its own header says so: "its prompt_body becomes the
// main-thread system prompt and it is NOT also registered as an invokable
// subagent". So a body that says "SendMessage to the team lead" names something
// that does not exist, and the SDK answers "No agent named 'lead' is reachable."
//
// Measured on three real run traces (issue #210): 8 of 26 SendMessage calls were
// addressed to `lead` and failed. Five subagents re-sent to `main` at a cost of
// ~9.6 KB of regenerated report plus three tool round-trips each; the other three
// abandoned SendMessage and returned the report through the Agent tool's return
// value, each prefixed with an apology about uzi's own plumbing.
//
// SCOPE, and it is narrower than "lead is unreachable in a uzi run": the repo
// source path (`subagentsFromTemplates`, PRD #37 Decision 3) DELIBERATELY does
// register a repo file named `lead` as an invokable subagent. That is why these
// assertions run over the BUILTIN roster only — a builtin body executes on the
// own/claim-payload path, where the partition above holds.
const reachableRecipient = "main"

// recipientLiteral is how a template must SPELL the recipient: backticked, so it
// reads as an identifier rather than as a word.
//
// The backticks are load-bearing rather than cosmetic, and this was measured, not
// assumed. A first version of this file tested for the bare substring "main" and
// three builtins passed it while naming no recipient at all — `reviewer.md:27`
// via "re-MAIN-ing references", and `web-ux.md:43` via an agent-browser flag
// value, `snapshot -i -s "main"`. Both are the vacuous-pass shape CLAUDE.md warns
// about: the assertion was satisfied by text that has nothing to do with the
// property. Requiring the delimiters removes the whole class.
const recipientLiteral = "`" + reachableRecipient + "`"

// sendMessageTarget captures the token following "SendMessage to". A body saying
// "…to the team lead" captures "the", which is what makes this fail closed.
var sendMessageTarget = regexp.MustCompile("SendMessage to ([^\\s,.:;]+)")

// collapseWS collapses every whitespace run to a single space so an assertion
// matches prose regardless of where the source happens to hard-wrap. Deliberately
// NOT render_test.go's `flatten` — see the file comment on why this file carries
// its own copy.
func collapseWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// TestBuiltinsAddressAReachableRecipient is the regression guard for issue #210.
//
// The three rules below fail in three different ways on purpose, because the
// defect arrived in three different phrasings and a single check caught only one
// of them:
//
//	rule A — "SendMessage to <someone-else>"     (nine of the thirteen sites)
//	rule B — "team-lead" as a bare address        (tester.md's three sites)
//	rule C — "SendMessage" with NO recipient      (spec-keeper.md's two sites)
//
// Rule C is what stops the fix being cosmetic: retiring the wrong name without
// supplying the right one leaves the agent guessing, and rules A and B would both
// be satisfied by that.
func TestBuiltinsAddressAReachableRecipient(t *testing.T) {
	for _, def := range Builtins() {
		body := collapseWS(def.PromptBody)

		// Rule A. Every explicit "SendMessage to X" must name the reachable agent.
		for _, m := range sendMessageTarget.FindAllStringSubmatch(body, -1) {
			got := strings.Trim(m[1], "`")
			if got != reachableRecipient {
				t.Errorf("%s: addresses %q; the only reachable recipient is %q\n  in: %q",
					def.Name, got, reachableRecipient, m[0])
			}
		}

		// Rule B. `team-lead` is never a real agent name and only ever appears as
		// an address. Prose may still call the role "the lead"; it may not spell it
		// as though it were an addressable identifier.
		if strings.Contains(body, "team-lead") {
			t.Errorf("%s: contains %q, which is not an addressable agent name; "+
				"address %q and call the role \"the lead\" in prose",
				def.Name, "team-lead", reachableRecipient)
		}

		// Rule C. Every mention of the tool must carry its recipient nearby, so a
		// body cannot say "Report via SendMessage:" and leave the agent to guess.
		// The window is generous: it only has to span the recipient itself.
		//
		// It reaches BOTH WAYS, and that is a correction rather than a nicety. A
		// first version looked only forward and reddened tester.md for writing
		// "send `main` … ONE structured message via SendMessage" — a perfectly
		// clear instruction in which the recipient simply precedes the tool name.
		// A directional window silently demands one English word order, which is
		// a property of the assertion and not of the template.
		const window = 48
		for _, idx := range allIndexes(body, "SendMessage") {
			start := idx - window
			if start < 0 {
				start = 0
			}
			end := idx + len("SendMessage") + window
			if end > len(body) {
				end = len(body)
			}
			if !strings.Contains(body[start:end], recipientLiteral) {
				t.Errorf("%s: a SendMessage instruction names no recipient within %d chars; "+
					"it must name %s\n  in: %q",
					def.Name, window, recipientLiteral, body[start:end])
			}
		}
	}
}

// TestBuiltinsNameTheRecipientTheyCanActuallyReach is the POSITIVE half of the
// pair above, and it is not redundant with it.
//
// CLAUDE.md documents the failure this guards against: a negative assertion about
// a retired string goes VACUOUS the moment nothing renders that string, and then
// passes forever regardless of what the template says. Rules A-C are all negative
// in that sense. This one fails if the recipient is simply absent, so the pair
// covers both "the wrong name came back" and "no name is there at all".
func TestBuiltinsNameTheRecipientTheyCanActuallyReach(t *testing.T) {
	var checked int
	for _, def := range Builtins() {
		if !strings.Contains(def.PromptBody, "SendMessage") {
			continue
		}
		checked++
		if !strings.Contains(def.PromptBody, recipientLiteral) {
			t.Errorf("%s: instructs a SendMessage but never names %s, so the agent has "+
				"nothing to address", def.Name, recipientLiteral)
		}
	}
	// A positive control on the test itself: if a refactor ever stops builtins
	// mentioning SendMessage, this test silently checks nothing and must say so
	// rather than pass. `lead` and any inherit-all role need not mention it, so
	// the bar is "several", not "all".
	if checked < 5 {
		t.Fatalf("only %d builtins mention SendMessage; this test has stopped "+
			"exercising the roster it exists to guard", checked)
	}
}

// allIndexes returns every start index of sub in s. strings.Index alone finds only
// the first, which would let a second bad site hide behind a good one.
func allIndexes(s, sub string) []int {
	var out []int
	for off := 0; ; {
		i := strings.Index(s[off:], sub)
		if i < 0 {
			return out
		}
		out = append(out, off+i)
		off += i + len(sub)
	}
}
