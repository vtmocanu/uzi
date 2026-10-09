// @vitest-environment jsdom
// PRD #2602 M2: the run page progress card, one case per server-derived state, the
// blocked-by link, the phase marking, hostile milestone/activity text, and the absent cases
// that render nothing (null progress, `none`, an unknown state).
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RunProgressCard } from "./RunProgressCard";
import type { RunActivity, RunProgress } from "../lib/apiTypes";
import { formatLocalTime } from "../lib/budget";

const NOW = Date.parse("2026-10-09T12:00:00Z");
vi.mock("../lib/useNow", () => ({ useNow: () => NOW }));

afterEach(cleanup);

const STATUS_SINCE = "2026-10-09T06:50:00Z";
const HEALTH_SINCE = "2026-10-09T11:41:00Z";

const milestones = [
  { id: "api-completion-receipt", title: "api-completion-receipt" },
  { id: "worker-receipt-ordering", title: "worker-receipt-ordering" },
  { id: "recovery-docs-validation", title: "recovery-docs-validation" },
];

function progress(over: Partial<RunProgress>): RunProgress {
  return {
    state: "percent",
    pct: 70,
    milestone_done: 2,
    milestone_total: 3,
    active_milestone_id: "recovery-docs-validation",
    phase: "implement",
    ...over,
  };
}

type CardRun = Parameters<typeof RunProgressCard>[0]["run"];
function renderCard(over: Partial<CardRun>, activity: RunActivity | null = null) {
  const run: CardRun = {
    id: "r1",
    status: "running",
    status_since: null,
    health_since: null,
    milestones,
    milestones_completed: ["api-completion-receipt", "worker-receipt-ordering"],
    retry_not_before: null,
    recovery_retry_not_before: null,
    progress: progress({}),
    ...over,
  };
  return render(
    <MemoryRouter>
      <RunProgressCard run={run} activity={activity} />
    </MemoryRouter>,
  );
}

const card = (c: HTMLElement) => c.querySelector("[data-run-progress-card]") as HTMLElement | null;

function anActivity(over: Partial<RunActivity> = {}): RunActivity {
  return { agent: "lead", agent_label: "running a gate", tool: "Bash", detail: "", at: "2026-10-09T11:59:00Z", seq: 3, ...over };
}

describe("RunProgressCard (PRD #2602 M2)", () => {
  it("percent: ≈70%, milestone k of M with the active title, the segmented track and the active role", () => {
    const { container } = renderCard({}, anActivity());
    expect(card(container)?.getAttribute("data-run-progress-card")).toBe("percent");
    // ≈ is visual only; assistive tech hears "about 70%".
    const pct = container.querySelector("[data-progress-pct]") as HTMLElement;
    expect(pct.textContent).toBe("≈about 70%");
    expect(within(pct).getByText("≈").getAttribute("aria-hidden")).toBe("true");
    expect(within(pct).getByText("about").className).toContain("sr-only");
    expect(screen.getByText("milestone 3 of 3 · recovery-docs-validation")).toBeTruthy();
    const track = screen.getByRole("img");
    expect(track.getAttribute("aria-label")).toBe(
      "Plan progress: planning done, 2 of 3 milestones done, milestone 3 (recovery-docs-validation) in progress",
    );
    const segs = [...track.querySelectorAll("[data-seg]")].map((s) => [s.getAttribute("data-seg"), s.textContent]);
    expect(segs).toEqual([
      ["done", "plan"],
      ["done", "api-completion-receipt"],
      ["done", "worker-receipt-ordering"],
      ["now", "recovery-docs-validation"],
    ]);
    expect(screen.getByText("Active role")).toBeTruthy();
    expect(screen.getByText("lead").closest("dd")?.textContent).toBe("lead · running a gate");
  });

  it("names `N of M milestones done` when no milestone is active, with no `now` segment", () => {
    const { container } = renderCard({
      milestones_completed: ["api-completion-receipt"],
      progress: progress({ pct: 40, milestone_done: 1, active_milestone_id: "" }),
    });
    expect(screen.getByText("1 of 3 milestones done")).toBeTruthy();
    expect(container.querySelector('[data-seg="now"]')).toBeNull();
    expect(container.querySelectorAll('[data-seg="pending"]').length).toBe(2);
  });

  it.each(["review", "validate", "implement"] as const)("marks the current phase %s with ▸ and only that one", (phase) => {
    renderCard({ progress: progress({ phase }) });
    const cur = document.querySelector('[aria-current="step"]') as HTMLElement;
    expect(cur.textContent).toBe(`▸ ${phase}`);
    expect(document.querySelectorAll('[aria-current="step"]').length).toBe(1);
    expect(screen.getByText("current phase only, from the active role")).toBeTruthy();
  });

  it("hides the phase row when the phase is empty or unknown (PRD D6)", () => {
    // "deploy" stands for a phase a newer server might add: no row rather than three unmarked steps.
    for (const phase of ["", "deploy"] as RunProgress["phase"][]) {
      const { container } = renderCard({ progress: progress({ phase }) });
      expect(container.querySelector("[data-progress-phase]")).toBeNull();
      expect(container.textContent).not.toContain("▸");
      expect(screen.queryByText("implement")).toBeNull();
      expect(container.querySelector("[data-seg]")).not.toBeNull(); // the track still draws
      cleanup();
    }
  });

  // D6: phase is the current role's phase only. A waiting/parked/queued/stalled run keeps the
  // last phase in progress.phase, but no role is working it, so no phase is drawn (the TUI
  // draws phase only in the percent state too).
  it.each([
    ["stalled", "running"],
    ["waiting", "awaiting_input"],
    ["parked", "paused"],
    ["parked", "limit_wait"],
    ["queued", "queued"],
  ] as const)("no phase row in the %s state (%s), even with progress.phase set", (state, status) => {
    const { container } = renderCard({ status, progress: progress({ state, pct: null, phase: "implement" }) });
    expect(container.querySelector("[data-progress-phase]")).toBeNull();
    expect(container.querySelector('[aria-current="step"]')).toBeNull();
  });

  // The active-role line belongs to a working run: percent and stalled show it, an idle run
  // (waiting, parked, queued, planning) does not, even when a cached activity is passed in.
  it.each([
    ["percent", "running", true],
    ["stalled", "running", true],
    ["waiting", "awaiting_approval", false],
    ["parked", "paused", false],
    ["parked", "limit_wait", false],
    ["queued", "queued", false],
    ["planning", "running", false],
  ] as const)("active role line in the %s state (%s): %s", (state, status, shown) => {
    renderCard(
      { status, progress: progress({ state, pct: state === "percent" ? 70 : null }) },
      anActivity(),
    );
    expect(screen.queryByText("Active role") !== null).toBe(shown);
    expect(screen.queryByText("lead") !== null).toBe(shown);
  });

  // jsdom does no layout, so the overflow fix is locked in by structure: a long unbroken title
  // must sit in a column that may shrink below its min-content (minmax(0,1fr)) and wrap
  // anywhere, and every track column must be shrinkable with a truncating label. Measured in a
  // real browser at 1280/390px: no horizontal scroll with a 120-char unbroken title.
  it("a long unbroken milestone title cannot widen the card", () => {
    const long = "x".repeat(120);
    const { container } = renderCard({
      milestones: [...milestones.slice(0, 2), { id: "recovery-docs-validation", title: long }],
    });
    expect(card(container)?.className).toContain("grid-cols-[minmax(0,1fr)]");
    const what = container.querySelector("[data-progress-what]") as HTMLElement;
    expect(what.textContent).toBe(`milestone 3 of 3 · ${long}`);
    expect(what.className).toContain("[overflow-wrap:anywhere]");
    expect(what.className).toContain("min-w-0");
    const track = screen.getByRole("img") as HTMLElement;
    expect(track.style.gridTemplateColumns).toBe("minmax(0, 0.6fr) repeat(3, minmax(0, 1fr))");
    const label = track.querySelector('[data-seg="now"] > div:last-child') as HTMLElement;
    expect(label.textContent).toBe(long);
    expect(label.className).toContain("truncate");
    expect((label.parentElement as HTMLElement).className).toContain("min-w-0");
  });

  it("stalled: `◼ stalled · since HH:MM` from health_since, health reason in the title, no percent", () => {
    const { container } = renderCard({
      health: "stalled",
      health_reason: "no tool call for 25m",
      health_since: HEALTH_SINCE,
      progress: progress({ state: "stalled", pct: null }),
    });
    expect(card(container)?.getAttribute("data-run-progress-card")).toBe("stalled");
    const flag = screen.getByText(`stalled · since ${formatLocalTime(HEALTH_SINCE)}`).parentElement as HTMLElement;
    expect(flag.textContent).toBe(`◼stalled · since ${formatLocalTime(HEALTH_SINCE)}`);
    expect(flag.getAttribute("title")).toContain("no tool call for 25m");
    expect(flag.className).toContain("text-danger");
    expect(container.textContent).not.toMatch(/≈\d/);
  });

  it.each([
    ["awaiting_approval", "plan gate"],
    ["awaiting_input", "question"],
    ["awaiting_followup", "follow-up"],
  ] as const)("waiting (%s): `● waits on you · %s since HH:MM` from status_since", (status, reason) => {
    renderCard({ status, status_since: STATUS_SINCE, progress: progress({ state: "waiting", pct: null }) });
    const text = `waits on you · ${reason} since ${formatLocalTime(STATUS_SINCE)}`;
    const flag = screen.getByText(text).parentElement as HTMLElement;
    expect(flag.textContent).toBe(`●${text}`);
    expect(flag.className).toContain("text-warn");
  });

  it("parked on a usage limit: `⏸ limit wait · resumes HH:MM` when retry_not_before is ahead", () => {
    const at = "2026-10-09T21:00:00Z";
    renderCard({ status: "limit_wait", retry_not_before: at, progress: progress({ state: "parked", pct: null }) });
    expect(screen.getByText(`limit wait · resumes ${formatLocalTime(at)}`)).toBeTruthy();
    expect(screen.getByText("⏸")).toBeTruthy();
  });

  it("parked: no resume clause for a past retry_not_before or a park without a clock", () => {
    renderCard({
      status: "limit_wait",
      retry_not_before: "2026-10-09T09:00:00Z",
      progress: progress({ state: "parked", pct: null }),
    });
    expect(screen.getByText("limit wait")).toBeTruthy();
    cleanup();
    renderCard({
      status: "paused",
      retry_not_before: "2026-10-09T21:00:00Z",
      progress: progress({ state: "parked", pct: null }),
    });
    expect(screen.getByText("paused")).toBeTruthy();
    expect(document.body.textContent).not.toContain("resumes");
  });

  it("queued: a dim `queued` flag", () => {
    const { container } = renderCard({ status: "queued", progress: progress({ state: "queued", pct: null }) });
    expect(card(container)?.getAttribute("data-run-progress-card")).toBe("queued");
    expect(screen.getByText("queued")).toBeTruthy();
  });

  it("planning: `planning · no milestones frozen yet`, no track, no phase steps", () => {
    const { container } = renderCard({
      milestones: null,
      milestones_completed: null,
      progress: progress({ state: "planning", pct: null, milestone_done: 0, milestone_total: 0, active_milestone_id: "", phase: "" }),
    });
    expect(screen.getByText("planning")).toBeTruthy();
    expect(screen.getByText("no milestones frozen yet")).toBeTruthy();
    expect(container.querySelector("[data-seg]")).toBeNull();
    expect(container.querySelector("[data-progress-phase]")).toBeNull();
  });

  it("renders nothing for null/absent progress, `none`, an unknown state, or a percent without pct", () => {
    for (const p of [null, undefined, progress({ state: "none", pct: null }), progress({ pct: null }), {
      ...progress({}),
      state: "future" as RunProgress["state"],
    }]) {
      const { container } = renderCard({ progress: p });
      expect(container.innerHTML).toBe("");
      cleanup();
    }
  });

  it("blocked-by: an info link to /runs/<id> showing the first 8 characters, beside the waiting flag", () => {
    const id = "fca7a801-1111-4222-8333-944455556666";
    renderCard({
      status: "awaiting_input",
      status_since: STATUS_SINCE,
      progress: progress({ state: "waiting", pct: null, maybe_blocked_by_run_id: id }),
    });
    const link = screen.getByRole("link", { name: /may be blocked by fca7a801/ });
    expect(link.getAttribute("href")).toBe(`/runs/${id}`);
    expect(link.textContent).toBe("⧗may be blocked by fca7a801");
    expect(link.className).toContain("text-info");
    expect(screen.getByText("open question mentions an issue that run is working")).toBeTruthy();
    expect(screen.getByText(/waits on you · question/)).toBeTruthy();
  });

  it("no blocked-by link without the hint", () => {
    renderCard({ status: "awaiting_input", progress: progress({ state: "waiting", pct: null }) });
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("strips control and bidi characters from a hostile milestone title and activity, in text and attributes", () => {
    const hostile = "evil\u202Etxt.exe\u200B\u0007-step";
    // Bidi/control inside the first 8 characters, so the visible short id would carry them.
    const hostileId = "fc\u202Ea7\u0007a8\u200B01-1111-4222-8333-944455556666";
    const { container } = renderCard(
      {
        milestones: [
          { id: "m1", title: "first" },
          { id: "m2", title: hostile },
        ],
        milestones_completed: ["m1"],
        progress: progress({
          pct: 55,
          milestone_done: 1,
          milestone_total: 2,
          active_milestone_id: "m2",
          maybe_blocked_by_run_id: hostileId,
        }),
      },
      anActivity({ agent: "co\u202Eder", agent_label: "fix\u2066 it\u0000" }),
    );
    const clean = "eviltxt.exe-step";
    expect(screen.getByText(`milestone 2 of 2 · ${clean}`)).toBeTruthy();
    const now = container.querySelector('[data-seg="now"]') as HTMLElement;
    expect(now.textContent).toBe(clean);
    expect(now.getAttribute("title")).toBe(`${clean} · in progress`);
    expect(screen.getByRole("img").getAttribute("aria-label")).toContain(`(${clean})`);
    const link = screen.getByRole("link");
    expect(link.textContent).toBe("⧗may be blocked by fca7a801");
    expect(link.getAttribute("href")).toBe("/runs/fca7a801-1111-4222-8333-944455556666");
    const dd = within(container).getByText("coder").closest("dd") as HTMLElement;
    expect(dd.textContent).toBe("coder · fix it");
    // Nothing unsafe survives anywhere: text, titles or accessible names.
    const unsafe = (v: string) =>
      [...v].some((ch) => {
        const c = ch.codePointAt(0) ?? 0;
        return c < 0x20 || c === 0x7f || c === 0x200b || (c >= 0x202a && c <= 0x202e) || (c >= 0x2066 && c <= 0x2069);
      });
    expect(unsafe(hostile)).toBe(true); // control: the predicate flags the raw input
    expect(unsafe(hostileId.slice(0, 8))).toBe(true);
    expect(unsafe(container.textContent ?? "")).toBe(false);
    for (const el of container.querySelectorAll("[title],[aria-label]")) {
      expect(unsafe(el.getAttribute("title") ?? "")).toBe(false);
      expect(unsafe(el.getAttribute("aria-label") ?? "")).toBe(false);
    }
  });

  it("falls back to the sanitised id when the active milestone is not in the frozen list", () => {
    renderCard({ progress: progress({ active_milestone_id: "gh\u202Eost" }) });
    expect(screen.getByText("active milestone · ghost")).toBeTruthy();
  });
});
