// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { SalvagePanel } from "./SalvagePanel";
import type { Run } from "../lib/api";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

const RUN_UUID = "5f0c2a4e-8d1b-4c7a-9e3f-2b6d8a1c4e70";
const GOOD_REF = `refs/uzi-salvage/${RUN_UUID}`;
const TIP = "9c41e07b2d5a8f3e61b0c7d94a2e5f18b3c6d0a7";

// The panel reads only status and the salvage_* fields; a minimal cast matches the repo
// convention (RecoveryArchives.test.tsx) rather than spelling out the whole Run.
function aRun(over: Partial<Run> = {}): Run {
  return { id: RUN_UUID, status: "failed", salvage_state: "promoted", ...over } as Run;
}

function promoted(over: Partial<Run> = {}): Run {
  return aRun({
    salvage_state: "promoted",
    salvage_ref: GOOD_REF,
    salvage_tip: TIP,
    salvage_expires_at: new Date(Date.now() + 3 * 86_400_000 + 60_000).toISOString(),
    ...over,
  });
}

const heading = () => screen.queryByRole("heading", { name: "Salvage" });
const copyButton = () => screen.queryByRole("button", { name: "Copy command" });

describe("SalvagePanel per-state copy", () => {
  const cases: [string, string, string][] = [
    ["pending", "Saving a copy of the last published checkpoint…", "Saving"],
    [
      "unavailable",
      "No copy saved: the checkpoint was no longer on the forge at its recorded tip.",
      "Not saved",
    ],
    ["refused", "No copy saved: a different commit already holds this run's salvage ref.", "Not saved"],
    ["failed", "No copy saved after repeated attempts", "Not saved"],
    ["skipped_secret", "Not saved: this run failed on a secret-scan block.", "Not saved"],
    ["expired", "The saved copy expired and was removed.", "Expired"],
    ["disabled", "Salvage was turned off for this forge.", "Off"],
  ];

  it.each(cases)("%s renders its lead sentence and a worded badge", (state, lead, badge) => {
    const { container } = render(<SalvagePanel run={aRun({ salvage_state: state })} />);
    expect(heading()).not.toBeNull();
    expect(screen.getByText(lead)).toBeTruthy();
    // The badge states the outcome in words, so colour is never the only signal.
    expect(screen.getByText(badge)).toBeTruthy();
    // Wording discipline, checked against the whole rendered text (which is non-empty,
    // per the positive lead assertion above).
    const text = container.textContent ?? "";
    expect(text).not.toMatch(/recovered/i);
    expect(text).not.toMatch(/uzi-checkpoints/);
  });

  it("promoted says checkpointed commits were saved and may be behind the final local work", () => {
    const { container } = render(<SalvagePanel run={promoted()} />);
    expect(screen.getByText("Checkpointed commits saved")).toBeTruthy();
    expect(screen.getByText("Saved")).toBeTruthy();
    expect(screen.getByText(/last published checkpoint\. It may be behind the run's final local work\./)).toBeTruthy();
    // Ref, short tip (12 chars, not the full 40) and expiry (relative + absolute <time>).
    expect(screen.getByText(GOOD_REF)).toBeTruthy();
    expect(screen.getByText(TIP.slice(0, 12))).toBeTruthy();
    expect(screen.queryByText(TIP)).toBeNull();
    expect(screen.getByText(/^in 3d 0h,$/)).toBeTruthy();
    const time = container.querySelector("time");
    expect(time?.getAttribute("dateTime")).toMatch(/^\d{4}-\d{2}-\d{2}T/);
    expect(time?.textContent?.length).toBeGreaterThan(0);
    const text = container.textContent ?? "";
    expect(text).not.toMatch(/recovered/i);
    expect(text).not.toMatch(/uzi-checkpoints/);
  });

  it("pending shows the last error when there is one", () => {
    render(
      <SalvagePanel run={aRun({ salvage_state: "pending", salvage_last_error: "forge 502 on create" })} />,
    );
    expect(screen.getByText("Last attempt:", { exact: false })).toBeTruthy();
    expect(screen.getByText("forge 502 on create")).toBeTruthy();
  });

  it("failed shows the last error as escaped plain text with unsafe characters stripped", () => {
    render(
      <SalvagePanel
        run={aRun({ salvage_state: "failed", salvage_last_error: "<b>503</b>\u202e from forge" })}
      />,
    );
    expect(screen.getByText("Last error:", { exact: false })).toBeTruthy();
    // Rendered as text, not markup, and the bidi override is gone.
    expect(screen.getByText("<b>503</b> from forge")).toBeTruthy();
  });

  it("an unknown future state renders nothing, while a known one renders the panel", () => {
    const { rerender } = render(<SalvagePanel run={aRun({ salvage_state: "failed" })} />);
    expect(heading()).not.toBeNull();
    rerender(<SalvagePanel run={aRun({ salvage_state: "some_new_state" })} />);
    expect(heading()).toBeNull();
  });
});

describe("SalvagePanel visibility", () => {
  it("renders for a failed run and hides for every non-failed status", () => {
    const { rerender } = render(<SalvagePanel run={promoted()} />);
    expect(heading()).not.toBeNull();
    for (const status of ["completed", "cancelled", "running", "queued"] as const) {
      rerender(<SalvagePanel run={promoted({ status })} />);
      expect(heading()).toBeNull();
    }
  });

  it("hides when salvage_state is null or absent", () => {
    const { rerender } = render(<SalvagePanel run={promoted()} />);
    expect(heading()).not.toBeNull();
    rerender(<SalvagePanel run={promoted({ salvage_state: null })} />);
    expect(heading()).toBeNull();
    rerender(<SalvagePanel run={promoted({ salvage_state: undefined })} />);
    expect(heading()).toBeNull();
  });
});

describe("SalvagePanel fetch command", () => {
  it("offers the copyable fetch command for a well-formed promoted ref", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    render(<SalvagePanel run={promoted()} />);
    const cmd = `git fetch origin ${GOOD_REF}`;
    expect(screen.getByText(cmd)).toBeTruthy();
    const btn = copyButton();
    expect(btn).not.toBeNull();
    await act(async () => {
      fireEvent.click(btn!);
    });
    expect(writeText).toHaveBeenCalledWith(cmd);
    expect(screen.getByRole("button", { name: "Copied" })).toBeTruthy();
    expect(screen.getByText("Fetch command copied")).toBeTruthy();
  });

  it.each([
    ["an uppercase id", `refs/uzi-salvage/${RUN_UUID.toUpperCase()}`],
    ["a short id", "refs/uzi-salvage/5f0c2a4e"],
    ["a foreign namespace", `refs/uzi-checkpoint/${RUN_UUID}`],
    ["a trailing shell payload", `${GOOD_REF}; rm -rf ~`],
    ["an embedded newline", `${GOOD_REF}\n`],
  ])("withholds the command for %s but still shows the ref", (_label, ref) => {
    render(<SalvagePanel run={promoted({ salvage_ref: ref })} />);
    // Positive control: the promoted panel and its details did render.
    expect(screen.getByText("Checkpointed commits saved")).toBeTruthy();
    expect(screen.getByText(TIP.slice(0, 12))).toBeTruthy();
    expect(copyButton()).toBeNull();
    expect(screen.queryByText(/^git fetch origin/)).toBeNull();
  });

  it("withholds the command on a non-promoted state even with a well-formed ref", () => {
    const { rerender } = render(<SalvagePanel run={promoted()} />);
    expect(copyButton()).not.toBeNull();
    rerender(<SalvagePanel run={promoted({ salvage_state: "expired" })} />);
    expect(screen.getByText("The saved copy expired and was removed.")).toBeTruthy();
    expect(copyButton()).toBeNull();
  });
});
