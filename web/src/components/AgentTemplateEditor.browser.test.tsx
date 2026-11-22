import "../index.css";
import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AgentTemplateEditor } from "./AgentTemplateEditor";
import type { AgentTemplateInput } from "../lib/api";

afterEach(cleanup);

it("drives both effort selects on a lead template, submits the pins and shows the lead help", () => {
  const onSubmit = vi.fn<(input: AgentTemplateInput) => void>();
  render(
    <MemoryRouter>
      <AgentTemplateEditor
        initial={{ name: "lead", description: "d", model: null, tools: null, prompt_body: "b" }}
        nameEditable={false}
        submitLabel="Save"
        busy={false}
        error=""
        onSubmit={onSubmit}
      />
    </MemoryRouter>,
  );

  const help = screen.getByText("Used when you have no default effort for this harness");
  expect(help.getBoundingClientRect().height).toBeGreaterThan(0);

  const claude = screen.getByLabelText("Claude effort") as HTMLSelectElement;
  const codex = screen.getByLabelText("Codex effort") as HTMLSelectElement;
  expect(claude.value).toBe("");
  expect(codex.value).toBe("");
  expect(claude.options[0]?.text).toBe("Default · follows the run's effort");

  // Pick a level on each, then clear Codex back to Default.
  fireEvent.change(claude, { target: { value: "xhigh" } });
  fireEvent.change(codex, { target: { value: "low" } });
  expect(claude.value).toBe("xhigh");
  expect(codex.value).toBe("low");
  fireEvent.change(codex, { target: { value: "" } });
  expect(codex.value).toBe("");

  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  expect(onSubmit).toHaveBeenCalledTimes(1);
  expect(onSubmit.mock.calls[0]![0]).toMatchObject({ effort: "xhigh", codex_effort: null });
});
