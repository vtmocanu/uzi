import type {
  AdminOAuthConnection,
  OAuthAuthorizeRequest,
  OAuthConnection,
  OAuthRedirect,
} from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import {
  MOCK_OAUTH_APPROVE_URL,
  MOCK_OAUTH_DENY_URL,
  MOCK_OAUTH_REQUEST_ID,
  mockOAuthConnections,
  mockOAuthRequest,
} from "../data";
import { delay, requireAdmin, requireSession, users } from "./shared";

// ── OAuth consent requests (PRD #1910 M2) ────────────────────────────────────
// Mirrors the cookie-session endpoints under /api/oauth/requests/{id}. The real server also
// requires the browser-binding cookie; the mock has no cookies, so any signed-in caller owns
// the one seeded request.
const oauthRequests = new Map<string, OAuthAuthorizeRequest>([
  [MOCK_OAUTH_REQUEST_ID, { ...mockOAuthRequest, scopes: [...mockOAuthRequest.scopes] }],
]);

function pendingRequest(id: string): OAuthAuthorizeRequest {
  requireSession();
  const req = oauthRequests.get(id);
  if (!req || Date.parse(req.expires_at) <= Date.now()) throw new ApiError(404, "request not found");
  return req;
}

// ── OAuth connections (PRD #1910 M3) ─────────────────────────────────────────
// Live grants, owner-scoped like `WHERE user_id = $1 AND revoked_at IS NULL`; a revoked grant is
// dropped from the list whatever the state of its tokens (the mock keeps no access tokens).
let oauthConnections = mockOAuthConnections.map((c) => ({ ...c, scopes: [...c.scopes] }));

// Revoke all (POST /me/cli-tokens/revoke-all) revokes every live grant of the caller in the
// same transaction as their CLI and product tokens (D6); the mock mirrors that.
export function revokeAllOAuthGrantsOf(userId: string): void {
  oauthConnections = oauthConnections.filter((c) => c.user_id !== userId);
}

// Live connections of one product across all users, the mock of ListProducts' live_connection_count.
export function liveOAuthConnectionCount(productId: string): number {
  return oauthConnections.filter((c) => c.product_id === productId).length;
}

const stripOwner = ({ user_id: _user_id, ...c }: (typeof oauthConnections)[number]): OAuthConnection => ({
  ...c,
  scopes: [...c.scopes],
});

export const oauthApi = {
  listOAuthConnections: async (): Promise<{ connections: OAuthConnection[] }> => {
    const me = requireSession();
    return delay({ connections: oauthConnections.filter((c) => c.user_id === me.id).map(stripOwner) });
  },
  // The owner's disconnect of one connection (POST /me/oauth-connections/{id}/revoke). Owner-scoped:
  // a foreign, unknown or already-revoked id is a 404 and changes nothing.
  revokeOAuthConnection: async (id: string): Promise<null> => {
    const me = requireSession();
    if (!oauthConnections.some((c) => c.id === id && c.user_id === me.id)) {
      throw new ApiError(404, "connection not found");
    }
    oauthConnections = oauthConnections.filter((c) => c.id !== id);
    return delay(null);
  },
  // One product's live connections for the admin card, newest consent first.
  adminListProductConnections: async (
    productId: string,
  ): Promise<{ connections: AdminOAuthConnection[]; truncated: boolean }> => {
    requireAdmin();
    const connections = oauthConnections
      .filter((c) => c.product_id === productId)
      .sort((a, b) => Date.parse(b.connected_at) - Date.parse(a.connected_at))
      .map(({ id, user_id, scopes, connected_at, created_at, last_used_at }) => ({
        id,
        user_id,
        owner_email: users.find((u) => u.id === user_id)?.email ?? "",
        scopes: [...scopes],
        connected_at,
        created_at,
        last_used_at,
      }));
    return delay({ connections, truncated: false });
  },
  // The admin's disconnect of any user's connection (cookie-only admin write).
  adminRevokeOAuthConnection: async (id: string): Promise<null> => {
    requireAdmin();
    if (!oauthConnections.some((c) => c.id === id)) throw new ApiError(404, "connection not found");
    oauthConnections = oauthConnections.filter((c) => c.id !== id);
    return delay(null);
  },
  getOAuthRequest: async (id: string): Promise<OAuthAuthorizeRequest> => {
    const req = pendingRequest(id);
    return delay({ ...req, scopes: [...req.scopes] });
  },
  approveOAuthRequest: async (id: string): Promise<OAuthRedirect> => {
    const req = pendingRequest(id);
    if (req.status !== "pending") throw new ApiError(409, "request is no longer pending");
    req.status = "approved";
    return delay({ redirect_url: MOCK_OAUTH_APPROVE_URL });
  },
  denyOAuthRequest: async (id: string): Promise<OAuthRedirect> => {
    const req = pendingRequest(id);
    if (req.status !== "pending") throw new ApiError(409, "request is no longer pending");
    req.status = "denied";
    return delay({ redirect_url: MOCK_OAUTH_DENY_URL });
  },
};
