/// <reference types="@vitest/browser-playwright" />
import "../index.css";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { page } from "vitest/browser";
import { api } from "../lib/api";
import { RECORDED_EFFECTIVE_SCENARIOS } from "../mocks/data/effectiveSettings";
import { SchedulePreview } from "./SchedulePreview";

// Issue #3260: the preview block in a real engine. Target size and the contrast of its text per
// theme need real layout and computed styles, which jsdom cannot give.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { previewScheduleSettings: vi.fn() } };
});

const mockApi = vi.mocked(api);
const originalTheme = document.documentElement.getAttribute("data-theme");

beforeEach(() => {
  const p = RECORDED_EFFECTIVE_SCENARIOS[0].schedule_previews.find(
    (x) => x.name === "sweep pinned to codex with a claude model pin",
  )!;
  mockApi.previewScheduleSettings.mockResolvedValue(structuredClone(p.response));
});
afterEach(async () => {
  cleanup();
  await page.viewport(1280, 800);
  vi.clearAllMocks();
  if (originalTheme === null) document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", originalTheme);
});

// bg-surface is the schedule modal's dialog panel background.
function mount(width?: number) {
  render(
    <MemoryRouter>
      <div className="bg-surface p-4" data-testid="surface" style={width ? { width } : undefined}>
        <SchedulePreview request={{ target: "sweep", model: "sonnet" }} bothUsable debounceMs={0} />
      </div>
    </MemoryRouter>,
  );
}

const MODELS_LINK = "Models (opens in new tab)";

const heading = () => screen.findByRole("heading", { name: "If fired and claimed now" });

it("has the accessible heading and a definition list of rows", async () => {
  mount();
  await heading();
  const region = await screen.findByRole("region", { name: "If fired and claimed now" });
  await vi.waitFor(() => expect(region.querySelector("dl")).not.toBeNull());
  expect(region.querySelectorAll("dt").length).toBeGreaterThan(3);
});

it("the Models link is at least 24px tall", async () => {
  mount();
  const link = await screen.findByRole("link", { name: MODELS_LINK });
  expect(link.getBoundingClientRect().height).toBeGreaterThanOrEqual(24);
});

it("at a narrow width no value column is squeezed and the list does not overflow", async () => {
  await page.viewport(390, 844);
  const p = RECORDED_EFFECTIVE_SCENARIOS[0].schedule_previews.find((x) => x.name === "unpinned sweep")!;
  mockApi.previewScheduleSettings.mockResolvedValue(structuredClone(p.response));
  mount(320);
  const region = await screen.findByRole("region", { name: "If fired and claimed now" });
  await vi.waitFor(() => expect(region.querySelectorAll("dd").length).toBeGreaterThan(6));
  for (const dd of Array.from(region.querySelectorAll("dd"))) {
    if (!dd.textContent) continue;
    expect(dd.getBoundingClientRect().width, dd.textContent).toBeGreaterThanOrEqual(40);
  }
  const dl = region.querySelector("dl")!;
  expect(dl.scrollWidth).toBeLessThanOrEqual(dl.clientWidth);
});

type RGBA = [number, number, number, number]; // channels 0-255, alpha 0-1

// Parses the forms Chromium emits for computed colours: rgb()/rgba() (comma or space syntax),
// color(srgb r g b / a) with 0-1 channels, and oklab(). Anything else throws: a colour this test cannot read
// must fail loudly, never be skipped.
function parseColor(color: string): RGBA {
  const num = "(-?[\\d.]+(?:e-?\\d+)?)";
  const legacy = color.match(
    new RegExp(`^rgba?\\(\\s*${num}[,\\s]+${num}[,\\s]+${num}(?:\\s*[,/]\\s*${num}(%?))?\\s*\\)$`),
  );
  if (legacy) {
    const a = legacy[4] === undefined ? 1 : legacy[5] ? Number(legacy[4]) / 100 : Number(legacy[4]);
    return [Number(legacy[1]), Number(legacy[2]), Number(legacy[3]), a];
  }
  const srgb = color.match(new RegExp(`^color\\(srgb\\s+${num}\\s+${num}\\s+${num}(?:\\s*/\\s*${num}(%?))?\\s*\\)$`));
  if (srgb) {
    const a = srgb[4] === undefined ? 1 : srgb[5] ? Number(srgb[4]) / 100 : Number(srgb[4]);
    return [Number(srgb[1]) * 255, Number(srgb[2]) * 255, Number(srgb[3]) * 255, a];
  }
  // Chromium keeps color-mix()/opacity results in oklab (e.g. bg-warn/10).
  const oklab = color.match(new RegExp(`^oklab\\(\\s*${num}(%?)\\s+${num}\\s+${num}(?:\\s*/\\s*${num}(%?))?\\s*\\)$`));
  if (oklab) {
    const L = oklab[2] ? Number(oklab[1]) / 100 : Number(oklab[1]);
    const a = Number(oklab[3]);
    const b = Number(oklab[4]);
    const alpha = oklab[5] === undefined ? 1 : oklab[6] ? Number(oklab[5]) / 100 : Number(oklab[5]);
    const l = (L + 0.3963377774 * a + 0.2158037573 * b) ** 3;
    const m = (L - 0.1055613458 * a - 0.0638541728 * b) ** 3;
    const s = (L - 0.0894841775 * a - 1.291485548 * b) ** 3;
    const enc = (c: number) => {
      const x = Math.min(1, Math.max(0, c));
      return 255 * (x <= 0.0031308 ? 12.92 * x : 1.055 * x ** (1 / 2.4) - 0.055);
    };
    return [
      enc(4.0767416621 * l - 3.3077115913 * m + 0.2309699292 * s),
      enc(-1.2684380046 * l + 2.6097574011 * m - 0.3413193965 * s),
      enc(-0.0041960863 * l - 0.7034186147 * m + 1.707614701 * s),
      alpha,
    ];
  }
  throw new Error(`unparsed colour ${JSON.stringify(color)}`);
}

function over(top: RGBA, under: RGBA): RGBA {
  const a = top[3] + under[3] * (1 - top[3]);
  if (a === 0) return [0, 0, 0, 0];
  const ch = (i: number) => (top[i] * top[3] + under[i] * under[3] * (1 - top[3])) / a;
  return [ch(0), ch(1), ch(2), a];
}

// The element's real backdrop: every ancestor background (self included) composited, from the
// nearest opaque one down to the element.
function backdrop(el: Element): RGBA {
  const layers: RGBA[] = [];
  for (let n: Element | null = el; n; n = n.parentElement) {
    const c = parseColor(getComputedStyle(n).backgroundColor);
    if (c[3] === 0) continue;
    layers.push(c);
    if (c[3] >= 1) break;
  }
  const base = layers[layers.length - 1];
  if (!base || base[3] < 1) throw new Error(`no opaque background found above ${el.textContent}`);
  return layers.reduceRight((under, top) => over(top, under));
}

function dd(label: string, nth: 1 | 2, root: HTMLElement): Element {
  const dt = Array.from(root.querySelectorAll("dt")).find((d) => d.textContent === label);
  if (!dt) throw new Error(`no dt ${label}`);
  const el = nth === 1 ? dt.nextElementSibling : dt.nextElementSibling?.nextElementSibling;
  if (!el || el.tagName !== "DD") throw new Error(`no dd ${nth} after dt ${label}`);
  return el;
}

function luminance([r, g, b]: RGBA | [number, number, number]): number {
  const lin = (c: number) => {
    const s = c / 255;
    return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
}

it.each(["dawn", "ember"])("labels, values, hint and link read at WCAG AA contrast in %s", async (theme) => {
  document.documentElement.setAttribute("data-theme", theme);
  mount();
  const link = await screen.findByRole("link", { name: MODELS_LINK });
  const region = await screen.findByRole("region", { name: "If fired and claimed now" });
  await vi.waitFor(() => expect(region.querySelector("dl")).not.toBeNull());
  const targets = [
    link,
    dd("Model", 1, region),
    dd("Model", 2, region),
    dd("Effort", 1, region),
    // The warning Alert text, measured against the surface it sits on.
    screen.getByRole("status"),
    screen.getByText("Harness"),
    screen.getByText("updates as you change the picks above"),
    screen.getByText("This preview is advisory; Save validates again."),
    screen.getByRole("heading", { name: "If fired and claimed now" }),
  ];
  for (const el of targets) {
    const bgc = backdrop(el);
    const fgc = over(parseColor(getComputedStyle(el).color), bgc);
    const bg = luminance(bgc);
    const fg = luminance(fgc);
    const ratio = (Math.max(fg, bg) + 0.05) / (Math.min(fg, bg) + 0.05);
    expect(ratio, `${el.textContent} in ${theme}`).toBeGreaterThanOrEqual(4.5);
  }
});
