// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { issueLineModel, issueLineStage, type LineStage } from "../lib/issueLine";

// ?mock=overview-next (PRD #3878): scenario-gated seeding for the Overview preview. The
// scenario is read when the store seeds, so each test re-imports a fresh mockApi.
async function freshApi(scenario: string | null) {
  vi.resetModules();
  localStorage.clear();
  if (scenario) localStorage.setItem("uzi_mock_scenario", scenario);
  return (await import("./mockApi")).mockApi;
}

afterEach(() => localStorage.clear());

describe("overview-next mock scenario", () => {
  it("adds other users' runs to the admin list only, with their owners", async () => {
    const api = await freshApi("overview-next");
    const mine = (await api.listRuns({ view: "summary" })).runs;
    const factory = (await api.adminListRuns({ view: "summary" })).runs;
    const byId = new Map(factory.map((r) => [r.id, r]));

    expect(byId.get("run-ov-priya-approval")).toMatchObject({ status: "awaiting_approval", owner_email: "priya@uzi.local" });
    expect(byId.get("run-ov-sam-running")).toMatchObject({ status: "running", owner_email: "sam@uzi.local" });
    expect(byId.get("run-ov-jordan-limit")).toMatchObject({ status: "limit_wait", owner_email: "jordan@uzi.local" });
    for (const id of ["run-ov-priya-approval", "run-ov-sam-running", "run-ov-jordan-limit"]) {
      expect(mine.some((r) => r.id === id)).toBe(false);
    }
  });

  it("seeds an own completed run sharing repo and MR with run-ci-halted", async () => {
    const api = await freshApi("overview-next");
    const runs = (await api.listRuns({ view: "summary" })).runs;
    const halted = runs.find((r) => r.id === "run-ci-halted")!;
    const dup = runs.find((r) => r.id === "run-ov-ci-halted-retry")!;
    expect(dup).toMatchObject({ status: "completed", kind: "issue", mr_state: "opened" });
    expect([dup.repo_id, dup.mr_iid]).toEqual([halted.repo_id, halted.mr_iid]);
  });

  it("puts at least one run at every stage of the issue-work line for the demo admin", async () => {
    const api = await freshApi("overview-next");
    const own = (await api.listRuns({ view: "summary" })).runs;
    const factory = (await api.adminListRuns({ view: "summary" })).runs;
    const model = issueLineModel({ ownRuns: own, otherRuns: factory });
    const stages: LineStage[] = ["queued", "planning", "review", "building", "asking", "parked", "other"];
    for (const stage of stages) {
      expect(model.entries[stage].length, `stage ${stage}`).toBeGreaterThan(0);
    }
    expect(model.prs.length).toBeGreaterThan(0);
    // The line holds other users' runs too (planning, asking and Other work among them).
    for (const id of ["run-ov-lena-planning", "run-ov-omar-asking", "run-ov-ines-prompt"]) {
      expect(factory.find((r) => r.id === id), id).toBeDefined();
      expect(own.some((r) => r.id === id)).toBe(false);
    }
    const stageOf = (id: string) => issueLineStage([...own, ...factory].find((r) => r.id === id)!, new Set());
    expect(stageOf("run-ov-lena-planning")).toBe("planning");
    expect(stageOf("run-ov-omar-asking")).toBe("asking");
    expect(stageOf("run-ov-ines-prompt")).toBe("other");
  });

  it("seeds the viewer's revising, cooldown and ci_fix runs on the stages the line gives them", async () => {
    const api = await freshApi("overview-next");
    const own = (await api.listRuns({ view: "summary" })).runs;
    const stageOf = (id: string) => issueLineStage(own.find((r) => r.id === id)!, new Set());
    expect(own.find((r) => r.id === "run-ov-own-revising")).toMatchObject({ status: "awaiting_approval", is_revising: true });
    expect(stageOf("run-ov-own-revising")).toBe("planning");
    expect(own.find((r) => r.id === "run-ov-own-cooldown")).toMatchObject({ status: "running", health: "waiting_account_cooldown" });
    expect(stageOf("run-ov-own-cooldown")).toBe("parked");
    expect(own.find((r) => r.id === "run-ov-own-cifix")).toMatchObject({ kind: "ci_fix" });
    expect(stageOf("run-ov-own-cifix")).toBe("other");
  });

  it("turns maintenance on and serves a health document that raises the admin attention banner", async () => {
    const api = await freshApi("overview-next");
    expect((await api.getMaintenance()).active).toBe(true);
    const doc = await api.getAdminHealth();
    const banner = doc.checks.filter((c) => c.scope === "owner" && c.severity === "warn" && c.attention_banner === true);
    expect(banner.length).toBeGreaterThan(0);
    expect(doc.blocking).toBe(false);
  });

  it("serves an owner fleet covering every capacity state, only under the scenario", async () => {
    const api = await freshApi("overview-next");
    const fleet = (await api.listWorkers()).workers;
    const byId = new Map(fleet.map((w) => [w.id, w]));
    expect(byId.get("w-ov-rolling")).toMatchObject({ kind: "hosted", upgrade_status: "upgrading", upgrade_target: "v0.86.1-rc.2", active_runs: 1 });
    expect(byId.get("w-ov-rolling")?.draining_since).toEqual(expect.any(String));
    expect(byId.get("w-ov-cordoned")).toMatchObject({ effective_cordoned: true, active_runs: 0, max_concurrent_runs: 1 });
    expect(byId.get("w-ov-offline")?.status).toBe("offline");
    expect(byId.get("w-ov-nocap")?.max_concurrent_runs).toBeNull();
    expect(byId.get("w-ov-nostats")?.stats_source).toBeNull();
    const plain = await freshApi(null);
    expect((await plain.listWorkers()).workers.some((w) => w.id.startsWith("w-ov-"))).toBe(false);
  });

  it("gives the fleet rows their problem states, stats and runs on the worker", async () => {
    const api = await freshApi("overview-next");
    const fleet = (await api.listWorkers()).workers;
    const byId = new Map(fleet.map((w) => [w.id, w]));
    const own = (await api.listRuns({ view: "summary" })).runs;

    const b = byId.get("w-ov-full-b")!;
    expect(b.stats_disk_dind_bytes! / b.stats_disk_dind_total_bytes!).toBeGreaterThanOrEqual(0.85);
    expect(b.custody_decisions_needed).toBe(1);
    expect(b.version).toBe("0.87.0-rc.4");
    expect(b.run_disk?.some((d) => d.truncated)).toBe(true);

    const memauth = byId.get("w-ov-memauth")!;
    expect(memauth).toMatchObject({ kind: "hosted", ephemeral: true, status: "online" });
    expect(memauth.memory_authority_lost_at).toEqual(expect.any(String));

    const leased = byId.get("w-ov-leased")!;
    expect(leased).toMatchObject({ kind: "hosted", ephemeral: true, active_runs: 0 });
    expect(Date.parse(leased.ephemeral_lease_expires_at!)).toBeGreaterThan(Date.now());

    const offline = byId.get("w-ov-offline")!;
    expect(offline.status).toBe("offline");
    expect(offline.stats_source).toBe("cgroup");
    expect(offline.reported_runs?.length).toBeGreaterThan(0);

    // Exactly one reported run is unjoined; every other one is the viewer's own run.
    const unjoined = fleet.flatMap((w) => w.reported_runs ?? []).filter((r) => !own.some((o) => o.id === r.run_id));
    expect(unjoined.map((r) => r.run_id)).toEqual(["5e44c9d7-3b1a-4c8e-9f20-6a7d81c4e2b0"]);
    // A reported run never exceeds the worker's active-run count while it is online.
    for (const w of fleet.filter((w) => w.status === "online")) {
      expect(w.reported_runs?.length ?? 0, w.id).toBeLessThanOrEqual(w.active_runs);
    }
  });

  // PRD #4040: the owner list carries the pending-question excerpt on the seeded own
  // awaiting_input run only; the admin list never does, in either view, for any owner.
  it("puts pending_question on the viewer's seeded asking run only, never on admin rows", async () => {
    const api = await freshApi("overview-next");
    for (const own of [(await api.listRuns({ view: "summary" })).runs, (await api.listRuns()).runs]) {
      const carriers = own.filter((r) => r.pending_question !== undefined);
      expect(carriers.map((r) => [r.id, r.pending_question])).toEqual([
        ["run-ov-own-asking", "Should the cap count exports that are still queued?"],
      ]);
      expect(own.find((r) => r.id === "run-ov-own-asking")).toMatchObject({ status: "awaiting_input", issue_iid: 409 });
      // The malformed-payload fixture gets no excerpt, as the server rule gives none.
      expect(own.find((r) => r.id === "run-unreadable-question")).toBeDefined();
      expect(own.find((r) => r.id === "run-unreadable-question")).not.toHaveProperty("pending_question");
    }
    for (const factory of [(await api.adminListRuns({ view: "summary" })).runs, (await api.adminListRuns()).runs]) {
      expect(factory.find((r) => r.id === "run-ov-omar-asking")).toMatchObject({ status: "awaiting_input" });
      expect(factory.find((r) => r.id === "run-ov-own-asking")).toMatchObject({ status: "awaiting_input" });
      for (const r of factory) expect(r, r.id).not.toHaveProperty("pending_question");
    }
    // The run's log holds the question the excerpt quotes.
    const log = await api.getRunMessages("run-ov-own-asking");
    expect(log.messages.find((m) => m.kind === "question")?.payload).toMatchObject({
      question_id: expect.any(String),
      questions: [{ question: "Should the cap count exports that are still queued?" }],
    });
  });

  it("seeds none of it outside the scenario", async () => {
    const api = await freshApi(null);
    const factory = (await api.adminListRuns({ view: "summary" })).runs;
    const own = (await api.listRuns({ view: "summary" })).runs;
    expect(factory.some((r) => r.id.startsWith("run-ov-"))).toBe(false);
    expect(own.some((r) => r.id.startsWith("run-ov-"))).toBe(false);
    expect((await api.getMaintenance()).active).toBe(false);
    // #4042: the People chip's extra users stay out of the base roster.
    const emails = (await api.listUsers()).users.map((u) => u.email);
    expect(emails.some((e) => /^(priya|sam|jordan|lena|omar|ines|elif|mateo|hana)@/.test(e))).toBe(false);
  });

  // #4042: every other-run owner has a named user row, and the people without runs fill the
  // popover's signed-in and idle groups (one within 24 hours, one never signed in, one idle).
  it("gives every other-run owner a user row and seeds signed-in and idle people", async () => {
    const api = await freshApi("overview-next");
    const users = (await api.listUsers()).users;
    const byEmail = new Map(users.map((u) => [u.email, u]));
    const factory = (await api.adminListRuns({ view: "summary" })).runs;
    for (const r of factory.filter((r) => r.id.startsWith("run-ov-") && r.owner_email)) {
      const u = byEmail.get(r.owner_email!);
      expect(u, r.owner_email!).toBeDefined();
      expect(u!.display_name, r.owner_email!).toBeTruthy();
      expect(u!.is_active).toBe(true);
    }
    const owners = new Set(factory.map((r) => r.owner_email));
    const idleOnes = users.filter((u) => u.is_active && !owners.has(u.email) && u.id.startsWith("u-ov-"));
    const age = (u: (typeof users)[number]) => (u.last_login ? Date.now() - Date.parse(u.last_login) : null);
    expect(idleOnes.some((u) => age(u) !== null && age(u)! < 24 * 3600_000)).toBe(true);
    expect(idleOnes.some((u) => u.last_login === null)).toBe(true);
    expect(idleOnes.some((u) => age(u) !== null && age(u)! > 2 * 24 * 3600_000)).toBe(true);
  });
});
