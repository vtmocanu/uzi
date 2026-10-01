import type { OAuthAuthorizeRequest, OAuthRedirect } from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import {
  MOCK_OAUTH_APPROVE_URL,
  MOCK_OAUTH_DENY_URL,
  MOCK_OAUTH_REQUEST_ID,
  mockOAuthRequest,
} from "../data";
import { delay, requireSession } from "./shared";

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

export const oauthApi = {
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
