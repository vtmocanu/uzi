import { ApiError } from "../../lib/apiError";
import type { ResetLinkKind } from "../../lib/apiTypes";
import { mockAdmin, mockBuildInfo } from "../data";
import { state } from "../store";
import { delay, oidcDemo, users } from "./shared";
import { appSettings, sessionBody } from "./settings";

export const usersApi = {
  // ── Auth: instant and fake. Any credentials sign in as the admin. ──────────
  // The session bootstrap carries the instance labels alongside the user, mirroring
  // the real API (PRD #19 M2), so the mocked SPA resolves them the same way.
  register: async (email: string, _password: string, displayName: string) => {
    state.session = { ...mockAdmin, email, display_name: displayName || mockAdmin.display_name };
    state.vaultMode = appSettings.vault_policy === "optional" ? "unprotected" : "protected";
    state.vaultExists = state.vaultMode === "protected";
    state.vaultUnlocked = state.vaultMode === "protected";
    return delay(sessionBody());
  },
  login: async (email: string, _password: string) => {
    // Persona switch for the demo: logging in as a seeded non-admin (e.g.
    // mira@uzi.local) signs in AS that user, so the non-admin rendering paths
    // (no Global create, view-only builtin/global, own-skills-only) are
    // browser-checkable. Any other email is the admin, as before.
    const persona = users.find((u) => u.email === email.trim().toLowerCase());
    state.session = persona ? { ...persona } : { ...mockAdmin, email: email || mockAdmin.email };
    return delay(sessionBody());
  },
  // Demo mode has registration open and unrestricted. The OIDC fields follow the
  // scenario toggle (default off; ?mock=oidc / sso-only enable SSO) — PRD #45 N6.
  authConfig: async () => {
    const d = oidcDemo();
    return delay({
      registration_enabled: true,
      allowed_email_domains: [],
      oidc_enabled: d.oidcEnabled,
      oidc_provider_name: d.providerName,
      password_login_enabled: d.passwordLoginEnabled,
    });
  },
  // The in-browser demo build has no server; report "demo" to match the header pill.
  // A real SemVer, not "demo" (PRD #113 M5). Upgrade classification compares against
  // this, and a non-SemVer control-plane version turns classification OFF entirely — so
  // the literal "demo" made every badge and the whole Fleet panel unreachable in demo
  // mode. The demo-mode signal does not live here: AppShell renders a separate "demo"
  // pill, so nothing is lost by making this comparable.
  // The FULLY-STAMPED fixture (PRD #175), so the demo shows the popover with every
  // row present — a `dev` build here would hide the three fields this PRD exists to
  // add, in the build whose whole job is to show them off.
  //
  // KNOWN CONSEQUENCE, worth stating rather than leaving to be rediscovered: this
  // is the default for every VITE_UZI_MOCK=1 run, so a browser pass sees the
  // STAMPED shape unless someone swaps this line. The degraded shapes are covered
  // in BuildInfoPopover.test.tsx (mockBuildInfoUnstamped = the laptop's three-key
  // body, mockBuildInfoNoUptime = the struct-literal Handler's two-key one); to see
  // either in a browser, point this line at it. `typeof realApi` cannot enforce any
  // of it, since every field but version and founded is optional.
  version: async () => delay(mockBuildInfo),
  logout: async () => {
    state.session = null;
    return delay({ status: "ok" });
  },
  me: async () => {
    if (!state.session) throw new ApiError(401, "authentication required");
    return delay(sessionBody(), 40);
  },

  // ── Admin: users ────────────────────────────────────────────────────────────
  listUsers: async () => delay({ users: users.map((u) => ({ ...u })) }),
  setUserActive: async (id: string, isActive: boolean) => {
    const u = users.find((x) => x.id === id);
    if (!u) throw new ApiError(404, "user not found");
    u.is_active = isActive;
    return delay({ user: { ...u } });
  },

  // ── Admin-issued account reset link (PRD #3989) ─────────────────────────────
  // Mirrors the server's refusals (self, inactive) and its supersession: a new link for
  // a user retires that user's older one. Every invalid-link cause is one generic 404.
  createResetLink: async (id: string) => {
    const u = users.find((x) => x.id === id);
    if (!u) throw new ApiError(404, "user not found");
    if (u.id === state.session?.id) throw new ApiError(400, "you cannot issue a reset link for your own account");
    if (!u.is_active) throw new ApiError(400, "the user is deactivated");
    for (const [token, link] of resetLinks) if (link.userId === id) resetLinks.delete(token);
    const token = mockResetToken();
    const link: MockResetLink = {
      userId: id,
      // Passwordless (SSO-only) scenarios issue the no-password variant, as the server does.
      kind: oidcDemo().passwordless ? "vault_only" : "password",
      expiresAt: new Date(Date.now() + 60 * 60_000).toISOString(),
    };
    resetLinks.set(token, link);
    return delay({ token, expires_at: link.expiresAt, kind: link.kind, target_is_admin: u.is_admin });
  },
  previewReset: async (token: string) => {
    const link = liveResetLink(token);
    return delay({ kind: link.kind, expires_at: link.expiresAt });
  },
  redeemReset: async (token: string, password?: string) => {
    const link = liveResetLink(token);
    if (link.kind === "password") {
      if (password === undefined || password.length < 12) {
        throw new ApiError(400, "password must be at least 12 characters");
      }
      if (password.length > 1024) throw new ApiError(400, "password is too long");
    }
    resetLinks.delete(token);
    return delay(null);
  },
};

type MockResetLink = { userId: string; kind: ResetLinkKind; expiresAt: string };

// Live reset links by plaintext token. Two fixed demo links make both /reset variants
// drivable without an admin round trip: /reset#demo-password-reset and
// /reset#demo-vault-only-reset (a redeemed demo link is gone until reload).
const resetLinks = new Map<string, MockResetLink>([
  ["demo-password-reset", { userId: "demo", kind: "password", expiresAt: new Date(Date.now() + 60 * 60_000).toISOString() }],
  ["demo-vault-only-reset", { userId: "demo", kind: "vault_only", expiresAt: new Date(Date.now() + 60 * 60_000).toISOString() }],
]);

function liveResetLink(token: string): MockResetLink {
  const link = resetLinks.get(token);
  if (!link || Date.parse(link.expiresAt) <= Date.now()) {
    throw new ApiError(404, "this link is invalid or expired");
  }
  return link;
}

// A 43-char base64url token, the shape of the server's 256-bit token.
function mockResetToken(): string {
  const bytes = new Uint8Array(32);
  crypto.getRandomValues(bytes);
  return btoa(String.fromCharCode(...bytes)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}
