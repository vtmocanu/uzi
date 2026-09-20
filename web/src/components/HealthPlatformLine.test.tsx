// @vitest-environment jsdom
//
// Non-admin Overview platform line (PRD #1484 M5, D3/D4). A pure component over the viewer's
// own runs + workers, so the test drives it directly with props — no api, no provider. The
// line is selected by its SCOPED handle (role="status" + aria-label="Your hosted workers"),
// never a bare getByRole("status").
//
// Covered: appears only when all three conditions hold; absent for an admin (admins get the
// card); absent when any single condition fails.
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import { HealthPlatformLine } from "./HealthPlatformLine";
import type { RunListItem, Worker } from "../lib/api";

afterEach(cleanup);

// Minimal fixtures: the component reads only run.status and worker.kind/upgrade_status.
const queuedRun = { status: "queued" } as unknown as RunListItem;
const runningRun = { status: "running" } as unknown as RunListItem;
const failedHosted = { kind: "hosted", upgrade_status: "upgrade_failed" } as unknown as Worker;
const okHosted = { kind: "hosted", upgrade_status: "up_to_date" } as unknown as Worker;
const externalWorker = { kind: "external", upgrade_status: "unknown" } as unknown as Worker;

const line = () => screen.queryByRole("status", { name: "Your hosted workers" });

describe("HealthPlatformLine", () => {
  it("appears when the viewer is non-admin, has a queued run, and every hosted worker is upgrade_failed", () => {
    render(<HealthPlatformLine isAdmin={false} runs={[queuedRun]} workers={[failedHosted]} />);
    expect(line()).not.toBeNull();
    expect(screen.getByText(/This is a platform problem, not your run/)).toBeTruthy();
  });

  it("is absent for an admin even when the three conditions hold (admins get the card)", () => {
    render(<HealthPlatformLine isAdmin={true} runs={[queuedRun]} workers={[failedHosted]} />);
    expect(line()).toBeNull();
  });

  it("is absent when there is no queued run", () => {
    render(<HealthPlatformLine isAdmin={false} runs={[runningRun]} workers={[failedHosted]} />);
    expect(line()).toBeNull();
  });

  it("is absent when any hosted worker is not upgrade_failed", () => {
    render(<HealthPlatformLine isAdmin={false} runs={[queuedRun]} workers={[failedHosted, okHosted]} />);
    expect(line()).toBeNull();
  });

  it("is absent when the viewer owns no hosted worker", () => {
    render(<HealthPlatformLine isAdmin={false} runs={[queuedRun]} workers={[externalWorker]} />);
    expect(line()).toBeNull();
  });
});
