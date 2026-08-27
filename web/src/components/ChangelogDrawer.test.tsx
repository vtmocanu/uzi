// @vitest-environment jsdom
//
// ChangelogDrawer (PRD #415 M2 shell + M3 content): the left slide-in drawer built
// on the shared Modal shell. These pin the four Modal a11y behaviours
// (Escape/backdrop/✕ close, focus restore) and an independently scrolling body, plus
// the M3 rendering — release-tag heading links, category dots, PRD-link transform,
// and the current/newer markers + banner derived from `runningVersion`. `releases`
// is mocked to a fixed, deterministic set (including a `[NOT RELEASED]` and an
// `[Unreleased]`) so the assertions stay stable as the real CHANGELOG grows and the
// build-time import.meta.glob stays out of this test.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

// Spread the REAL module and override only `releases`, so the mock's
// `splitBulletTitle` (which ChangelogDrawer now imports) is the real
// implementation rather than a re-typed copy that could drift.
vi.mock("../lib/changelog", async (importActual) => ({
  ...(await importActual<typeof import("../lib/changelog")>()),
  releases: [
    {
      version: "Unreleased",
      date: null,
      body: "Working head.",
      groups: [{ category: "Added", bullets: ["Something in flight."] }],
      unreleased: true,
      notReleased: false,
    },
    {
      version: "0.50.0",
      date: "2026-09-01",
      titleMarker: "The big one",
      body: "not released body",
      groups: [{ category: "Changed", bullets: ["Reworked the thing."] }],
      unreleased: false,
      // A section numerically GREATER than the running/newest-released version but
      // marked [NOT RELEASED] — must never be "Newer" nor the banner's target.
      notReleased: true,
    },
    {
      version: "0.48.0",
      date: "2026-08-20",
      body: "Added a thing.",
      groups: [
        { category: "Added", bullets: ["A feature landed (PRD #123)."] },
        { category: "Fixed", bullets: ["A bug fixed [#413](https://github.com/vtmocanu/uzi/pull/413)."] },
        { category: "Security", bullets: ["Hardened a path."] },
        { category: "Dependencies", bullets: ["Bumped a dep."] },
      ],
      unreleased: false,
      notReleased: false,
    },
    {
      version: "0.10.0",
      date: "2026-08-10",
      body: "Fixed a thing.",
      groups: [{ category: "Fixed", bullets: ["Ordering fix."] }],
      unreleased: false,
      notReleased: false,
    },
    {
      version: "0.9.0",
      date: "2026-08-01",
      body: "Old release.",
      groups: [{ category: "Added", bullets: ["An old feature."] }],
      unreleased: false,
      notReleased: false,
    },
    // A title-bearing bullet for the split-render assertions. Uses a LOW version
    // (older than everything) so it carries no marker and never becomes the
    // banner's "available" target — the existing marker/banner math (which
    // expects v0.48.0 as newest eligible) stays intact.
    {
      version: "0.8.0",
      date: "2026-08-21",
      body: "x",
      groups: [
        {
          category: "Added",
          bullets: ["**Bold headline (PRD #534 follow-up).** The description text follows here."],
        },
      ],
      unreleased: false,
      notReleased: false,
    },
  ],
}));

import { ChangelogDrawer } from "./ChangelogDrawer";

afterEach(cleanup);

describe("ChangelogDrawer — M2 shell behaviours", () => {
  it("renders a version heading for each release — v-prefixed numbers, the raw token for Unreleased", () => {
    render(<ChangelogDrawer onClose={vi.fn()} />);
    expect(screen.getByText("v0.48.0")).toBeTruthy();
    expect(screen.getByText("v0.10.0")).toBeTruthy();
    expect(screen.getByText("Unreleased")).toBeTruthy();
  });

  it("closes on Escape", () => {
    const onClose = vi.fn();
    render(<ChangelogDrawer onClose={onClose} />);
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("closes on a backdrop click", () => {
    const onClose = vi.fn();
    render(<ChangelogDrawer onClose={onClose} />);
    fireEvent.mouseDown(screen.getByRole("dialog"));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("closes when the ✕ button is activated", () => {
    const onClose = vi.fn();
    render(<ChangelogDrawer onClose={onClose} />);
    fireEvent.click(screen.getByRole("button", { name: "Close changelog" }));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("moves focus into the dialog on open and restores it to the trigger on close", () => {
    const trigger = document.createElement("button");
    document.body.appendChild(trigger);
    trigger.focus();
    expect(document.activeElement).toBe(trigger);

    const { unmount } = render(<ChangelogDrawer onClose={vi.fn()} />);
    expect(document.activeElement).toBe(screen.getByRole("dialog"));

    unmount();
    expect(document.activeElement).toBe(trigger);
    trigger.remove();
  });

  it("gives the body its own scroll container so a long changelog scrolls independently", () => {
    render(<ChangelogDrawer onClose={vi.fn()} />);
    const body = screen.getByTestId("changelog-body");
    expect(body.className).toContain("overflow-y-auto");
    expect(body.textContent).toContain("v0.48.0");
  });

  it("carries the running version on a data attribute", () => {
    render(<ChangelogDrawer runningVersion="0.48.0" onClose={vi.fn()} />);
    expect(
      screen.getByRole("dialog").querySelector('[data-running-version="0.48.0"]'),
    ).not.toBeNull();
  });
});

describe("ChangelogDrawer — M3 markers + banner", () => {
  it("marks the running release 'You're running this' and strictly-greater releases 'Newer'", () => {
    render(<ChangelogDrawer runningVersion="0.10.0" onClose={vi.fn()} />);

    // The heading for 0.10.0 is the running one.
    const runningHeading = screen.getByText("v0.10.0").closest("li")!;
    expect(runningHeading.textContent).toContain("You're running this");

    // 0.48.0 is strictly greater and released → "Newer".
    const newerHeading = screen.getByText("v0.48.0").closest("li")!;
    expect(newerHeading.textContent).toContain("Newer");
    expect(newerHeading.textContent).not.toContain("You're running this");

    // 0.9.0 is older → no marker.
    const olderHeading = screen.getByText("v0.9.0").closest("li")!;
    expect(olderHeading.textContent).not.toContain("Newer");
    expect(olderHeading.textContent).not.toContain("You're running this");
  });

  it("shows a banner naming the newest AVAILABLE released version", () => {
    render(<ChangelogDrawer runningVersion="0.10.0" onClose={vi.fn()} />);
    const banner = screen.getByTestId("changelog-banner");
    // Newest eligible released version is 0.48.0 — NOT the numerically-greater
    // 0.50.0, which is [NOT RELEASED].
    expect(banner.textContent).toBe("This instance runs v0.10.0 · v0.48.0 available");
  });

  it("uses numeric semver ordering for markers (0.9.0 running < 0.10.0)", () => {
    render(<ChangelogDrawer runningVersion="0.9.0" onClose={vi.fn()} />);
    // Lexically "0.9.0" > "0.10.0"; numerically it is smaller, so 0.10.0 must read
    // "Newer", not older.
    const tenHeading = screen.getByText("v0.10.0").closest("li")!;
    expect(tenHeading.textContent).toContain("Newer");
    const runningHeading = screen.getByText("v0.9.0").closest("li")!;
    expect(runningHeading.textContent).toContain("You're running this");
  });

  it("never marks a [NOT RELEASED] section 'Newer' nor makes it the banner target", () => {
    render(<ChangelogDrawer runningVersion="0.10.0" onClose={vi.fn()} />);
    // 0.50.0 is numerically greater than everything but is [NOT RELEASED].
    const notReleased = screen.getByText("v0.50.0").closest("li")!;
    expect(notReleased.textContent).not.toContain("Newer");
    expect(notReleased.textContent).not.toContain("You're running this");
    // And it is not the banner's "available" target (0.48.0 is).
    expect(screen.getByTestId("changelog-banner").textContent).not.toContain("v0.50.0");
    // Positive pair: a genuinely newer RELEASED version IS marked.
    expect(screen.getByText("v0.48.0").closest("li")!.textContent).toContain("Newer");
  });

  it("renders NO markers and NO banner for a non-semver running version, while still rendering releases", () => {
    render(<ChangelogDrawer runningVersion="dev" onClose={vi.fn()} />);
    // Neutral: no banner, no markers.
    expect(screen.queryByTestId("changelog-banner")).toBeNull();
    expect(screen.queryByTestId("release-marker")).toBeNull();
    expect(screen.queryAllByText("Newer")).toHaveLength(0);
    // Positive assertion — the releases still render (no vacuous negative).
    expect(screen.getByText("v0.48.0")).toBeTruthy();
    expect(screen.getByText("v0.10.0")).toBeTruthy();
  });

  it("renders NO markers and NO banner when runningVersion is absent", () => {
    render(<ChangelogDrawer onClose={vi.fn()} />);
    expect(screen.queryByTestId("changelog-banner")).toBeNull();
    expect(screen.queryByTestId("release-marker")).toBeNull();
    expect(screen.getByText("v0.48.0")).toBeTruthy();
  });
});

describe("ChangelogDrawer — M3 rendering (links, dots)", () => {
  it("links a released semver heading to its release tag, but not [Unreleased]", () => {
    render(<ChangelogDrawer onClose={vi.fn()} />);
    const releasedLink = screen.getByText("v0.48.0").closest("a");
    expect(releasedLink).not.toBeNull();
    expect(releasedLink!.getAttribute("href")).toBe(
      "https://github.com/vtmocanu/uzi/releases/tag/v0.48.0",
    );
    expect(releasedLink!.getAttribute("target")).toBe("_blank");
    expect(releasedLink!.getAttribute("rel")).toBe("noopener noreferrer");

    // Unreleased is plain text — no tag link.
    expect(screen.getByText("Unreleased").closest("a")).toBeNull();
  });

  it("transforms a plain PRD ref into an /issues/ anchor and leaves a PR link a normal link", () => {
    render(<ChangelogDrawer onClose={vi.fn()} />);
    const prdLink = screen.getByRole("link", { name: "PRD #123" });
    expect(prdLink.getAttribute("href")).toBe("https://github.com/vtmocanu/uzi/issues/123");
    expect(prdLink.getAttribute("target")).toBe("_blank");

    const prLink = screen.getByRole("link", { name: "#413" });
    expect(prLink.getAttribute("href")).toBe("https://github.com/vtmocanu/uzi/pull/413");
  });

  it("renders a bullet's bold title on its own block, separate from its description", () => {
    render(<ChangelogDrawer onClose={vi.fn()} />);

    // The title's <strong> lives in a <p> that does NOT carry the description.
    const strong = Array.from(document.querySelectorAll("strong")).find((el) =>
      (el.textContent ?? "").includes("Bold headline"),
    );
    expect(strong).toBeTruthy();
    const titleP = strong!.closest("p")!;
    expect(titleP.textContent).not.toContain("The description text follows here");

    // The description renders in a DIFFERENT block.
    const descP = Array.from(document.querySelectorAll("p")).find((el) =>
      (el.textContent ?? "").includes("The description text follows here"),
    );
    expect(descP).toBeTruthy();
    expect(descP).not.toBe(titleP);

    // linkify applies to the title too: the `PRD #534` inside the bold span is an anchor.
    const prdLink = screen.getByRole("link", { name: "PRD #534" });
    expect(prdLink.getAttribute("href")).toBe("https://github.com/vtmocanu/uzi/issues/534");
  });

  it("maps each category to its status-tone dot class", () => {
    const { container } = render(<ChangelogDrawer onClose={vi.fn()} />);
    // Category label → its sibling dot's tone class.
    const dotFor = (label: string): string => {
      const heading = Array.from(container.querySelectorAll("span")).find(
        (el) => el.textContent === label && el.className.includes("uppercase"),
      );
      const dot = heading!.previousElementSibling as HTMLElement;
      return dot.className;
    };
    expect(dotFor("Added")).toContain("bg-ok");
    expect(dotFor("Changed")).toContain("bg-info");
    expect(dotFor("Fixed")).toContain("bg-warn");
    expect(dotFor("Security")).toContain("bg-danger");
    expect(dotFor("Dependencies")).toContain("bg-faint");
  });
});
