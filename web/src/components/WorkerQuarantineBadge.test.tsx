// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { AdminWorkerResources } from "./AdminWorkerResources";
import { WorkerQuarantineBadge } from "./WorkerQuarantineBadge";
import { mockAdminWorkers, mockWorkers } from "../mocks/data/workers";

afterEach(cleanup);

const NOW = Date.parse("2026-10-06T12:00:00Z");
const LATCHED = "2026-10-06T09:46:00Z"; // 2h 14m before NOW
// Markup plus an ANSI-ish fragment: it must reach the screen as these exact characters.
const HOSTILE = '<script>alert("x")</script><img src=x onerror=alert(1)> [31mcomm=??[0m';

describe("WorkerQuarantineBadge", () => {
  it("renders the word, with the age, the cause and the remedy in its title", () => {
    render(
      <WorkerQuarantineBadge
        nowMs={NOW}
        worker={{ residue_quarantined_at: LATCHED, residue_quarantine_cause: "pid 4242 unreadable" }}
      />,
    );
    const pill = screen.getByText("quarantined");
    const title = pill.getAttribute("title") ?? "";
    expect(title).toMatch(/^Quarantined 2h 14m ago: /);
    expect(title).toContain('Reported cause: "pid 4242 unreadable".');
    expect(title).toContain("Restart the worker's container to clear it.");
  });

  it.each([null, undefined])("renders nothing when residue_quarantined_at is %s", (at) => {
    const { container } = render(
      <WorkerQuarantineBadge worker={{ residue_quarantined_at: at, residue_quarantine_cause: "stale cause" }} />,
    );
    expect(container.innerHTML).toBe("");
  });

  it("says no cause was reported when the cause is null", () => {
    render(<WorkerQuarantineBadge nowMs={NOW} worker={{ residue_quarantined_at: LATCHED, residue_quarantine_cause: null }} />);
    expect(screen.getByText("quarantined").getAttribute("title")).toContain("No cause was reported.");
  });

  it("renders a hostile cause as literal text, never as markup", () => {
    const { container } = render(
      <WorkerQuarantineBadge nowMs={NOW} worker={{ residue_quarantined_at: LATCHED, residue_quarantine_cause: HOSTILE }} />,
    );
    expect(container.querySelector("script, img")).toBeNull();
    expect(screen.getByText("quarantined").getAttribute("title")).toContain(`Reported cause: "${HOSTILE}".`);
    // The same sentence reaches screen readers as text, not only through the title.
    expect(container.textContent).toContain(HOSTILE);
  });

  it("strips control and format characters out of the cause", () => {
    render(
      <WorkerQuarantineBadge
        nowMs={NOW}
        worker={{ residue_quarantined_at: LATCHED, residue_quarantine_cause: "evil\u202Eexe\u001b[2J\u200Bdone" }}
      />,
    );
    const title = screen.getByText("quarantined").getAttribute("title") ?? "";
    expect(title).toContain('Reported cause: "evilexe[2Jdone".');
    expect(title).not.toMatch(/[\p{Cc}\p{Cf}]/u);
  });
});

describe("AdminWorkerResources — quarantine", () => {
  it("shows the quarantine badge for a latched admin worker, and none otherwise", () => {
    const latched = mockAdminWorkers.find((w) => w.residue_quarantined_at);
    const clear = mockAdminWorkers.find((w) => !w.residue_quarantined_at);
    expect(latched && clear).toBeTruthy();
    const { unmount } = render(<AdminWorkerResources worker={latched!} diskOnly />);
    expect(screen.getByText("quarantined")).toBeTruthy();
    unmount();
    render(<AdminWorkerResources worker={clear!} />);
    expect(screen.queryByText("quarantined")).toBeNull();
  });
});

describe("mock workers — quarantine", () => {
  it.each([
    ["owner", mockWorkers],
    ["admin", mockAdminWorkers],
  ] as const)("the %s list carries exactly one latched worker with a hostile cause", (_scope, workers) => {
    const latched = workers.filter((w) => w.residue_quarantined_at);
    expect(latched.map((w) => w.id)).toEqual(["w-nas"]);
    expect(latched[0].residue_quarantine_cause).toContain("<script>");
  });
});
