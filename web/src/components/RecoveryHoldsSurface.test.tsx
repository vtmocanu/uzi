// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RecoveryHoldsSurface } from "./RecoveryHoldsSurface";
import { api, type RecoveryCustodyHold, type RecoveryCustodyHolds } from "../lib/api";

// The surface fetches its own owner-wide hold listing and mutates via api.discardHold. Mock
// those two; keep the recovery grouping/view logic real so these pins exercise the real
// classification and action gating.
vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return { ...actual, api: { getRecoveryHolds: vi.fn(), discardHold: vi.fn() } };
});
const mockApi = vi.mocked(api);

// Hostile worker name as \u ESCAPES (never raw bytes): a ZWSP, an RTL override and an
// HTML-injection payload. React escapes the tag; only stripUnsafeChars removes the
// invisibles — the two guards are asserted separately below.
const ZWSP = String.fromCharCode(0x200b);
const RLO = String.fromCharCode(0x202e);
const HOSTILE = `ci${ZWSP}runner${RLO}<script>alert(1)</script>`;

beforeEach(() => {
  Object.defineProperty(document, "hidden", { configurable: true, get: () => false });
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function hold(over: Partial<RecoveryCustodyHold> = {}): RecoveryCustodyHold {
  return {
    id: "h1",
    run_id: "r1",
    generation: 1,
    state: "open",
    attention: "active",
    worker_id: "w1",
    worker_name: "base (M)",
    has_available_capture: false,
    created_at: "2026-09-14T00:00:00Z",
    updated_at: "2026-09-14T00:00:00Z",
    ...over,
  };
}

function listing(holds: RecoveryCustodyHold[]): RecoveryCustodyHolds {
  const open = holds.filter((h) => h.state === "open");
  return {
    aggregate: {
      open_holds: open.length,
      custody_hold_limit: 8,
      decision_needed: open.filter((h) => h.attention === "source_only" || h.attention === "needs_action").length,
      blocked_runs: 0,
    },
    holds,
  };
}

async function renderSurface(resp: RecoveryCustodyHolds) {
  mockApi.getRecoveryHolds.mockResolvedValue(resp);
  const utils = render(
    <MemoryRouter>
      <RecoveryHoldsSurface />
    </MemoryRouter>,
  );
  await waitFor(() => expect(mockApi.getRecoveryHolds).toHaveBeenCalled());
  await act(async () => {
    await Promise.resolve();
  });
  return utils;
}

describe("RecoveryHoldsSurface", () => {
  it("self-hides when there are no holds", async () => {
    const { container } = await renderSurface(listing([]));
    expect(container.innerHTML).toBe("");
  });

  it("shows only decision-needed holds, grouped by worker, filtering out healthy protection (PRD #1371)", async () => {
    // A mix across two workers: healthy active + archive_ready on base (M) (filtered out), a
    // needs_action on base (M) and a source_only on jvm-worker (both shown). The surface is
    // decision-only, so the healthy rows never appear.
    await renderSurface(
      listing([
        hold({ id: "h-active", worker_id: "wa", worker_name: "base (M)", attention: "active" }),
        hold({ id: "h-ready", worker_id: "wa", worker_name: "base (M)", attention: "archive_ready", generation: 2, has_available_capture: true, capture_state: "available" }),
        hold({ id: "h-need", worker_id: "wa", worker_name: "base (M)", attention: "needs_action", generation: 3 }),
        hold({ id: "h-src", worker_id: "wb", worker_name: "jvm-worker", attention: "source_only" }),
      ]),
    );
    // Only the decision holds render, each with its state label.
    expect(screen.getByText("Needs attention")).toBeTruthy();
    expect(screen.getByText("Decision required")).toBeTruthy();
    // Healthy protection is filtered out entirely.
    expect(screen.queryByText("Active protection")).toBeNull();
    expect(screen.queryByText("Archive ready")).toBeNull();
    // Both worker groups (each carrying a decision hold) are present.
    expect(screen.getByText("jvm-worker")).toBeTruthy();
    expect(screen.getAllByText("base (M)").length).toBeGreaterThan(0);
  });

  it("hides when every open hold is healthy (active/capturing/archive_ready) (PRD #1371)", async () => {
    // None of these need an owner decision, so the decision-only surface renders nothing —
    // even the archive_ready export lives on the run view now.
    const { container } = await renderSurface(
      listing([
        hold({ id: "h-active", attention: "active" }),
        hold({ id: "h-capturing", attention: "capturing", generation: 2 }),
        hold({ id: "h-ready", attention: "archive_ready", generation: 3, has_available_capture: true, capture_state: "available" }),
      ]),
    );
    expect(container.innerHTML).toBe("");
  });

  it("hides when every hold is terminal (released/discarded) (PRD #1371)", async () => {
    // Released/discarded holds are not decisions and are excluded server-side by ?state=open;
    // even if one arrives, the decision-only surface renders nothing.
    const { container } = await renderSurface(
      listing([
        hold({ id: "h-rel", state: "released", attention: "released" }),
        hold({ id: "h-disc", state: "discarded", attention: "discarded", generation: 2 }),
      ]),
    );
    expect(container.innerHTML).toBe("");
  });

  it("sanitizes an untrusted worker name (no <script>, no invisible chars)", async () => {
    const { container } = await renderSurface(listing([hold({ worker_id: "wx", worker_name: HOSTILE, attention: "source_only" })]));
    // React escaping: no real <script> element was created.
    expect(container.querySelector("script")).toBeNull();
    // stripUnsafeChars: the rendered worker-name text has neither the ZWSP nor the RLO.
    const nameEl = screen.getAllByText(/ci.*runner/)[0];
    expect(nameEl.textContent ?? "").not.toContain(ZWSP);
    expect(nameEl.textContent ?? "").not.toContain(RLO);
  });

  it("discards a possible-only-copy hold behind a typed confirmation naming run/worker/generation/hold", async () => {
    // First listing has the source-only hold + one active hold. The decision-only surface
    // (PRD #1371) filters the active hold out, so only the source-only hold renders a discard
    // control; the reload drops the discarded one, leaving nothing to decide (surface hides).
    const src = hold({ id: "h-src", run_id: "run-xyz", worker_id: "wb", worker_name: "jvm-worker", generation: 4, attention: "source_only" });
    const keep = hold({ id: "h-keep", run_id: "run-keep", worker_id: "wb", worker_name: "jvm-worker", attention: "active" });
    mockApi.getRecoveryHolds
      .mockResolvedValueOnce(listing([src, keep]))
      .mockResolvedValue(listing([keep]));
    mockApi.discardHold.mockResolvedValue({ discarded: true });

    render(
      <MemoryRouter>
        <RecoveryHoldsSurface />
      </MemoryRouter>,
    );
    await waitFor(() => expect(mockApi.getRecoveryHolds).toHaveBeenCalled());
    await act(async () => {
      await Promise.resolve();
    });

    // Arm the confirmation.
    fireEvent.click(screen.getByRole("button", { name: /Discard held work/ }));

    // The warning names the run, worker, generation and hold id (D9).
    const group = screen.getByRole("group", { name: /Discard held work for run run-xyz/ });
    const groupText = group.textContent ?? "";
    expect(groupText).toContain("only copy");
    expect(groupText).toContain("run-xyz");
    expect(groupText).toContain("jvm-worker");
    expect(groupText).toContain("generation 4");
    expect(groupText).toContain("h-src");

    // The destructive button is disabled until the exact word is typed.
    const confirmBtn = () => within(group).getByRole("button", { name: /Discard held work/ });
    expect((confirmBtn() as HTMLButtonElement).disabled).toBe(true);
    const input = within(group).getByRole("textbox");
    fireEvent.change(input, { target: { value: "nope" } });
    expect((confirmBtn() as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(input, { target: { value: "discard" } });
    expect((confirmBtn() as HTMLButtonElement).disabled).toBe(false);

    fireEvent.click(confirmBtn());
    await waitFor(() => expect(mockApi.discardHold).toHaveBeenCalledWith("run-xyz", "h-src"));
    // The reload drops the discarded hold.
    await waitFor(() => expect(screen.queryByText("Decision required")).toBeNull());
  });

  it("Escape cancels the confirmation and performs no mutation", async () => {
    await renderSurface(listing([hold({ id: "h-src", attention: "source_only" })]));
    fireEvent.click(screen.getByRole("button", { name: /Discard held work/ }));
    const group = screen.getByRole("group", { name: /Discard held work/ });
    fireEvent.keyDown(group, { key: "Escape" });
    expect(screen.queryByRole("group", { name: /Discard held work/ })).toBeNull();
    expect(mockApi.discardHold).not.toHaveBeenCalled();
  });

  it("hides a lone healthy active hold — no card, no destructive action (PRD #1371)", async () => {
    const { container } = await renderSurface(listing([hold({ attention: "active" })]));
    expect(container.innerHTML).toBe("");
    expect(screen.queryByRole("button", { name: /Discard held work/ })).toBeNull();
    expect(screen.queryByRole("link", { name: /Export archive/ })).toBeNull();
  });

  it("clears busy after a successful discard whose reload fails, so the row does not wedge", async () => {
    // Discard succeeds, but the follow-up reload rejects. `load` swallows that and keeps the
    // last-good listing, so THIS row stays mounted — the busy flag must still clear (finally),
    // or the row wedges on "Discarding…" with its controls disabled until the next poll.
    const src = hold({ id: "h-src", run_id: "run-xyz", worker_id: "wb", worker_name: "jvm-worker", generation: 4, attention: "source_only" });
    mockApi.getRecoveryHolds
      .mockResolvedValueOnce(listing([src]))
      .mockRejectedValue(new Error("reload failed"));
    mockApi.discardHold.mockResolvedValue({ discarded: true });

    render(
      <MemoryRouter>
        <RecoveryHoldsSurface />
      </MemoryRouter>,
    );
    await waitFor(() => expect(mockApi.getRecoveryHolds).toHaveBeenCalled());
    await act(async () => {
      await Promise.resolve();
    });

    fireEvent.click(screen.getByRole("button", { name: /Discard held work/ }));
    const group = screen.getByRole("group", { name: /Discard held work/ });
    fireEvent.change(within(group).getByRole("textbox"), { target: { value: "discard" } });
    fireEvent.click(within(group).getByRole("button", { name: /Discard held work/ }));

    await waitFor(() => expect(mockApi.discardHold).toHaveBeenCalledWith("run-xyz", "h-src"));
    // The reload failed, so the row is still here — but the confirm button is enabled again
    // (busy cleared) and no longer reads "Discarding…". Its name reverting to "Discard held
    // work" (not "Discarding…") is itself the proof busy cleared.
    await waitFor(() => {
      const btn = within(
        screen.getByRole("group", { name: /Discard held work/ }),
      ).getByRole("button", { name: /Discard held work/ }) as HTMLButtonElement;
      expect(btn.disabled).toBe(false);
      expect(btn.textContent).not.toContain("Discarding");
    });
  });
});
