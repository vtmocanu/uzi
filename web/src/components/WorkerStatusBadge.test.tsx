// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { aWorker } from "../pages/overviewFixtures";
import { WorkerCordonBadge } from "./WorkerCordonBadge";
import { WorkerStatusBadge } from "./WorkerStatusBadge";

afterEach(cleanup);

describe("WorkerStatusBadge", () => {
  it("shows the heartbeat status for a worker that is not hibernating", () => {
    render(<WorkerStatusBadge worker={aWorker({ status: "offline" })} />);
    expect(screen.getByText("offline")).toBeTruthy();
  });

  it.each([
    ["preparing", "hibernating"],
    ["hibernated", "hibernated"],
    ["waking", "waking"],
  ] as const)("renders %s as %s instead of the offline status", (state, label) => {
    render(<WorkerStatusBadge worker={aWorker({ status: "offline", hibernation_state: state })} />);
    expect(screen.getByText(label)).toBeTruthy();
    expect(screen.queryByText("offline")).toBeNull();
    expect(screen.queryByText("wake failed")).toBeNull();
  });

  it("adds a wake-failed chip whose tooltip is the sanitized error text", () => {
    render(<WorkerStatusBadge worker={aWorker({ status: "offline", hibernation_state: "hibernated", wake_error: "boom‮ <i>x</i>" })} />);
    expect(screen.getByText("wake failed").getAttribute("title")).toBe("boom <i>x</i>");
  });

  it("does not read a hibernated worker as cordoned or intake fenced", () => {
    const w = aWorker({ hibernation_state: "hibernated", effective_cordoned: true });
    const { container } = render(<WorkerCordonBadge worker={w} />);
    expect(container.textContent).toBe("");
  });

  it("still shows intake fenced for a non-hibernating effective cordon", () => {
    render(<WorkerCordonBadge worker={aWorker({ effective_cordoned: true })} />);
    expect(screen.getByText("intake fenced")).toBeTruthy();
  });
});
