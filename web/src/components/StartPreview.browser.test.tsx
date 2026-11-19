/// <reference types="@vitest/browser-playwright" />
import "../index.css";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { userEvent } from "vitest/browser";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { api } from "../lib/api";
import { RECORDED_EFFECTIVE_SCENARIOS } from "../mocks/data/effectiveSettings";
import { DEFAULT_START_PREVIEW_REQUEST } from "../lib/startPreview";
import { StartPreviewProvider } from "../lib/startPreviewStore";
import { StartPreview } from "./StartPreview";

// Issue #3261: the disclosure in a real engine. Target size, native keyboard activation, a click
// inside a draggable card that must not start a drag, and a contrast smoke check per theme need
// real layout and real input events, which jsdom cannot give.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { getEffectiveSettings: vi.fn(), previewStartSettings: vi.fn() } };
});
vi.mock("../auth/AuthContext", () => ({ useOptionalAuth: () => ({ user: { id: "u1" } }) }));

const mockApi = vi.mocked(api);
const SCENARIO = RECORDED_EFFECTIVE_SCENARIOS[0];
const originalTheme = document.documentElement.getAttribute("data-theme");

beforeEach(() => {
  mockApi.getEffectiveSettings.mockResolvedValue(structuredClone(SCENARIO.get));
  mockApi.previewStartSettings.mockResolvedValue(
    structuredClone(SCENARIO.start_previews.find((p) => p.name === "claude")!.response),
  );
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  if (originalTheme === null) document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", originalTheme);
});

function mount() {
  render(
    <MemoryRouter>
      <StartPreviewProvider generation={1}>
        <div className="bg-ink p-4" data-testid="surface">
          <div draggable className="rounded-xl border p-3" data-testid="card">
            <StartPreview variant="compact" request={DEFAULT_START_PREVIEW_REQUEST} />
          </div>
        </div>
      </StartPreviewProvider>
    </MemoryRouter>,
  );
}

const button = () => screen.findByRole("button", { name: /If started now/ });

it("the disclosure target is at least 24px tall", async () => {
  mount();
  const rect = (await button()).getBoundingClientRect();
  expect(rect.height).toBeGreaterThanOrEqual(24);
});

it("toggles from the keyboard with Enter and Space, with no hover involved", async () => {
  mount();
  const b = await button();
  b.focus();
  expect(b.getAttribute("aria-expanded")).toBe("false");
  await userEvent.keyboard("{Enter}");
  expect(b.getAttribute("aria-expanded")).toBe("true");
  await userEvent.keyboard(" ");
  expect(b.getAttribute("aria-expanded")).toBe("false");
});

it("a click on the disclosure inside a draggable card toggles it, and the button is not itself draggable", async () => {
  mount();
  const b = await button();
  expect(b.getAttribute("draggable")).toBe("false");
  await userEvent.click(b);
  expect(b.getAttribute("aria-expanded")).toBe("true");
  await userEvent.click(b);
  expect(b.getAttribute("aria-expanded")).toBe("false");
});

it("interacting with the disclosure never starts a drag on the draggable card", async () => {
  mount();
  const b = await button();
  const card = screen.getByTestId("card");
  const onDragStart = vi.fn();
  card.addEventListener("dragstart", onDragStart);
  // Control: the spy does fire for a dragstart dispatched on the card itself.
  card.dispatchEvent(new Event("dragstart", { bubbles: true }));
  expect(onDragStart).toHaveBeenCalledTimes(1);
  onDragStart.mockClear();

  await userEvent.click(b);
  expect(b.getAttribute("aria-expanded")).toBe("true");
  b.focus();
  await userEvent.keyboard("{Enter}");
  expect(b.getAttribute("aria-expanded")).toBe("false");
  await userEvent.keyboard(" ");
  expect(b.getAttribute("aria-expanded")).toBe("true");
  expect(onDragStart).not.toHaveBeenCalled();
});

function rgb(color: string): [number, number, number] {
  const m = color.match(/rgba?\((\d+),\s*(\d+),\s*(\d+)/);
  if (!m) throw new Error(`unparsed colour ${color}`);
  return [Number(m[1]), Number(m[2]), Number(m[3])];
}
function luminance([r, g, b]: [number, number, number]): number {
  const lin = (c: number) => {
    const s = c / 255;
    return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
}

it.each(["dawn", "ember"])("the line reads at WCAG AA contrast in %s", async (theme) => {
  document.documentElement.setAttribute("data-theme", theme);
  mount();
  const b = await button();
  const fg = luminance(rgb(getComputedStyle(b).color));
  const bg = luminance(rgb(getComputedStyle(screen.getByTestId("surface")).backgroundColor));
  const ratio = (Math.max(fg, bg) + 0.05) / (Math.min(fg, bg) + 0.05);
  expect(ratio).toBeGreaterThanOrEqual(4.5);
});
