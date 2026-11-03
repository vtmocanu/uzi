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
  return screen.getByLabelText("Model") as HTMLSelectElement;
}

const WORKER_MODEL_HELP = "A user's worker model (Settings → Run defaults) overrides this for their own runs.";

describe("AgentTemplateEditor model Default label (#3255)", () => {
  it("names the SDK/account default for the lead and explains the user override", () => {
    const select = renderEditor("lead");
    expect(select.options[0]?.text).toBe("Default · SDK/account default (built-in)");
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
  it("retains the stored pin in live comparison, displays the diff and omits it on submit", () => {
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
    expect(screen.getByText("Codex model")).toBeTruthy();
    expect(screen.getByText("Default", { selector: "span.text-ok" })).toBeTruthy();
    expect(screen.getByText("gpt-6-sol", { selector: "span.text-danger" })).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Model"), { target: { value: "sonnet" } });
    expect(ref.current?.currentContent()).toMatchObject({ model: "sonnet", codex_model: "gpt-6-sol" });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(onSubmit).toHaveBeenCalledOnce();
    expect(onSubmit.mock.calls[0][0]).not.toHaveProperty("codex_model");
  });
});
