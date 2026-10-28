// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { mockApi } from "./mockApi";
import { mockUsers } from "./data";
import { mockAdminCodexRateLimits, mockMyCodexRateLimitsByUser } from "./data/codexRateLimits";
import { appSettings } from "./mockApi/settings";
import { secrets } from "./mockApi/secrets";
import { state } from "./store";

function makeStorage(): Storage {
  const values = new Map<string, string>();
  return {
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => void values.set(key, String(value)),
    removeItem: (key) => void values.delete(key),
    clear: () => values.clear(),
    key: (index) => [...values.keys()][index] ?? null,
    get length() { return values.size; },
  };
}

let savedState: typeof state;
let savedSettings: typeof appSettings;
let savedSecrets: typeof secrets;
let savedUrl: string;
let savedRaduAccounts: typeof mockMyCodexRateLimitsByUser[string];

beforeEach(() => {
  savedState = { ...state };
  savedSettings = { ...appSettings };
  savedSecrets = secrets.map((secret) => ({ ...secret }));
  savedUrl = window.location.href;
  savedRaduAccounts = [...mockMyCodexRateLimitsByUser["u-radu"]];
  vi.stubGlobal("localStorage", makeStorage());
  window.history.replaceState(null, "", window.location.pathname);
  state.session = { ...mockUsers.find((user) => user.id === "u-admin")! };
  state.vaultMode = "protected";
  state.vaultExists = undefined;
  state.vaultUnlocked = true;
  appSettings.vault_policy = "required";
});

afterEach(() => {
  delete state.vaultExists;
  Object.assign(state, savedState);
  Object.assign(appSettings, savedSettings);
  secrets.splice(0, secrets.length, ...savedSecrets);
  const raduAccounts = mockMyCodexRateLimitsByUser["u-radu"];
  raduAccounts.splice(0, raduAccounts.length, ...savedRaduAccounts);
  window.history.replaceState(null, "", savedUrl);
  window.localStorage.clear();
  vi.unstubAllGlobals();
});

function usePersona(id: string) {
  state.session = { ...mockUsers.find((user) => user.id === id)! };
}

async function ownAdminRow() {
  return (await mockApi.getAdminCodexRateLimits()).users.find((user) => user.id === state.session?.id)!;
}

describe("mock vault mode API", () => {
  it("defaults to protected and required, refusing disable without changing material", async () => {
    const before = await mockApi.vaultStatus();
    expect(before).toEqual({ mode: "protected", policy: "required", exists: true, unlocked: true });
    await expect(mockApi.vaultDisable()).rejects.toMatchObject({ status: 403 });
    expect(await mockApi.vaultStatus()).toEqual(before);
    const accounts = (await mockApi.getMyCodexRateLimits()).accounts;
    expect(accounts[0].status).toBe("fresh");
    await mockApi.vaultLock();
    const expected = accounts.map((account) => ({ ...account, status: "vault_locked" }));
    expect((await mockApi.getMyCodexRateLimits()).accounts).toEqual(expected);
    expect((await ownAdminRow()).accounts.every((account) => account.status === "vault_locked")).toBe(true);
  });

  it("requires unlock before optional disable and clears locked Codex projections only for the owner", async () => {
    usePersona("u-radu");
    appSettings.vault_policy = "optional";
    await mockApi.vaultLock();
    const locked = await mockApi.vaultStatus();
    await expect(mockApi.vaultDisable()).rejects.toMatchObject({ status: 409 });
    expect(await mockApi.vaultStatus()).toEqual(locked);
    const selfLocked = (await mockApi.getMyCodexRateLimits()).accounts;
    expect(selfLocked).toHaveLength(1);
    expect(selfLocked[0].status).toBe("vault_locked");
    expect(await ownAdminRow()).toMatchObject({ vault_mode: "protected", vault_locked: true });
    const credentials = (await mockApi.listSecrets()).secrets;

    await mockApi.vaultUnlock("demo password");
    expect((await mockApi.getMyCodexRateLimits()).accounts[0].status).toBe("fresh");
    expect((await ownAdminRow()).accounts[0].status).toBe("fresh");
    await expect(mockApi.vaultDisable()).resolves.toEqual({ mode: "unprotected" });
    const off = { mode: "unprotected", policy: "optional", exists: false, unlocked: false };
    expect(await mockApi.vaultStatus()).toEqual(off);
    expect((await mockApi.me()).vault).toEqual(off);
    const admin = await mockApi.getAdminCodexRateLimits();
    const own = admin.users.find((user) => user.id === "u-radu")!;
    expect(own).toMatchObject({ vault_mode: "unprotected", vault_locked: false });
    expect(own.accounts).toEqual(selfLocked.map((account) => ({ ...account, status: "fresh" })));
    expect((await mockApi.getMyCodexRateLimits()).accounts).toEqual(own.accounts);
    expect(admin.users.find((user) => user.id === "u-mira")).toEqual(
      mockAdminCodexRateLimits.find((user) => user.id === "u-mira"),
    );
    await mockApi.vaultUnlock("");
    expect(await mockApi.vaultStatus()).toEqual(off);
    expect(state.vaultExists).toBe(false);
    expect(state.vaultUnlocked).toBe(false);
    expect((await mockApi.listSecrets()).secrets).toEqual(credentials);
  });

  it.each(["stale", "pending"] as const)(
    "recovers a locked fixture to %s using only existing reading metadata",
    async (status) => {
      usePersona("u-radu");
      appSettings.vault_policy = "optional";
      const accounts = mockMyCodexRateLimitsByUser["u-radu"];
      const { last_success_at: lastSuccess, ...withoutSuccess } = accounts[0];
      accounts[0] = status === "stale"
        ? { ...accounts[0], stale: true }
        : { ...withoutSuccess, buckets: [] };
      expect(lastSuccess).toBeTruthy();
      await mockApi.vaultDisable();
      const expected = [{ ...accounts[0], status }];
      expect((await mockApi.getMyCodexRateLimits()).accounts).toEqual(expected);
      expect((await ownAdminRow()).accounts).toEqual(expected);
    },
  );

  it("hides provider-action reasons while locked and restores them after unlock", async () => {
    usePersona("u-radu");
    const accounts = mockMyCodexRateLimitsByUser["u-radu"];
    accounts[0] = { ...accounts[0], status: "credential_action_required", reason: "provider_rejected" };
    await mockApi.vaultLock();
    const locked = (await mockApi.getMyCodexRateLimits()).accounts[0];
    expect(locked.status).toBe("vault_locked");
    expect(locked.reason).toBeUndefined();
    expect((await ownAdminRow()).accounts[0]).toEqual(locked);
    await mockApi.vaultUnlock("demo password");
    expect((await mockApi.getMyCodexRateLimits()).accounts[0]).toMatchObject({
      status: "credential_action_required", reason: "provider_rejected",
    });
  });

  it("gates a credential save while protected and locked but permits it while Off", async () => {
    appSettings.vault_policy = "optional";
    await mockApi.vaultLock();
    await expect(mockApi.createOpenAIApiKey("demo-key", "vault-mode-test", false)).rejects.toMatchObject({
      status: 409,
      body: { code: "vault_locked" },
    });
    await mockApi.vaultUnlock("demo password");
    await mockApi.vaultDisable();
    const { secret } = await mockApi.createOpenAIApiKey("demo-key", "vault-mode-test", false);
    expect(secret).toMatchObject({ kind: "openai_api_key", label: "vault-mode-test", enabled: true });
    expect((await mockApi.listSecrets()).secrets.some((row) => row.id === secret.id)).toBe(true);
    expect((await mockApi.vaultStatus()).mode).toBe("unprotected");
  });

  it("Enable refuses a blank mock password without changes and returns rotation advice on enrollment", async () => {
    state.vaultMode = "unprotected";
    state.vaultExists = false;
    state.vaultUnlocked = false;
    const before = await mockApi.vaultStatus();
    await expect(mockApi.vaultEnable("  ")).rejects.toMatchObject({ status: 403 });
    expect(await mockApi.vaultStatus()).toEqual(before);
    const result = await mockApi.vaultEnable("synthetic demo password");
    expect(result.mode).toBe("protected");
    expect(result.rotation_advice).toContain("Rotate");
    expect(await mockApi.vaultStatus()).toEqual({
      mode: "protected", policy: "required", exists: true, unlocked: true,
    });
    await mockApi.vaultLock();
    await expect(mockApi.vaultEnable("another password")).rejects.toMatchObject({ status: 409 });
    expect((await mockApi.vaultStatus()).unlocked).toBe(false);
  });

  it("bounds OIDC Enable passphrases before changing mode or creating material", async () => {
    window.localStorage.setItem("uzi_mock_scenario", "oidc");
    state.vaultMode = "unprotected";
    state.vaultExists = false;
    state.vaultUnlocked = false;
    const before = await mockApi.vaultStatus();
    for (const length of [11, 1025]) {
      await expect(mockApi.vaultEnable("p".repeat(length))).rejects.toMatchObject({ status: 400 });
      expect(await mockApi.vaultStatus()).toEqual(before);
    }
    await expect(mockApi.vaultEnable("p".repeat(12))).resolves.toMatchObject({ mode: "protected" });
    expect((await mockApi.me()).vault).toMatchObject({ exists: true, unlocked: true });
  });

  it.each(["vaultEnable", "vaultCreatePassphrase"] as const)(
    "%s bounds OIDC passphrases by UTF-8 bytes",
    async (method) => {
      window.localStorage.setItem("uzi_mock_scenario", "oidc");
      state.vaultMode = "unprotected";
      state.vaultExists = false;
      state.vaultUnlocked = false;
      const before = await mockApi.vaultStatus();
      for (const length of [5, 513]) {
        await expect(mockApi[method]("é".repeat(length))).rejects.toMatchObject({ status: 400 });
        expect(await mockApi.vaultStatus()).toEqual(before);
        expect(state.vaultMode).toBe("unprotected");
        expect(state.vaultExists).toBe(false);
        expect(state.vaultUnlocked).toBe(false);
      }
      for (const length of [6, 512]) {
        state.vaultMode = "unprotected";
        state.vaultExists = false;
        state.vaultUnlocked = false;
        await mockApi[method]("é".repeat(length));
        expect(await mockApi.vaultStatus()).toEqual({
          mode: "protected", policy: "required", exists: true, unlocked: true,
        });
        expect(state.vaultMode).toBe("protected");
        expect(state.vaultExists).toBe(true);
        expect(state.vaultUnlocked).toBe(true);
      }
    },
  );

  it("protected OIDC unlock without a wrapper returns the generic password error without changes", async () => {
    window.localStorage.setItem("uzi_mock_scenario", "oidc");
    state.vaultMode = "protected";
    state.vaultExists = false;
    state.vaultUnlocked = false;
    const before = await mockApi.vaultStatus();
    await expect(mockApi.vaultUnlock("demo password")).rejects.toMatchObject({
      status: 403, message: "incorrect password",
    });
    expect(await mockApi.vaultStatus()).toEqual(before);
    expect(state.vaultMode).toBe("protected");
    expect(state.vaultExists).toBe(false);
    expect(state.vaultUnlocked).toBe(false);
  });

  it("legacy passphrase enrollment rejects password accounts even without a wrapper", async () => {
    state.vaultExists = false;
    const before = await mockApi.vaultStatus();
    await expect(mockApi.vaultCreatePassphrase("p".repeat(12))).rejects.toMatchObject({ status: 409 });
    expect(await mockApi.vaultStatus()).toEqual(before);
  });

  it.each(["protected", "unprotected"] as const)(
    "legacy OIDC passphrase enrollment from %s is bounded and create-only",
    async (mode) => {
      window.localStorage.setItem("uzi_mock_scenario", "oidc");
      state.vaultMode = mode;
      state.vaultExists = false;
      state.vaultUnlocked = false;
      const before = await mockApi.vaultStatus();
      for (const length of [11, 1025]) {
        await expect(mockApi.vaultCreatePassphrase("p".repeat(length))).rejects.toMatchObject({ status: 400 });
        expect(await mockApi.vaultStatus()).toEqual(before);
      }
      await expect(mockApi.vaultCreatePassphrase("p".repeat(1024))).resolves.toBeNull();
      const enrolled = { mode: "protected", policy: "required", exists: true, unlocked: true };
      expect(await mockApi.vaultStatus()).toEqual(enrolled);
      await expect(mockApi.vaultCreatePassphrase("q".repeat(12))).rejects.toMatchObject({ status: 409 });
      expect(await mockApi.vaultStatus()).toEqual(enrolled);
    },
  );

  it("preserves polling_disabled before the vault gate and leaves other owners' locked fixtures intact", async () => {
    usePersona("u-mihai");
    appSettings.vault_policy = "optional";
    await mockApi.vaultLock();
    expect((await mockApi.getMyCodexRateLimits()).accounts).toEqual(mockMyCodexRateLimitsByUser["u-mihai"]);
    expect((await ownAdminRow()).accounts[0].status).toBe("polling_disabled");
    await mockApi.vaultUnlock("demo password");
    await mockApi.vaultDisable();
    expect((await mockApi.getMyCodexRateLimits()).accounts).toEqual(mockMyCodexRateLimitsByUser["u-mihai"]);
    const admin = await mockApi.getAdminCodexRateLimits();
    expect(admin.users.find((user) => user.id === "u-radu")).toEqual(
      mockAdminCodexRateLimits.find((user) => user.id === "u-radu"),
    );
    expect(await ownAdminRow()).toMatchObject({ vault_mode: "unprotected", vault_locked: false });
  });
});


it.each(["optional", "required"])("new mock registrations follow %s policy without changing existing users", async policy => {
  appSettings.vault_policy = policy;
  const registered = await mockApi.register("new@example.com", "demo-password", "New user");
  expect(registered.vault).toEqual(policy === "optional"
    ? { mode: "unprotected", policy: "optional", exists: false, unlocked: false }
    : { mode: "protected", policy: "required", exists: true, unlocked: true });
  expect((await mockApi.getSettings()).unprotected_users).toBe(policy === "optional" ? 1 : 0);
});
