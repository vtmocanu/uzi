import type {
  AdminProductToken,
  Product,
  ProductToken,
  ProductTokenExpiry,
  ProductTokenScope,
} from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import {
  hasUnsafeProductChar,
  PRODUCT_DESCRIPTION_MAX_BYTES,
  PRODUCT_NAME_MAX_BYTES,
} from "../../lib/productText";
import { mockProducts, mockProductTokens } from "../data";
import { delay, requireAdmin, requireSession, users } from "./shared";

// ── Product registry + product tokens (PRD #1907) ────────────────────────────
// Mirrors the cookie-only endpoints: the per-user list is owner-scoped and carries
// no value, the mint returns the plaintext once and enforces the D15 cap (10 active
// per product per user), delete is soft (D9), and an admin may revoke one token (D8).
type OwnedProductToken = ProductToken & { user_id: string };
type StoredProduct = Omit<Product, "active_token_count">;

let products: StoredProduct[] = mockProducts.map((p) => ({ ...p }));
let productTokens: OwnedProductToken[] = mockProductTokens.map((t) => ({ ...t, scopes: [...t.scopes] }));
let productCounter = 0;
let productTokenCounter = 0;

const ACTIVE_CAP = 10;
// The server's list caps (active first, then newest; `truncated` reports the cut).
const USER_LIST_CAP = 200;
const ADMIN_LIST_CAP = 1000;
const EXPIRY_MS: Record<Exclude<ProductTokenExpiry, "never">, number> = {
  "30d": 30 * 86_400_000,
  "90d": 90 * 86_400_000,
  "1y": 365 * 86_400_000,
};

const byteLength = (s: string) => new TextEncoder().encode(s).length;

// The server's name/description gate: trimmed, byte-capped, and termsafe-clean (no
// control or Cf character, newline and tab included). Returns the trimmed value.
function validName(raw: string): string {
  const n = raw.trim();
  if (n === "" || byteLength(n) > PRODUCT_NAME_MAX_BYTES) {
    throw new ApiError(400, `name must be non-empty and at most ${PRODUCT_NAME_MAX_BYTES} bytes`);
  }
  if (hasUnsafeProductChar(n)) {
    throw new ApiError(400, "name must not contain control characters (tabs, newlines, terminal escape sequences)");
  }
  return n;
}
function validDescription(raw: string): string {
  const d = raw.trim();
  if (byteLength(d) > PRODUCT_DESCRIPTION_MAX_BYTES) {
    throw new ApiError(400, `description must be at most ${PRODUCT_DESCRIPTION_MAX_BYTES} bytes`);
  }
  if (hasUnsafeProductChar(d)) {
    throw new ApiError(400, "description must not contain control characters (tabs, newlines, terminal escape sequences)");
  }
  return d;
}

// Active first, then newest (created_at desc), capped, the way both list queries order
// and cut their rows.
function capped<T extends ProductToken>(rows: T[], cap: number): { tokens: T[]; truncated: boolean } {
  const sorted = [...rows].sort(
    (a, b) =>
      Number(isActive(b)) - Number(isActive(a)) || Date.parse(b.created_at) - Date.parse(a.created_at),
  );
  return { tokens: sorted.slice(0, cap), truncated: sorted.length > cap };
}

const isActive = (t: ProductToken) =>
  !t.revoked && (t.expires_at === null || Date.parse(t.expires_at) > Date.now());
const stripOwner = ({ user_id: _user_id, ...t }: OwnedProductToken): ProductToken => t;
const withCount = (p: StoredProduct): Product => ({
  ...p,
  active_token_count: productTokens.filter((t) => t.product_id === p.id && isActive(t)).length,
});

function findProduct(id: string): StoredProduct {
  const p = products.find((x) => x.id === id);
  if (!p) throw new ApiError(404, "product not found");
  return p;
}

// Called by the mock revoke-all (cliTokens.ts): the real POST
// /me/cli-tokens/revoke-all revokes the caller's product tokens in the same
// transaction as its CLI tokens (D8).
export function revokeAllProductTokensOf(userId: string): void {
  productTokens = productTokens.map((t) => (t.user_id === userId ? { ...t, revoked: true } : t));
}

export const productTokensApi = {
  listProductTokens: async () => {
    const me = requireSession();
    return delay(
      capped(
        productTokens.filter((t) => t.user_id === me.id).map(stripOwner),
        USER_LIST_CAP,
      ),
    );
  },
  listMintableProducts: async () => {
    requireSession();
    return delay({
      products: products
        .filter((p) => p.enabled && p.deleted_at === null)
        .map(({ id, name, description }) => ({ id, name, description })),
    });
  },
  createProductToken: async (input: {
    product_id: string;
    name: string;
    scopes: ProductTokenScope[];
    expiry: ProductTokenExpiry;
  }) => {
    const me = requireSession();
    const name = validName(input.name);
    const scopes = [...new Set(input.scopes)];
    if (scopes.length === 0 || scopes.some((s) => s !== "jobs:run" && s !== "jobs:read")) {
      throw new ApiError(400, 'scopes must be a non-empty subset of "jobs:run" and "jobs:read"');
    }
    const expiry = input.expiry || "90d";
    if (expiry !== "never" && !(expiry in EXPIRY_MS)) {
      throw new ApiError(400, 'expiry must be one of "30d", "90d", "1y" or "never"');
    }
    const product = findProduct(input.product_id);
    if (!product.enabled || product.deleted_at !== null) {
      throw new ApiError(409, "product is disabled or deleted; no token can be minted for it");
    }
    const active = productTokens.filter(
      (t) => t.user_id === me.id && t.product_id === product.id && isActive(t),
    ).length;
    if (active >= ACTIVE_CAP) {
      throw new ApiError(
        409,
        `you already have ${ACTIVE_CAP} active tokens for this product; revoke one before minting another`,
      );
    }
    const body = Array.from(crypto.getRandomValues(new Uint8Array(24)), (b) =>
      b.toString(16).padStart(2, "0"),
    ).join("");
    // The class prefix is assembled at runtime (source hygiene: no complete
    // token-shaped literal in tracked source).
    const cls = "uz" + "p_";
    const row: OwnedProductToken = {
      id: `pt-new-${++productTokenCounter}`,
      user_id: me.id,
      product_id: product.id,
      product_name: product.name,
      name,
      token_prefix: cls + body.slice(0, 4),
      scopes,
      revoked: false,
      created_at: new Date().toISOString(),
      last_used_at: null,
      last_used_ip: null,
      expires_at:
        expiry === "never" ? null : new Date(Date.now() + EXPIRY_MS[expiry]).toISOString(),
    };
    productTokens = [row, ...productTokens];
    return delay({ token: cls + body, product_token: stripOwner(row) }, 200);
  },
  revokeProductToken: async (id: string) => {
    const me = requireSession();
    const t = productTokens.find((x) => x.id === id && x.user_id === me.id && !x.revoked);
    if (!t) throw new ApiError(404, "token not found");
    t.revoked = true;
    return delay(null);
  },

  // ── Admin registry ──────────────────────────────────────────────────────────
  adminListProducts: async () => {
    requireAdmin();
    return delay({ products: products.map(withCount) });
  },
  adminCreateProduct: async (name: string, description: string) => {
    requireAdmin();
    const n = validName(name);
    const desc = validDescription(description);
    // Case-insensitive among live products, like the server's unique index.
    if (products.some((p) => p.deleted_at === null && p.name.toLowerCase() === n.toLowerCase())) {
      throw new ApiError(409, "a product with this name already exists");
    }
    const p: StoredProduct = {
      id: `prod-new-${++productCounter}`,
      name: n,
      description: desc,
      enabled: true,
      deleted_at: null,
      created_at: new Date().toISOString(),
    };
    products = [p, ...products];
    return delay({ product: withCount(p) }, 200);
  },
  adminUpdateProduct: async (id: string, patch: { description?: string; enabled?: boolean }) => {
    requireAdmin();
    if (patch.description === undefined && patch.enabled === undefined) {
      throw new ApiError(400, "nothing to update: set description and/or enabled");
    }
    const p = findProduct(id);
    if (p.deleted_at !== null) {
      throw new ApiError(409, "product is deleted; a deleted product cannot be changed or re-enabled");
    }
    if (patch.description !== undefined) p.description = validDescription(patch.description);
    if (patch.enabled !== undefined) p.enabled = patch.enabled;
    return delay({ product: withCount(p) });
  },
  adminDeleteProduct: async (id: string) => {
    requireAdmin();
    const p = findProduct(id);
    if (p.deleted_at !== null) throw new ApiError(409, "product is already deleted");
    const wasEnabled = p.enabled;
    const active = withCount(p).active_token_count;
    p.enabled = false;
    p.deleted_at = new Date().toISOString();
    return delay({ product: withCount(p), stopped_token_count: wasEnabled ? active : 0 });
  },
  adminListProductTokens: async () => {
    requireAdmin();
    const tokens: AdminProductToken[] = productTokens.map((t) => ({
      ...t,
      owner_email: users.find((u) => u.id === t.user_id)?.email ?? "",
    }));
    return delay(capped(tokens, ADMIN_LIST_CAP));
  },
  adminRevokeProductToken: async (id: string) => {
    requireAdmin();
    const t = productTokens.find((x) => x.id === id && !x.revoked);
    if (!t) throw new ApiError(404, "token not found");
    t.revoked = true;
    return delay(null);
  },
};
