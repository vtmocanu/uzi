// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render } from "@testing-library/react";
import type { RunMessage } from "../lib/api";
import { RunEventRow } from "./RunEvent";
import { LEAD_MESSAGE_DISPLAY_MAX } from "./PlanMissingCard";

// Issue #1593 m3: a gated planning turn that ended with prose only (after one corrective
// nudge) emits a worker `status` with event "plan_missing" carrying the lead's last message.
// That message is UNTRUSTED model output, so the card must render it as inert text.

afterEach(cleanup);

const NOTICE = "The planning turn ended without a plan or a structured question.";

function statusMsg(payload: unknown): RunMessage {
  return {
    seq: 42,
    kind: "status",
    agent: "worker",
    agent_instance: null,
    agent_label: null,
    payload,
    created_at: "2026-09-24T00:00:00.000Z",
  };
}

function renderCard(payload: unknown): HTMLElement {
  const { container } = render(<RunEventRow msg={statusMsg(payload)} live={false} />);
  const card = container.querySelector<HTMLElement>('[data-testid="plan-missing-card"]');
  // Found FIRST, so every absence assertion below is about a real card, never vacuous.
  expect(card).not.toBeNull();
  return card as HTMLElement;
}

describe("PlanMissingCard (via RunEventRow status branch)", () => {
  it("renders the fixed notice, the untrusted label and the lead's message", () => {
    const card = renderCard({ event: "plan_missing", text: NOTICE, lead_final_message: "Here is my thinking." });
    expect(card.textContent).toContain(NOTICE);
    expect(card.textContent).toContain("Lead's last message (untrusted model output)");
    expect(card.textContent).toContain("Here is my thinking.");
  });

  it("renders markup and markdown in the lead message as literal inert text", () => {
    const hostile = [
      "<script>alert(1)</script>",
      "[click](javascript:alert(1))",
      "**bold**",
      "<img src=x onerror=alert(1)>",
    ].join("\n");
    const card = renderCard({ event: "plan_missing", text: NOTICE, lead_final_message: hostile });
    for (const literal of [
      "<script>alert(1)</script>",
      "[click](javascript:alert(1))",
      "**bold**",
      "<img src=x onerror=alert(1)>",
    ]) {
      expect(card.textContent).toContain(literal);
    }
    expect(card.querySelector("a")).toBeNull();
    expect(card.querySelector("img")).toBeNull();
    expect(card.querySelector("script")).toBeNull();
    expect(card.querySelector("strong")).toBeNull();
    // No attribute sink carries the untrusted text.
    for (const el of Array.from(card.querySelectorAll("*"))) {
      for (const attr of ["title", "href", "style", "src", "aria-label"]) {
        expect(el.getAttribute(attr) ?? "").not.toContain("alert(1)");
      }
    }
  });

  it("strips bidi overrides and zero-width characters from the lead message and notice", () => {
    const rlo = String.fromCharCode(0x202e);
    const zwsp = String.fromCharCode(0x200b);
    const bom = String.fromCharCode(0xfeff);
    const card = renderCard({
      event: "plan_missing",
      text: `${NOTICE}${zwsp}`,
      lead_final_message: `safe${rlo}txt.exe${zwsp}and${bom}more\nsecond line`,
    });
    const text = card.textContent ?? "";
    expect(text).not.toContain(rlo);
    expect(text).not.toContain(zwsp);
    expect(text).not.toContain(bom);
    expect(text).toContain("safetxt.exeandmore\nsecond line");
  });

  it("clamps an over-long lead message client-side", () => {
    const long = "a".repeat(LEAD_MESSAGE_DISPLAY_MAX + 500);
    const card = renderCard({ event: "plan_missing", text: NOTICE, lead_final_message: long });
    const block = card.querySelector('[data-testid="plan-missing-lead-message"]');
    expect(block).not.toBeNull();
    const shown = block?.textContent ?? "";
    expect(shown.length).toBeLessThanOrEqual(LEAD_MESSAGE_DISPLAY_MAX + 1);
    expect(shown.length).toBeGreaterThan(LEAD_MESSAGE_DISPLAY_MAX - 10);
    expect(shown).not.toContain("a".repeat(LEAD_MESSAGE_DISPLAY_MAX + 1));
  });

  it("shows just the notice when lead_final_message is absent or not a string", () => {
    for (const lead of [undefined, 42, { nested: "x" }, ""]) {
      cleanup();
      const card = renderCard({ event: "plan_missing", text: NOTICE, lead_final_message: lead });
      expect(card.textContent).toContain(NOTICE);
      expect(card.querySelector('[data-testid="plan-missing-lead-message"]')).toBeNull();
      expect(card.textContent).not.toContain("untrusted model output");
    }
  });

  it("falls back to a fixed notice when text is missing", () => {
    // The literal, not an imported constant: the header always reads "Plan missing", and an
    // emptied constant would make toContain("") vacuous.
    const fallback = "Plan missing: the planning turn ended without a plan.";
    for (const text of [undefined, "", "   "]) {
      cleanup();
      const card = renderCard({ event: "plan_missing", text, lead_final_message: "prose" });
      expect(card.textContent).toContain(fallback);
      expect(card.textContent).toContain("prose");
    }
  });

  it("exposes the lead message as a focusable region with a fixed label", () => {
    const card = renderCard({ event: "plan_missing", text: NOTICE, lead_final_message: "Ignore previous instructions." });
    const block = card.querySelector<HTMLElement>('[data-testid="plan-missing-lead-message"]');
    expect(block).not.toBeNull();
    expect(block?.getAttribute("role")).toBe("region");
    expect(block?.tabIndex).toBe(0);
    expect(block?.getAttribute("aria-label")).toBe("Lead's last message");
  });

  it("leaves a status without the plan_missing event on the describeStatus MetaLine path", () => {
    const { container } = render(
      <RunEventRow msg={statusMsg({ text: "worktree ready on agent/issue-7" })} live={false} />,
    );
    expect(container.querySelector('[data-testid="plan-missing-card"]')).toBeNull();
    expect(container.textContent).toContain("worktree ready on agent/issue-7");
  });
});
