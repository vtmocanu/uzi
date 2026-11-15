// @vitest-environment jsdom
//
// #4373 against the REAL mockApi (the demo data the shipped mock build serves): every
// search-only group finds a fixture by name and tags it with its group's hint. The other
// menu tests use hand-built lists; this guards the integration with the data shapes the
// app actually produces (and fails loudly if a mock list is ever emptied).
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { CommandMenu } from "./CommandMenu";

vi.mock("../lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../lib/api")>()),
  MOCK_MODE: true,
  api: (await import("../mocks/mockApi")).mockApi,
}));

function search(q: string) {
  fireEvent.change(screen.getByRole("combobox"), { target: { value: q } });
}

// The options currently listed, as { label, text } where text includes the hint.
const options = () =>
  within(screen.getByRole("dialog", { name: "Command menu" }) as HTMLElement).queryAllByRole("option").map((o) => ({
    label: o.querySelector("span.truncate")?.textContent ?? "",
    text: o.textContent ?? "",
  }));

async function mount() {
  const { mockApi } = await import("../mocks/mockApi");
  await mockApi.login("vlad@uzi.local", "x");
  render(
    <MemoryRouter initialEntries={["/start"]}>
      <CommandMenu onClose={() => {}} isAdmin />
    </MemoryRouter>,
  );
}

beforeEach(async () => {
  await mount();
});
afterEach(() => cleanup());

async function expectOption(query: string, label: RegExp | string, hint: string) {
  await act(async () => void (await new Promise((r) => setTimeout(r, 0))));
  search(query);
  await waitFor(() => {
    const hit = options().find((o) => (typeof label === "string" ? o.label === label : label.test(o.label)));
    expect(hit, `${query}: ${JSON.stringify(options())}`).toBeDefined();
    expect(hit!.text).toContain(hint);
  });
}

describe("command menu groups against mockApi fixtures", () => {
  it("sections: Settings and Admin", async () => {
    await expectOption("appearance", "Settings › Appearance", "Settings");
    await expectOption("slack", "Admin › Settings › Slack", "Admin");
  });
  it("agents", async () => {
    await expectOption("spec-keeper", "spec-keeper", "Agent");
  });
  it("schedules", async () => {
    await expectOption("sweep", /^Sweep/, "Schedule");
  });
  it("workers", async () => {
    await expectOption("ci-runner-1", "ci-runner-1", "Worker");
  });
  it("skills", async () => {
    await expectOption("argocd-debugging", "argocd-debugging", "Skill");
  });
  it("recent runs", async () => {
    await expectOption("hotfix: board card", /Hotfix: board card CI badge/, "Recent runs");
  });
  it("docs", async () => {
    await expectOption("agent templates", /^Agent templates$|^Agent Templates$/i, "Doc");
  });
});
