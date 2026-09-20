// @vitest-environment jsdom
//
// Non-admin Overview platform line (PRD #1484 M5, D3/D4). A pure component over the viewer's
// own runs + workers, so the test drives it directly with props — no api, no provider. The
// line is selected by its SCOPED handle (role="status" + aria-label="Your hosted workers"),
// never a bare getByRole("status").
//
// Covered: appears only when all three conditions hold; absent for an admin (admins get the
// card); absent when any single condition fails. The cause clause is DERIVED through
// likelyCause (WorkerUpgradeBadge) from the viewer's own upgrade_blocking_* — an ImagePullBackOff
// worker reads the image-pull cause, a seed-nix CrashLoopBackOff reads its OWN cause (not the
// image-pull string), proving the derivation rather than a hard-coded copy string.
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import { HealthPlatformLine } from "./HealthPlatformLine";
import type { RunListItem, Worker } from "../lib/api";

afterEach(cleanup);

// Minimal fixtures: the component reads run.status and worker.kind/upgrade_status, plus the
// upgrade_blocking_* the cause clause derives from. failedHosted carries the incident's
// ImagePullBackOff; seedNixHosted a DIFFERENT upgrade_failed reason.
const queuedRun = { status: "queued" } as unknown as RunListItem;
const runningRun = { status: "running" } as unknown as RunListItem;
const failedHosted = {
  kind: "hosted",
  upgrade_status: "upgrade_failed",
  upgrade_blocking_container: "worker",
  upgrade_blocking_reason: "ImagePullBackOff",
  upgrade_last_exit_code: null,
} as unknown as Worker;
const seedNixHosted = {
  kind: "hosted",
  upgrade_status: "upgrade_failed",
  upgrade_blocking_container: "seed-nix",
  upgrade_blocking_reason: "CrashLoopBackOff",
  upgrade_last_exit_code: null,
} as unknown as Worker;
const okHosted = { kind: "hosted", upgrade_status: "up_to_date" } as unknown as Worker;
const externalWorker = { kind: "external", upgrade_status: "unknown" } as unknown as Worker;

const line = () => screen.queryByRole("status", { name: "Your hosted workers" });

describe("HealthPlatformLine", () => {
  it("appears with the IMAGE-PULL cause when the fleet is stuck on ImagePullBackOff", () => {
    render(<HealthPlatformLine isAdmin={false} runs={[queuedRun]} workers={[failedHosted]} />);
    expect(line()).not.toBeNull();
    // The cause clause is likelyCause's image-pull sentence, derived from the worker's own
    // upgrade_blocking_reason — not the retired hard-coded "the worker image cannot be pulled".
    expect(screen.getByText(/The image could not be pulled/)).toBeTruthy();
    expect(screen.getByText(/This is a platform problem, not your run/)).toBeTruthy();
  });

  it("reflects a DIFFERENT likelyCause for a non-image-pull reason (seed-nix CrashLoopBackOff)", () => {
    render(<HealthPlatformLine isAdmin={false} runs={[queuedRun]} workers={[seedNixHosted]} />);
    expect(line()).not.toBeNull();
    // The copy carries the seed-nix cause, NOT the image-pull string — this fails against a
    // hard-coded image-pull copy and so proves the cause is derived per worker.
    expect(screen.getByText(/The nix store reseed is failing/)).toBeTruthy();
    expect(screen.queryByText(/The image could not be pulled/)).toBeNull();
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
