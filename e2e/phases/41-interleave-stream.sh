# shellcheck shell=bash
# phase:    interleave-stream
# title:    PRD #43 M5: interleaved multi-agent stream persists + replays (gapless seq, per-agent attribution)
# critical: no
# lane:     gitlab
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# =============================================================================
# PRD #43 M5 — interleaved multi-agent stream guard. A real parallel-subagent run
# weaves messages from several agents together; the SDK is the only thing that
# emits them, so the isolated stack drives the stub's scripted interleave
# (UZI_STUB_INTERLEAVE, executor.ts: lead/coder/reviewer alternating, each name
# recurring NON-ADJACENTLY — a second coder, reviewer, then lead). We then prove
# the persistence + replay contract the live SDK path relies on: (1) the whole run
# streams a gapless, strictly-ordered per-run seq; (2) per-agent attribution
# survives the round-trip; (3) a reconnect's REST `?after=<seq>` replay returns the
# SAME interleaved order.
say "PRD #43 M5: interleaved multi-agent stream persists + replays (gapless seq, per-agent attribution)"
if [ "$EXECUTOR" != stub ]; then
  say "PRD #43 M5 interleave scenario: SKIPPED (stub-only — UZI_STUB_INTERLEAVE is a stub sentinel; executor=$EXECUTOR)"
else
login   # fresh admin session re-unlocks the vault for the run claim

IID_IL="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E interleave stream","description":"implements prds/43-intra-run-parallel-subagents.md UZI_STUB_INTERLEAVE"}' \
  | jq -r '.card.iid')"
# create_run (not apipost) hardens against the transient board-reconcile 404 here: this
# create-issue → immediately-create-run sits under the fast (2s) poller the MR-close phase
# leaves on, which occasionally drops the just-created issue from the board for one tick
# (PRD #51 M6 — see the create_run comment). It still fails hard on any non-transient error.
RUN_IL="$(create_run "$REPO_ID" "$IID_IL")" || fail "interleave run-create failed (non-transient; see stderr)"
{ [ -n "$RUN_IL" ] && [ "$RUN_IL" != null ]; } || fail "interleave run was not created"
wait_status "$RUN_IL" awaiting_approval
apipost "/api/runs/$RUN_IL/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$RUN_IL" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
pass "interleave run $RUN_IL completed"

# The expected scripted stream (must mirror STUB_INTERLEAVE_STREAM in executor.ts):
# an ordered [agent, step] vector, with coder/reviewer/lead each recurring non-adjacently.
EXPECT_IL='[["lead",1],["coder",2],["reviewer",3],["coder",4],["reviewer",5],["lead",6]]'

MSGS_IL="$(apiget "/api/runs/$RUN_IL/messages")"

# (1) The WHOLE run (worker infra frames + the interleaved agent frames) carries a
#     gapless, strictly-ordered 1..N per-run seq — no drops, no dupes.
echo "$MSGS_IL" | jq -e '.messages | (length > 0) and ([.[].seq] == [range(1; length+1)])' >/dev/null \
  || fail "interleave run_messages seq is not a gapless 1..N sequence"
pass "run_messages seq gapless 1..$(echo "$MSGS_IL" | jq '.messages | length') across the interleaved stream"

# (2) Per-agent attribution survives persistence: the scripted frames (those carrying
#     a `step`), in seq order, are exactly the expected [agent, step] vector — proving
#     lead/coder/reviewer stayed correctly attributed through the interleave (and that
#     the non-adjacent same-name repeats did not collapse or swap).
SCRIPTED_IL="$(echo "$MSGS_IL" | jq -c '[.messages | sort_by(.seq)[] | select(.payload.step != null) | [.agent, .payload.step]]')"
[ "$SCRIPTED_IL" = "$EXPECT_IL" ] \
  || fail "interleaved agent stream mis-attributed or mis-ordered after persistence (got $SCRIPTED_IL, want $EXPECT_IL)"
# The scripted frames' seqs are strictly increasing + distinct (implied by the gapless
# global seq, asserted here directly against the interleaved subset).
echo "$MSGS_IL" | jq -e '
  [.messages[] | select(.payload.step != null) | .seq] as $q
  | ($q == ($q | sort)) and (($q | unique) == $q) and (($q | length) == 6)' >/dev/null \
  || fail "interleaved frames do not carry strictly-increasing distinct seqs"
pass "per-agent attribution intact after round-trip: $SCRIPTED_IL"

# --- PRD #99: per-INSTANCE attribution on the same interleaved stream ----------
# The lanes are drawn in the browser, which this harness does not run; what it CAN
# prove is the wire contract they are built from, and that contract is the whole
# feature. NO new frames were needed: PRD #99 M2 added the two columns to the
# EXISTING six, so EXPECT_IL above is untouched and stays frozen.
#
# An exact vector, mirroring EXPECT_IL's style, so a reorder/relabel/drop all fail
# with the actual value printed.
ATTR_IL="$(echo "$MSGS_IL" | jq -c '[.messages | sort_by(.seq)[] | select(.payload.step != null) | [.agent, .agent_instance, .agent_label]]')"
EXPECT_ATTR_IL='[["lead",null,null],["coder","stub-inst-a","API wiring"],["reviewer","stub-inst-rev-a","audit unit A"],["coder","stub-inst-b","web gate UX"],["reviewer","stub-inst-rev-b","audit unit B"],["lead",null,null]]'
[ "$ATTR_IL" = "$EXPECT_ATTR_IL" ] \
  || fail "PRD #99 attribution altered through persistence (got $ATTR_IL, want $EXPECT_ATTR_IL)"
pass "PRD #99: both attribution columns round-trip verbatim on every scripted frame"

# The load-bearing PROPERTY, stated independently of the literal above so it survives
# a future fixture edit: the two same-role `coder` frames carry DIFFERENT non-null
# instance ids. Equal ids — or null ones — are Problem 2 verbatim (the two parallel
# coders merge into one garbled lane), and every other assertion in this phase still
# passes in that state, which is exactly why this one is written separately.
echo "$MSGS_IL" | jq -e '
  [.messages[] | select(.payload.step != null and .agent == "coder")] as $c
  | ($c | length) == 2
    and ($c[0].agent_instance != null) and ($c[1].agent_instance != null)
    and ($c[0].agent_instance != $c[1].agent_instance)
    and ($c[0].agent_label != $c[1].agent_label)' >/dev/null \
  || fail "the two parallel coder frames lost their DISTINCT instance ids/labels (Problem 2: they would merge into one lane)"
pass "PRD #99: two parallel coder invocations stay distinguishable -> two labelled lanes, no merged coder block"

# The lead is the parentless actor: both columns null on the interleaved stream too,
# so it falls back to a role-keyed lane beside the two instance-keyed ones.
echo "$MSGS_IL" | jq -e '
  [.messages[] | select(.payload.step != null and .agent == "lead")]
  | (length == 2) and all(.agent_instance == null and .agent_label == null)' >/dev/null \
  || fail "the scripted lead frames should carry NULL for both attribution columns"
pass "PRD #99: lead frames carry NULL instance + label (the role-fallback lane)"

# (3) Reconnect replay: REST `?after=<seq>` from a pivot INSIDE the interleave returns
#     exactly the tail (same seqs, same order) — the same interleaved order a WS
#     reconnect would replay. Pivot = the seq of scripted step 2 (the first `coder`),
#     so the tail still contains the non-adjacent coder@4 + reviewer@5 recurrences.
PIVOT_IL="$(echo "$MSGS_IL" | jq '[.messages[] | select(.payload.step == 2)][0].seq')"
{ [ -n "$PIVOT_IL" ] && [ "$PIVOT_IL" != null ]; } || fail "could not resolve the interleave replay pivot seq"
REPLAY_IL="$(apiget "/api/runs/$RUN_IL/messages?after=$PIVOT_IL")"
echo "$REPLAY_IL" | jq -e --argjson p "$PIVOT_IL" '.messages | (length > 0) and all(.seq > $p)' >/dev/null \
  || fail "replay ?after=$PIVOT_IL returned a message with seq <= pivot"
# The replay is byte-identical to the tail of the full stream (seq/agent/kind/payload,
# in order). PRD #99 added agent_instance/agent_label to the projection: without them a
# replay that dropped the lane identity matched the tail anyway, so a reconnect could
# silently re-place every subagent message into the NULL-instance role lane and this
# assertion would still pass.
FULL_TAIL_IL="$(echo "$MSGS_IL" | jq -c --argjson p "$PIVOT_IL" '[.messages | sort_by(.seq)[] | select(.seq > $p) | {seq, agent, agent_instance, agent_label, kind, payload}]')"
REPLAY_LIST_IL="$(echo "$REPLAY_IL" | jq -c '[.messages | sort_by(.seq)[] | {seq, agent, agent_instance, agent_label, kind, payload}]')"
[ "$FULL_TAIL_IL" = "$REPLAY_LIST_IL" ] || fail "replay ?after=$PIVOT_IL did not match the tail of the full stream"
# And specifically: the interleaved order of the scripted frames after the pivot is preserved.
echo "$REPLAY_IL" | jq -e '
  [.messages | sort_by(.seq)[] | select(.payload.step != null) | [.agent, .payload.step]]
    == [["reviewer",3],["coder",4],["reviewer",5],["lead",6]]' >/dev/null \
  || fail "replay lost the interleaved order of the scripted frames after the pivot"
pass "reconnect replay (?after=$PIVOT_IL) returned the same interleaved order (coder@4 + reviewer@5 recurrences intact)"
fi

