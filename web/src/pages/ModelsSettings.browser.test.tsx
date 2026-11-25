// PRD #3263 M1 and M2 in a real browser (layout needs a layout engine): below 768px the grid's
// cells stack under their row with a visible family label; the grid answers the card's width
// (the 15rem sidebar arrives at 1024px), so Claude and Codex sit side by side under column
// headers once the card is 49rem wide, with the row name above the pair until 60rem and in its
// own track from there; no select truncates a value at any width; the sticky Save bar never
// covers the last row; and every control's computed accessible name is "<Row> — <Family>". M2
// lengthens the grid with the helper and cross-check groups: their pairs stack the same way, the
// routing row spans both columns, a dimmed judge lane keeps its value at AA contrast in every
// theme and full emphasis while focused, and the bar still clears the last cross-check row.
import "../index.css";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { page } from "vitest/browser";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router-dom";
import { api, type EffectiveSettings, type UserSettings } from "../lib/api";
import { ModelsSettings } from "./ModelsSettings";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { getMySettings: vi.fn(), getEffectiveSettings: vi.fn(), putMyModelSettings: vi.fn() } };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: () => ({ user: { id: "u1" } }) }));

const mockApi = vi.mocked(api);

const settings = {
  default_harness: null, default_model: null, default_claude_model: "sonnet", default_codex_model: null,
  default_effort: null, default_codex_effort: "runtime", judge_model: null, summary_model: null, judge_runs_on: null,
  harness_overflow_enabled: false, theme: null, appearance_mode: null, light_theme: null, dark_theme: null, typeface: null,
  planning_claude_model: null, planning_claude_effort: null, planning_codex_model: null, planning_codex_effort: null,
  cross_check_pins: (["plan", "code"] as const).flatMap((stage) => (["claude", "codex"] as const).map((harness) => ({
    // A null Claude worker default renders the longest checker Default, "Default · SDK/account default".
    stage, harness, model: null, effort: null, worker_default_model: harness === "claude" ? null : "gpt-6.2-sol",
    resolved_model: harness === "claude" ? "sonnet" : "gpt-6.2-sol", resolved_effort: "high",
    model_source: "worker default", effort_source: "cross-check default", active: true,
  }))),
  // The longest Default values the page shows in practice (as the mock serves them): a select
  // must fit them without truncating.
  planning_models: [
    { harness: "claude", model: null, default_model: "opus", default_source: "template", resolved_model: "opus", source: "template" },
    { harness: "codex", model: null, default_model: "gpt-6-sol", default_source: "template", resolved_model: "gpt-6-sol", source: "template" },
  ],
  task_review_efforts: (["claude", "codex"] as const).map((harness) => ({
    harness, effort: null, default_effort: "medium", default_source: "builtin", resolved_effort: "medium", source: "builtin", mode: "explicit",
  })),
} as UserSettings;

const effective = {
  version: 1,
  harnesses: [
    { harness: "claude", usable: true, refusal: null, refusal_detail: null, credential: null },
    { harness: "codex", usable: true, refusal: null, refusal_detail: null, credential: null },
  ],
  start: [{ choice: "default", harness: { value: null, source: "chosen_at_create", fixed_at: "create", notes: ["automatic_weekly_reset"] }, refusal: null, refusal_detail: null, anthropic_override_allowed: true }],
  models: (["worker_model", "planning_model"] as const).flatMap((setting) => (["claude", "codex"] as const).map((harness) => ({
    harness, setting, model: null, default_model: harness === "claude" ? "opus" : "gpt-6.2-sol", default_source: "builtin",
    resolved_model: harness === "claude" ? "sonnet" : "gpt-6.2-sol", source: harness === "claude" ? "user_default" : "builtin",
    fixed_at: "claim", ignored: null, notes: [],
  }))),
  efforts: (["worker_effort", "planning_effort"] as const).flatMap((setting) => (["claude", "codex"] as const).map((harness) => ({
    harness, setting, effort: null, default_effort: "medium", default_source: "builtin", resolved_effort: "medium",
    source: "builtin", mode: "explicit", fixed_at: "claim", notes: [],
  }))),
  cross_check: { models: [], efforts: [] },
  judge_harness: { value: null, source: "same_as_run", fixed_at: "create", notes: [] },
} as unknown as EffectiveSettings;

const NAMES = [
  "Default harness", "Worker model — Claude", "Worker model — Codex", "Worker effort — Claude", "Worker effort — Codex",
  "Planning model — Claude", "Planning model — Codex", "Planning effort — Claude", "Planning effort — Codex",
  "Judge runs on", "Judge model — Claude", "Judge model — Codex", "Summary model — Claude", "Summary model — Codex",
  "Task review model — Claude", "Task review model — Codex", "Task review effort — Claude", "Task review effort — Codex",
  "Plan cross-check model — Claude", "Plan cross-check model — Codex", "Plan cross-check effort — Claude", "Plan cross-check effort — Codex",
  "Code cross-check model — Claude", "Code cross-check model — Codex", "Code cross-check effort — Claude", "Code cross-check effort — Codex",
];
// One pair row per group, to check stacking and side-by-side across the whole grid.
const PAIR_ROWS = ["Worker model", "Planning effort", "Judge model", "Task review effort", "Code cross-check effort"];

async function mount(width: number) {
  await page.viewport(width, 700);
  const router = createMemoryRouter([{ path: "*", element: <ModelsSettings /> }], { initialEntries: ["/settings/models"] });
  render(
    // AppShell's frame: the 15rem sidebar from lg, <main>'s padding (which the settings tab row's
    // negative margins assume) and the 68rem content column, so widths match the real page.
    <div className="lg:pl-60">
      <div className="min-w-0 px-4 sm:px-6">
        <div className="mx-auto w-full max-w-[68rem]">
          <RouterProvider router={router} />
        </div>
      </div>
    </div>,
  );
  await screen.findByRole("combobox", { name: "Worker model — Claude" });
  // Make the bar carry a change so it is at its tallest.
  fireEvent.change(screen.getByRole("combobox", { name: "Worker model — Codex" }), { target: { value: "gpt-6-sol" } });
}

const rect = (el: Element) => el.getBoundingClientRect();
const cellOf = (name: string) => screen.getByRole("combobox", { name }).closest("[data-field]")!;
const familyLabel = (name: string) => cellOf(name).querySelector("label")!;
const rowName = (row: string) => document.querySelector(`[data-row="${row}"] h4`)!;

// A select truncates an option when an auto-width clone holding only that option is wider than
// the select itself. The Codex worker effort's Runtime default option is the shared #3251 catalog
// sentence (lib/modelSettings codexRuntimeDefaultLabel), so it is exempt here.
const LONG_SHARED_OPTION = /^Runtime default · catalog default/;
function truncatedOptions(): string[] {
  const out: string[] = [];
  for (const el of document.querySelectorAll<HTMLSelectElement>("form select")) {
    const has = el.getBoundingClientRect().width;
    for (const option of el.options) {
      if (LONG_SHARED_OPTION.test(option.text)) continue;
      const clone = el.cloneNode(false) as HTMLSelectElement;
      clone.removeAttribute("id");
      clone.append(option.cloneNode(true));
      clone.style.cssText = "position:absolute;visibility:hidden;width:auto;max-width:none";
      el.parentElement!.appendChild(clone);
      const need = clone.getBoundingClientRect().width;
      clone.remove();
      if (need > has + 0.5) out.push(`${el.id}: "${option.text}" needs ${Math.ceil(need)}px, has ${Math.floor(has)}px`);
    }
  }
  return out;
}

// WCAG 2.x contrast of an element's text against the first opaque background behind it.
function rgb(css: string): [number, number, number, number] {
  const m = css.match(/rgba?\(([^)]+)\)/);
  if (!m) throw new Error(`unparsed colour ${css}`);
  const [r, g, b, a = "1"] = m[1].split(/[\s,/]+/).filter(Boolean);
  return [Number(r), Number(g), Number(b), Number(a)];
}
function luminance([r, g, b]: number[]): number {
  const lin = (c: number) => ((c /= 255) <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4);
  return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
}
function textContrast(el: Element): number {
  let node: Element | null = el;
  let bg: number[] | null = null;
  while (node && !bg) {
    const c = rgb(getComputedStyle(node).backgroundColor);
    if (c[3] === 1) bg = c;
    node = node.parentElement;
  }
  const fg = rgb(getComputedStyle(el).color);
  expect(fg[3]).toBe(1);
  // Any opacity on the element or an ancestor fades the text toward the background.
  let opacity = 1;
  for (let n: Element | null = el; n; n = n.parentElement) opacity *= Number(getComputedStyle(n).opacity);
  const shown = fg.slice(0, 3).map((v, i) => v * opacity + bg![i] * (1 - opacity));
  const [l1, l2] = [luminance(shown), luminance(bg!)].sort((a, b) => b - a);
  return (l1 + 0.05) / (l2 + 0.05);
}

function expectBarClearsLastRow() {
  window.scrollTo(0, document.documentElement.scrollHeight);
  const rows = document.querySelectorAll("[data-row]");
  const last = rows[rows.length - 1];
  expect(last.getAttribute("data-row")).toBe("Code cross-check effort");
  const bar = screen.getByTestId("models-save-bar");
  expect(rect(last).bottom).toBeLessThanOrEqual(rect(bar).top + 0.5);
  // Sticky: the bar is inside the viewport.
  expect(rect(bar).bottom).toBeLessThanOrEqual(window.innerHeight + 0.5);
}

beforeEach(() => {
  mockApi.getMySettings.mockResolvedValue({ settings });
  mockApi.getEffectiveSettings.mockResolvedValue(effective);
});
afterEach(() => {
  cleanup();
  window.scrollTo(0, 0);
});

describe("Models grid layout", () => {
  // Card content below 49rem: 390 and 700 (phone, tablet), 768 (no sidebar yet) and 1024 (the
  // sidebar has just arrived), where two families side by side would truncate their values.
  it.each([390, 700, 768, 1024])("stacks the cells where the card is narrow (%ipx), each with a visible family label", async (width) => {
    await mount(width);
    for (const row of PAIR_ROWS) {
      const claude = rect(cellOf(`${row} — Claude`));
      const codex = rect(cellOf(`${row} — Codex`));
      expect(codex.top, row).toBeGreaterThanOrEqual(claude.bottom);
      expect(Math.abs(codex.left - claude.left), row).toBeLessThan(1);
      for (const family of ["Claude", "Codex"]) {
        const label = familyLabel(`${row} — ${family}`);
        expect(rect(label).width, `${row} ${family} label`).toBeGreaterThan(20);
        expect(label.textContent).toContain(family);
      }
    }
    // At the top of a page taller than the viewport, the bar is pinned to the viewport bottom.
    window.scrollTo(0, 0);
    expect(document.documentElement.scrollHeight).toBeGreaterThan(window.innerHeight);
    const bar = rect(screen.getByTestId("models-save-bar"));
    expect(bar.bottom).toBeLessThanOrEqual(window.innerHeight + 0.5);
    expect(bar.top).toBeLessThan(window.innerHeight);
    // Nothing overflows the viewport sideways.
    expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(width);
    expectBarClearsLastRow();
  });

  // 900 and 1023 have no sidebar; 1279 and 1280 carry it; 1440 is wide enough for the name track.
  it.each([900, 1023, 1279, 1280, 1440])(
    "puts Claude and Codex side by side once the card is wide (%ipx), labelled by the column headers",
    async (width) => {
      await mount(width);
      for (const row of PAIR_ROWS) {
        const claude = rect(cellOf(`${row} — Claude`));
        const codex = rect(cellOf(`${row} — Codex`));
        expect(Math.abs(codex.top - claude.top), row).toBeLessThan(1);
        expect(codex.left, row).toBeGreaterThanOrEqual(claude.right);
        // Below a 60rem card the row name sits above the pair; from there it has its own track.
        const name = rect(rowName(row));
        if (width < 1440) expect(name.bottom, row).toBeLessThanOrEqual(claude.top + 0.5);
        else expect(name.right, row).toBeLessThanOrEqual(claude.left);
      }
      // Judge runs on spans both family columns.
      const routing = rect(cellOf("Judge runs on"));
      expect(routing.left).toBeLessThanOrEqual(rect(cellOf("Judge model — Claude")).left + 1);
      expect(routing.right).toBeGreaterThanOrEqual(rect(cellOf("Judge model — Codex")).right - 1);
      // The per-cell family label is visually hidden (sr-only) where the headers show.
      expect(rect(familyLabel("Worker effort — Claude")).width).toBeLessThanOrEqual(1);
      expectBarClearsLastRow();
    },
  );

  it.each([700, 768, 900, 1023, 1024, 1279, 1280, 1440])("truncates no select's widest option at %ipx", async (width) => {
    await mount(width);
    // Every select offers its options at full length, including "Default · SDK/account default".
    const firstOption = (name: string) => screen.getByRole("combobox", { name }).querySelector("option")!.textContent;
    expect(firstOption("Plan cross-check model — Claude")).toBe("Default · SDK/account default");
    expect(firstOption("Planning model — Codex")).toBe("Same as the run · gpt-6-sol (lead template)");
    expect(firstOption("Task review effort — Codex")).toBe("Default · follows worker effort (medium)");
    expect(truncatedOptions()).toEqual([]);
  });

  it.each(["dawn", "hall", "shadow", "ember", "mission"])(
    "keeps the dimmed judge lane's value at AA contrast on %s, restores it while focused, and keeps it editable",
    async (theme) => {
      document.documentElement.setAttribute("data-theme", theme);
      try {
        await mount(1280);
        fireEvent.change(screen.getByRole("combobox", { name: "Judge runs on" }), { target: { value: "claude" } });
        const codex = screen.getByRole("combobox", { name: "Judge model — Codex" }) as HTMLSelectElement;
        const claude = screen.getByRole("combobox", { name: "Judge model — Claude" }) as HTMLSelectElement;
        const wrapper = codex.closest("[data-field]")!.querySelector("[data-dim]") as HTMLElement;
        expect(wrapper).not.toBeNull();
        // Dimmed by treatment, never by opacity: a dashed, unfilled control whose value stays >= 4.5:1.
        expect(getComputedStyle(wrapper).opacity).toBe("1");
        expect(getComputedStyle(codex).borderStyle).toBe("dashed");
        expect(getComputedStyle(codex).color).not.toBe(getComputedStyle(claude).color);
        expect(textContrast(codex), `${theme} dimmed`).toBeGreaterThanOrEqual(4.5);
        // The reason itself is not inside the dimmed wrapper.
        const reason = document.getElementById(codex.getAttribute("aria-describedby")!.split(" ").find((id) => id.endsWith("-not-used"))!)!;
        expect(reason.textContent).toBe("Not used: the judge runs on Claude");
        expect(wrapper.contains(reason)).toBe(false);
        expect(textContrast(reason), `${theme} reason`).toBeGreaterThanOrEqual(4.5);
        codex.focus();
        expect(document.activeElement).toBe(codex);
        // Focus restores the full control: the active lane's colour and border.
        expect(getComputedStyle(codex).borderStyle).toBe("solid");
        expect(getComputedStyle(codex).color).toBe(getComputedStyle(claude).color);
        expect(getComputedStyle(codex).backgroundColor).toBe(getComputedStyle(claude).backgroundColor);
        expect(codex.disabled).toBe(false);
      } finally {
        document.documentElement.removeAttribute("data-theme");
      }
    },
  );

  // #4665/#4671: a long model ID in a shown saved line (here the server resolves another value
  // than the saved choice, so the line shows) wraps on a phone instead of widening the page, and
  // the line and the Reset to default button stay at AA contrast.
  it("wraps a long model ID inside its saved line at 390px", async () => {
    const long = "acme-internal-finetune-claude-opus-distilled-2026-11-24-experiment-b";
    mockApi.getEffectiveSettings.mockResolvedValue({
      ...effective,
      models: effective.models.map((m) =>
        m.setting === "worker_model" && m.harness === "claude" ? { ...m, resolved_model: long } : m,
      ),
    } as EffectiveSettings);
    await mount(390);
    const cell = cellOf("Worker model — Claude");
    const tag = cell.querySelector<HTMLElement>("[data-prediction]")!;
    expect(tag.textContent).toContain(long);
    expect(rect(tag).right).toBeLessThanOrEqual(rect(cell).right + 0.5);
    // Wrapped: taller than one line of its own text.
    expect(rect(tag).height).toBeGreaterThan(parseFloat(getComputedStyle(tag).lineHeight) * 1.5);
    expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(390);
  });

  it.each(["dawn", "hall", "shadow", "ember", "mission"])("keeps the saved line and Reset to default at AA contrast on %s", async (theme) => {
    document.documentElement.setAttribute("data-theme", theme);
    try {
      await mount(1280);
      // Default harness hides its value (automatic); Worker model — Codex is changed by mount().
      for (const name of ["Default harness", "Worker model — Codex"]) {
        const line = cellOf(name).querySelector<HTMLElement>("[data-prediction]")!;
        expect(textContrast(line), `${theme} ${name}`).toBeGreaterThanOrEqual(4.5);
      }
      // A saved override the select already shows: no visible line, a Reset button instead.
      expect(cellOf("Worker model — Claude").querySelector("[data-prediction]")).toBeNull();
      const reset = screen.getByRole("button", { name: "Reset to default (Worker model — Claude)" });
      expect(textContrast(reset), `${theme} reset`).toBeGreaterThanOrEqual(4.5);
    } finally {
      document.documentElement.removeAttribute("data-theme");
    }
  });

  // #4665/#4671: the visible line is split or hidden; the COMPUTED description must still be one
  // sentence, clean (line hidden) and dirty (mount() leaves Worker model — Codex changed).
  it("computes the saved prediction as one description sentence, clean and dirty", async () => {
    await mount(1280);
    expect(screen.getByRole("combobox", { name: "Worker model — Claude", description: /^Saved effective value: sonnet · your choice$/ })).toBeTruthy();
    expect(screen.getByRole("combobox", { name: "Worker model — Codex", description: /^Saved effective value: gpt-6\.2-sol · built-in, Unsaved$/ })).toBeTruthy();
  });

  it("computes every control's accessible name as <Row> — <Family>", async () => {
    await mount(1024);
    for (const name of NAMES) {
      const el = screen.getByRole("combobox", { name });
      expect(el.getAttribute("aria-describedby"), name).toContain("-saved");
    }
  });
});
