// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { OverviewRunRow } from "./OverviewRunRow";
import { aRun } from "../pages/overviewFixtures";

afterEach(cleanup);

function row(props: { owner?: string; demo: boolean }) {
  render(
    <MemoryRouter>
      <ul>
        <OverviewRunRow run={aRun({ issue_title: "Some title" })} now={Date.parse("2026-07-05T12:05:00Z")} {...props} />
      </ul>
    </MemoryRouter>,
  );
}

describe("OverviewRunRow owner", () => {
  it("shows no owner on the viewer's own rows", () => {
    row({ demo: false });
    expect(screen.queryByText(/@/)).toBeNull();
  });

  it("shows another user's email with the same value in its title", () => {
    row({ owner: "mira.lopez@uzi.local", demo: false });
    const owner = screen.getByText("mira.lopez@uzi.local");
    expect(owner.getAttribute("title")).toBe("mira.lopez@uzi.local");
  });

  it("masks the owner email in the text and in the title attribute in demo mode", () => {
    row({ owner: "mira.lopez@uzi.local", demo: true });
    expect(document.body.textContent).not.toContain("mira.lopez");
    expect(document.body.textContent).not.toContain("@uzi.local");
    const titled = [...document.querySelectorAll("[title]")].map((el) => el.getAttribute("title") ?? "");
    expect(titled.some((t) => t.includes("mira.lopez") || t.includes("uzi.local"))).toBe(false);
    // The masked owner is still rendered, and its title carries the same masked text.
    const masked = document.querySelector("span[title].truncate");
    expect(masked?.textContent).toBeTruthy();
    expect(masked?.getAttribute("title")).toBe(masked?.textContent);
  });
});
