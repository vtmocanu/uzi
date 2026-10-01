// @vitest-environment jsdom
//
// PRD #95 M3 + issue #1800: the steer queue's delivery states (Decision 7) are derived
// client-side from the receipt timestamps (consumed_at, applied_at, included_at,
// inclusion_reported) and run.status, and the queue must SURVIVE the run going terminal
// (B1) — it lives in its own card lifted to useRunStream, not inside the !terminal-gated
// composer, so "Not delivered — run finished" and "Not confirmed — run finished" are
// reachable. The harness (Claude or Codex) never changes a chip: SteerInput has no harness.

import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { SteerQueueCard } from "./SteerQueueCard";
import type { Run, SteerInput } from "../lib/api";

afterEach(cleanup);

function input(over: Partial<SteerInput> = {}): SteerInput {
  return {
    id: 1,
    body: "resume the agent",
    created_at: "2026-07-20T10:00:00Z",
    consumed_at: null,
    applied_at: null,
    included_at: null,
    inclusion_reported: true,
    kind: "follow_up",
    disposition: null,
    ...over,
  };
}

// A scope directive (PRD #634): never consumed, its chip is driven by disposition.
function scope(over: Partial<SteerInput> = {}): SteerInput {
  return {
    id: 10,
    body: "scope ceiling → complete through milestone 2 of 4",
    created_at: "2026-07-20T10:00:00Z",
    consumed_at: null,
    applied_at: null,
    included_at: null,
    inclusion_reported: false,
    kind: "scope",
    disposition: null,
    ...over,
  };
}

const noop = () => {};

const RECEIVED = "2026-07-20T10:01:00Z";
const ROUTED = "2026-07-20T10:01:30Z";
const INCLUDED = "2026-07-20T10:02:00Z";

// Renders a single follow-up at the given run state.
function renderCard(row: SteerInput, status: string | undefined, terminal = false) {
  render(
    <SteerQueueCard inputs={[row]} terminal={terminal} status={status} busy={false} onStop={noop} onSend={noop} />,
  );
}

describe("SteerQueueCard delivery states (Decision 7, issue #1800)", () => {
  it("NULL consumed_at + live, not-at-gate → Queued", () => {
    renderCard(input(), "running");
    expect(screen.getByText("Queued")).toBeTruthy();
  });

  it("NULL consumed_at + awaiting_approval → still Queued (worker has not received it yet)", () => {
    renderCard(input(), "awaiting_approval");
    expect(screen.getByText("Queued")).toBeTruthy();
    expect(screen.queryByText(/waits for approval/)).toBeNull();
  });

  it("NULL consumed_at + terminal → 'Not delivered — run finished'", () => {
    renderCard(input(), "completed", true);
    expect(screen.getByText("Not delivered — run finished")).toBeTruthy();
  });

  it("consumed, not routed, running → Received", () => {
    renderCard(input({ consumed_at: RECEIVED }), "running");
    expect(screen.getByText("Received")).toBeTruthy();
  });

  it("consumed + applied, running → Routed", () => {
    renderCard(input({ consumed_at: RECEIVED, applied_at: ROUTED }), "running");
    expect(screen.getByText("Routed")).toBeTruthy();
  });

  it("received + awaiting_approval → 'Received — waits for approval'", () => {
    renderCard(input({ consumed_at: RECEIVED }), "awaiting_approval");
    expect(screen.getByText("Received — waits for approval")).toBeTruthy();
    expect(screen.queryByText(/^Received$/)).toBeNull();
  });

  it("routed + awaiting_approval → 'Routed — waits for approval'", () => {
    renderCard(input({ consumed_at: RECEIVED, applied_at: ROUTED }), "awaiting_approval");
    expect(screen.getByText("Routed — waits for approval")).toBeTruthy();
    // The state's own base word must not also read as plain Routed or as Included.
    expect(screen.queryByText(/^Routed$/)).toBeNull();
    expect(screen.queryByText(/Included/)).toBeNull();
  });

  it("received + awaiting_input → names the ANSWER, not the approval (PRD #88)", () => {
    // A follow-up submitted at a clarification park is received immediately, but it reaches
    // a prompt only on the next turn, which does not come until the human answers. Reusing
    // the gate's copy would send the user hunting for a plan gate that is not there.
    renderCard(input({ consumed_at: RECEIVED }), "awaiting_input");
    expect(screen.getByText("Received — awaits your answer")).toBeTruthy();
    expect(screen.queryByText(/waits for approval/)).toBeNull();
  });

  it("routed + awaiting_input → 'Routed — awaits your answer'", () => {
    renderCard(input({ consumed_at: RECEIVED, applied_at: ROUTED }), "awaiting_input");
    expect(screen.getByText("Routed — awaits your answer")).toBeTruthy();
  });

  it("routed + awaiting_followup → 'Routed — resumes the run' (PRD #517)", () => {
    renderCard(input({ consumed_at: RECEIVED, applied_at: ROUTED }), "awaiting_followup");
    expect(screen.getByText("Routed — resumes the run")).toBeTruthy();
  });

  it("included_at set → 'Included in a prompt' on every run status, with an honest title", () => {
    for (const [status, terminal] of [
      ["running", false],
      ["awaiting_approval", false],
      ["completed", true],
    ] as const) {
      cleanup();
      renderCard(input({ consumed_at: RECEIVED, applied_at: ROUTED, included_at: INCLUDED }), status, terminal);
      const chip = screen.getByText("Included in a prompt");
      expect(chip).toBeTruthy();
      expect(chip.closest("[title]")?.getAttribute("title")).toBe(
        "Included in a prompt the agent was given; whether it acted on it shows in the agent's messages.",
      );
      expect(screen.queryByText(/Delivered|Not confirmed/)).toBeNull();
    }
  });

  it("consumed, inclusion unconfirmed, terminal → 'Not confirmed — run finished'", () => {
    renderCard(input({ consumed_at: RECEIVED, applied_at: ROUTED }), "completed", true);
    const chip = screen.getByText("Not confirmed — run finished");
    expect(chip).toBeTruthy();
    expect(chip.closest("[title]")?.getAttribute("title")).toBe(
      "The worker received this follow-up, but no prompt carrying it was confirmed before the run finished.",
    );
    expect(screen.queryByText(/Not included/)).toBeNull();
    expect(screen.queryByText(/Delivered/)).toBeNull();
  });

  it("received after a resume (non-terminal, not included) → still Received, not Not confirmed", () => {
    renderCard(input({ consumed_at: RECEIVED }), "running");
    expect(screen.getByText("Received")).toBeTruthy();
    expect(screen.queryByText(/Not confirmed/)).toBeNull();
  });

  it("legacy (no inclusion report), live → 'Received/Routed — no inclusion report'", () => {
    renderCard(input({ consumed_at: RECEIVED, inclusion_reported: false }), "running");
    expect(screen.getByText("Received — no inclusion report")).toBeTruthy();
    cleanup();
    renderCard(input({ consumed_at: RECEIVED, applied_at: ROUTED, inclusion_reported: false }), "running");
    expect(screen.getByText("Routed — no inclusion report")).toBeTruthy();
  });

  it("legacy at a park → the no-inclusion-report chip wins over the park qualifier", () => {
    for (const status of ["awaiting_approval", "awaiting_input", "awaiting_followup"]) {
      cleanup();
      renderCard(input({ consumed_at: RECEIVED, inclusion_reported: false }), status);
      expect(screen.getByText("Received — no inclusion report")).toBeTruthy();
      expect(screen.queryByText(/waits for approval|awaits your answer|resumes the run/)).toBeNull();
    }
  });

  it("legacy, terminal → never claims 'Not confirmed' (the worker cannot say)", () => {
    renderCard(input({ consumed_at: RECEIVED, applied_at: ROUTED, inclusion_reported: false }), "completed", true);
    expect(screen.getByText("Routed — no inclusion report")).toBeTruthy();
    expect(screen.queryByText(/Not confirmed/)).toBeNull();
  });

  it("gate copy degrades to plain Received when status is not provided", () => {
    renderCard(input({ consumed_at: RECEIVED }), undefined);
    expect(screen.getByText("Received")).toBeTruthy();
    expect(screen.queryByText(/waits for approval/)).toBeNull();
  });
});

// PRD #634: an operator scope-ceiling directive (kind "scope") is never consumed —
// its chip is driven ENTIRELY by `disposition`, not by consumed_at. The row also
// carries a small "scope" tag so it is distinguishable from a follow-up.
describe("SteerQueueCard scope directives (PRD #634)", () => {
  it("disposition 'applied' → the Applied pill (finalized at the ceiling)", () => {
    render(
      <SteerQueueCard
        inputs={[scope({ disposition: "applied" })]}
        terminal={true}
        status="completed"
        busy={false}
        onStop={noop}
        onSend={noop}
      />,
    );
    expect(screen.getByText("Applied — finalized at the ceiling")).toBeTruthy();
    // The scope directive body renders (React-escaped) and is tagged as a scope row.
    expect(screen.getByText(/complete through milestone 2 of 4/)).toBeTruthy();
    expect(screen.getByText("scope")).toBeTruthy();
    // consumed_at is null on a scope row, but it must NOT read as an undelivered follow-up.
    expect(screen.queryByText("Not delivered — run finished")).toBeNull();
    expect(screen.queryByText("Queued")).toBeNull();
  });

  it("disposition null (pending) → the Active pill, NOT a consumed_at-derived chip", () => {
    render(
      <SteerQueueCard
        inputs={[scope({ disposition: null })]}
        terminal={false}
        status="running"
        busy={false}
        onStop={noop}
        onSend={noop}
      />,
    );
    expect(screen.getByText("Active — scope ceiling set")).toBeTruthy();
    // A null consumed_at on a live run would be "Queued" for a follow-up; a scope row
    // must not take that branch.
    expect(screen.queryByText("Queued")).toBeNull();
  });

  it("disposition 'superseded' → the Superseded pill", () => {
    render(
      <SteerQueueCard
        inputs={[scope({ disposition: "superseded" })]}
        terminal={false}
        status="running"
        busy={false}
        onStop={noop}
        onSend={noop}
      />,
    );
    expect(screen.getByText("Superseded — a later directive replaced it")).toBeTruthy();
  });

  it("disposition 'declined' → the Declined pill", () => {
    render(
      <SteerQueueCard
        inputs={[scope({ disposition: "declined" })]}
        terminal={true}
        status="completed"
        busy={false}
        onStop={noop}
        onSend={noop}
      />,
    );
    expect(screen.getByText("Declined — not acted on")).toBeTruthy();
  });
});

describe("SteerQueueCard survives the terminal transition (B1)", () => {
  it("renders the queue read-only (no composer, no Stop) on a terminal run with a non-empty queue", () => {
    render(
      <SteerQueueCard
        inputs={[input()]}
        terminal={true}
        status="completed"
        busy={false}
        onStop={noop}
        onSend={noop}
      />,
    );
    // Queue still shows...
    expect(screen.getByText("resume the agent")).toBeTruthy();
    // ...but the live-only steering controls are gone.
    expect(screen.queryByText("Send follow-up")).toBeNull();
    expect(screen.queryByText("Stop run")).toBeNull();
  });

  it("renders nothing on a terminal run with an empty queue", () => {
    const { container } = render(
      <SteerQueueCard inputs={[]} terminal={true} status="completed" busy={false} onStop={noop} onSend={noop} />,
    );
    expect(container.firstChild).toBeNull();
  });

  it("a live run shows the composer + Stop alongside the queue", () => {
    render(
      <SteerQueueCard inputs={[input()]} terminal={false} status="running" busy={false} onStop={noop} onSend={noop} />,
    );
    expect(screen.getByText("Send follow-up")).toBeTruthy();
    expect(screen.getByText("Stop run")).toBeTruthy();
  });

  it("a stable transition keeps an included entry's chip: same queue, terminal flips", () => {
    const rows = [input({ id: 5, consumed_at: RECEIVED, applied_at: ROUTED, included_at: INCLUDED })];
    const { rerender } = render(
      <SteerQueueCard inputs={rows} terminal={false} status="running" busy={false} onStop={noop} onSend={noop} />,
    );
    expect(screen.getByText("Included in a prompt")).toBeTruthy();
    // The run completes; the same lifted queue re-renders read-only, chip unchanged.
    rerender(
      <SteerQueueCard inputs={rows} terminal={true} status="completed" busy={false} onStop={noop} onSend={noop} />,
    );
    expect(screen.getByText("Included in a prompt")).toBeTruthy();
    expect(screen.queryByText("Send follow-up")).toBeNull();
  });
});

describe("SteerQueueCard non-owner hiding (Decision 8/N2)", () => {
  it("a non-owner viewer (canSteer=false, empty queue) renders NOTHING — no heading, no broken Send", () => {
    const { container } = render(
      <SteerQueueCard
        inputs={[]}
        terminal={false}
        status="running"
        canSteer={false}
        busy={false}
        onStop={noop}
        onSend={noop}
      />,
    );
    expect(container.firstChild).toBeNull();
    expect(screen.queryByText("Steer this run")).toBeNull();
    expect(screen.queryByText("Send follow-up")).toBeNull();
    expect(screen.queryByText("Stop run")).toBeNull();
  });

  it("an OWNER with an empty live queue (canSteer=true) still sees the composer", () => {
    render(
      <SteerQueueCard
        inputs={[]}
        terminal={false}
        status="running"
        canSteer={true}
        busy={false}
        onStop={noop}
        onSend={noop}
      />,
    );
    expect(screen.getByText("Steer this run")).toBeTruthy();
    expect(screen.getByText("Send follow-up")).toBeTruthy();
  });

  it("canSteer defaults to owner (true) when the prop is omitted", () => {
    render(<SteerQueueCard inputs={[]} terminal={false} status="running" busy={false} onStop={noop} onSend={noop} />);
    expect(screen.getByText("Send follow-up")).toBeTruthy();
  });
});

// A silent-onSend guard: the card must not throw when send/stop are wired to noops
// (RunView routes them through act()); this is a smoke assertion, not a behavior test.
describe("SteerQueueCard smoke", () => {
  it("does not crash rendering an optimistic (negative temp id) entry as Queued", () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    render(
      <SteerQueueCard
        inputs={[input({ id: -1700000000000 })]}
        terminal={false}
        status="running"
        busy={false}
        onStop={noop}
        onSend={noop}
      />,
    );
    expect(screen.getByText("Queued")).toBeTruthy();
    vi.restoreAllMocks();
  });
});

// PRD #35 (web-ux F1). The composer's Send is the run page's only FILLED primary
// button. On a parked run it sat there, fully enabled, promising "resumes the agent as
// its next turn" — for a run that will not resume for hours — while `Stop run`, the
// control the user was actually hunting for, was a muted outline below it.
describe("SteerQueueCard — the composer does not promise resumption on a parked run (PRD #35)", () => {
  const placeholder = () =>
    (screen.getByPlaceholderText(/send a follow-up message/i) as HTMLTextAreaElement).placeholder;

  it("says the message is QUEUED, not that it resumes the agent", () => {
    render(
      <SteerQueueCard inputs={[]} terminal={false} status="limit_wait" busy={false} onStop={noop} onSend={noop} />,
    );
    expect(placeholder()).toContain("queued until the run resumes");
    expect(placeholder()).not.toContain("resumes the agent as its next turn");
  });

  // Issue #754: a pool_wait run is the same self-resuming hold as limit_wait — a
  // follow-up sent here will NOT un-park it (it is blocked on a pooled token), so the
  // composer must show the "queued until the run resumes" copy, never the resumption
  // promise.
  it("says QUEUED on a pool_wait run too, never that it resumes the agent", () => {
    render(
      <SteerQueueCard inputs={[]} terminal={false} status="pool_wait" busy={false} onStop={noop} onSend={noop} />,
    );
    expect(placeholder()).toContain("queued until the run resumes");
    expect(placeholder()).not.toContain("resumes the agent as its next turn");
  });

  // Issue #1197: a recovery_wait run is the same self-resuming hold again — a follow-up
  // sent here does NOT un-park it (it auto-resumes on a capped backoff), so the composer
  // must show the "queued until the run resumes" copy, never the resumption promise.
  it("says QUEUED on a recovery_wait run too, never that it resumes the agent", () => {
    render(
      <SteerQueueCard inputs={[]} terminal={false} status="recovery_wait" busy={false} onStop={noop} onSend={noop} />,
    );
    expect(placeholder()).toContain("queued until the run resumes");
    expect(placeholder()).not.toContain("resumes the agent as its next turn");
  });

  // PRD #1190: a `paused` run is the same kind of hold as limit_wait/pool_wait from the
  // composer's side — nothing runs until Resume, so a follow-up sent here is NOT the next
  // turn; it is queued until the run resumes. The "resumes the agent" placeholder would be
  // the opposite of what happens (the very false promise limit_wait fixed).
  it("says QUEUED on a paused run too, never that it resumes the agent", () => {
    render(
      <SteerQueueCard inputs={[]} terminal={false} status="paused" busy={false} onStop={noop} onSend={noop} />,
    );
    expect(placeholder()).toContain("queued until the run resumes");
    expect(placeholder()).not.toContain("resumes the agent as its next turn");
  });

  it("leaves the copy untouched on a live run", () => {
    render(
      <SteerQueueCard inputs={[]} terminal={false} status="running" busy={false} onStop={noop} onSend={noop} />,
    );
    expect(placeholder()).toContain("resumes the agent as its next turn");
  });
});

// PRD #1190: the "‖ Pause ▾" menu beside Stop run, for a RUNNING, pausable run. The card
// only reads a handful of run fields, so a subset cast keeps each case readable (a paused
// run reads none of the omitted lifecycle fields).
function pauseRun(over: Partial<Run> = {}): Run {
  return {
    kind: "issue",
    interactive: false,
    status: "running",
    milestones: null,
    milestones_completed: null,
    checkpoint_tip_at: null,
    pause_requested_at: null,
    ...over,
  } as Run;
}

describe("SteerQueueCard — Pause ▾ menu (PRD #1190)", () => {
  const PAUSE_BTN = /pause ▾/i;

  it("is ABSENT on a chat run (unpausable kind — chat parks between turns)", () => {
    render(
      <SteerQueueCard
        inputs={[]}
        terminal={false}
        status="running"
        busy={false}
        onStop={noop}
        onSend={noop}
        run={pauseRun({ kind: "chat" })}
        onPause={vi.fn()}
      />,
    );
    expect(screen.queryByRole("button", { name: PAUSE_BTN })).toBeNull();
    // Stop run is still there — only the pause affordance is withheld.
    expect(screen.getByText("Stop run")).toBeTruthy();
  });

  it("is ABSENT on an interactive task (it parks after each turn)", () => {
    render(
      <SteerQueueCard
        inputs={[]}
        terminal={false}
        status="running"
        busy={false}
        onStop={noop}
        onSend={noop}
        run={pauseRun({ kind: "task", interactive: true })}
        onPause={vi.fn()}
      />,
    );
    expect(screen.queryByRole("button", { name: PAUSE_BTN })).toBeNull();
  });

  it("is ABSENT when run/onPause are omitted (every pre-#1190 caller is unchanged)", () => {
    render(
      <SteerQueueCard inputs={[]} terminal={false} status="running" busy={false} onStop={noop} onSend={noop} />,
    );
    expect(screen.queryByRole("button", { name: PAUSE_BTN })).toBeNull();
  });

  it("is ABSENT once a pause is already pending (the header chip owns it then)", () => {
    render(
      <SteerQueueCard
        inputs={[]}
        terminal={false}
        status="running"
        busy={false}
        onStop={noop}
        onSend={noop}
        run={pauseRun({ kind: "issue", pause_requested_at: "2026-07-20T10:00:00Z" })}
        onPause={vi.fn()}
      />,
    );
    expect(screen.queryByRole("button", { name: PAUSE_BTN })).toBeNull();
  });

  it("is PRESENT on an issue run, and its milestone item says 'After current milestone'", () => {
    render(
      <SteerQueueCard
        inputs={[]}
        terminal={false}
        status="running"
        busy={false}
        onStop={noop}
        onSend={noop}
        run={pauseRun({
          kind: "issue",
          milestones: [
            { id: "m1", title: "one" },
            { id: "m2", title: "two" },
            { id: "m3", title: "three" },
          ],
          milestones_completed: ["m1"],
        })}
        onPause={vi.fn()}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: PAUSE_BTN }));
    expect(screen.getByRole("menuitem", { name: /after current milestone/i })).toBeTruthy();
    expect(screen.queryByText(/after current step/i)).toBeNull();
    // Both items present, including the danger "Pause now".
    expect(screen.getByRole("menuitem", { name: /pause now/i })).toBeTruthy();
  });

  it("says 'After current step' on a prompt run with no frozen milestones", () => {
    render(
      <SteerQueueCard
        inputs={[]}
        terminal={false}
        status="running"
        busy={false}
        onStop={noop}
        onSend={noop}
        run={pauseRun({ kind: "prompt", milestones: null, milestones_completed: null })}
        onPause={vi.fn()}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: PAUSE_BTN }));
    expect(screen.getByRole("menuitem", { name: /after current step/i })).toBeTruthy();
    expect(screen.queryByText(/after current milestone/i)).toBeNull();
  });

  it("HIDES the milestone item on the final milestone (only Pause now remains)", () => {
    render(
      <SteerQueueCard
        inputs={[]}
        terminal={false}
        status="running"
        busy={false}
        onStop={noop}
        onSend={noop}
        run={pauseRun({
          kind: "issue",
          milestones: [
            { id: "m1", title: "one" },
            { id: "m2", title: "two" },
            { id: "m3", title: "three" },
          ],
          // two of three done → the third (final) is in flight → milestone park == finishing.
          milestones_completed: ["m1", "m2"],
        })}
        onPause={vi.fn()}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: PAUSE_BTN }));
    expect(screen.queryByText(/after current milestone/i)).toBeNull();
    expect(screen.queryByText(/after current step/i)).toBeNull();
    expect(screen.getByRole("menuitem", { name: /pause now/i })).toBeTruthy();
  });

  it("'Pause now' names the checkpoint age from checkpoint_tip_at", () => {
    render(
      <SteerQueueCard
        inputs={[]}
        terminal={false}
        status="running"
        busy={false}
        onStop={noop}
        onSend={noop}
        run={pauseRun({
          kind: "issue",
          checkpoint_tip_at: new Date(Date.now() - 9 * 60_000).toISOString(),
        })}
        onPause={vi.fn()}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: PAUSE_BTN }));
    // The age is NAMED — the invariant the copy exists for (D5), pinned by the shape, not a
    // brittle exact minute.
    expect(screen.getByText(/Work since the last checkpoint \(.+ ago\) is discarded\./)).toBeTruthy();
    // And it is roughly the value we seeded.
    expect(screen.getByText(/\(9m ago\)/)).toBeTruthy();
  });

  it("'Pause now' drops the age clause when no checkpoint has been pushed yet", () => {
    render(
      <SteerQueueCard
        inputs={[]}
        terminal={false}
        status="running"
        busy={false}
        onStop={noop}
        onSend={noop}
        run={pauseRun({ kind: "issue", checkpoint_tip_at: null })}
        onPause={vi.fn()}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: PAUSE_BTN }));
    expect(screen.getByText("Drops the turn in flight. Work since the last checkpoint is discarded.")).toBeTruthy();
  });

  it("clicking an item calls onPause with the mode and closes the menu", () => {
    const onPause = vi.fn();
    render(
      <SteerQueueCard
        inputs={[]}
        terminal={false}
        status="running"
        busy={false}
        onStop={noop}
        onSend={noop}
        run={pauseRun({ kind: "issue" })}
        onPause={onPause}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: PAUSE_BTN }));
    fireEvent.click(screen.getByRole("menuitem", { name: /pause now/i }));
    expect(onPause).toHaveBeenCalledWith("now");
    // Menu closed after a choice.
    expect(screen.queryByRole("menuitem", { name: /pause now/i })).toBeNull();
  });

  it("still ACCEPTS a follow-up while parked — the fix is honesty about when, not a block", () => {
    // Queueing a message for a parked run is genuinely useful: it is delivered on
    // resume. Disabling the composer would remove a working feature to fix a sentence.
    const onSend = vi.fn();
    render(
      <SteerQueueCard inputs={[]} terminal={false} status="limit_wait" busy={false} onStop={noop} onSend={onSend} />,
    );
    const box = screen.getByPlaceholderText(/send a follow-up message/i) as HTMLTextAreaElement;
    fireEvent.change(box, { target: { value: "try the other approach" } });
    fireEvent.click(screen.getByRole("button", { name: /send follow-up/i }));
    expect(onSend).toHaveBeenCalledWith("try the other approach");
  });
});
