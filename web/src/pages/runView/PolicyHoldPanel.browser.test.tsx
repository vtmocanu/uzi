import "../../index.css";
import { afterEach, expect, it, vi } from "vitest";
import { page, userEvent } from "vitest/browser";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { PolicyHoldPanel } from "./PolicyHoldPanel";
import { api, type Run } from "../../lib/api";
import wording from "../../../../fixtures/policy-hold/wording.json";

vi.mock("../../lib/api", async original => {
  const actual = await original<typeof import("../../lib/api")>();
  return { ...actual, api: { ...actual.api, resumePolicyHold: vi.fn() } };
});
const originalTheme = document.documentElement.getAttribute("data-theme");
afterEach(() => {
  cleanup(); vi.clearAllMocks();
  if (originalTheme === null) document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", originalTheme);
});

const run = {
  id: "00000000-0000-0000-0000-000000003607", status: "paused", hold_reason: "codex_policy_hold",
  policy_hold: {
    preserve_required: false, pending_fresh: false, outcome_id: "episode-7",
    continuation_mode: "fresh_from_saved_work", resolved_at: null, applying_worker_id: null,
    applying_generation: 2, source_generation: 2, refusal_count: 1, policy_tag: null, phase: "implementation",
    correlation_id: null, published: true, captured_head: "0123456789abcdef0123456789abcdef01234567",
    hold_cause: "attempt_path", resume_modes: ["fresh_from_saved_work"],
    recovery_required: false, recovery_verified_this_claim: false,
  },
} as Run;
const matrix = [1100, 375].flatMap(width => ["dawn", "ember"].map(theme => ({ width, theme })));

it.each(matrix)("keyboard confirm flow at $width in $theme", async ({ width, theme }) => {
  await page.viewport(width, 900);
  document.documentElement.setAttribute("data-theme", theme);
  const refreshRun = vi.fn();
  vi.mocked(api.resumePolicyHold).mockResolvedValue({ run, replay: true });
  render(<main className="p-4"><PolicyHoldPanel run={run} canSteer refreshRun={refreshRun} /></main>);
  const label = wording.modes.fresh_from_saved_work.label;

  await userEvent.tab();
  const action = screen.getByRole("button", { name: label });
  expect(document.activeElement).toBe(action);
  expect(action.matches(":focus-visible")).toBe(true);
  expect(getComputedStyle(action).boxShadow).not.toBe("none");
  await userEvent.type(page.elementLocator(action), "{Enter}", { skipClick: true });

  const confirm = screen.getByRole("group", { name: label });
  await waitFor(() => expect(document.activeElement).toBe(confirm));
  expect(document.getElementById(confirm.getAttribute("aria-describedby")!)?.textContent)
    .toBe(wording.modes.fresh_from_saved_work.consequence);

  await userEvent.tab();
  const box = screen.getByLabelText("Guidance for the agent (optional)");
  expect(document.activeElement).toBe(box);
  expect(getComputedStyle(box).boxShadow).not.toBe("none");
  await userEvent.keyboard("Keep the patch minimal.");
  expect(screen.getByText("23 / 16,000 bytes")).toBeTruthy();

  expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(width);
  for (const el of confirm.querySelectorAll("p, button, textarea")) {
    expect(el.getBoundingClientRect().right).toBeLessThanOrEqual(width);
  }

  await userEvent.tab();
  const submit = screen.getByRole("button", { name: "Confirm and resume" });
  expect(document.activeElement).toBe(submit);
  await userEvent.type(page.elementLocator(submit), "{Enter}", { skipClick: true });
  await waitFor(() => expect(api.resumePolicyHold).toHaveBeenCalledExactlyOnceWith(run.id,
    { mode: "fresh_from_saved_work", expected_episode_id: "episode-7", guidance: "Keep the patch minimal." }));
  const note = await screen.findByText("Already resumed (this request was applied earlier).");
  await waitFor(() => expect(document.activeElement).toBe(note.parentElement));
  expect(refreshRun).toHaveBeenCalled();
});
