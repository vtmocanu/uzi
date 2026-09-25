// @vitest-environment jsdom
//
// PRD #1429 M4a: the "codex-only" mock scenario. It must make a browser demo show
// Codex as the account's ACTIVE (and only usable) harness end to end — no Anthropic
// credential, a usable Codex credential, and every run's harness reading "codex" —
// so the harness picker/badges/legibility work is browsable offline without a real
// Codex provider.

import { afterEach, describe, expect, it } from "vitest";
import { mockApi } from "./mockApi";

// This jsdom build does not expose window.localStorage by default (mockApi.oidc.test.ts
// documents the same gap); back it with a Map-based Storage stub.
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
  installStorage();
  window.localStorage.setItem("uzi_mock_scenario", name);
}

afterEach(() => {
  try {
    window.localStorage.clear();
  } catch {
    // no-op if a prior test never installed the stub
  }
});

describe("the default scenario is unaffected by the codex-only overlay (negative control)", () => {
  it("listSecrets still carries the seeded Anthropic default token", async () => {
    const { secrets } = await mockApi.listSecrets();
    expect(secrets.some((s) => s.kind === "anthropic_token" && s.is_default)).toBe(true);
  });

  // PRD #1653 M4: the default scenario seeds exactly ONE Codex run among Claude runs, so
  // the harness chip shows both providers offline; the overlay leaves it as seeded.
  it("every listed run reads harness claude except the one seeded Codex run", async () => {
    const { runs } = await mockApi.listRuns();
    expect(runs.length).toBeGreaterThan(1);
    expect(runs.filter((r) => r.harness === "codex").map((r) => r.id)).toEqual(["run-rework-capped"]);
    expect(runs.filter((r) => r.id !== "run-rework-capped").every((r) => r.harness === "claude")).toBe(true);
  });

  it("getRun on the seeded Codex run reads harness codex (list and detail agree)", async () => {
    const { run } = await mockApi.getRun("run-rework-capped");
    expect(run.harness).toBe("codex");
  });
});

describe("?mock=codex-only makes Codex the account's active harness (PRD #1429 M4a)", () => {
  it("listSecrets carries a usable Codex credential and NO Anthropic token", async () => {
    setScenario("codex-only");
    const { secrets } = await mockApi.listSecrets();
    expect(secrets.some((s) => s.kind === "anthropic_token")).toBe(false);
    const codexDefault = secrets.find(
      (s) => (s.kind === "codex_auth" || s.kind === "openai_api_key") && s.is_default,
    );
    expect(codexDefault).toBeTruthy();
    // Usable per D3: an openai_api_key default is usable by existence; a codex_auth
    // default is usable only when linked.
    const usable = codexDefault?.kind === "openai_api_key" || codexDefault?.codex_status === "linked";
    expect(usable).toBe(true);
  });

  it("every listed run reads harness codex", async () => {
    setScenario("codex-only");
    const { runs } = await mockApi.listRuns();
    expect(runs.length).toBeGreaterThan(0);
    expect(runs.every((r) => r.harness === "codex")).toBe(true);
  });

  it("getRun on a seeded run also reads harness codex", async () => {
    setScenario("codex-only");
    const { runs } = await mockApi.listRuns();
    const { run } = await mockApi.getRun(runs[0].id);
    expect(run.harness).toBe("codex");
  });

  it("a newly created run implicitly resolves to codex (D11: the only usable harness)", async () => {
    setScenario("codex-only");
    const { run } = await mockApi.createRun("repo-uzi", 31);
    expect(run.harness).toBe("codex");
  });

  it("an EXPLICIT harness choice is never overridden by the scenario (D2: explicit never falls back)", async () => {
    setScenario("codex-only");
    const { run } = await mockApi.createRun("repo-uzi", 29, undefined, undefined, "claude");
    expect(run.harness).toBe("claude");
  });
});
