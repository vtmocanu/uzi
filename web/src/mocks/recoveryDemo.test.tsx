// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { mockApi } from "./mockApi";
import { state } from "./store";
import { api } from "../lib/api";
import { recoveryExportCommand } from "../lib/recovery";
import { RecoveryArchivesPanel } from "../components/RecoveryArchives";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

it("keeps normal failed-run recovery exports executable without relaxing UUID guards", async () => {
  const failed = [...state.runs.values()].filter(run => run.status === "failed");
  expect(failed.length).toBeGreaterThan(2);
  // One request per seeded failed run, bounded by the finite default fixture set.
  // A missing or invalid run fails this regression instead of skipping a sibling.
  for (const fixture of failed) {
    const { run } = await mockApi.getRun(fixture.id);
    const summary = await mockApi.getRunArchives(run.id);
    expect(summary.archives).toHaveLength(1);
    const capture = summary.archives[0];
    expect(capture.run_id).toBe(run.id);
    expect(recoveryExportCommand(run.id, capture.id)).toBe(
      `uzi run export ${run.id} --output <path> --capture ${capture.id}`,
    );
  }
});

it("renders the normal board-to-run recovery hint with the exact fixture UUIDs", async () => {
  const { board } = await mockApi.getBoard("repo-atlas");
  const card = board.cards.find(card => card.iid === 7)!;
  expect(card.latest_run).toBeTruthy();
  const { run } = await mockApi.getRun(card.latest_run!.id);
  expect(run.landing_state).toBe("needs_landing");
  expect((await mockApi.getRunMessages(run.id)).messages.length).toBeGreaterThan(0);
  const summary = await mockApi.getRunArchives(run.id);
  const capture = summary.archives[0];
  vi.spyOn(api, "getRunArchives").mockResolvedValue(summary);
  render(<RecoveryArchivesPanel run={run} />);
  const command = `uzi run export ${run.id} --output <path> --capture ${capture.id}`;
  expect(await screen.findByText(command)).toBeTruthy();
  expect(screen.getByRole("button", { name: "Copy" })).toBeTruthy();
  expect(screen.getByRole("link", { name: "Export archive" })).toBeTruthy();
  expect(screen.queryByText(/Export command unavailable/)).toBeNull();
  expect(recoveryExportCommand("run-closed", capture.id)).toBeNull();
});

it("keeps the closed-MR board route, transcript and review attached to the same UUID run", async () => {
  const { board } = await mockApi.getBoard("repo-uzi");
  const card = board.cards.find(card => card.latest_run?.mr_state === "closed")!;
  expect(card).toBeTruthy();
  const { run } = await mockApi.getRun(card.latest_run!.id);
  expect(run.id).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
  expect((await mockApi.getRunMessages(run.id)).messages.length).toBeGreaterThan(0);
  expect((await mockApi.getRunReview(run.id)).review?.target_run_id).toBe(run.id);
});
