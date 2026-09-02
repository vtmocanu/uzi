import { ApiError } from "../../lib/apiError";
import { mockAdmin, mockBuildInfo } from "../data";
import { state } from "../store";
import { delay, oidcDemo, users } from "./shared";
import { sessionBody } from "./settings";

export const usersApi = {
  // ── Auth: instant and fake. Any credentials sign in as the admin. ──────────
  // The session bootstrap carries the instance labels alongside the user, mirroring
  // the real API (PRD #19 M2), so the mocked SPA resolves them the same way.
  register: async (email: string, _password: string, displayName: string) => {
    state.session = { ...mockAdmin, email, display_name: displayName || mockAdmin.display_name };
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
};
