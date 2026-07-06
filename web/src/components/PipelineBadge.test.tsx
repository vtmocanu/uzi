// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { PipelineBadge } from "./PipelineBadge";
import type { PipelineStatus } from "../lib/api";

afterEach(cleanup);

function pipeline(over: Partial<PipelineStatus> = {}): PipelineStatus {
  return {
    status: "failed",
    web_url: "https://gitlab.example.com/g/r/-/pipelines/4242",
    pipeline_id: 4242,
    synced_at: new Date().toISOString(),
    ...over,
  };
}

describe("PipelineBadge", () => {
  it("links to the pipeline on the forge and labels the status", () => {
    render(<PipelineBadge pipeline={pipeline({ status: "failed" })} />);
    const link = screen.getByRole("link", { name: /CI failed/i });
    expect(link.getAttribute("href")).toBe("https://gitlab.example.com/g/r/-/pipelines/4242");
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.textContent).toContain("CI failed");
  });

  it("renders GitLab 'success' as the friendlier 'passed'", () => {
    render(<PipelineBadge pipeline={pipeline({ status: "success" })} />);
    // getByRole throws if the accessible-named link is absent, so this is the assertion.
    expect(screen.getByRole("link", { name: /CI passed/i }).textContent).toContain("CI passed");
  });

  it("shows the exact status and sync staleness on hover", () => {
    const synced = new Date(Date.now() - 60_000).toISOString();
    render(<PipelineBadge pipeline={pipeline({ status: "running", synced_at: synced })} />);
    const link = screen.getByRole("link", { name: /CI running/i });
    expect(link.getAttribute("title")).toMatch(/^Pipeline running · synced .*ago$/);
  });
});
