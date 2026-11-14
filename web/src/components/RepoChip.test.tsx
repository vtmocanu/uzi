// @vitest-environment jsdom
// Issue #4334: the Repos chip and its popover, drawn from a hand-built RepoActivityModel
// (plus one test that runs the real model to prove an unsafe fallback path never renders raw).
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RepoChip } from "./RepoChip";
import { repoActivityModel, type ActiveRepo, type RepoActivityModel } from "../lib/overviewRepos";
import type { RunSummaryItem } from "../lib/api";

afterEach(cleanup);

const hasUnsafe = (s: string) => /evil/.test(s) || [...s].some((c) => c.charCodeAt(0) < 32 || c === "\u202E");

function active(over: Partial<ActiveRepo> & { path: string }): ActiveRepo {
  return {
    key: over.path,
    badge: over.path.split("/").pop()!.slice(0, 2),
    parts: { running: 1, queued: 0, waiting: 0, owner: 0 },
    pill: "1 running",
    tone: "ok",
    ...over,
  };
}

function model(over: Partial<RepoActivityModel> = {}): RepoActivityModel {
  const repos = [
    active({ path: "org/alpha" }),
    active({ path: "org/bravo", pill: "1 queued", tone: "neutral" }),
    active({ path: "org/charlie", pill: "waits on them", tone: "warn" }),
  ];
  return {
    active: { state: "ok", repos, atLeast: false, count: 3 },
    total: 9,
    recent: [{ key: "r-new", path: "org/delta", badge: "de", ago: "3h ago" }],
    idle: { count: 3, minDays: 9, maxDays: 30, never: 1 },
    listFailed: false,
    ...over,
  };
}

function renderChip(m: RepoActivityModel) {
  return render(
    <MemoryRouter>
      <RepoChip model={m} />
    </MemoryRouter>,
  );
}

const chip = () => screen.getByRole("button", { name: /^Repos:/ });
const open = () => fireEvent.click(chip());
const dialog = () => screen.getByRole("dialog", { name: "Repos in uzi" });
const okActive = (over: { atLeast?: boolean; count?: number } = {}): RepoActivityModel["active"] => ({
  state: "ok",
  repos: [active({ path: "org/alpha" }), active({ path: "org/bravo" })],
  atLeast: false,
  count: 2,
  ...over,
});

describe("RepoChip trigger", () => {
  it("reads N repos active against the total", () => {
    renderChip(model());
    expect(chip().textContent).toBe("al" + "br" + "+1" + "3 repos active of 9");
    expect(chip().getAttribute("aria-label")).toBe("Repos: 3 active now of 9");
    expect(chip().getAttribute("aria-haspopup")).toBe("dialog");
    expect(chip().getAttribute("aria-expanded")).toBe("false");
  });

  it("uses the plural word for one repo", () => {
    renderChip(model({ active: okActive({ count: 1 }), total: 4 }));
    expect(chip().getAttribute("aria-label")).toBe("Repos: 1 active now of 4");
    expect(chip().textContent).toContain("1 repos active of 4");
  });

  it("says at least with a total, in the chip, the name and the header", () => {
    renderChip(model({ active: okActive({ atLeast: true, count: 7 }), total: 12 }));
    expect(chip().textContent).toContain("at least 7 repos active of 12");
    expect(chip().getAttribute("aria-label")).toBe("Repos: at least 7 active now of 12");
    open();
    expect(within(dialog()).getByRole("heading", { name: "At least 7 of 12 repos active now" })).toBeTruthy();
  });

  it("drops the total when it is unknown", () => {
    renderChip(model({ active: okActive({ count: 2 }), total: null }));
    expect(chip().textContent).toContain("2 repos active");
    expect(chip().textContent).not.toContain(" of ");
    expect(chip().getAttribute("aria-label")).toBe("Repos: 2 active now");
    open();
    expect(within(dialog()).getByRole("heading", { name: "2 repos active now" })).toBeTruthy();
  });

  it("says at least with no total exactly", () => {
    renderChip(model({ active: okActive({ atLeast: true, count: 5 }), total: null }));
    expect(chip().textContent).toContain("at least 5 repos active");
    expect(chip().textContent).not.toContain(" of ");
    expect(chip().getAttribute("aria-label")).toBe("Repos: at least 5 active now");
    open();
    expect(within(dialog()).getByRole("heading", { name: "At least 5 repos active now" })).toBeTruthy();
  });

  it("says unavailable, and renders nothing while loading", () => {
    const { rerender, container } = renderChip(model({ active: { state: "unavailable" } }));
    expect(chip().textContent).toBe("Repos active unavailable");
    expect(chip().getAttribute("aria-label")).toBe("Repos: active now unavailable");
    open();
    expect(within(dialog()).getByRole("heading", { name: "Repos active unavailable" })).toBeTruthy();
    expect(within(dialog()).getByText("The admin run list could not be read.")).toBeTruthy();
    rerender(
      <MemoryRouter>
        <RepoChip model={model({ active: { state: "loading" } })} />
      </MemoryRouter>,
    );
    expect(container.firstChild).toBeNull();
  });
});

describe("RepoChip popover", () => {
  it("lists active repos with a tone pill, in model order", () => {
    renderChip(model());
    open();
    const d = within(dialog());
    expect(d.getByRole("heading", { name: "3 of 9 repos active now" })).toBeTruthy();
    const rows = d.getAllByRole("listitem").map((li) => li.textContent);
    expect(rows.slice(0, 3)).toEqual(["alorg/alpha1 running", "brorg/bravo1 queued", "chorg/charlie" + "waits on them"]);
    expect(d.getByText("waits on them").className).toContain("text-warn");
    expect(d.getAllByText("1 running")[0].className).toContain("text-ok");
    expect(d.getByText("1 queued").className).not.toContain("text-ok");
    expect(d.getByText("org/alpha").getAttribute("title")).toBe("org/alpha");
  });

  it("draws the group labels in order: Active now, run within 7 days, Idle", () => {
    renderChip(model());
    open();
    const labels = Array.from(dialog().querySelectorAll("p.uppercase")).map((p) => p.textContent);
    expect(labels).toEqual(["Active now", "Run within 7 days, nothing active", "Idle"]);
    const d = within(dialog());
    expect(d.getByText("3h ago")).toBeTruthy();
    expect(d.getByText("3 last ran 9 to 30 days ago, 1 never ran")).toBeTruthy();
  });

  it("says no repo has listed work", () => {
    renderChip(model({ active: { state: "ok", repos: [], atLeast: false, count: 0 }, recent: [], idle: null }));
    open();
    expect(within(dialog()).getByText("No repo has listed active work.")).toBeTruthy();
    expect(within(dialog()).getByRole("heading", { name: "0 of 9 repos active now" })).toBeTruthy();
    expect(within(dialog()).queryByText("Idle")).toBeNull();
  });

  it("shows only Active now while the repo read is loading", () => {
    renderChip(model({ total: null, recent: null, idle: null, listFailed: false }));
    open();
    const d = within(dialog());
    expect(d.getByText("Active now")).toBeTruthy();
    expect(d.queryByText(/Run within 7 days/)).toBeNull();
    expect(d.queryByText("Idle")).toBeNull();
    expect(d.queryByText("The repo list could not be read.")).toBeNull();
  });

  it("says the repo list could not be read, and hides the recent and idle groups, when the read failed", () => {
    renderChip(model({ total: null, recent: null, idle: null, listFailed: true }));
    open();
    const d = within(dialog());
    expect(d.getByText("The repo list could not be read.")).toBeTruthy();
    expect(d.getByText("Active now")).toBeTruthy();
    expect(d.queryByText(/Run within 7 days/)).toBeNull();
    expect(d.queryByText("Idle")).toBeNull();
  });

  it("states what the count covers in the header tooltip", () => {
    renderChip(model());
    open();
    const tip =
      "Active counts repos with a listed non-terminal run: chat, judge and cross-check runs are not listed, and the list stops at 500 runs. Last run includes every run kind.";
    expect(within(dialog()).getByText(tip)).toBeTruthy();
    expect(dialog().querySelector(`[title="${tip}"]`)).not.toBeNull();
  });

  it("links the footer to the repos page", () => {
    renderChip(model());
    open();
    const link = within(dialog()).getByRole("link", { name: "Your repos" });
    expect(link.getAttribute("href")).toBe("/repos");
  });

  it("renders a disabled-but-active repo's unsafe path only from the sanitized, masked fallback", () => {
    const evil = "evil\u202Eorg/\u001bx\u0007repo";
    const m = repoActivityModel({
      ownRuns: [],
      adminRuns: {
        runs: [{ id: "r-1", status: "running", health: "ok", repo_id: "r-evil", repo_path: evil } as RunSummaryItem],
        failed: false,
        lastOkAt: 0,
      },
      viewerEmail: "me@x.io",
      repoActivity: { repos: [], failed: false },
      now: Date.parse("2026-11-08T12:00:00Z"),
      demo: true,
    });
    renderChip(m);
    open();
    const row = within(dialog()).getAllByRole("listitem")[0];
    const pathEl = within(row).getByText("demo/xrepo");
    expect(pathEl.getAttribute("title")).toBe("demo/xrepo");
    expect(row.textContent).toBe("xrdemo/xrepo1 running");
    expect(chip().textContent).toContain("xr");
    expect(hasUnsafe(dialog().innerHTML)).toBe(false);
    expect(hasUnsafe(chip().outerHTML)).toBe(false);
  });
});

describe("RepoChip keyboard and focus", () => {
  it("opens with focus inside the dialog, and Escape returns focus to the chip", () => {
    renderChip(model());
    chip().focus();
    fireEvent.click(chip());
    expect(chip().getAttribute("aria-expanded")).toBe("true");
    expect(chip().getAttribute("aria-controls")).toBe(dialog().id);
    expect(dialog().contains(document.activeElement)).toBe(true);
    fireEvent.keyDown(document, { key: "Escape" });
    expect(chip().getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(chip());
  });

  it("closes on a mousedown outside, but not inside", () => {
    render(
      <MemoryRouter>
        <RepoChip model={model()} />
        <p>elsewhere</p>
      </MemoryRouter>,
    );
    open();
    fireEvent.mouseDown(dialog());
    expect(screen.queryByRole("dialog")).not.toBeNull();
    fireEvent.mouseDown(screen.getByText("elsewhere"));
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});
