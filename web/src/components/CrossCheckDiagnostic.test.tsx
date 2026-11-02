// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { cleanup, fireEvent, render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { RunMessage } from "../lib/api";
import { mockRuns } from "../mocks/data/runs";
import { ActivityFeed } from "./ActivityFeed";
import { describeStatus, RunEventRow } from "./RunEvent";

afterEach(cleanup);

beforeEach(() => {
  const values = new Map<string, string>();
  Object.defineProperty(window, "localStorage", {
    configurable: true,
    value: {
      getItem: (key: string) => values.get(key) ?? null,
      setItem: (key: string, value: string) => { values.set(key, value); },
      removeItem: (key: string) => { values.delete(key); },
    },
  });
});

// These are the fixed M1 producer pairs, repeated here deliberately: rendering tests
// consume persisted RunMessage payloads, without importing the worker's factory.
const diagnostics = [
  ["provider_error", "Cross-check incomplete: provider reported an error."],
  ["checker_error_result", "Cross-check incomplete: checker returned an unsuccessful result."],
  ["transport_ended", "Cross-check incomplete: checker event stream ended without a terminal result."],
  ["no_complete_verdict", "Cross-check incomplete: terminal result contained no complete verdict."],
  ["tool_budget_exceeded", "Cross-check incomplete: Codex checker exceeded its tool-call budget."],
  ["unknown_execution_error", "Cross-check incomplete: execution error; specific cause not recorded."],
] as const;

function status(seq: number, payload: unknown): RunMessage {
  return {
    seq, kind: "status", agent: "cross-checker", agent_instance: null,
    agent_label: null, payload, created_at: "2026-07-04T00:00:00.000Z",
  };
}

function diagnostic(seq: number, stage: string, harness: string, code: string, text: string) {
  return status(seq, { event: "cross_check_diagnostic", stage, harness, reason_class: "model_error", code, text });
}

function renderCheckerTrace(messages: RunMessage[], harness: "claude" | "codex" = "claude") {
  const r = render(
    <MemoryRouter>
      <ActivityFeed
        run={{ ...mockRuns[0], kind: "cross_check", harness, status: "failed" }}
        messages={messages} runningLive={false} connected={true} terminal={true}
      />
    </MemoryRouter>,
  );
  const expand = r.queryByRole("button", { name: "Expand all" });
  if (expand) fireEvent.click(expand);
  return r;
}

describe("persisted cross-check diagnostic status", () => {
  for (const stage of ["plan", "code"]) {
    for (const harness of ["claude", "codex"] as const) {
      describe(`${stage} / ${harness}`, () => {
        it.each(diagnostics)("describeStatus and RunEventRow show %s fixed text", (code, text) => {
          const message = diagnostic(1, stage, harness, code, text);
          expect(describeStatus(message.payload)).toBe(text);
          const r = render(<RunEventRow msg={message} harness={harness} live={false} />);
          expect(r.getByText(text)).toBeTruthy();
          expect(r.container.textContent).not.toContain("status: cross_check_diagnostic");
        });

        for (const view of ["agent", "timeline"]) {
          it(`shows all six fixed diagnostic texts in the ${view} checker trace`, () => {
            window.localStorage.setItem("uzi.activity.view", JSON.stringify(view));
            const messages = diagnostics.map(([code, text], i) => diagnostic(i + 1, stage, harness, code, text));
            const original = JSON.stringify(messages);
            const r = renderCheckerTrace(messages, harness);
            for (const [, text] of diagnostics) expect(r.getAllByText(text).length).toBeGreaterThan(0);
            expect(r.container.textContent).not.toContain("status: cross_check_diagnostic");
            expect(JSON.stringify(messages)).toBe(original);
          });
        }
      });
    }
  }

  it.each([
    {},
    { text: null },
    { text: 42 },
    { text: false },
    { text: { message: "not a string" } },
    { text: ["not a string"] },
  ])("retains the generic fallback for invalid or absent text %j", (over) => {
    const message = status(1, {
      event: "cross_check_diagnostic", stage: "plan", harness: "codex",
      reason_class: "model_error", code: "provider_error", ...over,
    });
    expect(describeStatus(message.payload)).toBe("status: cross_check_diagnostic");
    const r = render(<RunEventRow msg={message} live={false} />);
    expect(r.getByText("status: cross_check_diagnostic")).toBeTruthy();
  });

  it("ignores extraneous exception metadata while showing canonical fixed text", () => {
    const [code, text] = diagnostics[0];
    const token = "glpat-" + "12345678901234567890";
    const metadata = {
      error: `provider exception ${token}`,
      url: "https://example.invalid/private",
      path: "/private/checker/trace",
      excerpt: "<img src=x onerror=alert(1)>",
      stack: "private stack\u001b[31m\u202e",
    };
    const message = status(1, {
      event: "cross_check_diagnostic", stage: "code", harness: "claude",
      reason_class: "model_error", code, text, ...metadata,
    });
    expect(describeStatus(message.payload)).toBe(text);
    const r = renderCheckerTrace([message]);
    expect(r.getAllByText(text).length).toBeGreaterThan(0);
    for (const value of Object.values(metadata)) {
      expect(r.container.textContent).not.toContain(value);
      expect(r.container.innerHTML).not.toContain(value);
    }
    expect(r.container.querySelector("img, script, a[href], [onerror]")).toBeNull();
  });

  it("renders HTML-looking status text as inert React text, without claiming to scrub it", () => {
    // This is deliberately not a canonical producer output: the producer guarantees
    // fixed text; this test only pins the existing React escaping at the render sink.
    const text = '<img src=x onerror=alert> <script>danger</script> **plain status**';
    const message = diagnostic(1, "plan", "codex", "provider_error", text);
    expect(describeStatus(message.payload)).toBe(text);
    const row = render(<RunEventRow msg={message} live={false} />);
    expect(row.getByText(text)).toBeTruthy();
    expect(row.container.querySelector("img, script, a, [onerror]")).toBeNull();
    row.unmount();
    const feed = renderCheckerTrace([message], "codex");
    expect(feed.getAllByText(text).length).toBeGreaterThan(0);
    expect(feed.container.querySelector("img, script, a, [onerror]")).toBeNull();
  });

  for (const view of ["agent", "timeline"]) {
    it(`keeps legacy status and checker activity visible without diagnostics in ${view}`, () => {
      window.localStorage.setItem("uzi.activity.view", JSON.stringify(view));
      const messages = [
        status(1, { text: "Checker is starting" }),
        status(2, { event: "check_activity_start", v: 1, cap: 200, generation: 1 }),
        status(3, { event: "check_activity", op: "read", outcome: "ok", path: "src/check.ts" }),
        status(4, { event: "check_activity_summary", generation: 1,
          reads: { ok: 1 }, searches: {}, dropped: 0, complete: true }),
        status(5, { event: "legacy_checker_event" }),
        status(6, { event: "legacy_checker_event", text: "Legacy worker progress" }),
      ];
      const r = renderCheckerTrace(messages);
      for (const text of [
        "Checker is starting", "activity capture started", "read src/check.ts · ok",
        "activity summary: 1 read, 0 searches, 0 dropped, complete",
        "status: legacy_checker_event", "Legacy worker progress",
      ]) expect(r.getAllByText(text).length).toBeGreaterThan(0);
    });

    it(`keeps the parent summary fallback compatible in ${view}`, () => {
      window.localStorage.setItem("uzi.activity.view", JSON.stringify(view));
      const fallback = "The cross-check did not complete.";
      const message: RunMessage = {
        ...status(1, { stage: "plan", verdict: "failed", reason_class: "model_error",
          findings: { summary: fallback, items: [] } }),
        kind: "cross_check", agent: null,
      };
      const r = render(
        <MemoryRouter>
          <ActivityFeed run={{ ...mockRuns[0], kind: "issue", status: "awaiting_approval" }}
            messages={[message]} runningLive={false} connected={true} terminal={false} />
        </MemoryRouter>,
      );
      const expand = r.queryByRole("button", { name: "Expand all" });
      if (expand) fireEvent.click(expand);
      expect(r.getByRole("region", { name: "Plan cross-check event" }).textContent).toContain(fallback);
      expect(r.getByText(fallback)).toBeTruthy();
    });
  }
});
