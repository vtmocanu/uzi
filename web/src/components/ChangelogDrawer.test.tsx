// @vitest-environment jsdom
//
// ChangelogDrawer (PRD #415 M2 + M3): the left slide-in release-notes panel built
// on the shared Modal shell. These pin the drawer's own contract — mount only
// while open, the three close paths (Esc, backdrop, the ✕ button) each reaching
// onClose, focus restored to the trigger on close, an independently-scrollable
// body, the empty-state fallback — plus the M3 rich rendering: linked headings,
// category dots, PRD linkify, the current/newer markers and banner, and the enter
// slide. The Modal's a11y mechanics themselves are covered by Modal.test.tsx.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { useState } from "react";
import { ChangelogDrawer } from "./ChangelogDrawer";
import type { Release, ReleaseGroup } from "../lib/changelog";

// `releases` is bundled from CHANGELOG.md at build time, so mock the module to
// drive both a populated and an empty list deterministically. A live getter lets
// each test swap the array before rendering.
const { releasesRef } = vi.hoisted(() => ({ releasesRef: { current: [] as Release[] } }));
vi.mock("../lib/changelog", () => ({
  get releases() {
    return releasesRef.current;
  },
}));

function makeRelease(
  version: string,
  date: string | null,
  opts: { released?: boolean; groups?: ReleaseGroup[]; titleMarker?: string } = {},
): Release {
  const r: Release = {
    version,
    date,
    body: "",
    groups: opts.groups ?? [],
    released: opts.released ?? true,
  };
  if (opts.titleMarker !== undefined) r.titleMarker = opts.titleMarker;
  return r;
}

const SAMPLE: Release[] = [
  makeRelease("0.48.0", "2026-08-19"),
  makeRelease("0.47.0", "2026-08-01"),
];

beforeEach(() => {
  releasesRef.current = SAMPLE;
});

afterEach(cleanup);

describe("ChangelogDrawer — shell + close paths", () => {
  it("renders nothing when closed and the dialog when open", () => {
    const { rerender } = render(<ChangelogDrawer open={false} onClose={vi.fn()} />);
    expect(screen.queryByRole("dialog")).toBeNull();

    rerender(<ChangelogDrawer open onClose={vi.fn()} />);
    expect(screen.getByRole("dialog", { name: "Release notes" })).toBeTruthy();
  });

  it("closes on Escape", () => {
    const onClose = vi.fn();
    render(<ChangelogDrawer open onClose={onClose} />);
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("closes on a backdrop click", () => {
    const onClose = vi.fn();
    render(<ChangelogDrawer open onClose={onClose} />);
    // A mousedown on the backdrop itself (target === currentTarget) closes.
    fireEvent.mouseDown(screen.getByRole("dialog"));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("closes on the ✕ button", () => {
    const onClose = vi.fn();
    render(<ChangelogDrawer open onClose={onClose} />);
    fireEvent.click(screen.getByRole("button", { name: "Close release notes" }));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("restores focus to the trigger after close (Modal restore)", () => {
    function Harness() {
      const [open, setOpen] = useState(false);
      return (
        <>
          <button type="button" onClick={() => setOpen(true)}>
            open changelog
          </button>
          <ChangelogDrawer open={open} onClose={() => setOpen(false)} />
        </>
      );
    }
    render(<Harness />);
    const trigger = screen.getByRole("button", { name: "open changelog" });
    trigger.focus();
    fireEvent.click(trigger);
    // Modal moves focus into the dialog on open.
    expect(document.activeElement).toBe(screen.getByRole("dialog"));

    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    // On close the drawer unmounts the Modal, which restores focus to the trigger.
    expect(document.activeElement).toBe(trigger);
  });

  it("gives the body its own scroll container and lists the releases", () => {
    render(<ChangelogDrawer open onClose={vi.fn()} />);
    const body = screen.getByTestId("changelog-body");
    // Independent body scroll, per the spec — a positive assertion on the class
    // that provides it.
    expect(body.className).toContain("overflow-y-auto");
    // The list is present rather than the empty-state paragraph.
    expect(within(body).getByRole("list")).toBeTruthy();
    expect(screen.queryByText("No release notes yet.")).toBeNull();
  });

  it("renders a graceful message when there are no releases", () => {
    releasesRef.current = [];
    render(<ChangelogDrawer open onClose={vi.fn()} />);
    expect(screen.getByText("No release notes yet.")).toBeTruthy();
    // Negative pair: no list is rendered in the empty state.
    expect(screen.queryByRole("list")).toBeNull();
  });
});

describe("ChangelogDrawer — the enter slide", () => {
  it("flips the panel from off-screen to in-place after mount", () => {
    render(<ChangelogDrawer open onClose={vi.fn()} />);
    const panel = screen.getByTestId("changelog-panel");
    // The on-mount effect flips `entered`, so after render (effects flushed) the
    // panel is translated in place, not off-screen left. Under prefers-reduced-
    // motion the motion-reduce class suppresses the transition; the end state is
    // the same in-place transform.
    expect(panel.getAttribute("data-entered")).toBe("true");
    expect(panel.className).toContain("translate-x-0");
    expect(panel.className).toContain("motion-reduce:transition-none");
  });
});

describe("ChangelogDrawer — rich rendering", () => {
  it("renders a version heading and date per release", () => {
    render(<ChangelogDrawer open onClose={vi.fn()} />);
    // displayVersion prefixes numeric versions with v.
    expect(screen.getByText("v0.48.0")).toBeTruthy();
    expect(screen.getByText("v0.47.0")).toBeTruthy();
    expect(screen.getByText("2026-08-19")).toBeTruthy();
    expect(screen.getByText("2026-08-01")).toBeTruthy();
  });

  it("links a real semver heading to its GitHub release tag", () => {
    const { container } = render(<ChangelogDrawer open onClose={vi.fn()} />);
    const link = container.querySelector<HTMLAnchorElement>('a[href$="/releases/tag/v0.48.0"]');
    expect(link).not.toBeNull();
    expect(link!.getAttribute("href")).toBe(
      "https://github.com/vtmocanu/uzi/releases/tag/v0.48.0",
    );
    expect(link!.getAttribute("target")).toBe("_blank");
    expect(link!.getAttribute("rel")).toBe("noopener noreferrer");
    expect(link!.textContent).toBe("v0.48.0");
  });

  it("leaves a non-semver [Unreleased] heading as plain text (no tag link)", () => {
    releasesRef.current = [makeRelease("Unreleased", null, { released: false })];
    const { container } = render(<ChangelogDrawer open onClose={vi.fn()} />);
    // The heading renders, but not as a link.
    expect(screen.getByText("Unreleased")).toBeTruthy();
    expect(container.querySelector('a[href*="/releases/tag/"]')).toBeNull();
  });

  it("leaves a staged [NOT RELEASED] semver heading as plain text (its tag does not exist yet)", () => {
    // 0.50.0 is a real semver version but released:false — its `v0.50.0` tag has
    // not been cut, so linking the heading would 404. It renders, but not linked.
    releasesRef.current = [makeRelease("0.50.0", "2026-09-01", { released: false })];
    const { container } = render(<ChangelogDrawer open onClose={vi.fn()} />);
    expect(screen.getByText("v0.50.0")).toBeTruthy();
    expect(container.querySelector('a[href*="/releases/tag/"]')).toBeNull();
  });

  it("renders a category dot in the mapped status tone", () => {
    releasesRef.current = [
      makeRelease("0.48.0", "2026-08-19", {
        groups: [{ category: "Added", bullets: ["did a thing"] }],
      }),
    ];
    render(<ChangelogDrawer open onClose={vi.fn()} />);
    const dot = screen.getByTestId("category-dot");
    // Added → ok, a real tailwind token (check-styles guards the stem exists).
    expect(dot.className).toContain("bg-ok");
    expect(screen.getByText("Added")).toBeTruthy();
  });

  it("linkifies a PRD #N reference to the repo issue, and keeps an existing PR link", () => {
    releasesRef.current = [
      makeRelease("0.48.0", "2026-08-19", {
        groups: [
          {
            category: "Fixed",
            bullets: ["Closes PRD #7 and keeps [#12](https://github.com/vtmocanu/uzi/pull/12)"],
          },
        ],
      }),
    ];
    const { container } = render(<ChangelogDrawer open onClose={vi.fn()} />);

    // PRD #7 became an anchor to the issue tracker…
    const prd = container.querySelector<HTMLAnchorElement>('a[href$="/issues/7"]');
    expect(prd).not.toBeNull();
    expect(prd!.getAttribute("href")).toBe("https://github.com/vtmocanu/uzi/issues/7");
    expect(prd!.getAttribute("target")).toBe("_blank");
    expect(prd!.getAttribute("rel")).toBe("noopener noreferrer");
    // …rendered quieter than a PR ref (faint tone).
    expect(prd!.className).toContain("text-faint");

    // …and the pre-existing PR link is untouched (not double-wrapped, not faint).
    const pr = container.querySelector<HTMLAnchorElement>('a[href$="/pull/12"]');
    expect(pr).not.toBeNull();
    expect(pr!.getAttribute("href")).toBe("https://github.com/vtmocanu/uzi/pull/12");
    expect(pr!.className).not.toContain("text-faint");
  });

  it("renders a title-marker subtitle when present", () => {
    releasesRef.current = [
      makeRelease("0.48.0", "2026-08-19", { titleMarker: "signed container images" }),
    ];
    render(<ChangelogDrawer open onClose={vi.fn()} />);
    expect(screen.getByText("signed container images")).toBeTruthy();
  });
});

describe("ChangelogDrawer — current / newer markers and banner", () => {
  // Running 0.48.0. 0.50.0 is [NOT RELEASED] (semver but released:false), so it is
  // never "current", never "Newer", and never the "available" target; 0.49.0 is
  // the greatest RELEASED version.
  const MARKERS: Release[] = [
    makeRelease("Unreleased", null, { released: false }),
    makeRelease("0.50.0", "2026-09-01", { released: false }),
    makeRelease("0.49.0", "2026-08-25"),
    makeRelease("0.48.0", "2026-08-19"),
    makeRelease("0.47.0", "2026-08-01"),
  ];

  it("flags the running release current and a strictly-greater released one Newer", () => {
    releasesRef.current = MARKERS;
    render(<ChangelogDrawer open onClose={vi.fn()} version="0.48.0" />);

    // Positive: exactly one "You're running this" (the 0.48.0 section)…
    expect(screen.getAllByText("You're running this")).toHaveLength(1);
    // …and exactly one "Newer" — 0.49.0. The [NOT RELEASED] 0.50.0 is greater
    // numerically but released:false, so it does NOT get a Newer marker.
    expect(screen.getAllByText("Newer")).toHaveLength(1);
  });

  it("names the greatest RELEASED version in the banner, not the unreleased one", () => {
    releasesRef.current = MARKERS;
    render(<ChangelogDrawer open onClose={vi.fn()} version="0.48.0" />);
    const banner = screen.getByTestId("changelog-banner");
    expect(banner.textContent).toContain("v0.48.0");
    expect(banner.textContent).toContain("v0.49.0");
    expect(banner.textContent).toContain("available");
    // The [NOT RELEASED] 0.50.0 is never the available target.
    expect(banner.textContent).not.toContain("v0.50.0");
  });

  it("drops the `available` clause when the running version is already the greatest", () => {
    releasesRef.current = MARKERS;
    render(<ChangelogDrawer open onClose={vi.fn()} version="0.49.0" />);
    const banner = screen.getByTestId("changelog-banner");
    expect(banner.textContent).toContain("v0.49.0");
    expect(banner.textContent).not.toContain("available");
    // Running 0.49.0 is the current release; nothing released is Newer than it.
    expect(screen.getAllByText("You're running this")).toHaveLength(1);
    expect(screen.queryByText("Newer")).toBeNull();
  });

  it("shows NO markers and NO banner for a non-semver running version (neutral)", () => {
    releasesRef.current = MARKERS;
    render(<ChangelogDrawer open onClose={vi.fn()} version="dev" />);
    expect(screen.queryByText("You're running this")).toBeNull();
    expect(screen.queryByText("Newer")).toBeNull();
    expect(screen.queryByTestId("changelog-banner")).toBeNull();
  });

  it("shows NO markers and NO banner when the running version is absent (neutral)", () => {
    releasesRef.current = MARKERS;
    render(<ChangelogDrawer open onClose={vi.fn()} />);
    expect(screen.queryByText("You're running this")).toBeNull();
    expect(screen.queryByText("Newer")).toBeNull();
    expect(screen.queryByTestId("changelog-banner")).toBeNull();
  });
});
