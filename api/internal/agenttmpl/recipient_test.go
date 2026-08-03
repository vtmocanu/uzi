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
// invokable subagent — that file's HEADER comment (agents.ts:9-14, not
// assembleAgents's own doc comment) says so: "its prompt_body becomes the
// main-thread system prompt and it is NOT also registered as an invokable
// subagent". So a body that says "SendMessage to the team lead" names something
// that does not exist, and the SDK answers "No agent named 'lead' is reachable."
//
// Measured on three real run traces (issue #210): 8 of 26 SendMessage calls were
// addressed to `lead` and failed. Five subagents re-sent to `main`; three of the
// five paid three recovery round-trips (ToolSearch → TaskList → re-send) and the
// other two re-sent immediately, and the regenerated reports ranged 0.4-10.0 KB,
// 30 KB in total. The other three abandoned SendMessage and returned the report
// through the Agent tool's return value, each prefixed with an apology about
// uzi's own plumbing.
//
// COUNT, corrected: the fix touched 15 recipient sites, not 13. Thirteen is the
// number of SendMessage occurrences (rule A's iteration count); two more lines
// name the addressee with no SendMessage token on them at all, and those are
// precisely the two that rules A-C are blind to — see rule D.
//
// SCOPE, and it is narrower than "lead is unreachable in a uzi run": the repo
// source path (`subagentsFromTemplates`, PRD #37 Decision 3) DELIBERATELY does
// register a repo file named `lead` as an invokable subagent. That is why these
// assertions run over the BUILTIN roster only — a builtin body executes on the
// own/claim-payload path, where the partition above holds.
//
// This is a DESCRIPTION of what the SDK registers, not an invariant uzi enforces:
// nothing constrains SendMessage's `to`, and `main` appears nowhere in agent/src/
// because it is an SDK-supplied identity. Making it an enforced invariant (a
// PreToolUse matcher) is a behaviour change, filed separately.
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
// property.
const recipientLiteral = "`" + reachableRecipient + "`"

// recipientClause is the exact text that must FOLLOW the tool name. Adjacency,
// not proximity: see rule A.
const recipientClause = " to " + recipientLiteral

// channelClause is what an address-shaped mention of the role must be followed by
// (rule D). Naming the role is fine; naming it without the channel is what leaves
// a cold-starting agent addressing a name that does not resolve.
const channelClause = " via SendMessage" + recipientClause

// addressAfterTool captures the token a body addresses when it writes
// "SendMessage to X" and X is not the reachable recipient. Anchored at the start
// of the text FOLLOWING the tool name, so it only ever describes an adjacency
// failure — it is a diagnostic, not a second rule.
var addressAfterTool = regexp.MustCompile(`^ to ([^\s,.:;]+)`)

// teamLeadAddress matches the defect's own spelling in address position:
// "to the team lead", "to team-lead", and the hyphen/space variants between.
// Deliberately case-insensitive — the phrasing is what matters, not the casing.
var teamLeadAddress = regexp.MustCompile(`(?i)to (the )?team[- ]lead`)

// leadAddress matches the surviving role word in address position. Unlike the
// pattern above this one is NOT banned outright: rule D requires it to carry the
// channel, which is how "escalate to the lead" becomes actionable rather than
// unreachable.
var leadAddress = regexp.MustCompile(`(?i)to the lead`)

// collapseWS collapses every whitespace run to a single space so an assertion
// matches prose regardless of where the source happens to hard-wrap. Deliberately
// NOT render_test.go's `flatten` — see the file comment on why this file carries
// its own copy. It is what makes ADJACENCY a property of the sentence rather than
// of the line: "SendMessage\nto `main`" and "SendMessage to `main`" are the same
// instruction and must be judged the same way.
func collapseWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// TestBuiltinsAddressAReachableRecipient is the regression guard for issue #210.
//
// FOUR rules, failing in four different ways, because the defect arrived in four
// different phrasings and no single check caught more than one:
//
//	rule A — a SendMessage not immediately followed by "to `main`"
//	rule B — "team-lead" spelled as though it were an addressable identifier
//	rule C — "to the team lead": the defect's own wording, in address position
//	rule D — "to the lead" without the channel named right after it
//
// Rule A REPLACED a 48-char proximity window, and the replacement is the whole
// point of this revision. The window was satisfiable three separate ways by
// sentences anyone would write in this repo — most cheaply by `main` in its
// GIT-BRANCH sense, which is the most repeated backticked token here
// (`builtins/lead.md:61` already carries "never touch `main`"). Both of these
// PASSED the window and RED the adjacency rule:
//
//	Report findings to the team lead via SendMessage; never touch `main`.
//	Report via SendMessage: findings only, and never push to `main`.
//
// Adjacency also folds in a defect the old rules had between them: the old
// address rule stripped backticks before comparing, so a bare "SendMessage to
// main" satisfied it while the window rejected it, producing a "names no
// recipient" message for a body that named one. Here there is one rule and one
// verdict, and `addressAfterTool` only refines the MESSAGE.
//
// Rules C and D exist because two changed sites are invisible to rule A: they
// name the addressee with no SendMessage token on the line at all
// (`fact-checker.md`'s and `tester.md`'s write-approval sentences). Reverting
// either to "the team lead" left every earlier rule silent.
//
// The recipient/referent split is the reason C and D are not one rule. Prose may
// still CALL the role "the lead" — "every dispatch from the team lead"
// (spec-keeper), "the URL the team lead gives you" (web-ux), "the lead
// re-delegates" (tester) are descriptions of who does what and must stay. What
// may not survive is addressing that name, since it is not one the SDK resolves.
//
// Residual, stated rather than implied: rule D sees the "to the …" shape only.
// "PROPOSE that the lead spin up …" and "ask the lead how to …" (web-ux) carry
// the channel today because this commit put it there, and nothing here would
// notice if it were removed.
func TestBuiltinsAddressAReachableRecipient(t *testing.T) {
	for _, def := range Builtins() {
		body := collapseWS(def.PromptBody)

		// Rule A. ADJACENCY: every mention of the tool carries its recipient
		// immediately, so no sentence can put the tool and the word `main` in one
		// window while meaning two unrelated things by them.
		for _, idx := range allIndexes(body, "SendMessage") {
			rest := body[idx+len("SendMessage"):]
			if strings.HasPrefix(rest, recipientClause) {
				continue
			}
			if m := addressAfterTool.FindStringSubmatch(rest); m != nil {
				// Two different failures reach here and they need different
				// messages. "addresses main; the only reachable recipient is
				// `main`" is what the old rule printed for the bare-word form, and
				// it reads as a contradiction rather than as an instruction.
				if strings.Trim(m[1], "`") == reachableRecipient {
					t.Errorf("%s: names the right recipient but spells it %q; it must be "+
						"backticked, exactly %q\n  in: %q",
						def.Name, m[1], recipientClause, excerpt(body, idx))
				} else {
					t.Errorf("%s: addresses %q; the only reachable recipient is %s\n  in: %q",
						def.Name, m[1], recipientLiteral, excerpt(body, idx))
				}
				continue
			}
			t.Errorf("%s: a SendMessage instruction is not immediately followed by %q, "+
				"so it names no recipient\n  in: %q",
				def.Name, recipientClause, excerpt(body, idx))
		}

		// Rule B. `team-lead` is never a real agent name and only ever appeared as
		// an address. Prose may still call the role "the lead"; it may not spell it
		// as though it were an addressable identifier.
		if strings.Contains(body, "team-lead") {
			t.Errorf("%s: contains %q, which is not an addressable agent name; "+
				"address %s and call the role \"the lead\" in prose",
				def.Name, "team-lead", recipientLiteral)
		}

		// Rule C. "to the team lead" was the defect's own wording at nine of the
		// original sites. Banned outright rather than conditionally: once the
		// channel is named the address is `main`, and the role word this repo's
		// surviving prose uses is "the lead".
		for _, loc := range teamLeadAddress.FindAllStringIndex(body, -1) {
			t.Errorf("%s: addresses %q, which no SDK name resolves; "+
				"address %s and keep \"the lead\" as the role word\n  in: %q",
				def.Name, body[loc[0]:loc[1]], recipientLiteral, excerpt(body, loc[0]))
		}

		// Rule D. The role word in address position must carry the channel. This
		// is what covers an outbound imperative that never names the tool —
		// "escalate to the lead", "propose it to the lead" — which rules A-C are
		// all blind to.
		for _, loc := range leadAddress.FindAllStringIndex(body, -1) {
			if strings.HasPrefix(body[loc[1]:], channelClause) {
				continue
			}
			t.Errorf("%s: addresses the role without naming the channel; "+
				"%q must be followed by %q\n  in: %q",
				def.Name, body[loc[0]:loc[1]], channelClause, excerpt(body, loc[0]))
		}
	}
}

// TestBuiltinsNameTheRecipientTheyCanActuallyReach is the POSITIVE half of the
// pair above, and it is not redundant with it.
//
// CLAUDE.md documents the failure this guards against: a negative assertion about
// a retired string goes VACUOUS the moment nothing renders that string, and then
// passes forever regardless of what the template says. Rules A-D are all negative
// in that sense. This one fails if the recipient is simply absent, so the pair
// covers both "the wrong name came back" and "no name is there at all".
func TestBuiltinsNameTheRecipientTheyCanActuallyReach(t *testing.T) {
	all := Builtins()
	var checked int
	for _, def := range all {
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
	// rather than pass.
	//
	// The bar is DERIVED from the roster rather than hardcoded, and that was
	// measured: a literal floor of 5 left half the roster of headroom, so
	// stripping five builtins kept this control silent. Ten of the eleven mention
	// SendMessage today — `lead` is the exception, because it runs on the main
	// thread and has nobody to report to — so a floor of len-2 leaves exactly one
	// role of slack for a future role that legitimately never reports, and grows
	// with the roster instead of drifting away from it.
	if want := len(all) - 2; checked < want {
		t.Fatalf("only %d of %d builtins mention SendMessage, want at least %d; this "+
			"test has stopped exercising the roster it exists to guard",
			checked, len(all), want)
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

// excerpt returns the text around idx, so a failure names the sentence rather than
// only the rule. A window is the wrong instrument for DECIDING (that is rule A's
// whole point) and the right one for REPORTING.
func excerpt(s string, idx int) string {
	const span = 48
	start := idx - span
	if start < 0 {
		start = 0
	}
	end := idx + span
	if end > len(s) {
		end = len(s)
	}
	return s[start:end]
}
