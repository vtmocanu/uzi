package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// PRD #84 M4 4c: the AUTHORITATIVE, server-side capability approval gate (capabilityGate),
// enforced in SubmitInput for every approve_plan — both the selection-bearing dispatch and
// the nil-selection plain-enqueue path, so it has no bypass.
// A run at awaiting_approval owned by exactly one worker whose EFFECTIVE caps do not satisfy
// the run's required_capabilities is BLOCKED with ErrCapabilityUnmet (→ 409), UNLESS the
// capability-aware kill-switch is OFF or the owning worker satisfies. These are service-level
// arm tests over the fakeStore harness; the effective-caps fold itself is pinned identically
// to fn_worker_can_claim by the store live-DB tests.
//
// gatedRun (agent_selection_test.go) parks a run at awaiting_approval with a live owning
// worker; here we set the run's required set and the worker's caps to exercise the gate. A
// repo selection with no exclusions is always valid against gatedRun's two-agent roster, so
// validateSelection passes and the capability block is what the test observes.

func approveSel() *AgentSelection { return &AgentSelection{Source: AgentSourceRepo} }

// Flag ON, base worker (no docker), run requires docker → BLOCKED with the unmet set, and
// no approve_plan is enqueued.
func TestSubmitApproveBlocksOnUnmetCapability(t *testing.T) {
	fs, svc, user, runID := gatedRun(t)
	svc.capabilitySettings = fakeCapabilitySettings{on: true}
	fs.runByID.RequiredCapabilities = []string{"docker"}
	// gatedRun's worker has no capabilities and docker_enabled unset → effective is empty.

	_, err := svc.SubmitInput(context.Background(), user, runID, "approve_plan", "", approveSel())
	if !errors.Is(err, ErrCapabilityUnmet) {
		t.Fatalf("want ErrCapabilityUnmet, got %v", err)
	}
	var unmet *CapabilityUnmetError
	if !errors.As(err, &unmet) {
		t.Fatalf("error must be a *CapabilityUnmetError carrying the unmet set, got %T", err)
	}
	if len(unmet.Unmet) != 1 || unmet.Unmet[0] != "docker" {
		t.Fatalf("unmet = %v, want [docker]", unmet.Unmet)
	}
	if fs.createdApproval != nil {
		t.Fatal("a blocked approve must NOT enqueue an approve_plan input")
	}
	if fs.createdInput != nil {
		t.Fatal("a blocked approve must not take the plain enqueue path either")
	}
}

// A nil-selection approve_plan (the plain-enqueue path, no AgentSelection) is gated too:
// the capability check is hoisted into SubmitInput and covers BOTH the selection-bearing
// dispatch and this nil path, so there is no bypass. Without the hoist this approve would
// skip submitApproval entirely and enqueue onto an incapable worker.
func TestSubmitApproveNilSelectionAlsoBlocks(t *testing.T) {
	fs, svc, user, runID := gatedRun(t)
	svc.capabilitySettings = fakeCapabilitySettings{on: true}
	fs.runByID.RequiredCapabilities = []string{"docker"}

	_, err := svc.SubmitInput(context.Background(), user, runID, "approve_plan", "", nil)
	if !errors.Is(err, ErrCapabilityUnmet) {
		t.Fatalf("nil-selection approve_plan must be blocked, got %v", err)
	}
	if fs.createdApproval != nil || fs.createdInput != nil {
		t.Fatal("a blocked nil-selection approve must enqueue nothing on either path")
	}
}

// Flag OFF → best-effort claim, no eligibility enforced, so the same unmet run approves.
func TestSubmitApproveFlagOffDoesNotBlock(t *testing.T) {
	fs, svc, user, runID := gatedRun(t)
	svc.capabilitySettings = fakeCapabilitySettings{on: false}
	fs.runByID.RequiredCapabilities = []string{"docker"}

	if _, err := svc.SubmitInput(context.Background(), user, runID, "approve_plan", "", approveSel()); err != nil {
		t.Fatalf("flag OFF must not block: %v", err)
	}
	if fs.createdApproval == nil {
		t.Fatal("approve_plan must be enqueued when the flag is off")
	}
}

// The owning worker HAS docker (docker_enabled) → the effective fold satisfies the docker
// requirement, so the approve proceeds even with the flag ON.
func TestSubmitApproveDockerWorkerSatisfies(t *testing.T) {
	fs, svc, user, runID := gatedRun(t)
	svc.capabilitySettings = fakeCapabilitySettings{on: true}
	fs.runByID.RequiredCapabilities = []string{"docker"}
	fs.workerByID.DockerEnabled = pgtype.Bool{Bool: true, Valid: true}

	if _, err := svc.SubmitInput(context.Background(), user, runID, "approve_plan", "", approveSel()); err != nil {
		t.Fatalf("a docker-enabled owning worker must satisfy a docker requirement: %v", err)
	}
	if fs.createdApproval == nil {
		t.Fatal("approve_plan must be enqueued when the owning worker satisfies")
	}
}

// A worker whose STORED capabilities (not docker_enabled) cover the requirement satisfies
// too — the fold unions worker.Capabilities.
func TestSubmitApproveStoredCapabilitySatisfies(t *testing.T) {
	fs, svc, user, runID := gatedRun(t)
	svc.capabilitySettings = fakeCapabilitySettings{on: true}
	fs.runByID.RequiredCapabilities = []string{"jvm"}
	fs.workerByID.Capabilities = []string{"jvm"}

	if _, err := svc.SubmitInput(context.Background(), user, runID, "approve_plan", "", approveSel()); err != nil {
		t.Fatalf("a worker whose stored caps cover the requirement must satisfy: %v", err)
	}
	if fs.createdApproval == nil {
		t.Fatal("approve_plan must be enqueued when the owning worker satisfies")
	}
}

// A run with no required_capabilities is never gated, even with the flag ON — the guard
// short-circuits before loading the worker.
func TestSubmitApproveNoRequirementsNeverGated(t *testing.T) {
	fs, svc, user, runID := gatedRun(t)
	svc.capabilitySettings = fakeCapabilitySettings{on: true}
	fs.runByID.RequiredCapabilities = nil

	if _, err := svc.SubmitInput(context.Background(), user, runID, "approve_plan", "", approveSel()); err != nil {
		t.Fatalf("a run with no requirements must approve: %v", err)
	}
	if fs.createdApproval == nil {
		t.Fatal("approve_plan must be enqueued for a run with no requirements")
	}
}

// The override PRIMITIVES compose: OverrideRunRequiredCapabilities clears the run's required
// set, so a subsequent approve on the same run is no longer blocked. (The handler no longer
// calls these two in sequence — it uses SubmitInputWithCapabilityOverride, which does the
// clear atomically; see TestOverrideApproveSucceedsAndClears. This still pins the primitives.)
func TestOverrideClearsThenApproveSucceeds(t *testing.T) {
	fs, svc, user, runID := gatedRun(t)
	svc.capabilitySettings = fakeCapabilitySettings{on: true}
	fs.runByID.RequiredCapabilities = []string{"docker"}
	fs.clearCapsRows = 1 // the owner+awaiting_approval row matches

	// Sanity: without the override this run blocks.
	if _, err := svc.SubmitInput(context.Background(), user, runID, "approve_plan", "", approveSel()); !errors.Is(err, ErrCapabilityUnmet) {
		t.Fatalf("precondition: unmet run must block, got %v", err)
	}

	// Re-arm the run's requirement (the sanity call did not clear it) and apply the override.
	fs.runByID.RequiredCapabilities = []string{"docker"}
	fs.createdApproval = nil
	if err := svc.OverrideRunRequiredCapabilities(context.Background(), user, runID); err != nil {
		t.Fatalf("OverrideRunRequiredCapabilities: %v", err)
	}
	if fs.clearedCaps == nil {
		t.Fatal("override must call ClearRunRequiredCapabilities")
	}
	if fs.clearedCaps.ID != runID || fs.clearedCaps.UserID != user {
		t.Fatalf("clear scoped to id=%v user=%v, want id=%v user=%v", fs.clearedCaps.ID, fs.clearedCaps.UserID, runID, user)
	}
	// The fake emptied runByID.RequiredCapabilities on clear, mirroring the real UPDATE, so
	// the approve now reloads an empty required set and proceeds.
	if _, err := svc.SubmitInput(context.Background(), user, runID, "approve_plan", "", approveSel()); err != nil {
		t.Fatalf("approve after override must succeed: %v", err)
	}
	if fs.createdApproval == nil {
		t.Fatal("approve_plan must be enqueued after the override cleared the requirement")
	}
}

// The happy override path through the real entry point: a VALID selection on an unmet run
// BYPASSES the gate, enqueues the approve, AND clears the requirement — atomically, only after
// the enqueue succeeds.
func TestOverrideApproveSucceedsAndClears(t *testing.T) {
	fs, svc, user, runID := gatedRun(t)
	svc.capabilitySettings = fakeCapabilitySettings{on: true}
	fs.runByID.RequiredCapabilities = []string{"docker"}
	fs.clearCapsRows = 1

	if _, err := svc.SubmitInputWithCapabilityOverride(context.Background(), user, runID, "approve_plan", "", approveSel()); err != nil {
		t.Fatalf("override approve on an unmet run must succeed (gate bypassed): %v", err)
	}
	if fs.createdApproval == nil {
		t.Fatal("a successful override approve must enqueue the approve_plan input")
	}
	if fs.clearedCaps == nil {
		t.Fatal("a successful override approve must clear required_capabilities")
	}
	if fs.clearedCaps.ID != runID || fs.clearedCaps.UserID != user {
		t.Fatalf("clear scoped to id=%v user=%v, want id=%v user=%v", fs.clearedCaps.ID, fs.clearedCaps.UserID, runID, user)
	}
}

// Review fix (PRD #84 M4 4c): the override is ATOMIC with a successful approve. A FAILED
// approve (an invalid agent selection) must leave the run's required_capabilities INTACT so a
// retry stays gated — the clear runs ONLY after the approve validates and enqueues. Before the
// fix the handler cleared BEFORE SubmitInput, so a failed approve permanently dropped the
// requirement and the retry was ungated.
func TestOverrideFailedApproveLeavesRequirementIntact(t *testing.T) {
	fs, svc, user, runID := gatedRun(t)
	svc.capabilitySettings = fakeCapabilitySettings{on: true}
	fs.runByID.RequiredCapabilities = []string{"docker"}
	fs.clearCapsRows = 1

	// An invalid selection: excluding an agent that is not in the repo roster {coder,reviewer},
	// which validateSelection rejects with ErrInvalidSelection AFTER the gate would be bypassed.
	badSel := &AgentSelection{Source: AgentSourceRepo, Exclusions: []string{"no-such-agent"}}
	_, err := svc.SubmitInputWithCapabilityOverride(context.Background(), user, runID, "approve_plan", "", badSel)
	if !errors.Is(err, ErrInvalidSelection) {
		t.Fatalf("an invalid selection must fail with ErrInvalidSelection, got %v", err)
	}
	if fs.clearedCaps != nil {
		t.Fatal("a FAILED override approve must NOT clear required_capabilities (the retry must stay gated)")
	}
	if fs.createdApproval != nil {
		t.Fatal("a failed approve must not enqueue an approve_plan input")
	}
	// The requirement is untouched, so a subsequent NON-override approve still blocks — the
	// direct proof that the failed override left the run gated.
	if _, err := svc.SubmitInput(context.Background(), user, runID, "approve_plan", "", approveSel()); !errors.Is(err, ErrCapabilityUnmet) {
		t.Fatalf("after a failed override the run must still be gated, got %v", err)
	}
}

// The CapabilityUnmetError message names the unmet capabilities so the handler can render a
// remediation hint; errors.Is still matches the sentinel.
func TestCapabilityUnmetErrorCarriesNames(t *testing.T) {
	err := &CapabilityUnmetError{Unmet: []string{"docker", "jvm"}}
	if !errors.Is(err, ErrCapabilityUnmet) {
		t.Fatal("CapabilityUnmetError must errors.Is ErrCapabilityUnmet")
	}
	msg := err.Error()
	for _, want := range []string{"docker", "jvm"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error message %q must name %q", msg, want)
		}
	}
}
