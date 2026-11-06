// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { RepoAgentSourceSetting, REPO_AGENTS_PLANNING_WARNING } from "./RepoAgentSourceSetting";

afterEach(cleanup);

describe("RepoAgentSourceSetting", () => {
  it("offers the three choices and maps not set to null", () => {
    const onChange = vi.fn();
    render(<RepoAgentSourceSetting value={null} onChange={onChange} />);
    const select = screen.getByLabelText("Default agents for my runs on this repo") as HTMLSelectElement;
    expect([...select.options].map((o) => o.textContent)).toEqual([
      "uzi templates",
      "repo agents",
      "not set (today’s behaviour)",
    ]);
    expect(select.value).toBe("");
    fireEvent.change(select, { target: { value: "own" } });
    fireEvent.change(select, { target: { value: "repo" } });
    fireEvent.change(select, { target: { value: "" } });
    expect(onChange.mock.calls).toEqual([["own"], ["repo"], [null]]);
  });

  it("shows the planning warning only for repo agents", () => {
    const { rerender } = render(<RepoAgentSourceSetting value="own" onChange={() => {}} />);
    expect(screen.queryByText(REPO_AGENTS_PLANNING_WARNING)).toBeNull();
    rerender(<RepoAgentSourceSetting value={undefined} onChange={() => {}} />);
    expect(screen.queryByText(REPO_AGENTS_PLANNING_WARNING)).toBeNull();
    rerender(<RepoAgentSourceSetting value="repo" onChange={() => {}} />);
    expect(screen.getByText(REPO_AGENTS_PLANNING_WARNING)).toBeTruthy();
  });
});
