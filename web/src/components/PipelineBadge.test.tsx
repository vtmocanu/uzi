// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { FixCiButton, PipelineBadge } from "./PipelineBadge";
import type { PipelineStatus } from "../lib/api";

afterEach(cleanup);

function pipeline(over: Partial<PipelineStatus> = {}): PipelineStatus {
  return {
    ref: "main",
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

  it("does NOT link a non-https web_url (forge-URL scheme guard) — renders a plain pill", () => {
    render(<PipelineBadge pipeline={pipeline({ status: "failed", web_url: "javascript:alert(1)" })} />);
    expect(screen.queryByRole("link")).toBeNull();
    // The status is still shown, just not as an anchor.
    expect(screen.getByText(/CI failed/i)).toBeTruthy();
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

describe("FixCiButton", () => {
  it("renders only for a failed pipeline and fires onClick", () => {
    let clicked = 0;
    const { rerender } = render(
      <FixCiButton pipeline={pipeline({ status: "failed" })} busy={false} onClick={() => (clicked += 1)} />,
    );
    const btn = screen.getByRole("button", { name: /fix ci/i });
    btn.click();
    expect(clicked).toBe(1);

    // A passing pipeline shows no Fix CI affordance.
    rerender(<FixCiButton pipeline={pipeline({ status: "success" })} busy={false} onClick={() => {}} />);
    expect(screen.queryByRole("button", { name: /fix ci/i })).toBeNull();
  });

  it("disables while busy", () => {
    render(<FixCiButton pipeline={pipeline({ status: "failed" })} busy={true} onClick={() => {}} />);
    expect((screen.getByRole("button", { name: /starting/i }) as HTMLButtonElement).disabled).toBe(true);
  });
});
