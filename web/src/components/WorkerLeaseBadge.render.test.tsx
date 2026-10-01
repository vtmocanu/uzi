// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render } from "@testing-library/react";
import type { Worker } from "../lib/api";
import { WorkerLeaseBadge } from "./WorkerLeaseBadge";

const base = { id: "w-e", name: "auto (M)", kind: "hosted", ephemeral: true } as unknown as Worker;

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe("WorkerLeaseBadge clock (PRD #2006 M2)", () => {
  it("counts down from the time the lease APPEARS, not from the row's mount time", () => {
    // A row mounts while its run is in flight (no lease), and only gains the lease on a
    // poll hours later. The countdown must read against the current clock: at 12:00 an
    // expiry of 14:00 is 2h left, not the 5h a clock frozen at the 09:00 mount gives.
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-10-01T09:00:00Z"));
    const { container, rerender } = render(<WorkerLeaseBadge worker={base} />);
    expect(container.textContent).toBe("");

    vi.setSystemTime(new Date("2026-10-01T12:00:00Z"));
    rerender(<WorkerLeaseBadge worker={{ ...base, ephemeral_lease_expires_at: "2026-10-01T14:00:00Z" }} />);
    expect(container.textContent).toBe("leased · 2h 0m left");
  });

  it("restarts the clock when the lease is renewed to a new expiry", () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-10-01T12:00:00Z"));
    const { container, rerender } = render(
      <WorkerLeaseBadge worker={{ ...base, ephemeral_lease_expires_at: "2026-10-01T14:00:00Z" }} />,
    );
    expect(container.textContent).toBe("leased · 2h 0m left");

    vi.setSystemTime(new Date("2026-10-01T13:00:00Z"));
    rerender(<WorkerLeaseBadge worker={{ ...base, ephemeral_lease_expires_at: "2026-10-01T15:00:00Z" }} />);
    expect(container.textContent).toBe("leased · 2h 0m left");
  });
});
