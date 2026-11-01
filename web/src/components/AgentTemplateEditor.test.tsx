// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AgentTemplateEditor } from "./AgentTemplateEditor";

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
});
