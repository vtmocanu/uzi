// @vitest-environment jsdom
// Transport mocks only: actual PlanPanel → RunView → useRunStream → api serializer.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { createMemoryRouter, RouterProvider } from "react-router-dom";
import { RunView } from "./RunView";
import { mockRuns } from "../mocks/data/runs";
import { MOCK_MODE, type Run } from "../lib/api";
import { hostileSemanticText, semanticGateFixture } from "../lib/semanticGate.testData";
import { isSemanticGate } from "../lib/semanticGate";

class Socket {
  static last: Socket;
  onopen: (() => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: ((event: MessageEvent) => void) | null = null;
  constructor() { Socket.last = this; }
  close() {}
  state() { this.onmessage?.({ data: JSON.stringify({ type: "state" }) } as MessageEvent); }
}
let current: Run;
let writes: { url: string; body: Record<string, unknown>; headers: Record<string, string> }[];
let releaseCredential: (() => void) | undefined;
let holdCredential: boolean;
let refuseApproval: boolean;
function response(body: unknown, status = 200) {
  return { ok: status < 400, status, text: async () => JSON.stringify(body) } as Response;
}
beforeEach(() => {
  expect(MOCK_MODE).toBe(false);
  current = { ...mockRuns[0], id: "semantic-run", harness: "claude", status: "awaiting_approval",
    gate_revision: 4, semantic_gate: semanticGateFixture(), plan_md: "Structural plan",
    milestones: [{ id: "m1", title: "Original" }], milestones_candidate: [{ id: "m1", title: "Original" }],
    own_agents: [], repo_agents: [], required_capabilities: ["docker"], required_tools: [],
    plan_cross_check_summary: undefined, mr_iid: null, outcome_pending: undefined };
  writes = []; holdCredential = false; refuseApproval = false; releaseCredential = undefined;
  vi.stubGlobal("WebSocket", Socket);
  vi.stubGlobal("fetch", vi.fn(async (url: string, init: RequestInit) => {
    if (init?.method !== "GET") {
      writes.push({ url, body: JSON.parse(init.body as string), headers: init.headers as Record<string, string> });
      if (url.endsWith("/credential") && holdCredential) {
        await new Promise<void>(resolve => { releaseCredential = resolve; });
        return response({ run: current });
      }
      if (refuseApproval && url.endsWith("/inputs")) return response({ error: "Semantic candidate changed", reason: "semantic_gate_mismatch" }, 409);
      return response({ server_side: true });
    }
    if (url === "/api/runs/semantic-run") return response({ run: structuredClone(current) });
    if (url.endsWith("/messages")) return response({ messages: [] });
    if (url.endsWith("/inputs")) return response({ inputs: [] });
    if (url === "/api/repos") return response({ repos: [] });
    if (url === "/api/workers") return response({ workers: [] });
    if (url.includes("/secrets")) return response({ secrets: [{
      id: "sec1", kind: "anthropic_token", label: "demo-key", enabled: true, is_default: false, auto_eligible: true,
      disabled_at: null, created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z",
    }] });
    if (url.endsWith("/review")) return response({ review: null, pending_judge: null });
    return response({}, 404);
  }));
});
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
async function mount() {
  const router = createMemoryRouter([{ path: "/runs/:id", element: <RunView /> }], { initialEntries: ["/runs/semantic-run"] });
  const rendered = render(<RouterProvider router={router} />);
  await screen.findByRole("button", { name: /^Approve plan/ });
  return rendered;
}

it.each([false, true])("real approval chain echoes exact raw authority and marker, override=%s", async override => {
  const original = structuredClone(current.semantic_gate!.approval);
  await mount();
  fireEvent.click(screen.getByRole("button", { name: override ? /Run without docker/ : /^Approve plan/ }));
  await waitFor(() => expect(writes).toHaveLength(1));
  expect(writes[0].body).toEqual({
    kind: "approve_plan", body: "", selection: { source: "own", exclusions: [] },
    ...(override ? { override_capabilities: true } : {}),
    expected_gate_revision: 4, semantic_approval: original,
  });
  expect(writes[0].headers["X-Uzi-Candidate-Identity-Version"]).toBe("semantic_v1");
  expect((writes[0].body.semantic_approval as typeof original).maintainers[0].reason).toBe(hostileSemanticText);
  expect(current.semantic_gate!.approval).toEqual(original);
});

it("shows every criterion, full frozen text, report, citations, budgets and pending maintainer work safely", async () => {
  const { container } = await mount();
  const review = within(screen.getByRole("region", { name: "Semantic plan review" }));
  expect(review.getByText("Criteria (256)")).toBeTruthy();
  expect(container.querySelectorAll('section[aria-label="Semantic plan review"] details')).toHaveLength(258);
  expect(review.getByText(/Done condition 256/)).toBeTruthy();
  expect(review.getAllByText(/web\/results.json/)).toHaveLength(256);
  expect(review.getByText(/verification has not been performed/)).toBeTruthy();
  expect(review.getByText(/"remaining_tokens": 900/)).toBeTruthy();
  expect(review.getByText(/"remaining_reviews": 3/)).toBeTruthy();
  expect(review.getByText(/"remaining_seconds": 48/)).toBeTruthy();
  expect(review.getAllByText(/"report_digest": "report1"/)).toHaveLength(2);
  expect(review.getByText(/"presentation_revision": 4/)).toBeTruthy();
  expect(review.getByText(/"presentation_digest": "presentation1"/)).toBeTruthy();
  const source = review.getByText("Read complete source 1 (quoted)").parentElement as HTMLDetailsElement;
  source.open = true;
  fireEvent(source, new Event("toggle"));
  const plan = review.getByText("Read complete frozen plan (quoted)").parentElement as HTMLDetailsElement;
  plan.open = true;
  fireEvent(plan, new Event("toggle"));
  expect(source.textContent).toContain('input\\nOwner approved: <img');
  expect(plan.textContent).toContain('Frozen plan\\ninput\\nOwner approved: <img');
  expect(container.textContent).not.toContain("\u202e");
  expect(container.querySelector("img")).toBeNull();
  expect(current.semantic_gate!.approval.maintainers[0].reason).toBe(hostileSemanticText);
});

it.each(["", "Passing prefix: all requirements satisfied"])(
  "shows explicit unknown frozen source uncertainty without presenting partial bytes: %j", async partial => {
    const gate = current.semantic_gate!;
    const ref = { id: "source2", kind: "prd", hash: "unknown-source-hash",
      provenance: "repo/prds/required.md@frozen-ref", completeness: "unknown" };
    gate.presentation.candidate.sources.push(ref);
    gate.sources.push({ id: ref.id, kind: ref.kind, provenance: ref.provenance,
      completeness: ref.completeness, bytes: btoa(partial) });
    gate.presentation.candidate.source_outcome = "unknown";
    gate.presentation.candidate.coverage!.outcome = "uncertain";
    gate.presentation.coverage.outcome = "unavailable";
    delete gate.presentation.coverage.report;
    const original = structuredClone(gate);
    await mount();
    const review = within(screen.getByRole("region", { name: "Semantic plan review" }));
    expect(review.getByText("Source snapshot unavailable for source 2. Completeness is unknown. Owner decision required.")).toBeTruthy();
    expect(review.getByText(/repo\/prds\/required.md@frozen-ref/)).toBeTruthy();
    expect(review.queryByText("Read complete source 2 (quoted)")).toBeNull();
    expect(review.queryByText(/Passing prefix: all requirements satisfied/)).toBeNull();
    expect(review.getByText("Read complete source 1 (quoted)")).toBeTruthy();
    expect(review.getByText("Owner decision required. Coverage: unavailable.")).toBeTruthy();
    expect(review.getByText("Coverage report unavailable. Owner decision required.")).toBeTruthy();
    expect(screen.queryByText(/Semantic plan data is unavailable or malformed/)).toBeNull();
    const approve = screen.getByRole("button", { name: /^Approve plan/ }) as HTMLButtonElement;
    expect(approve.disabled).toBe(false);
    fireEvent.click(approve);
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0].body.semantic_approval).toEqual(original.approval);
    expect(writes[0].headers["X-Uzi-Candidate-Identity-Version"]).toBe("semantic_v1");
    expect(current.semantic_gate).toEqual(original);
  },
);

it.each([null, {}, { approval: {} }, { ...semanticGateFixture(), plan: null },
  { ...semanticGateFixture(), sources: [{ ...semanticGateFixture().sources[0], bytes: null }] },
  { ...semanticGateFixture(), plan: "/w==" }, { ...semanticGateFixture(), plan: "%%%" }])(
  "blocks malformed semantic data without structural fallback", async value => {
    current = { ...current, semantic_gate: value } as Run;
    await mount();
    expect(screen.getByText(/Semantic plan data is unavailable or malformed/)).toBeTruthy();
    for (const button of [screen.getByRole("button", { name: /^Approve plan/ }), screen.getByRole("button", { name: /Run without docker/ })]) {
      expect((button as HTMLButtonElement).disabled).toBe(true);
      fireEvent.click(button);
    }
    expect(writes).toHaveLength(0);
  },
);

it.each(["candidate", "source", "criteria", "generation", "maintainers", "report"] as const)(
  "blocks internally inconsistent semantic %s before approval", async field => {
    const gate = semanticGateFixture();
    if (field === "candidate") gate.approval.candidate_digest = "foreign";
    if (field === "source") gate.approval.source_digest = "foreign";
    if (field === "criteria") gate.approval.criteria_digest = "foreign";
    if (field === "generation") gate.approval.claim_generation++;
    if (field === "maintainers") gate.approval.maintainers[0].conditions = ["Foreign condition"];
    if (field === "report") gate.presentation.coverage.report!.criteria_digest = "foreign";
    current.semantic_gate = gate;
    await mount();
    expect(screen.getByText(/Semantic plan data is unavailable or malformed/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /^Approve plan/ }));
    expect(writes).toHaveLength(0);
  },
);

it("semantic reset is explicitly unsupported and cannot downgrade to ordinary reset", async () => {
  current.milestones_candidate = [{ id: "m1", title: "Changed" }];
  current.reset_state_digest = "reset-digest";
  current.completion_revision = 2;
  await mount();
  expect(screen.getByText(/Semantic plan reset is unsupported/)).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: /^Approve plan/ }));
  expect(screen.queryByRole("region", { name: "Confirm milestone reset" })).toBeNull();
  expect(writes).toHaveLength(0);
});

it.each(["unavailable", "exhausted"])("requires an owner decision when coverage is %s", async outcome => {
  current.semantic_gate!.presentation.coverage.outcome = outcome;
  delete current.semantic_gate!.presentation.coverage.report;
  await mount();
  expect(screen.getByText(`Owner decision required. Coverage: ${outcome}.`)).toBeTruthy();
  expect(screen.getByText("Coverage report unavailable. Owner decision required.")).toBeTruthy();
});

it("captures old approval before credential await even if polling replaces the candidate; server refusal is surfaced without retry", async () => {
  holdCredential = true;
  const old = structuredClone(current.semantic_gate!.approval);
  await mount();
  const select = screen.getByLabelText("Anthropic token for the implementation phase");
  const opt = await within(select).findByRole("option", { name: /demo-key/ });
  fireEvent.change(select, { target: { value: (opt as HTMLOptionElement).value } });
  fireEvent.click(screen.getByRole("button", { name: /^Approve plan/ }));
  await waitFor(() => expect(releaseCredential).toBeTypeOf("function"));
  current = structuredClone(current);
  current.gate_revision = 5;
  current.semantic_gate!.approval.presentation_revision = 5;
  current.semantic_gate!.approval.presentation_digest = "new-presentation";
  current.semantic_gate!.presentation.candidate.revision = 8;
  current.semantic_gate!.approval.candidate_revision = 8;
  const gate = current.semantic_gate!;
  gate.presentation.candidate.digest = "new-candidate";
  gate.presentation.binding.candidate_digest = "new-candidate";
  gate.approval.candidate_digest = "new-candidate";
  gate.presentation.coverage.candidate_digest = "new-candidate";
  gate.presentation.coverage.report!.candidate_digest = "new-candidate";
  gate.presentation.candidate.coverage!.candidate_digest = "new-candidate";
  refuseApproval = true;
  act(() => Socket.last.state());
  await waitFor(() => expect(screen.getByText(/"revision": 8/)).toBeTruthy());
  await act(async () => { releaseCredential!(); });
  await waitFor(() => expect(writes.filter(w => w.url.endsWith("/inputs"))).toHaveLength(1));
  const approval = writes.find(w => w.url.endsWith("/inputs"))!;
  expect(approval.body.semantic_approval).toEqual(old);
  expect(approval.body.expected_gate_revision).toBe(4);
  expect(await screen.findByText("Semantic candidate changed")).toBeTruthy();
  expect(writes.filter(w => w.url.endsWith("/inputs"))).toHaveLength(1);
});

it("structural absence preserves payload and omits marker", async () => {
  delete current.semantic_gate;
  await mount();
  fireEvent.click(screen.getByRole("button", { name: /^Approve plan/ }));
  await waitFor(() => expect(writes).toHaveLength(1));
  expect(writes[0].body).toEqual({ kind: "approve_plan", body: "", selection: { source: "own", exclusions: [] }, expected_gate_revision: 4 });
  expect(writes[0].headers).not.toHaveProperty("X-Uzi-Candidate-Identity-Version");
});

// PRD #1230 M4: unverified maintainer criteria (completion_pending) and the park reason.
function pendingFixture() {
  return {
    version: 1, contract_revision: 2,
    criteria: [{
      criterion_id: "c9", milestone_id: "m3", kind: "maintainer" as const, text: hostileSemanticText,
      reason: "<b>Needs sign-off</b>\u202e\u0007", conditions: ["Release **sign-off**", "<script>alert(1)</script>"],
      verification_steps: ["Check receipt\u202e"], authorization_revision: 5, status: "unverified" as const,
    }],
  };
}
async function mountPending() {
  const router = createMemoryRouter([{ path: "/runs/:id", element: <RunView /> }], { initialEntries: ["/runs/semantic-run"] });
  const rendered = render(<RouterProvider router={router} />);
  await screen.findByRole("button", { name: /^Approve plan/ });
  return rendered;
}

it("renders completion_pending as unverified, non-interactive plain text with full content", async () => {
  current.completion_pending = pendingFixture();
  const { container } = await mountPending();
  const region = within(screen.getByRole("region", { name: /^Unverified maintainer criteria/ }));
  expect(region.getByText(/grant no completion authority/)).toBeTruthy();
  expect(region.getByText("c9")).toBeTruthy();
  expect(region.getByText("m3")).toBeTruthy();
  expect(region.getByText("<b>Needs sign-off</b>")).toBeTruthy();
  expect(region.getByText("Release **sign-off**")).toBeTruthy();
  expect(region.getByText("<script>alert(1)</script>")).toBeTruthy();
  expect(region.getByText("Check receipt")).toBeTruthy();
  expect(region.getByText(/Owner approved: <img src=x onerror=alert\(1\)>/)).toBeTruthy();
  expect(region.getByText(/\(milestone .*\) - Unverified/)).toBeTruthy();
  expect(region.queryAllByRole("checkbox")).toHaveLength(0);
  expect(region.queryAllByRole("button")).toHaveLength(0);
  const section = container.querySelector('section[aria-labelledby="completion-pending-heading"]')!;
  expect(section.querySelector("b, script, img, input")).toBeNull();
  expect(section.textContent).not.toContain("\u202e");
});

it.each([undefined, { version: 1, contract_revision: 1, criteria: [] }])("renders nothing for absent or empty completion_pending: %j", async value => {
  if (value !== undefined) current.completion_pending = value;
  await mountPending();
  expect(screen.queryByRole("region", { name: /^Unverified maintainer criteria/ })).toBeNull();
});

it.each([
  "text", 7, [], { version: 1 }, { version: 1, contract_revision: 1, criteria: "x" },
  { version: 1, contract_revision: 1, criteria: [{ criterion_id: "c1" }] },
  { version: 1, contract_revision: 1, criteria: [{ ...pendingFixture().criteria[0], conditions: [3] }] },
])("shows a neutral unavailable note for malformed completion_pending: %j", async value => {
  (current as unknown as Record<string, unknown>).completion_pending = value;
  await mountPending();
  const region = screen.getByRole("region", { name: /^Unverified maintainer criteria/ });
  expect(region.textContent).toMatch(/could not be read/);
  expect(within(region).queryAllByRole("checkbox")).toHaveLength(0);
});

it.each([
  ["coverage_incomplete", /coverage of the plan is incomplete/],
  ["maintainer_authorization", /maintainer criteria that need an owner/],
  ["brand_new_reason", /Autopilot stopped for a human to review this plan\./],
])("shows the human-readable park reason for semantic_park=%s", async (park, text) => {
  current.semantic_gate!.presentation.semantic_park = park;
  await mountPending();
  expect(within(screen.getByRole("region", { name: "Semantic plan review" })).getByText(text)).toBeTruthy();
});

it.each([7, null, ["x"], {}])("rejects a non-string semantic_park: %j", park => {
  const gate = semanticGateFixture();
  (gate.presentation as unknown as Record<string, unknown>).semantic_park = park;
  expect(isSemanticGate(gate)).toBe(false);
});

it("shows no park line when semantic_park is absent", async () => {
  await mountPending();
  expect(screen.queryByText(/Autopilot stopped for a human/)).toBeNull();
});

// PRD #1230 D5: source drift observations (semantic_drift). A detector's record, never an approval or a
// satisfaction, so the panel is plain text with no control in it.
function driftFixture() {
  const ref = "0123456789abcdef0123456789abcdef01234567";
  return {
    observations: [
      { phase: "presentation" as const, candidate_digest: "d".repeat(64), gate_revision: 4, observed_at: "2026-02-03T04:05:06Z",
        issue: { source_id: "issue:7", frozen_hash: "a".repeat(64), observed_hash: "a".repeat(64), outcome: "unchanged" as const },
        prds: [], links_added: [], links_removed: [] },
      { phase: "publication" as const, pushed_sha: ref, observed_at: "2026-02-03T05:06:07Z",
        issue: { source_id: "issue:7", frozen_hash: "a".repeat(64), observed_hash: "b".repeat(64), outcome: "changed" as const, checkbox_only: true },
        prds: [
          { source_id: "prd:prds/<b>9-x</b>\u202e.md", frozen_hash: "c".repeat(64), outcome: "unknown" as const, reason: "timeout" as const, ref },
          { source_id: "prd:prds/8-y.md", frozen_hash: "e".repeat(64), observed_hash: "f".repeat(64), outcome: "changed" as const, ref },
        ],
        links_added: ["prds/10-new.md"], links_removed: ["prds/2-old.md"], links_truncated: true },
    ],
    dropped: 3, contradiction: true,
  };
}

it("renders semantic_drift as a plain-text record with a contradiction note", async () => {
  current.semantic_drift = driftFixture();
  const { container } = await mountPending();
  const region = within(screen.getByRole("region", { name: /^Source drift observations/ }));
  expect(region.getByText(/Contradiction: owner review needed/)).toBeTruthy();
  expect(region.getByText(/3 older observations no longer kept/)).toBeTruthy();
  expect(region.getByText(/When the plan was presented for approval/)).toBeTruthy();
  expect(region.getByText(/When the work was published/)).toBeTruthy();
  expect(region.getByText("Issue issue:7: unchanged")).toBeTruthy();
  expect(region.getByText("Issue issue:7: changed, checkbox state only")).toBeTruthy();
  expect(region.getByText("prd:prds/<b>9-x</b>.md: unknown, timeout")).toBeTruthy();
  expect(region.getByText("prd:prds/8-y.md: changed")).toBeTruthy();
  expect(region.getByText("prds/10-new.md")).toBeTruthy();
  expect(region.getByText("prds/2-old.md")).toBeTruthy();
  expect(region.getByText(/Some PRD links were not compared/)).toBeTruthy();
  expect(region.getByText(/nothing here marks a criterion as met/)).toBeTruthy();
  expect(region.queryAllByRole("checkbox")).toHaveLength(0);
  expect(region.queryAllByRole("button")).toHaveLength(0);
  const section = container.querySelector('section[aria-labelledby="semantic-drift-heading"]')!;
  expect(section.querySelector("b, script, img, input")).toBeNull();
  expect(section.textContent).not.toContain("\u202e");
});

it("shows no contradiction note when the server reports none", async () => {
  const clean = driftFixture();
  current.semantic_drift = { observations: clean.observations.slice(0, 1), dropped: 0, contradiction: false };
  await mountPending();
  const region = screen.getByRole("region", { name: /^Source drift observations/ });
  expect(region.textContent).not.toMatch(/Contradiction/);
  expect(region.textContent).not.toMatch(/no longer kept/);
});

it.each([undefined, null, { observations: [], dropped: 0, contradiction: false }])("renders nothing for absent or empty semantic_drift: %j", async value => {
  if (value !== undefined) (current as unknown as Record<string, unknown>).semantic_drift = value;
  await mountPending();
  expect(screen.queryByRole("region", { name: /^Source drift observations/ })).toBeNull();
});

it.each([
  "text", 7, [], {}, { observations: "x", dropped: 0, contradiction: true },
  { observations: [], dropped: -1, contradiction: true }, { observations: [], dropped: 0 },
  { observations: [{ ...driftFixture().observations[0], phase: "later" }], dropped: 0, contradiction: false },
  { observations: [{ ...driftFixture().observations[0], issue: { source_id: "i", frozen_hash: "a", outcome: "satisfied" } }], dropped: 0, contradiction: false },
  { observations: [{ ...driftFixture().observations[0], links_added: [3] }], dropped: 0, contradiction: false },
])("shows a neutral note for malformed semantic_drift: %j", async value => {
  (current as unknown as Record<string, unknown>).semantic_drift = value;
  await mountPending();
  const region = screen.getByRole("region", { name: /^Source drift observations/ });
  expect(region.textContent).toMatch(/could not be read/);
  expect(region.textContent).not.toMatch(/Contradiction/);
});
