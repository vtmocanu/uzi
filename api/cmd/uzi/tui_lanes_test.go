package main

import (
	"strings"
	"testing"
	"time"
)

// PRD #112 D5: the lane classifiers, ported from the web. These are table tests over
// pure functions, which is the whole reason the logic lives outside the update loop.

func lf(seq int32, agent, instance, label string, age time.Duration, now time.Time) laneFrame {
	return laneFrame{Seq: seq, Kind: "text", Agent: agent, AgentInstance: instance,
		AgentLabel: label, CreatedAt: now.Add(-age)}
}

func TestLaneKeyOf(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name            string
		agent, instance string
		want            string
	}{
		{"instance wins", "coder", "toolu_01", "toolu_01"},
		{"falls back to the role", "coder", "", "coder"},
		{"falls back to lead", "", "", laneLead},
		// The `||` vs `??` distinction, which is the whole reason the web comment
		// exists: an EMPTY instance must fall THROUGH to the role, never become a key.
		// `??` semantics would key this lane on "" and split it off on its own.
		{"empty instance falls through, never becomes a key", "coder", "", "coder"},
		{"empty instance and empty role reach lead", "", "", laneLead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := laneKeyOf(lf(1, tc.agent, tc.instance, "", 0, now))
			if got != tc.want {
				t.Errorf("laneKeyOf(agent=%q instance=%q) = %q, want %q", tc.agent, tc.instance, got, tc.want)
			}
		})
	}
}

// laneRole's resumed-subagent case (PRD #99 Decision 8) and its inverse.
func TestLaneRoleTakesFirstNonLeadRole(t *testing.T) {
	now := time.Now()

	// THE CASE D5 MANDATES. An SDK replay frame opens the lane looking like the lead
	// (subagent_type absent → the worker stores the literal "lead"), then the real role
	// appears. Titling from the FIRST frame would label a coder's lane "lead".
	resumed := []laneFrame{
		lf(1, laneLead, "toolu_01", "", time.Minute, now),
		lf(2, "coder", "toolu_01", "", 30*time.Second, now),
	}
	if got := laneRole(resumed); got != "coder" {
		t.Errorf("laneRole(resumed subagent) = %q, want \"coder\" — the lane must be titled by the first NON-lead role, or a resumed subagent is labelled lead while holding a coder's tool rail", got)
	}

	// Testing "agent != nil" instead would NOT catch the above, because the field is
	// already collapsed to "lead" upstream and is never null. This fixture proves the
	// distinction is real: every frame has a non-empty agent, so a nil-check passes
	// them all and would return "lead".
	for _, f := range resumed {
		if f.Agent == "" {
			t.Fatal("fixture is wrong: the resumed-subagent case requires every frame to NAME an agent, since the upstream collapse means it is never absent")
		}
	}

	// The INVERSE: an all-lead lane legitimately stays "lead". A repo can ship
	// .claude/agents/lead.md, which registers as an ordinary invocable subagent, so
	// this cannot be "first non-lead role, or bust".
	allLead := []laneFrame{
		lf(1, laneLead, "toolu_02", "", time.Minute, now),
		lf(2, laneLead, "toolu_02", "", time.Second, now),
	}
	if got := laneRole(allLead); got != laneLead {
		t.Errorf("laneRole(all-lead lane) = %q, want %q — an all-lead lane can be a REAL invocation of a repo's own lead.md", got, laneLead)
	}

	// The fallback's VALUE is pinned even though the fallback EXPRESSION is provably
	// equivalent to a bare `return laneLead` (see the note in laneRole). Returning
	// "unknown" or "" instead would break every roster row.
	if got := laneRole(nil); got != laneLead {
		t.Errorf("laneRole(no frames) = %q, want %q", got, laneLead)
	}
}

func TestLaneLabelIsIndependentOfRole(t *testing.T) {
	now := time.Now()
	// The first frame carrying a label wins, even when it is not the first frame and
	// not the frame that named the role.
	frames := []laneFrame{
		lf(1, laneLead, "toolu_01", "", time.Minute, now),
		lf(2, "coder", "toolu_01", "write the tests", 30*time.Second, now),
		lf(3, "coder", "toolu_01", "a later label", time.Second, now),
	}
	if got := laneLabel(frames); got != "write the tests" {
		t.Errorf("laneLabel = %q, want the FIRST label present", got)
	}
	// A lane with no label renders the role alone — no placeholder text.
	if got := laneLabel([]laneFrame{lf(1, "coder", "toolu_02", "", 0, now)}); got != "" {
		t.Errorf("laneLabel(no labels) = %q, want \"\" (the role renders alone, with no placeholder)", got)
	}
}

// The label clamp is on RUNES at the server's cap, not UTF-16 units at the web's.
func TestLaneLabelClampIsRuneSafe(t *testing.T) {
	// Astral-plane runes: 4 bytes each, 2 UTF-16 code units each. A UTF-16 clamp at an
	// odd boundary splits a surrogate pair; a rune clamp cannot.
	label := strings.Repeat("𝄞", laneLabelCap+20)
	got := capCell(cellText(label), laneLabelCap)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("a label past the cap was not truncated: %q", got)
	}
	for i, r := range got {
		if r == 0xFFFD {
			t.Fatalf("clamped label contains U+FFFD at byte %d — the clamp split a codepoint, which is the UTF-16 defect the web's own comment concedes and the Go port must not replicate", i)
		}
	}
	if n := len([]rune(got)); n > laneLabelCap {
		t.Errorf("clamped label is %d runes, want <= %d", n, laneLabelCap)
	}
}

// The D5 ladder, all six rungs plus the three traps.
//
// 🔴 NOT EXHAUSTIVE OVER STATUSES, and it is not trying to be. `limit_wait` (PRD #35)
// rides the same gate rung as `awaiting_approval` and is covered by
// TestCrewStateForParkedRun below, deliberately kept separate.
//
// The reason is structural rather than tidiness: each row here isolates ONE rung's
// precedence, so a single `limit_wait` row would pin the VERDICT without pinning WHICH
// RUNG produced it — and the whole point of the park's placement is that it fires above
// two rungs that answer differently (recency says `idle`, a frozen health flag says
// `stalled`). Proving that needs the stale-vs-fresh and flagged-vs-ok PAIRS, which one
// table row structurally cannot hold. Folding it in would produce a row that passes for
// a reason nobody could later separate from the one intended.
func TestCrewStateForLadder(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	const me, other = "toolu_me", "toolu_other"
	fresh := now.Add(-5 * time.Second)
	stale := now.Add(-10 * time.Minute)

	for _, tc := range []struct {
		name           string
		status, health string
		actor, active  string
		last           time.Time
		want           crewState
	}{
		// Rung 1: terminal beats everything, including an active actor.
		{"completed is done", "completed", "ok", me, me, fresh, crewDone},
		{"failed is done", "failed", "stalled", me, me, fresh, crewDone},
		{"cancelled is done", "cancelled", "ok", me, me, fresh, crewDone},
		// Rung 2: a gate or a missing worker blocks the WHOLE crew — even the lane
		// that is otherwise the active speaker.
		{"gate dominates the active lane", "awaiting_approval", "ok", me, me, fresh, crewWaiting},
		{"gate dominates a quiet lane", "awaiting_approval", "ok", other, me, stale, crewWaiting},
		{"waiting_worker dominates", "running", "waiting_worker", me, me, fresh, crewWaiting},
		// Rungs 3 and 4: the active speaker reads health, never recency.
		{"active + stalled health", "running", "stalled", me, me, fresh, crewStalled},
		{"active + slow health", "running", "slow", me, me, fresh, crewStalled},
		{"active + looping health", "running", "looping", me, me, fresh, crewStalled},
		{"active + ok health", "running", "ok", me, me, fresh, crewWorking},
		// Rungs 5 and 6: the non-active recency split.
		{"quiet but recent", "running", "ok", other, me, fresh, crewWaiting},
		{"quiet and old", "running", "ok", other, me, stale, crewIdle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := crewStateFor(tc.status, tc.health, tc.actor, tc.active, tc.last, now)
			if got != tc.want {
				t.Errorf("crewStateFor(status=%s health=%s actor=%s active=%s) = %s, want %s",
					tc.status, tc.health, tc.actor, tc.active, got, tc.want)
			}
		})
	}

	// TRAP 1, stated as its own test because it is the one a reimplementation gets
	// wrong: a HEALTHY tool call lasting longer than the 45s recency window must still
	// read `working`. The server's stall flag defaults to 300s, so 45s-300s is the
	// normal healthy range for a long tool call. Applying laneStaleAfter to the active
	// lane flips all of them to idle.
	longCall := now.Add(-4 * time.Minute) // past 45s, well inside the server's 300s
	if got := crewStateFor("running", "ok", me, me, longCall, now); got != crewWorking {
		t.Errorf("a healthy %v tool call by the ACTIVE speaker = %s, want working — the active lane trusts run.health and must never be gated by the %v recency window",
			now.Sub(longCall), got, laneStaleAfter)
	}
}

// PRD #35 Success Criterion 2, on the CLI's side of it: "a parked run is visibly
// waiting, never stalled".
//
// Kept OUT of the ladder table above deliberately. Two of these three cases pass
// against the un-fixed ladder for reasons that have nothing to do with the park, so
// folding them into a 12-row table would have hidden which rows actually carry the
// property. Measured against `crewStateFor` with the statusLimitWait clause removed:
//
//	stale + no active speaker  → crewIdle     (would have read "nobody is coming")
//	stalled health + active    → crewStalled  (the criterion failing outright)
//	fresh + no active speaker  → crewWaiting  (right answer, wrong reason — recency)
//
// The third is here for completeness and pins nothing; the first two are the test.
func TestCrewStateForParkedRun(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	const me = "toolu_me"

	// A park runs for HOURS, so every lane is far past the 45s recency window. Without
	// the statusLimitWait rung this is the case the rail actually shows, and `idle`
	// says "nothing is happening and nobody is coming" about a run with a server-side
	// promotion already scheduled.
	if got := crewStateFor(statusLimitWait, "ok", me, "", now.Add(-4*time.Hour), now); got != crewWaiting {
		t.Errorf("crewStateFor(limit_wait, quiet for 4h) = %s, want %s — a parked run is waiting on a scheduled promotion, not idle", got, crewWaiting)
	}

	// 🔴 THE ONE THAT MATTERS, and the reason this test does not just trust the api
	// lane. ListActiveRunsForHealth is a POSITIVE allowlist over
	// (queued, running, awaiting_approval), so the PRD #47 detector never revisits a
	// parked run: whatever health flag was live at park time is FROZEN for the whole
	// park, which can be days. The server clears it on the park write, but if that ever
	// regresses the symptom appears HERE, and "visibly waiting, never stalled" would be
	// failing through the health column while the status column looked fine.
	for _, health := range []string{"stalled", "slow", "looping"} {
		if got := crewStateFor(statusLimitWait, health, me, me, now.Add(-4*time.Hour), now); got != crewWaiting {
			t.Errorf("crewStateFor(limit_wait, health=%q, active speaker) = %s, want %s — a stale health flag frozen at park time must not make a parked run read stalled",
				health, got, crewWaiting)
		}
	}

	// Same answer from the other side of the recency split, so the rung is proven to be
	// the source of the verdict rather than the timer happening to agree.
	if got := crewStateFor(statusLimitWait, "ok", me, "", now.Add(-5*time.Second), now); got != crewWaiting {
		t.Errorf("crewStateFor(limit_wait, recent) = %s, want %s", got, crewWaiting)
	}

	// The park is NOT terminal, which is the inverse property and the one that would
	// silently break `uzi run logs --follow` and the stream reconcile if it were.
	if isTerminalRunStatus(statusLimitWait) {
		t.Error("isTerminalRunStatus(limit_wait) = true — a parked run resumes, so treating it as terminal draws every lane `done` on a run that is about to start talking again")
	}
	if terminalRunStatuses[statusLimitWait] {
		t.Error("run.go's terminalRunStatuses contains limit_wait — `uzi run logs --follow` would exit mid-run and truncate the capture")
	}

	// No active speaker on a parked run: nothing is executing, so naming one lane the
	// active one would be a lie (and would earn it a `working` dot on the rung below).
	frames := []laneFrame{lf(1, "coder", "toolu_01", "", time.Minute, now)}
	if got := activeLaneKey(statusLimitWait, frames); got != "" {
		t.Errorf("activeLaneKey(limit_wait) = %q, want \"\" — a parked run has no executing agent", got)
	}
}

// TRAP 2: the active lane is computed over `live`, which INCLUDES claimed.
func TestActiveLaneKeyIncludesClaimed(t *testing.T) {
	now := time.Now()
	frames := []laneFrame{
		lf(1, "coder", "toolu_01", "", time.Minute, now),
		lf(2, "tester", "toolu_02", "", time.Second, now),
	}
	for _, status := range []string{"running", "claimed"} {
		if got := activeLaneKey(status, frames); got != "toolu_02" {
			t.Errorf("activeLaneKey(%q) = %q, want the newest lane %q — `live` includes claimed, and a running-only test makes every lane of a claimed run read idle",
				status, got, "toolu_02")
		}
	}
	for _, status := range []string{"queued", "awaiting_approval", "completed", "failed", "cancelled"} {
		if got := activeLaneKey(status, frames); got != "" {
			t.Errorf("activeLaneKey(%q) = %q, want \"\" (no active speaker when the run is not live)", status, got)
		}
	}
	if got := activeLaneKey("running", nil); got != "" {
		t.Errorf("activeLaneKey with no frames = %q, want \"\"", got)
	}
}

// TRAP 3: the rollup is worst-state-wins, not newest.
func TestWorstLaneStateIsWorstNotNewest(t *testing.T) {
	// A stalled lane must surface even when it is the OLDEST input and every other
	// lane is healthy — "newest wins" would return working here.
	got := worstLaneState([]crewState{crewStalled, crewWorking, crewWorking, crewDone})
	if got != crewStalled {
		t.Errorf("worstLaneState = %s, want stalled — one stalled lane must surface over working ones (worst wins, not newest)", got)
	}
	// The full priority ladder, each rung beating the one below it.
	for _, tc := range []struct {
		in   []crewState
		want crewState
	}{
		{[]crewState{crewWaiting, crewWorking, crewIdle, crewDone}, crewWaiting},
		{[]crewState{crewWorking, crewIdle, crewDone}, crewWorking},
		{[]crewState{crewIdle, crewDone}, crewIdle},
		{[]crewState{crewDone}, crewDone},
		{nil, crewIdle},
	} {
		if got := worstLaneState(tc.in); got != tc.want {
			t.Errorf("worstLaneState(%v) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestBuildLanesGroupsByInvocationInFirstSeenOrder(t *testing.T) {
	now := time.Now()
	// One definition. The auditor found this as the SIXTH instance of the
	// literal-in-diagnostic class, and my own sweep missed it because it collected
	// literals used as VALUES while this one is a COMPARISON operand — the sweep's
	// scope was narrower than the class it was written to catch, which is the same
	// defect one level up.
	const wantLabel = "write it"
	const inst1, inst2 = "toolu_01", "toolu_02"
	frames := []laneFrame{
		lf(1, laneLead, "", "", 3*time.Minute, now),
		lf(2, "coder", inst1, wantLabel, 2*time.Minute, now),
		lf(3, "coder", inst2, "write it too", 90*time.Second, now),
		lf(4, "coder", inst1, "", time.Minute, now),
		lf(5, laneLead, "", "", 30*time.Second, now),
	}
	lanes := buildLanes(frames)
	if len(lanes) != 3 {
		t.Fatalf("got %d lanes, want 3 (lead + two coder invocations)", len(lanes))
	}
	// Two parallel invocations of the SAME role stay distinct — the whole point of
	// keying on the invocation rather than the role.
	if lanes[1].Key != inst1 || lanes[2].Key != inst2 {
		t.Errorf("lane keys = %q/%q, want %q/%q — two parallel invocations of one role must not collapse into one lane",
			lanes[1].Key, lanes[2].Key, inst1, inst2)
	}
	if lanes[0].Key != laneLead {
		t.Errorf("first lane key = %q, want %q (instance-less frames coalesce per role)", lanes[0].Key, laneLead)
	}
	// Order is first-seen, so the rail does not reshuffle as frames arrive.
	if lanes[0].Frames[0].Seq != 1 || lanes[1].Frames[0].Seq != 2 || lanes[2].Frames[0].Seq != 3 {
		t.Error("lanes are not in first-seen order; the rail would reshuffle on every update")
	}
	// LastActivity is the NEWEST frame in the lane, which is what the recency split reads.
	if !lanes[0].LastActivity.Equal(frames[4].CreatedAt) {
		t.Errorf("lead lane LastActivity = %v, want the newest frame's %v", lanes[0].LastActivity, frames[4].CreatedAt)
	}
	if lanes[1].Label != wantLabel {
		t.Errorf("lane label = %q, want %q", lanes[1].Label, wantLabel)
	}
}
