// @vitest-environment jsdom
//
// HarnessPicker (PRD #1429 M4a): the shared three-state harness picker (inherit,
// claude, codex). It must render all three options, reflect its controlled value, and
// emit the right selection on change — mirroring TokenPicker's test shape.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { HarnessPicker } from "./HarnessPicker";
import type { HarnessSelection } from "../lib/harnessSelection";

function renderPicker(value: HarnessSelection) {
  const onChange = vi.fn();
  render(<HarnessPicker value={value} onChange={onChange} label="Harness for this run" />);
  const select = screen.getByLabelText("Harness for this run") as HTMLSelectElement;
  return { onChange, select };
}

afterEach(cleanup);

describe("HarnessPicker — always renders the three states", () => {
  it("renders inherit/claude/codex options", () => {
    renderPicker("inherit");
    expect(screen.getByRole("option", { name: /Use my default/ })).toBeTruthy();
    expect(screen.getByRole("option", { name: "Claude" })).toBeTruthy();
    expect(screen.getByRole("option", { name: "Codex" })).toBeTruthy();
    expect(screen.getAllByRole("option")).toHaveLength(3);
  });
});

describe("HarnessPicker — controlled value + change events", () => {
  it("reflects the controlled selection", () => {
    const { select } = renderPicker("codex");
    expect(select.value).toBe("codex");
  });

  it("emits the picked harness on change", () => {
    const { select, onChange } = renderPicker("inherit");
    fireEvent.change(select, { target: { value: "codex" } });
    expect(onChange).toHaveBeenCalledWith("codex");
  });

  it("emits inherit when the user re-picks the default option", () => {
    const { select, onChange } = renderPicker("claude");
    fireEvent.change(select, { target: { value: "inherit" } });
    expect(onChange).toHaveBeenCalledWith("inherit");
  });

  it("disables the control when told to", () => {
    const onChange = vi.fn();
    render(<HarnessPicker value="inherit" onChange={onChange} label="Harness" disabled />);
    expect((screen.getByLabelText("Harness") as HTMLSelectElement).disabled).toBe(true);
  });
});
