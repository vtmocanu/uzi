// @vitest-environment jsdom
import { afterEach, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { ForgeOriginStatus } from "./ForgeOriginStatus";
import type { ForgeOriginCooldown } from "../lib/apiTypes";
import { forgeCooldownText } from "../lib/forgeCooldown";

afterEach(cleanup);
const base: ForgeOriginCooldown = {
  origin: "https://forge.example", cooled_until: "2026-10-29T12:00:00Z",
  half_open_run_id: null, state: "cooling", detail: "",
};
it("states the cooldown time, then eligibility without claiming recovery", () => {
  expect(forgeCooldownText(base, Date.parse("2026-10-29T11:00:00Z"))).toContain("Forge cooldown until ");
  expect(forgeCooldownText(base, Date.parse(base.cooled_until))).toContain("Cooldown elapsed; retry eligible, recovery unverified.");
});
it.each(["half_open", "legacy_serialized_until_upgrade"] as const)("shows the exact reserved run for %s", (state) => {
  render(<ForgeOriginStatus cooldown={{ ...base, state, half_open_run_id: "run-exact-123" }} />);
  expect(screen.getByText(/run-exact-123/).textContent).toContain("recovery unverified");
  if (state === "legacy_serialized_until_upgrade") {
    expect(screen.getByText(/run-exact-123/).textContent).toContain("active flight");
    expect(screen.getByText(/run-exact-123/).textContent).toContain("until it finishes or qualifying proof");
    expect(screen.getByText(/run-exact-123/).textContent).not.toContain("serialized until upgrade");
  }
});
it("does not turn a reserved or missing projection into healthy", () => {
  const { container, rerender } = render(<ForgeOriginStatus />);
  expect(container.textContent).toBe("");
  rerender(<ForgeOriginStatus cooldown={{ ...base, state: "reserved_unverified" }} />);
  expect(screen.getByText(/run identity unverified/)).toBeTruthy();
});
it("sanitizes hostile origin, run and detail in text and title and keeps markup inert", () => {
  const { container } = render(<ForgeOriginStatus cooldown={{ ...base, state: "half_open",
    origin: "https://for\u202ege.\u0007example", half_open_run_id: "run-\u200b123\r\n",
    detail: "<img src=x onerror=alert(1)>\u202e\u001b" }} />);
  const node = container.querySelector("p")!;
  expect(node.textContent).toContain("https://forge.example");
  expect(node.title).toBe(node.textContent);
  expect(node.title).toContain("run-123");
  expect(node.title).not.toMatch(/[\p{Cc}\p{Cf}]/u);
  expect(container.querySelector("img")).toBeNull();
  expect(node.title).toContain("<img src=x onerror=alert(1)>");
});
