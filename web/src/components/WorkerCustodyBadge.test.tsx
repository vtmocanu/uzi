// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { WorkerCustodyBadge } from "./WorkerCustodyBadge";

afterEach(cleanup);

// Regression pin for PRD #1296 M4/M5 E25 (D4): the "retaining work" pill marks a worker
// whose teardown is deferred because it holds unpublished committed work. It must appear
// ONLY on an open custody hold, and vanish (not fall back to some other pill) when the
// field is false or absent on the wire.

describe("WorkerCustodyBadge", () => {
  it("renders the 'retaining work' pill when the worker holds an open custody hold", () => {
    render(<WorkerCustodyBadge worker={{ retaining_unpublished_work: true }} />);
    const pill = screen.getByText("retaining work");
    expect(pill).toBeTruthy();
    // The title copy is the operator's only explanation of why teardown is deferred; pin it
    // verbatim so a wording drift is caught (jsdom reads the title attribute a visual pass
    // never can).
    expect(pill.getAttribute("title")).toBe(
      "Retaining unpublished committed work for durable recovery. Teardown is deferred until the work is archived or explicitly discarded; this holds no run slot.",
    );
  });

  it("renders nothing when retaining_unpublished_work is false", () => {
    const { container } = render(<WorkerCustodyBadge worker={{ retaining_unpublished_work: false }} />);
    expect(container.innerHTML).toBe("");
  });

  it("renders nothing when the field is absent (older payload / mock)", () => {
    const { container } = render(<WorkerCustodyBadge worker={{}} />);
    expect(container.innerHTML).toBe("");
  });
});
