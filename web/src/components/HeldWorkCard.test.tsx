// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { RecoveryCustodyAggregate } from "../lib/api";
import { custodyAlertView } from "../lib/recovery";
import { HeldWorkCard } from "./HeldWorkCard";

afterEach(cleanup);

function agg(over: Partial<RecoveryCustodyAggregate>): RecoveryCustodyAggregate {
  return { open_holds: 0, admission_counted_holds: 0, custody_hold_limit: 8, decision_needed: 0, blocked_runs: 0, ...over };
}

function mount(a: RecoveryCustodyAggregate | null, recoveryWaitCount = 0) {
  return render(
    <MemoryRouter>
      <HeldWorkCard holds={a ? { aggregate: a, holds: [] } : null} recoveryWaitCount={recoveryWaitCount} />
    </MemoryRouter>,
  );
}

const cases: { name: string; a: RecoveryCustodyAggregate; used: number }[] = [
  { name: "blocked", a: agg({ open_holds: 3, admission_counted_holds: 3, blocked_runs: 2 }), used: 3 },
  { name: "capacity reached", a: agg({ open_holds: 8, admission_counted_holds: 8 }), used: 8 },
  { name: "decision needed", a: agg({ open_holds: 2, admission_counted_holds: 2, decision_needed: 2 }), used: 2 },
  { name: "healthy below the limit", a: agg({ open_holds: 2, admission_counted_holds: 2 }), used: 2 },
  { name: "no open hold", a: agg({}), used: 0 },
  { name: "open_holds 0 with decision_needed 2", a: agg({ open_holds: 0, admission_counted_holds: 0, decision_needed: 2 }), used: 0 },
  { name: "admission_counted_holds 0 with open_holds 3", a: agg({ open_holds: 3, admission_counted_holds: 0 }), used: 0 },
];

describe("HeldWorkCard", () => {
  it.each(cases)("$name: same headline and tone as the custody alert, three figures, link only for a decision", ({ a, used }) => {
    const { container } = mount(a);
    const view = custodyAlertView(a, 0);
    const section = container.querySelector("section") as HTMLElement;

    if (view) {
      const headline = screen.getByText(view.headline);
      expect(headline.className).toContain(view.tone === "danger" ? "text-danger" : "text-warn");
      expect(section.getAttribute("role")).toBe(view.tone === "danger" ? "alert" : "status");
      expect(section.getAttribute("aria-live")).toBe(view.tone === "danger" ? "assertive" : "polite");
      expect(section.getAttribute("aria-label")).toBe("Held work");
    } else {
      expect(screen.queryByText(/Held work is blocking|needs your attention|Custody capacity reached/)).toBeNull();
      expect(section.getAttribute("role")).toBeNull();
      expect(section.getAttribute("aria-live")).toBeNull();
    }

    // The three figures always render, zeros included.
    expect([...container.querySelectorAll("strong")].map((e) => e.textContent)).toEqual([
      `${used} / ${a.custody_hold_limit}`,
      String(a.open_holds),
      String(a.decision_needed),
    ]);
    expect(screen.getByText("custody slots used")).toBeTruthy();
    expect(screen.getByText(a.open_holds === 1 ? "open custody hold" : "open custody holds")).toBeTruthy();
    expect(screen.getByText(a.decision_needed === 1 ? "hold needs a decision" : "holds need a decision")).toBeTruthy();

    const link = screen.queryByRole("link", { name: "Review held work" });
    if (view && view.decisionNeeded > 0) {
      expect(link?.getAttribute("href")).toBe("/workers?tab=workers#recovery-holds");
    } else {
      expect(link).toBeNull();
    }
  });

  it("titles the card and names its scope", () => {
    mount(agg({}));
    expect(screen.getByRole("heading", { name: /Held work/ }).textContent).toContain("yours");
  });

  it("uses singular figure labels for one hold", () => {
    mount(agg({ open_holds: 1, admission_counted_holds: 1, decision_needed: 1 }));
    expect(screen.getByText("open custody hold")).toBeTruthy();
    expect(screen.getByText("hold needs a decision")).toBeTruthy();
  });

  it("reads the configured limit in its copy, never a hardcoded 8", () => {
    mount(agg({ custody_hold_limit: 3, open_holds: 1, admission_counted_holds: 1 }));
    expect(screen.getByText(/New runs wait for admission once all 3 custody slots are taken\./)).toBeTruthy();
    expect(screen.getByText(/Holds protect each run's committed but unpushed work, while it runs and after a worker is lost\./)).toBeTruthy();
    expect(screen.getByText("1 / 3")).toBeTruthy();
  });

  it("drops the admission sentence when there is no limit", () => {
    mount(agg({ custody_hold_limit: 0 }));
    expect(screen.queryByText(/New runs wait for admission/)).toBeNull();
    expect(screen.getByText(/Holds protect each run's committed/)).toBeTruthy();
  });

  it("shows how many runs are waiting to recover, only when some are", () => {
    mount(agg({}), 2);
    expect(screen.getByText("2 runs waiting to recover")).toBeTruthy();
    cleanup();
    mount(agg({}), 1);
    expect(screen.getByText("1 run waiting to recover")).toBeTruthy();
    cleanup();
    mount(agg({}), 0);
    expect(screen.queryByText(/waiting to recover/)).toBeNull();
  });

  it("says the data is unavailable, with no figures, before holds have loaded", () => {
    const { container } = mount(null);
    expect(screen.getByText("Held work is unavailable right now.")).toBeTruthy();
    expect(screen.queryByText("custody slots used")).toBeNull();
    expect(screen.queryByRole("link")).toBeNull();
    expect(container.querySelector("[role]")).toBeNull();
  });

  it("fills as many meter cells as slots are used, in the tone of the headline", () => {
    const { container } = mount(agg({ open_holds: 3, admission_counted_holds: 3, blocked_runs: 1 }));
    const cells = container.querySelectorAll("[aria-hidden=true] > span");
    expect(cells).toHaveLength(8);
    expect([...cells].filter((c) => c.className.includes("bg-danger"))).toHaveLength(3);
  });

  it("falls back to a proportional bar past 12 slots", () => {
    const { container } = mount(agg({ custody_hold_limit: 20, open_holds: 5, admission_counted_holds: 5 }));
    const fill = container.querySelector("[aria-hidden=true] > span") as HTMLElement;
    expect(fill.style.width).toBe("25%");
  });
});
