// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RunRow } from "./RunsList";
import { mockRuns, runListItem } from "../mocks/data";

afterEach(cleanup);

// PRD 4173 M6: a run of a repo linked to an issue tracker carries issue_key, and the row's
// stretched link and issue reference name it instead of "#iid".
describe("RunsList row — issue key (PRD 4173 M6)", () => {
  const renderRow = (key?: string) => {
    const run = { ...runListItem(mockRuns.find((r) => r.id === "run-queued")!), issue_key: key };
    render(
      <MemoryRouter>
        <RunRow run={run} now={Date.now()} />
      </MemoryRouter>,
    );
    return run;
  };

  it("names the key in the run link and the issue reference", () => {
    const run = renderRow("ABC-7");
    expect(run.issue_iid).not.toBeNull();
    const overlay = screen.getByRole("link", { name: new RegExp(`^Open run for issue ABC-7`) });
    expect(overlay.getAttribute("href")).toBe(`/runs/${run.id}`);
    expect(screen.queryByRole("link", { name: new RegExp(`^Open run for issue #${run.issue_iid}`) })).toBeNull();
    expect(screen.getByText(/ABC-7/, { selector: "a[aria-label^='Open issue']" })).toBeTruthy();
  });

  it("keeps #iid without a key", () => {
    const run = renderRow(undefined);
    expect(screen.getByRole("link", { name: new RegExp(`^Open run for issue #${run.issue_iid}`) })).toBeTruthy();
  });
});
