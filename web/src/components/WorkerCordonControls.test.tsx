// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { WorkerCordonControls } from "./WorkerCordonControls";
import { incidentFleetWorkers } from "../mocks/data/health";
import { ApiError } from "../lib/apiError";
import type { Worker } from "../lib/api";

const help = "Manual cordon stops new intake. Assigned work can continue subject to other restrictions.";
const worker: Worker = { ...incidentFleetWorkers()[0], name: "target", ephemeral: false, manual_cordoned_at: null };
function mount(over: Partial<Worker> = {}, onChange = vi.fn().mockResolvedValue(undefined), hideAction = false) {
  return render(<WorkerCordonControls worker={{ ...worker, ...over }} disabled={false}
    hideAction={hideAction} onChange={onChange}>
    {(button, panel) => <><header data-testid="header">{button}</header>{panel}</>}
  </WorkerCordonControls>);
}
afterEach(cleanup);

describe("split manual cordon controls", () => {
  it("shows help only in the full-width form below the header and focuses the reason", () => {
    mount();
    const header = screen.getByTestId("header");
    const trigger = screen.getByRole("button", { name: "Cordon target" });
    expect(header.contains(trigger)).toBe(true);
    expect(trigger.classList.contains("h-8")).toBe(true);
    expect(screen.queryByText(help)).toBeNull();
    fireEvent.click(trigger);
    const reason = screen.getByLabelText("Reason (optional)");
    const form = reason.closest("form")!;
    expect(form.contains(screen.getByText(help))).toBe(true);
    expect(header.contains(form)).toBe(false);
    expect(header.nextElementSibling).toBe(form.parentElement);
    expect(form.parentElement?.classList.contains("w-full")).toBe(true);
    expect(document.activeElement).toBe(reason);
  });
  it.each(["cancel", "escape"])("restores button focus after %s", (action) => {
    mount();
    fireEvent.click(screen.getByRole("button", { name: "Cordon target" }));
    if (action === "cancel") fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    else fireEvent.keyDown(screen.getByLabelText("Reason (optional)"), { key: "Escape" });
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Cordon target" }));
    expect(screen.queryByText(help)).toBeNull();
  });
  it("restores button focus after success", async () => {
    const onChange = vi.fn().mockResolvedValue(undefined);
    mount({}, onChange);
    fireEvent.click(screen.getByRole("button", { name: "Cordon target" }));
    fireEvent.change(screen.getByLabelText("Reason (optional)"), { target: { value: "maintenance" } });
    fireEvent.click(screen.getByRole("button", { name: "Cordon" }));
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "Cordon target" })));
    expect(onChange).toHaveBeenCalledExactlyOnceWith(true, "maintenance");
    expect(screen.queryByLabelText("Reason (optional)")).toBeNull();
  });
  it("describes Uncordon with visually hidden text and focuses errors below the header", async () => {
    const onChange = vi.fn().mockRejectedValue(new ApiError(409, "Could not uncordon"));
    mount({ manual_cordoned_at: "2026-10-01T00:00:00Z" }, onChange);
    const trigger = screen.getByRole("button", { name: "Uncordon target" });
    const description = document.getElementById(trigger.getAttribute("aria-describedby")!)!;
    expect(description.textContent?.trim()).toBe("Uncordon removes only the manual hold.");
    expect(description.classList.contains("sr-only")).toBe(true);
    expect(screen.queryByText(help)).toBeNull();
    fireEvent.click(trigger);
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe("Could not uncordon");
    const header = screen.getByTestId("header");
    expect(header.contains(alert)).toBe(false);
    expect(header.nextElementSibling).toBe(alert.parentElement);
    await waitFor(() => expect(document.activeElement).toBe(alert));
    expect(onChange).toHaveBeenCalledExactlyOnceWith(false, undefined);
  });
  it.each([{ ephemeral: true }, { ephemeral: false }])("hides actions during delete confirmation (ephemeral=$ephemeral)", (over) => {
    mount(over, undefined, true);
    expect(screen.queryByRole("button")).toBeNull();
    expect(screen.queryByText(help)).toBeNull();
    expect(screen.getByTestId("header")).toBeTruthy();
  });
  it("renders neither control slot for an ephemeral worker", () => {
    mount({ ephemeral: true });
    expect(screen.queryByRole("button")).toBeNull();
    expect(screen.getByTestId("header").nextElementSibling).toBeNull();
  });
});
