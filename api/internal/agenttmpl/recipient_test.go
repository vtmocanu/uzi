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
// Those two figure sets describe DIFFERENT populations and must never be summed:
// 0.4-10.0 KB / 30 KB total is the five RE-SENT reports, while the three abandoned
// ones are 6468 / 6251 / 4400 chars (run `84b6a933`, seq 337 / 994 / 2454). They
// sit close enough on the page to be added by someone skimming.
//
// COUNT, corrected twice. `4fde2088` touched 15 recipient sites, not the 13 its
// commit message claims: 13 was the number of SendMessage occurrences at that
// commit, and two further lines name the addressee with no SendMessage token on
// them at all. Those two are what RULE C catches — not rule D, which never sees
// them, because both name `main` in backticks with no "lead" on the line at all.
//
// The occurrence count is NOT frozen at 13, and this paragraph used to say it was,
// in the present tense, about a loop 130 lines below. What rule A actually iterates
// has gone 13 → 21 → 23 across the three commits of this one issue: 13 at
// `4fde2088`, 21 at `98cabb06` (which added eight in the same commit that wrote
// "thirteen"), and 23 here (architect 4, spec-keeper 4, documenter 3, web-ux 3,
// coder 2, fact-checker 2, tester 2, auditor 1, researcher 1, reviewer 1, lead 0).
// That drift is the general hazard rather than a slip: a count derived from the
// roster goes stale the moment the roster moves — and it moves fastest in the
// commit doing the moving. Derive it (`len(all)-2` below), or date it as this
// paragraph now does; never state it flat.
//
// SCOPE, and it is narrower than "lead is unreachable in a uzi run": the repo
// source path (`subagentsFromTemplates`, PRD #37 Decision 3) DELIBERATELY does
// register a repo file named `lead` as an invokable subagent. That is why these
// assertions run over the BUILTIN roster only — a builtin body executes on the
// own/claim-payload path, where the partition above holds.
//
// This is a DESCRIPTION of what the SDK registers, not an invariant uzi enforces:
// nothing constrains SendMessage's `to`, and `main` appears nowhere in agent/src/
// AS A RECIPIENT, because it is an SDK-supplied identity. The qualifier is not
// hedging — the sole occurrence of the literal is `runner.ts:649`, the git
// DEFAULT-BRANCH fallback, so the unqualified claim is refuted by the very
// homograph rule A exists to separate. It was established with the qualifier and
// lost it in transcription; a claim can degrade by being shortened rather than by
// being wrong. Making the description an enforced invariant (a PreToolUse
// matcher) is a behaviour change, filed separately.
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

// channelClause is the idiom that makes an address actionable: the role word plus
// the tool plus the name the tool resolves. Rules D and E look for it somewhere in
// the same sentence rather than immediately adjacent, because unlike rule A they
// are not deciding what follows a token — see rule D on why that difference is
// deliberate rather than an inconsistency.
const channelClause = "SendMessage" + recipientClause

// addressAfterTool captures the token a body addresses when it writes
// "SendMessage to X" and X is not the reachable recipient. Anchored at the start
// of the text FOLLOWING the tool name, so it only ever describes an adjacency
// failure — it is a diagnostic, not a second rule.
var addressAfterTool = regexp.MustCompile(`^ to ([^\s,.:;]+)`)

// teamLeadAddress matches the defect's own spelling in address position:
// "to the team lead", "to team-lead", and the hyphen/space variants between.
// Case-insensitive because the phrasing is what matters, not the casing.
//
// The trailing \b is load-bearing and was measured: without it "escalations go to
// the team leadership channel" matches, and the failure then REPORTS the substring
// "to the team lead" — a string the author never wrote. A pattern with no boundary
// produces both a false positive and a diagnostic that misquotes the source, which
// is the worse half: it sends the reader looking for text that is not there.
var teamLeadAddress = regexp.MustCompile(`(?i)to (the )?team[- ]lead\b`)

// leadAddress matches the surviving role word in address position. Unlike the
// pattern above this one is NOT banned outright: rule D requires it to carry the
// channel, which is how "escalate to the lead" becomes actionable rather than
// unreachable.
var leadAddress = regexp.MustCompile(`(?i)to the lead`)

// communicativeVerb is what separates ADDRESSING the lead from merely mentioning
// them after the word "to". Rule D fires only when one appears earlier in the same
// sentence, and that guard was measured rather than assumed: without it, "whether
// to widen the scope is up to the lead" and "on a tie, defer to the lead's
// judgment" both redden, and neither instructs anyone to send anything.
//
// Stems plus optional suffixes rather than a word list, and the trailing \b is
// again load-bearing: "hand" must not match inside "handle".
var communicativeVerb = regexp.MustCompile(
	`(?i)\b(surfac|report|propos|escalat|ask|send|hand|tell|rais|flag|submit|notif|deliver|messag)(e|es|ed|ing|s|y|ies)?\b`)

// approvalGate marks a sentence that stops the agent until somebody says yes.
// Rule E requires such a sentence to name who that somebody is; see rule E for why
// no adjacency rule can cover this.
var approvalGate = regexp.MustCompile(`(?i)\bwait(s|ing)? for (approval|sign-?off|permission)\b`)

// sentenceEnd matches a sentence terminator only when whitespace or end-of-text
// follows it, so `spec-keeper.md`'s "propose human.md changes to the lead …" stays
// ONE sentence. Splitting on every "." puts the verb in the previous fragment and
// silently disarms rule D at that site — a narrowing that reads as a refinement
// and is a hole.
var sentenceEnd = regexp.MustCompile(`[.;:!?](\s|$)`)

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
// FIVE rules, failing in five different ways, because the defect arrived in that
// many phrasings and no single check caught more than one:
//
//	rule A — a SendMessage not immediately followed by "to `main`"
//	rule B — "team-lead" spelled as though it were an addressable identifier
//	rule C — "to the team lead": the defect's own wording, in address position
//	rule D — the role ADDRESSED without the channel in the same sentence
//	rule E — an approval gate that names no approver
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
// TWO PROPERTIES OF RULE A ARE DELIBERATE COSTS RATHER THAN OVERSIGHTS, and both
// were argued rather than inherited.
//
// (1) It mandates ONE ENGLISH WORD ORDER, which is exactly what the comment on the
// deleted window condemned ("a directional window silently demands one English
// word order, which is a property of the assertion and not of the template").
// `tester.md`'s own earlier phrasing, "send `main` … ONE structured message via
// SendMessage", is red under this rule and was fine under the old one. The trade
// was taken knowingly: a bidirectional window cannot distinguish `main`-the-branch
// from `main`-the-recipient — that is what let three plausible sentences through —
// while a required word order can, because "SendMessage to `main`" has no reading
// in which `main` is a branch. Word order is a property of the assertion, and it
// is the only property available that separates the two senses. The template pays
// one wording constraint; the alternative was a guard that passed the defect.
//
// (2) It is ABSOLUTE: a body may not mention the tool at all without addressing
// it, so a sentence like "If SendMessage fails, return the report as your final
// message instead" is red despite being a description rather than an instruction —
// and despite describing what three subagents actually did in these traces. The
// obvious exemption is "a mention not preceded by via/through/using", and it was
// rejected on inspection: that discriminator is a proximity heuristic of exactly
// the class this revision deleted, and it fails OPEN on the most natural way to
// reintroduce the bug. "Use SendMessage to report to the team lead" carries none
// of those three prepositions, so the exemption would wave through a sentence that
// is precisely the defect. The cost of absoluteness is bounded and loud: an author
// who wants the fallback sentence writes it without the token ("if the message
// cannot be delivered, return the report as your final message instead") and finds
// out at gate time, with the offending text quoted. The cost of the exemption is
// silent and unbounded.
//
// Rules C, D and E exist because rule A cannot see a site that never names the
// tool — including the two `fact-checker.md` / `tester.md` write-approval
// sentences this issue changed.
//
// The recipient/referent split is the reason C and D are not one rule. Prose may
// still CALL the role "the lead" — "every dispatch from the team lead"
// (spec-keeper), "the URL the team lead gives you" (web-ux), "the lead
// re-delegates" (tester) are descriptions of who does what and must stay. What
// may not survive is addressing that name, since it is not one the SDK resolves.
//
// Residual, stated rather than implied: rule D keys on "to the …", so an address
// built without it — "PROPOSE that the lead spin up …", "ask the lead how to …",
// both in `web-ux.md` — is seen by nothing here. Both carry the channel today
// because issue #210's follow-up put it there; stripping it is green, twice.
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

		// Rule D. The role word in ADDRESS position must carry the channel. This is
		// what covers an outbound imperative that never names the tool — "escalate
		// to the lead", "propose it to the lead" — which rules A-C are all blind to.
		//
		// Two narrowings separate an address from a mention, and the rule is wrong
		// without either. A communicative verb must appear EARLIER IN THE SAME
		// SENTENCE, because "up to the lead" and "defer to the lead's judgment" are
		// ordinary prose that no channel belongs in. And the channel is accepted
		// ANYWHERE in that sentence rather than immediately after, because
		// "(the lead's conversation)" is this roster's house gloss at eight sites,
		// so "Report to the lead's conversation via SendMessage to `main`" is a
		// correct sentence that an adjacency test reddens.
		//
		// That is the same latitude rule A refuses, and the asymmetry is the point:
		// rule A decides what may FOLLOW a token whose two senses are otherwise
		// indistinguishable, while rule D decides whether a sentence names a channel
		// at all. Only the first question needs word order to answer it.
		for _, loc := range leadAddress.FindAllStringIndex(body, -1) {
			sentence := enclosingSentence(body, loc[0])
			if !communicativeVerb.MatchString(body[sentence[0]:loc[0]]) {
				continue // a mention, not an address
			}
			if strings.Contains(body[sentence[0]:sentence[1]], channelClause) {
				continue
			}
			t.Errorf("%s: addresses the role without naming the channel; a sentence "+
				"containing %q must also contain %q\n  in: %q",
				def.Name, body[loc[0]:loc[1]], channelClause, body[sentence[0]:sentence[1]])
		}

		// Rule E. An approval gate names its approver.
		//
		// This is the one failure NO adjacency rule can reach, and it is the failure
		// the proximity rule rule A replaced was built for: "retiring the wrong name
		// without supplying the right one leaves the agent guessing." Both write-
		// approval sentences (`fact-checker.md`, `tester.md`) survive DELETION of
		// their recipient with every other rule silent — the tool token goes with
		// it, so rule A sees nothing, and no "lead" remains for C or D. What does
		// survive the deletion is the gate itself, so that is what this keys on.
		//
		// The property is not about this issue at all: an instruction to stop and
		// wait for a yes is unusable without saying whose yes, whatever the wording.
		for _, loc := range approvalGate.FindAllStringIndex(body, -1) {
			sentence := enclosingSentence(body, loc[0])
			if strings.Contains(body[sentence[0]:sentence[1]], recipientLiteral) {
				continue
			}
			t.Errorf("%s: an approval gate names no approver; a sentence containing %q "+
				"must also name %s\n  in: %q",
				def.Name, body[loc[0]:loc[1]], recipientLiteral, body[sentence[0]:sentence[1]])
		}
	}
}

// TestBuiltinsNameTheRecipientTheyCanActuallyReach is the POSITIVE half of the
// pair above, and it is not redundant with it.
//
// CLAUDE.md documents the failure this guards against: a negative assertion about
// a retired string goes VACUOUS the moment nothing renders that string, and then
// passes forever regardless of what the template says. Rules A-E are all negative
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

// enclosingSentence returns the [start, end) bounds of the sentence containing idx.
// Rules D and E both need it, and both need the SAME bounds: the verb that makes an
// address an address, and the channel or approver that makes it usable, are related
// to each other by sharing a sentence and by nothing else.
//
// It reports bounds rather than a substring so a caller can search only the text
// BEFORE the match (rule D wants the verb earlier in the sentence, not anywhere in
// it — "ask the lead" and "the lead will ask you" are different claims).
func enclosingSentence(s string, idx int) [2]int {
	start := 0
	for _, loc := range sentenceEnd.FindAllStringIndex(s[:idx], -1) {
		start = loc[1]
	}
	end := len(s)
	if loc := sentenceEnd.FindStringIndex(s[idx:]); loc != nil {
		end = idx + loc[0]
	}
	return [2]int{start, end}
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
