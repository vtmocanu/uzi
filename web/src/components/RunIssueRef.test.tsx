// @vitest-environment jsdom
//
// RunIssueRef (PRD #411 M2): the three-way render of a run's originating issue ref —
// a muted kind chip when there is no issue, a forge external link when a valid https
// issue_web_url is snapshotted, and a plain `#<iid>` span when the issue is no longer
// cached. Plus the runKindLabel mapping and the in-card stopPropagation guard.
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
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
});

describe("RunIssueRef — stopPropagation in a card link", () => {
  // The anchor sits inside a wrapper whose onClick is a spy. An outer capture-phase
  // preventDefault suppresses jsdom's navigation attempt without touching bubbling, so
  // whether the spy fires reflects only the anchor's stopPropagation behaviour.
  function renderInCard(inCardLink: boolean) {
    const spy = vi.fn();
    render(
      <div onClickCapture={(e) => e.preventDefault()}>
        <div onClick={spy}>
          <RunIssueRef
            issueIid={26}
            issueWebUrl={URL26}
            kind="issue"
            forgeType="gitlab"
            inCardLink={inCardLink}
          />
        </div>
      </div>,
    );
    return spy;
  }

  it("positive: with inCardLink, a click on the anchor does NOT bubble to the wrapper", () => {
    const spy = renderInCard(true);
    fireEvent.click(screen.getByRole("link"));
    expect(spy).not.toHaveBeenCalled();
  });

  it("paired negative: without inCardLink, the click bubbles to the wrapper", () => {
    const spy = renderInCard(false);
    fireEvent.click(screen.getByRole("link"));
    expect(spy).toHaveBeenCalledTimes(1);
  });
});
