// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { OnboardingChecklist } from "./OnboardingChecklist";

afterEach(cleanup);

function mount(over: Partial<Parameters<typeof OnboardingChecklist>[0]> = {}) {
  return render(
    <MemoryRouter>
      <OnboardingChecklist hasToken={false} hasForge={false} reposEnabled={0} workerJoined={false} {...over} />
    </MemoryRouter>,
  );
}

const classesOf = (text: string) => screen.getByText(text).className.split(/\s+/).filter(Boolean);

describe("OnboardingChecklist", () => {
  it("keeps no decoration-* class on a done step, so the strike falls back to currentColor (PRD #60)", () => {
    mount({ workerJoined: true });
    const classes = classesOf("Bring a worker online");
    expect(classes).toContain("line-through");
    // Deliberately wider than the defect: a decoration COLOR made the strike nearly invisible
    // (a border token at ~1.9:1). With none set, the line inherits the muted text color.
    // jsdom cannot observe the computed color, so the class list is the proxy.
    expect(classes.filter((c) => c.startsWith("decoration-"))).toEqual([]);
    // Sanity: an open step is not struck at all.
    expect(classesOf("Add your Anthropic token")).not.toContain("line-through");
  });

  it("strikes step 4 and counts it when workerJoined is true", () => {
    const { container } = mount({ workerJoined: true });
    expect(classesOf("Bring a worker online")).toContain("line-through");
    expect(container.textContent).toContain("1/4 done");
  });

  it("leaves step 4 open, with its hint and link, for a fleet that never registered", () => {
    const { container } = mount({ workerJoined: false });
    expect(classesOf("Bring a worker online")).not.toContain("line-through");
    expect(container.textContent).toContain("0/4 done");
    expect(screen.getByRole("link", { name: /Workers/ }).getAttribute("href")).toBe("/workers");
  });

  it("counts every finished step and drops the hints of the done ones", () => {
    const { container } = mount({ hasToken: true, hasForge: true, reposEnabled: 2, workerJoined: true });
    expect(container.textContent).toContain("4/4 done");
    expect(screen.queryByText("Enable a repo")?.className).toContain("line-through");
    expect(screen.queryByRole("link")).toBeNull();
  });
});
