// @vitest-environment jsdom
//
// LastRun (PRD #308 / PRD #1093 M4): the "Last fire" detail panel. A pause-all fire
// (its only skip carries the schedules_paused reason) renders ONE explanatory row instead
// of a per-candidate list; an ordinary skip fire still renders the candidate list, so the
// pause branch is discriminating in both directions.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { LastFireDetail } from "./LastRun";
import type { LastFire, LastFireSkip, Schedule } from "../lib/api";

// LastFireDetail reads useAuth().uziLabel for the cap hint; a minimal stub keeps it out of
// an AuthProvider.
vi.mock("../auth/AuthContext", () => ({ useAuth: () => ({ uziLabel: "uzi" }) }));

afterEach(cleanup);

function sched(over: Partial<Schedule> = {}): Schedule {
  return {
    id: "s1",
    repo_id: "repo-uzi",
    repo_path: "vtmocanu/uzi",
    target: "sweep",
    issue_iid: null,
    labels: ["bug"],
    prompt: "",
    timing: "recurring",
    cron_expr: "0 2 * * *",
    run_at: null,
    timezone: "UTC",
    next_fire_at: null,
    last_fired_at: null,
    last_fire: null,
    auto_approve: true,
    wait_on_limit: true,
    max_issues: 3,
    guidance: null,
    baked_guidance: null,
    model: null,
    output_mode: null,
    override_subagent_model: false,
    enabled: true,
    status: "active",
    origin: "user",
    catalog_slug: null,
    customized: false,
    sibling_group_id: null,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    next_fires: [],
    ...over,
  };
}

function fire(skips: LastFireSkip[]): LastFire {
  return {
    fired_at: "2026-09-02T02:00:00Z",
    matched: skips.length,
    capped: false,
    started: [],
    skips,
  };
}

function renderDetail(f: LastFire) {
  return render(
    <MemoryRouter>
      <LastFireDetail s={sched()} fire={f} />
    </MemoryRouter>,
  );
}

describe("LastFireDetail — schedules_paused fire (PRD #1093)", () => {
  it("renders the explanatory row instead of a candidate list for a pause-all fire", () => {
    renderDetail(fire([{ issue_iid: null, title: "", reason: "schedules_paused", web_url: null }]));
    // The explanatory copy is shown.
    expect(screen.getByText(/All your schedules were paused/)).toBeTruthy();
    expect(screen.getByText(/nothing replays on resume/)).toBeTruthy();
    // The per-candidate skip label ("all schedules paused") is NOT rendered as a row badge —
    // the pause fire replaces the candidate list entirely.
    expect(screen.queryByText("all schedules paused")).toBeNull();
  });

  it("still renders the per-candidate list for an ordinary (non-pause) skip fire (control)", () => {
    renderDetail(
      fire([{ issue_iid: 42, title: "some issue", reason: "not_eligible", web_url: null }]),
    );
    // The ordinary candidate row + its typed reason label render...
    expect(screen.getByText("some issue")).toBeTruthy();
    expect(screen.getByText("not eligible")).toBeTruthy();
    // ...and the pause explanatory copy does NOT appear.
    expect(screen.queryByText(/All your schedules were paused/)).toBeNull();
  });
});
