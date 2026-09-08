// @vitest-environment jsdom
//
// SelectAllCheckbox's whole reason to exist is the indeterminate visual, which React cannot
// drive through a prop — so the load-bearing assertion is that the DOM node's `indeterminate`
// property is set imperatively, and cleared when the flag drops.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { SelectAllCheckbox } from "./SelectAllCheckbox";

afterEach(cleanup);

describe("SelectAllCheckbox", () => {
  it("sets the DOM indeterminate property when indeterminate is true", () => {
    render(<SelectAllCheckbox checked={false} indeterminate onChange={vi.fn()} label="Select all 5 shown" />);
    const box = screen.getByRole("checkbox") as HTMLInputElement;
    // indeterminate is a node property, invisible to a prop/attribute check.
    expect(box.indeterminate).toBe(true);
    expect(box.checked).toBe(false);
  });

  it("clears the indeterminate property when the flag drops to false", () => {
    const { rerender } = render(
      <SelectAllCheckbox checked={false} indeterminate onChange={vi.fn()} label="Select all 5 shown" />,
    );
    expect((screen.getByRole("checkbox") as HTMLInputElement).indeterminate).toBe(true);
    rerender(<SelectAllCheckbox checked indeterminate={false} onChange={vi.fn()} label="Select all 5 shown" />);
    const box = screen.getByRole("checkbox") as HTMLInputElement;
    expect(box.indeterminate).toBe(false);
    expect(box.checked).toBe(true);
  });

  it("uses the label as both the accessible name and the visible text", () => {
    render(<SelectAllCheckbox checked={false} onChange={vi.fn()} label="Select all 5 shown" />);
    // getByRole with a name resolves through the aria-label.
    expect(screen.getByRole("checkbox", { name: "Select all 5 shown" })).toBeTruthy();
    expect(screen.getByText("Select all 5 shown")).toBeTruthy();
  });

  it("reports the toggled value on change", () => {
    const onChange = vi.fn();
    render(<SelectAllCheckbox checked={false} onChange={onChange} label="Select all 5 shown" />);
    fireEvent.click(screen.getByRole("checkbox"));
    expect(onChange).toHaveBeenCalledWith(true);
  });
});
