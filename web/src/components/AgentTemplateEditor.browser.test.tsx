import "../index.css";
import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AgentTemplateEditor } from "./AgentTemplateEditor";
import type { AgentTemplateInput } from "../lib/api";

const originalTheme = document.documentElement.getAttribute("data-theme");

afterEach(() => {
  cleanup();
  if (originalTheme === null) document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", originalTheme);
});

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

  const help = screen.getByText(/^An effort chosen at start, then your Run defaults effort/);
  expect(help.getBoundingClientRect().height).toBeGreaterThan(0);

  const claude = screen.getByLabelText("Claude effort") as HTMLSelectElement;
  const codex = screen.getByLabelText("Codex effort") as HTMLSelectElement;
  expect(claude.value).toBe("");
  expect(codex.value).toBe("");
  expect(claude.options[0]?.text).toBe("Default · built-in (medium)");

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

it.each(["dawn", "ember"])("shows the four family-labelled pin fields and both lead help paragraphs in %s", (theme) => {
  document.documentElement.setAttribute("data-theme", theme);
  render(
    <MemoryRouter>
      <AgentTemplateEditor
        initial={{ name: "lead", description: "d", model: null, tools: null, prompt_body: "b" }}
        nameEditable={false}
        submitLabel="Save"
        busy={false}
        error=""
        onSubmit={vi.fn()}
      />
    </MemoryRouter>,
  );
  for (const label of ["Claude model", "Codex model", "Claude effort", "Codex effort"]) {
    const rect = screen.getByLabelText(label).getBoundingClientRect();
    expect(rect.width, label).toBeGreaterThan(0);
    expect(rect.height, label).toBeGreaterThan(0);
  }
  for (const re of [/^A model chosen at start or a schedule's model/, /^An effort chosen at start, then/]) {
    expect(screen.getByText(re).getBoundingClientRect().height).toBeGreaterThan(0);
  }
});
