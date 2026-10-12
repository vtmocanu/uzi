// Shared leaf helpers for the mock API modules: the jittered `delay` used to
// make loading states render believably, session gating, the demo-scenario
// reader and its OIDC mapping, and the mutable `users` roster that several
// domains read from.

import type { User } from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import { state } from "../store";
import { mockUsers } from "../data";

const jitter = () => 90 + Math.random() * 180;
export const delay = <T>(value: T, ms = jitter()): Promise<T> =>
  new Promise((resolve) => setTimeout(() => resolve(value), ms));

export function requireSession(): User {
  if (!state.session) throw new ApiError(401, "authentication required");
  return state.session;
}

// requireAdmin gates the admin-only routes the way the real API's RequireAdmin does:
// a session is required first, then admin, else 403 — the same precedent as the
// global-template/skill writes.
export function requireAdmin(): User {
  const me = requireSession();
  if (!me.is_admin) throw new ApiError(403, "admin only");
  return me;
}

// Mutable copy of the seed roster (CRUD operates on this). Lives in shared
// because several domains read it, and keeping it in its own users module would
// make a settings → users → settings cycle later. Never reassigned.
export let users: User[] = mockUsers.map((u) => ({ ...u }));

import { mockScenario } from "../mockScenario";
export { mockScenario } from "../mockScenario";

interface OidcDemo {
  oidcEnabled: boolean;
  providerName: string;
  passwordLoginEnabled: boolean;
  oidcStatus: string;
  passwordless: boolean; // has_password === false → the passphrase-create banner shows
}

// oidcDemo maps the scenario to the OIDC fields the auth-config, session, and
// settings responses expose. Scenarios: "oidc" (SSO alongside password),
// "oidc-degraded" (admin status degraded), "sso-only" (SSO only, password form
// hidden). Default: OIDC off, password on — the original demo behavior.
export function oidcDemo(): OidcDemo {
  switch (mockScenario()) {
    case "oidc":
      return { oidcEnabled: true, providerName: "Keycloak", passwordLoginEnabled: true, oidcStatus: "ok", passwordless: true };
    case "oidc-degraded":
      return { oidcEnabled: true, providerName: "Keycloak", passwordLoginEnabled: true, oidcStatus: "degraded", passwordless: true };
    case "sso-only":
      return { oidcEnabled: true, providerName: "Keycloak", passwordLoginEnabled: false, oidcStatus: "ok", passwordless: true };
    default:
      return { oidcEnabled: false, providerName: "SSO", passwordLoginEnabled: true, oidcStatus: "disabled", passwordless: false };
  }
}
