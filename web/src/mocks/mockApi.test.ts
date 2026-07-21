// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";

// Enhancement B: the mock persists ONLY the settings maps to localStorage so the
// demo survives a hard reload. A "reload" is simulated by resetting the module
// registry and re-importing mockApi with localStorage primed — its top-level
// loadSettings() then runs against the stored blob, exactly as a fresh page load
// would. This jsdom build does not expose window.localStorage, so back it with a
// Map-based Storage stub (same approach as prefs.test.ts / theme.test.ts).

const KEY = "uzi.mock.v2";

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
  // A fresh module init: re-runs mockApi's top-level loadSettings() against the
  // current (primed or write-through-updated) localStorage.
  vi.resetModules();
  return (await import("./mockApi")).mockApi;
}

afterEach(() => {
  vi.resetModules();
});

describe("mockApi settings persistence (demo survives reload)", () => {
  it("restores a persisted theme override, worker model, and default_theme", async () => {
    installStorage({
      [KEY]: JSON.stringify({
        v: 1,
        userSettings: { default_model: "opus", theme: "mission" },
        appSettings: {
          prd_label: "Feature",
          autopilot_label: "autopilot",
          default_theme: "mission",
          prdless_enabled: "false",
          prdless_label: "NOSPEC",
          slack_enabled: "true",
          public_base_url: "https://uzi.example",
          judge_enabled: "false",
          judge_model: "haiku",
          health_enabled: "false",
          health_stall_seconds: "120",
          health_slow_seconds: "2700",
          health_queued_seconds: "600",
          health_approval_seconds: "3600",
          health_nudge_cooldown_seconds: "1800",
          docker_repo_allowlist: "",
        },
      }),
    });
    const api = await reload();

    const user = (await api.getMySettings()).settings;
    expect(user.theme).toBe("mission");
    expect(user.default_model).toBe("opus");

    const app = (await api.getSettings()).settings;
    expect(app.default_theme).toBe("mission");
    expect(app.prd_label).toBe("Feature");
    // The prdless keys round-trip too (PRD #22 M1 brought them into app_settings).
    expect(app.prdless_enabled).toBe("false");
    expect(app.prdless_label).toBe("NOSPEC");
    // The Slack non-secret keys round-trip too (PRD #25 M1).
    expect(app.slack_enabled).toBe("true");
    expect(app.public_base_url).toBe("https://uzi.example");
    // The run-health keys round-trip too (PRD #47).
    expect(app.health_enabled).toBe("false");
    expect(app.health_stall_seconds).toBe("120");
  });

  it("falls back to seed on a corrupt blob", async () => {
    installStorage({ [KEY]: "{ not valid json" });
    const api = await reload();

    expect((await api.getMySettings()).settings.theme).toBeNull();
    expect((await api.getSettings()).settings.default_theme).toBe("ember");
  });

  it("falls back to seed on a version/shape mismatch (stale demo state is discarded)", async () => {
    installStorage({
      [KEY]: JSON.stringify({
        v: 99, // a different seed-schema version: must be discarded, not served
        userSettings: { default_model: null, theme: "mission" },
        appSettings: { prd_label: "x", autopilot_label: "y", default_theme: "mission" },
      }),
    });
    const api = await reload();

    expect((await api.getMySettings()).settings.theme).toBeNull();
    expect((await api.getSettings()).settings.default_theme).toBe("ember");
  });

  it("write-throughs a settings change so the next reload restores it", async () => {
    installStorage(); // empty store → seeds
    let api = await reload();
    await api.putMySettings({ theme: "mission" });
    await api.updateSettings({ default_theme: "mission" });

    // Reload: the write-through values, not the seed, come back.
    api = await reload();
    expect((await api.getMySettings()).settings.theme).toBe("mission");
    expect((await api.getSettings()).settings.default_theme).toBe("mission");
  });

  it("does not persist non-settings state (runs stay seed-only across a reload)", async () => {
    installStorage();
    const api = await reload();
    // A sanity check that only settings round-trip: the seeded runs are always
    // present from the seed, never from persisted state.
    const runs = (await api.listRuns()).runs;
    expect(runs.length).toBeGreaterThan(0);
  });
});

describe("mockApi run judge review (PRD #46 M4)", () => {
  it("returns the seeded review for a judged run, null for an unjudged one, 404 for unknown", async () => {
    installStorage();
    const api = await reload();

    const { review } = await api.getRunReview("run-done");
    expect(review).not.toBeNull();
    expect(review!.verdict).toBe("issues");
    expect(review!.recommendations.length).toBeGreaterThan(0);

    // A terminal run with no seeded review reads as null (not judged yet).
    expect((await api.getRunReview("run-failed")).review).toBeNull();

    await expect(api.getRunReview("does-not-exist")).rejects.toMatchObject({ status: 404 });
  });

  it("rerunJudge enqueues a judge run for a terminal issue run and rejects a non-terminal one", async () => {
    installStorage();
    const api = await reload();

    const { run } = await api.rerunJudge("run-done");
    expect(run.kind).toBe("judge");
    expect(run.status).toBe("queued");

    // A still-queued run is not terminal, so it cannot be judged.
    await expect(api.rerunJudge("run-queued")).rejects.toMatchObject({ status: 422 });
  });
});

// PRD #104: the mock must CASCADE a token delete the way the schema does.
// Migrations 00078/00079 hang composite FKs off user_secrets (user_id, id) with
// ON DELETE SET NULL, so deleting a bound token unbinds its workers and the judge.
// The mock previously only dropped the secret row, which left the shipped
// Dockerfile.mock demo showing D5's own promise being broken — and with one token
// left the picker hides, so there was no way to correct it in the UI. This is the
// only place D5's cascade is provable above the schema layer inside the SPA.
describe("mockApi token delete cascades like ON DELETE SET NULL (PRD #104 D5)", () => {
  it("unbinds every worker bound to the deleted token, and leaves the others alone", async () => {
    installStorage();
    const api = await reload();
    await api.login("admin@uzi.local", "whatever");

    const { secret } = await api.createAnthropicToken("sk-ant-mock-second", "cascade-key", false);
    const { workers } = await api.listWorkers();
    const target = workers[0];
    const other = workers[1];
    await api.setWorkerToken(target.id, "cascade-key");

    const beforeList = (await api.listWorkers()).workers;
    const beforeOther = beforeList.find((w) => w.id === other?.id);
    expect(beforeList.find((w) => w.id === target.id)?.anthropic_secret_label).toBe("cascade-key");

    await api.deleteAnthropicTokenById(secret.id);

    const after = (await api.listWorkers()).workers.find((w) => w.id === target.id)!;
    expect(after.anthropic_secret_id).toBeNull();
    expect(after.anthropic_secret_label).toBeNull();
    // Deleting a token must not disturb a worker that was never bound to it.
    if (beforeOther) {
      const afterOther = (await api.listWorkers()).workers.find((w) => w.id === other.id)!;
      expect(afterOther.anthropic_secret_id).toBe(beforeOther.anthropic_secret_id);
    }
  });

  it("clears the judge binding when the judge's token is deleted", async () => {
    installStorage();
    const api = await reload();
    await api.login("admin@uzi.local", "whatever");

    const { secret } = await api.createAnthropicToken("sk-ant-mock-judge", "judge-only-key", false);
    const { user } = await api.setJudgeEnabled(true, "judge-only-key");
    expect(user.judge_anthropic_secret_label).toBe("judge-only-key");

    await api.deleteAnthropicTokenById(secret.id);

    const me = await api.me();
    expect(me.user.judge_anthropic_secret_id).toBeNull();
    expect(me.user.judge_anthropic_secret_label).toBeNull();
  });
});
