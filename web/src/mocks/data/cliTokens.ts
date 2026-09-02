import type {
  CliToken,
} from "../../lib/api";
import { daysAgo, minsAgo } from "./time";
import { mockAdmin } from "./users";

// ── CLI tokens (PRD #64 M6) ──────────────────────────────────────────────────
// Seed rows that exercise the whole forensic surface: a healthy in-use token, an
// admin_ro (scope badge), a never-used one, a STALE one (unused 90+ days, so the
// hint fires), and a revoked one (the soft-deleted incident trail — no Revoke
// button, dimmed).
// Each seed token carries a user_id so the mock can filter by the session user,
// mirroring the real list endpoint's `WHERE user_id=$1` (the public CliToken type
// has no user_id — the server never returns it — so it lives only on the fixture
// and mockApi strips it before responding). Attribution splits the tokens across
// the admin (u-admin) and a non-admin persona (u-mira) so logging in as mira in
// the demo shows only mira's own token, not the admin's.
export const mockCliTokens: (CliToken & { user_id: string })[] = [
  {
    id: "cli-1",
    user_id: mockAdmin.id,
    name: "laptop",
    token_prefix: "uzc_a1b2",
    scope: "user",
    revoked: false,
    created_at: daysAgo(20),
    last_used_at: minsAgo(9),
    last_used_ip: "192.168.1.24",
    expires_at: null,
  },
  {
    id: "cli-2",
    user_id: mockAdmin.id,
    name: "ci-runner",
    token_prefix: "uzc_9f3e",
    scope: "user",
    revoked: false,
    created_at: daysAgo(12),
    last_used_at: minsAgo(140),
    last_used_ip: "10.0.4.7",
    expires_at: null,
  },
  {
    id: "cli-3",
    user_id: mockAdmin.id,
    name: "factory audit (read-only)",
    token_prefix: "uza_77c0",
    scope: "admin_ro",
    revoked: false,
    created_at: daysAgo(3),
    last_used_at: null,
    last_used_ip: null,
    expires_at: daysAgo(-87), // ~90 days out
  },
  {
    id: "cli-4",
    user_id: "u-mira",
    name: "old-thinkpad",
    token_prefix: "uzc_5d2a",
    scope: "user",
    revoked: false,
    created_at: daysAgo(140),
    last_used_at: daysAgo(120),
    last_used_ip: "203.0.113.9",
    expires_at: null,
  },
  {
    id: "cli-5",
    user_id: mockAdmin.id,
    name: "leaked-in-a-gist",
    token_prefix: "uzc_0b11",
    scope: "user",
    revoked: true,
    created_at: daysAgo(60),
    last_used_at: daysAgo(58),
    last_used_ip: "198.51.100.3",
    expires_at: null,
  },
];
