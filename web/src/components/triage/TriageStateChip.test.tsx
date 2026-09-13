// @vitest-environment jsdom
//
// TriageStateChip is the one triage ladder for all three surfaces, so these cover each rung's
// copy AND the two adapters (judgeState / findingState) that feed it — the adapters are what
// let the judge wire (`todo`, per-occurrence set_via) and the finding wire (`to_file`,
// dismiss_reason) render one chip. The filed link's https guard is asserted on the href, the
// sink isHttpsUrl exists for.
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { TriageStateChip } from "./TriageStateChip";
import { findingState, judgeState } from "./triageCopy";
import type { Disposition, JudgeOccurrence } from "../../lib/api";

afterEach(cleanup);

function occ(over: Partial<JudgeOccurrence> = {}): JudgeOccurrence {
  return {
    run_id: "run-a",
    run_title: "run a",
    review_id: "rev-1",
    rec_id: "rec-a",
    verdict: "issues",
    confidence: "",
    bucket: "todo",
    ...over,
  };
}

describe("TriageStateChip — the ladder copy", () => {
  it("renders the open rung as 'To triage'", () => {
    render(<TriageStateChip state="to_triage" />);
    expect(screen.getByText("To triage")).toBeTruthy();
  });

  it("links a filed issue only when its URL is https, carrying #N and the safe rel", () => {
    render(<TriageStateChip state="filed" filed={{ issue_iid: 1180, issue_url: "https://forge.example/x/-/issues/1180" }} />);
    const link = screen.getByRole("link", { name: /Filed #1180/ });
    expect(link.getAttribute("href")).toBe("https://forge.example/x/-/issues/1180");
    expect(link.getAttribute("rel")).toBe("noopener noreferrer");
  });

  it("renders a non-https filed URL as inert text, never as an anchor", () => {
    const { container } = render(
      <TriageStateChip state="filed" filed={{ issue_iid: 1180, issue_url: "javascript:alert(1)" }} />,
    );
    // THE SECURITY PROPERTY: no anchor was rendered, so nothing carries the scheme.
    expect(container.querySelector("a")).toBeNull();
    // ...and the coordinate is still identified.
    expect(screen.getByText("Filed #1180")).toBeTruthy();
  });

  it("renders a bare 'Filed' when no issue ref is carried", () => {
    render(<TriageStateChip state="filed" />);
    expect(screen.getByText("Filed")).toBeTruthy();
  });

  it("renders a plain '✓ Done' for a human done (no set_via)", () => {
    render(<TriageStateChip state="done" />);
    expect(screen.getByText("Done")).toBeTruthy();
    // No automatic-close explanation on a human done.
    expect(screen.queryByTitle(/Marked done automatically/)).toBeNull();
  });

  it("renders 'Done via #N' with an explanatory title when the issue-close sync did it", () => {
    render(<TriageStateChip state="done" setVia="issue_close" filed={{ issue_iid: 71, issue_url: "https://f/71" }} />);
    expect(screen.getByText("Done via #71")).toBeTruthy();
    // The distinguishing claim lives on the attribute, asserted there — not in the visible label.
    expect(screen.getByTitle("Marked done automatically when the filed issue was closed")).toBeTruthy();
  });

  it("renders 'Dismissed · Won't do' for a wont_do", () => {
    render(<TriageStateChip state="dismissed" reason="wont_do" />);
    expect(screen.getByText("Dismissed · Won't do")).toBeTruthy();
  });

  it("renders 'Dismissed · Not an issue' for a false positive", () => {
    render(<TriageStateChip state="dismissed" reason="not_an_issue" />);
    expect(screen.getByText("Dismissed · Not an issue")).toBeTruthy();
  });

  it("renders 'Dismissed · barred CLI' with its policy title for the system auto-dismissal", () => {
    render(<TriageStateChip state="dismissed" reason="barred_cli" />);
    expect(screen.getByText("Dismissed · barred CLI")).toBeTruthy();
    expect(screen.getByTitle(/policy permanently bars/)).toBeTruthy();
  });

  it("renders a bare 'Dismissed' when no reason is carried", () => {
    render(<TriageStateChip state="dismissed" />);
    expect(screen.getByText("Dismissed")).toBeTruthy();
  });
});

describe("TriageStateChip — fed by judgeState (occurrence)", () => {
  it("maps a todo occurrence to the open chip", () => {
    render(<TriageStateChip {...judgeState(occ({ bucket: "todo" }))} />);
    expect(screen.getByText("To triage")).toBeTruthy();
  });

  it("maps a filed occurrence to a linked Filed chip", () => {
    render(
      <TriageStateChip
        {...judgeState(occ({ bucket: "filed", filed_issue: { issue_iid: 9, issue_url: "https://f/9", filed_at: "2026-01-01T00:00:00Z" } }))}
      />,
    );
    expect(screen.getByRole("link", { name: /Filed #9/ })).toBeTruthy();
  });

  it("maps a done-via-issue-close occurrence to 'Done via #N'", () => {
    render(
      <TriageStateChip
        {...judgeState(occ({ bucket: "done", set_via: "issue_close", filed_issue: { issue_iid: 12, issue_url: "https://f/12", filed_at: "2026-01-01T00:00:00Z" } }))}
      />,
    );
    expect(screen.getByText("Done via #12")).toBeTruthy();
  });

  it("maps a denied_cli dismissed occurrence to the barred-CLI chip, and a plain one to bare Dismissed", () => {
    const { unmount } = render(<TriageStateChip {...judgeState(occ({ bucket: "dismissed", set_via: "denied_cli" }))} />);
    expect(screen.getByText("Dismissed · barred CLI")).toBeTruthy();
    unmount();
    render(<TriageStateChip {...judgeState(occ({ bucket: "dismissed" }))} />);
    expect(screen.getByText("Dismissed")).toBeTruthy();
  });

  // PRD #1184 M4: an admin cross-user Mark done. With no filed iid on the aggregate occurrence it
  // reads the bare "Done by an admin", carrying the admin-provenance title.
  it("maps an admin done occurrence to 'Done by an admin'", () => {
    render(<TriageStateChip {...judgeState(occ({ bucket: "done", set_via: "admin" }))} />);
    expect(screen.getByText("Done by an admin")).toBeTruthy();
    expect(screen.getByTitle("Marked done by an admin across every user's runs")).toBeTruthy();
  });
});

describe("TriageStateChip — fed by judgeState (disposition)", () => {
  function disp(over: Partial<Disposition> = {}): Disposition {
    return { category: "improve_uzi", target: "t", status: "dismissed", reason: "wont_do", set_at: "2026-01-01T00:00:00Z", stale: false, ...over };
  }

  it("maps a done disposition to a plain Done", () => {
    render(<TriageStateChip {...judgeState(disp({ status: "done", reason: "" }))} />);
    expect(screen.getByText("Done")).toBeTruthy();
  });

  it("maps a not_an_issue disposition to the false-positive chip", () => {
    render(<TriageStateChip {...judgeState(disp({ status: "dismissed", reason: "not_an_issue" }))} />);
    expect(screen.getByText("Dismissed · Not an issue")).toBeTruthy();
  });

  it("maps an empty-reason dismissed disposition to Won't do (the run page's precedence)", () => {
    render(<TriageStateChip {...judgeState(disp({ status: "dismissed", reason: "" }))} />);
    expect(screen.getByText("Dismissed · Won't do")).toBeTruthy();
  });

  // PRD #1184 M4: the run-page disposition now carries set_via, so an admin cross-user done reads
  // "Done by an admin" on the run page too — the same label the Judge occurrence shows.
  it("maps an admin done disposition to 'Done by an admin' (the run-page path)", () => {
    render(<TriageStateChip {...judgeState(disp({ status: "done", reason: "", set_via: "admin" }))} />);
    expect(screen.getByText("Done by an admin")).toBeTruthy();
  });

  it("maps an issue_close done disposition to 'Done via issue close' (no iid on the run page)", () => {
    render(<TriageStateChip {...judgeState(disp({ status: "done", reason: "", set_via: "issue_close" }))} />);
    expect(screen.getByText("Done via issue close")).toBeTruthy();
  });
});

describe("TriageStateChip — fed by findingState (the to_file wire)", () => {
  it("maps the finding open key 'to_file' to the open chip", () => {
    render(<TriageStateChip {...findingState({ status: "to_file" })} />);
    expect(screen.getByText("To triage")).toBeTruthy();
  });

  it("maps a filed finding to a linked Filed chip from filed_issue_iid/url", () => {
    render(<TriageStateChip {...findingState({ status: "filed", filed_issue_iid: 5, filed_issue_url: "https://f/5" })} />);
    expect(screen.getByRole("link", { name: /Filed #5/ })).toBeTruthy();
  });

  it("maps a done finding to 'Done via #N' (a finding's done is always the issue-close sync)", () => {
    render(<TriageStateChip {...findingState({ status: "done", filed_issue_iid: 5, filed_issue_url: "https://f/5" })} />);
    expect(screen.getByText("Done via #5")).toBeTruthy();
  });

  it("maps a dismissed finding to its reason", () => {
    render(<TriageStateChip {...findingState({ status: "dismissed", dismiss_reason: "not_an_issue" })} />);
    expect(screen.getByText("Dismissed · Not an issue")).toBeTruthy();
  });
});
