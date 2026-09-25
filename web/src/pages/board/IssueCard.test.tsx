// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { IssueCard } from "./IssueCard";
import type { Card } from "../../lib/api";

afterEach(cleanup);

const FAILED_PIPELINE: NonNullable<Card["pipeline"]> = {
  status: "failed",
  web_url: "https://gitlab.example.com/o/r/-/pipelines/1",
  ref: "agent/issue-7",
  pipeline_id: 1,
  synced_at: new Date().toISOString(),
};

function aCard(over: Partial<Card> = {}): Card {
  return {
    iid: 7,
    title: "Fix the flaky build",
    state: "opened",
    labels: ["uzi"],
    web_url: "https://gitlab.example.com/o/r/-/issues/7",
    forge_type: "gitlab",
    author: "mira",
    has_prd_link: false,
    column: "Review",
    closed: false,
    conflict: false,
    forge_updated_at: new Date().toISOString(),
    latest_run: null,
    pipeline: null,
    ...over,
  };
}

function renderCard(over: Partial<Card> = {}) {
  return render(
    <MemoryRouter>
      <IssueCard
        card={aCard(over)}
        repoId="repo-1"
        chips={[]}
        laneLabel="Review"
        canMoveUp={false}
        canMoveDown={false}
        onMoveUp={vi.fn()}
        onMoveDown={vi.fn()}
        insertionEdge={null}
        gate={{ enabled: true, reason: "" }}
        starting={false}
        onStart={vi.fn()}
        fixCiBusy={false}
        onFixCi={vi.fn()}
        uziLabel="uzi"
        isEligible
        canPromote={false}
        promoting={false}
        onPromote={vi.fn()}
        onDragStart={vi.fn()}
        onDragEnd={vi.fn()}
        dimmed={false}
      />
    </MemoryRouter>,
  );
}

// PRD #1650 D3a: once automatic CI fixing halts on the card's branch, Fix CI is the
// user's to press, and the card says so with an "Autofix stopped" marker.
describe("IssueCard — CI auto-fix halted marker (#1650 D3a)", () => {
  it("shows the marker beside the pipeline badge, with the attempt count in its name and title", () => {
    renderCard({ pipeline: FAILED_PIPELINE, ci_autofix_halted: true, ci_autofix_attempts: 3 });
    const expected = "CI auto-fix stopped after 3 attempts. Fix CI is yours to press.";
    const marker = screen.getByRole("img", { name: expected });
    expect(marker.textContent).toBe("Autofix stopped");
    expect(marker.getAttribute("title")).toBe(expected);
    // The pipeline badge and the Fix CI button still render next to it.
    expect(screen.getByRole("link", { name: /CI failed/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Fix CI" })).toBeTruthy();
  });

  it("uses the singular for one attempt", () => {
    renderCard({ pipeline: FAILED_PIPELINE, ci_autofix_halted: true, ci_autofix_attempts: 1 });
    expect(
      screen.getByRole("img", { name: "CI auto-fix stopped after 1 attempt. Fix CI is yours to press." }),
    ).toBeTruthy();
  });

  it("shows the marker where the badge would be when the card has no cached pipeline", () => {
    renderCard({ pipeline: null, ci_autofix_halted: true, ci_autofix_attempts: 2 });
    const marker = screen.getByRole("img", {
      name: "CI auto-fix stopped after 2 attempts. Fix CI is yours to press.",
    });
    expect(marker.textContent).toBe("Autofix stopped");
    expect(screen.queryByRole("link", { name: /^CI / })).toBeNull();
  });

  it("is absent when ci_autofix_halted is false", () => {
    renderCard({ pipeline: FAILED_PIPELINE, ci_autofix_halted: false, ci_autofix_attempts: 1 });
    // Positive anchor: the card and its pipeline row did render.
    expect(screen.getByText("#7")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Fix CI" })).toBeTruthy();
    expect(screen.queryByText("Autofix stopped")).toBeNull();
    expect(screen.queryByRole("img", { name: /auto-fix stopped/ })).toBeNull();
  });

  it("is absent when the halt fields are missing (an older API replica)", () => {
    renderCard({ pipeline: FAILED_PIPELINE });
    expect(screen.getByText("#7")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Fix CI" })).toBeTruthy();
    expect(screen.queryByText("Autofix stopped")).toBeNull();
    expect(screen.queryByRole("img", { name: /auto-fix stopped/ })).toBeNull();
  });
});
