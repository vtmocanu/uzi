import { afterEach, expect, it, vi } from "vitest";

afterEach(() => vi.resetModules());

it.each([true, false])("code changes preserve plan=%s in mock state and response", async (plan) => {
  vi.resetModules();
  const { mockApi } = await import("./mockApi");
  const { state } = await import("./store");
  expect(state.session).not.toBeNull();
  state.session!.plan_cross_check_enabled = plan;
  for (const code of [true, false]) {
    const result = await mockApi.setCodeCrossCheckEnabled(code);
    expect(result.user).toMatchObject({ code_cross_check_enabled: code, plan_cross_check_enabled: plan });
    expect(state.session).toMatchObject({ code_cross_check_enabled: code, plan_cross_check_enabled: plan });
  }
});

it.each([true, false])("plan changes preserve code=%s in mock state and response", async (code) => {
  vi.resetModules();
  const { mockApi } = await import("./mockApi");
  const { state } = await import("./store");
  expect(state.session).not.toBeNull();
  state.session!.code_cross_check_enabled = code;
  for (const plan of [true, false]) {
    const result = await mockApi.setPlanCrossCheckEnabled(plan);
    expect(result.user).toMatchObject({ code_cross_check_enabled: code, plan_cross_check_enabled: plan });
    expect(state.session).toMatchObject({ code_cross_check_enabled: code, plan_cross_check_enabled: plan });
  }
});
