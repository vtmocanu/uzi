// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { useState } from "react";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { AgentPicker, type OwnTemplate } from "./AgentPicker";
import type { AgentSelectionInput, RepoAgent } from "../lib/api";

afterEach(cleanup);

const REPO: RepoAgent[] = [
  { name: "coder", description: "Implements features." },
  { name: "reviewer", description: "Reviews changes." },
  { name: "tester", description: "Runs the tests." },
];
const OWN: OwnTemplate[] = [
  { name: "coder", description: "builtin coder", custom: false },
  { name: "reviewer", description: "builtin reviewer", custom: false },
  { name: "go-perf", description: "my custom perf agent", custom: true },
];

// Harness: renders the picker and surfaces the latest emitted selection + the
// live approve-button label (derived from it) for assertions.
function Harness({ repoAgents, ownTemplates }: { repoAgents: RepoAgent[]; ownTemplates: OwnTemplate[] }) {
  const [sel, setSel] = useState<AgentSelectionInput | null>(null);
  const activeCount =
    sel === null
      ? 0
      : (sel.source === "repo" ? repoAgents.length : ownTemplates.length) - sel.exclusions.length;
  return (
    <>
      <AgentPicker repoAgents={repoAgents} ownTemplates={ownTemplates} onChange={setSel} />
      <output data-testid="sel">{JSON.stringify(sel)}</output>
      <output data-testid="count">{activeCount}</output>
    </>
  );
}

const selection = (): AgentSelectionInput => JSON.parse(screen.getByTestId("sel").textContent || "null");
const chip = (name: string) => screen.getByRole("button", { name: new RegExp(`^●?\\s*${name}`, "i") });

describe("AgentPicker", () => {
  it("State A: repo detected → repo is the default source with no exclusions", () => {
    render(<Harness repoAgents={REPO} ownTemplates={OWN} />);
    expect(selection()).toEqual({ source: "repo", exclusions: [] });
    // Both cards' pills: repo shows detected + default.
    expect(screen.getByText(/detected · 3/)).toBeTruthy();
    expect(screen.getByText("default")).toBeTruthy();
  });

  it("State B: no repo agents → own is the default, the repo card is inert", () => {
    render(<Harness repoAgents={[]} ownTemplates={OWN} />);
    expect(selection()).toEqual({ source: "own", exclusions: [] });
    expect(screen.getByText("none detected")).toBeTruthy();
    // The repo radio is disabled (inert card).
    const repoRadio = screen.getByRole("radio", { name: /repo agents/i }) as HTMLInputElement;
    expect(repoRadio.disabled).toBe(true);
  });

  it("excluding a chip adds it to the selection and drops the live count", () => {
    render(<Harness repoAgents={REPO} ownTemplates={OWN} />);
    // Exclude tester from the repo roster.
    fireEvent.click(chip("tester"));
    expect(selection()).toEqual({ source: "repo", exclusions: ["tester"] });
    expect(screen.getByTestId("count").textContent).toBe("2");
    // Toggling it back removes the exclusion.
    fireEvent.click(chip("tester"));
    expect(selection()).toEqual({ source: "repo", exclusions: [] });
  });

  it("never lets the roster drop to zero — excluding the last agent is a no-op", () => {
    render(<Harness repoAgents={[{ name: "solo", description: "only one" }]} ownTemplates={OWN} />);
    expect(screen.getByTestId("count").textContent).toBe("1");
    fireEvent.click(chip("solo")); // would leave zero
    expect(selection()).toEqual({ source: "repo", exclusions: [] });
    expect(screen.getByTestId("count").textContent).toBe("1");
  });

  it("switching to the own source changes the emitted selection", () => {
    render(<Harness repoAgents={REPO} ownTemplates={OWN} />);
    fireEvent.click(screen.getByRole("radio", { name: /my agent templates/i }));
    expect(selection().source).toBe("own");
    // Exclusions are per-source: switching starts the own card with none.
    expect(selection().exclusions).toEqual([]);
  });

  it("clicking a chip on the non-selected card switches the source to it", () => {
    render(<Harness repoAgents={REPO} ownTemplates={OWN} />);
    expect(selection().source).toBe("repo");
    // Exclude the custom own template — this also flips the source to own.
    fireEvent.click(chip("go-perf"));
    expect(selection()).toEqual({ source: "own", exclusions: ["go-perf"] });
  });

  it("renders repo descriptions as INERT plain text — never a clickable link", () => {
    const evil: RepoAgent[] = [
      { name: "netcheck", description: "See http://evil.example/x — click here" },
      { name: "reviewer", description: "Reviews." },
    ];
    const { container } = render(<Harness repoAgents={evil} ownTemplates={OWN} />);
    // No anchor anywhere in the picker: the untrusted description is not linkified.
    expect(container.querySelector("a")).toBeNull();
    // The description rides only the chip's title attribute (a hover tooltip),
    // as plain text — never markdown, never an element with the URL as href.
    expect(chip("netcheck").title).toContain("http://evil.example/x");
    expect(container.querySelector('[href*="evil.example"]')).toBeNull();
  });

  it("shows a `custom` badge on user-scope templates", () => {
    render(<Harness repoAgents={[]} ownTemplates={OWN} />);
    const goPerf = chip("go-perf");
    expect(within(goPerf).getByText("custom")).toBeTruthy();
  });

  // #166: a builtin/global template description crosses a principal boundary into
  // another user's tooltip via the chip `title` ATTRIBUTE. The existing test at
  // "renders repo descriptions as INERT plain text" covers the repo sink (:123);
  // this covers the own-template sink (:148). Both channels were invisible to the
  // #124 suite, which asserts over container.textContent (attribute values
  // excluded), so this asserts the attribute directly.
  it("carries the own-template description into the chip title ATTRIBUTE, not textContent", () => {
    const DESC = "template tooltip — attribute channel, cross-principal";
    const own: OwnTemplate[] = [{ name: "go-perf", description: DESC, custom: true }];
    const { container } = render(<Harness repoAgents={[]} ownTemplates={own} />);
    expect(chip("go-perf").title).toBe(DESC);
    // Proves the assertion targets the attribute, not the visible text.
    expect(container.textContent).not.toContain(DESC);
  });
});
