import type { CliAuthRequestMeta, CliToken, CliTokenScope } from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import { MOCK_CLI_AUTH_REQUEST_ID, mockCliAuthRequest, mockCliTokens } from "../data";
import { delay, requireSession } from "./shared";

// Tokens are owner-attributed (user_id) so every read/write is scoped to the
// session user, mirroring the real endpoints (`WHERE user_id=$1`). user_id is
// mock-internal — stripped before responding, since the wire CliToken has none.
type OwnedCliToken = CliToken & { user_id: string };
let cliTokens: OwnedCliToken[] = mockCliTokens.map((t) => ({ ...t }));
let cliTokenCounter = 0;
const stripOwner = ({ user_id: _user_id, ...t }: OwnedCliToken): CliToken => t;

// The seeded consent request; approve/deny flip its status in place.
const cliAuthRequests = new Map<string, CliAuthRequestMeta & { user_code: string }>([
  [MOCK_CLI_AUTH_REQUEST_ID, { ...mockCliAuthRequest }],
]);

// crockford32 excludes I, L, O, U. normalizeMockUserCode folds a human-typed code
// to the canonical stored form, mirroring the server (uppercase; hyphens/spaces
// dropped; O→0, I/L→1) so a careful-but-imperfect read still matches.
const CROCKFORD32 = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";
function normalizeMockUserCode(s: string): string {
  let out = "";
  for (const r of s.toUpperCase()) {
    if (r === "O") out += "0";
    else if (r === "I" || r === "L") out += "1";
    else if (CROCKFORD32.includes(r)) out += r;
  }
  return out;
}

export const cliTokensApi = {
  // ── CLI tokens (PRD #64 M6) ────────────────────────────────────────────────
  // Mirrors the cookie-only CRUD: list carries no value, mint returns the
  // plaintext once, admin_ro is admin-only, revoked rows stay (the incident
  // trail), and revoke-all is idempotent.
  listCliTokens: async () => {
    const me = requireSession();
    // Only the caller's own tokens — the real endpoint filters `WHERE user_id=$1`,
    // so as a non-admin persona you must not see the admin's tokens.
    return delay({ tokens: cliTokens.filter((t) => t.user_id === me.id).map(stripOwner) });
  },
  createCliToken: async (name: string, scope: CliTokenScope) => {
    const me = requireSession();
    const trimmed = name.trim();
    if (trimmed === "") throw new ApiError(400, "name must be non-empty and at most 200 characters");
    if (scope !== "user" && scope !== "admin_ro") throw new ApiError(400, "scope must be 'user' or 'admin_ro'");
    if (scope === "admin_ro" && !me.is_admin) {
      throw new ApiError(403, "admin access required to mint an admin-scoped token");
    }
    const cls = scope === "admin_ro" ? "uza" : "uzc";
    const body = Array.from(crypto.getRandomValues(new Uint8Array(24)), (b) => b.toString(16).padStart(2, "0")).join("");
    const now = new Date().toISOString();
    const row: OwnedCliToken = {
      id: `cli-new-${++cliTokenCounter}`,
      user_id: me.id,
      name: trimmed,
      token_prefix: `${cls}_${body.slice(0, 4)}`,
      scope,
      revoked: false,
      created_at: now,
      last_used_at: null,
      last_used_ip: null,
      // Expiry matrix (static mint path): a user token never expires; an admin_ro
      // token is bounded to 90 days.
      expires_at: scope === "admin_ro" ? new Date(Date.now() + 90 * 86_400_000).toISOString() : null,
    };
    cliTokens = [row, ...cliTokens];
    return delay({ token: `${cls}_${body}`, cli_token: stripOwner(row) }, 200);
  },
  revokeCliToken: async (id: string) => {
    const me = requireSession();
    // Owner-scoped: a foreign id is a 404, exactly like the server.
    const t = cliTokens.find((x) => x.id === id && x.user_id === me.id && !x.revoked);
    if (!t) throw new ApiError(404, "token not found");
    t.revoked = true;
    return delay(null);
  },
  revokeAllCliTokens: async () => {
    const me = requireSession();
    // Only the caller's tokens, mirroring `WHERE user_id=$1`.
    cliTokens = cliTokens.map((t) => (t.user_id === me.id ? { ...t, revoked: true } : t));
    return delay(null);
  },

  // ── CLI browser-login consent flow (PRD #64 M6) ────────────────────────────
  // getCliAuthRequest never returns the user_code (the human types it from their
  // terminal — the anti-phishing property). approve validates the typed code.
  getCliAuthRequest: async (id: string) => {
    requireSession();
    const req = cliAuthRequests.get(id);
    if (!req) throw new ApiError(404, "request not found");
    const status =
      req.status === "pending" && Date.parse(req.expires_at) <= Date.now() ? "expired" : req.status;
    return delay({ client_desc: req.client_desc, status, expires_at: req.expires_at });
  },
  approveCliAuth: async (requestId: string, userCode: string, scope: CliTokenScope) => {
    const me = requireSession();
    if (scope !== "user" && scope !== "admin_ro") throw new ApiError(400, "scope must be 'user' or 'admin_ro'");
    if (scope === "admin_ro" && !me.is_admin) {
      throw new ApiError(403, "admin access required to approve an admin-scoped login");
    }
    const req = cliAuthRequests.get(requestId);
    if (!req) throw new ApiError(404, "request not found");
    if (Date.parse(req.expires_at) <= Date.now()) throw new ApiError(410, "request expired");
    if (req.status !== "pending") throw new ApiError(409, "request is no longer pending");
    if (normalizeMockUserCode(userCode) !== req.user_code) {
      throw new ApiError(400, "the code you entered does not match");
    }
    req.status = "approved";
    return delay({ status: "approved" });
  },
  denyCliAuth: async (requestId: string) => {
    requireSession();
    const req = cliAuthRequests.get(requestId);
    if (!req) throw new ApiError(404, "request not found");
    if (req.status !== "pending" || Date.parse(req.expires_at) <= Date.now()) {
      throw new ApiError(409, "request is no longer pending");
    }
    req.status = "denied";
    return delay({ status: "denied" });
  },
};
