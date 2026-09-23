// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { useState } from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import type { Harness as HarnessKind } from "../lib/api";
import { ModelSelect } from "./ModelSelect";
import { Field } from "./ui";

afterEach(cleanup);

// Controlled harness: mirrors how the editor/Settings wire ModelSelect (the
// emitted model string is the source of truth). The output node exposes the
// current value for assertions.
function Harness({
  initial,
  pickerHarness,
  allowCustom,
}: {
  initial: string;
  pickerHarness?: HarnessKind;
  allowCustom?: boolean;
}) {
  const [model, setModel] = useState(initial);
  return (
    <>
      <ModelSelect value={model} onChange={setModel} harness={pickerHarness} allowCustom={allowCustom} />
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

// PRD #1429 D6: the two closed harness vocabularies. Claude keeps today's aliases
// (defaulting harness omitted, so every pre-M4a call site is unaffected); Codex is
// EXACTLY gpt-6-astra/gpt-5.6-sol/gpt-6-sol with NO custom escape hatch.
describe("ModelSelect — harness-scoped vocabulary (PRD #1429 D6)", () => {
  it("offers today's Claude aliases plus Other… when harness is omitted (back-compat)", () => {
    render(<Harness initial="" />);
    expect(screen.getByRole("option", { name: "opus" })).toBeTruthy();
    expect(screen.getByRole("option", { name: "sonnet" })).toBeTruthy();
    expect(screen.getByRole("option", { name: /Other/ })).toBeTruthy();
    expect(screen.queryByRole("option", { name: "gpt-6-astra" })).toBeNull();
  });

  it("offers the Claude vocabulary explicitly when harness='claude'", () => {
    render(<Harness initial="" pickerHarness="claude" />);
    expect(screen.getByRole("option", { name: "opus" })).toBeTruthy();
    expect(screen.getByRole("option", { name: /Other/ })).toBeTruthy();
  });

  it("offers ONLY the curated Codex models, with no custom escape hatch", () => {
    render(<Harness initial="" pickerHarness="codex" />);
    expect(screen.getByRole("option", { name: "gpt-6-astra" })).toBeTruthy();
    expect(screen.getByRole("option", { name: "gpt-5.6-sol" })).toBeTruthy();
    expect(screen.getByRole("option", { name: "gpt-6-sol" })).toBeTruthy();
    expect(screen.getByRole("option", { name: "Inherit (account default)" })).toBeTruthy();
    expect(screen.queryByRole("option", { name: /Other/ })).toBeNull();
    // No Claude alias leaks into the Codex vocabulary.
    expect(screen.queryByRole("option", { name: "opus" })).toBeNull();
  });

  it("selects a curated Codex alias when the value is one", () => {
    render(<Harness initial="gpt-6-astra" pickerHarness="codex" />);
    expect(combo().value).toBe("gpt-6-astra");
    expect(screen.queryByLabelText("Custom model ID")).toBeNull();
  });

  it("re-derives the picker when the harness prop itself changes (no value change)", () => {
    function Switcher() {
      const [h, setH] = useState<HarnessKind>("claude");
      const [model, setModel] = useState("opus");
      return (
        <>
          <button type="button" onClick={() => setH("codex")}>
            switch to codex
          </button>
          <ModelSelect value={model} onChange={setModel} harness={h} />
        </>
      );
    }
    render(<Switcher />);
    // "opus" is curated under Claude.
    expect(combo().value).toBe("opus");
    fireEvent.click(screen.getByRole("button", { name: "switch to codex" }));
    // The SAME stored value ("opus") is not part of the Codex vocabulary, so the
    // picker must re-derive it as custom rather than keep showing a Codex <option
    // value="opus"> that no longer exists (a stale <select> value would silently
    // fall back to the browser's first option instead).
    expect(combo().value).toBe("custom");
    expect((screen.getByLabelText("Custom model ID") as HTMLInputElement).value).toBe("opus");
  });
});

// PRD #1551 M3: `allowCustom` explicitly overrides the harness default. The Codex Run
// Defaults lane opts in; ScheduleModal / AgentTemplateEditor omit it and keep the
// harness-derived default (Claude yes, Codex no).
describe("ModelSelect — explicit allowCustom (PRD #1551 M3)", () => {
  it("opens the Other… escape hatch for Codex when allowCustom is set", () => {
    render(<Harness initial="" pickerHarness="codex" allowCustom />);
    // Positive control: the curated Codex option is present…
    expect(screen.getByRole("option", { name: "gpt-6-astra" })).toBeTruthy();
    // …and now so is Other…, which the default Codex picker withholds.
    expect(screen.getByRole("option", { name: /Other/ })).toBeTruthy();
    fireEvent.change(combo(), { target: { value: "custom" } });
    fireEvent.change(screen.getByLabelText("Custom model ID"), {
      target: { value: "gpt-5.6-terra" },
    });
    expect(value()).toBe("gpt-5.6-terra");
    expect(combo().value).toBe("custom");
  });

  it("still withholds Other… for Codex when allowCustom is NOT passed (default caller)", () => {
    render(<Harness initial="" pickerHarness="codex" />);
    // Positive control that the picker rendered: the curated Codex option exists.
    expect(screen.getByRole("option", { name: "gpt-6-astra" })).toBeTruthy();
    // The negative it guards: no Other… entry without the explicit opt-in.
    expect(screen.queryByRole("option", { name: /Other/ })).toBeNull();
  });

  it("allowCustom={false} closes the escape hatch even for Claude", () => {
    render(<Harness initial="" pickerHarness="claude" allowCustom={false} />);
    // Positive control: a Claude curated option is present…
    expect(screen.getByRole("option", { name: "opus" })).toBeTruthy();
    // …but the explicit false suppresses Other…, overriding the Claude default.
    expect(screen.queryByRole("option", { name: /Other/ })).toBeNull();
  });
});

// PRD #1551 M3: the custom input placeholder is provider-specific, so a Codex custom id
// hints at gpt-… and a Claude one at claude-…. Placeholder is an attribute sink, asserted
// on the attribute, never on textContent (web.md "Untrusted values rendered into attributes").
describe("ModelSelect — provider-specific custom placeholder (PRD #1551 M3)", () => {
  it("uses a gpt-… placeholder in the Codex custom input", () => {
    render(<Harness initial="gpt-5.6-terra" pickerHarness="codex" allowCustom />);
    const custom = screen.getByLabelText("Custom model ID") as HTMLInputElement;
    expect(custom.placeholder).toBe("gpt-…");
  });

  it("uses a claude-… placeholder in the Claude custom input", () => {
    render(<Harness initial="claude-opus-4-8" pickerHarness="claude" />);
    const custom = screen.getByLabelText("Custom model ID") as HTMLInputElement;
    expect(custom.placeholder).toBe("claude-…");
  });
});
