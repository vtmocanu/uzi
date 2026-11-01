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
      <EffortSelect value={effort} onChange={setEffort} defaultLabel="Default · test" />
      <output data-testid="effort">{effort}</output>
    </>
  );
}

const combo = () => screen.getByRole("combobox") as HTMLSelectElement;
const value = () => screen.getByTestId("effort").textContent;

describe("EffortSelect", () => {
  // Enum-drift guard (PRD #617 SC/risk): the rendered option set equals the documented
  // closed set. The level const is UNEXPORTED, so we assert via the rendered <option>s.
  it("renders exactly the Default option plus the five closed levels", () => {
    render(<Harness initial="" />);
    const values = Array.from(combo().options).map((o) => o.value);
    expect(values).toEqual(["", "low", "medium", "high", "xhigh", "max"]);
  });

  // Label guard (issue #3255): the first option text is exactly the caller's
  // defaultLabel and no option ever says "Inherit".
  it("uses the passed defaultLabel as the first option and never says Inherit", () => {
    render(<Harness initial="" />);
    const labels = Array.from(combo().options).map((o) => o.text);
    expect(labels[0]).toBe("Default · test");
    expect(labels.some((t) => /inherit/i.test(t))).toBe(false);
  });

  // Copy guard (issue #1157): the visible option LABELS surface uzi's default effort
  // (medium) — the Default option and the medium option carry the "uzi default" copy,
  // the other four render their bare value. The option VALUES are unchanged (asserted
  // above), so this is a pure label check via the rendered <option>s' text.
  it("labels the Default and medium options with the uzi default copy", () => {
    render(<Harness initial="" />);
    const labels = Array.from(combo().options).map((o) => o.text);
    expect(labels).toEqual([
      "Default · test",
      "low",
      "medium (uzi default)",
      "high",
      "xhigh",
      "max",
    ]);
  });

  // PRD #3247: the preference-only runtime token is offered ONLY when the caller passes
  // runtimeLabel (the ordinary worker cards); it sits right after Default.
  it("offers the Runtime default option only when runtimeLabel is passed, after Default", () => {
    const { unmount } = render(<EffortSelect value="" onChange={() => {}} defaultLabel="Default · test" />);
    expect(Array.from(combo().options).map((o) => o.value)).not.toContain("runtime");
    unmount();
    render(<EffortSelect value="" onChange={() => {}} defaultLabel="Default · test" runtimeLabel="Runtime default · Codex decides" />);
    expect(Array.from(combo().options).map((o) => [o.value, o.text])).toEqual([
      ["", "Default · test"],
      ["runtime", "Runtime default · Codex decides"],
      ["low", "low"],
      ["medium", "medium (uzi default)"],
      ["high", "high"],
      ["xhigh", "xhigh"],
      ["max", "max"],
    ]);
  });

  it("selecting a level fires onChange with that level", () => {
    render(<Harness initial="" />);
    fireEvent.change(combo(), { target: { value: "low" } });
    expect(value()).toBe("low");
    expect(combo().value).toBe("low");
  });

  it("selecting Default fires onChange with ''", () => {
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
