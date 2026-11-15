// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AgentsChip } from "./AgentsChip";
import { agentSourceChipModel, type AgentSourceChipModel } from "../lib/agentSourceChip";
import { mockAgentSource } from "../mocks/data";

afterEach(cleanup);

const model: AgentSourceChipModel = {
  total: 16,
  groups: [
    { key: "source", label: "From your agent source", agents: [{ name: "reviewer", detail: null }] },
    { key: "shipped", label: "Shipped with uzi", agents: [{ name: "lead", detail: null }, { name: "tester", detail: "edited by an admin" }] },
    { key: "admin", label: "Added by an admin", agents: [{ name: "ui-copy", detail: null }] },
    { key: "yours", label: "Yours", agents: [{ name: "mine", detail: null }] },
  ],
  suffix: null,
  sourceMeta: null,
  sourceNotice: null,
  reviewInSettings: false,
};

function mount(m: AgentSourceChipModel = model) {
  return render(<MemoryRouter><AgentsChip model={m} /></MemoryRouter>);
}

const chip = () => screen.getByRole("button", { name: /^Agents:/ });

describe("AgentsChip", () => {
  it("shows the configured count with its matching accessible name", () => {
    mount();
    expect(chip().getAttribute("aria-label")).toBe("Agents: 16 configured");
    expect(chip().textContent).toBe("16 agents");
    expect(chip().getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("uses the singular and adds only an admin source suffix supplied by the model", () => {
    mount({ ...model, total: 1, suffix: { text: "3 changes to review", tone: "warn" }, reviewInSettings: true });
    expect(chip().textContent).toBe("1 agent · 3 changes to review");
    expect(chip().getAttribute("aria-label")).toBe("Agents: 1 configured, 3 changes to review");
    expect(chip().querySelector(".text-warn")).not.toBeNull();
  });

  it("groups agents by source and scope, with admin-edited builtins named", () => {
    mount();
    fireEvent.click(chip());
    const dialog = screen.getByRole("dialog", { name: "Agents configured" });
    expect(within(dialog).getByRole("heading", { name: "16 agents configured" })).toBeTruthy();
    for (const label of ["From your agent source", "Shipped with uzi", "Added by an admin", "Yours"]) {
      expect(within(dialog).getByText(label)).toBeTruthy();
    }
    expect(within(dialog).getByText("tester").parentElement?.textContent).toContain("edited by an admin");
    expect(within(dialog).getByRole("link", { name: "Manage agents" }).getAttribute("href")).toBe("/agents");
  });

  it("shows sanitized metadata in text and title and routes attention to Settings", () => {
    const meta = "https://forge.example.com@demo-ref, synced 2h ago";
    mount({ ...model, suffix: { text: "sync failed", tone: "danger" }, sourceMeta: meta, sourceNotice: "The last agent-source sync failed.", reviewInSettings: true });
    fireEvent.click(chip());
    const text = screen.getByText(meta);
    expect(text.getAttribute("title")).toBe(meta);
    expect(screen.getByText("The last agent-source sync failed.")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Review in Settings" }).getAttribute("href")).toBe("/admin/settings#agent-source");
  });

  it("strips unsafe URL/ref characters in rendered text and title, and masks both in demo mode", () => {
    const source = structuredClone(mockAgentSource);
    source.config.url = "https://github.com/private/agents\u202e\u0007";
    source.config.ref = "secret-ref\u202e\u0007";
    const build = (demo: boolean) => agentSourceChipModel([], { viewerId: "owner", isAdmin: true, source, demo, now: Date.now() });
    const { unmount } = mount(build(false));
    fireEvent.click(chip());
    const normal = screen.getByText(build(false).sourceMeta!);
    expect(normal.textContent).not.toContain("\u202e");
    expect(normal.textContent).not.toContain("\u0007");
    expect(normal.getAttribute("title")).not.toContain("\u202e");
    expect(normal.getAttribute("title")).not.toContain("\u0007");
    unmount();
    mount(build(true));
    fireEvent.click(chip());
    const demo = screen.getByText(build(true).sourceMeta!);
    expect(demo.textContent).toContain("https://forge.example.com@demo-ref");
    expect(demo.textContent).not.toContain("private");
    expect(demo.getAttribute("title")).toContain("https://forge.example.com@demo-ref");
  });

  it("keeps admin-only source details absent when the model has none", () => {
    mount();
    fireEvent.click(chip());
    expect(screen.queryByText(/forge\.example\.com|sync failed|review pending/)).toBeNull();
    expect(screen.queryByRole("link", { name: "Review in Settings" })).toBeNull();
  });

  it("closes on Escape and an outside click, restoring focus to the chip", () => {
    mount();
    fireEvent.click(chip());
    expect(document.activeElement).toBe(screen.getByRole("dialog"));
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(chip());
    fireEvent.click(chip());
    fireEvent.mouseDown(document.body);
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});
