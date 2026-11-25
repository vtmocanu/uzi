// @vitest-environment jsdom
//
// PRD #3254 M2: the demo serves the server's RECORDED effective-settings behavior
// (fixtures/api-contract/effective_settings.behavior.json), per mock scenario.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { EffectiveSettings, Harness } from "../lib/apiTypes";
import behavior from "../../../fixtures/api-contract/effective_settings.behavior.json";
import { MODEL_SETTING_SOURCE_LABELS } from "../lib/modelSettings";
import { RECORDED_EFFECTIVE_SCENARIOS } from "./data/effectiveSettings";
import { mockApi } from "./mockApi";
import { templates } from "./mockApi/agents";

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
    expect(RECORDED_EFFECTIVE_SCENARIOS.map((s) => s.name)).toEqual(["default", "codexOnly", "overflowCodexDefault", "planning", "planningCodex"]);
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
  it("serves the default recording: both harnesses usable, 10 model cells, 6 effort cells", async () => {
    const got = await mockApi.getEffectiveSettings();
    expect(got).toEqual(behavior.scenarios[0].get);
    expect(got.harnesses.map((h) => [h.harness, h.usable])).toEqual([["claude", true], ["codex", true]]);
    expect(got.models).toHaveLength(10);
    expect(got.efforts).toHaveLength(6);
    expect(got.cross_check.models).toHaveLength(4);
    expect(got.cross_check.efforts).toHaveLength(4);
  });

  it("shows the owner's stored planning pair on the planning cells, and the recording otherwise", async () => {
    const before = await mockApi.getEffectiveSettings();
    expect(before.models.find((c) => c.setting === "planning_model" && c.harness === "claude")).toMatchObject({ harness: "claude", model: null, source: before.models.find((c) => c.setting === "worker_model" && c.harness === "claude")?.source });
    await mockApi.putMySettings({ planning_claude_model: "opus", planning_claude_effort: "xhigh" });
    const got = await mockApi.getEffectiveSettings();
    await mockApi.putMySettings({ planning_claude_model: null, planning_claude_effort: null });
    expect(await mockApi.getEffectiveSettings()).toEqual(behavior.scenarios[0].get);
    expect(got.models.find((c) => c.setting === "planning_model" && c.harness === "claude")).toMatchObject({ model: "opus", resolved_model: "opus", source: "user_planning_default" });
    expect(got.efforts.find((c) => c.setting === "planning_effort" && c.harness === "claude")).toMatchObject({ effort: "xhigh", resolved_effort: "xhigh", source: "user_planning_default", mode: "explicit" });
    expect(got.models.find((c) => c.setting === "worker_model" && c.harness === "claude")?.source).not.toBe("user_planning_default");
  });

  it("keeps Codex planning edits out of Claude and worker cells", async () => {
    const before = await mockApi.getEffectiveSettings();
    await mockApi.putMySettings({ planning_codex_model: "custom-plan", planning_codex_effort: "max" });
    const got = await mockApi.getEffectiveSettings();
    await mockApi.putMySettings({ planning_codex_model: null, planning_codex_effort: null });
    expect(got.models.find(c => c.setting === "planning_model" && c.harness === "codex")).toMatchObject({ model: "custom-plan", resolved_model: "custom-plan", source: "user_planning_default" });
    expect(got.efforts.find(c => c.setting === "planning_effort" && c.harness === "codex")).toMatchObject({ effort: "max", resolved_effort: "max", mode: "explicit" });
    expect(got.models.filter(c => c.setting !== "planning_model" || c.harness !== "codex")).toEqual(before.models.filter(c => c.setting !== "planning_model" || c.harness !== "codex"));
    expect(await mockApi.getEffectiveSettings()).toEqual(before);
  });

  it("the recorded planning scenario carries plan_phase on Claude previews only", () => {
    const planning = RECORDED_EFFECTIVE_SCENARIOS.find((s) => s.name === "planning");
    expect(planning).toBeDefined();
    const byName = (n: string) => planning!.start_previews.find((p) => p.name === n)!.response;
    expect(byName("claude").plan_phase).toEqual({ model: "opus", effort: "high", model_source: "user_planning_default", effort_source: "user_planning_default" });
    expect(byName("codex").plan_phase).toBeUndefined();
    // Every non-planning recording predates the field and stays free of it.
    for (const s of RECORDED_EFFECTIVE_SCENARIOS.filter((x) => x.name !== "planning" && x.name !== "planningCodex")) {
      expect(JSON.stringify(s.start_previews)).not.toContain("plan_phase");
      expect(JSON.stringify(s.schedule_previews)).not.toContain("plan_phase");
    }
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

  // Issue #3258 N3: the recordings carry no lead-template effort; a delivered mock lead
  // template's effort is applied at response time, as leadEffortSettingCell derives it.
  it("shows a delivered lead template's effort with source template when the preference is unset", async () => {
    // The demo ships no lead row and "lead" is reserved at create, so seed a builtin one directly.
    const template = {
      ...templates[0]!, id: "b-lead-3258", name: "lead", scope: "builtin" as const, is_builtin: true,
      model: null, codex_model: null, effort: null, codex_effort: "high",
    };
    templates.push(template);
    try {
      await mockApi.setTemplateAllocations({ my_overrides: [{ template_id: template.id, enabled: true }] });
      const got = await mockApi.getEffectiveSettings();
      const worker = got.efforts.filter((c) => c.setting === "worker_effort");
      expect(worker.find((c) => c.harness === "codex")).toMatchObject({
        effort: null, default_effort: "high", default_source: "template",
        resolved_effort: "high", source: "template", mode: "explicit",
      });
      const recorded = behavior.scenarios[0].get.efforts.find((c) => c.setting === "worker_effort" && c.harness === "claude");
      expect(worker.find((c) => c.harness === "claude")).toEqual(recorded);
      expect(MODEL_SETTING_SOURCE_LABELS.some((l) => l.field === "effort" && l.token === "template")).toBe(true);
    } finally {
      await mockApi.setTemplateAllocations({ my_overrides: [] });
      templates.splice(templates.indexOf(template), 1);
    }
    expect(await mockApi.getEffectiveSettings()).toEqual(behavior.scenarios[0].get);
  });

  it("resolves the stored preference, naming the template only as the default", async () => {
    const template = {
      ...templates[0]!, id: "b-lead-3258b", name: "lead", scope: "builtin" as const, is_builtin: true,
      model: null, codex_model: null, effort: null, codex_effort: "high",
    };
    templates.push(template);
    const recorded = behavior.scenarios[0].get.efforts.find((c) => c.setting === "worker_effort" && c.harness === "codex")!;
    try {
      await mockApi.setTemplateAllocations({ my_overrides: [{ template_id: template.id, enabled: true }] });
      for (const pref of ["low", "runtime"]) {
        await mockApi.putMySettings({ default_codex_effort: pref });
        const got = await mockApi.getEffectiveSettings();
        const codex = got.efforts.find((c) => c.setting === "worker_effort" && c.harness === "codex");
        expect(codex).toEqual({
          ...recorded, default_effort: "high", default_source: "template",
          effort: pref, resolved_effort: pref === "runtime" ? null : pref,
          mode: pref === "runtime" ? "runtime_default" : "explicit", source: "user_default",
        });
      }
    } finally {
      await mockApi.putMySettings({ default_codex_effort: null });
      await mockApi.setTemplateAllocations({ my_overrides: [] });
      templates.splice(templates.indexOf(template), 1);
    }
  });

  it.each(["claude", "codex"] as const)("%s planning effort inherits the lead template unless pinned", async (harness) => {
    const template = {
      ...templates[0]!, id: "b-lead-planning-effort", name: "lead", scope: "builtin" as const, is_builtin: true,
      model: null, codex_model: null, effort: harness === "claude" ? "low" : null, codex_effort: harness === "codex" ? "low" : null,
    };
    templates.push(template);
    try {
      await mockApi.setTemplateAllocations({ my_overrides: [{ template_id: template.id, enabled: true }] });
      const inherited = await mockApi.getEffectiveSettings();
      expect(inherited.efforts.find(c => c.setting === "planning_effort" && c.harness === harness)).toMatchObject({
        default_effort: "low", default_source: "template", resolved_effort: "low", source: "template",
      });
      await mockApi.putMySettings({ [`planning_${harness}_effort`]: "high" });
      const pinned = await mockApi.getEffectiveSettings();
      expect(pinned.efforts.find(c => c.setting === "planning_effort" && c.harness === harness)).toMatchObject({
        default_effort: "low", default_source: "template", resolved_effort: "high", source: "user_planning_default",
      });
    } finally {
      await mockApi.putMySettings({ [`planning_${harness}_effort`]: null });
      await mockApi.setTemplateAllocations({ my_overrides: [] });
      templates.splice(templates.indexOf(template), 1);
    }
  });

  it("returns a copy, so a caller cannot change the recording", async () => {
    const first = await mockApi.getEffectiveSettings();
    first.harnesses.length = 0;
    expect((await mockApi.getEffectiveSettings()).harnesses).toHaveLength(2);
  });
});

describe("recorded planning resets", () => {
  async function reload() {
    vi.resetModules();
    return (await import("./mockApi")).mockApi;
  }

  function expectOtherCellsUnchanged(got: EffectiveSettings, before: EffectiveSettings, harness: Harness, workerChanged = false) {
    const otherCells = (view: EffectiveSettings) => ({
      ...view,
      models: view.models.filter(c => c.harness !== harness || (c.setting !== "planning_model" && !(workerChanged && c.setting === "worker_model"))),
      efforts: view.efforts.filter(c => c.harness !== harness || (c.setting !== "planning_effort" && !(workerChanged && c.setting === "worker_effort"))),
    });
    expect(otherCells(got)).toEqual(otherCells(before));
  }

  afterEach(() => vi.resetModules());

  it("serves every untouched GET recording verbatim, including both planning scenarios", async () => {
    const scenarios: Record<string, string> = { default: "", codexOnly: "codex-only", overflowCodexDefault: "overflow-codex-default", planning: "planning-pair", planningCodex: "planning-codex-pair" };
    for (const s of behavior.scenarios) {
      setScenario(scenarios[s.name]);
      const api = await reload();
      expect(await api.getEffectiveSettings(), s.name).toEqual(s.get);
      expect((await api.getMySettings()).settings).toMatchObject({
        planning_claude_model: null, planning_claude_effort: null,
        planning_codex_model: null, planning_codex_effort: null,
      });
    }
  });

  for (const { scenario, harness, workerModel, workerEffort, effortSource, mode } of [
    { scenario: "planning-pair", harness: "claude", workerModel: "opus", workerEffort: "medium", effortSource: "builtin", mode: "explicit" },
    { scenario: "planning-codex-pair", harness: "codex", workerModel: "gpt-6-sol", workerEffort: null, effortSource: "user_default", mode: "runtime_default" },
  ] as const) {
    describe(scenario, () => {
      beforeEach(() => setScenario(scenario));

      it.each([false, true])("clears the model while retaining effort (explicit effort: %s)", async (saveEffort) => {
        const api = await reload();
        const before = await api.getEffectiveSettings();
        await api.putMySettings({
          [`planning_${harness}_model`]: null,
          ...(saveEffort ? { [`planning_${harness}_effort`]: "high" } : {}),
        });
        const got = await api.getEffectiveSettings();
        expect(got.models.find(c => c.setting === "planning_model" && c.harness === harness))
          .toMatchObject({ model: null, resolved_model: workerModel, source: "template" });
        expect(got.efforts).toEqual(before.efforts);
        expectOtherCellsUnchanged(got, before, harness);
      });

      it("clears only effort, inheriting the worker resolution and runtime mode", async () => {
        const api = await reload();
        const before = await api.getEffectiveSettings();
        await api.putMySettings({ [`planning_${harness}_effort`]: null });
        const got = await api.getEffectiveSettings();
        expect(got.efforts.find(c => c.setting === "planning_effort" && c.harness === harness))
          .toMatchObject({ effort: null, resolved_effort: workerEffort, source: effortSource, mode });
        expect(got.models).toEqual(before.models);
        expectOtherCellsUnchanged(got, before, harness);
      });

      it("clears both fields and preserves the saved reset through a storage-backed reload", async () => {
        const api = await reload();
        const before = await api.getEffectiveSettings();
        const reset = { [`planning_${harness}_model`]: null, [`planning_${harness}_effort`]: null };
        await api.putMySettings(reset);
        expect(JSON.parse(localStorage.getItem("uzi.mock.v4")!).userSettings).toMatchObject(reset);
        const saved = await api.getEffectiveSettings();
        expect(saved.models.find(c => c.setting === "planning_model" && c.harness === harness))
          .toMatchObject({ model: null, resolved_model: workerModel, source: "template" });
        expect(saved.efforts.find(c => c.setting === "planning_effort" && c.harness === harness))
          .toMatchObject({ effort: null, resolved_effort: workerEffort, source: effortSource, mode });
        expectOtherCellsUnchanged(saved, before, harness);

        const reloaded = await reload();
        expect((await reloaded.getMySettings()).settings).toMatchObject(reset);
        expect(await reloaded.getEffectiveSettings()).toEqual(saved);
      });

      it.each(["low", "runtime"])("inherits an explicit ordinary user lane (%s) when both planning fields clear", async (effort) => {
        const api = await reload();
        const before = await api.getEffectiveSettings();
        const model = harness === "claude" ? "sonnet" : "custom-worker";
        await api.putMySettings({
          [`default_${harness}_model`]: model,
          [harness === "claude" ? "default_effort" : "default_codex_effort"]: effort,
          [`planning_${harness}_model`]: null,
          [`planning_${harness}_effort`]: null,
        });
        const got = await api.getEffectiveSettings();
        expect(got.models.find(c => c.setting === "planning_model" && c.harness === harness))
          .toMatchObject({ model: null, resolved_model: model, source: "user_default" });
        expect(got.efforts.find(c => c.setting === "planning_effort" && c.harness === harness))
          .toMatchObject({ effort: null, resolved_effort: effort === "runtime" ? null : "low", source: "user_default", mode: effort === "runtime" ? "runtime_default" : "explicit" });
        expect(got.models.find(c => c.setting === "worker_model" && c.harness === harness))
          .toMatchObject({ model, resolved_model: model, source: "user_default" });
        expect(got.efforts.find(c => c.setting === "worker_effort" && c.harness === harness))
          .toMatchObject({ effort, resolved_effort: effort === "runtime" ? null : effort, source: "user_default", mode: effort === "runtime" ? "runtime_default" : "explicit" });
        expectOtherCellsUnchanged(got, before, harness, true);
      });
    });
  }
});

describe("previewStartSettings", () => {
  it("answers every recorded request with its recorded response, in each scenario", async () => {
    const scenarios: Record<string, string> = { default: "", codexOnly: "codex-only", overflowCodexDefault: "overflow-codex-default", planning: "planning-pair", planningCodex: "planning-codex-pair" };
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
  const scenarios: Record<string, string> = { default: "", codexOnly: "codex-only", overflowCodexDefault: "overflow-codex-default", planning: "planning-pair", planningCodex: "planning-codex-pair" };

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
