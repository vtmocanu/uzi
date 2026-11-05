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

const WORKER_MODEL_HELP = "Each harness's Run defaults model takes precedence over its lead template pin.";
const MODEL_HELP = "Leave a harness on Default so this subagent uses whatever the lead runs on. A schedule with “Apply model also to agents” on overrides these pins.";

describe("AgentTemplateEditor model Default label (#3255)", () => {
  it("names the SDK/account default for the lead and explains the user override", () => {
    const select = renderEditor("lead");
    expect(select.options[0]?.text).toBe("Default · SDK/account default (built-in)");
    const codex = screen.getByLabelText("Codex model") as HTMLSelectElement;
    expect(codex.options[0]?.text).toBe("Default · built-in (gpt-6.1-sol)");
    expect(screen.getByText(MODEL_HELP).id).toBe(select.getAttribute("aria-describedby"));
    expect(codex.getAttribute("aria-describedby")).toBe(select.getAttribute("aria-describedby"));
    expect(screen.getByText(WORKER_MODEL_HELP)).toBeTruthy();
  });

  it("names the lead's model for a subagent and omits the lead-only help", () => {
    const select = renderEditor("coder");
    expect(select.options[0]?.text).toBe("Default · lead's model");
    expect(screen.queryByText(WORKER_MODEL_HELP)).toBeNull();
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
    expect(screen.getByText("Default · lead's model", { selector: "span.text-ok" })).toBeTruthy();
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
    expect(screen.getByText("Default · lead's model", { selector: "span.text-ok" })).toBeTruthy();
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
      "inherit", "gpt-6-astra", "gpt-5.6-sol", "gpt-6-sol", "gpt-6.1-sol", "gpt-6-luna",
    ]);
    expect(screen.getByLabelText("Custom model ID")).toBeTruthy();
    fireEvent.submit(container.querySelector("form")!);
    expect(onSubmit).toHaveBeenCalledWith(expect.objectContaining({ model: "claude-custom", codex_model: "gpt-6-sol" }));
    const preview = container.querySelector("pre")!.textContent!;
    expect(preview).toContain("model: claude-custom");
    expect(preview).not.toContain("gpt-6-sol");
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
