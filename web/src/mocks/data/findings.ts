import { minsAgo } from "./time";
import { mockAdmin } from "./users";

// ── Incidental Findings backlog (PRD #333 M7) ────────────────────────────────
// One row per (repo, location) COORDINATE (deduped across runs, D7), carrying the disposition
// status, the "seen in N runs" count, the actionable evidence id, and — for a filed/dismissed
// coordinate — the resolution. `run_ids` models the evidence rows (which runs saw it) so the
// mock can honour the ?run= deep-link semi-join; `finding_id` null models a coordinate whose
// evidence cascaded away with a deleted run (D12): display-only, non-actionable. All free-text
// (last_title, location) is agent-authored and rendered inert by every surface.
export interface MockFinding {
  finding_id: string | null;
  user_id: string;
  location: string;
  repo_id: string;
  repo_path: string;
  status: "open" | "filing" | "filed" | "dismissed";
  last_title: string;
  description_md: string;
  labels: string[];
  seen_in_runs: number;
  filed_issue_iid: number | null;
  filed_issue_url: string | null;
  resolved_at: string | null;
  run_ids: string[];
}

export const mockFindings: MockFinding[] = [
  {
    finding_id: "find-1",
    user_id: mockAdmin.id,
    location: "api/internal/store/sweeper.go#sweepLoop",
    repo_id: "repo-uzi",
    repo_path: "vtmocanu/uzi",
    status: "open",
    last_title: "Leaked ticker in sweepLoop never stopped on shutdown",
    description_md:
      "The `time.Ticker` started in `sweepLoop` is never `Stop()`ed, so a cancelled sweeper leaks the ticker's goroutine. Off-task: noticed while wiring the findings store.",
    labels: ["bug"],
    seen_in_runs: 2,
    filed_issue_iid: null,
    filed_issue_url: null,
    resolved_at: null,
    run_ids: ["run-live", "run-done"],
  },
  {
    finding_id: "find-2",
    user_id: mockAdmin.id,
    location: "api/internal/httpx/retry.go#doWithRetry",
    repo_id: "repo-uzi",
    repo_path: "vtmocanu/uzi",
    status: "open",
    last_title: "Retry loop can never succeed — it retries a non-idempotent POST",
    description_md:
      "`doWithRetry` re-issues the same POST on a 5xx, but the upstream is non-idempotent, so a retry after a partial write always 409s. The retry is dead code that only delays the failure.",
    labels: ["bug", "reliability"],
    seen_in_runs: 1,
    filed_issue_iid: null,
    filed_issue_url: null,
    resolved_at: null,
    run_ids: ["run-live"],
  },
  {
    finding_id: "find-3",
    user_id: mockAdmin.id,
    location: "api/internal/auth/boot.go#verifyKeys",
    repo_id: "repo-uzi",
    repo_path: "vtmocanu/uzi",
    status: "filed",
    last_title: "Boot key-check skips the JWT audience claim",
    description_md: "The boot verification never checks the `aud` claim, so a token minted for another service is accepted.",
    labels: ["security"],
    seen_in_runs: 3,
    filed_issue_iid: 512,
    filed_issue_url: "https://gitlab.example.com/vtmocanu/uzi/-/issues/512",
    resolved_at: minsAgo(200),
    run_ids: ["run-done"],
  },
  {
    finding_id: "find-4",
    user_id: mockAdmin.id,
    location: "internal/handlers/webhook.go#parseSignature",
    repo_id: "repo-atlas",
    repo_path: "vtmocanu/atlas-api",
    status: "open",
    last_title: "Webhook signature compared with == (timing side-channel)",
    description_md: "The HMAC signature is compared with `==`, not a constant-time compare, leaking the prefix length under timing.",
    labels: ["security"],
    seen_in_runs: 1,
    filed_issue_iid: null,
    filed_issue_url: null,
    resolved_at: null,
    run_ids: ["run-live"],
  },
  {
    finding_id: "find-5",
    user_id: mockAdmin.id,
    location: "api/internal/cache/lru.go#evict",
    repo_id: "repo-uzi",
    repo_path: "vtmocanu/uzi",
    status: "dismissed",
    last_title: "LRU eviction holds the lock while calling a user callback",
    description_md: "Eviction invokes the on-evict callback under the cache mutex; a slow callback stalls every reader.",
    labels: [],
    seen_in_runs: 1,
    filed_issue_iid: null,
    filed_issue_url: null,
    resolved_at: minsAgo(300),
    run_ids: ["run-done"],
  },
  {
    // A filed coordinate whose evidence cascaded away with a deleted run (D12): finding_id null,
    // so it is display-only — the backlog still shows it (disposition-driven), last_title keeps
    // it legible, but there is nothing to act on.
    finding_id: null,
    user_id: mockAdmin.id,
    location: "api/internal/queue/drain.go#drainOnce",
    repo_id: "repo-uzi",
    repo_path: "vtmocanu/uzi",
    status: "filed",
    last_title: "drainOnce double-acks on a redelivery",
    description_md: "",
    labels: [],
    seen_in_runs: 0,
    filed_issue_iid: 488,
    filed_issue_url: "https://gitlab.example.com/vtmocanu/uzi/-/issues/488",
    resolved_at: minsAgo(4000),
    run_ids: [],
  },
];
