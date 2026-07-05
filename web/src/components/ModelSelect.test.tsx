// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { useState } from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { ModelSelect } from "./ModelSelect";
import { Field } from "./ui";

afterEach(cleanup);

// Controlled harness: mirrors how the editor/Settings wire ModelSelect (the
// emitted model string is the source of truth). The output node exposes the
// current value for assertions.
function Harness({ initial }: { initial: string }) {
  const [model, setModel] = useState(initial);
  return (
    <>
      <ModelSelect value={model} onChange={setModel} />
      <output data-testid="model">{model}</output>
    </>
  );
}

const combo = () => screen.getByRole("combobox") as HTMLSelectElement;
const value = () => screen.getByTestId("model").textContent;

describe("ModelSelect", () => {
  it("shows inherit and no custom input for an empty value", () => {
    render(<Harness initial="" />);
    expect(combo().value).toBe("inherit");
    expect(screen.queryByLabelText("Custom model ID")).toBeNull();
  });

  it("selects a curated alias when the value is one", () => {
    render(<Harness initial="opus" />);
    expect(combo().value).toBe("opus");
    expect(screen.queryByLabelText("Custom model ID")).toBeNull();
  });

  it("initializes a non-alias value into the custom state with the text prefilled", () => {
    render(<Harness initial="claude-fable-5" />);
    expect(combo().value).toBe("custom");
    const custom = screen.getByLabelText("Custom model ID") as HTMLInputElement;
    expect(custom.value).toBe("claude-fable-5");
  });

  it("reveals the custom input on Other… and stays custom even while empty", () => {
    render(<Harness initial="" />);
    fireEvent.change(combo(), { target: { value: "custom" } });
    // The effective value is empty, but the field must not collapse back to
    // inherit — the custom input stays visible.
    expect(value()).toBe("");
    expect(combo().value).toBe("custom");
    expect(screen.getByLabelText("Custom model ID")).not.toBeNull();
  });

  it("emits the raw custom text as typed", () => {
    render(<Harness initial="custom-start" />);
    const custom = screen.getByLabelText("Custom model ID") as HTMLInputElement;
    fireEvent.change(custom, { target: { value: "claude-opus-4-8" } });
    expect(value()).toBe("claude-opus-4-8");
    expect(combo().value).toBe("custom");
  });

  it("emits an alias when picked and inherit ('') when inherit is picked", () => {
    render(<Harness initial="opus" />);
    fireEvent.change(combo(), { target: { value: "sonnet" } });
    expect(value()).toBe("sonnet");
    fireEvent.change(combo(), { target: { value: "inherit" } });
    expect(value()).toBe("");
    expect(screen.queryByLabelText("Custom model ID")).toBeNull();
  });
});

// Guards the S1 a11y fix: with Field htmlFor targeting the select's id, the
// visible "Model" label names ONLY the select (accessible name stays "Model",
// not "Model claude-fable-5"), and the custom input keeps its own name.
describe("ModelSelect label association", () => {
  function LabeledHarness({ initial }: { initial: string }) {
    const [model, setModel] = useState(initial);
    return (
      <Field label="Model" htmlFor="m">
        <ModelSelect id="m" value={model} onChange={setModel} />
      </Field>
    );
  }

  it("names only the select via the label, unpolluted in custom mode", () => {
    render(<LabeledHarness initial="claude-fable-5" />);
    const labeled = screen.getByLabelText("Model");
    expect(labeled.tagName).toBe("SELECT");
    expect((labeled as HTMLSelectElement).value).toBe("custom");
    expect(screen.getByLabelText("Custom model ID")).not.toBeNull();
  });
});
