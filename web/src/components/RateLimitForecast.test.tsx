// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { RateLimitForecastMeter } from "./RateLimitForecast";
import { MeterTrack } from "./Meter";
import type { PaceForecast } from "../lib/rateLimits";

afterEach(cleanup);

const OVER: PaceForecast = { state: "over", projectedPct: 130 };
const ON_PACE_HIGH: PaceForecast = { state: "on_pace", projectedPct: 108 };
const ON_PACE_LOW: PaceForecast = { state: "on_pace", projectedPct: 95 };
const SAFE: PaceForecast = { state: "safe", projectedPct: 0 };

function renderMeter(forecast: PaceForecast, pct = 56) {
  return render(
    <RateLimitForecastMeter label="5-hour window" pct={pct} valueText={`${pct}%, resets in 2h`} forecast={forecast} className="mt-1.5 h-2" />,
  );
}

// The ghost is an aria-hidden div carrying the translucent tone class; the fill is
// MeterTrack's own div. Query the ghost by its tone class.
function ghost(container: HTMLElement, tone: string): HTMLElement | null {
  return container.querySelector<HTMLElement>(`div[aria-hidden].${tone.replace("/", "\\/")}`);
}

describe("RateLimitForecastMeter (PRD #309 M3)", () => {
  it("safe → renders the plain atom: no ghost, no marker, no title, base valueText", () => {
    const { container } = renderMeter(SAFE);
    const bar = screen.getByRole("progressbar", { name: "5-hour window" });
    expect(bar.getAttribute("aria-valuetext")).toBe("56%, resets in 2h"); // untouched
    expect(container.querySelector("[title]")).toBeNull(); // no hover projection
    expect(container.querySelector('[data-testid="forecast-overflow-marker"]')).toBeNull();
    expect(ghost(container, "bg-warn/40")).toBeNull();
    expect(ghost(container, "bg-danger/40")).toBeNull();
  });

  it("over → coral ghost + », projection only in aria-valuetext and the hover title", () => {
    const { container } = renderMeter(OVER);
    expect(ghost(container, "bg-danger/40")).not.toBeNull(); // coral ghost
    expect(ghost(container, "bg-warn/40")).toBeNull();
    const marker = screen.getByTestId("forecast-overflow-marker");
    expect(marker.className).toContain("text-danger");
    expect(marker.className).toContain("left-full"); // over → OUTSIDE the bar's right edge (overshoot)
    // Projected % is hover/aria-only, NEVER printed inline as visible number text.
    const bar = screen.getByRole("progressbar", { name: "5-hour window" });
    expect(bar.getAttribute("aria-valuetext")).toBe("56%, resets in 2h — projected 130% by reset, over");
    expect(container.querySelector("[title]")?.getAttribute("title")).toBe(
      "56%, resets in 2h — projected 130% by reset, over",
    );
    expect(bar.getAttribute("aria-valuenow")).toBe("56"); // fill still reflects CURRENT usage
    expect(container.textContent).not.toContain("130"); // projection is NEVER inline visible text (D4)
  });

  it("on-pace over the cap (100–115) → gold ghost + gold » ", () => {
    const { container } = renderMeter(ON_PACE_HIGH);
    expect(ghost(container, "bg-warn/40")).not.toBeNull(); // gold ghost
    expect(ghost(container, "bg-danger/40")).toBeNull();
    const marker = screen.getByTestId("forecast-overflow-marker");
    expect(marker.className).toContain("text-warn");
    expect(marker.className).not.toContain("left-full"); // on pace → NOT outside the bar
    expect(marker.style.left).toContain("56%"); // anchored just past the fill's end (pct=56), not the cap
    expect(screen.getByRole("progressbar").getAttribute("aria-valuetext")).toBe(
      "56%, resets in 2h — projected 108% by reset, on pace",
    );
  });

  it("on-pace landing at/under the cap (≤100) → gold ghost but NO » marker", () => {
    const { container } = renderMeter(ON_PACE_LOW);
    expect(ghost(container, "bg-warn/40")).not.toBeNull();
    expect(container.querySelector('[data-testid="forecast-overflow-marker"]')).toBeNull();
  });

  it("ghost animation is disabled under prefers-reduced-motion", () => {
    const { container } = renderMeter(OVER);
    expect(ghost(container, "bg-danger/40")?.className).toContain("motion-reduce:transition-none");
  });

  it("the shared MeterTrack atom, used directly (e.g. WorkerStats), carries no forecast overlay", () => {
    const { container } = render(<MeterTrack label="cpu" fillPct={70} valueText="70%" className="h-2" />);
    expect(container.querySelector("[title]")).toBeNull();
    expect(container.querySelector('[data-testid="forecast-overflow-marker"]')).toBeNull();
    expect(container.querySelector("div[aria-hidden]")).toBeNull();
  });
});
