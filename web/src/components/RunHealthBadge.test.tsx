// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { RunHealthBadge } from "./RunHealthBadge";
import type { HealthFlaggable } from "../lib/runBadge";

afterEach(cleanup);

const NOW = Date.parse("2026-07-04T12:05:00Z");

function flaggable(over: Partial<HealthFlaggable> = {}): HealthFlaggable {
  return {
    health: "ok",
    health_since: null,
    health_reason: null,
    status: "running",
    ...over,
  };
}

describe("RunHealthBadge (PRD #47 dashboard/runs-list surface)", () => {
  it("renders the ⚠ flag with elapsed and the owner reason as a tooltip", () => {
    render(
      <RunHealthBadge
        run={flaggable({
          health: "stalled",
          health_since: "2026-07-04T12:03:00Z",
          health_reason: "the agent stopped sending updates",
        })}
        nowMs={NOW}
      />,
    );
    const badge = screen.getByText("⚠ stalled · 2m");
    expect(badge).toBeTruthy();
    expect(badge.getAttribute("title")).toBe("the agent stopped sending updates");
  });

  it("renders nothing for a healthy run", () => {
    const { container } = render(<RunHealthBadge run={flaggable({ health: "ok" })} nowMs={NOW} />);
    expect(container.firstChild).toBeNull();
  });

  it("renders nothing for a terminal run carrying a stale flag (belt-and-braces)", () => {
    const { container } = render(
      <RunHealthBadge run={flaggable({ health: "stalled", status: "completed" })} nowMs={NOW} />,
    );
    expect(container.firstChild).toBeNull();
  });

  it("omits the tooltip for a non-owner (health_reason null)", () => {
    render(
      <RunHealthBadge
        run={flaggable({ health: "looping", health_since: "2026-07-04T12:04:00Z", health_reason: null })}
        nowMs={NOW}
      />,
    );
    const badge = screen.getByText(/⚠ looping/);
    expect(badge.getAttribute("title")).toBeNull();
  });
});
