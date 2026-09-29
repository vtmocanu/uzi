import type { Product, ProductToken } from "../../lib/api";
import { daysAgo, minsAgo } from "./time";
import { mockAdmin } from "./users";

// ── Product registry + product tokens (PRD #1907) ────────────────────────────
// Three products cover every registry state: a live enabled one with tokens, a
// disabled one (its tokens are refused but still counted as active), and a
// soft-deleted one kept for the audit trail. active_token_count is recomputed by
// the mock from the token rows, so the seed value here is only the initial shape.
export const mockProducts: Omit<Product, "active_token_count">[] = [
  {
    id: "prod-helpdesk",
    name: "Helpdesk assistant",
    description: "Opens uzi jobs from support tickets and posts the result back to the ticket.",
    enabled: true,
    deleted_at: null,
    created_at: daysAgo(45),
  },
  {
    id: "prod-metrics",
    name: "Weekly metrics export",
    description: "Reads finished job results into the Monday report.",
    enabled: false,
    deleted_at: null,
    created_at: daysAgo(30),
  },
  {
    id: "prod-legacy",
    name: "Legacy importer",
    description: "Retired after the forge migration.",
    enabled: false,
    deleted_at: daysAgo(6),
    created_at: daysAgo(120),
  },
];

// Seed tokens, owner-attributed like mockCliTokens (user_id is on the wire only for
// the admin inventory; the per-user list strips it). Rows exercise: an in-use token,
// a never-used one with no expiry, an expired one, a revoked one, a token of the
// disabled product, one of the deleted product, and another user's token so the
// admin inventory has a second owner.
export const mockProductTokens: (ProductToken & { user_id: string })[] = [
  {
    id: "pt-1",
    user_id: mockAdmin.id,
    product_id: "prod-helpdesk",
    product_name: "Helpdesk assistant",
    name: "support-prod",
    token_prefix: "uzp_4c1e",
    scopes: ["jobs:run", "jobs:read"],
    revoked: false,
    created_at: daysAgo(14),
    last_used_at: minsAgo(3),
    last_used_ip: "10.20.3.14",
    expires_at: daysAgo(-76),
  },
  {
    id: "pt-2",
    user_id: mockAdmin.id,
    product_id: "prod-helpdesk",
    product_name: "Helpdesk assistant",
    name: "staging read-only",
    token_prefix: "uzp_90ab",
    scopes: ["jobs:read"],
    revoked: false,
    created_at: daysAgo(4),
    last_used_at: null,
    last_used_ip: null,
    expires_at: null,
  },
  {
    id: "pt-3",
    user_id: mockAdmin.id,
    product_id: "prod-metrics",
    product_name: "Weekly metrics export",
    name: "monday-report",
    token_prefix: "uzp_7d02",
    scopes: ["jobs:read"],
    revoked: false,
    created_at: daysAgo(28),
    last_used_at: daysAgo(8),
    last_used_ip: "10.20.9.2",
    expires_at: daysAgo(-2),
  },
  {
    id: "pt-4",
    user_id: mockAdmin.id,
    product_id: "prod-helpdesk",
    product_name: "Helpdesk assistant",
    name: "trial",
    token_prefix: "uzp_e5f3",
    scopes: ["jobs:run"],
    revoked: false,
    created_at: daysAgo(40),
    last_used_at: daysAgo(12),
    last_used_ip: "198.51.100.40",
    expires_at: daysAgo(10),
  },
  {
    id: "pt-5",
    user_id: "u-mira",
    product_id: "prod-helpdesk",
    product_name: "Helpdesk assistant",
    name: "mira laptop test",
    token_prefix: "uzp_b3c9",
    scopes: ["jobs:run", "jobs:read"],
    revoked: false,
    created_at: daysAgo(2),
    last_used_at: minsAgo(55),
    last_used_ip: "203.0.113.21",
    expires_at: daysAgo(-28),
  },
  {
    id: "pt-6",
    user_id: "u-mira",
    product_id: "prod-legacy",
    product_name: "Legacy importer",
    name: "importer",
    token_prefix: "uzp_12de",
    scopes: ["jobs:run"],
    revoked: true,
    created_at: daysAgo(100),
    last_used_at: daysAgo(7),
    last_used_ip: "203.0.113.21",
    expires_at: null,
  },
];
