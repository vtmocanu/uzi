// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { useState } from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { EffortSelect } from "./EffortSelect";

afterEach(cleanup);

// Controlled harness: mirrors how RunDefaults wires EffortSelect (the emitted
// effort string is the source of truth). The output node exposes the current value.
function Harness({ initial }: { initial: string }) {
  const [effort, setEffort] = useState(initial);
  return (
    <>
      <EffortSelect value={effort} onChange={setEffort} />
      <output data-testid="effort">{effort}</output>
    </>
  );
}

const combo = () => screen.getByRole("combobox") as HTMLSelectElement;
const value = () => screen.getByTestId("effort").textContent;

describe("EffortSelect", () => {
  // Enum-drift guard (PRD #617 SC/risk): the rendered option set equals the documented
  // closed set. The level const is UNEXPORTED, so we assert via the rendered <option>s.
  it("renders exactly the Inherit option plus the five closed levels", () => {
    render(<Harness initial="" />);
    const values = Array.from(combo().options).map((o) => o.value);
    expect(values).toEqual(["", "low", "medium", "high", "xhigh", "max"]);
  });

  it("selecting a level fires onChange with that level", () => {
    render(<Harness initial="" />);
    fireEvent.change(combo(), { target: { value: "low" } });
    expect(value()).toBe("low");
    expect(combo().value).toBe("low");
  });

  it("selecting Inherit fires onChange with ''", () => {
    render(<Harness initial="max" />);
    fireEvent.change(combo(), { target: { value: "" } });
    expect(value()).toBe("");
    expect(combo().value).toBe("");
  });

  it("the initial value prop selects the matching option", () => {
    render(<Harness initial="xhigh" />);
    expect(combo().value).toBe("xhigh");
  });

  // The closed-dropdown invariant: unlike ModelSelect there is NO free-text mode, so
  // no text <input> is ever rendered.
  it("renders no free-text input", () => {
    render(<Harness initial="high" />);
    expect(screen.queryByRole("textbox")).toBeNull();
  });
});
