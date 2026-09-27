// @vitest-environment jsdom
//
// PRD #1732: the mock's Disable / Enable is a second implementation of PatchSecretEnabled
// and of the read filters (D4, D9, D11, D14), so the demo cannot teach a rule the server
// does not have. Each test reloads the module so the in-memory store re-seeds.
import { afterEach, describe, expect, it, vi } from "vitest";

async function fresh() {
  vi.resetModules();
  return (await import("./mockApi")).mockApi;
}

afterEach(() => {
  vi.useRealTimers();
});

describe("mock setSecretEnabled (PRD #1732 D4, D11)", () => {
  it("refuses to disable the default without an enabled replacement while one exists (409)", async () => {
    const api = await fresh();
    await expect(api.setSecretEnabled("anthropic_token", "sec-default", false)).rejects.toMatchObject({ status: 409 });
    // A disabled replacement is refused too.
    await expect(
      api.setSecretEnabled("anthropic_token", "sec-default", false, "sec-old-laptop"),
    ).rejects.toMatchObject({ status: 409 });
  });

  it("hands the default over atomically, keeps disabled_at on a repeat, and 404s a wrong kind", async () => {
    const api = await fresh();
    const { secret } = await api.setSecretEnabled("anthropic_token", "sec-default", false, "sec-console");
    expect(secret.enabled).toBe(false);
    expect(secret.is_default).toBe(false);
    const { secrets } = await api.listSecrets();
    expect(secrets.find((s) => s.id === "sec-console")?.is_default).toBe(true);
    const again = await api.setSecretEnabled("anthropic_token", "sec-default", false);
    expect(again.secret.disabled_at).toBe(secret.disabled_at);
    await expect(api.setSecretEnabled("codex_auth", "sec-default", false)).rejects.toMatchObject({ status: 404 });
  });

  it("clears the default on the slot's last enabled credential, and Enable makes it default again", async () => {
    const api = await fresh();
    const { secrets } = await api.listSecrets();
    const anthropic = secrets.filter((s) => s.kind === "anthropic_token" && s.enabled);
    for (const s of anthropic.filter((s) => !s.is_default)) {
      await api.setSecretEnabled("anthropic_token", s.id, false);
    }
    const last = await api.setSecretEnabled("anthropic_token", "sec-default", false);
    expect(last.secret.is_default).toBe(false);
    const after = (await api.listSecrets()).secrets.filter((s) => s.kind === "anthropic_token");
    expect(after.some((s) => s.is_default)).toBe(false);
    const back = await api.setSecretEnabled("anthropic_token", "sec-console", true);
    expect(back.secret.is_default).toBe(true);
  });
});

// The server forces a NEW credential to be the default while its slot has no default row
// (every credential disabled): InsertUserSecret's NOT EXISTS for Anthropic, and
// `req.Default || n == 0 || !hasDefault` for the shared Codex slot.
describe("mock create into a slot with no default (PRD #1732 D4)", () => {
  async function disableSlot(api: Awaited<ReturnType<typeof fresh>>, kinds: string[]) {
    const { secrets } = await api.listSecrets();
    const live = secrets.filter((s) => kinds.includes(s.kind) && s.enabled);
    for (const s of live.filter((s) => !s.is_default)) await api.setSecretEnabled(s.kind, s.id, false);
    for (const s of live.filter((s) => s.is_default)) await api.setSecretEnabled(s.kind, s.id, false);
  }

  it("makes a new Anthropic token the default, but does not pool it (not the first row)", async () => {
    const api = await fresh();
    await disableSlot(api, ["anthropic_token"]);
    const { secret } = await api.createAnthropicToken("sk-ant-x", "fresh-key", false);
    expect(secret.is_default).toBe(true);
    expect(secret.auto_eligible).toBe(false);
  });

  it("makes a new Codex credential the shared default", async () => {
    const api = await fresh();
    await disableSlot(api, ["codex_auth", "openai_api_key"]);
    const { secret } = await api.createOpenAIApiKey("sk-x", "fresh-openai", false);
    expect(secret.is_default).toBe(true);
  });
});

describe("mock read filters (PRD #1732 D1, D9, D13)", () => {
  it("omits a disabled token from the owner and admin meters, and shows it again after a fresh reading", async () => {
    const api = await fresh();
    const mine = await api.getMyRateLimits();
    expect(mine.tokens.some((t) => t.secret_id === "sec-old-laptop")).toBe(false);
    const admin = await api.getAdminRateLimits();
    expect(admin.users.flatMap((u) => u.tokens).some((t) => t.secret_id === "sec-old-laptop")).toBe(false);

    await api.setSecretEnabled("anthropic_token", "sec-console", false);
    expect((await api.getMyRateLimits()).tokens.some((t) => t.secret_id === "sec-console")).toBe(false);

    vi.useFakeTimers({ shouldAdvanceTime: true });
    await api.setSecretEnabled("anthropic_token", "sec-console", true);
    const checking = (await api.getMyRateLimits()).tokens.find((t) => t.secret_id === "sec-console");
    // Back, but its pre-disable reading is not current yet.
    expect(checking?.limits.status).toBe("unavailable");
    vi.advanceTimersByTime(5_000);
    const fresh2 = (await api.getMyRateLimits()).tokens.find((t) => t.secret_id === "sec-console");
    expect(fresh2?.limits.status).toBe("ok");
  });

  it("drops a disabled Codex alias from its account, keeping the account live through its sibling", async () => {
    const api = await fresh();
    const { accounts } = await api.getMyCodexRateLimits();
    const team = accounts.find((a) => a.account_id === "cdx-acct-team");
    expect(team?.aliases).toContain("team-codex");
    const deps = await api.getSecretDependents("codex_auth", "sec-codex-team-laptop");
    expect(deps.enabled_siblings.items.map((s) => s.label)).toEqual(["team-codex"]);
  });

  it("promotes a held run when the credential it waits on is enabled (D14)", async () => {
    const api = await fresh();
    expect((await api.getRun("run-cred-disabled")).run.status).toBe("paused");
    await api.setSecretEnabled("anthropic_token", "sec-old-laptop", true);
    const { run } = await api.getRun("run-cred-disabled");
    expect(run.status).toBe("queued");
    expect(run.hold_reason).toBeNull();
  });
});

// PRD #1732: the server refuses to promote a disabled credential (default or auto-select
// pool) with ErrCredentialDisabled, and the delete-default guard counts ENABLED rows only.
describe("mock promotion and delete-default guards (PRD #1732 D5, D12)", () => {
  const DISABLED = "credential is disabled; enable it in Settings";

  it("refuses to make a disabled token the default or pool it, and writes nothing", async () => {
    const api = await fresh();
    await expect(
      api.patchAnthropicToken("sec-old-laptop", { label: "renamed", default: true }),
    ).rejects.toMatchObject({ status: 409, message: DISABLED });
    await expect(api.setTokenAutoEligible("sec-old-laptop", true)).rejects.toMatchObject({
      status: 409,
      message: DISABLED,
    });
    const row = (await api.listSecrets()).secrets.find((s) => s.id === "sec-old-laptop");
    expect(row?.label).not.toBe("renamed");
    expect(row?.is_default).toBe(false);
    // Opting OUT of the pool, and a plain rename, stay allowed on a disabled row.
    await expect(api.setTokenAutoEligible("sec-old-laptop", false)).resolves.toBeTruthy();
    await expect(api.patchAnthropicToken("sec-old-laptop", { label: "renamed" })).resolves.toBeTruthy();
  });

  it("refuses to make a disabled Codex credential the shared default", async () => {
    const api = await fresh();
    await expect(api.patchCodexAuth("sec-codex-team-laptop", { default: true })).rejects.toMatchObject({
      status: 409,
      message: DISABLED,
    });
  });

  it("deletes the default when every other token is disabled", async () => {
    const api = await fresh();
    const { secrets } = await api.listSecrets();
    for (const s of secrets.filter((s) => s.kind === "anthropic_token" && s.enabled && !s.is_default)) {
      await api.setSecretEnabled("anthropic_token", s.id, false);
    }
    await expect(api.deleteAnthropicTokenById("sec-default")).resolves.toBeNull();
  });

  it("refuses to delete the default while another enabled token exists, with the server's text", async () => {
    const api = await fresh();
    await expect(api.deleteAnthropicTokenById("sec-default")).rejects.toMatchObject({
      status: 409,
      message: "cannot delete the default token while other enabled tokens exist; set another token as default first",
    });
  });
});
