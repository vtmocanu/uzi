// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RunRow } from "./RunsList";
import { mockJobRuns, mockRuns, runListItem } from "../mocks/data";

// PRD #1908 M7: a repo-less job run in the runs list. The list API sends repo_path "" and
// forge_type "" for it, with no issue_iid; the row must read as a job (kind chip + title),
// never as an empty repo path or a "#<iid>" link. The rows come from the mock fixtures via
// the same runListItem the mock API uses, so the fixture and the row are tested together.

const NOW = Date.now();

function renderRow(id: string, from = mockJobRuns) {
  const run = runListItem(from.find((r) => r.id === id)!);
  return { run, ...render(
    <MemoryRouter>
      <RunRow run={run} now={NOW} />
    </MemoryRouter>,
  ) };
}

afterEach(cleanup);

describe("RunsList job row (PRD #1908)", () => {
  it("shows the job title and a job kind chip, with no repo path and no issue link", () => {
    const { run, container } = renderRow("run-job-queued");
    expect(run.repo_path).toBe("");
    expect(screen.getByText("Summarise the incident timeline from the attached notes")).toBeTruthy();
    const chip = screen.getByText("job");
    // The chip leads the meta line: nothing (no empty repo span) sits before its wrapper.
    const wrapper = chip.parentElement!;
    expect(wrapper.previousElementSibling).toBeNull();
    // The row's only links are the stretched run link; no forge/issue anchor.
    const links = [...container.querySelectorAll("a")];
    expect(links.map((a) => a.getAttribute("href"))).toEqual(["/runs/run-job-queued"]);
    expect(links[0].getAttribute("aria-label")).toBe(
      "Open run: Summarise the incident timeline from the attached notes",
    );
  });

  it("control: an issue run still leads its meta line with the repo path", () => {
    const { run } = renderRow("run-queued", mockRuns);
    expect(run.repo_path).not.toBe("");
    const path = screen.getByText(run.repo_path);
    expect(path.nextElementSibling).not.toBeNull();
  });
});
