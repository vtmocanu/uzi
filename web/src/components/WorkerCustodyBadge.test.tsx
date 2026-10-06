// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import type { AdminWorker, Worker } from "../lib/api";
import { mockWorkers } from "../mocks/data/workers";
import { WorkerCustodyBadge } from "./WorkerCustodyBadge";

afterEach(cleanup);

describe("WorkerCustodyBadge", () => {
  it.each([1, 3])("renders when %i custody decisions need the owner", (count) => {
    render(<WorkerCustodyBadge worker={{ custody_decisions_needed: count }} />);
    const pill = screen.getByText("retaining work");
    expect(pill.getAttribute("title")).toBe(
      "Retaining unpublished committed work that needs an owner decision.",
    );
  });

  it.each([0, undefined])("renders nothing for count %s even with the old retention flag", (count) => {
    const worker: Worker = {
      ...mockWorkers[0],
      retaining_unpublished_work: true,
      custody_decisions_needed: count,
    };
    const { container } = render(<WorkerCustodyBadge worker={worker} />);
    expect(container.innerHTML).toBe("");
  });

  it("accepts owner and admin worker payloads", () => {
    const owner: Worker = { ...mockWorkers[0], custody_decisions_needed: 1 };
    const admin: AdminWorker = {
      ...owner, owner_email: "owner@example.com", disk_pressure_volumes: [], cleanup_pending: false,
    };
    render(<><WorkerCustodyBadge worker={owner} /><WorkerCustodyBadge worker={admin} /></>);
    expect(screen.getAllByText("retaining work")).toHaveLength(2);
  });
});
