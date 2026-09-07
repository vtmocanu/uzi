import type {
  SecretMeta,
} from "../../lib/api";
import { daysAgo, minsAgo } from "./time";

// ── Secrets ──────────────────────────────────────────────────────────────────

// Two tokens for the demo user (PRD #104): the multi-token list, the default
// badge, the per-token meters and the worker/judge pickers are only browsable in
// the mock if the fixture actually has more than one.
export const mockSecrets: SecretMeta[] = [
  {
    id: "sec-default",
    kind: "anthropic_token",
    label: "default",
    is_default: true,
    auto_eligible: false,
    created_at: daysAgo(30),
    updated_at: daysAgo(4),
  },
  {
    id: "sec-console",
    kind: "anthropic_token",
    label: "console-key",
    is_default: false,
    // Pooled, so the mock shows the PRD #111 toggle in its ON state and the
    // eligibility chip beside it.
    auto_eligible: true,
    created_at: daysAgo(9),
    updated_at: daysAgo(9),
  },
  // Two more pooled tokens so the states that MATTER are browsable (F2): a token
  // the poller has never reached, and one that is nearly spent. Both are pooled —
  // an un-pooled token shows no chip at all, so only a pooled one can demonstrate
  // that opting in is not the same as being pickable.
  {
    id: "sec-never-polled",
    kind: "anthropic_token",
    label: "refused-key",
    is_default: false,
    auto_eligible: true,
    created_at: daysAgo(3),
    updated_at: daysAgo(3),
  },
  {
    id: "sec-low",
    kind: "anthropic_token",
    label: "nearly-spent",
    is_default: false,
    auto_eligible: true,
    created_at: daysAgo(12),
    updated_at: daysAgo(1),
  },
  // PRD #217 M3: the token whose reading carries source `limit_report` (see
  // mockMyTokenRateLimits). Its auto_eligible MUST match its meter fixture
  // token-for-token (data.test.ts) — both false, so it stays out of the pool.
  {
    id: "sec-parked",
    kind: "anthropic_token",
    label: "parked-key",
    is_default: false,
    auto_eligible: false,
    created_at: daysAgo(6),
    updated_at: minsAgo(14),
  },
  // Codex credentials (PRD #1147 / issue #1174 item 10). Seeded so ALL FOUR status
  // badges — and the legend that explains them — are browsable in the mock: the
  // create path only ever births `staging`/`static`, so `linked` and `failed` are
  // unreachable without fixture rows. This set is independent of the anthropic
  // rows above: it carries its OWN single default (the linked login), no codex row
  // is `auto_eligible` (there is no Codex auto-selection pool), and none of these
  // touch the anthropic-only data.test.ts invariants.
  {
    id: "sec-codex-linked",
    kind: "codex_auth",
    label: "chatgpt-plus",
    is_default: true,
    auto_eligible: false,
    codex_status: "linked",
    created_at: daysAgo(20),
    updated_at: daysAgo(2),
  },
  {
    id: "sec-codex-staging",
    kind: "codex_auth",
    label: "pending-login",
    is_default: false,
    auto_eligible: false,
    codex_status: "staging",
    created_at: daysAgo(1),
    updated_at: minsAgo(35),
  },
  {
    id: "sec-codex-failed",
    kind: "codex_auth",
    label: "revoked-login",
    is_default: false,
    auto_eligible: false,
    codex_status: "failed",
    created_at: daysAgo(11),
    updated_at: daysAgo(1),
  },
  {
    id: "sec-openai-static",
    kind: "openai_api_key",
    label: "openai-key",
    is_default: false,
    auto_eligible: false,
    codex_status: "static",
    created_at: daysAgo(15),
    updated_at: daysAgo(15),
  },
];
