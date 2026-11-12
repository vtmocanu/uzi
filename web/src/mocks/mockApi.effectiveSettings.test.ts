// @vitest-environment jsdom
//
// PRD #3254 M2: the demo serves the server's RECORDED effective-settings behavior
// (fixtures/api-contract/effective_settings.behavior.json), per mock scenario.

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import behavior from "../../../fixtures/api-contract/effective_settings.behavior.json";
import { MODEL_SETTING_SOURCE_LABELS } from "../lib/modelSettings";
import { RECORDED_EFFECTIVE_SCENARIOS } from "./data/effectiveSettings";
import { mockApi } from "./mockApi";

function installStorage(): void {
  const m = new Map<string, string>();
  Object.defineProperty(window, "localStorage", {
    configurable: true,
    value: {
      getItem: (k: string) => (m.has(k) ? m.get(k)! : null),
      setItem: (k: string, v: string) => void m.set(k, String(v)),
      removeItem: (k: string) => void m.delete(k),
      clear: () => m.clear(),
      key: (i: number) => [...m.keys()][i] ?? null,
      get length() {
        return m.size;
      },
    } as Storage,
  });
}

function setScenario(name: string): void {
  window.localStorage.setItem("uzi_mock_scenario", name);
}

beforeEach(installStorage);
afterEach(() => window.localStorage.clear());

describe("recorded effective-settings corpus", () => {
  it("the mock data is a verbatim copy of the Go-recorded corpus", () => {
    expect(RECORDED_EFFECTIVE_SCENARIOS).toEqual(behavior.scenarios);
    expect(RECORDED_EFFECTIVE_SCENARIOS.map((s) => s.name)).toEqual(["default", "codexOnly", "overflowCodexDefault"]);
  });

  it("every harness, credential and note token it serves has a settings-surface label", () => {
    const labelled = new Set(MODEL_SETTING_SOURCE_LABELS.map((r) => `${r.field}/${r.token}`));
    const missing: string[] = [];
    for (const { get, start_previews } of RECORDED_EFFECTIVE_SCENARIOS) {
      const harnesses = [...get.start.map((c) => c.harness), ...start_previews.map((p) => p.response.harness)];
      for (const h of harnesses) {
        if (h) {
          if (!labelled.has(`harness/${h.source}`)) missing.push(`harness/${h.source}`);
          for (const n of h.notes) if (!labelled.has(`note/${n}`)) missing.push(`note/${n}`);
        }
      }
      const credentials = [
        ...get.harnesses.map((h) => h.credential),
        ...start_previews.flatMap((p) => [p.response.credential, ...(p.response.alternatives ?? []).map((a) => a.credential)]),
      ];
      for (const c of credentials) if (c && !labelled.has(`credential/${c.source}`)) missing.push(`credential/${c.source}`);
    }
    expect(missing).toEqual([]);
  });
});

describe("getEffectiveSettings", () => {
  it("serves the default recording: both harnesses usable, 8 model cells, 4 effort cells", async () => {
    const got = await mockApi.getEffectiveSettings();
    expect(got).toEqual(behavior.scenarios[0].get);
    expect(got.harnesses.map((h) => [h.harness, h.usable])).toEqual([["claude", true], ["codex", true]]);
    expect(got.models).toHaveLength(8);
    expect(got.efforts).toHaveLength(4);
    expect(got.cross_check.models).toHaveLength(4);
    expect(got.cross_check.efforts).toHaveLength(4);
  });

  it("serves the codexOnly recording under the codex-only scenario", async () => {
    setScenario("codex-only");
    const got = await mockApi.getEffectiveSettings();
    expect(got).toEqual(behavior.scenarios[1].get);
    expect(got.harnesses[0]).toMatchObject({ harness: "claude", usable: false, refusal: "no_credential_for_harness" });
    expect(got.start.find((c) => c.choice === "claude")).toMatchObject({ harness: null, refusal: "no_credential_for_harness" });
  });

  // PRD #3257: the recordings have "Judge runs on" unset; the stored choice is applied at
  // response time, as the server derives it, without touching the recorded copy.
  it("reflects a saved Judge runs on: a user_default judge harness and not_used on the other lane", async () => {
    try {
      await mockApi.putMySettings({ judge_runs_on: "codex" });
      const got = await mockApi.getEffectiveSettings();
      expect(got.judge_harness).toEqual({ value: "codex", source: "user_default", fixed_at: "create", notes: [] });
      const judge = got.models.filter((c) => c.setting === "judge_model");
      expect(judge.map((c) => [c.harness, c.notes.includes("not_used")])).toEqual([["claude", true], ["codex", false]]);
      // Every other cell is the recording's, and the recording itself is untouched.
      expect(got.models.filter((c) => c.setting !== "judge_model")).toEqual(
        behavior.scenarios[0].get.models.filter((c) => c.setting !== "judge_model"),
      );
      expect(RECORDED_EFFECTIVE_SCENARIOS[0].get.judge_harness.source).toBe("same_as_run");

      await mockApi.putMySettings({ judge_runs_on: "claude" });
      const claude = await mockApi.getEffectiveSettings();
      expect(claude.judge_harness).toMatchObject({ value: "claude", source: "user_default" });
      expect(claude.models.filter((c) => c.setting === "judge_model").map((c) => [c.harness, c.notes.includes("not_used")]))
        .toEqual([["claude", false], ["codex", true]]);

      await mockApi.putMySettings({ judge_runs_on: null });
      expect(await mockApi.getEffectiveSettings()).toEqual(behavior.scenarios[0].get);
    } finally {
      await mockApi.putMySettings({ judge_runs_on: null });
    }
  });

  it("returns a copy, so a caller cannot change the recording", async () => {
    const first = await mockApi.getEffectiveSettings();
    first.harnesses.length = 0;
    expect((await mockApi.getEffectiveSettings()).harnesses).toHaveLength(2);
  });
});

describe("previewStartSettings", () => {
  it("answers every recorded request with its recorded response, in each scenario", async () => {
    const scenarios: Record<string, string> = { default: "", codexOnly: "codex-only", overflowCodexDefault: "overflow-codex-default" };
    for (const s of behavior.scenarios) {
      setScenario(scenarios[s.name]);
      for (const p of s.start_previews) {
        expect(await mockApi.previewStartSettings(p.request), `${s.name}: ${p.name}`).toEqual(p.response);
      }
    }
  });

  it("returns the recorded refused alternative: Codex refused while Claude is valid", async () => {
    setScenario("overflow-codex-default");
    const got = await mockApi.previewStartSettings({ harness: null, credential_override: { mode: "inherit" } });
    expect(got.refusal).toBeNull();
    expect(got.credential).toBeNull();
    expect(got.alternatives).toEqual([
      { harness: "claude", credential: { kind: "anthropic", source: "claim_selection", fixed_at: "claim", auth_mode: null }, refusal: null, refusal_detail: null },
      { harness: "codex", credential: null, refusal: "credential_override_harness_unsupported", refusal_detail: null },
    ]);
  });

  it("treats an absent secret_id and a missing override like the recorded null", async () => {
    const got = await mockApi.previewStartSettings({ harness: "claude", credential_override: null });
    expect(got.harness).toMatchObject({ value: "claude", source: "run_override" });
  });

  it("serves the scenario's default case for a request the corpus does not record", async () => {
    const got = await mockApi.previewStartSettings({ harness: "codex", credential_override: { mode: "default" } });
    expect(got).toEqual(behavior.scenarios[0].start_previews[0].response);
  });
});

describe("previewScheduleSettings", () => {
  const scenarios: Record<string, string> = { default: "", codexOnly: "codex-only", overflowCodexDefault: "overflow-codex-default" };

  it("answers every recorded request with its recorded response, in each scenario", async () => {
    for (const s of behavior.scenarios) {
      setScenario(scenarios[s.name]);
      for (const p of s.schedule_previews) {
        expect(await mockApi.previewScheduleSettings(p.request), `${s.name}: ${p.name}`).toEqual(p.response);
      }
    }
  });

  it("serves the recorded schedule pin on the other harness as ignored", async () => {
    const got = await mockApi.previewScheduleSettings({ target: "sweep", harness: "codex", model: "sonnet", override_subagent_model: true });
    expect(got.harness).toMatchObject({ value: "codex", source: "schedule_pin" });
    expect(got.worker_model?.ignored).toMatchObject({ value: "sonnet", source: "schedule_pin" });
    expect(got.worker_model?.notes).toContain("schedule_pin_ignored");
    expect(got.notes).toContain("issue_labels_may_select_harness");
  });

  it("serves the recorded save-step refusal with no harness-dependent fields", async () => {
    const got = await mockApi.previewScheduleSettings({ target: "sweep", model: "two words" });
    expect(got).toMatchObject({ refusal: "invalid_model", worker_model: null, worker_effort: null, credential: null, subagents: null });
  });

  it("serves a chosen-at-create harness as alternatives, Claude then Codex, in the overflow scenario", async () => {
    setScenario("overflow-codex-default");
    const got = await mockApi.previewScheduleSettings({ target: "sweep" });
    expect(got.harness).toMatchObject({ value: null, source: "chosen_at_create" });
    expect(got.worker_model).toBeNull();
    expect(got.alternatives?.map((a) => a.harness)).toEqual(["claude", "codex"]);
  });

  it("serves the scenario's plain case for the target when the request is unrecorded, as a copy", async () => {
    const got = await mockApi.previewScheduleSettings({ target: "sweep", harness: "claude", model: "opus" });
    expect(got).toEqual(behavior.scenarios[0].schedule_previews[0].response);
    got.notes.length = 0;
    expect((await mockApi.previewScheduleSettings({ target: "sweep" })).notes).toEqual(behavior.scenarios[0].schedule_previews[0].response.notes);
  });

  it("treats a saved schedule like an unrecorded request", async () => {
    const got = await mockApi.previewScheduleSettings({ schedule_id: "00000000-0000-4000-8000-000000000001" });
    expect(got).toEqual(behavior.scenarios[0].schedule_previews[0].response);
  });
});
