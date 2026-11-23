// @vitest-environment jsdom
//
// PRD #3263: the mock PUT /me/model-settings follows the real handler's atomic contract:
// the WHOLE body validates first, every defect comes back in one 400 shaped exactly as the
// real client sees it (ApiError with {error, field, errors}), nothing is written unless all of
// it is valid, and a valid save answers {settings, effective}. A one-shot hook injects the
// server's write failures.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { ApiError } from "../lib/apiError";

async function reload() {
  vi.resetModules();
  const mod = await import("./mockApi");
  const settings = await import("./mockApi/settings");
  return { api: mod.mockApi, failNext: settings.failNextMockModelSettingsWrite };
}
beforeEach(() => { localStorage.clear(); vi.resetModules(); });
afterEach(() => vi.resetModules());

async function rejection(p: Promise<unknown>): Promise<ApiError> {
  try {
    await p;
  } catch (err) {
    // Each test reloads the module graph, so the mock's ApiError is not this file's class:
    // check the shape the real client exposes instead of instanceof.
    if (err instanceof Error && err.name === "ApiError") return err as ApiError;
    throw err;
  }
  throw new Error("expected the save to be refused");
}

describe("mock putMyModelSettings", () => {
  it("saves every field together and answers canonical settings plus the effective projection", async () => {
    const { api } = await reload();
    const res = await api.putMyModelSettings({
      default_harness: "codex",
      default_codex_model: " gpt-6-sol ",
      default_effort: "runtime",
      planning_claude_effort: "xhigh",
      planning_codex_model: "custom-plan",
    });
    expect(res.settings).toMatchObject({
      default_harness: "codex", default_codex_model: "gpt-6-sol", default_effort: "runtime",
      planning_claude_effort: "xhigh", planning_codex_model: "custom-plan",
    });
    expect(res.effective).toEqual(await api.getEffectiveSettings());
    expect(res.effective?.efforts.find((c) => c.setting === "planning_effort" && c.harness === "claude"))
      .toMatchObject({ effort: "xhigh", source: "user_planning_default" });
    expect((await api.getMySettings()).settings).toMatchObject({ default_harness: "codex", default_codex_model: "gpt-6-sol" });
  });

  it("null clears an override and omitted keys stay unchanged", async () => {
    const { api } = await reload();
    await api.putMyModelSettings({ default_claude_model: "sonnet", default_codex_effort: "high" });
    const { settings } = await api.putMyModelSettings({ default_claude_model: null });
    expect(settings.default_claude_model).toBeNull();
    expect(settings.default_codex_effort).toBe("high");
  });

  it("reports EVERY defect in one 400, first one flat, and writes nothing", async () => {
    const { api } = await reload();
    const before = (await api.getMySettings()).settings;
    const err = await rejection(api.putMyModelSettings({
      default_claude_model: "sonnet", // valid, but must not be written
      default_codex_model: "opus",
      default_harness: "gemini",
      planning_claude_effort: "runtime",
      cross_check_pins: [{ stage: "plan", harness: "codex", effort: "turbo" }],
    }));
    expect(err.status).toBe(400);
    const body = err.body as { error: string; field: string; errors: { field: string; message: string; stage?: string; harness?: string; member?: string }[] };
    expect(body.errors.map((e) => e.field)).toEqual(["default_codex_model", "default_harness", "planning_claude_effort", "cross_check_pins"]);
    expect(body.field).toBe("default_codex_model");
    expect(body.error).toBe(body.errors[0].message);
    expect(err.message).toBe(body.errors[0].message);
    expect(body.errors[2].message).toContain("planning_claude_effort");
    expect(body.errors[3]).toMatchObject({ stage: "plan", harness: "codex", member: "effort" });
    expect((await api.getMySettings()).settings).toEqual(before);
  });

  it("refuses unknown keys, including the legacy default_model, before anything else", async () => {
    const { api } = await reload();
    const err = await rejection(api.putMyModelSettings({ default_model: "opus", theme: "dawn", default_effort: "nope" } as never));
    const body = err.body as { errors: { field: string }[] };
    expect(body.errors.map((e) => e.field)).toEqual(["default_model", "theme"]);
  });

  it("rejects a Codex model containing spaces without applying other fields", async () => {
    const { api } = await reload();
    const before = (await api.getMySettings()).settings;
    const err = await rejection(api.putMyModelSettings({ default_codex_model: "two words", planning_codex_effort: "high" }));
    expect(err.body).toMatchObject({ field: "default_codex_model" });
    expect((await api.getMySettings()).settings).toEqual(before);
  });

  it("returns the saved worker model and effort in effective predictions", async () => {
    const { api } = await reload();
    const { effective } = await api.putMyModelSettings({ default_codex_model: "custom-codex", default_codex_effort: "runtime" });
    expect(effective?.models.find(c => c.setting === "worker_model" && c.harness === "codex"))
      .toMatchObject({ resolved_model: "custom-codex", source: "user_default" });
    expect(effective?.efforts.find(c => c.setting === "worker_effort" && c.harness === "codex"))
      .toMatchObject({ resolved_effort: null, mode: "runtime_default", source: "user_default" });
  });

  it("accepts Runtime default on worker effort but not on planning effort", async () => {
    const { api } = await reload();
    const result = await api.putMyModelSettings({ default_codex_effort: "runtime" });
    expect(result.settings.default_codex_effort).toBe("runtime");
    expect((await api.getMySettings()).settings.default_codex_effort).toBe("runtime");
    const err = await rejection(api.putMyModelSettings({ planning_codex_effort: "runtime" }));
    expect((err.body as { field: string }).field).toBe("planning_codex_effort");
  });

  it.each(["write_failed", "commit_unknown"] as const)("an injected %s leaves every value unchanged", async (failure) => {
    const { api, failNext } = await reload();
    const before = (await api.getMySettings()).settings;
    failNext(failure);
    const err = await rejection(api.putMyModelSettings({ default_harness: "claude", default_claude_model: "haiku" }));
    expect(err.status).toBe(500);
    expect((err.body as { reason?: string }).reason).toBe(failure === "commit_unknown" ? "settings_commit_unknown" : undefined);
    expect((await api.getMySettings()).settings).toEqual(before);
    // One-shot: the next save goes through.
    await expect(api.putMyModelSettings({ default_claude_model: "haiku" })).resolves.toBeTruthy();
  });

  it("an injected saved_readback_failed is a durable write answered by a 500", async () => {
    const { api, failNext } = await reload();
    failNext("saved_readback_failed");
    const err = await rejection(api.putMyModelSettings({ default_claude_model: "haiku" }));
    expect((err.body as { reason?: string }).reason).toBe("settings_saved_readback_failed");
    expect((await api.getMySettings()).settings.default_claude_model).toBe("haiku");
  });

  it("an injected effective_readback_failed still commits and answers effective null", async () => {
    const { api, failNext } = await reload();
    failNext("effective_readback_failed");
    const res = await api.putMyModelSettings({ default_claude_model: "haiku" });
    expect(res).toMatchObject({ effective: null, effective_error: "settings_effective_readback_failed" });
    expect(res.settings.default_claude_model).toBe("haiku");
  });

  // PRD #3263 M2: the helper lanes and the four checker cells validate like the server, all
  // before anything is written, and pin errors carry their stage/harness/member.
  it("saves helper and checker choices with worker ones in one step", async () => {
    const { api } = await reload();
    const { settings } = await api.putMyModelSettings({
      default_codex_model: "gpt-6-sol",
      judge_runs_on: "claude", judge_model: "sonnet", codex_judge_model: "gpt-6-astra",
      summary_model: "haiku", codex_summary_model: "gpt-6-luna",
      task_review_model: "custom-reviewer", codex_task_review_model: "gpt-6-sol",
      task_review_effort: "runtime", codex_task_review_effort: "high",
      cross_check_pins: [
        { stage: "plan", harness: "claude", effort: "max" },
        { stage: "code", harness: "codex", model: " custom-codex-checker " },
      ],
    });
    expect(settings).toMatchObject({
      default_codex_model: "gpt-6-sol",
      judge_runs_on: "claude", judge_model: "sonnet", codex_judge_model: "gpt-6-astra",
      summary_model: "haiku", codex_summary_model: "gpt-6-luna",
      task_review_model: "custom-reviewer", codex_task_review_model: "gpt-6-sol",
      task_review_effort: "runtime", codex_task_review_effort: "high",
    });
    const pin = (stage: string, harness: string) => settings.cross_check_pins?.find((p) => p.stage === stage && p.harness === harness);
    expect(pin("plan", "claude")).toMatchObject({ model: null, effort: "max" });
    expect(pin("code", "codex")).toMatchObject({ model: "custom-codex-checker", effort: null });
    expect(pin("plan", "codex")).toMatchObject({ model: null, effort: null });
  });

  it("refuses every helper and pin defect in one 400 with cell identity, writing nothing", async () => {
    const { api } = await reload();
    await api.putMyModelSettings({ judge_model: "opus", cross_check_pins: [{ stage: "code", harness: "claude", effort: "low" }] });
    const before = (await api.getMySettings()).settings;
    const err = await rejection(api.putMyModelSettings({
      default_claude_model: "haiku", // valid, but must not be written
      judge_runs_on: "gemini",
      judge_model: "gpt-6-sol", // a Codex id in the Claude lane
      codex_judge_model: "gpt-6-luna", // a Codex role model, but not in the curated judge set
      summary_model: "bad\u202e", // a format character, as ValidateModel refuses
      codex_summary_model: "sonnet",
      task_review_model: "two words",
      codex_task_review_model: "custom-codex",
      codex_task_review_effort: "turbo",
      cross_check_pins: [
        { stage: "plan", harness: "claude", model: "gpt-6.1-sol", effort: "runtime" },
        { stage: "code", harness: "codex", model: "opus" },
      ],
    }));
    const body = err.body as { field: string; errors: { field: string; message: string; stage?: string; harness?: string; member?: string }[] };
    expect(body.errors.map((e) => e.field)).toEqual([
      "judge_model", "codex_judge_model", "summary_model", "codex_summary_model", "task_review_model",
      "codex_task_review_model", "codex_task_review_effort", "judge_runs_on",
      "cross_check_pins", "cross_check_pins", "cross_check_pins",
    ]);
    expect(body.field).toBe("judge_model");
    expect(body.errors[0].message).toBe("judge_model: model is a Codex model id");
    expect(body.errors[2].message).toContain("summary_model:");
    expect(body.errors.slice(8).map(({ stage, harness, member }) => ({ stage, harness, member }))).toEqual([
      { stage: "plan", harness: "claude", member: "model" },
      { stage: "plan", harness: "claude", member: "effort" },
      { stage: "code", harness: "codex", member: "model" },
    ]);
    expect((await api.getMySettings()).settings).toEqual(before);
  });
});
