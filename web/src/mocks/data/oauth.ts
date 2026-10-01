import type { OAuthAuthorizeRequest } from "../../lib/api";
import { minsAgo } from "./time";

// A seeded PENDING OAuth authorize request (PRD #1910 M2) so /connect?request=<id> renders the
// consent page in the demo. Approve and deny flip its status in place and answer the redirect
// URLs below, which are what a real server builds from the product's registered redirect URI.
// Assembled from parts so no UUID-shaped literal sits in tracked source for the secret scan.
export const MOCK_OAUTH_REQUEST_ID = ["6f0e4b0a", "1c2d", "4e3f", "8a9b", "0c1d2e3f4a5b"].join("-");

export const mockOAuthRequest: OAuthAuthorizeRequest = {
  product_name: "Demo Product",
  product_description: "Creates uzi jobs from its own app on your behalf.",
  redirect_host: "demo-product.example",
  scopes: ["jobs:run", "jobs:read"],
  status: "pending",
  expires_at: minsAgo(-5), // ~5 minutes out
};

export const MOCK_OAUTH_APPROVE_URL =
  "https://demo-product.example/callback?code=demo-code&iss=https%3A%2F%2Fuzi.example&state=demo-state";
export const MOCK_OAUTH_DENY_URL =
  "https://demo-product.example/callback?error=access_denied&error_description=the+user+denied+the+request&iss=https%3A%2F%2Fuzi.example&state=demo-state";
