// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MeterTrack, clampPct, toneFor } from "./Meter";

afterEach(cleanup);

describe("toneFor thresholds (PRD #53 / #49 Decision 6)", () => {
  it("is ok below 80, warn at 80–94, danger at 95+", () => {
    expect(toneFor(0)).toBe("ok");
    expect(toneFor(79)).toBe("ok");
    expect(toneFor(79.9)).toBe("ok");
    expect(toneFor(80)).toBe("warn");
    expect(toneFor(94)).toBe("warn");
    expect(toneFor(94.9)).toBe("warn");
    expect(toneFor(95)).toBe("danger");
    expect(toneFor(100)).toBe("danger");
    expect(toneFor(6400)).toBe("danger");
  });
});

describe("clampPct", () => {
  it("clamps to [0,100]", () => {
    expect(clampPct(-5)).toBe(0);
    expect(clampPct(50)).toBe(50);
    expect(clampPct(6400)).toBe(100);
  });
});

describe("MeterTrack", () => {
  it("renders an accessible progressbar whose tone/width/valuenow agree on one rounded int", () => {
    render(<MeterTrack label="5-hour window" fillPct={94.99} valueText="95%" />);
    const bar = screen.getByRole("progressbar", { name: "5-hour window" });
    // 94.99 rounds to 95 → danger, and the number the reader hears matches the fill.
    expect(bar.getAttribute("aria-valuenow")).toBe("95");
    expect(bar.getAttribute("aria-valuetext")).toBe("95%");
    const fill = bar.firstChild as HTMLElement;
    expect(fill.style.width).toBe("95%");
    expect(fill.className).toMatch(/bg-danger/);
  });

  it("clamps the DOM width to 100% for an over-100 value", () => {
    render(<MeterTrack label="cpu" fillPct={640} valueText="640%" />);
    const bar = screen.getByRole("progressbar", { name: "cpu" });
    expect(bar.getAttribute("aria-valuenow")).toBe("100");
    expect((bar.firstChild as HTMLElement).style.width).toBe("100%");
  });

  it("fades the fill when dim is set (stale reading)", () => {
    render(<MeterTrack label="7-day window" fillPct={12} valueText="12%" dim />);
    const fill = screen.getByRole("progressbar", { name: "7-day window" }).firstChild as HTMLElement;
    expect(fill.className).toMatch(/opacity-40/);
    expect(fill.className).toMatch(/bg-ok/);
  });
});
