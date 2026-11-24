import "../../index.css";
import { afterEach, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { JudgePanel } from "./JudgePanel";
import { api, type JudgeSkip, type Run } from "../../lib/api";

vi.mock("../../lib/api", async original => {
  const actual = await original<typeof import("../../lib/api")>();
  return { ...actual, api: { ...actual.api, getRunReview: vi.fn(), rerunJudge: vi.fn(), listRepos: vi.fn().mockResolvedValue({ repos: [] }) } };
});
afterEach(() => { cleanup(); vi.clearAllMocks(); });

const run = { id: "r-skip", kind: "issue", status: "completed" } as Run;
const AT = "2026-11-10T09:30:00Z";

const cases: [string, JudgeSkip, string][] = [
  ["claude historical lane", { harness: "claude", reason: "provider_lane_unsupported", at: AT },
    "the selected source was a model provider, which the judge did not support at the time."],
  ["codex current lane", { harness: "codex", reason: "provider_lane_unsupported", at: AT },
    "the selected Codex source is a model provider, which the Codex judge does not support yet."],
  ["source unusable", { harness: "claude", reason: "provider_source_unusable", at: AT },
    "the selected model provider could not serve the judge."],
];

it.each(cases)("renders the skip notice for %s", async (_name, skip, cause) => {
  vi.mocked(api.getRunReview).mockResolvedValue({ review: null, pending_judge: null, skip });
  render(<MemoryRouter><main className="p-4"><JudgePanel run={run} /></main></MemoryRouter>);
  const p = await screen.findByText((_, el) => el?.tagName === "P" && (el.textContent ?? "").startsWith("The judge was skipped"));
  expect(p.textContent?.endsWith(cause)).toBe(true);
  expect(p.getBoundingClientRect().width).toBeGreaterThan(0);
});
