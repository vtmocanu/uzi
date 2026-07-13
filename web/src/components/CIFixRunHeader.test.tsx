// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { CIFixRunHeader } from "./CIFixRunHeader";
import type { Run } from "../lib/api";

afterEach(cleanup);

function run(over: Partial<Run> = {}): Run {
  return {
    id: "r1",
    repo_id: "repo-1",
    kind: "ci_fix",
    issue_iid: null,
    issue_title: "Fix CI",
    issue_description: "",
    title: null,
    resume_of_run_id: null,
    status: "completed",
    requeue_count: 0,
    iteration_count: 0,
    auto_approve: false,
    worker_id: null,
    branch: "ci-fix/pipeline-4200",
    mr_iid: 7,
    mr_state: null,
    failure_reason: null,
    stop_kind: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    pipeline_ref: "main",
    pipeline_web_url: "https://gitlab.example.com/g/r/-/pipelines/4200",
    fix_verdict: "verified",
    plan_md: null,
    repo_agents: null,
    agent_source: null,
    agent_exclusions: null,
    own_agents: null,
    claimed_at: null,
    started_at: null,
    finished_at: null,
    created_at: "2026-07-06T00:00:00Z",
    updated_at: "2026-07-06T00:00:00Z",
    ...over,
  };
}

describe("CIFixRunHeader", () => {
  it("links an https failing-pipeline URL and shows the verdict chip", () => {
    render(<CIFixRunHeader run={run({ fix_verdict: "verified" })} terminal />);
    expect(screen.getByRole("link", { name: /failing pipeline/i }).getAttribute("href")).toBe(
      "https://gitlab.example.com/g/r/-/pipelines/4200",
    );
    expect(screen.getByText(/verified/i)).toBeTruthy();
  });

  it("does NOT link a non-https pipeline_web_url (forge-URL scheme guard)", () => {
    render(<CIFixRunHeader run={run({ pipeline_web_url: "javascript:alert(1)" })} terminal />);
    expect(screen.queryByRole("link")).toBeNull();
    // Still shown, just as plain text.
    expect(screen.getByText(/failing pipeline/i)).toBeTruthy();
  });

  it("renders nothing for an issue run", () => {
    const { container } = render(<CIFixRunHeader run={run({ kind: "issue", issue_iid: 5 })} terminal />);
    expect(container.textContent).toBe("");
  });

  it("shows no verdict chip while the run is still working", () => {
    render(<CIFixRunHeader run={run({ status: "running", fix_verdict: null })} terminal={false} />);
    expect(screen.queryByText(/verified|unverified|fix failed|not a code/i)).toBeNull();
  });
});
