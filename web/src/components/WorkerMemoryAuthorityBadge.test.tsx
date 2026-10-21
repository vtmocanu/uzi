// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { AdminWorkerResources } from "./AdminWorkerResources";
import { WorkerMemoryAuthorityBadge } from "./WorkerMemoryAuthorityBadge";
import { mockAdminWorkers, mockWorkers } from "../mocks/data/workers";

afterEach(cleanup);

const NOW = Date.parse("2026-10-06T12:00:00Z");
const LOST = "2026-10-06T09:46:00Z"; // 2h 14m before NOW

describe("WorkerMemoryAuthorityBadge", () => {
  it("renders the word, with the age, the scope of the gate and the remedy in its title", () => {
    render(<WorkerMemoryAuthorityBadge nowMs={NOW} worker={{ memory_authority_lost_at: LOST }} />);
    const pill = screen.getByText("memory authority lost");
    const title = pill.getAttribute("title") ?? "";
    expect(title).toMatch(/^Guard-bound runs withheld for 2h 14m: /);
    expect(title).toContain("did not report memory authority");
    expect(title).toContain("Judge, job, cross-check, review and isolated-fetch runs still run.");
    expect(title).toContain("Restart the worker's container to regain it.");
  });

  it("repeats the explanation as screen-reader text", () => {
    const { container } = render(<WorkerMemoryAuthorityBadge nowMs={NOW} worker={{ memory_authority_lost_at: LOST }} />);
    expect(container.querySelector(".sr-only")?.textContent).toContain("Restart the worker's container to regain it.");
  });

  it.each([null, undefined])("renders nothing when memory_authority_lost_at is %s", (at) => {
    const { container } = render(<WorkerMemoryAuthorityBadge worker={{ memory_authority_lost_at: at }} />);
    expect(container.innerHTML).toBe("");
  });
});

describe("AdminWorkerResources — memory authority", () => {
  it("shows the badge for a gated admin worker, and none otherwise", () => {
    const gated = mockAdminWorkers.find((w) => w.memory_authority_lost_at);
    const clear = mockAdminWorkers.find((w) => !w.memory_authority_lost_at);
    expect(gated && clear).toBeTruthy();
    const { unmount } = render(<AdminWorkerResources worker={gated!} diskOnly />);
    expect(screen.getByText("memory authority lost")).toBeTruthy();
    unmount();
    render(<AdminWorkerResources worker={clear!} />);
    expect(screen.queryByText("memory authority lost")).toBeNull();
  });
});

describe("mock workers — memory authority", () => {
  it.each([
    ["owner", mockWorkers],
    ["admin", mockAdminWorkers],
  ] as const)("the %s list carries exactly one gated worker, distinct from the quarantined one", (_scope, workers) => {
    const gated = workers.filter((w) => w.memory_authority_lost_at);
    expect(gated.map((w) => w.id)).toEqual(["w-ci"]);
    expect(gated[0].residue_quarantined_at).toBeFalsy();
    expect(workers.filter((w) => w.residue_quarantined_at).map((w) => w.id)).not.toContain("w-ci");
  });
});
