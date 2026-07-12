// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { mockApi } from "./mockApi";

// The demo mock hides the PRD #45 OIDC UX by default; a ?mock= scenario (read here
// via the uzi_mock_scenario localStorage key) surfaces it for demo builds / QA.
afterEach(() => {
  window.localStorage.clear();
});

describe("mock OIDC scenario toggle (PRD #45)", () => {
  it("defaults to OIDC off / password login on", async () => {
    const c = await mockApi.authConfig();
    expect(c.oidc_enabled).toBe(false);
    expect(c.password_login_enabled).toBe(true);
    const me = await mockApi.me();
    expect(me.has_password).toBe(true);
  });

  it("?mock=oidc enables the SSO button and a passwordless session", async () => {
    window.localStorage.setItem("uzi_mock_scenario", "oidc");
    const c = await mockApi.authConfig();
    expect(c.oidc_enabled).toBe(true);
    expect(c.oidc_provider_name).toBe("Keycloak");
    expect(c.password_login_enabled).toBe(true);

    const me = await mockApi.me();
    expect(me.has_password).toBe(false);
    expect(me.vault?.exists).toBe(false);
    expect(me.vault?.unlocked).toBe(false);
  });

  it("?mock=sso-only hides the password form", async () => {
    window.localStorage.setItem("uzi_mock_scenario", "sso-only");
    const c = await mockApi.authConfig();
    expect(c.oidc_enabled).toBe(true);
    expect(c.password_login_enabled).toBe(false);
  });

  it("?mock=oidc-degraded surfaces a degraded admin OIDC status", async () => {
    window.localStorage.setItem("uzi_mock_scenario", "oidc-degraded");
    const s = await mockApi.getSettings();
    expect(s.oidc_status).toBe("degraded");
    expect(s.oidc_provider_name).toBe("Keycloak");
  });
});
