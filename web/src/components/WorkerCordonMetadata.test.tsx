// @vitest-environment jsdom
import { afterEach, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { mockWorkers } from "../mocks/data";
import { WorkerCordonMetadata } from "./WorkerCordonMetadata";

afterEach(cleanup);

it("renders escaped attribution offline under custody, with safe title and dateTime attributes", () => {
  const { container } = render(<WorkerCordonMetadata worker={{
    ...mockWorkers[0], status: "offline", retaining_unpublished_work: true,
    manual_cordoned_at: "2026-10-21T12:00:00Z\n",
    manual_cordoned_by_label: "\u001b[31mAda\u001b[0m\u202e\t",
    manual_cordon_reason: "\u001b]0;hidden\u001b\\<b>check</b>\n🚀\u0085\u200b",
  }} />);
  expect(screen.getByTitle("Ada").textContent).toBe("Ada");
  expect(screen.getByTitle("<b>check</b>🚀").textContent).toBe("Reason: <b>check</b>🚀");
  const time = container.querySelector("time")!;
  expect(time.getAttribute("datetime")).toBe("2026-10-21T12:00:00.000Z");
  expect(time.title).toBe("2026-10-21T12:00:00.000Z");
  expect(container.querySelector("b")).toBeNull();
  expect(container.querySelectorAll("button, input, a")).toHaveLength(0);
  for (const el of container.querySelectorAll("*")) {
    for (const attr of el.attributes) expect(attr.value).not.toMatch(/[\p{Cc}\p{Cf}]/u);
  }
});

it("falls back to UUID and handles invalid timestamps, missing reasons and hostile optional DTOs", () => {
  const { container, rerender } = render(<WorkerCordonMetadata worker={{
    ...mockWorkers[0], manual_cordoned_at: "invalid\u202e",
    manual_cordoned_by_label: "\n\t\u200b", manual_cordoned_by: "actor\u202e-id",
  }} />);
  expect(screen.getByTitle("actor-id").textContent).toBe("actor-id");
  expect(screen.getByText("time unavailable")).toBeTruthy();
  expect(container.querySelector("time")).toBeNull();
  expect(container.textContent).not.toContain("Reason:");
  rerender(<WorkerCordonMetadata worker={{
    ...mockWorkers[0], manual_cordoned_at: {} as string,
    manual_cordoned_by_label: [] as unknown as string,
    manual_cordon_reason: 42 as unknown as string,
  }} />);
  expect(screen.getByText("unknown actor")).toBeTruthy();
  expect(screen.getByText("time unavailable")).toBeTruthy();
  rerender(<WorkerCordonMetadata worker={mockWorkers[0]} />);
  expect(container.innerHTML).toBe("");
});
