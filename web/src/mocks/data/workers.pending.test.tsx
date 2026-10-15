// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { WorkerPendingOutcomeBadge } from "../../components/WorkerPendingOutcomeBadge";
import { mockAdminWorkers, mockWorkers } from "./workers";

afterEach(cleanup);

describe("mock worker pending outcomes issue 2009", () => {
  it.each([
    ["owner", mockWorkers],
    ["admin", mockAdminWorkers],
  ] as const)("renders confirmed but incomplete evidence without invented rows in %s", (_scope, workers) => {
    const worker = workers.find((w) => w.id === "w-laptop");
    expect(worker).toBeDefined();
    expect(worker).toMatchObject({ pending_outcome_count: 1, pending_overflow: true });
    expect(worker?.reported_runs).toBeUndefined();
    const { container } = render(<WorkerPendingOutcomeBadge worker={worker!} />);
    expect(screen.getByText("1 confirmed pending; more may be pending").getAttribute("title"))
      .toBe("Outcome journaled on this worker but not yet delivered to the api.\n");
    expect(container.querySelector("a")).toBeNull();
  });

  it("keeps partial legacy data without aggregate evidence", () => {
    const worker = mockWorkers.find((w) => w.id === "w-ci");
    expect(worker).toBeDefined();
    expect(worker).not.toHaveProperty("pending_outcome_count");
    expect(worker).not.toHaveProperty("pending_overflow");
    const { container } = render(<WorkerPendingOutcomeBadge worker={worker!} />);
    expect(container.innerHTML).toBe("");
  });
});
