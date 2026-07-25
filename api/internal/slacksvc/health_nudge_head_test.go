package slacksvc

// PRD #108 M4 — 'looping' now carries two causes, and the nudge head must tell
// them apart.

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
	const before = "⚠ This run looks like it's repeating the same step."
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
	// The reason is consulted for exactly one enum. If it leaked into the others,
	// every nudge would become reason-coupled and slacksvc's enum-keyed contract
	// would be gone rather than narrowed.
	for _, h := range []string{healthStalled, healthSlow, healthWaitingWorker, healthApprovalIdle, "ok"} {
		if a, b := healthNudgeHead(h, ""), healthNudgeHead(h, reasonPersistFailing); a != b {
			t.Errorf("%s: head changed with the reason (%q vs %q)", h, a, b)
		}
	}
}
