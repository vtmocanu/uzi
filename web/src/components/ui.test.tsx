// @vitest-environment jsdom
// PRD #118: the Select primitive must MERGE a caller-supplied className with its
// base field styling (like Input/Textarea), not clobber the base. This positive
// pin proves the two compose — base tokens AND caller classes both survive.

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { RUN_STATUS_TONES, Select, StatusPill, Toggle } from "./ui";
import { runBadge } from "../lib/runBadge";
import type { LatestRun, RunStatus } from "../lib/api";

// A LatestRun carrying nothing but the status under test: no mr_iid (so `completed`
// stays a plain badge rather than the MR chip) and no stop_kind (so the stopped overlay
// never fires). NOW == created_at, so any elapsed suffix is deterministic.
const NOW = Date.parse("2026-07-28T00:00:00Z");
function latestRun(status: string): LatestRun {
  return {
    id: "r",
    status: status as RunStatus,
    mr_iid: null,
    mr_web_url: null,
    mr_state: null,
    failure_reason: null,
    stop_kind: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    owner_name: "V",
    worker_name: null,
    is_mine: true,
    run_count: 1,
    created_at: "2026-07-28T00:00:00Z",
    updated_at: "2026-07-28T00:00:00Z",
  };
}

afterEach(cleanup);

describe("StatusPill", () => {
  // PRD #88: RUN_STATUS_TONES falls to `{ tone: "neutral" }` for an unknown status, so a
  // status missing from the map renders as a calm grey pill — the same treatment
  // `cancelled` gets. That is the exact wrong reading for a run blocked on a human, and
  // it fails silently: the label still reads correctly, only the colour lies.
  it("gives awaiting_input the same warn treatment as the plan gate", () => {
    const { container: parked } = render(<StatusPill status="awaiting_input" />);
    const { container: gated } = render(<StatusPill status="awaiting_approval" />);
    const parkedPill = parked.firstElementChild as HTMLElement;
    const gatedPill = gated.firstElementChild as HTMLElement;
    expect(parkedPill.className).toBe(gatedPill.className);
    // The pulse rides the inner dot, so a tone-only assertion would miss it.
    expect(parkedPill.querySelector(".animate-pulse")).not.toBeNull();
    // The label is the only thing that distinguishes them, and it must. (This asserted
    // "awaiting input" until the web-ux pass: correct about the de-underscored fallback,
    // and pinning the exact phrasing runBadge's own comment rejects. The tone assertion
    // above is what this test is FOR; the label belongs to the two tests below.)
    expect(parkedPill.textContent).not.toBe(gatedPill.textContent);
    expect(gatedPill.textContent).toContain("awaiting approval");
  });

  it("renders the derived 'planning' status with the indigo plan tone (issue #321)", () => {
    // planning is not a real runs.status value — it is the effective status
    // effectiveRunStatus derives from is_planning, wired onto every surface in M3. The
    // default label fallback yields "planning", and RUN_STATUS_TONES maps it to `plan`.
    const { container } = render(<StatusPill status="planning" />);
    const pill = container.firstElementChild as HTMLElement;
    expect(container.textContent).toContain("planning");
    expect(pill.className).toContain("text-plan");
    // It pulses like running — it IS live work, just pre-approval.
    expect(pill.querySelector(".animate-pulse")).not.toBeNull();
  });

  it("still falls back to neutral for a genuinely unknown status", () => {
    const { container } = render(<StatusPill status="future_status" />);
    expect(container.textContent).toContain("future status");
  });

  it("does NOT print the phrasing runBadge deliberately rejects", () => {
    // Two independent renderings of one status had silently diverged: runBadge says
    // "needs your answer" for awaiting_input, with the reasoning that "awaiting input"
    // reads as machine-waiting-for-machine — while this pill printed exactly that, on the
    // run header and every Runs-list row. Free until now only because awaiting_approval
    // happens to read fine de-underscored.
    const { container } = render(<StatusPill status="awaiting_input" />);
    expect(container.textContent).toContain("needs your answer");
    expect(container.textContent).not.toContain("awaiting input");
  });

  it("agrees with runBadge on the LABEL for every status both surfaces render", () => {
    // The real invariant, and the one that catches the NEXT divergence rather than this
    // one. It iterates RUN_STATUS_TONES rather than a hardcoded list, so a status added
    // to the app is covered WITHOUT anyone remembering to extend this test — which is how
    // `limit_wait` (PRD #35, added on another branch) arrived already asserted.
    //
    // Complements runBadge.test.ts's TONE-agreement loop over the same map: that one pins
    // the colour, this one pins the words, and a status can drift on either alone.
    //
    // The skip is a runBadge overlay with no StatusPill counterpart, carved out the
    // same way that test carves out the stop_kind nuance:
    //   cancelled — isStoppedRun rewrites it to "stopped", and RunView passes the literal
    //               "stopped" to the pill instead, so the two never meet on this key.
    // `running` used to be skipped too — runBadge once appended live elapsed
    // ("running 4m") that a clockless pill could not match — but since issue #256 M4 the
    // elapsed moved to the meta-line duration token and the badge is a bare "running",
    // so the word now agrees with StatusPill and is asserted here rather than skipped.
    const OVERLAY_ONLY = new Set(["cancelled"]);
    const checked: string[] = [];
    for (const status of Object.keys(RUN_STATUS_TONES)) {
      if (OVERLAY_ONLY.has(status)) continue;
      const badge = runBadge(latestRun(status), NOW);
      if (badge.kind !== "badge") continue;
      const { container, unmount } = render(<StatusPill status={status} />);
      expect([status, container.textContent?.trim()]).toEqual([status, badge.label]);
      unmount();
      checked.push(status);
    }
    // An iterate-the-map loop passes vacuously if the map is empty or the skip set eats
    // it, so assert the population it actually covered — the same weakness runBadge's own
    // loop names about itself.
    expect(checked).toContain("awaiting_input");
    expect(checked).toContain("limit_wait");
    // PRD #517: the follow-up park must render one word on both surfaces too — a missing
    // RUN_STATUS_LABELS entry would leave StatusPill printing "awaiting followup" against
    // runBadge's "awaiting follow-up", which this loop then catches.
    expect(checked).toContain("awaiting_followup");
    // Issue #754: pool_wait must print "waiting for pool" on BOTH surfaces — a missing
    // RUN_STATUS_LABELS entry would leave StatusPill printing "pool wait" against
    // runBadge's "waiting for pool", which this loop then catches.
    expect(checked).toContain("pool_wait");
    expect(checked.length).toBeGreaterThanOrEqual(5);
  });
});

describe("Select", () => {
  it("merges a caller className with the base field styling", () => {
    render(
      <Select className="h-8 custom-x">
        <option value="">x</option>
      </Select>,
    );
    const select = screen.getByRole("combobox");
    // Base styling from INPUT_CLASS survives...
    expect(select.className).toContain("border-edge");
    expect(select.className).toContain("bg-raised");
    // ...and so do the caller-supplied classes.
    expect(select.className).toContain("h-8");
    expect(select.className).toContain("custom-x");
  });
});

describe("Toggle", () => {
  it("forwards aria-describedby to the switch button (backward-compatible addition)", () => {
    render(<Toggle label="Feature" checked={false} onChange={() => {}} aria-describedby="feat-desc" />);
    const sw = screen.getByRole("switch", { name: "Feature" });
    expect(sw.getAttribute("aria-describedby")).toBe("feat-desc");
  });

  it("omits aria-describedby when not supplied", () => {
    render(<Toggle label="Bare" checked={false} onChange={() => {}} />);
    expect(screen.getByRole("switch", { name: "Bare" }).hasAttribute("aria-describedby")).toBe(false);
  });
});
