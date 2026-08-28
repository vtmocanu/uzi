// @vitest-environment jsdom
//
// RunIssueRef (PRD #411 M2): the three-way render of a run's originating issue ref —
// a muted kind chip when there is no issue, a forge external link when a valid https
// issue_web_url is snapshotted, and a plain `#<iid>` span when the issue is no longer
// cached. Plus the runKindLabel mapping.
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { RunIssueRef, runKindLabel } from "./RunIssueRef";

afterEach(cleanup);

const URL26 = "https://gitlab.example.com/vtmocanu/uzi/-/issues/26";

describe("RunIssueRef — forge link (branch 2)", () => {
  it("positive: renders an external anchor to the forge issue with #26 in its accessible name", () => {
    render(
      <RunIssueRef issueIid={26} issueWebUrl={URL26} kind="issue" forgeType="gitlab" />,
    );
    const link = screen.getByRole("link");
    expect(link.getAttribute("href")).toBe(URL26);
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toBe("noreferrer");
    // The accessible name comes from aria-label: "Open issue #26 on GitLab".
    expect(link.getAttribute("aria-label")).toContain("#26");
    expect(link.getAttribute("aria-label")).toBe("Open issue #26 on GitLab");
    // The visible number is #26 too.
    expect(link.textContent).toContain("#26");
  });

  it("paired negative on the SAME #26 wording: no anchor when the url is absent, #26 is plain text (branch 3)", () => {
    const { container } = render(
      <RunIssueRef issueIid={26} issueWebUrl={null} kind="issue" forgeType="gitlab" />,
    );
    // No link role AND no anchor element at all (a hrefless <a> has no link role,
    // so the element-level check is the one that binds branch 3 vs branch 2).
    expect(screen.queryByRole("link")).toBeNull();
    expect(container.querySelector("a")).toBeNull();
    // ...but the SAME #26 wording is present as plain text.
    expect(screen.getByText("#26")).toBeTruthy();
  });
});

describe("RunIssueRef — kind chip (branch 1, guards the dangling-# regression)", () => {
  const cases: Array<[string, string]> = [
    ["task", "task"],
    ["ci_fix", "ci fix"],
    ["prompt", "prompt"],
  ];
  for (const [kind, label] of cases) {
    it(`kind=${kind} renders the label "${label}" as a chip, with no anchor and no literal #`, () => {
      const { container } = render(
        <RunIssueRef issueIid={null} issueWebUrl={null} kind={kind} forgeType="gitlab" />,
      );
      // Positive: the chip label text is present.
      expect(screen.getByText(label)).toBeTruthy();
      // No link in the chip branch.
      expect(screen.queryByRole("link")).toBeNull();
      // Paired negative on the same rendered surface: no dangling "#" leaks into the chip.
      expect(container.textContent).not.toContain("#");
    });
  }
});

describe("runKindLabel", () => {
  it("maps ci_fix to a spaced label and passes other kinds through with underscores spaced", () => {
    expect(runKindLabel("ci_fix")).toBe("ci fix");
    expect(runKindLabel("task")).toBe("task");
    expect(runKindLabel("self_improve")).toBe("self improve");
  });

  it("maps mr_rework to the legible 'MR rework' label (PRD #700)", () => {
    expect(runKindLabel("mr_rework")).toBe("MR rework");
  });
});

describe("RunIssueRef — raised scopes z-10 to the interactive anchor only (#485 NB1)", () => {
  // In a stretched-link card the forge anchor must sit ABOVE the overlay (relative
  // z-10) so it stays clickable, but the non-interactive branches must stay BELOW it
  // so a click on their footprint still navigates to the run. `raised` must therefore
  // lift branch 2 only, never branch 1 (kind chip) or branch 3 (cached-out #iid span).
  it("branch 2 (https anchor): raised adds relative z-10 to the anchor", () => {
    render(<RunIssueRef issueIid={26} issueWebUrl={URL26} kind="issue" forgeType="gitlab" raised />);
    const link = screen.getByRole("link");
    expect(link.className).toContain("relative");
    expect(link.className).toContain("z-10");
  });

  it("branch 1 (kind chip): raised does NOT lift the non-interactive chip", () => {
    const { container } = render(
      <RunIssueRef issueIid={null} issueWebUrl={null} kind="task" forgeType="gitlab" raised />,
    );
    const chip = container.firstElementChild as HTMLElement;
    expect(chip.tagName).toBe("SPAN");
    expect(chip.className).not.toContain("z-10");
  });

  it("branch 3 (cached-out #iid span): raised does NOT lift the plain span", () => {
    const { container } = render(
      <RunIssueRef issueIid={26} issueWebUrl={null} kind="issue" forgeType="gitlab" raised />,
    );
    // No anchor in branch 3 (hrefless), so the rendered element is the plain span.
    expect(container.querySelector("a")).toBeNull();
    const span = container.firstElementChild as HTMLElement;
    expect(span.tagName).toBe("SPAN");
    expect(span.className).not.toContain("z-10");
  });
});

describe('RunIssueRef — tone="inherit" lets the ref read the parent colour (#485 NB2)', () => {
  // The Board needs-attention strip themes the whole pill `text-warn`; a ref that forces
  // its own text-faint/text-muted mismatches the amber. tone="inherit" drops those
  // explicit colours so the number reads warn, and (branch 2) swaps hover:text-brand —
  // which would break the warn theme — for a warn-safe hover:underline. Each check pairs
  // an inherit assertion with a default-tone positive control on the same surface.
  it("branch 2 (https anchor): inherit drops text-faint and hover:text-brand", () => {
    render(
      <RunIssueRef issueIid={26} issueWebUrl={URL26} kind="issue" forgeType="gitlab" tone="inherit" />,
    );
    const link = screen.getByRole("link");
    expect(link.className).not.toContain("text-faint");
    expect(link.className).not.toContain("hover:text-brand");
    // The warn-safe hover affordance is still present.
    expect(link.className).toContain("hover:underline");
  });

  it("branch 2 (https anchor): default tone DOES force text-faint and hover:text-brand", () => {
    render(<RunIssueRef issueIid={26} issueWebUrl={URL26} kind="issue" forgeType="gitlab" />);
    const link = screen.getByRole("link");
    expect(link.className).toContain("text-faint");
    expect(link.className).toContain("hover:text-brand");
  });

  it("branch 2 (https anchor): inherit still composes raised as relative z-10", () => {
    render(
      <RunIssueRef
        issueIid={26}
        issueWebUrl={URL26}
        kind="issue"
        forgeType="gitlab"
        raised
        tone="inherit"
      />,
    );
    const link = screen.getByRole("link");
    expect(link.className).toContain("relative");
    expect(link.className).toContain("z-10");
    expect(link.className).not.toContain("text-faint");
  });

  it("branch 1 (kind chip): inherit drops text-muted", () => {
    const { container } = render(
      <RunIssueRef issueIid={null} issueWebUrl={null} kind="task" forgeType="gitlab" tone="inherit" />,
    );
    const chip = container.firstElementChild as HTMLElement;
    expect(chip.tagName).toBe("SPAN");
    expect(chip.className).not.toContain("text-muted");
  });

  it("branch 1 (kind chip): default tone DOES force text-muted", () => {
    const { container } = render(
      <RunIssueRef issueIid={null} issueWebUrl={null} kind="task" forgeType="gitlab" />,
    );
    const chip = container.firstElementChild as HTMLElement;
    expect(chip.className).toContain("text-muted");
  });
});
