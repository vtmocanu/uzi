// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { JobResultPanel } from "./JobResultPanel";
import { RunCompletedLine } from "../RunView";
import { SteerQueueCard } from "../../components/SteerQueueCard";
import type { Run, RunJob } from "../../lib/api";
import { extendEnabled } from "../../lib/budget";
import { isCredentialSwitchRefusedLane } from "../../lib/credentialOverride";
import { mockJobRuns, mockRuns } from "../../mocks/data";

// PRD #1908 M7 (D-D): the job run's result panel. Every free-text member is untrusted, so
// these tests pin the inert rendering: the report goes through the hardened <Markdown> (raw
// HTML inert, javascript: links neutralized), findings are plain text, only an http(s) URL
// becomes a link, and the product-supplied label only ever appears attributed to its product.
// The completed fixture (mocks/data/jobs.ts) is the hostile one mock mode shows.

const doneJob = mockJobRuns.find((r) => r.id === "run-job-done")!;
const queuedJob = mockJobRuns.find((r) => r.id === "run-job-queued")!;
const issueRun = mockRuns.find((r) => r.id === "run-queued")!;

function withJob(run: Run, job: Partial<RunJob>): Run {
  return { ...run, job: { ...run.job!, ...job } };
}

afterEach(cleanup);

describe("JobResultPanel (PRD #1908)", () => {
  it("renders the report as sanitized markdown: formatting works, HTML and javascript: links stay inert", () => {
    const { container } = render(<JobResultPanel run={doneJob} />);
    const report = screen.getByTestId("job-report");
    // Positive: markdown IS rendered (a heading and a safe link).
    expect(within(report).getByRole("heading", { name: "Summary" })).toBeTruthy();
    const safe = within(report).getByRole("link", { name: "change log" });
    expect(safe.getAttribute("href")).toBe("https://example.com/changelog");
    expect(safe.getAttribute("rel")).toBe("noopener noreferrer");
    // The javascript: link keeps its text but loses its href.
    const neutral = within(report).getByText("Open the admin panel");
    expect(neutral.closest("a")?.getAttribute("href") ?? null).toBeNull();
    // Raw HTML is literal text, not elements: the text is present AND no element exists.
    expect(report.textContent).toContain("<script>alert('report')</script>");
    expect(report.textContent).toContain("<img src=x");
    expect(report.querySelector("img")).toBeNull();
    expect(container.querySelector("script")).toBeNull();
  });

  it("renders findings as inert text with their severity, and only an http(s) URL as a link", () => {
    render(<JobResultPanel run={doneJob} />);
    const list = screen.getByTestId("job-findings");
    const items = within(list).getAllByRole("listitem");
    expect(items.map((li) => within(li).getAllByText(/^(Error|Warning|Info)$/)[0].textContent)).toEqual([
      "Error",
      "Warning",
      "Info",
    ]);
    expect(screen.getByText("1 error, 1 warning, 1 note")).toBeTruthy();

    // file:line is plain code text.
    const loc = within(items[0]).getByText("legacy-rollback.md:42");
    expect(loc.tagName).toBe("CODE");

    // https URL: a real external link with the repo's rel/target convention.
    const link = within(items[1]).getByRole("link", { name: "https://example.com/dashboards/rollback" });
    expect(link.getAttribute("href")).toBe("https://example.com/dashboards/rollback");
    expect(link.getAttribute("rel")).toBe("noopener noreferrer");
    expect(link.getAttribute("target")).toBe("_blank");

    // javascript: URL: shown as text, never an anchor.
    const js = within(items[2]).getByText("javascript:alert(document.cookie)");
    expect(js.closest("a")).toBeNull();
    // The message's markdown/HTML is literal text: the link syntax is not parsed either.
    expect(items[2].textContent).toContain("<script>alert('finding')</script> [click](javascript:alert(1))");
    expect(within(items[2]).queryByRole("link")).toBeNull();
    expect(items[2].querySelector("script")).toBeNull();
  });

  it("renders a data: or relative finding URL as text too", () => {
    const run = withJob(doneJob, {
      result: {
        status: "completed",
        report_md: "",
        findings: [
          { severity: "warning", message_md: "a", url: "data:text/html,<b>x</b>", file: null, line: null },
          { severity: "bogus", message_md: "b", url: "/relative/path", file: null, line: null },
        ],
      },
    });
    render(<JobResultPanel run={run} />);
    const list = screen.getByTestId("job-findings");
    expect(within(list).getByText("data:text/html,<b>x</b>").closest("a")).toBeNull();
    expect(within(list).getByText("/relative/path").closest("a")).toBeNull();
    // An unknown severity renders its (stripped) raw value in a neutral badge.
    expect(within(list).getByText("bogus")).toBeTruthy();
    expect(screen.getByText("The job returned no report text.")).toBeTruthy();
  });

  it("shows the product-supplied label only as 'reported by' the product, stripped of bidi and markup", () => {
    render(<JobResultPanel run={doneJob} />);
    const origin = screen.getByTestId("job-origin");
    expect(origin.textContent).toContain("Requested by");
    expect(origin.textContent).toContain("as reported by Helpdesk assistant.");
    expect(origin.textContent).toContain("uzi has not verified this.");
    const label = origin.querySelector("q")!;
    // RLO (U+202E) stripped; the markup is text, not a <b> element.
    expect(label.textContent).toBe("opstsil-kcehc <b>admin</b> (verified by uzi)");
    expect(label.textContent).not.toContain("‮");
    expect(origin.querySelector("b")).toBeNull();
  });

  it("attributes a label with no product to the caller, and a label-less job to its product or CLI token", () => {
    const { rerender } = render(
      <JobResultPanel run={withJob(doneJob, { origin: { requested_by_label: "kim", product_name: null } })} />,
    );
    expect(screen.getByTestId("job-origin").textContent).toContain("as reported by the caller.");
    rerender(
      <JobResultPanel run={withJob(doneJob, { origin: { requested_by_label: null, product_name: "Helpdesk assistant" } })} />,
    );
    expect(screen.getByText(/Created through/).textContent).toBe("Created through Helpdesk assistant.");
    rerender(<JobResultPanel run={withJob(doneJob, { origin: { requested_by_label: null, product_name: null } })} />);
    expect(screen.getByText("Created with a CLI token.")).toBeTruthy();
  });

  it("lists inputs by name and size", () => {
    render(<JobResultPanel run={doneJob} />);
    const inputs = screen.getByRole("list", { name: "Inputs" });
    expect(within(inputs).getAllByRole("listitem").map((li) => li.textContent)).toEqual([
      "deploy.md6.0 KiB",
      "oncall.md3.8 KiB",
      "legacy-rollback.md1.2 MiB",
    ]);
  });

  it("before a result, says where it will appear; a finished job without one says so", () => {
    const { rerender } = render(<JobResultPanel run={queuedJob} />);
    expect(screen.getByText("The report and findings appear here when the job finishes.")).toBeTruthy();
    expect(screen.getByText("pager-export.csv").nextElementSibling?.textContent).toBe("412 B");
    rerender(<JobResultPanel run={{ ...queuedJob, status: "failed" }} />);
    expect(screen.getByText("This job ended without returning a result.")).toBeTruthy();
  });

  it("renders nothing for a non-job run", () => {
    const { container } = render(<JobResultPanel run={issueRun} />);
    expect(container.innerHTML).toBe("");
  });
});

describe("job run chrome (PRD #1908)", () => {
  it("the completed line points at the result instead of a branch or MR", () => {
    render(<RunCompletedLine run={doneJob} duration="7m" />);
    expect(screen.getByText(/Ran for 7m\./).textContent).toBe("Ran for 7m. Report and findings below.");
  });

  it("the steer card offers Stop but no follow-up composer on a job", () => {
    const noop = () => {};
    const { rerender } = render(
      <SteerQueueCard inputs={[]} terminal={false} status="running" busy={false} onStop={noop} onSend={noop} run={issueRun} />,
    );
    // Control: an issue run has the composer.
    expect(screen.getByRole("textbox")).toBeTruthy();
    rerender(
      <SteerQueueCard inputs={[]} terminal={false} status="running" busy={false} onStop={noop} onSend={noop} run={queuedJob} />,
    );
    expect(screen.queryByRole("textbox")).toBeNull();
    expect(screen.getByText("A job takes no follow-ups. Stopping it cancels the job.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Stop run" })).toBeTruthy();
  });

  it("a job is never extendable or credential-switchable, whatever its cap", () => {
    expect(extendEnabled(queuedJob)).toBe(false);
    expect(extendEnabled(issueRun)).toBe(true);
    expect(isCredentialSwitchRefusedLane(queuedJob)).toBe(true);
    expect(isCredentialSwitchRefusedLane(issueRun)).toBe(false);
  });
});
