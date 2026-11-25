// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { createRef } from "react";
import { MemoryRouter } from "react-router-dom";
import { AgentTemplateEditor, type AgentTemplateEditorHandle } from "./AgentTemplateEditor";

afterEach(cleanup);

function renderEditor(name: string) {
  render(
    <MemoryRouter>
      <AgentTemplateEditor
        initial={{ name, description: "d", model: null, tools: null, prompt_body: "b" }}
        nameEditable={false}
        submitLabel="Save"
        busy={false}
        error=""
        onSubmit={vi.fn()}
      />
    </MemoryRouter>,
  );
  return screen.getByLabelText("Claude model") as HTMLSelectElement;
}

const LEAD_MODEL_HELP = "A model chosen at start or a schedule's model, then your worker model for that harness in Settings → Models, take precedence over this pin; with none of these and no pin, Claude uses the SDK/account default and Codex uses gpt-6.2-sol.";
const LEAD_EFFORT_HELP = "An effort chosen at start, then your worker effort for that harness in Settings → Models (a level or Runtime default), take precedence over this pin; with none of these and no pin, the lead runs at built-in medium.";
const MODEL_HELP = "Leave a harness on Default so this subagent uses whatever the lead runs on. A schedule with “Use the run's resolved model for subagents too” on overrides these model pins; effort pins are kept.";

describe("AgentTemplateEditor model Default label (#3255)", () => {
  it("names the SDK/account default for the lead and explains the precedence", () => {
    const select = renderEditor("lead");
    expect(select.options[0]?.text).toBe("Default · SDK/account default (built-in)");
    const codex = screen.getByLabelText("Codex model") as HTMLSelectElement;
    expect(codex.options[0]?.text).toBe("Default · built-in (gpt-6.2-sol)");
    expect((screen.getByLabelText("Claude effort") as HTMLSelectElement).options[0]?.text).toBe("Default · built-in (medium)");
    expect((screen.getByLabelText("Codex effort") as HTMLSelectElement).options[0]?.text).toBe("Default · built-in (medium)");
    const modelHelp = screen.getByText(LEAD_MODEL_HELP);
    const effortHelp = screen.getByText(LEAD_EFFORT_HELP);
    expect(modelHelp.id).toBe("template-model-help");
    expect(select.getAttribute("aria-describedby")).toBe(modelHelp.id);
    expect(codex.getAttribute("aria-describedby")).toBe(modelHelp.id);
    for (const label of ["Claude effort", "Codex effort"]) {
      expect(screen.getByLabelText(label).getAttribute("aria-describedby")).toBe(effortHelp.id);
    }
    expect(screen.queryByText(MODEL_HELP)).toBeNull();
  });

  it("says Default · follows lead for a subagent and omits the lead-only help", () => {
    const select = renderEditor("coder");
    expect(select.options[0]?.text).toBe("Default · follows lead");
    expect((screen.getByLabelText("Claude effort") as HTMLSelectElement).options[0]?.text).toBe("Default · follows the run's effort");
    expect(screen.queryByText(LEAD_MODEL_HELP)).toBeNull();
    expect(screen.queryByText(LEAD_EFFORT_HELP)).toBeNull();
    const help = screen.getByText(MODEL_HELP);
    for (const label of ["Claude model", "Codex model"]) {
      expect(screen.getByLabelText(label).getAttribute("aria-describedby")).toBe(help.id);
    }
    expect(document.getElementById(help.id)).toBe(help);
  });

  it("shows the Default labels in the builtin diff for all four columns, including a stored empty Claude model", () => {
    render(
      <MemoryRouter>
        <AgentTemplateEditor
          initial={{ name: "lead", description: "d", model: "", codex_model: "gpt-6-luna", effort: "high", codex_effort: "low", tools: null, prompt_body: "b" }}
          builtin={{ name: "lead", description: "d", model: "", codex_model: null, effort: null, codex_effort: null, tools: null, prompt_body: "b" }}
          nameEditable={false}
          submitLabel="Save"
          busy={false}
          error=""
          onSubmit={vi.fn()}
        />
      </MemoryRouter>,
    );
    // shipped side of every row is the Default label; the unchanged Claude model is not listed.
    expect(screen.getAllByText("Default · built-in (medium)", { selector: "span.text-ok" })).toHaveLength(2);
    expect(screen.getByText("Default · built-in (gpt-6.2-sol)", { selector: "span.text-ok" })).toBeTruthy();
    expect(screen.queryByText(/^Default$/, { selector: "span" })).toBeNull();
  });

  it("shows the Claude model Default label for a stored empty model against a pinned shipped one", () => {
    render(
      <MemoryRouter>
        <AgentTemplateEditor
          initial={{ name: "coder", description: "d", model: "", tools: null, prompt_body: "b" }}
          builtin={{ name: "coder", description: "d", model: "sonnet", codex_model: null, tools: null, prompt_body: "b" }}
          nameEditable={false}
          submitLabel="Save"
          busy={false}
          error=""
          onSubmit={vi.fn()}
        />
      </MemoryRouter>,
    );
    expect(screen.getByText("sonnet", { selector: "span.text-ok" })).toBeTruthy();
    expect(screen.getByText("Default · follows lead", { selector: "span.text-danger" })).toBeTruthy();
  });

  it("the shipped-vs-current model diff names the Default label for a null shipped model", () => {
    render(
      <MemoryRouter>
        <AgentTemplateEditor
          initial={{ name: "coder", description: "d", model: "sonnet", tools: null, prompt_body: "b" }}
          builtin={{ name: "coder", description: "d", model: null, codex_model: null, tools: null, prompt_body: "b" }}
          nameEditable={false}
          submitLabel="Save"
          busy={false}
          error=""
          onSubmit={vi.fn()}
        />
      </MemoryRouter>,
    );
    expect(screen.getByText("Default · follows lead", { selector: "span.text-ok" })).toBeTruthy();
    expect(screen.queryByText(/^inherit$/i)).toBeNull();
  });
});


describe("AgentTemplateEditor Codex contract plumbing", () => {
  it("edits both pins in live comparison, displays the diff and submits both", () => {
    const ref = createRef<AgentTemplateEditorHandle>();
    const onSubmit = vi.fn();
    render(
      <MemoryRouter>
        <AgentTemplateEditor
          ref={ref}
          initial={{ name: "coder", description: "d", model: null, codex_model: "gpt-6-sol", tools: null, prompt_body: "b" }}
          builtin={{ name: "coder", description: "d", model: null, codex_model: null, tools: null, prompt_body: "b" }}
          nameEditable={false}
          submitLabel="Save"
          busy={false}
          error=""
          onSubmit={onSubmit}
        />
      </MemoryRouter>,
    );
    expect(ref.current?.currentContent().codex_model).toBe("gpt-6-sol");
    expect(screen.getByLabelText("Codex model")).toBeTruthy();
    expect(screen.getByText("Default · follows lead", { selector: "span.text-ok" })).toBeTruthy();
    expect(screen.getByText("gpt-6-sol", { selector: "span.text-danger" })).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Claude model"), { target: { value: "sonnet" } });
    expect(ref.current?.currentContent()).toMatchObject({ model: "sonnet", codex_model: "gpt-6-sol" });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(onSubmit).toHaveBeenCalledOnce();
    expect(onSubmit.mock.calls[0][0]).toMatchObject({ model: "sonnet", codex_model: "gpt-6-sol" });
    fireEvent.change(screen.getByLabelText("Codex model"), { target: { value: "gpt-6-luna" } });
    expect(ref.current?.currentContent().codex_model).toBe("gpt-6-luna");
    expect(screen.getByText("gpt-6-luna", { selector: "span.text-danger" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(onSubmit.mock.calls[1][0].codex_model).toBe("gpt-6-luna");
    fireEvent.change(screen.getByLabelText("Codex model"), { target: { value: "inherit" } });
    fireEvent.change(screen.getByLabelText("Claude model"), { target: { value: "inherit" } });
    expect(ref.current?.currentContent()).toMatchObject({ model: null, codex_model: null });
    expect(screen.getByText("Matches the shipped definition.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(onSubmit.mock.calls[2][0]).toMatchObject({ model: null, codex_model: null });
  });
});


describe("AgentTemplateEditor pin validation", () => {
  it.each([
    { model: "gpt-6-sol", codex_model: null, warning: "Claude template model must not be a Codex role model ID." },
    { model: null, codex_model: "sonnet", warning: "Codex template model must be a supported Codex role model ID." },
    { model: "é".repeat(51), codex_model: null, warning: "Model is too long (max 100 bytes); use a shorter alias or model ID." },
    { model: "two words", codex_model: null, warning: "Model must be a single token with no spaces." },
    { model: "x".repeat(101), codex_model: null, warning: "Model is too long (max 100 characters); use a shorter alias or model ID." },
    { model: "bad\u0007id", codex_model: null, warning: "Model contains a newline, control, or format character; remove it before saving." },
  ])("blocks button and programmatic submission for $warning", ({ model, codex_model, warning }) => {
    const onSubmit = vi.fn();
    const { container } = render(<AgentTemplateEditor
      initial={{ name: "coder", description: "d", model, codex_model, tools: null, prompt_body: "b" }}
      nameEditable={false} submitLabel="Save" busy={false} error="" onSubmit={onSubmit}
    />);
    expect(screen.getAllByText(warning)).toHaveLength(1);
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.submit(container.querySelector("form")!);
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it("keeps the Codex vocabulary closed and trims custom Claude IDs without exporting the Codex pin", () => {
    const onSubmit = vi.fn();
    const { container } = render(<AgentTemplateEditor
      initial={{ name: "coder", description: "d", model: " claude-custom ", codex_model: " gpt-6-sol ", tools: null, prompt_body: "b" }}
      nameEditable={false} submitLabel="Save" busy={false} error="" onSubmit={onSubmit}
    />);
    const codex = screen.getByLabelText("Codex model") as HTMLSelectElement;
    expect(Array.from(codex.options, (option) => option.value)).toEqual([
      "inherit", "gpt-6-astra", "gpt-5.6-sol", "gpt-6-sol", "gpt-6.1-sol", "gpt-6.2-sol", "gpt-6.2-astra", "gpt-6-luna",
    ]);
    expect(screen.getByLabelText("Custom model ID")).toBeTruthy();
    fireEvent.submit(container.querySelector("form")!);
    expect(onSubmit).toHaveBeenCalledWith(expect.objectContaining({ model: "claude-custom", codex_model: "gpt-6-sol" }));
    const preview = container.querySelector("pre")!.textContent!;
    expect(preview).toContain("model: claude-custom");
    expect(preview).not.toContain("gpt-6-sol");
  });

  it("a Codex-only edit submits the unchanged Claude pins, and the reverse", () => {
    const onSubmit = vi.fn();
    render(<AgentTemplateEditor
      initial={{ name: "coder", description: "d", model: "claude-custom-v1", codex_model: "gpt-6-sol", effort: "high", codex_effort: "low", tools: null, prompt_body: "b" }}
      nameEditable={false} submitLabel="Save" busy={false} error="" onSubmit={onSubmit}
    />);
    fireEvent.change(screen.getByLabelText("Codex model"), { target: { value: "gpt-6-luna" } });
    fireEvent.change(screen.getByLabelText("Codex effort"), { target: { value: "max" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(onSubmit.mock.calls[0][0]).toMatchObject({ model: "claude-custom-v1", effort: "high", codex_model: "gpt-6-luna", codex_effort: "max" });
    fireEvent.change(screen.getByLabelText("Claude effort"), { target: { value: "low" } });
    fireEvent.change(screen.getByLabelText("Codex model"), { target: { value: "gpt-6-sol" } });
    fireEvent.change(screen.getByLabelText("Codex effort"), { target: { value: "low" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(onSubmit.mock.calls[1][0]).toMatchObject({ model: "claude-custom-v1", effort: "low", codex_model: "gpt-6-sol", codex_effort: "low" });
  });

  it("live preview carries the Claude effort as it changes and never the Codex effort (#3258)", () => {
    const { container } = render(<AgentTemplateEditor
      initial={{ name: "coder", description: "d", model: null, effort: "high", codex_effort: "low", tools: null, prompt_body: "b" }}
      nameEditable={false} submitLabel="Save" busy={false} error="" onSubmit={vi.fn()}
    />);
    const preview = () => container.querySelector("pre")!.textContent!;
    expect(preview()).toContain("effort: high");
    expect(preview()).not.toContain("low");
    fireEvent.change(screen.getByLabelText("Claude effort"), { target: { value: "max" } });
    expect(preview()).toContain("effort: max");
    fireEvent.change(screen.getByLabelText("Claude effort"), { target: { value: "" } });
    expect(preview()).not.toContain("effort");
  });

  it.each([
    { description: "bad\nvalue", tools: null },
    { description: "d", tools: ["bad,tool"] },
  ])("preserves frontmatter guards on programmatic submit", ({ description, tools }) => {
    const onSubmit = vi.fn();
    const { container } = render(<AgentTemplateEditor
      initial={{ name: "coder", description, model: null, tools, prompt_body: "b" }}
      nameEditable={false} submitLabel="Save" busy={false} error="" onSubmit={onSubmit}
    />);
    expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.submit(container.querySelector("form")!);
    expect(onSubmit).not.toHaveBeenCalled();
  });
});
