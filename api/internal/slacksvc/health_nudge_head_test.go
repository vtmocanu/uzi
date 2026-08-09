package slacksvc

// Two health enums carry more than one cause, and the nudge head must tell each
// pair apart: 'looping' since PRD #108 M4 (tool repetition vs a persistence wedge),
// and 'waiting_worker' since issue #182 (an unclaimed run vs a run whose worker has
// not acted on the response its owner already gave). Each arm consults exactly one
// mirrored reason constant; every other reason falls through to the enum's original
// sentence, which is also what a mirror drift degrades to.

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestReasonPersistFailingMirrorsWorkersvc is the slacksvc half of the mirror pin.
// The workersvc half (TestReasonPersistFailingIsMirroredBySlack) catches a reword
// on that side; this one catches a reword HERE, which is the silent direction — the
// nudge would quietly fall back to the tool-repetition sentence with nothing
// failing anywhere. Both halves carry the literal, so either edit alone reddens.
func TestReasonPersistFailingMirrorsWorkersvc(t *testing.T) {
	const inWorkersvc = "the agent's updates can't be saved, so it keeps resending them"
	if reasonPersistFailing != inWorkersvc {
		t.Fatalf("slacksvc's mirrored reason = %q, but internal/workersvc/health.go's reasonPersistFailing is %q.\nThey are compared for equality at runtime, so a mismatch does not error — it silently restores the tool-repetition wording, which is false for a persistence wedge.",
			reasonPersistFailing, inWorkersvc)
	}
}

func TestHealthNudgeHeadPersistenceCauseIsItsOwnSentence(t *testing.T) {
	// The existing sentence says the agent is repeating the same STEP. For a
	// persistence wedge that is simply false — the agent is working fine and the
	// SERVER cannot store its updates — so the owner reading it is pointed at the
	// wrong thing.
	got := healthNudgeHead(healthLooping, reasonPersistFailing)
	if strings.Contains(got, "repeating the same step") {
		t.Fatalf("nudge head = %q; a persistence wedge is not a repeated step, and telling the owner it is sends them looking at the agent instead of at the payload", got)
	}
	if !strings.Contains(got, "saved") {
		t.Fatalf("nudge head = %q, want it to name the cause (the updates are not being saved)", got)
	}
}

func TestHealthNudgeHeadLeavesTheToolRepetitionWordingExactlyAsItWas(t *testing.T) {
	// PRD #108 ADDED an arm; it did not reword an existing one. This pins the old
	// sentence verbatim so the additive change stays additive — no shipped nudge's
	// wording moves.
	const before = "⚠️ This run looks like it's repeating the same step."
	if got := healthNudgeHead(healthLooping, reasonLooping()); got != before {
		t.Fatalf("nudge head for the tool-repetition cause = %q, want the unchanged %q", got, before)
	}
	if got := healthNudgeHead(healthLooping, ""); got != before {
		t.Fatalf("nudge head with NO reason = %q, want the unchanged %q — an empty reason must fall back to the original wording, which is also what a mirror drift degrades to", got, before)
	}
}

// reasonLooping is workersvc's tool-repetition reason. slacksvc holds no workersvc
// import (gate.go, gatekeeper.go), so the test states the string it would receive
// rather than importing it — and any string OTHER than reasonPersistFailing must
// take the same branch, which is what the empty-reason case above also proves.
func reasonLooping() string { return "the agent keeps repeating the same action" }

func TestHealthNudgeTextThreadsTheReasonThrough(t *testing.T) {
	// The head is chosen by the reason and the body then appends it, so the two must
	// not contradict each other in the same message.
	body := healthNudgeText(healthLooping, reasonPersistFailing, "https://uzi.example", uuid.New())
	if strings.Contains(body, "repeating the same step") {
		t.Fatalf("nudge body = %q: the head still carries the tool-repetition framing", body)
	}
	if !strings.Contains(body, reasonPersistFailing) {
		t.Fatalf("nudge body = %q, want it to carry the detector's reason verbatim", body)
	}
}

func TestHealthNudgeHeadOtherEnumsAreUnaffectedByTheReason(t *testing.T) {
	// The reason is consulted for exactly TWO enums, and each consults only its OWN
	// mirrored constant. The property that must hold everywhere is the general one: a
	// reason belonging to a DIFFERENT enum must never move a head. If a reason leaked
	// across arms, every nudge would become reason-coupled and slacksvc's enum-keyed
	// contract would be gone rather than narrowed.
	//
	// (This test read "exactly one enum" and looped reasonPersistFailing over a set
	// that included healthWaitingWorker. Issue #182 made that sentence false while
	// leaving the assertion green — reasonPersistFailing is not waiting_worker's
	// constant, so it still takes the default arm. A test that passes for a reason its
	// comment denies is the shape worth catching; the assertion is now the general
	// property rather than an instance of it.)
	foreign := map[string]string{
		healthStalled:       reasonPersistFailing,
		healthSlow:          reasonPersistFailing,
		healthWaitingWorker: reasonPersistFailing,     // looping's, not its own
		healthApprovalIdle:  reasonVerdictUndelivered, // waiting_worker's, not its own
		healthLooping:       reasonVerdictUndelivered, // waiting_worker's, not its own
		"ok":                reasonVerdictUndelivered,
	}
	for h, r := range foreign {
		if a, b := healthNudgeHead(h, ""), healthNudgeHead(h, r); a != b {
			t.Errorf("%s: head changed with another enum's reason %q (%q vs %q)", h, r, a, b)
		}
	}
}

// TestReasonVerdictUndeliveredMirrorsWorkersvc is the slacksvc half of issue #182's
// mirror pin, exactly as TestReasonPersistFailingMirrorsWorkersvc is for PRD #108's.
// It catches a reword HERE, which is the silent direction: the nudge would quietly
// fall back to the unclaimed-run sentence with nothing failing anywhere.
func TestReasonVerdictUndeliveredMirrorsWorkersvc(t *testing.T) {
	const inWorkersvc = "the worker hasn't picked up your response yet"
	if reasonVerdictUndelivered != inWorkersvc {
		t.Fatalf("slacksvc's mirrored reason = %q, but internal/workersvc/health.go's reasonVerdictUndelivered is %q.\nThey are compared for equality at runtime, so a mismatch does not error — it silently restores the \"still waiting for a worker to pick it up\" wording, which tells the owner their run is UNCLAIMED while its worker is holding it.",
			reasonVerdictUndelivered, inWorkersvc)
	}
}

// The head this replaces asserts the run is unclaimed. It is not: a run only reaches
// awaiting_approval because a live worker put it there, and the owner has already
// used the approve/revise buttons. Telling them to wait for a worker to pick the run
// up contradicts both the run's state and their own action.
func TestHealthNudgeHeadUndeliveredVerdictIsItsOwnSentence(t *testing.T) {
	got := healthNudgeHead(healthWaitingWorker, reasonVerdictUndelivered)
	if strings.Contains(got, "waiting for a worker to pick it up") {
		t.Fatalf("nudge head = %q; the run's worker already holds it, so this asks the owner to wait for a claim that has already happened", got)
	}
	if !strings.Contains(got, "already responded") {
		t.Fatalf("nudge head = %q, want it to name what the owner needs to know first: they are not the blocker", got)
	}
}

func TestHealthNudgeHeadLeavesTheUnclaimedWordingExactlyAsItWas(t *testing.T) {
	// Issue #182 ADDED an arm; it did not reword an existing one. The queued arm's
	// nudge is unchanged.
	const before = "⏳ This run is still waiting for a worker to pick it up."
	if got := healthNudgeHead(healthWaitingWorker, reasonWaitingWorker()); got != before {
		t.Fatalf("nudge head for the unclaimed cause = %q, want the unchanged %q", got, before)
	}
	if got := healthNudgeHead(healthWaitingWorker, reasonNoWorker()); got != before {
		t.Fatalf("nudge head for the no-worker-online cause = %q, want the unchanged %q", got, before)
	}
	if got := healthNudgeHead(healthWaitingWorker, ""); got != before {
		t.Fatalf("nudge head with NO reason = %q, want the unchanged %q — an empty reason must fall back to the original wording, which is also what a mirror drift degrades to", got, before)
	}
}

// The queued arm's two reasons, stated rather than imported for the same reason
// reasonLooping is (slacksvc holds no workersvc import). Both must take the default
// arm, which is what proves the new branch is keyed on its own constant and not on
// "any reason at all".
func reasonWaitingWorker() string { return "waiting for a worker to pick up this run" }
func reasonNoWorker() string      { return "no worker is online to pick up this run" }

// The head is chosen by the reason and the body then appends it verbatim, so the two
// must not contradict each other inside one Slack message. That contradiction is
// precisely what shipped before this fix:
//
//	⏳ This run is still waiting for a worker to pick it up. the worker hasn't picked up your response yet.
func TestHealthNudgeTextUndeliveredVerdictDoesNotContradictItself(t *testing.T) {
	body := healthNudgeText(healthWaitingWorker, reasonVerdictUndelivered, "https://uzi.example", uuid.New())
	if strings.Contains(body, "waiting for a worker to pick it up") {
		t.Fatalf("nudge body = %q: the head still carries the unclaimed-run framing while the reason says a worker is holding it", body)
	}
	if !strings.Contains(body, reasonVerdictUndelivered) {
		t.Fatalf("nudge body = %q, want it to carry the detector's reason verbatim", body)
	}
}
