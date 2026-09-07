// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import type { AppSettings, UpdateSettingsPayload } from "../lib/apiTypes";

// Mock-fidelity regressions: three places where the VITE_UZI_MOCK demo backend
// diverged from the real Go API. Each test fails on the pre-fix mock and passes
// after, mirroring the server behaviour it is pinned to:
//   F1 (schedules) — addScheduleRepo validates the sibling-uniqueness conflict
//                     BEFORE stamping the source's group id, so a rejected add
//                     leaves the source un-stamped (server does it in one tx).
//   F2 (secrets)   — the kind-path deleteAnthropicToken runs the same worker+judge
//                     unbind cascade the by-id path does (schema ON DELETE SET NULL).
//   F3 (settings)  — updateSettings accepts the nine AppSettings keys the server's
//                     Validate accepts (health bools/seconds, capability/project-sync
//                     flags, docker_repo_allowlist) instead of 400 "unknown setting".
//
// Each test reloads the module against a fresh (empty) localStorage so the
// in-memory store re-seeds. Because vi.resetModules() gives the freshly imported
// module its OWN ApiError class, rejections are asserted on the `.status` field
// (toMatchObject), never instanceof.

const KEY = "uzi.mock.v3";

function installStorage(initial: Record<string, string> = {}): void {
  const m = new Map<string, string>(Object.entries(initial));
  const storage = {
    getItem: (k: string) => (m.has(k) ? m.get(k)! : null),
    setItem: (k: string, v: string) => void m.set(k, String(v)),
    removeItem: (k: string) => void m.delete(k),
    clear: () => m.clear(),
    key: (i: number) => [...m.keys()][i] ?? null,
    get length() {
      return m.size;
    },
  } as Storage;
  Object.defineProperty(window, "localStorage", { configurable: true, value: storage });
}

async function reload() {
  vi.resetModules();
  return (await import("./mockApi")).mockApi;
}

beforeEach(() => installStorage({ [KEY]: "" }));
afterEach(() => vi.resetModules());

const VALID_UUID = "11111111-2222-3333-4444-555555555555";

describe("mockApi — F1 addScheduleRepo validates before stamping the group id", () => {
  it("rejects adding the source's OWN repo with 409 and leaves the source un-stamped", async () => {
    const api = await reload();
    const { repos } = await api.listRepos();
    const repoId = repos[0].id;

    const created = await api.createSchedule(repoId, { target: "issue", timing: "recurring" });
    expect(created.sibling_group_id).toBeNull();

    // Adding the schedule's own repo is a partial-unique-index conflict.
    await expect(api.addScheduleRepo(created.id, repoId)).rejects.toMatchObject({ status: 409 });

    // The rejected add must NOT have stamped a fresh group id onto the source (this is
    // the half that fails on the pre-fix mock, which stamped before checking).
    const all = await api.listSchedules();
    const src = all.find((s) => s.id === created.id);
    expect(src?.sibling_group_id ?? null).toBeNull();
  });

  it("adds a genuinely new repo to the group and both rows share one sibling_group_id", async () => {
    const api = await reload();
    const { repos } = await api.listRepos();
    expect(repos.length).toBeGreaterThan(1);
    const repoA = repos[0].id;
    const repoB = repos[1].id;

    const created = await api.createSchedule(repoA, { target: "issue", timing: "recurring" });
    const sibling = await api.addScheduleRepo(created.id, repoB);

    const all = await api.listSchedules();
    const src = all.find((s) => s.id === created.id);
    expect(src?.sibling_group_id).toBeTruthy();
    expect(sibling.sibling_group_id).toBe(src?.sibling_group_id);
    expect(sibling.repo_id).toBe(repoB);
  });
});

describe("mockApi — F2 kind-path deleteAnthropicToken cascades the unbind", () => {
  it("clears the bound worker and judge bindings when the sole token is deleted", async () => {
    const api = await reload();

    // Trim the seed down to exactly one anthropic token (the sole default), deleting the
    // non-defaults by id first (D6: the default cannot go while siblings exist).
    let toks = (await api.listSecrets()).secrets.filter((s) => s.kind === "anthropic_token");
    for (const s of toks.filter((t) => !t.is_default)) await api.deleteAnthropicTokenById(s.id);
    toks = (await api.listSecrets()).secrets.filter((s) => s.kind === "anthropic_token");
    expect(toks).toHaveLength(1);
    const token = toks[0];

    // Bind a worker to the token (pinned) and bind the judge to it.
    const { workers } = await api.listWorkers();
    const workerId = workers[0].id;
    await api.setWorkerBindMode(workerId, "pinned", token.label);
    await api.setJudgeEnabled(true, token.label);

    // Sanity: bindings are actually set before the delete.
    const boundWorker = (await api.listWorkers()).workers.find((w) => w.id === workerId);
    expect(boundWorker?.anthropic_secret_id).toBe(token.id);
    const boundMe = await api.me();
    expect(boundMe.user.judge_anthropic_secret_id).toBe(token.id);

    // The KIND-path delete must cascade the unbind (fails on the pre-fix mock).
    await api.deleteAnthropicToken();

    const worker = (await api.listWorkers()).workers.find((w) => w.id === workerId);
    expect(worker?.anthropic_secret_id ?? null).toBeNull();
    expect(worker?.anthropic_secret_label ?? null).toBeNull();

    const me = await api.me();
    expect(me.user.judge_anthropic_secret_id ?? null).toBeNull();
    expect(me.user.judge_anthropic_secret_label ?? null).toBeNull();
  });
});

describe("mockApi — F3 updateSettings accepts the nine missing AppSettings keys", () => {
  it("accepts valid values for the health/flag/list keys and echoes them", async () => {
    const api = await reload();

    const check = async (key: keyof AppSettings, payload: UpdateSettingsPayload, value: string) => {
      const res = await api.updateSettings(payload);
      expect((res.settings as unknown as Record<string, string>)[key]).toBe(value);
    };

    await check("health_enabled", { health_enabled: "true" }, "true");
    await check("health_stall_seconds", { health_stall_seconds: "120" }, "120");
    await check("health_stall_seconds", { health_stall_seconds: "0" }, "0");
    await check("capability_aware_scheduling", { capability_aware_scheduling: "false" }, "false");
    await check("github_project_sync_enabled", { github_project_sync_enabled: "true" }, "true");
    await check("docker_repo_allowlist", { docker_repo_allowlist: VALID_UUID }, VALID_UUID);
    await check("docker_repo_allowlist", { docker_repo_allowlist: "" }, "");
  });

  it("still 400s on invalid values for those keys", async () => {
    const api = await reload();

    await expect(api.updateSettings({ health_enabled: "yes" })).rejects.toMatchObject({
      status: 400,
    });
    await expect(api.updateSettings({ health_stall_seconds: "30" })).rejects.toMatchObject({
      status: 400,
    });
    await expect(api.updateSettings({ docker_repo_allowlist: "not-a-uuid" })).rejects.toMatchObject(
      { status: 400 },
    );
  });
});
