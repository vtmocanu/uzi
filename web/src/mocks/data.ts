// Seed data for the fully in-browser mock mode (VITE_UZI_MOCK=1). Everything
// here is plain in-memory state: no request ever leaves the browser. Timestamps
// are derived from Date.now() at module load so relative times ("last seen 2m
// ago") always look fresh in a demo.

import type {
  AdminRateLimitUser,
  AdminWorker,
  AgentTemplate,
  Board,
  BuildInfo,
  BuiltinDefinition,
  CliAuthRequestMeta,
  CliToken,
  Disposition,
  ForgeConnection,
  GuardrailOverrideMeta,
  IssueProposal,
  LatestRun,
  Memory,
  Milestone,
  MyRateLimits,
  PendingJudge,
  RateLimitSource,
  RecommendationCategory,
  Repo,
  ReviewStatus,
  ReviewVerdict,
  Run,
  TriageCounts,
  RunListItem,
  RunMessage,
  RunUsage,
  SecretMeta,
  TokenRateLimits,
  SteerInput,
  Skill,
  ToolAllowlistEntry,
  User,
  Worker,
} from "../lib/api";

const NOW = Date.now();
export const minsAgo = (m: number) => new Date(NOW - m * 60_000).toISOString();
// secsAgo gives sub-minute granularity, needed for the judge run's start/finish stamps
// (PRD #69 M6): a retrospective takes seconds, so its Duration tile needs second-level
// timestamps rather than the minute-granular minsAgo the reviews otherwise use.
export const secsAgo = (s: number) => new Date(NOW - s * 1000).toISOString();
export const daysAgo = (d: number) => new Date(NOW - d * 86_400_000).toISOString();
// minsAhead is the FUTURE direction, which nothing needed until PRD #35: a parked
// run's whole surface is a countdown, and a countdown seeded in the past renders the
// already-expired state instead of the one worth looking at. Relative to the same
// frozen NOW as its siblings, so the demo's clocks stay consistent with each other.
export const minsAhead = (m: number) => new Date(NOW + m * 60_000).toISOString();

// ── Users ────────────────────────────────────────────────────────────────────

export const mockAdmin: User = {
  id: "u-admin",
  email: "vlad@uzi.local",
  display_name: "Vlad",
  is_admin: true,
  is_active: true,
  autopilot_enabled: false,
  judge_enabled: false,
  ci_autofix_enabled: false,
  wait_on_limit: false,
  judge_anthropic_secret_id: null,
  judge_anthropic_secret_label: null,
  created_at: daysAgo(41),
  last_login: minsAgo(7),
};

export const mockUsers: User[] = [
  mockAdmin,
  {
    id: "u-mira",
    email: "mira@uzi.local",
    display_name: "Mira Ionescu",
    is_admin: false,
    is_active: true,
    autopilot_enabled: true,
    judge_enabled: true,
    ci_autofix_enabled: false,
    // PRD #35: the demo's one opted-IN user, and it is the autopilot user on purpose.
    // An autopilot run has no start affordance at all, so this default is the ONLY
    // way its opt-in can ever be expressed — pairing the two is what makes the
    // setting's reason for existing visible in the fixture rather than only in prose.
    wait_on_limit: true,
    judge_anthropic_secret_id: null,
    judge_anthropic_secret_label: null,
    created_at: daysAgo(33),
    last_login: minsAgo(95),
  },
  {
    id: "u-andrei",
    email: "andrei@uzi.local",
    display_name: "Andrei Pop",
    is_admin: false,
    is_active: true,
    autopilot_enabled: false,
    judge_enabled: false,
    ci_autofix_enabled: false,
    wait_on_limit: false,
    judge_anthropic_secret_id: null,
    judge_anthropic_secret_label: null,
    created_at: daysAgo(20),
    last_login: daysAgo(1),
  },
  {
    id: "u-dan",
    email: "dan@uzi.local",
    display_name: null,
    is_admin: false,
    is_active: false,
    autopilot_enabled: false,
    judge_enabled: false,
    ci_autofix_enabled: false,
    wait_on_limit: false,
    judge_anthropic_secret_id: null,
    judge_anthropic_secret_label: null,
    created_at: daysAgo(18),
    last_login: daysAgo(12),
  },
  // radu + mihai exist so a demo login reaches the WARN and STALE own-reading
  // states of the rate-limit meters (PRD #53); see mockMyRateLimitsByUser.
  {
    id: "u-radu",
    email: "radu@uzi.local",
    display_name: "Radu Marin",
    is_admin: false,
    is_active: true,
    autopilot_enabled: false,
    judge_enabled: false,
    ci_autofix_enabled: false,
    wait_on_limit: false,
    judge_anthropic_secret_id: null,
    judge_anthropic_secret_label: null,
    created_at: daysAgo(15),
    last_login: minsAgo(20),
  },
  {
    id: "u-mihai",
    email: "mihai@uzi.local",
    display_name: "Mihai Radu",
    is_admin: false,
    is_active: true,
    autopilot_enabled: false,
    judge_enabled: false,
    ci_autofix_enabled: false,
    wait_on_limit: false,
    judge_anthropic_secret_id: null,
    judge_anthropic_secret_label: null,
    created_at: daysAgo(25),
    last_login: daysAgo(1),
  },
];

// ── Claude rate limits (PRD #53) ─────────────────────────────────────────────
// Resets are epoch SECONDS derived from NOW so the demo countdowns stay fresh.
const NOW_SECS = Math.floor(NOW / 1000);
const H = 3600;
const D = 86_400;
const MIN = 60;

function okReading(
  pct5: number,
  in5: number,
  pct7: number,
  in7: number,
  syncedMins = 2,
  source: RateLimitSource = "usage_endpoint",
  stale = false,
): MyRateLimits {
  return {
    status: "ok",
    five_hour: { pct: pct5, resets_at: NOW_SECS + in5 },
    seven_day: { pct: pct7, resets_at: NOW_SECS + in7 },
    source,
    synced_at: minsAgo(syncedMins),
    stale,
  };
}

// The signed-in demo user's own readings (Settings card + sidebar). Since PRD #104
// this is ONE READING PER TOKEN: the default matches mockup frame A (8% / 27%,
// both green under the PRD #115 bands, "Live"), and the console key is busier so
// the two meter pairs are visibly different rather than duplicates.
export const mockMyRateLimits: MyRateLimits = okReading(8, 1 * H + 23 * MIN, 27, 2 * D + 4 * H);

// 🔴 auto_eligible HERE MUST AGREE WITH mockSecrets, TOKEN FOR TOKEN. The settings
// row draws its toggle from mockSecrets and its chip from this list, so a
// disagreement renders a checked box beside "not in pool" — which is what the demo
// build shipped until this was fixed, and exactly the contradiction the feature
// exists to prevent. The component now suppresses a chip that disagrees with the
// toggle rather than drawing it, so a mistake here degrades to a missing chip; that
// is a backstop, not a licence to let these drift.
export const mockMyTokenRateLimits: TokenRateLimits[] = [
  {
    secret_id: "sec-default",
    label: "default",
    is_default: true,
    // NOT pooled — the reserved-credential case D2 exists for. Its contrast with
    // console-key below is the thing worth seeing on a cold load.
    auto_eligible: false,
    auto_status: "not_pooled" as const,
    limits: mockMyRateLimits,
  },
  {
    // 34% / 22% — busier than the default but still both green under the #115
    // bands, so vlad reads "Live" on both tokens (mockup frame A) and is the
    // admin table's live_ok row.
    secret_id: "sec-console",
    label: "console-key",
    is_default: false,
    // Pooled AND pickable: the healthy chip, which had been rendering on no row at all.
    auto_eligible: true,
    auto_status: "eligible" as const,
    limits: okReading(34, 2 * H + 5 * MIN, 22, 3 * D + 2 * H, 3),
  },
  {
    // F2: `never polled` — pooled, but uzi has NEVER read a usage figure for it, so
    // the selector can never pick it. This is R7's silent no-op, and without a
    // fixture carrying it the state the chip mechanism exists to surface was
    // unreachable in the demo build. `unavailable` is what a token with no gauge row
    // actually returns.
    secret_id: "sec-never-polled",
    label: "refused-key",
    is_default: false,
    auto_eligible: true,
    auto_status: "no_reading" as const,
    limits: { status: "unavailable" as const },
  },
  {
    // F2: `low headroom` — pooled, current, and nearly spent. Distinct from the three
    // above in the way F4 is about: per D10 this token IS still picked when every
    // pooled token is this low, so it must not wear the same "skipped" tone.
    secret_id: "sec-low",
    label: "nearly-spent",
    is_default: false,
    auto_eligible: true,
    auto_status: "below_threshold" as const,
    limits: okReading(93, 40 * MIN, 88, 1 * D, 3),
  },
  {
    // PRD #217 M3: a `limit_report` reading — the only fixture that makes the
    // park-time source disclosure (D6) visible under VITE_UZI_MOCK=1. This token
    // just hit its five-hour wall, so the park wrote that window 100% consumed with
    // source `limit_report` and DID NOT bump synced_at (D3): the reading is 100%
    // but the "updated 14m ago" line beside it is OLDER, which is exactly the state
    // the "Recorded at usage limit" badge exists to explain. Kept NON-pooled so it
    // stays outside pooledFixtureStatus while still agreeing with mockSecrets.
    secret_id: "sec-parked",
    label: "parked-key",
    is_default: false,
    auto_eligible: false,
    auto_status: "not_pooled" as const,
    limits: okReading(100, 35 * MIN, 40, 2 * D + 5 * H, 14, "limit_report"),
  },
];

// tokenised wraps a single reading as a one-token list, for the personas whose
// fixture is a single credential.
function tokenised(limits: MyRateLimits, label = "default"): TokenRateLimits[] {
  return limits.status === "no_token"
    ? [] // token-less is an EMPTY list since M5, not a status
    : [
        {
          secret_id: `sec-${label}`,
          label,
          is_default: true,
          auto_eligible: false,
          auto_status: "not_pooled" as const,
          limits,
        },
      ];
}

// Per-persona readings so a demo login as a seeded non-admin reaches every own-
// reading state; anyone else gets the live-ok default (u-admin). warn (radu) and
// stale (mihai) are here so the sidebar-dim + Settings "Stale" badge and a warn-
// tone bar are browsable, not just visible in the admin table.
export const mockMyRateLimitsByUser: Record<string, TokenRateLimits[]> = {
  "u-admin": mockMyTokenRateLimits, // live ok, TWO tokens
  "u-radu": tokenised(okReading(62, 44 * MIN, 83, 1 * D + 9 * H, 3)), // warn (7d 83%)
  "u-mira": tokenised(okReading(97, 2 * H + 10 * MIN, 71, 4 * D + 1 * H, 1)), // danger (5h 97%)
  // stale own-reading: no live countdown (resets null), aged synced_at, stale flag.
  "u-mihai": tokenised({
    status: "ok",
    five_hour: { pct: 31, resets_at: null },
    seven_day: { pct: 12, resets_at: null },
    source: "header_probe",
    synced_at: minsAgo(180),
    stale: true,
  }),
  "u-andrei": tokenised({ status: "unavailable" }),
  "u-dan": [], // token-less: an EMPTY list, not a no_token status (PRD #104 M5)
};

// The admin all-users table (mockup frame C) plus a warn row and an unavailable
// row, so every row state is demonstrable: live-ok, live-warn, live-danger,
// stale+vault-locked, unavailable, no_token.
export const mockAdminRateLimits: AdminRateLimitUser[] = [
  // vlad holds TWO tokens, so the admin table's per-user grouping is demonstrable.
  { id: "u-admin", email: "vlad@example.com", name: "vlad", vault_locked: false, tokens: mockMyTokenRateLimits },
  { id: "u-radu", email: "radu@example.com", name: "radu", vault_locked: false, tokens: tokenised(okReading(62, 44 * MIN, 83, 1 * D + 9 * H, 3)) },
  { id: "u-ana", email: "ana@example.com", name: "ana", vault_locked: false, tokens: tokenised(okReading(97, 2 * H + 10 * MIN, 71, 4 * D + 1 * H, 1)) },
  // sorin demonstrates the new PRD #115 85–94 danger band: 5h at 88% paints a red
  // bar (danger tone ≥85) but the status pill stays a green "Live" because no
  // window has crossed 95 (the badge stays decoupled at ≥95).
  { id: "u-sorin", email: "sorin@example.com", name: "sorin", vault_locked: false, tokens: tokenised(okReading(88, 3 * H + 5 * MIN, 76, 3 * D + 6 * H, 4)) },
  { id: "u-mihai", email: "mihai@example.com", name: "mihai", vault_locked: true, tokens: tokenised(okReading(31, 0, 12, 0, 180, "header_probe", true)) },
  { id: "u-dana", email: "dana@example.com", name: "dana", vault_locked: false, tokens: tokenised({ status: "unavailable" }) },
  { id: "u-irina", email: "irina@example.com", name: "irina", vault_locked: false, tokens: [] },
];

// ── Notifications inbox (PRD #46 M2) ─────────────────────────────────────────
// The mock keeps the full row (incl. user_id + owner) so listNotifications can
// filter to the caller (own view) or show everyone (admin all-view), exactly like
// the API. payload follows the { title, body } convention the inbox renders; the
// judge is the seeded tenant. A couple of rows belong to another user so the admin
// all-view has cross-owner content to show.
export interface MockNotification {
  id: string;
  user_id: string;
  kind: string;
  payload: Record<string, unknown>;
  run_id: string | null;
  review_id: string | null;
  read_at: string | null;
  created_at: string;
  owner_email: string;
  owner_display_name: string | null;
}

export const mockNotifications: MockNotification[] = [
  {
    id: "ntf-1",
    user_id: mockAdmin.id,
    kind: "judge_review",
    payload: { title: "Run review ready", body: "verdict: issues — 2 recommendations, incl. a missing worker tool" },
    run_id: "run-done",
    review_id: null,
    read_at: null,
    created_at: minsAgo(6),
    owner_email: mockAdmin.email,
    owner_display_name: mockAdmin.display_name,
  },
  {
    // A NON-judge row (PRD #98 M5). It is here so demo mode renders every state the inbox
    // now has: this row deep-links to /runs/{id} while its judge neighbours deep-link to
    // /judge?run={id}, and by sitting BETWEEN ntf-1 and ntf-2 it also breaks the judge run
    // — leaving one ungrouped judge row above it and a grouped pair below. Without it the
    // demo showed exactly one of the three states, and the grouping is what would have
    // hidden the retarget from anyone looking.
    id: "ntf-mr",
    user_id: mockAdmin.id,
    kind: "mr_merged",
    payload: { title: "Merge request merged", body: "!42 — add rg to the worker image" },
    run_id: "run-done",
    review_id: null,
    read_at: null,
    created_at: minsAgo(20),
    owner_email: mockAdmin.email,
    owner_display_name: mockAdmin.display_name,
  },
  {
    id: "ntf-2",
    user_id: mockAdmin.id,
    kind: "judge_review",
    payload: { title: "Run review ready", body: "verdict: ideal — nothing to change" },
    run_id: "run-live",
    review_id: null,
    read_at: null,
    created_at: minsAgo(58),
    owner_email: mockAdmin.email,
    owner_display_name: mockAdmin.display_name,
  },
  {
    id: "ntf-3",
    user_id: mockAdmin.id,
    kind: "judge_review",
    payload: { title: "Run review ready", body: "verdict: ok — one template tweak suggested" },
    run_id: null,
    review_id: null,
    read_at: daysAgo(1),
    created_at: daysAgo(1),
    owner_email: mockAdmin.email,
    owner_display_name: mockAdmin.display_name,
  },
  {
    id: "ntf-4",
    user_id: "u-mira",
    kind: "judge_review",
    payload: { title: "Run review ready", body: "verdict: issues — worker missing `jq`" },
    run_id: null,
    review_id: null,
    read_at: null,
    created_at: minsAgo(120),
    owner_email: "mira@uzi.local",
    owner_display_name: "Mira Ionescu",
  },
];

// ── Run judge reviews (PRD #46 M4) ───────────────────────────────────────────
// Seeded verdicts for the run-page review panel, keyed by target run id. run-done
// carries a full "issues" verdict with recommendations so the panel + preview mode
// have something to show; other terminal runs (e.g. run-failed) have no review, so
// their panel renders the "not judged yet" state with a Run-judge button.
export interface MockReview {
  id: string;
  target_run_id: string;
  verdict: ReviewVerdict;
  summary_md: string;
  judge_model: string;
  status: ReviewStatus;
  created_at: string;
  updated_at: string;
  recommendations: {
    id: string;
    category: RecommendationCategory;
    target: string;
    rationale_md: string;
    confidence: "" | "low" | "medium" | "high";
    created_at: string;
  }[];
  // Settled recommendation→issue links (PRD #68), keyed by (category, target). The panel
  // renders a filed row for a matching recommendation instead of the File-issue button.
  filed_issues: {
    category: RecommendationCategory;
    target: string;
    issue_iid: number;
    issue_url: string;
    filed_at: string;
  }[];
  // Triage dispositions (PRD #94), coordinate-keyed like filed_issues. `stale` is a
  // seeded flag (the real server hash-compares); mockApi.recomputeTriage derives
  // `triage` from these + filed_issues on every mutation, matching the server ladder.
  //
  // `set_via` mirrors the recommendation_dispositions column (PRD #98 Decision 6): absent
  // means a PERSON set it, "issue_close" means the M6 poller sync did. It is a MOCK-side
  // extension of Disposition because the run-page DTO does not carry provenance — only the
  // Judge menu's occurrence does — and without it the mock cannot render an auto-done at
  // all, which is the one state the "done via #IID" label exists for.
  dispositions: (Disposition & { set_via?: "issue_close" })[];
  triage: TriageCounts;
  // The judge run's OWN timing + token/cost usage (PRD #69 M6), for the panel's
  // time/tokens/cost strip. Shape mirrors the real ReviewDTO.judge_run: `usage` is null
  // for a pre-feature judge that posted no result frame (the panel renders NO strip),
  // and a set object renders the four-tile strip. Kept honest per PRD #311 mock currency.
  judge_run?: {
    judge_run_id: string;
    claimed_at: string | null;
    started_at: string | null;
    finished_at: string | null;
    usage: {
      input_tokens: number;
      cache_read_tokens: number;
      cache_creation_tokens: number;
      output_tokens: number;
      cost_usd: number;
    } | null;
  };
}

export const mockReviews: MockReview[] = [
  {
    id: "rev-done",
    target_run_id: "run-done",
    verdict: "issues",
    summary_md:
      "The run **delivered the feature** and opened an MR, but it lost time on two avoidable things:\n\n" +
      "- a missing worker tool (`shellcheck`) forced a fallback\n" +
      "- the same search re-ran three times before finding the handler\n\n" +
      "The failing lookup that cost the most time:\n\n" +
      "```sh\ngrep -rn 'handleReview' api/internal/handler\n```\n\n" +
      "See the [run timeline](https://gitlab.example.com/vtmocanu/uzi/-/issues/71) for the full trace.",
    judge_model: "haiku",
    status: "complete",
    created_at: minsAgo(6),
    updated_at: minsAgo(6),
    recommendations: [
      {
        id: "rec-1",
        category: "install_worker_tool",
        target: "shellcheck",
        rationale_md:
          "The agent tried **`shellcheck`** twice and hit `command not found`:\n\n" +
          "- first on the pre-commit hook\n" +
          "- then on the manual lint pass\n\n" +
          "```sh\nshellcheck scripts/*.sh\n```\n\n" +
          "Installing it in the worker image would save the fallback. See the [worker image spec](https://gitlab.example.com/vtmocanu/uzi/-/blob/main/docs/judge.md).",
        confidence: "high",
        created_at: minsAgo(6),
      },
      {
        id: "rec-2",
        category: "improve_agent",
        target: "reviewer",
        rationale_md:
          "The repo reviewer agent approved on the first pass without checking the migration ordering; tightening its checklist would catch this class of issue.",
        confidence: "medium",
        created_at: minsAgo(6),
      },
      {
        // No category default resolves (selfimprove_repo unset by default), so the draft
        // opens with an empty picker + a reason — exercises mock state D in the demo.
        id: "rec-3",
        category: "improve_uzi",
        target: "api/internal/poller",
        rationale_md:
          "The run waited ~4m for the first poll tick after the label was applied; a webhook path would cut queue-to-claim latency.",
        confidence: "low",
        created_at: minsAgo(6),
      },
      {
        // Already filed with a RECENT link — the non-stale state C variant on load.
        id: "rec-4",
        category: "add_agent",
        target: "deploy-agent",
        rationale_md:
          "No agent owns the deploy step, so the run hand-rolled it; a dedicated deploy-agent would standardize it.",
        confidence: "medium",
        created_at: minsAgo(6),
      },
      {
        // Dismissed as a false positive (PRD #94) → the "Dismissed · Not an issue"
        // danger chip and the false-positive count.
        id: "rec-5",
        category: "improve_uzi",
        target: "run timeout too aggressive",
        rationale_md:
          "The judge read the run as timing out, but it was a deliberate cancel from the approval gate — false positive.",
        confidence: "low",
        created_at: minsAgo(6),
      },
    ],
    // Two seeded links so both state-C variants render on load: rec-1's filed_at is OLDER
    // than the review's updated_at (minsAgo(6)) → the STALE variant ("filed for an earlier
    // version"); rec-4's is NEWER → the plain filed row. The idle button (mock A) shows on
    // rec-2/rec-3.
    filed_issues: [
      {
        category: "install_worker_tool",
        target: "shellcheck",
        issue_iid: 71,
        issue_url: "https://gitlab.example.com/vtmocanu/uzi/-/issues/71",
        filed_at: minsAgo(20),
      },
      {
        category: "add_agent",
        target: "deploy-agent",
        issue_iid: 72,
        issue_url: "https://gitlab.example.com/vtmocanu/uzi/-/issues/72",
        filed_at: minsAgo(2),
      },
    ],
    // Seeded so the demo stack renders EVERY triage state on load (PRD #94):
    //  - rec-1 (shellcheck): DONE + STALE — filed AND marked done (done > filed on the
    //    ladder), and flagged stale to render the "recommendation changed" note.
    //  - rec-2 (reviewer): DISMISSED · Won't do.
    //  - rec-5 (run timeout…): DISMISSED · Not an issue (a false positive).
    //  - rec-3 (poller): TO DO (no disposition, no filed link).
    //  - rec-4 (deploy-agent): FILED (a settled link, no disposition).
    dispositions: [
      { category: "install_worker_tool", target: "shellcheck", status: "done", reason: "", set_at: minsAgo(120), stale: true },
      { category: "improve_agent", target: "reviewer", status: "dismissed", reason: "wont_do", set_at: minsAgo(40), stale: false },
      { category: "improve_uzi", target: "run timeout too aggressive", status: "dismissed", reason: "not_an_issue", set_at: minsAgo(15), stale: false },
    ],
    // total 5: todo 1 (rec-3), filed 1 (rec-4), done 1 (rec-1), dismissed 2 (rec-2,
    // rec-5), of which 1 is a false positive (rec-5). Recomputed by mockApi on mutation.
    triage: { total: 5, todo: 1, filed: 1, done: 1, dismissed: 2, false_positives: 1 },
    // A judged-on-metered-tokens judge (PRD #69 M6): the strip shows real tokens, a ~14s
    // duration, and a non-zero cost.
    judge_run: {
      judge_run_id: "judge-run-done",
      claimed_at: secsAgo(378),
      started_at: secsAgo(374),
      finished_at: secsAgo(360),
      usage: {
        input_tokens: 48200,
        cache_read_tokens: 12800,
        cache_creation_tokens: 3100,
        output_tokens: 1840,
        cost_usd: 0.42,
      },
    },
  },
  // ── Two more reviews so the Judge menu (PRD #98 M3) demos dedup across runs ──────
  // These share coordinates with rev-done so the by-target grouping has something to
  // dedup: `improve_uzi / api/internal/poller` recurs in all three runs (the frequency
  // signal, top of To triage), `install_worker_tool / shellcheck` is DONE in run-done
  // but TODO here (a PARTIALLY-SETTLED group — some occurrences settled, some open, the
  // shape the rollup and the scope=open fan-out hinge on), and `add_agent / deploy-agent`
  // is filed in two runs (a Filed-tab group seen in 2 runs).
  {
    id: "rev-closed",
    target_run_id: "run-closed",
    verdict: "issues",
    summary_md:
      "The retry landed, but the worker was still missing shellcheck and the first poll tick lagged — both recurring across recent runs.",
    judge_model: "haiku",
    status: "complete",
    created_at: minsAgo(120),
    updated_at: minsAgo(120),
    recommendations: [
      {
        id: "rc-1",
        category: "install_worker_tool",
        // Same coordinate as rev-done's rec-1 (done there) → this group is partially
        // settled: rolls up To triage because THIS occurrence is open.
        target: "shellcheck",
        rationale_md: "`shellcheck` was missing again on this run's worker image — the second run to hit it.",
        confidence: "high",
        created_at: minsAgo(120),
      },
      {
        id: "rc-2",
        category: "improve_uzi",
        target: "api/internal/poller",
        rationale_md: "Queue-to-claim latency again dominated the run's wall time; a webhook path keeps coming up.",
        confidence: "medium",
        created_at: minsAgo(120),
      },
      {
        id: "rc-3",
        category: "enable_tool",
        target: "ripgrep",
        rationale_md: "The agent shelled out to `grep -r` repeatedly; enabling ripgrep would speed the search phase.",
        confidence: "low",
        created_at: minsAgo(120),
      },
    ],
    // ripgrep was filed as #91 and that issue has since been CLOSED, so the M6 poller sync
    // marked the coordinate done on its own — the auto-done the Judge menu labels
    // "Done via #91", visibly distinct from rev-cancelled's hand-marked "Done" on
    // adjust_template/coder. Both grammars are seeded so the difference is demoable rather
    // than merely implemented.
    filed_issues: [
      {
        category: "enable_tool",
        target: "ripgrep",
        issue_iid: 91,
        issue_url: "https://gitlab.example.com/vtmocanu/uzi/-/issues/91",
        filed_at: minsAgo(90),
      },
    ],
    dispositions: [
      // set_by_user_id would be NULL server-side: nobody clicked this.
      { category: "enable_tool", target: "ripgrep", status: "done", reason: "", set_at: minsAgo(30), stale: false, set_via: "issue_close" },
    ],
    // total 3: todo 2 (rc-1, rc-2), done 1 (rc-3 — auto, via the closed #91). The done rung
    // outranks filed on the shared ladder, so the filed link above does NOT make it filed.
    triage: { total: 3, todo: 2, filed: 0, done: 1, dismissed: 0, false_positives: 0 },
    // A judge run on a SUBSCRIPTION plan (PRD #69 M6): the SDK prices it at $0, so the
    // Cost tile renders "—" (never "$0.00") while the token + duration tiles still show.
    judge_run: {
      judge_run_id: "judge-run-closed",
      claimed_at: secsAgo(300),
      started_at: secsAgo(297),
      finished_at: secsAgo(288),
      usage: {
        input_tokens: 31500,
        cache_read_tokens: 9200,
        cache_creation_tokens: 0,
        output_tokens: 1120,
        cost_usd: 0,
      },
    },
  },
  {
    id: "rev-cancelled",
    target_run_id: "run-cancelled",
    verdict: "ok",
    summary_md: "A short run; the healthcheck landed cleanly. The poller latency note recurs, and a template tweak is worth doing.",
    judge_model: "haiku",
    status: "complete",
    created_at: daysAgo(3),
    updated_at: daysAgo(3),
    recommendations: [
      {
        id: "rx-1",
        category: "improve_uzi",
        // Third occurrence of the poller coordinate → "seen in 3 runs", the top group.
        target: "api/internal/poller",
        rationale_md: "Same first-poll latency as the other runs — this is the most-recurring recommendation in your backlog.",
        confidence: "medium",
        created_at: daysAgo(3),
      },
      {
        id: "rx-2",
        category: "add_agent",
        // Same coordinate as rev-done's rec-4 (also filed) → a Filed-tab group seen in 2.
        target: "deploy-agent",
        rationale_md: "The deploy step was hand-rolled here too; a dedicated deploy-agent would standardize it.",
        confidence: "medium",
        created_at: daysAgo(3),
      },
      {
        id: "rx-3",
        category: "adjust_template",
        target: "coder",
        rationale_md: "The coder re-ran a failing test without reading the error; a template line to read the error first would help.",
        confidence: "low",
        created_at: daysAgo(3),
      },
    ],
    // deploy-agent filed here → a Filed-tab group seen in 2 runs.
    filed_issues: [
      {
        category: "add_agent",
        target: "deploy-agent",
        issue_iid: 88,
        issue_url: "https://gitlab.example.com/vtmocanu/uzi/-/issues/88",
        filed_at: daysAgo(3),
      },
    ],
    // coder marked done → a Done-tab group.
    dispositions: [
      { category: "adjust_template", target: "coder", status: "done", reason: "", set_at: daysAgo(2), stale: false },
    ],
    // total 3: todo 1 (rx-1), filed 1 (rx-2), done 1 (rx-3), dismissed 0.
    triage: { total: 3, todo: 1, filed: 1, done: 1, dismissed: 0, false_positives: 0 },
    // A PRE-FEATURE judge (PRD #69 M6): it posted no result frame, so there is no
    // run_usage row — usage is null and the panel renders NO cost/time strip (never a
    // fabricated 0). The timings are still present (a pre-feature judge could report
    // running), which is why the strip gates on `usage`, not on `judge_run`.
    judge_run: {
      judge_run_id: "judge-run-cancelled",
      claimed_at: secsAgo(500),
      started_at: secsAgo(496),
      finished_at: secsAgo(480),
      usage: null,
    },
  },
];

// ── Pending judges (PRD #119) ────────────────────────────────────────────────
// The ACTIVE judge run per TARGET run id, mirroring the server's
// uq_runs_one_active_judge_per_target read: at most one entry per target, and it is
// orthogonal to whether a review exists. Keyed by target id rather than modelled as a
// judge Run because the wire has no back-link to derive it from — the run DTO carries
// no target_run_id — and a mock must not invent wire fields (see mockApi.test.ts's
// set_via case). getRunReview reads it; rerunJudge 409s off it, the way the unique
// index makes the real server 409.
//
// Four terminal fixtures cover the panel's four states between them — one each, so
// dropping any one of them takes a state off the demo:
//   run-failed    — no review + scheduled: the empty-state "verdict will appear here"
//                   copy with the button disabled and relabelled "Judge scheduled";
//   run-closed    — a seeded review + running: the re-judge-in-flight note over an
//                   existing verdict;
//   run-done      — a seeded review and NO entry: the settled panel with a live
//                   Re-run judge button, which must stay demoable;
//   run-unjudged  — NO review and NO entry: the never-judged empty state with the
//                   ENABLED Run judge button. Added after #119 took run-failed for
//                   the scheduled case and left this one, the state the PRD promises
//                   is unchanged, with no fixture at all.
export const mockPendingJudges: Record<string, PendingJudge> = {
  "run-failed": { state: "scheduled", enqueued_at: minsAgo(2) },
  "run-closed": { state: "running", enqueued_at: minsAgo(9) },
};

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
];

// ── Forge ────────────────────────────────────────────────────────────────────

export const mockConnection: ForgeConnection = {
  id: "conn-1",
  forge_type: "gitlab",
  base_url: "https://gitlab.example.com",
  bot_username: "uzi-bot",
  bot_forge_user_id: 4021,
  human_username: "vlad",
  created_at: daysAgo(30),
  last_verified_at: minsAgo(42),
  // Demo happy path: least-privilege ✓ — api-only token, Developer on protected
  // mains. The finding states are exercised in the component tests.
  privilege_status: "ok",
  privilege_checked_at: minsAgo(20),
  privilege_report: {
    checked_at: minsAgo(20),
    status: "ok",
    token: { scopes: ["api"], active: true, violations: [], warnings: [] },
    repos: [
      { repo_id: "repo-uzi", path: "vtmocanu/uzi", role: "write", member: true, findings: [] },
      { repo_id: "repo-atlas", path: "vtmocanu/atlas-api", role: "write", member: true, findings: [] },
    ],
  },
};

export const mockForgeConfig = {
  allowed_base_urls: ["https://gitlab.example.com"],
  forge_types: ["gitlab"],
};

// A two-forge config variant (PRD #65 D11) for exercising the connect-form
// forge-type picker's VISIBLE branch in tests. Deliberately NOT wired into the demo
// mockApi.forgeConfig — the demo mirrors production, which advertises only
// ["gitlab"] until M6b, so the picker stays hidden there (dark landing).
export const mockForgeConfigMultiForge = {
  allowed_base_urls: ["https://gitlab.example.com", "https://forge.example.com"],
  forge_types: ["gitlab", "forgejo"],
};

// A three-forge config variant (PRD #238 D2/D11) advertising GitHub alongside GitLab
// and Forgejo, for exercising the connect-form picker with GitHub's arm. Like the
// two-forge variant, deliberately NOT wired into mockApi.forgeConfig — production
// advertises only ["gitlab"] until the go-live flip (M10), so the picker stays hidden
// in the demo (dark landing).
export const mockForgeConfigAllForges = {
  allowed_base_urls: [
    "https://gitlab.example.com",
    "https://forge.example.com",
    "https://github.com",
  ],
  forge_types: ["gitlab", "forgejo", "github"],
};

// PRD #66 M9 (D8): the atlas repo's admin override, shared by the Boards repo fixture
// and the admin blocked-repos list so both read ONE literal. Typed as the wire
// GuardrailOverrideMeta (the same shape RepoDTO.guardrail_override and
// BlockedRepoDTO.guardrail_override carry).
export const mockAtlasOverride: GuardrailOverrideMeta = {
  reason: "forge fix scheduled for next sprint; accepting the risk until then",
  by: "vlad@example.com",
  at: daysAgo(3),
};

export const mockRepos: Repo[] = [
  {
    id: "repo-uzi",
    connection_id: "conn-1",
    forge_project_id: 118,
    path_with_namespace: "vtmocanu/uzi",
    web_url: "https://gitlab.example.com/vtmocanu/uzi",
    default_branch: "main",
    enabled: true,
    repo_skills_enabled: true,
    // A deliberate mix: repo skills on, repo instructions off — so the Trusted-repo
    // panel's sub-toggle independence is exercisable under VITE_UZI_MOCK=1.
    repo_claudemd_enabled: false,
    repo_devbox_opt_in: true,
    pipeline: {
      status: "failed",
      web_url: "https://gitlab.example.com/vtmocanu/uzi/-/pipelines/4242",
      ref: "main",
      pipeline_id: 4242,
      synced_at: minsAgo(1),
    },
    // PRD #66 M8 (D8): no admin guardrail override active. The M9 UI renders off this.
    guardrail_override: null,
    // PRD #66 M9 (D8): not refused by the guardrail (server-computed).
    guardrail_blocked: false,
  },
  {
    id: "repo-atlas",
    connection_id: "conn-1",
    forge_project_id: 204,
    path_with_namespace: "vtmocanu/atlas-api",
    web_url: "https://gitlab.example.com/vtmocanu/atlas-api",
    default_branch: "main",
    enabled: true,
    repo_skills_enabled: false,
    repo_claudemd_enabled: false,
    repo_devbox_opt_in: false,
    pipeline: {
      status: "success",
      web_url: "https://gitlab.example.com/vtmocanu/atlas-api/-/pipelines/3311",
      ref: "main",
      pipeline_id: 3311,
      synced_at: minsAgo(2),
    },
    // PRD #66 M9 (D8): an admin has explicitly allowed this repo through the
    // guardrail — the "allowed by admin" badge + Revoke path in the demo.
    guardrail_override: { ...mockAtlasOverride },
    guardrail_blocked: false,
  },
  {
    // PRD #66 M9 (D8): a repo the push/merge guardrail REFUSES right now
    // (guardrail_blocked, server-computed). It makes the Boards "runs blocked" badge,
    // the admin inline "Allow anyway" modal, and the member "ask an admin" pointer all
    // reachable under VITE_UZI_MOCK=1, and it is the row the admin blocked-repos list's
    // Allow/Revoke round-trips against (see mockBlockedRepoMeta + mockApi).
    id: "repo-payments",
    connection_id: "conn-1",
    forge_project_id: 512,
    path_with_namespace: "team-beta/payments-api",
    web_url: "https://gitlab.example.com/team-beta/payments-api",
    default_branch: "main",
    enabled: true,
    repo_skills_enabled: false,
    repo_claudemd_enabled: false,
    repo_devbox_opt_in: false,
    pipeline: null,
    guardrail_override: null,
    guardrail_blocked: true,
  },
  {
    id: "repo-www",
    connection_id: "conn-1",
    forge_project_id: 87,
    path_with_namespace: "example/website",
    web_url: "https://gitlab.example.com/example/website",
    default_branch: "main",
    enabled: false,
    repo_skills_enabled: false,
    repo_claudemd_enabled: false,
    repo_devbox_opt_in: false,
    pipeline: null,
    guardrail_override: null,
    guardrail_blocked: false,
  },
];

// PRD #66 M9 (D8): per-repo metadata the admin cross-user blocked-repos list needs but
// the wire Repo does not carry — owner identity and the UNDERLYING (pre-override) block
// reasons live on the connection/report server-side. mockApi.adminListBlockedRepos JOINS
// this with the SHARED, mutable repos state (the same state the Boards page and the
// Allow/Revoke mutations act on), so a demo Allow or Revoke round-trips into the list
// instead of 404ing against a static deep-copy. Keyed by repo id; a repo absent here
// falls back to the demo owner/connection.
//
// block_messages here are the repo's UNDERLYING waivable block reasons (what it shows
// when blocked, and what re-arms the guardrail on Revoke). An overridden-clean repo like
// repo-atlas still lists them so revoking its override re-blocks it — the admin DTO emits
// [] for it while the override stands (it is not currently blocked).
export interface MockBlockedRepoMeta {
  owner_id: string;
  owner_email: string;
  forge_type: string;
  block_messages: string[];
  privilege_status: string | null;
  privilege_checked_at: string | null;
}

export const mockBlockedRepoMeta: Record<string, MockBlockedRepoMeta> = {
  // Cross-user: an owner's repo the guardrail refuses right now (blocked).
  "repo-payments": {
    owner_id: "u-dana",
    owner_email: "dana@example.com",
    forge_type: "gitlab",
    block_messages: [
      "the default branch is protected but the write role (Developer) may push to it",
      "the write role (Developer) may merge to the default branch",
    ],
    privilege_status: "violations",
    privilege_checked_at: minsAgo(18),
  },
  // Overridden-clean now, but carries the underlying waivable block so Revoke re-arms it.
  "repo-atlas": {
    owner_id: "u-admin",
    owner_email: "vlad@example.com",
    forge_type: "gitlab",
    block_messages: ["the default branch is protected but the write role (Developer) may push to it"],
    privilege_status: "violations",
    privilege_checked_at: minsAgo(18),
  },
};

// ── Boards ───────────────────────────────────────────────────────────────────

// LIVE_RUN_ID is the seeded run whose message stream is SIMULATED live: the mock
// engine starts a timed script the first time the run view subscribes to it.
// Declared here (above the boards) because a card's latest_run references it.
export const LIVE_RUN_ID = "run-live";

const uziUrl = (iid: number) => `https://gitlab.example.com/vtmocanu/uzi/-/issues/${iid}`;
const atlasUrl = (iid: number) => `https://gitlab.example.com/vtmocanu/atlas-api/-/issues/${iid}`;

// latestRun builds a card's latest_run snapshot (PRD #12 M2). Kept as inline
// literals per card so the seed stays declarative; every id matches a real run
// in mockRuns so the card's "view run" link and history resolve.
function latestRun(fields: Partial<LatestRun> & Pick<LatestRun, "id" | "status">): LatestRun {
  return {
    mr_iid: null,
    mr_web_url: null,
    mr_state: null,
    failure_reason: null,
    stop_kind: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    owner_name: "Vlad",
    worker_name: null,
    is_mine: true,
    run_count: 1,
    created_at: minsAgo(30),
    updated_at: minsAgo(30),
    ...fields,
  };
}

// boardFixtures is authored in whatever order reads well while editing. What the
// demo board RENDERS in is fixed below, once, rather than depending on the order
// somebody happened to paste a card in.
const boardFixtures: Record<string, Board> = {
  "repo-uzi": {
    repo_id: "repo-uzi",
    path_with_namespace: "vtmocanu/uzi",
    forge_type: "gitlab",
    web_url: "https://gitlab.example.com/vtmocanu/uzi",
    columns: [
      { label_name: "Ready", position: 0 },
      { label_name: "In progress", position: 1 },
      { label_name: "Review", position: 2 },
    ],
    cards: [
      {
        iid: 31,
        title: "Add CSV export to the runs list",
        state: "opened",
        labels: ["PRD"],
        web_url: uziUrl(31),
        author: "mira",
        forge_type: "gitlab",
        has_prd_link: false,
        column: "",
        closed: false,
        conflict: false,
        forge_updated_at: minsAgo(5),
        latest_run: null,
        pipeline: null,
      },
      {
        iid: 29,
        title: "Retry failed forge column moves with backoff",
        state: "opened",
        labels: ["PRD"],
        web_url: uziUrl(29),
        author: "vlad",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "",
        closed: false,
        conflict: false,
        forge_updated_at: minsAgo(90),
        latest_run: null,
        // "canceled" → the neutral tone (also covers skipped / no-CI).
        pipeline: {
          ref: "agent/issue-29",
          status: "canceled",
          web_url: "https://gitlab.example.com/vtmocanu/uzi/-/pipelines/4188",
          pipeline_id: 4188,
          synced_at: minsAgo(6),
        },
      },
      {
        iid: 27,
        title: "Dark-mode toggle for the docs section",
        state: "opened",
        // A CONTENT label alongside the workflow ones. Before this, not one mock card
        // carried one, so M4's label chips rendered on exactly zero cards in the build
        // most people click (web-ux S7). "Ready" is this card's own column and is
        // correctly chipless — which is why a naive "the fixture has labels" check
        // would not have caught it.
        labels: ["PRD", "Ready", "enhancement"],
        web_url: uziUrl(27),
        author: "vlad",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "Ready",
        closed: false,
        conflict: false,
        forge_updated_at: minsAgo(20),
        latest_run: null,
        // "manual" → the attention tone (a human must click play in GitLab).
        pipeline: {
          ref: "agent/issue-27",
          status: "manual",
          web_url: "https://gitlab.example.com/vtmocanu/uzi/-/pipelines/4190",
          pipeline_id: 4190,
          synced_at: minsAgo(4),
        },
      },
      {
        iid: 26,
        title: "Board card badges for MR pipeline status",
        state: "opened",
        labels: ["PRD", "Ready"],
        web_url: uziUrl(26),
        author: "andrei",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "Ready",
        closed: false,
        conflict: false,
        // Freshly queued, not yet claimed by a worker: renders the "queued" badge
        // (violet under the mission theme, gray under ember) on the board card.
        forge_updated_at: minsAgo(240),
        latest_run: latestRun({
          id: "run-queued",
          status: "queued",
          created_at: minsAgo(1),
          updated_at: minsAgo(1),
        }),
        pipeline: null,
      },
      {
        iid: 24,
        title: "Worker heartbeat metrics endpoint",
        state: "opened",
        labels: ["PRD", "In progress"],
        web_url: uziUrl(24),
        author: "vlad",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "In progress",
        closed: false,
        conflict: false,
        forge_updated_at: minsAgo(45),
        latest_run: latestRun({
          id: LIVE_RUN_ID,
          status: "running",
          worker_name: "laptop",
          created_at: minsAgo(2),
          updated_at: minsAgo(1),
        }),
        // The agent branch's MR pipeline is still running.
        pipeline: {
          status: "running",
          web_url: "https://gitlab.example.com/vtmocanu/uzi/-/pipelines/4239",
          ref: "agent/issue-24",
          pipeline_id: 4239,
          synced_at: minsAgo(1),
        },
      },
      {
        iid: 22,
        title: "Per-run cost budget with hard stop",
        state: "opened",
        labels: ["PRD", "In progress", "Review", "bug"],
        web_url: uziUrl(22),
        author: "mira",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "Review",
        closed: false,
        conflict: true,
        forge_updated_at: minsAgo(1500),
        latest_run: null,
        // A red per-card pipeline: the Fix CI affordance (M6) will hang off this.
        pipeline: {
          status: "failed",
          web_url: "https://gitlab.example.com/vtmocanu/uzi/-/pipelines/4201",
          ref: "agent/issue-22",
          pipeline_id: 4201,
          synced_at: minsAgo(3),
        },
      },
      {
        // PRD #35: the board's only parked card. It is what makes runBadge's
        // `limit_wait` arm reachable in mock mode — and the card is deliberately the
        // STATIC form: LatestRun carries no reset timestamps, so there is nothing to
        // count down from here. If a future change makes this card show a countdown,
        // something widened LatestRun and the Board.tsx zero-delta constraint is gone.
        iid: 23,
        title: "Stream run logs to the CLI with backpressure",
        state: "opened",
        labels: ["PRD", "In progress"],
        web_url: uziUrl(23),
        author: "mira",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "In progress",
        closed: false,
        conflict: false,
        // Required on Card since PRD #102 M6. Stamped at the run's creation rather
        // than "now": the issue has not been touched on the forge since uzi picked
        // it up, so a fresh timestamp here would put a parked card at the top of the
        // Updated sort and misrepresent a stalled run as the liveliest thing on the
        // board.
        forge_updated_at: minsAgo(141),
        latest_run: latestRun({
          id: "run-limit-wait",
          status: "limit_wait",
          worker_name: "laptop",
          created_at: minsAgo(141),
          updated_at: minsAgo(6),
        }),
        pipeline: null,
      },
      {
        iid: 21,
        title: "Plan-approval notifications via email",
        state: "opened",
        labels: ["PRD", "Review"],
        web_url: uziUrl(21),
        author: "vlad",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "Review",
        closed: false,
        conflict: false,
        forge_updated_at: minsAgo(8),
        latest_run: latestRun({
          id: "run-awaiting",
          status: "awaiting_approval",
          worker_name: "laptop",
          created_at: minsAgo(10),
          updated_at: minsAgo(6),
        }),
        pipeline: null,
      },
      {
        iid: 18,
        title: "Run view: fold tool results under their calls",
        state: "closed",
        labels: ["PRD"],
        web_url: uziUrl(18),
        author: "vlad",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "",
        closed: true,
        conflict: false,
        forge_updated_at: minsAgo(3000),
        latest_run: latestRun({
          id: "run-done",
          status: "completed",
          mr_iid: 42,
          mr_web_url: "https://gitlab.example.com/vtmocanu/uzi/-/merge_requests/42",
          mr_state: "merged",
          worker_name: "laptop",
          created_at: minsAgo(225),
          updated_at: minsAgo(184),
        }),
        pipeline: null,
      },
      {
        // A completed run whose MR was closed unmerged: the reopen-watcher (PRD #24)
        // bounced the card back to In progress, and the chip shows the closed state.
        iid: 12,
        title: "Retry the flaky worker heartbeat probe",
        state: "opened",
        labels: ["PRD", "In progress"],
        web_url: uziUrl(12),
        author: "vlad",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "In progress",
        closed: false,
        conflict: false,
        forge_updated_at: minsAgo(130),
        latest_run: latestRun({
          id: "run-closed",
          status: "completed",
          mr_iid: 8,
          mr_state: "closed",
          worker_name: "laptop",
          created_at: minsAgo(300),
          updated_at: minsAgo(120),
        }),
        pipeline: null,
      },
      {
        iid: 15,
        title: "Encrypt per-user Anthropic tokens at rest",
        state: "closed",
        labels: ["PRD"],
        web_url: uziUrl(15),
        author: "vlad",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "",
        closed: true,
        conflict: false,
        forge_updated_at: minsAgo(4200),
        // run-unjudged works this issue, so the card carries its snapshot — every other
        // card whose issue has a run in mockRuns does, and a card claiming no run for an
        // issue that has one is the cross-fixture contradiction this file keeps tripping
        // over. Same shape as iid 18's: a closed issue whose run merged.
        latest_run: latestRun({
          id: "run-unjudged",
          status: "completed",
          mr_iid: 39,
          mr_web_url: "https://gitlab.example.com/vtmocanu/uzi/-/merge_requests/39",
          mr_state: "merged",
          worker_name: "laptop",
          created_at: minsAgo(520),
          updated_at: minsAgo(470),
        }),
        pipeline: null,
      },
      // ── Non-PRD issues (PRD #102 M6) ────────────────────────────────────────
      // The toggle is default-off, so without these the demo build ships a control
      // that visibly does nothing. They are ordinary open issues of the kind any repo
      // has: one carrying a content label, one carrying none at all — the shape a
      // freshly filed issue takes, and the shape whose labels used to marshal as JSON
      // null.
      //
      // PRD #196: a `bug`-only issue. `bug` ships in BOTH default lists, so this card
      // is on the board out of the box (membership = primary ∪ extras) AND runnable
      // (the run-eligible default is PRD,bug) — it offers Start run directly, no
      // Promote and no PRDLESS, with no prds/*.md link (the non-primary waiver). Its
      // `bug` chip is highlighted and hoisted ahead of the cap; `web` rides along
      // plain. The `documentation` card just below is the contrast: visible-only, so
      // it offers Promote instead. This is the §4/§7 headline of the mock.
      {
        iid: 32,
        title: "Board drag drops the card on Safari 17",
        state: "opened",
        labels: ["bug", "web"],
        web_url: uziUrl(32),
        author: "vlad",
        forge_type: "gitlab",
        has_prd_link: false,
        column: "",
        closed: false,
        conflict: false,
        forge_updated_at: minsAgo(30),
        latest_run: null,
        pipeline: null,
      },
      {
        iid: 33,
        title: "Typo in the worker setup docs",
        state: "opened",
        labels: ["documentation"],
        web_url: uziUrl(33),
        author: "mira",
        forge_type: "gitlab",
        has_prd_link: false,
        column: "",
        closed: false,
        conflict: false,
        forge_updated_at: minsAgo(20),
        latest_run: null,
        pipeline: null,
      },
      {
        iid: 34,
        title: "Sidebar scrolls twice on a narrow window",
        state: "opened",
        labels: [],
        web_url: uziUrl(34),
        author: "vlad",
        forge_type: "gitlab",
        has_prd_link: false,
        column: "",
        closed: false,
        conflict: false,
        forge_updated_at: minsAgo(200),
        latest_run: null,
        pipeline: null,
      },
      // Decision 13a: uzi's own tracking issue is cached and is NEVER rendered, with
      // the toggle on or off. It sits here so the exclusion is exercised by the build
      // people click rather than only by a unit test.
      {
        iid: 35,
        title: "uzi self-improvement",
        state: "opened",
        labels: ["uzi-self-improve"],
        web_url: uziUrl(35),
        author: "uzi-bot",
        forge_type: "gitlab",
        has_prd_link: false,
        column: "",
        closed: false,
        conflict: false,
        forge_updated_at: minsAgo(45),
        latest_run: null,
        pipeline: null,
      },
    ],
    pipeline: {
      status: "failed",
      web_url: "https://gitlab.example.com/vtmocanu/uzi/-/pipelines/4242",
      ref: "main",
      pipeline_id: 4242,
      synced_at: minsAgo(1),
    },
  },
  "repo-atlas": {
    repo_id: "repo-atlas",
    path_with_namespace: "vtmocanu/atlas-api",
    forge_type: "gitlab",
    web_url: "https://gitlab.example.com/vtmocanu/atlas-api",
    columns: [
      { label_name: "Ready", position: 0 },
      { label_name: "Doing", position: 1 },
    ],
    cards: [
      {
        iid: 9,
        title: "Rate-limit the public search endpoint",
        state: "opened",
        labels: ["PRD"],
        web_url: atlasUrl(9),
        author: "andrei",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "",
        closed: false,
        conflict: false,
        forge_updated_at: minsAgo(60),
        latest_run: null,
        pipeline: null,
      },
      {
        iid: 8,
        title: "OpenAPI spec drift check in CI",
        state: "opened",
        labels: ["PRD", "Ready"],
        web_url: atlasUrl(8),
        author: "mira",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "Ready",
        closed: false,
        conflict: false,
        forge_updated_at: minsAgo(12),
        latest_run: null,
        pipeline: null,
      },
      {
        iid: 7,
        title: "Postgres connection pool tuning",
        state: "opened",
        labels: ["PRD", "Doing"],
        web_url: atlasUrl(7),
        author: "vlad",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "Doing",
        closed: false,
        conflict: false,
        forge_updated_at: minsAgo(700),
        latest_run: latestRun({
          id: "run-failed",
          status: "failed",
          failure_reason: "run timed out after 2h0m0s (RUN_TIMEOUT)",
          worker_name: "ci-runner-1",
          run_count: 2,
          created_at: daysAgo(1.3),
          updated_at: daysAgo(1.1),
        }),
        pipeline: null,
      },
      {
        iid: 5,
        title: "Healthcheck should ping the DB pool",
        state: "closed",
        labels: ["PRD"],
        web_url: atlasUrl(5),
        author: "vlad",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "",
        closed: true,
        conflict: false,
        forge_updated_at: minsAgo(5400),
        latest_run: latestRun({
          id: "run-cancelled",
          status: "cancelled",
          created_at: daysAgo(3.1),
          updated_at: daysAgo(3),
        }),
        pipeline: null,
      },
    ],
    pipeline: null,
  },
};

// The demo board renders in ASCENDING issue number, which is what the real server
// serves for a board nobody has dragged: `ORDER BY board_position ASC NULLS LAST,
// forge_issue_iid ASC` with every position NULL. The fixtures used to be authored
// DESCENDING, so the one build anyone actually clicks showed `Manual` mode visibly
// not in issue order — contradicting, on screen, the safety argument Decision 7a
// makes for shipping Manual as the default (web-ux S7).
//
// Sorted globally rather than per lane: the client buckets by column while
// preserving relative order, so globally-ascending IS ascending within each column,
// by the same reasoning the SQL comment gives for its board-global positions.
export const mockBoards: Record<string, Board> = Object.fromEntries(
  Object.entries(boardFixtures).map(([id, b]) => [id, { ...b, cards: [...b.cards].sort((x, y) => x.iid - y.iid) }]),
);

// ── Workers ──────────────────────────────────────────────────────────────────

export const mockWorkers: Worker[] = [
  {
    id: "w-laptop",
    name: "laptop",
    status: "online",
    kind: "external",
    hosted_size: null,
    busy: true,
    active_runs: 1,
    max_concurrent_runs: null,
    template_declared: "base",
    template_reported: "base",
    version: "0.4.2",
    upgrade_status: "up_to_date",
    upgrade_detail: null,
    upgrade_target: "0.4.2",
    upgrade_blocking_container: null,
    upgrade_blocking_reason: null,
    upgrade_last_exit_code: null,
    last_heartbeat_at: minsAgo(0.2),
    online_since: minsAgo(192), // online ~3h 12m
    created_at: daysAgo(14),
    // cgroup sample with a limit → CPU bar + "used / limit · %" memory bar (ok tone).
    stats_cpu_pct: 34.2,
    stats_mem_bytes: 2254857830, // 2.1 GiB
    stats_mem_limit_bytes: 4294967296, // 4 GiB → ~52%
    stats_source: "cgroup",
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_bind_mode: "default",
  },
  {
    // Declared jvm at issuance but the running image is base → drift badge demo.
    id: "w-ci",
    name: "ci-runner-1",
    status: "offline",
    kind: "external",
    hosted_size: null,
    busy: false,
    active_runs: 0,
    max_concurrent_runs: null,
    template_declared: "jvm",
    template_reported: "base",
    version: "0.4.1",
    upgrade_status: "outdated",
    upgrade_detail: "running 0.4.1, target 0.4.2",
    upgrade_target: "0.4.2",
    upgrade_blocking_container: null,
    upgrade_blocking_reason: null,
    upgrade_last_exit_code: null,
    last_heartbeat_at: daysAgo(2),
    online_since: null, // offline → no uptime anchor
    created_at: daysAgo(21),
    // Offline → its last-known cgroup sample renders dimmed, never live-looking.
    stats_cpu_pct: 12,
    stats_mem_bytes: 1610612736, // 1.5 GiB
    stats_mem_limit_bytes: 2147483648, // 2 GiB → 75%
    stats_source: "cgroup",
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_bind_mode: "default",
  },
  {
    // PRD #113 M5: the FAILED upgrade. Present so the demo can show the failed-worker
    // strip, the likely-cause copy and the copy-kubectl-command button — a state the
    // product ships and the demo could not previously reach, which meant a browser pass
    // could only ever validate the healthy path.
    //
    // The shape is the v0.11.0 incident: an init container wedged reseeding the nix
    // store. Fictional ids and no registry path, deliberately.
    id: "w-stuck",
    name: "stuck-roller",
    status: "offline",
    kind: "hosted",
    hosted_size: "m",
    docker: false,
    busy: false,
    active_runs: 0,
    max_concurrent_runs: null,
    template_declared: "base",
    template_reported: "base",
    // Still reporting the OLD version: a worker whose new pod never became Ready is
    // offline, so its stored version cannot move. That is the whole reason roll health
    // has to come from the controller rather than from the worker.
    version: "0.4.1",
    upgrade_status: "upgrade_failed",
    upgrade_detail: "seed-nix: CrashLoopBackOff (6 restarts, last exit 2)",
    // Target BELOW the control plane's 0.4.2, so this one worker also renders the Fleet
    // panel's B-1 divergence line. Coherent rather than contrived: the controller is
    // rolling this worker to the PINNED tag 0.4.1 and the pod is wedged getting there.
    // One worker carrying both states is what keeps PRD #58's quota headroom intact —
    // see the note in mockApi.ts.
    upgrade_target: "0.4.1",
    upgrade_blocking_container: "seed-nix",
    upgrade_blocking_reason: "CrashLoopBackOff",
    // The incident's exit code, so the strip's cause line can discriminate a permissions
    // failure from the volume filling up rather than naming both.
    upgrade_last_exit_code: 2,
    last_heartbeat_at: minsAgo(14),
    online_since: null, // offline → no uptime anchor
    created_at: daysAgo(11),
    stats_cpu_pct: null,
    stats_mem_bytes: null,
    stats_mem_limit_bytes: null,
    stats_source: null,
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_bind_mode: "default",
  },
  {
    // Un-quota'd / cgroup-v1 host → process fallback: no known limit (absolute mem,
    // no percentage bar) and the "worker process only" label.
    id: "w-nas",
    name: "nas-runner",
    status: "online",
    kind: "external",
    hosted_size: null,
    busy: false,
    active_runs: 0,
    max_concurrent_runs: null,
    template_declared: "base",
    template_reported: "base",
    version: "0.4.2",
    upgrade_status: "up_to_date",
    upgrade_detail: null,
    upgrade_target: "0.4.2",
    upgrade_blocking_container: null,
    upgrade_blocking_reason: null,
    upgrade_last_exit_code: null,
    last_heartbeat_at: minsAgo(0.4),
    online_since: daysAgo(1), // online ~1d
    created_at: daysAgo(6),
    stats_cpu_pct: 8.3,
    stats_mem_bytes: 503316480, // 480 MiB
    stats_mem_limit_bytes: null, // unlimited/unknown → no bar
    stats_source: "process",
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_bind_mode: "default",
  },
  {
    // A hosted worker (PRD #58): the controller runs this one in the cluster. Seeded
    // ONLINE with a live sample so the hosted + docker badges are seen on a realistic
    // row rather than on a permanently-pending one — and so the demo starts at 1 of
    // its quota of 2, one provision away from the at-quota state. docker:true (PRD #83
    // M3) exercises the docker badge; the other seeded hosted rows leave it undefined.
    id: "w-hosted-eu",
    name: "base.m-1a2b", // derived by the server from template + size (AWS-style base.m-<hex>); the form sends no name
    status: "online",
    kind: "hosted",
    hosted_size: "m",
    docker: true,
    busy: false,
    active_runs: 0,
    max_concurrent_runs: null,
    template_declared: "base",
    template_reported: "base",
    version: "0.4.2",
    upgrade_status: "up_to_date",
    upgrade_detail: null,
    upgrade_target: "0.4.2",
    upgrade_blocking_container: null,
    upgrade_blocking_reason: null,
    upgrade_last_exit_code: null,
    last_heartbeat_at: minsAgo(0.3),
    online_since: minsAgo(27), // online ~27m
    created_at: daysAgo(3),
    stats_cpu_pct: 21.5,
    stats_mem_bytes: 1181116006, // 1.1 GiB
    stats_mem_limit_bytes: 4294967296, // 4 GiB → ~27%
    stats_source: "cgroup",
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_bind_mode: "default",
  },
];

export const mockAdminWorkers: AdminWorker[] = [
  { ...mockWorkers[0], owner_email: mockAdmin.email },
  { ...mockWorkers[1], owner_email: mockAdmin.email },
  { ...mockWorkers[2], owner_email: mockAdmin.email },
  { ...mockWorkers[3], owner_email: mockAdmin.email },
  {
    // A cap-2 worker running both slots → "2/2 runs" badge demo (PRD #42), and a
    // near-limit cgroup sample → danger-tone CPU + memory bars (≥95%).
    id: "w-mira",
    name: "mira-desktop",
    status: "online",
    kind: "external",
    hosted_size: null,
    busy: true,
    active_runs: 2,
    max_concurrent_runs: 2,
    template_declared: "jvm",
    template_reported: "jvm",
    version: "0.4.2",
    upgrade_status: "up_to_date",
    upgrade_detail: null,
    upgrade_target: "0.4.2",
    upgrade_blocking_container: null,
    upgrade_blocking_reason: null,
    upgrade_last_exit_code: null,
    last_heartbeat_at: minsAgo(0.5),
    online_since: daysAgo(2), // online ~2d
    created_at: daysAgo(9),
    stats_cpu_pct: 96.4,
    stats_mem_bytes: 8160437862, // 7.6 GiB
    stats_mem_limit_bytes: 8589934592, // 8 GiB → 95%
    stats_source: "cgroup",
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_bind_mode: "default",
    owner_email: "mira@uzi.local",
  },
];

// ── Tool allowlist + repo tool profiles (PRD #18 M4) ─────────────────────────

export const mockToolAllowlist: ToolAllowlistEntry[] = [
  { id: "tal-kubectl", name: "kubectl", pinned_version: null, note: "For the k8s repos", updated_by: mockAdmin.id, created_at: daysAgo(20), updated_at: daysAgo(20) },
  { id: "tal-opentofu", name: "opentofu", pinned_version: "1.7", note: null, updated_by: mockAdmin.id, created_at: daysAgo(20), updated_at: daysAgo(20) },
  { id: "tal-jq", name: "jq", pinned_version: null, note: null, updated_by: mockAdmin.id, created_at: daysAgo(20), updated_at: daysAgo(20) },
];

// A seed profile so the demo repo shows a couple of selected tools.
export const mockRepoToolProfiles: Record<string, string[]> = {
  "repo-uzi": ["jq", "kubectl"],
};

// ── Agent templates ──────────────────────────────────────────────────────────

// The TRAILING NEWLINE is load-bearing, not formatting (issue #201 M4a F7b).
// Every real builtin `.md` ends with one — agenttmpl's parse->Render round-trip
// pins it byte-for-byte — and without it here the demo's own diff opened with a
// phantom "changed" row: identical text shown once removed and once added,
// because diffLines keeps the newline inside its token. The mock was making the
// diff look broken in a way real data never would.
const builtinBody = (name: string, description: string) =>
  `You are the ${name} agent.\n\n## Role\n\n${description}\n\n## Working agreement\n\n- Stay inside the repository you were given.\n- Report findings tersely; the orchestrator relays them.\n- Never touch \`main\` — all work lands on a branch and goes out as an MR.\n`;

// mockShippedBuiltins is the SHIPPED side of the drift comparison (issue #201
// M4a): what this mock "release" carries under each builtin name, and what Reset
// restores a row to.
//
// IT IS A SEPARATE CONSTANT FROM mockTemplates ON PURPOSE, and that separation is
// the whole fixture design. mockTemplates holds the STORED rows; when the two
// were the same array every row was a pristine clone of its own baseline, so
// nothing could carry the badge on a fresh load and editing the fixture moved
// both sides of the comparison at once. Apart, the mock computes drift honestly
// (mockApi's sameContent) instead of hard-coding a flag, and each stored row
// below can differ in exactly one column.
export const mockShippedBuiltins: BuiltinDefinition[] = [
  {
    name: "coder",
    description: "Implements features, fixes bugs, refactors code. Runs the project's test/lint commands before reporting done.",
    model: null,
    tools: null,
    prompt_body: builtinBody("coder", "Implements features, fixes bugs, refactors code."),
  },
  {
    name: "reviewer",
    description: "Reviews code changes for correctness, style, and edge cases. Reports findings only; never modifies code.",
    model: null,
    tools: ["Bash", "Read", "Grep", "Glob", "WebFetch", "SendMessage", "TaskUpdate", "TaskList", "TaskGet"],
    prompt_body: builtinBody("reviewer", "Reviews code changes for correctness, style, and edge cases."),
  },
  {
    name: "auditor",
    description: "Audits code for security vulnerabilities and unsafe patterns. Reports findings only; never modifies code.",
    model: null,
    tools: ["Bash", "Read", "Grep", "Glob", "WebFetch"],
    prompt_body: builtinBody("auditor", "Audits code for security vulnerabilities and unsafe patterns."),
  },
  {
    name: "tester",
    description: "Validates changes by exercising them against representative real-world inputs and verifying observable behavior.",
    model: null,
    tools: null,
    prompt_body: builtinBody("tester", "Validates changes against representative real-world inputs."),
  },
  {
    name: "documenter",
    description: "Updates documentation only. Never modifies source code.",
    model: null,
    tools: null,
    prompt_body: builtinBody("documenter", "Updates documentation only."),
  },
  {
    name: "fact-checker",
    description: "Adversarially verifies factual claims in docs, specs, and teammate outputs against authoritative sources.",
    model: null,
    tools: ["Bash", "Read", "Grep", "Glob", "WebFetch", "WebSearch"],
    prompt_body: builtinBody("fact-checker", "Adversarially verifies factual claims against authoritative sources."),
  },
];

// storedBuiltin builds the STORED row for a shipped builtin: a byte-for-byte
// clone of the shipped definition, then whatever single-column edit the case
// needs. Starting from the shipped side rather than restating it is what keeps a
// "pristine" row genuinely pristine when the shipped text is later reworded.
const storedBuiltin = (
  id: string,
  name: string,
  opts: Partial<AgentTemplate> = {},
): AgentTemplate => {
  const def = mockShippedBuiltins.find((d) => d.name === name);
  if (!def) throw new Error(`storedBuiltin: no shipped definition named ${name}`);
  return {
    id,
    name,
    description: def.description,
    model: def.model,
    tools: def.tools,
    prompt_body: def.prompt_body,
    is_builtin: true,
    scope: "builtin",
    user_id: null,
    updated_by: null,
    // Recomputed on every mock read; the literal is just the resting value.
    differs_from_builtin: false,
    created_at: daysAgo(40),
    updated_at: daysAgo(40),
    ...opts,
  };
};

// mockTemplates is the STORED side: the rows the mock list endpoint returns.
//
// Each drifted row differs from its shipped twin in exactly ONE column, so the
// badge, the diff and any comparison written against this data are exercised
// per-column rather than in aggregate. mockTemplateDriftCases (below) names each
// case and is asserted by mockApi.templateDrift.test.ts, so a later fixture edit
// that quietly removes one goes red instead of leaving everything green over a
// case that no longer exists.
export const mockTemplates: AgentTemplate[] = [
  // Pristine control: matches its shipped twin exactly, so it must NOT badge.
  storedBuiltin("t-coder", "coder"),
  // tools ORDER only — the case nothing else pins. Same members, swapped.
  storedBuiltin("t-reviewer", "reviewer", {
    tools: ["Read", "Bash", "Grep", "Glob", "WebFetch", "SendMessage", "TaskUpdate", "TaskList", "TaskGet"],
    updated_by: "vlad@uzi.local",
    updated_at: daysAgo(6),
  }),
  // tools MEMBERSHIP: an admin granted the auditor a tool it does not ship with.
  storedBuiltin("t-auditor", "auditor", {
    tools: ["Bash", "Read", "Grep", "Glob", "WebFetch", "WebSearch"],
    updated_by: "vlad@uzi.local",
    updated_at: daysAgo(9),
  }),
  // prompt_body only.
  storedBuiltin("t-tester", "tester", {
    prompt_body: `${builtinBody("tester", "Validates changes against representative real-world inputs.")}\n- Always run the live-DB sweep before reporting a suite green.`,
    updated_by: "vlad@uzi.local",
    updated_at: daysAgo(3),
  }),
  // model only: shipped inherits (null), the stored row pins one.
  storedBuiltin("t-documenter", "documenter", {
    model: "haiku",
    updated_by: "vlad@uzi.local",
    updated_at: daysAgo(14),
  }),
  // description only.
  storedBuiltin("t-fact-checker", "fact-checker", {
    description: "Adversarially verifies factual claims against authoritative sources, and cites every one.",
    updated_by: "vlad@uzi.local",
    updated_at: daysAgo(5),
  }),
  // A builtin row with NO shipped twin: a role a later release dropped. It must
  // report no drift (nothing to compare against) AND must not offer Reset, which
  // would answer 409. Absent from mockShippedBuiltins on purpose.
  {
    id: "t-spec-keeper",
    name: "spec-keeper",
    description: "Keeps specs/ in sync with implementation work. Maintains specs/human.md and specs/ai.md.",
    model: null,
    tools: null,
    prompt_body: builtinBody("spec-keeper", "Keeps specs/ in sync with implementation work."),
    is_builtin: true,
    scope: "builtin",
    user_id: null,
    updated_by: null,
    differs_from_builtin: false,
    created_at: daysAgo(40),
    updated_at: daysAgo(40),
  },
  // A global row: no shipped counterpart, so never badged whatever its content.
  {
    id: "t-release-notes",
    name: "release-notes",
    description: "Drafts release notes from the merged MRs since the last tag.",
    model: "haiku",
    tools: ["Bash", "Read", "Grep", "Glob"],
    prompt_body: builtinBody("release-notes", "Drafts release notes from the merged MRs since the last tag."),
    is_builtin: false,
    scope: "global",
    user_id: null,
    updated_by: "vlad@uzi.local",
    differs_from_builtin: false,
    created_at: daysAgo(11),
    updated_at: daysAgo(2),
  },
  // THE CASE THAT SEPARATES A SCOPE-CHECKING IMPLEMENTATION FROM A NAME-ONLY ONE:
  // a private template whose name collides with a builtin (00048 allows it
  // explicitly). Its content deliberately differs from the shipped `coder`, so a
  // comparison keyed on name alone badges it — and the badge would advertise a
  // Reset that answers 400.
  {
    id: "t-my-coder",
    name: "coder",
    description: "My own coder: same name as the builtin, deliberately different content.",
    model: "sonnet",
    tools: ["Bash", "Read", "Edit"],
    prompt_body: "You are my personal coder. Follow my house style, not the shipped one.\n",
    is_builtin: false,
    scope: "user",
    user_id: mockAdmin.id,
    updated_by: "vlad@uzi.local",
    differs_from_builtin: false,
    created_at: daysAgo(8),
    updated_at: daysAgo(8),
  },
];

// mockTemplateDriftCases names the discriminating fixture cases above by template
// id. It exists so the set can be ASSERTED rather than assumed: a later edit that
// collapses two cases into one, or drops the removed-builtin row, then reds
// instead of silently shrinking coverage while every test stays green.
export const mockTemplateDriftCases: Record<string, string> = {
  "t-coder": "pristine control — must not badge",
  "t-reviewer": "tools order only",
  "t-auditor": "tools membership",
  "t-tester": "prompt_body only",
  "t-documenter": "model only (shipped inherits, stored pins)",
  "t-fact-checker": "description only",
  "t-spec-keeper": "builtin with no shipped twin",
  "t-release-notes": "global scope — no shipped counterpart",
  "t-my-coder": "user scope colliding with a builtin name",
};

// ── Agent skills (PRD #16) ────────────────────────────────────────────────────

// Three scopes, exactly as the real read returns them: a builtin (shipped,
// resettable, never deletable), a global (admin-managed), and one "Mine" skill
// owned by the demo session (admin). The mock reconciler treats the builtin's
// seed body as its reset target.
export const mockSkills: Skill[] = [
  {
    id: "skill-prd-lifecycle",
    name: "prd-lifecycle",
    description:
      "The end-of-run PRD checklist: tick only what the branch's evidence supports, and move the file to prds/done/ when every item is complete.",
    body: [
      "# prd-lifecycle",
      "",
      "At the end of an issue run, scan every unchecked item in the issue's",
      "linked `prds/*.md` file and tick only the ones direct evidence supports.",
      "Move the file to `prds/done/` when (and only when) every item is",
      "complete; a partly-done PRD keeps its ticks and stays where it is.",
      "",
      "## Reviewer half",
      "",
      "Check the PRD diff against what the branch actually changed, and send",
      "back an unsupported completion claim.",
    ].join("\n"),
    scope: "builtin",
    user_id: null,
    updated_by: null,
    created_at: daysAgo(40),
    updated_at: daysAgo(40),
  },
  {
    id: "skill-argo-debug",
    name: "argocd-debugging",
    description:
      "Diagnose a stuck ArgoCD sync: OutOfSync vs Degraded, hook failures, and where to read controller logs.",
    body: "# argocd-debugging\n\nStart from the Application status, then the resource tree…",
    scope: "global",
    user_id: null,
    updated_by: mockAdmin.email,
    created_at: daysAgo(12),
    updated_at: daysAgo(3),
  },
  {
    id: "skill-qdrant-kb",
    name: "qdrant-kb",
    description: "My notes on the team's qdrant knowledge-base schema and the ingest CLI flags.",
    body: "# qdrant-kb\n\nCollections, payload indexes, and the `kb ingest` flags I always forget…",
    scope: "user",
    user_id: mockAdmin.id,
    updated_by: mockAdmin.email,
    created_at: daysAgo(5),
    updated_at: daysAgo(1),
  },
  {
    // Owned by another user (Mira). The admin session sees it in the "Other
    // users" group (view-only — admins can read but not edit others' private
    // skills); signed in as Mira it is her "Mine".
    id: "skill-mira-runbook",
    name: "mira-deploy-runbook",
    description: "Mira's personal runbook for the staging deploy dance.",
    body: "# mira-deploy-runbook\n\nThe order I run the staging promotion steps in…",
    scope: "user",
    user_id: "u-mira",
    updated_by: "mira@uzi.local",
    created_at: daysAgo(4),
    updated_at: daysAgo(2),
  },
];

// Seed allocation: the builtin prd-lifecycle is shared onto the coder template
// so the allocation panel shows a populated union out of the box.
export const mockAllocations: Record<string, { shared: string[]; mine: string[] }> = {
  "t-coder": { shared: ["skill-prd-lifecycle"], mine: [] },
};

// ── Runs ─────────────────────────────────────────────────────────────────────

// PRD #122 milestone demo fixtures. The FROZEN list is the denominator; a running run
// carries some completed + one in-progress so the checklist and the M{done}/{total}
// badge advance in the live sim (engine.ts patches these alongside iteration_count).
// Titles are ordinary text here — the untrusted-render path is exercised by the app's
// stripUnsafeChars, not by hostile fixtures.
export const HEARTBEAT_MILESTONES: Milestone[] = [
  { id: "hb-1", title: "Add /metrics endpoint scaffolding" },
  { id: "hb-2", title: "Expose heartbeat freshness gauge" },
  { id: "hb-3", title: "Wire the collector to the worker registry" },
  { id: "hb-4", title: "Add a staleness alert threshold" },
  { id: "hb-5", title: "Document the metric and add a dashboard panel" },
];
// The candidate list at the plan gate (PRE-APPROVAL), shown only on run-awaiting.
const APPROVAL_NOTIFY_CANDIDATES: Milestone[] = [
  { id: "an-1", title: "Detect a plan parking at the approval gate" },
  { id: "an-2", title: "Render the notification email template" },
  { id: "an-3", title: "Deliver via the existing notifier, respecting mutes" },
];

export const mockRuns: Run[] = [
  {
    // run-queued is parked in the queue, not yet claimed by a worker: it renders
    // the "queued" status badge (PRD #21 SC3 — violet under mission, gray under
    // ember) on issue 26's board card and in the runs list.
    id: "run-queued",
    repo_id: "repo-uzi",
    issue_iid: 26,
    issue_title: "Board card badges for MR pipeline status",
    issue_description: "See prds/12-board-run-lifecycle.md.",
    kind: "issue",
    title: null,
    resume_of_run_id: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    status: "queued",
    requeue_count: 0,
    iteration_count: 0,
    auto_approve: false,
    worker_id: null,
    branch: null,
    model: null,
    override_subagent_model: false,
    forge_type: "gitlab",
    mr_web_url: null,
    mr_iid: null,
    mr_state: null,
    failure_reason: null,
    stop_kind: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    plan_md: null,
    repo_agents: null,
    agent_source: null,
    agent_exclusions: null,
    own_agents: null,
    // Not claimed yet, so no credential was spent — both null (PRD #111 M1).
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_select_reason: null,
    anthropic_headroom_pct: null,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    claimed_at: null,
    started_at: null,
    finished_at: null,
    created_at: minsAgo(1),
    updated_at: minsAgo(1),
  },
  {
    // A repo-less judge meta-run (PRD #239 Decision 4). It is NON-terminal on
    // purpose: it exists so the Runs-badge parity test can prove the count EXCLUDES
    // kind "judge" — a judge run that could not be counted would pin nothing. It is
    // also why the mock listRuns filters kind "judge" (as the real ListRunsForUser
    // does), so this fixture never leaks onto the /runs page it must stay off.
    id: "run-judge-meta",
    repo_id: null,
    issue_iid: null,
    issue_title: "Judge meta-run (excluded from Runs count)",
    issue_description: "A repo-less judge run — off the /runs page and the Runs badge.",
    kind: "judge",
    title: null,
    resume_of_run_id: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    status: "running",
    requeue_count: 0,
    iteration_count: 0,
    auto_approve: false,
    worker_id: "w-laptop",
    branch: null,
    model: null,
    override_subagent_model: false,
    forge_type: "gitlab",
    mr_web_url: null,
    mr_iid: null,
    mr_state: null,
    failure_reason: null,
    stop_kind: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    plan_md: null,
    repo_agents: null,
    agent_source: null,
    agent_exclusions: null,
    own_agents: null,
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_select_reason: null,
    anthropic_headroom_pct: null,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    claimed_at: minsAgo(4),
    started_at: minsAgo(4),
    finished_at: null,
    created_at: minsAgo(5),
    updated_at: minsAgo(1),
  },
  {
    id: LIVE_RUN_ID,
    repo_id: "repo-uzi",
    issue_iid: 24,
    issue_title: "Worker heartbeat metrics endpoint",
    issue_description: "Expose worker heartbeat freshness as a metrics endpoint. See prds/13-worker-metrics.md.",
    kind: "issue",
    title: null,
    resume_of_run_id: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    status: "running",
    requeue_count: 0,
    iteration_count: 0,
    auto_approve: false,
    worker_id: "w-laptop",
    branch: "agent/issue-24",
    model: null,
    override_subagent_model: false,
    forge_type: "gitlab",
    mr_web_url: null,
    mr_iid: null,
    mr_state: null,
    failure_reason: null,
    stop_kind: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    plan_md: null,
    repo_agents: null,
    agent_source: null,
    agent_exclusions: null,
    own_agents: null,
    // PRD #122: a milestone-structured run mid-flight. The live sim (engine.ts) advances
    // these alongside iteration_count, so the checklist and the M{done}/{total} badge move.
    milestones: HEARTBEAT_MILESTONES,
    milestones_completed: ["hb-1"],
    milestones_in_progress: ["hb-2"],
    milestones_candidate: null,
    budget_max_iterations: 12,
    budget_wall_seconds: 7200,
    anthropic_secret_id: "sec-console",
    anthropic_secret_label: "console-key",
    // M5: the headline case, and D20's own example — `console-key — auto, 62% headroom`.
    anthropic_select_reason: "auto",
    anthropic_headroom_pct: 62,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    claimed_at: minsAgo(1),
    started_at: minsAgo(1),
    finished_at: null,
    created_at: minsAgo(2),
    updated_at: minsAgo(1),
  },
  {
    // run-awaiting is parked at the plan gate (PRD #12): it drives the board's
    // attention strip + loud card and renders the run view's plan-approval panel.
    id: "run-awaiting",
    repo_id: "repo-uzi",
    issue_iid: 21,
    issue_title: "Plan-approval notifications via email",
    issue_description: "Notify a run's owner when their plan is parked awaiting approval. See prds/9-approval-notify.md.",
    kind: "issue",
    title: null,
    resume_of_run_id: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    status: "awaiting_approval",
    requeue_count: 0,
    iteration_count: 0,
    auto_approve: false,
    worker_id: "w-laptop",
    branch: null,
    model: null,
    override_subagent_model: false,
    forge_type: "gitlab",
    mr_web_url: null,
    mr_iid: null,
    mr_state: null,
    failure_reason: null,
    stop_kind: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    plan_md: SAMPLE_PLAN(),
    // PRD #37: this repo shipped .claude/agents/, so the plan gate shows State A
    // (repo agents detected, default source).
    repo_agents: [
      { name: "coder", description: "Implements features, fixes bugs, refactors code." },
      { name: "reviewer", description: "Reviews code changes for correctness and edge cases." },
      { name: "tester", description: "Exercises changes against representative inputs." },
      { name: "auditor", description: "Audits code for security vulnerabilities." },
      { name: "documenter", description: "Updates documentation only." },
      { name: "spec-keeper", description: "Keeps specs/ in sync with implementation." },
      { name: "fact-checker", description: "Adversarially verifies factual claims." },
      { name: "web-ux", description: "Validates web interfaces in a real browser." },
    ],
    agent_source: null,
    agent_exclusions: null,
    own_agents: null,
    // PRD #122: at the plan gate the milestones are NOT frozen yet — the candidate list
    // is what the panel surfaces (PRE-APPROVAL). milestones stays null so the badge and
    // checklist stay hidden until approval freezes the list.
    milestones: null,
    milestones_completed: null,
    milestones_in_progress: null,
    milestones_candidate: APPROVAL_NOTIFY_CANDIDATES,
    budget_max_iterations: 8,
    budget_wall_seconds: 5400,
    anthropic_secret_id: "sec-default",
    anthropic_secret_label: "default",
    // M5: an ordinary default, for contrast with the fallback above.
    anthropic_select_reason: "default",
    anthropic_headroom_pct: null,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    claimed_at: minsAgo(9),
    started_at: minsAgo(9),
    finished_at: null,
    created_at: minsAgo(10),
    updated_at: minsAgo(6),
  },
  {
    // run-seeded (PRD #209 M5): a run created WITH a user-authored plan (plan_source
    // "seeded"). It skips the Phase-1 planning turn and the approval gate, so it NEVER
    // enters awaiting_approval and PlanPanel never renders — SeededPlanPanel is the only
    // surface for its plan_md, which is otherwise unreachable on the run page. It also
    // exercises item 2: agent_source is "repo" from creation while repo_agents is still
    // null (the worker has not reported the clone's .claude/agents/ yet), which is the
    // roster-pending state AgentRosterSummary must show instead of an asserted empty
    // roster. Reachable at /runs/run-seeded.
    id: "run-seeded",
    repo_id: "repo-uzi",
    issue_iid: 34,
    issue_title: "Add a --since filter to `uzi run list`",
    issue_description:
      "Filter the runs list by age. Started from a plan authored locally in Claude Code (PRDLESS + --plan-file).",
    kind: "issue",
    title: null,
    resume_of_run_id: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    status: "running",
    requeue_count: 0,
    iteration_count: 0,
    auto_approve: false,
    worker_id: "w-laptop",
    branch: "agent/issue-34",
    model: null,
    override_subagent_model: false,
    forge_type: "gitlab",
    mr_web_url: null,
    mr_iid: null,
    mr_state: null,
    failure_reason: null,
    stop_kind: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    plan_md: SEEDED_PLAN(),
    // The load-bearing field for M5: the run view keys the seeded-plan surface on this.
    plan_source: "seeded",
    // Repo source chosen at create time, but the clone's roster has not been reported
    // yet — the roster-pending state (PRD #209 M5 item 2).
    repo_agents: null,
    agent_source: "repo",
    agent_exclusions: null,
    own_agents: null,
    anthropic_secret_id: "sec-default",
    anthropic_secret_label: "default",
    anthropic_select_reason: "default",
    anthropic_headroom_pct: null,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    claimed_at: minsAgo(4),
    started_at: minsAgo(4),
    finished_at: null,
    created_at: minsAgo(5),
    updated_at: minsAgo(1),
  },
  {
    // run-unreadable-question is parked on a clarification question whose payload the
    // UI CANNOT render (PRD #88): the `question` message carries no `question_id`.
    //
    // It exists because the empty state was inferred from code and could not be produced
    // by the demo — which is the D-N complaint one surface over: a state nothing can
    // reach is a state nobody has looked at, and it is the state a user meets when a
    // worker is mid-rollout or a payload is truncated. The run cannot be answered (the
    // api rejects an answer that names no question) so the panel must say so rather than
    // rendering nothing until the 24h deadline fails the run.
    id: "run-unreadable-question",
    repo_id: "repo-uzi",
    issue_iid: 33,
    issue_title: "Cache the forge issue list between polls",
    issue_description: "Reduce forge calls on the board poll. See prds/33-issue-cache.md.",
    kind: "issue",
    title: null,
    resume_of_run_id: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    status: "awaiting_input",
    requeue_count: 0,
    iteration_count: 1,
    auto_approve: false,
    worker_id: "w-laptop",
    branch: null,
    model: null,
    override_subagent_model: false,
    forge_type: "gitlab",
    mr_web_url: null,
    mr_iid: null,
    mr_state: null,
    failure_reason: null,
    stop_kind: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    // PRD #35: this run is parked on a QUESTION, not on a usage limit. The two parks
    // are independent, so a clarification park carries no limit state at all.
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    plan_md: null,
    repo_agents: null,
    agent_source: "own",
    agent_exclusions: null,
    own_agents: null,
    // PRD #122: a milestone-structured run parked on a clarification question, so the
    // checklist shows mid-flight progress next to the "needs your answer" park.
    milestones: [
      { id: "cache-1", title: "Add an in-memory issue cache keyed by repo + poll cursor" },
      { id: "cache-2", title: "Invalidate on the board webhook" },
      { id: "cache-3", title: "Expose a cache-hit metric" },
    ],
    milestones_completed: ["cache-1"],
    milestones_in_progress: ["cache-2"],
    milestones_candidate: null,
    budget_max_iterations: 6,
    budget_wall_seconds: null,
    anthropic_secret_id: "sec-default",
    anthropic_secret_label: "default",
    anthropic_select_reason: "default",
    anthropic_headroom_pct: null,
    claimed_at: minsAgo(20),
    started_at: minsAgo(20),
    finished_at: null,
    created_at: minsAgo(21),
    updated_at: minsAgo(14),
  },
  {
    id: "run-done",
    repo_id: "repo-uzi",
    issue_iid: 18,
    issue_title: "Run view: fold tool results under their calls",
    issue_description: "See prds/11-run-view-ux.md.",
    kind: "issue",
    title: null,
    resume_of_run_id: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    status: "completed",
    requeue_count: 0,
    iteration_count: 2,
    auto_approve: true,
    worker_id: "w-laptop",
    branch: "agent/issue-18",
    // PRD #300: a completed run whose schedule froze a per-run model, so the
    // run-detail header model badge is exercisable in mock mode (the rest stay
    // null = inherited the owner's Worker default).
    model: "fable",
    override_subagent_model: true,
    forge_type: "gitlab",
    mr_web_url: "https://gitlab.example.com/vtmocanu/uzi/-/merge_requests/42",
    mr_iid: 42,
    mr_state: "merged",
    failure_reason: null,
    stop_kind: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    plan_md: SAMPLE_PLAN(),
    repo_agents: null,
    agent_source: null,
    agent_exclusions: null,
    own_agents: null,
    // PRD #122: a COMPLETED milestone run — every milestone reported complete, so the
    // checklist reads M4/4 and the badge is a full count.
    milestones: [
      { id: "fold-1", title: "Group tool_result under its originating tool_use" },
      { id: "fold-2", title: "Collapse long results behind a disclosure" },
      { id: "fold-3", title: "Keep the running/live tail expanded" },
      { id: "fold-4", title: "Add a per-phase token line" },
    ],
    milestones_completed: ["fold-1", "fold-2", "fold-3", "fold-4"],
    milestones_in_progress: [],
    milestones_candidate: null,
    budget_max_iterations: 10,
    budget_wall_seconds: 7200,
    anthropic_secret_id: "sec-console",
    anthropic_secret_label: "console-key",
    // M5: D10's best-of-pool. Every pooled token was under the floor and the emptiest was
    // spent anyway — the run worked, and the pool is nearly exhausted.
    anthropic_select_reason: "best_of_pool",
    anthropic_headroom_pct: 8,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    claimed_at: minsAgo(220),
    started_at: minsAgo(219),
    finished_at: minsAgo(184),
    created_at: minsAgo(225),
    updated_at: minsAgo(184),
  },
  {
    // run-report-only: a COMPLETED issue run that intentionally opened NO merge request
    // (issue #279). Its deliverable is report_md — the lead's server-scrubbed findings —
    // not a code change, so branch and mr_iid are null. This is the fixture that exercises
    // the "report only" hero chip, the RunCompletedLine deliverable clause, and the
    // Findings panel. report_md is UNTRUSTED text rendered as escaped plain text, so the
    // fixture leans on ordinary prose; the injection-payload case is a unit test, not a
    // hostile demo fixture.
    id: "run-report-only",
    repo_id: "repo-uzi",
    issue_iid: 44,
    issue_title: "Audit: are any queries missing the tenant scope?",
    issue_description: "Investigate and report; no code change expected.",
    kind: "issue",
    title: null,
    resume_of_run_id: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    report_only: true,
    report_md:
      "Reviewed all 63 queries in queries/*.sql. Every tenant-scoped read filters on " +
      "owner_id or joins through a row that does; no unscoped read reaches a tenant table.\n\n" +
      "Two admin-only reports (AdminListRuns, admin rate-limits) read across tenants by " +
      "design and are gated at the handler. No code change required.",
    status: "completed",
    requeue_count: 0,
    iteration_count: 3,
    auto_approve: false,
    worker_id: "w-laptop",
    branch: null,
    model: null,
    override_subagent_model: false,
    forge_type: "gitlab",
    mr_web_url: null,
    mr_iid: null,
    mr_state: null,
    failure_reason: null,
    stop_kind: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    plan_md: SAMPLE_PLAN(),
    repo_agents: null,
    agent_source: null,
    agent_exclusions: null,
    own_agents: null,
    milestones: null,
    milestones_completed: null,
    milestones_in_progress: null,
    milestones_candidate: null,
    budget_max_iterations: null,
    budget_wall_seconds: null,
    anthropic_secret_id: "sec-console",
    anthropic_secret_label: "console-key",
    anthropic_select_reason: "pinned",
    anthropic_headroom_pct: null,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    claimed_at: minsAgo(70),
    started_at: minsAgo(69),
    finished_at: minsAgo(58),
    created_at: minsAgo(75),
    updated_at: minsAgo(58),
  },
  {
    // run-unjudged: the NEVER-JUDGED fixture (PRD #119 follow-up). Terminal and
    // judge-eligible (kind "issue"), with NO seeded review and NO entry in
    // mockPendingJudges — the only combination that renders the run-review panel's
    // untouched empty state, "This run hasn't been judged yet.", beside an ENABLED
    // Run judge button.
    //
    // 🔴 IT EXISTS BECAUSE #119 CONSUMED THE FIXTURE THAT USED TO REACH IT. run-failed
    // was the one terminal run with no review, and giving it a scheduled auto-judge
    // left run-done (review, settled), run-closed (review + running) and run-failed
    // (no review + scheduled) — three of the panel's four states, with the fourth, the
    // control state the PRD promises is UNCHANGED, unreachable in the browser. The
    // regression is invisible to the suite, which mounts JudgePanel against stubbed
    // responses and never asks whether mock mode can still show the state.
    // Do not give this run a review or a pending judge; that is its whole job.
    //
    // The story is a run whose owner had the judge switched off when it finished: a
    // clean merged issue run, the ordinary shape, so the demo's enabled button sits
    // under a settled panel rather than a failure.
    id: "run-unjudged",
    repo_id: "repo-uzi",
    issue_iid: 15,
    issue_title: "Encrypt per-user Anthropic tokens at rest",
    issue_description: "See prds/15-token-encryption.md.",
    kind: "issue",
    title: null,
    resume_of_run_id: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    status: "completed",
    requeue_count: 0,
    iteration_count: 1,
    auto_approve: false,
    worker_id: "w-laptop",
    branch: "agent/issue-15",
    model: null,
    override_subagent_model: false,
    forge_type: "gitlab",
    mr_web_url: "https://gitlab.example.com/vtmocanu/uzi/-/merge_requests/39",
    mr_iid: 39,
    mr_state: "merged",
    failure_reason: null,
    stop_kind: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    plan_md: SAMPLE_PLAN(),
    repo_agents: null,
    agent_source: null,
    agent_exclusions: null,
    own_agents: null,
    anthropic_secret_id: "sec-default",
    anthropic_secret_label: "default",
    anthropic_select_reason: "default",
    anthropic_headroom_pct: null,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    claimed_at: minsAgo(515),
    started_at: minsAgo(514),
    finished_at: minsAgo(470),
    created_at: minsAgo(520),
    updated_at: minsAgo(470),
  },
  {
    // run-closed: a completed run whose MR was later closed unmerged (PRD #24). It
    // renders the muted, struck-through "!8 closed" chip in the runs list, the
    // dashboard, and on its board card.
    id: "run-closed",
    repo_id: "repo-uzi",
    issue_iid: 12,
    issue_title: "Retry the flaky worker heartbeat probe",
    issue_description: "See prds/12-board-run-lifecycle.md.",
    kind: "issue",
    title: null,
    resume_of_run_id: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    status: "completed",
    requeue_count: 0,
    iteration_count: 1,
    auto_approve: false,
    worker_id: "w-laptop",
    branch: "agent/issue-12",
    model: null,
    override_subagent_model: false,
    forge_type: "gitlab",
    mr_web_url: null,
    mr_iid: 8,
    mr_state: "closed",
    failure_reason: null,
    stop_kind: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    plan_md: SAMPLE_PLAN(),
    repo_agents: null,
    agent_source: null,
    agent_exclusions: null,
    own_agents: null,
    // The DELETED-token shape (PRD #111 M1): the FK nulled the id, the label
    // snapshot survived. This is the case the mock exists to make browsable —
    // the run still names the account it billed after the token is gone.
    anthropic_secret_id: null,
    anthropic_secret_label: "retired-key",
    // M5: a mode on a DELETED credential. The two are independent fields, so the chip
    // has to say both things at once.
    anthropic_select_reason: "pinned",
    anthropic_headroom_pct: null,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    claimed_at: minsAgo(305),
    started_at: minsAgo(304),
    finished_at: minsAgo(120),
    created_at: minsAgo(306),
    updated_at: minsAgo(120),
  },
  {
    id: "run-failed",
    repo_id: "repo-atlas",
    issue_iid: 7,
    issue_title: "Postgres connection pool tuning",
    issue_description: "See prds/3-pool-tuning.md.",
    kind: "issue",
    title: null,
    resume_of_run_id: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    status: "failed",
    requeue_count: 1,
    iteration_count: 4,
    auto_approve: false,
    worker_id: "w-ci",
    branch: "agent/issue-7",
    model: null,
    override_subagent_model: false,
    forge_type: "gitlab",
    mr_web_url: null,
    mr_iid: null,
    mr_state: null,
    failure_reason: "run timed out after 2h0m0s (RUN_TIMEOUT)",
    stop_kind: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    plan_md: null,
    repo_agents: null,
    agent_source: null,
    agent_exclusions: null,
    own_agents: null,
    // PRD #122: a run that FAILED mid-milestone — two done, one still in progress when
    // it timed out, so the checklist shows the incomplete state a failure leaves behind.
    milestones: [
      { id: "pool-1", title: "Add pool size + timeout config" },
      { id: "pool-2", title: "Wire pgxpool with the new limits" },
      { id: "pool-3", title: "Add a saturation metric" },
      { id: "pool-4", title: "Load-test and tune the defaults" },
    ],
    milestones_completed: ["pool-1", "pool-2"],
    milestones_in_progress: ["pool-3"],
    milestones_candidate: null,
    budget_max_iterations: 4,
    budget_wall_seconds: 7200,
    anthropic_secret_id: "sec-default",
    anthropic_secret_label: "default",
    // M5: the judge lane's own mode. Rendered `judge binding` and NOT `pinned`, which
    // would send a user to Settings → Workers for a binding that does not exist.
    anthropic_select_reason: "judge",
    anthropic_headroom_pct: null,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    claimed_at: daysAgo(1.2),
    started_at: daysAgo(1.2),
    finished_at: daysAgo(1.1),
    created_at: daysAgo(1.3),
    updated_at: daysAgo(1.1),
  },
  {
    id: "run-cancelled",
    repo_id: "repo-atlas",
    issue_iid: 5,
    issue_title: "Healthcheck should ping the DB pool",
    issue_description: "See prds/2-health.md.",
    kind: "issue",
    title: null,
    resume_of_run_id: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    status: "cancelled",
    requeue_count: 0,
    iteration_count: 0,
    auto_approve: false,
    worker_id: null,
    branch: null,
    model: null,
    override_subagent_model: false,
    forge_type: "gitlab",
    mr_web_url: null,
    mr_iid: null,
    mr_state: null,
    failure_reason: null,
    stop_kind: "cancelled",
    health: "ok",
    health_reason: null,
    health_since: null,
    plan_md: null,
    repo_agents: null,
    agent_source: null,
    agent_exclusions: null,
    own_agents: null,
    anthropic_secret_id: "sec-default",
    anthropic_secret_label: "default",
    // M5: the FALLBACK state, and the mock's most important credential row. The worker
    // is set to auto and the run spent the default anyway because no pooled token had a
    // fresh reading — the one case where the user's configuration and what actually
    // happened differ, and the only one a browser pass can judge the amber chip against.
    anthropic_select_reason: "pool_stale",
    anthropic_headroom_pct: null,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    claimed_at: null,
    started_at: null,
    finished_at: daysAgo(3),
    created_at: daysAgo(3.1),
    updated_at: daysAgo(3),
  },
  {
    // PRD #35: a run PARKED on an Anthropic usage limit — the only fixture whose
    // status is `limit_wait`, so it is the only one that renders the warn pill, the
    // resume countdown, and the per-run toggle in its parked form.
    //
    // Its two timestamps are deliberately in the FUTURE and deliberately NOT equal.
    // Everything else in this file is `minsAgo`, and a countdown seeded in the past
    // renders "Resuming shortly" — which is a real state, but not the one anybody
    // opens this fixture to look at.
    //
    // 🔴 retry_not_before is EARLIER than limit_resets_at, and that is the whole
    // point of the pair. It is not an offset or a fudge: the stamp is pool-aware, so
    // an owner whose second credential still has headroom is promoted before the
    // dead credential's window rolls over. A fixture with retry_not_before >=
    // limit_resets_at would let a countdown wired to the wrong field look right.
    id: "run-limit-wait",
    repo_id: "repo-uzi",
    issue_iid: 23,
    issue_title: "Stream run logs to the CLI with backpressure",
    issue_description: "See prds/23-cli-log-stream.md.",
    kind: "issue",
    title: null,
    resume_of_run_id: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    status: "limit_wait",
    requeue_count: 0,
    // The park does NOT bump requeue_count (that counts worker deaths); the run has
    // been round twice, which is what makes the "attempt 2" clause render.
    iteration_count: 2,
    auto_approve: false,
    worker_id: "w-laptop",
    branch: "agent/issue-24",
    model: null,
    override_subagent_model: false,
    forge_type: "gitlab",
    mr_web_url: null,
    mr_iid: null,
    mr_state: null,
    failure_reason: null,
    stop_kind: null,
    // 🔴 `ok`, NOT a flag, and this is a fixture assertion rather than a default. The
    // park query CLEARS the health columns on entry precisely because the health
    // detector's allowlist never revisits a parked run, so a flag live at park time
    // would freeze for the whole park. A fixture carrying `stalled` here would
    // reproduce the bug that clearing exists to prevent, and it would look plausible.
    health: "ok",
    health_reason: null,
    health_since: null,
    plan_md: null,
    repo_agents: null,
    agent_source: null,
    agent_exclusions: null,
    own_agents: null,
    anthropic_secret_id: "sec-default",
    anthropic_secret_label: "default",
    anthropic_select_reason: "auto",
    anthropic_headroom_pct: 3,
    wait_on_limit: true,
    limit_resets_at: minsAhead(154),
    retry_not_before: minsAhead(97),
    limit_wait_count: 2,
    rate_limit_type: "five_hour",
    claimed_at: minsAgo(140),
    started_at: minsAgo(139),
    finished_at: null,
    created_at: minsAgo(141),
    updated_at: minsAgo(6),
  },
  {
    // PRD #35, second parked run: the DEGRADED countdown states, which the first
    // fixture cannot reach. Added because the browser validator had to override
    // Date.now() inside the page to see them at all, and a state reachable only by
    // patching the clock is one that regresses silently.
    //
    // Three things differ from run-limit-wait, each unlocking a branch:
    //   * retry_not_before is in the PAST → "Resuming shortly" instead of a countdown.
    //     Real, not a broken fixture: the promotion pass runs on a ticker, so an
    //     expired stamp means "waiting for the next sweep".
    //   * the window is seven_day and days out → exercises formatCountdown's "Nd Nh"
    //     arm and the long-horizon reset text, where the two fixtures' 5-hour and
    //     7-day framing must not be swapped.
    //   * limit_wait_count is 1 → the SUPPRESSED "attempt" clause, the opposite of
    //     the first fixture's "attempt 2".
    //
    // Note retry_not_before is EARLIER than limit_resets_at here too, and by six days
    // rather than an hour — the pool-aware promotion at its most visible, and the case
    // where the "sooner than…" explanation earns its place.
    id: "run-limit-wait-due",
    repo_id: "repo-atlas",
    issue_iid: 9,
    issue_title: "Cache the tenant lookup in the auth middleware",
    issue_description: "See prds/9-tenant-cache.md.",
    kind: "issue",
    title: null,
    resume_of_run_id: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    status: "limit_wait",
    requeue_count: 0,
    iteration_count: 1,
    auto_approve: false,
    worker_id: "w-ci",
    branch: "agent/issue-9",
    model: null,
    override_subagent_model: false,
    forge_type: "gitlab",
    mr_web_url: null,
    mr_iid: null,
    mr_state: null,
    failure_reason: null,
    stop_kind: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    plan_md: null,
    repo_agents: null,
    agent_source: null,
    agent_exclusions: null,
    own_agents: null,
    anthropic_secret_id: "sec-console",
    anthropic_secret_label: "console-key",
    anthropic_select_reason: "auto",
    anthropic_headroom_pct: 1,
    wait_on_limit: true,
    limit_resets_at: minsAhead(6 * 24 * 60),
    retry_not_before: minsAgo(3),
    limit_wait_count: 1,
    rate_limit_type: "seven_day",
    claimed_at: minsAgo(300),
    started_at: minsAgo(299),
    finished_at: null,
    created_at: minsAgo(301),
    updated_at: minsAgo(3),
  },
];

// ── Past-run history (ux-tweaks item 3) ──────────────────────────────────────
// The runs page's past section groups by date (days this week → weeks this month →
// months beyond) and reveals progressively ("Show 50 more"), and seven hand-written
// terminal fixtures cannot exhibit either. mockHistoryRuns generates ~150 terminal
// runs DETERMINISTICALLY — titles, repos, workers, statuses and MR states cycle;
// timestamps walk back from NOW through the current week, the current month's
// weeks, then six prior months — so the demo shows every grouping grain and needs
// three reveals to exhaust, and looks the same on every refresh (a demo that
// reshuffles is worse than no demo). All terminal on purpose: the runs-in-progress
// badge count and the judge fixtures' reachable-state coverage are unaffected.

const HIST_TITLES = [
  "Deduplicate webhook deliveries on retry",
  "Add index for the runs-by-owner query",
  "Fix flaky worker heartbeat test",
  "Surface pipeline status on the MR chip",
  "Rotate forge PAT without downtime",
  "Batch notification inserts per fire",
  "Add CSV export to the usage table",
  "Harden the SSRF allowlist parser",
  "Retry claim on serialization failure",
  "Trim stale branches after merge",
  "Cache repo metadata for the board",
  "Fix timezone drift in schedule fires",
  "Add per-model cost breakdown",
  "Migrate secrets to envelope v2",
  "Reduce bundle size of the run view",
  "Add keyboard shortcuts to the board",
  "Fix orphaned tool_result pairing",
  "Speed up the seed sync fan-out",
  "Add health probe to the controller",
  "Label sync races the poller — fix",
  "Document the vault unlock flow",
  "Add rate-limit forecast to dashboard",
  "Fix double-count in phase folding",
  "Prune completed judge meta-runs",
] as const;
const HIST_REPOS = ["repo-uzi", "repo-atlas", "repo-payments"] as const;
const HIST_WORKERS = ["w-laptop", "w-ci", "w-nas", "w-hosted-eu", "w-mira"] as const;

function histRun(i: number, minsBack: number): Run {
  // Mostly completed, a steady trickle of failed and (deliberately) cancelled rows so
  // the calm "stopped" pill and the danger "failed" pill both appear down the history.
  const status = i % 9 === 4 ? "failed" : i % 13 === 7 ? "cancelled" : "completed";
  const cancelled = status === "cancelled";
  const ranMins = 23 + (i % 6) * 17; // 23–108 minutes of work
  const merged = status === "completed" && i % 4 !== 3;
  const hasMr = status === "completed" || i % 3 === 0;
  return {
    id: `run-hist-${i}`,
    repo_id: HIST_REPOS[i % HIST_REPOS.length],
    issue_iid: 400 + i,
    issue_title: HIST_TITLES[i % HIST_TITLES.length],
    issue_description: "Generated demo history — see mockHistoryRuns in mocks/data.ts.",
    kind: "issue",
    title: null,
    resume_of_run_id: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    status,
    requeue_count: 0,
    iteration_count: 1 + (i % 4),
    auto_approve: i % 5 === 0,
    worker_id: HIST_WORKERS[i % HIST_WORKERS.length],
    branch: `agent/issue-${400 + i}`,
    model: null,
    override_subagent_model: false,
    forge_type: "gitlab",
    mr_web_url: hasMr ? `https://gitlab.example.com/demo/-/merge_requests/${900 + i}` : null,
    mr_iid: hasMr ? 900 + i : null,
    mr_state: hasMr ? (merged ? "merged" : status === "completed" ? "opened" : "closed") : null,
    failure_reason: status === "failed" ? "gate red: vitest — 2 failed" : null,
    stop_kind: cancelled ? "cancelled" : null,
    health: "ok",
    health_reason: null,
    health_since: null,
    plan_md: null,
    repo_agents: null,
    agent_source: null,
    agent_exclusions: null,
    own_agents: null,
    anthropic_secret_id: "sec-console",
    anthropic_secret_label: "console-key",
    anthropic_select_reason: "auto",
    anthropic_headroom_pct: null,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    claimed_at: minsAgo(minsBack + ranMins),
    started_at: minsAgo(minsBack + ranMins),
    finished_at: minsAgo(minsBack),
    created_at: minsAgo(minsBack + ranMins + 8),
    updated_at: minsAgo(minsBack),
  };
}

// Minutes-back offsets, walked oldest-last: a run every ~9h across the last 7 days
// (today, yesterday and the current week's weekday buckets), every ~26h out to day
// 28 (the current month's week buckets), then every ~40h across six prior months
// (the month buckets). The small modular jitter keeps timestamps off round hours.
const HIST_OFFSETS_MIN: number[] = (() => {
  const out: number[] = [];
  for (let h = 3; h < 7 * 24; h += 9) out.push(h * 60 + (h % 5) * 7);
  for (let d = 7; d < 28; d++) out.push(d * 1440 + (d % 7) * 300);
  for (let h = 28 * 24; h < 210 * 24; h += 40) out.push(h * 60 + (h % 11) * 13);
  return out;
})();

export const mockHistoryRuns: Run[] = HIST_OFFSETS_MIN.map((m, i) => histRun(i, m));

// ── Other users' active runs (ux-tweaks amendment 2026-08-14 (2)) ─────────────
// The factory card shows OTHER users' runs only — the admin's own runs live in the
// Active section above it — so the demo needs runs the session admin does NOT own,
// or the filtered card would render permanently empty (before this, adminListRuns
// stamped every run with the session email: the demo factory list was 100% "mine",
// which is exactly the duplication the amendment is about). The mock Run has no
// owner column; mockOtherRunOwners IS that column: these runs are seeded into
// state.runs (so an admin can open them), excluded from the caller-scoped
// listRuns / runsInProgressCount, and attributed by adminListRuns from this map.
export const mockOtherRunOwners: Record<string, string> = {
  "run-mira-embed": "mira@uzi.local",
  "run-andrei-queued": "andrei@uzi.local",
};

export const mockOtherUserRuns: Run[] = [
  {
    id: "run-mira-embed",
    repo_id: "repo-atlas",
    issue_iid: 61,
    issue_title: "Embed the changelog in the release email",
    issue_description: "Another user's in-flight run — factory-card demo fixture.",
    kind: "issue",
    title: null,
    resume_of_run_id: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    status: "running",
    requeue_count: 0,
    iteration_count: 2,
    auto_approve: false,
    worker_id: "w-mira",
    branch: "agent/issue-61",
    model: null,
    override_subagent_model: false,
    forge_type: "gitlab",
    mr_web_url: null,
    mr_iid: null,
    mr_state: null,
    failure_reason: null,
    stop_kind: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    plan_md: null,
    repo_agents: null,
    agent_source: null,
    agent_exclusions: null,
    own_agents: null,
    anthropic_secret_id: "sec-mira",
    anthropic_secret_label: "mira-key",
    anthropic_select_reason: "default",
    anthropic_headroom_pct: null,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    claimed_at: minsAgo(18),
    started_at: minsAgo(18),
    finished_at: null,
    created_at: minsAgo(20),
    updated_at: minsAgo(2),
  },
  {
    // Queued and NOT the viewer's own: the factory card must render this as a plain
    // "queued" pill — another owner's vault state is unknown to the viewing admin
    // (PRD #32), and with own rows filtered out the waiting-for-unlock badge has no
    // business on this list at all.
    id: "run-andrei-queued",
    repo_id: "repo-uzi",
    issue_iid: 77,
    issue_title: "Add retry budget to the seed sync",
    issue_description: "Another user's queued run — factory-card demo fixture.",
    kind: "issue",
    title: null,
    resume_of_run_id: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    status: "queued",
    requeue_count: 0,
    iteration_count: 0,
    auto_approve: false,
    worker_id: null,
    branch: null,
    model: null,
    override_subagent_model: false,
    forge_type: "gitlab",
    mr_web_url: null,
    mr_iid: null,
    mr_state: null,
    failure_reason: null,
    stop_kind: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    plan_md: null,
    repo_agents: null,
    agent_source: null,
    agent_exclusions: null,
    own_agents: null,
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_select_reason: null,
    anthropic_headroom_pct: null,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    claimed_at: null,
    started_at: null,
    finished_at: null,
    created_at: minsAgo(7),
    updated_at: minsAgo(7),
  },
];

export function SAMPLE_PLAN(): string {
  return [
    "## Plan",
    "",
    "1. **Pair tool results to calls by id** in `web/src/components/RunEvent.tsx` — never by adjacency, so parallel calls pair correctly.",
    "2. **Fold each result under its call** with an auto-expanding error state.",
    "3. **Cap the DOM** on very long runs behind a \"show earlier\" expander.",
    "4. **Tests**: pairing (parallel calls), orphan results, cap interaction with folding.",
    "",
    "No schema or API changes. Touches `web/` only.",
  ].join("\n");
}

// SEEDED_PLAN is a user-authored plan for the PRD #209 seeded-run demo. It is written
// to be SELF-SUFFICIENT (D7): it names files and steps explicitly and assumes no prior
// conversation, because a seeded run starts cold with plan_md as its only instructions —
// unlike SAMPLE_PLAN above, which is the worker's own Phase-1 output.
export function SEEDED_PLAN(): string {
  return [
    "## Add a `--since` filter to `uzi run list`",
    "",
    "Goal: let `uzi run list` take `--since <duration>` (e.g. `--since 24h`) and show only runs created within that window.",
    "",
    "1. **CLI flag** — in `api/cmd/uzi/run.go`, add a `--since` string flag to the `run list` command; parse it with `time.ParseDuration` and reject an invalid value with a usage error.",
    "2. **Client** — thread the parsed cutoff to `ListRuns` as an optional `since time.Time`, encoded onto the query string.",
    "3. **Filter server-side** — in the runs list handler, when `since` is present add `created_at >= $since` to the query rather than filtering in Go.",
    "4. **Tests** — a unit test for the duration parse (valid + invalid), and a handler test asserting an out-of-window run is excluded.",
    "",
    "Roster: the repo's own agents in `.claude/agents/`. No migration. Touches `api/` only.",
  ].join("\n");
}

// runListItem decorates a Run into the list shape the API returns.
// PRD #40 demo: usage for a run that actually ran (terminal or running); a queued /
// awaiting_approval run has none, so its list row shows no tok/cost — exactly the
// pre-feature "never a fake 0" behavior. A running run shows a smaller "so far".
function demoRunUsage(r: Run): RunUsage | null {
  if (r.status === "queued" || r.status === "claimed" || r.status === "awaiting_approval") return null;
  // History rows (ux-tweaks item 3) spread deterministically across 0.3x–2.8x so a
  // 150-run past list doesn't repeat one implausible figure down the page. The
  // hand-written fixtures keep their documented 1.33M/$1.87 exactly as before.
  const histScale = r.id.startsWith("run-hist-") ? 0.3 + (hashCode(r.id) % 100) / 40 : 1;
  const scale = (r.status === "running" ? 0.4 : 1) * histScale;
  const round = (n: number) => Math.round(n * scale);
  return {
    input_tokens: round(114_400),
    cache_read_tokens: round(1_170_000),
    cache_creation_tokens: 0,
    output_tokens: round(48_200),
    cost_usd: Math.round(187 * scale) / 100,
  };
}

// hashCode is a tiny stable string hash — enough to spread demo runs deterministically
// across the judge-badge states without pulling in a dependency.
function hashCode(s: string): number {
  let h = 0;
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) | 0;
  return Math.abs(h);
}

// demoJudge gives the demo run list every judge-badge state (PRD #98 M4): unjudged,
// a clean verdict with no count, and a verdict carrying a to-triage count. Keyed off the
// run id so a given run looks the same on every render — a demo that reshuffles its
// badges on refresh is worse than no demo.
function demoJudge(r: Run): Pick<RunListItem, "judge_verdict" | "judge_todo_count"> {
  // Only finished runs get judged (PRD #46 enqueues at the terminal transition).
  if (r.status !== "completed" && r.status !== "failed") {
    return { judge_verdict: null, judge_todo_count: 0 };
  }
  const bucket = hashCode(r.id) % 4;
  switch (bucket) {
    case 0:
      return { judge_verdict: null, judge_todo_count: 0 }; // judged-less: no badge at all
    case 1:
      return { judge_verdict: "ideal", judge_todo_count: 0 }; // ⚖ ideal
    case 2:
      return { judge_verdict: "ok", judge_todo_count: 0 }; // ⚖ ok
    default:
      return { judge_verdict: "issues", judge_todo_count: 2 }; // ⚖ issues · 2
  }
}

export function runListItem(r: Run, ownerEmail?: string): RunListItem {
  const repo = mockRepos.find((x) => x.id === r.repo_id);
  const worker = mockWorkers.find((w) => w.id === r.worker_id);
  const usage = demoRunUsage(r);
  return {
    ...r,
    repo_path: repo?.path_with_namespace ?? r.repo_id ?? "",
    worker_name: worker?.name ?? null,
    ...demoJudge(r),
    ...(usage ? { usage } : {}),
    ...(ownerEmail ? { owner_email: ownerEmail } : {}),
  };
}

// ── Seeded message history for the completed run ─────────────────────────────

let doneSeq = 0;
const doneAt = (minAgo: number) => minsAgo(minAgo);
const dm = (kind: string, agent: string | null, payload: unknown, minAgo: number): RunMessage => ({
  seq: ++doneSeq,
  kind,
  agent,
  agent_instance: null,
  agent_label: null,
  payload,
  created_at: doneAt(minAgo),
});

export const mockDoneMessages: RunMessage[] = [
  // The init frame is the strip's model — the run's MAIN-THREAD model, i.e. the
  // lead's — so it must match the lead's assistant frames below (opus), or the demo
  // would show a strip model that no agent row accounts for.
  dm("status", null, { event: "init", model: "claude-opus-4-8" }, 219),
  // PRD #93: `model` rides the same assistant frame as `usage` — the coder pinned
  // sonnet while lead/reviewer ran on opus, so the demo run shows a mixed column.
  dm("text", "lead", { text: "Reading the PRD and the current run-view rendering to scope the fold-results work.", model: "claude-opus-4-8", usage: { input_tokens: 38_200, cache_read_input_tokens: 401_500, cache_creation_input_tokens: 2_000, output_tokens: 14_800 } }, 218),
  dm("tool_use", "lead", { id: "tu-1", name: "Read", input: { file_path: "prds/11-run-view-ux.md" } }, 218),
  dm("tool_result", "lead", { tool_use_id: "tu-1", content: "# PRD 11 — Run view UX\n\nFold tool results under their calls…" }, 218),
  dm("tool_use", "lead", { id: "tu-2", name: "Grep", input: { pattern: "tool_result", path: "web/src" } }, 217),
  dm("tool_result", "lead", { tool_use_id: "tu-2", content: "web/src/components/RunEvent.tsx:12\nweb/src/components/ActivityFeed.tsx:44" }, 217),
  dm("plan", "lead", { text: SAMPLE_PLAN() }, 216),
  // PRD #40: the plan turn's own result frame → a distinct "Plan" per-phase row.
  dm("status", null, {
    event: "result",
    subtype: "success",
    duration_ms: 5 * 60_000,
    num_turns: 9,
    total_cost_usd: 0.24,
    usage: { input_tokens: 21_400, cache_read_input_tokens: 188_000, cache_creation_input_tokens: 0, output_tokens: 6_100 },
    modelUsage: {
      "claude-sonnet-5": { inputTokens: 27_100, outputTokens: 11_600, cacheReadInputTokens: 402_000, cacheCreationInputTokens: 0, costUSD: 0.19 },
      "claude-sonnet-4-6": { inputTokens: 7_600, outputTokens: 3_200, cacheReadInputTokens: 118_000, cacheCreationInputTokens: 0, costUSD: 0.05 },
    },
  }, 216),
  dm("status", null, { text: "plan submitted — awaiting approval" }, 216),
  dm("status", null, { text: "plan approved by vlad@uzi.local" }, 205),
  dm("text", "coder", { text: "Implementing the id-based pairing index and the fold-under-call rendering.", model: "claude-sonnet-5", usage: { input_tokens: 51_600, cache_read_input_tokens: 583_900, cache_creation_input_tokens: 0, output_tokens: 24_100 } }, 204),
  dm("tool_use", "coder", { id: "tu-3", name: "Edit", input: { file_path: "web/src/components/RunEvent.tsx" } }, 203),
  dm("tool_result", "coder", { tool_use_id: "tu-3", content: "ok" }, 203),
  dm("tool_use", "coder", { id: "tu-4", name: "Bash", input: { command: "cd web && npx vitest run src/components/RunEvent.test.tsx" } }, 200),
  dm("tool_result", "coder", { tool_use_id: "tu-4", content: "✓ 14 tests passed" }, 199),
  // PRD #116: the real #115 story — the lead tried to spawn the SDK built-in
  // `Explore` subagent, the subagent guard denied it (guardrails.ts), and the run
  // carried on. It must read as a neutral "⊘ blocked" chip, not a red error.
  dm("tool_use", "lead", { id: "tu-5", name: "Agent", input: { subagent_type: "Explore", description: "map the remaining render paths" } }, 198),
  dm("tool_result", "lead", { tool_use_id: "tu-5", content: "denied by guardrail: only the run's assembled subagents may be invoked", is_error: true }, 198),
  dm("text", "lead", { text: "I'll use the researcher subagent instead." }, 197),
  dm("text", "reviewer", { text: "Pairing is by id, orphan results render standalone, and the cap keeps folding correct at the boundary. One nit: memoize the index. Approved after that.", model: "claude-opus-4-8", usage: { input_tokens: 18_900, cache_read_input_tokens: 149_700, cache_creation_input_tokens: 0, output_tokens: 7_600 } }, 195),
  dm("tool_use", "coder", { id: "tu-6", name: "Edit", input: { file_path: "web/src/components/ActivityFeed.tsx" } }, 192),
  dm("tool_result", "coder", { tool_use_id: "tu-6", content: "ok" }, 192),
  dm("tool_use", "coder", { id: "tu-7", name: "Bash", input: { command: "cd web && npm run typecheck && npm test" } }, 190),
  dm("tool_result", "coder", { tool_use_id: "tu-7", content: "typecheck clean\n✓ 61 tests passed" }, 188),
  dm("status", null, { text: "pushing branch agent/issue-18 and opening the MR" }, 185),
  dm("status", null, {
    event: "result",
    subtype: "success",
    duration_ms: 2_100_000,
    num_turns: 38,
    total_cost_usd: 1.87,
    usage: { input_tokens: 114_400, cache_read_input_tokens: 1_170_000, cache_creation_input_tokens: 0, output_tokens: 48_200 },
    modelUsage: {
      "claude-sonnet-5": { inputTokens: 138_000, outputTokens: 92_000, cacheReadInputTokens: 3_100_000, cacheCreationInputTokens: 0, costUSD: 1.19 },
      "claude-opus-4-8": { inputTokens: 46_000, outputTokens: 27_000, cacheReadInputTokens: 640_000, cacheCreationInputTokens: 0, costUSD: 0.68 },
    },
  }, 184),
];

// ── Awaiting-approval history (parked at the plan gate) ───────────────────────

let awaitSeq = 0;
const am = (kind: string, agent: string | null, payload: unknown, minAgo: number): RunMessage => ({
  seq: ++awaitSeq,
  kind,
  agent,
  agent_instance: null,
  agent_label: null,
  payload,
  created_at: minsAgo(minAgo),
});

export const mockAwaitingMessages: RunMessage[] = [
  am("status", null, { event: "init", model: "claude-sonnet-4-6" }, 9),
  am("text", "lead", { text: "Reading the PRD for issue #21 and mapping how run-owner notifications should be delivered." }, 9),
  am("tool_use", "lead", { id: "aw-1", name: "Read", input: { file_path: "prds/9-approval-notify.md" } }, 8),
  am("tool_result", "lead", { tool_use_id: "aw-1", content: "# PRD 9 — Plan-approval notifications\n\nEmail the run owner when a plan parks…" }, 8),
  am("plan", "lead", { text: SAMPLE_PLAN() }, 6),
  am("status", null, { text: "plan submitted — awaiting approval" }, 6),
];

// ── Unreadable-question history (PRD #88) ────────────────────────────────────

let unreadableSeq = 0;
const um = (kind: string, agent: string | null, payload: unknown, minAgo: number): RunMessage => ({
  seq: ++unreadableSeq,
  kind,
  agent,
  agent_instance: null,
  agent_label: null,
  payload,
  created_at: minsAgo(minAgo),
});

// The `question` message here is DELIBERATELY UNRENDERABLE: it carries no `question_id`.
// That is not a hypothetical shape — it is what a surface sees from a truncated payload,
// or from a worker mid-rollout that emits a question the current parse cannot use. The
// api rejects an answer that names no question, so the run genuinely cannot be answered;
// the panel's job is to SAY that instead of leaving the run at "needs your answer" with
// nothing on screen until the deadline fails it.
//
// Keep the field ABSENT rather than empty-string: both are unusable, and absent is the
// shape a producer change actually yields.
export const mockUnreadableQuestionMessages: RunMessage[] = [
  um("status", null, { event: "init", model: "claude-sonnet-4-6" }, 20),
  um("text", "lead", { text: "Mapping the poll path before I propose a caching strategy." }, 19),
  um("question", "lead", { questions: [{ question: "Which cache TTL?", header: "TTL" }] }, 14),
];

// ── Failed-run history (short) ───────────────────────────────────────────────

let failSeq = 0;
const fm = (kind: string, agent: string | null, payload: unknown): RunMessage => ({
  seq: ++failSeq,
  kind,
  agent,
  agent_instance: null,
  agent_label: null,
  payload,
  created_at: daysAgo(1.15),
});

export const mockFailedMessages: RunMessage[] = [
  fm("status", null, { event: "init", model: "claude-opus-4-8" }), // matches the lead's frames below
  fm("text", "lead", { text: "Benchmarking the pool under load before proposing settings.", model: "claude-opus-4-8", usage: { input_tokens: 8_200, cache_read_input_tokens: 42_000, cache_creation_input_tokens: 0, output_tokens: 1_900 } }),
  fm("tool_use", "lead", { id: "f-1", name: "Bash", input: { command: "go test -bench=Pool ./internal/store/..." } }),
  fm("tool_result", "lead", { tool_use_id: "f-1", content: "benchmark hung — no output after 40m", is_error: true }),
  fm("error", null, { text: "run timed out after 2h0m0s (RUN_TIMEOUT)" }),
  // PRD #40 (Decision 4): a failed run still spent tokens — a usage-bearing error
  // result frame, so the run view shows its pre-death usage.
  fm("error", null, {
    event: "result",
    subtype: "error_during_execution",
    errors: ["run timed out after 2h0m0s (RUN_TIMEOUT)"],
    num_turns: 4,
    duration_ms: 2 * 60 * 60_000,
    total_cost_usd: 0.11,
    usage: { input_tokens: 8_200, cache_read_input_tokens: 42_000, cache_creation_input_tokens: 0, output_tokens: 1_900 },
    modelUsage: {
      "claude-sonnet-5": { inputTokens: 10_400, outputTokens: 3_100, cacheReadInputTokens: 96_000, cacheCreationInputTokens: 0, costUSD: 0.09 },
      "claude-sonnet-4-6": { inputTokens: 3_200, outputTokens: 900, cacheReadInputTokens: 27_000, cacheCreationInputTokens: 0, costUSD: 0.02 },
    },
  }),
];

// ── Parked-on-a-usage-limit history (PRD #35) ────────────────────────────────
// The stream behind run-limit-wait. It is the only fixture carrying the two new
// message kinds, so it is what makes their render cases reachable in mock mode —
// and mock mode is how this gets browser-validated at all, since a non-mock
// `vite dev`/`preview` of this repo proxies /api at whatever real stack is running.
//
// It carries BOTH kinds even though one run can only have parked, because they are
// two different rows (warn vs. danger) and the pair in one visible stream is the
// only way to see that the distinction survives. The `limit_hit` here is the earlier
// death of a run that had NOT opted in, replayed in the same log for that reason.

let limitSeq = 0;
const lm = (kind: string, agent: string | null, payload: unknown, minAgo: number): RunMessage => ({
  seq: ++limitSeq,
  kind,
  agent,
  agent_instance: null,
  agent_label: null,
  payload,
  created_at: minsAgo(minAgo),
});

export const mockLimitWaitMessages: RunMessage[] = [
  lm("status", null, { event: "init", model: "claude-opus-4-8" }, 139),
  lm("text", "lead", { text: "Reading the worker heartbeat path to scope the metrics endpoint." }, 138),
  lm("tool_use", "lead", { id: "lw-1", name: "Read", input: { file_path: "api/internal/handler/workers.go" } }, 138),
  lm("tool_result", "lead", { tool_use_id: "lw-1", content: "// Heartbeat records liveness for the claim path…" }, 137),
  // The first park. Two keys only — `rate_limit_type` and an ISO `resets_at` — which
  // is the whole payload the worker emits. There is deliberately no `attempt`: the
  // count is incremented server-side after this message is written, so a
  // worker-supplied one would be a stale N-1. The run row's limit_wait_count is the
  // live value and the run-view strip renders it.
  lm("limit_wait", "worker", { rate_limit_type: "five_hour", resets_at: minsAgo(51) }, 92),
  lm("status", null, { text: "resumed after the usage window reopened" }, 44),
  lm("text", "coder", { text: "Picking the metrics endpoint back up from the plan." }, 43),
  lm("tool_use", "coder", { id: "lw-2", name: "Edit", input: { file_path: "api/internal/handler/metrics.go" } }, 40),
  lm("tool_result", "coder", { tool_use_id: "lw-2", content: "ok" }, 40),
  // The second park — the live one, whose reset matches the run row's limit_resets_at.
  // The two parks differ ONLY in resets_at, which is exactly what distinguishes the
  // rows now that no count rides them: they must not read as duplicates.
  lm("limit_wait", "worker", { rate_limit_type: "five_hour", resets_at: minsAhead(154) }, 6),
  // A limit_hit from an opted-OUT run, replayed here so the danger-toned variant is
  // visible somewhere in mock mode. Its window name is one this build knows; the
  // untrusted-value path is covered by tests rather than by a hostile fixture.
  lm("limit_hit", "worker", { rate_limit_type: "seven_day_opus", resets_at: minsAhead(3_400) }, 5),
  // A park with NO keys at all — the shape when the SDK frame carried neither field.
  // Both are OMITTED rather than null, so this is what "unknown" actually looks like
  // on the wire, and the row still has to say the one thing it is for.
  lm("limit_wait", "worker", {}, 5),
];

// ── Crew-roster demo runs (PRD #95 M2) ───────────────────────────────────────
// These exist to exercise the crew-state ladder + the collapse/Follow UX in mock
// mode: a multi-agent BURST that used to yank the pane (now updates the crew strip
// + one-liners in place), plus health-varied runs so every crew state renders.
// Combined with the existing seeds — run-live (working), run-awaiting (waiting at
// gate), run-done (done), run-queued (empty) — these two cover the remaining
// stalled + waiting_worker + non-active idle/waiting split.

let crewSeq = 0;
const cm = (kind: string, agent: string | null, payload: unknown, minAgo: number): RunMessage => ({
  seq: ++crewSeq,
  kind,
  agent,
  agent_instance: null,
  agent_label: null,
  payload,
  created_at: minsAgo(minAgo),
});

// A three-agent burst: lead handed off long ago (idle), coder spoke recently
// (waiting), reviewer is the newest speaker (the active one). On run-crew's
// `looping` health the active reviewer reads amber `stalled`, never green.
export const mockCrewMessages: RunMessage[] = [
  cm("status", null, { event: "init", model: "claude-opus-4-8" }, 12),
  cm("text", "lead", { text: "Scoping the crew-roster work; handing the implementation to coder." }, 11),
  cm("tool_use", "lead", { id: "cw-1", name: "Read", input: { file_path: "prds/95-activity-pane-v2.md" } }, 11),
  cm("tool_result", "lead", { tool_use_id: "cw-1", content: "# PRD 95 — activity pane v2…" }, 11),
  cm("text", "coder", { text: "Building the crew strip + collapse-by-default accordion." }, 1),
  cm("tool_use", "coder", { id: "cw-2", name: "Edit", input: { file_path: "web/src/components/ActivityFeed.tsx" } }, 1),
  cm("tool_result", "coder", { tool_use_id: "cw-2", content: "ok" }, 1),
  cm("tool_use", "coder", { id: "cw-3", name: "Bash", input: { command: "cd web && npm run typecheck" } }, 0.9),
  cm("tool_result", "coder", { tool_use_id: "cw-3", content: "typecheck clean" }, 0.8),
  cm("tool_use", "coder", { id: "cw-4", name: "Bash", input: { command: "cd web && npx vitest run src/components/ActivityFeed.test.tsx" } }, 0.6),
  cm("tool_result", "coder", { tool_use_id: "cw-4", content: "✓ 29 tests passed" }, 0.5),
  cm("text", "reviewer", { text: "Reviewing the ladder — checking that a long tool call still reads working." }, 0.3),
  cm("tool_use", "reviewer", { id: "cw-5", name: "Grep", input: { pattern: "STALE_MS", path: "web/src" } }, 0.2),
  cm("tool_use", "reviewer", { id: "cw-6", name: "Bash", input: { command: "go test ./..." } }, 0.1),
];

let degradedSeq = 0;
const gm = (kind: string, agent: string | null, payload: unknown, minAgo: number): RunMessage => ({
  seq: ++degradedSeq,
  kind,
  agent,
  agent_instance: null,
  agent_label: null,
  payload,
  created_at: minsAgo(minAgo),
});

// run-degraded: the worker went quiet (health=waiting_worker), so the whole crew
// reads `waiting` regardless of who spoke last.
export const mockDegradedMessages: RunMessage[] = [
  gm("status", null, { event: "init", model: "claude-opus-4-8" }, 8),
  gm("text", "lead", { text: "Working the fix; last heartbeat was a while ago." }, 7),
  gm("tool_use", "lead", { id: "dg-1", name: "Bash", input: { command: "go build ./..." } }, 6),
];

function demoIssueRun(over: Partial<Run> & Pick<Run, "id" | "status" | "health">): Run {
  return {
    repo_id: "repo-uzi",
    issue_iid: 24,
    issue_title: over.issue_title ?? "Crew roster demo run",
    issue_description: "Demo run exercising the PRD #95 crew-state ladder.",
    kind: "issue",
    title: null,
    resume_of_run_id: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    requeue_count: 0,
    iteration_count: 0,
    auto_approve: false,
    worker_id: "w-laptop",
    branch: "agent/issue-24",
    model: null,
    override_subagent_model: false,
    forge_type: "gitlab",
    mr_web_url: null,
    mr_iid: null,
    mr_state: null,
    failure_reason: null,
    stop_kind: null,
    health_reason: null,
    health_since: null,
    plan_md: null,
    repo_agents: null,
    agent_source: null,
    agent_exclusions: null,
    own_agents: null,
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_select_reason: null,
    anthropic_headroom_pct: null,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    claimed_at: minsAgo(12),
    started_at: minsAgo(12),
    finished_at: null,
    created_at: minsAgo(13),
    updated_at: minsAgo(1),
    ...over,
  };
}

// ── PRD #99: per-instance lane demo runs ─────────────────────────────────────
// Every other fixture in this file hardcodes both attribution columns to null, so
// before these existed `VITE_UZI_MOCK=1 npm run dev` showed NONE of PRD #99: every
// run coalesced to legacy role lanes and the feature had no demoable state. That is
// the same blind spot that let both code suites stay green while the lane grouping
// was removable (see the M5 mutation record) — a fixture that cannot express a
// feature cannot demo it OR guard it.
let laneSeq = 0;
const nm = (
  kind: string,
  agent: string | null,
  instance: string | null,
  label: string | null,
  payload: unknown,
  minAgo: number,
): RunMessage => ({
  seq: ++laneSeq,
  kind,
  agent,
  agent_instance: instance,
  agent_label: label,
  payload,
  created_at: minsAgo(minAgo),
});

// run-lanes — the PRD headline. Two `coder` invocations running in parallel, with
// DISTINCT instance ids and their own task labels, interleaved NON-adjacently (a
// reviewer frame sits between them). Non-adjacency is the load-bearing part: two
// contiguous coder frames render correctly even under the pre-#99 consecutive-author
// grouping, so they would not show the fix. The lead's own turns carry neither
// column, so its lane is the role-keyed fallback beside the two instance-keyed ones.
export const mockLaneMessages: RunMessage[] = [
  nm("status", null, null, null, { event: "init", model: "claude-opus-4-8" }, 18),
  nm("text", "lead", null, null, { text: "Dispatching two coders in parallel: API wiring and the web gate." }, 17),
  nm("tool_use", "lead", null, null, { id: "ln-1", name: "Agent", input: { description: "API wiring", subagent_type: "coder" } }, 17),
  nm("tool_use", "lead", null, null, { id: "ln-2", name: "Agent", input: { description: "web gate UX", subagent_type: "coder" } }, 17),
  nm("tool_use", "coder", "toolu_01coderA", "API wiring", { id: "ln-3", name: "Read", input: { file_path: "api/internal/store/queries/runtime.sql" } }, 12),
  nm("tool_result", "coder", "toolu_01coderA", "API wiring", { tool_use_id: "ln-3", content: "-- name: InsertRunMessage :execrows" }, 12),
  nm("tool_use", "reviewer", "toolu_01review", "audit the wire threading", { id: "ln-4", name: "Grep", input: { pattern: "agent_instance", path: "api" } }, 9),
  nm("tool_use", "coder", "toolu_01coderB", "web gate UX", { id: "ln-5", name: "Edit", input: { file_path: "web/src/components/ActivityFeed.tsx" } }, 7),
  nm("tool_result", "coder", "toolu_01coderB", "web gate UX", { tool_use_id: "ln-5", content: "1 change applied" }, 7),
  // The same instance speaking again, several frames later: it folds back into its
  // OWN lane instead of opening a fresh near-empty bar (Problem 1's fix).
  nm("tool_use", "coder", "toolu_01coderA", "API wiring", { id: "ln-6", name: "Bash", input: { command: "cd api && go test ./internal/store/..." } }, 3),
  nm("tool_result", "coder", "toolu_01coderA", "API wiring", { tool_use_id: "ln-6", content: "ok  gitlab.example.com/vtmocanu/uzi/api/internal/store  0.42s" }, 2),
  nm("tool_use", "coder", "toolu_01coderB", "web gate UX", { id: "ln-7", name: "Bash", input: { command: "cd web && npx vitest run src/components/ActivityFeed.test.tsx" } }, 0.3),
];

// run-busy — the conditional role rollup, and specifically the ONE state pairing
// nobody had seen on screen: a doubled role whose worst state is `waiting` while one
// of its own lanes is visibly pulsing `working`. worstStateFor takes the MIN of
// STATE_PRIORITY and `waiting`(1) outranks `working`(2), so the chip reads `waiting`
// and does not pulse while the lane below it does. That is by design (the chip is a
// worst-state summary, not a most-active one) but nothing on screen says so, and the
// no-legend decision removed the obvious place to say it. This fixture exists so the
// pairing is browsable rather than theoretical — see the open question in PRD #99.
//
// Shape: `tester` is doubled — toolu_01testX is the newest speaker (active → working)
// and toolu_01testY spoke 0.4 min ago, inside the 45s recency window (→ waiting).
let busySeq = 0;
const bm = (
  kind: string,
  agent: string | null,
  instance: string | null,
  label: string | null,
  payload: unknown,
  minAgo: number,
): RunMessage => ({
  seq: ++busySeq,
  kind,
  agent,
  agent_instance: instance,
  agent_label: label,
  payload,
  created_at: minsAgo(minAgo),
});

export const mockBusyMessages: RunMessage[] = [
  bm("status", null, null, null, { event: "init", model: "claude-opus-4-8" }, 22),
  bm("text", "lead", null, null, { text: "Six subagents across the migration and the web rebuild." }, 21),
  bm("tool_use", "researcher", "toolu_01resrch", "survey prior art in inspiration/", { id: "b-1", name: "Read", input: { file_path: "inspiration/bottega/README.md" } }, 20),
  // A long label, over the 48-rune layout clamp: it renders truncated with an
  // ellipsis, which is the only on-screen signal that text was dropped.
  bm("tool_use", "coder", "toolu_01coder1", "thread agent_instance and agent_label through the entire message wire, end to end", { id: "b-2", name: "Bash", input: { command: "cd api && go build ./... && go test ./..." } }, 12),
  bm("tool_use", "coder", "toolu_01coder2", "rebuild the activity pane around per-instance lanes", { id: "b-3", name: "Edit", input: { file_path: "web/src/components/ActivityFeed.tsx" } }, 10),
  // A subagent frame with an instance but NO label: its lane titles as the bare role,
  // no `·` suffix and no placeholder (the label-absent degradation).
  bm("tool_use", "auditor", "toolu_01audit1", null, { id: "b-4", name: "Grep", input: { pattern: "CLAUDE_CODE_OAUTH_TOKEN", path: "agent/src" } }, 8),
  bm("tool_use", "documenter", "toolu_01docs01", "docs: run activity page", { id: "b-5", name: "Edit", input: { file_path: "docs/run-activity.md" } }, 7),
  bm("tool_use", "reviewer", "toolu_01review", "review the migration + wire threading", { id: "b-6", name: "Read", input: { file_path: "api/internal/store/migrations/00075_run_message_instance.sql" } }, 6),
  // The doubled role. Y spoke recently (waiting), X is the newest speaker (working) —
  // so the `tester ×2` chip rolls up as `waiting` while X's lane pulses.
  bm("tool_use", "tester", "toolu_01testY", "unit: RunEvent", { id: "b-7", name: "Bash", input: { command: "npx vitest run src/components/RunEvent.test.tsx" } }, 0.4),
  bm("tool_use", "tester", "toolu_01testX", "e2e: approval gate", { id: "b-8", name: "Bash", input: { command: "./e2e/run-e2e.sh" } }, 0.1),
];

// mockSeededMessages backs the PRD #209 seeded-plan demo run (run-seeded). A seeded run
// SKIPS the Phase-1 planning turn, so there is deliberately NO `plan` message here — the
// plan arrives with the claim (SeededPlanPanel renders run.plan_md, not a feed message).
// Kept short and lead-only so the "roster pending" state on the run page stays coherent:
// the worker has not yet reported the clone's .claude/agents/, so no subagent lanes have
// opened.
export const mockSeededMessages: RunMessage[] = [
  {
    seq: 1,
    kind: "status",
    agent: null,
    agent_instance: null,
    agent_label: null,
    payload: { event: "init", model: "claude-opus-4-8" },
    created_at: minsAgo(4),
  },
  {
    seq: 2,
    kind: "text",
    agent: "lead",
    agent_instance: null,
    agent_label: null,
    payload: {
      text: "Implementing the plan you supplied at create time — skipping the planning turn and the approval gate.",
    },
    created_at: minsAgo(3),
  },
];

// ── PRD #237: live-only token counts (run mid-first-turn) ────────────────────
// run-live-tokens exercises the run-page state this issue adds: a running run that has
// emitted per-call assistant `usage` frames but NO result frame yet, so the usage panel
// must show a LIVE / in-flight token section BEFORE any confirmed/billed figure exists.
// Every other running fixture either carries no usage frames or (like run-done) a result
// frame, so none of them could exhibit the live-before-confirmed state on its own.
//
// Two dedup properties the panel relies on are baked in deliberately: the SDK emits
// several frames per API call all repeating ONE usage, so the fixture includes
// byte-identical (agent_instance, usage) pairs — two lead frames (instance null) and two
// coder frames (same non-null instance) — that the panel must COLLAPSE rather than
// double-count. Model attribution is kept internally consistent per agent: lead on opus
// (agent_instance null), coder on sonnet (non-null agent_instance, mirroring
// mockLaneMessages), so the live breakdown spans exactly two model keys.
let liveTokensSeq = 0;
const ltm = (
  kind: string,
  agent: string | null,
  instance: string | null,
  label: string | null,
  payload: unknown,
  minAgo: number,
): RunMessage => ({
  seq: ++liveTokensSeq,
  kind,
  agent,
  agent_instance: instance,
  agent_label: label,
  payload,
  created_at: minsAgo(minAgo),
});

export const mockLiveTokensMessages: RunMessage[] = [
  // Init frame → the strip's main-thread (lead) model; matches the lead's opus frames.
  ltm("status", null, null, null, { event: "init", model: "claude-opus-4-8" }, 6),
  // Lead's first API call (opus). The SDK repeats one usage across several frames per
  // call, so this exact usage appears TWICE back-to-back — the panel must collapse the
  // duplicate (agent_instance=null, usage) to a single contribution.
  ltm("text", "lead", null, null, { text: "Scoping the token-panel work and dispatching a coder for the web side.", model: "claude-opus-4-8", usage: { input_tokens: 12_400, cache_read_input_tokens: 88_000, cache_creation_input_tokens: 1_500, output_tokens: 3_200 } }, 6),
  ltm("text", "lead", null, null, { text: "Scoping the token-panel work and dispatching a coder for the web side.", model: "claude-opus-4-8", usage: { input_tokens: 12_400, cache_read_input_tokens: 88_000, cache_creation_input_tokens: 1_500, output_tokens: 3_200 } }, 6),
  ltm("tool_use", "lead", null, null, { id: "lt-1", name: "Agent", input: { description: "web token panel", subagent_type: "coder" } }, 5),
  // Coder subagent (sonnet), a distinct instance/lane. Its first call's usage ALSO
  // repeats byte-for-byte across two adjacent frames sharing the SAME agent_instance —
  // the second dedup case (a per-instance one, not just the lead's null-instance one).
  ltm("text", "coder", "toolu_01coderLT", "web token panel", { text: "Reading the run-view usage rendering to add the live section.", model: "claude-sonnet-5", usage: { input_tokens: 22_600, cache_read_input_tokens: 143_000, cache_creation_input_tokens: 0, output_tokens: 8_400 } }, 4),
  ltm("text", "coder", "toolu_01coderLT", "web token panel", { text: "Reading the run-view usage rendering to add the live section.", model: "claude-sonnet-5", usage: { input_tokens: 22_600, cache_read_input_tokens: 143_000, cache_creation_input_tokens: 0, output_tokens: 8_400 } }, 4),
  ltm("tool_use", "coder", "toolu_01coderLT", "web token panel", { id: "lt-2", name: "Read", input: { file_path: "web/src/components/RunUsage.tsx" } }, 4),
  // A SECOND, distinct call from each agent (new usage numbers) so the live totals are a
  // real sum across calls, not one repeated figure. Coder still on sonnet, lead still on
  // opus → two model keys in the live breakdown.
  ltm("text", "coder", "toolu_01coderLT", "web token panel", { text: "Adding the in-flight token section above the confirmed figures.", model: "claude-sonnet-5", usage: { input_tokens: 24_900, cache_read_input_tokens: 167_400, cache_creation_input_tokens: 0, output_tokens: 9_700 } }, 2),
  ltm("text", "lead", null, null, { text: "Watching the coder's progress before the reviewer pass.", model: "claude-opus-4-8", usage: { input_tokens: 13_800, cache_read_input_tokens: 96_500, cache_creation_input_tokens: 0, output_tokens: 2_100 } }, 1),
  // Deliberately NO result frame: the run is mid-first-turn, so there are live tokens but
  // nothing confirmed/billed yet — the whole point of this fixture.
];

export const mockLaneRuns: Run[] = [
  demoIssueRun({
    id: "run-lanes",
    issue_iid: 99,
    issue_title: "Two parallel coders (PRD #99 headline case)",
    issue_description: "Demo run: two same-role subagent invocations that must not merge into one lane.",
    branch: "agent/issue-99",
    status: "running",
    health: "ok",
  }),
  demoIssueRun({
    id: "run-busy",
    issue_iid: 99,
    issue_title: "Busy crew: role rollup, doubled tester, clamped label",
    issue_description: "Demo run: enough lanes and doubled roles to trigger the conditional role rollup.",
    branch: "agent/issue-99",
    status: "running",
    health: "ok",
  }),
  // run-stalled — the SAME message stream as run-busy, differing only in run health.
  // `stalled` is not a per-lane property: crewStateFor returns it only for the ACTIVE
  // lane and only when the RUN's health is degraded, so the mockup's headline case
  // ("the one stalled tester is the first thing you see") could not be browsed on any
  // healthy fixture. Reusing run-busy's stream rather than authoring a third one keeps
  // run-busy meaning exactly what its ordering tests say about it — the two runs differ
  // by one field, so the rollup's stalled-first behaviour is visible as a DIFF against
  // the run beside it rather than as a separate story.
  demoIssueRun({
    id: "run-stalled",
    issue_iid: 99,
    // web-ux F23. `open_failed` had no mock run, so the LONGEST string in the
    // vocabulary could only ever be seen as a rendering preview — and F20's overflow
    // was findable exactly because a long string was on screen. This also fixes a
    // second thing: a *running* run carrying NO credential is a shape no real instance
    // produces, since a running run has by definition claimed one.
    anthropic_secret_id: "sec-default",
    anthropic_secret_label: "default",
    anthropic_select_reason: "open_failed",
    anthropic_headroom_pct: null,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    issue_title: "Busy crew, degraded: the stalled role sorts to the front",
    issue_description: "Demo run: same stream as run-busy with a looping health flag, so the active tester lane reads stalled and its role chip leads the rollup.",
    branch: "agent/issue-99",
    status: "running",
    health: "looping",
    health_reason: "no new tool calls in the last few minutes",
    health_since: minsAgo(3),
  }),
  // run-live-tokens (PRD #237) — a running run mid-first-turn: assistant `usage` frames
  // have arrived but NO result frame has, so the usage panel shows a LIVE / in-flight
  // token section with nothing confirmed yet. Streams mockLiveTokensMessages.
  demoIssueRun({
    id: "run-live-tokens",
    issue_iid: 237,
    issue_title: "Live token counts before the first result frame (PRD #237)",
    issue_description: "Demo run: per-call usage frames with no result frame, so tokens are live-only.",
    branch: "agent/issue-237",
    status: "running",
    health: "ok",
  }),
];

export const mockCrewRuns: Run[] = [
  demoIssueRun({
    id: "run-crew",
    issue_title: "Crew roster: looping worker (stalled active speaker)",
    status: "running",
    health: "looping",
    health_reason: "no new tool calls in the last few minutes",
    health_since: minsAgo(3),
  }),
  demoIssueRun({
    id: "run-degraded",
    // The other half of F23: `pool_empty`, the second string with no mock run.
    anthropic_secret_id: "sec-default",
    anthropic_secret_label: "default",
    anthropic_select_reason: "pool_empty",
    anthropic_headroom_pct: null,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    issue_title: "Crew roster: worker went quiet (waiting on a worker)",
    status: "running",
    health: "waiting_worker",
    health_reason: "the worker's heartbeat is stale",
    health_since: minsAgo(2),
  }),
];

// Sample steer queue per run (PRD #95), so M3's SteerQueueCard has demo data across
// every delivery state without needing a live consume: NULL consumed_at → Queued,
// set → Delivered; the run's status decides the gate/terminal copy client-side.
const steerInput = (
  id: number,
  body: string,
  createdMinAgo: number,
  consumedMinAgo: number | null,
): SteerInput => ({
  id,
  body,
  created_at: minsAgo(createdMinAgo),
  consumed_at: consumedMinAgo == null ? null : minsAgo(consumedMinAgo),
});

export const mockRunInputs: Record<string, SteerInput[]> = {
  // Live run: one delivered, one still queued (newest-first).
  [LIVE_RUN_ID]: [
    steerInput(2, "also add a Prometheus histogram for heartbeat age", 1, null),
    steerInput(1, "focus on the metrics endpoint first", 3, 2),
  ],
  // At the gate: a follow-up consumed while parked → "Delivered — applies after approval".
  "run-awaiting": [steerInput(3, "prefer email over Slack for the first cut", 5, 4)],
  // Finished run: a follow-up that was never consumed → "Not delivered — run finished".
  "run-done": [steerInput(4, "one more nit: memoize the tool index", 186, null)],
  "run-crew": [
    steerInput(6, "check the reduced-motion path too", 1, null),
    steerInput(5, "make sure a long tool call still reads working", 6, 5),
  ],
};

// ── Chat conversations (PRD #39 M4) ──────────────────────────────────────────
// Chat rides the run machinery, so a conversation is a run with kind='chat' and
// no repo (repo_id "" stands in for the real backend's nullable repo_id — the
// chat UI never reads it). Their message logs seed the same store maps as issue
// runs so the streaming machinery renders them unchanged.

const CHAT_1 = "chat-uzi-1"; // active, answers a question about uzi's own source
const CHAT_2 = "chat-uzi-2"; // active, carries a pending issue proposal
const CHAT_3 = "chat-uzi-3"; // ended, offers Continue

function chatRun(over: Partial<Run> & { id: string; title: string; status: Run["status"] }): Run {
  const { title, ...rest } = over;
  return {
    repo_id: null, // a chat run has no repo (runDTO repo_id is nullable, PRD #39)
    kind: "chat",
    issue_iid: null,
    issue_title: title, // conversation title fallback (useRunStream reads the run)
    issue_description: "",
    title, // the runDTO's chat title
    resume_of_run_id: null,
    requeue_count: 0,
    iteration_count: 0,
    auto_approve: false,
    worker_id: "w-laptop",
    branch: null,
    model: null,
    override_subagent_model: false,
    forge_type: "gitlab",
    mr_web_url: null,
    mr_iid: null,
    mr_state: null,
    failure_reason: null,
    stop_kind: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    plan_md: null,
    repo_agents: null, // a chat run carries no agent roster (PRD #37 fields)
    agent_source: null,
    agent_exclusions: null,
    own_agents: null,
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_select_reason: null,
    anthropic_headroom_pct: null,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    claimed_at: minsAgo(6),
    started_at: minsAgo(6),
    finished_at: null,
    created_at: minsAgo(7),
    updated_at: minsAgo(1),
    ...rest,
  };
}

export const mockChatRuns: Run[] = [
  chatRun({ id: CHAT_1, title: "How does the plan-approval gate work?", status: "running", updated_at: minsAgo(1) }),
  chatRun({ id: CHAT_2, title: "Add a worker metrics dashboard", status: "running", updated_at: minsAgo(3) }),
  chatRun({
    id: CHAT_3,
    title: "Why did run #57 fail?",
    status: "completed",
    finished_at: minsAgo(40),
    updated_at: minsAgo(40),
  }),
];

// makeChatLog assigns gapless per-conversation seq, like the real run log.
function makeChatLog(entries: [kind: string, agent: string | null, payload: unknown][]): RunMessage[] {
  return entries.map(([kind, agent, payload], i) => ({
    seq: i + 1,
    kind,
    agent,
    agent_instance: null,
    agent_label: null,
    payload,
    created_at: minsAgo(6 - i * 0.5),
  }));
}

// The pending proposal shown in CHAT_2. Its description deliberately contains a
// URL so the demo shows a model-supplied link rendered INERT (never an anchor).
export const mockProposals: IssueProposal[] = [
  {
    id: "prop-metrics-1",
    run_id: CHAT_2,
    repo_path: "vtmocanu/uzi",
    title: "Add a worker metrics dashboard",
    description:
      "Surface per-worker heartbeat freshness, active-run count, and template drift on one page, reading the existing /api/metrics/workers data. Idea sketched at https://example.com/metrics-notes — worth a quick spike first.",
    labels: ["PRD", "enhancement"],
    status: "pending",
    created_at: minsAgo(3),
  },
];

export const mockChatMessages: Record<string, RunMessage[]> = {
  [CHAT_1]: makeChatLog([
    ["user_message", null, { text: "How does the plan-approval gate work?" }],
    ["text", "chat", { text: "Let me check how the gate is wired in uzi's source before I answer." }],
    ["tool_use", "chat", { id: "c1-1", name: "Grep", input: { pattern: "awaiting_approval", path: "/opt/uzi-src/api" } }],
    ["tool_result", "chat", { tool_use_id: "c1-1", content: "api/internal/workersvc/service.go:388\napi/internal/store/queries/runtime.sql:154\napi/internal/poller/sweeper.go:41" }],
    ["text", "chat", {
      text:
        "The gate is a **run state**, not a flag. When the agent submits a plan the run moves to `awaiting_approval` and the worker parks the SDK session. The API refuses to resume it until you approve:\n\n1. The agent emits a `plan` message and the run enters `awaiting_approval` (`api/internal/workersvc/service.go`).\n2. Your **Approve** click posts an `approve_plan` input; only then does the worker resume the session and start implementing.\n3. A **Reject** sends your reason back as the next turn instead.\n\nSo nothing touches a branch before you approve. Want me to show the resume path in `runtime.sql`?",
    }],
  ]),
  [CHAT_2]: makeChatLog([
    ["user_message", null, { text: "Could you file an issue to add a worker metrics dashboard?" }],
    ["text", "chat", { text: "Here is a draft. Review it and click **Create issue** if it looks right — I can't open it myself." }],
    ["proposal", "chat", mockProposals[0]],
  ]),
  [CHAT_3]: makeChatLog([
    ["user_message", null, { text: "Why did run #57 fail?" }],
    ["tool_use", "chat", { id: "c3-1", name: "get_run", input: { run_id: "57" } }],
    ["tool_result", "chat", { tool_use_id: "c3-1", content: "status: failed\nfailure_reason: run timed out after 2h0m0s (RUN_TIMEOUT)" }],
    ["text", "chat", {
      text:
        "Run #57 hit the **2-hour wall-clock** (`RUN_TIMEOUT`) — it did not fail on a code error. The last activity was a benchmark that hung with no output for ~40 minutes, so the sweeper timed the run out. If you re-run it, cap or skip that benchmark first.",
    }],
    ["status", null, { text: "conversation ended (idle timeout)" }],
  ]),
};

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

// ── Agent memory (PRD #90) ────────────────────────────────────────────────
// Seed cross-run learnings across TWO repos so the Settings → Memory tab shows
// its group-by-repo layout, plus a second repo with one entry. Each row carries a
// user_id so the mock filters by session user (the wire Memory type has none — the
// server owner-scopes it — so it lives only on the fixture and mockApi strips it).
// Attributed to the admin persona so logging in as the demo admin shows them.
export const mockMemories: (Memory & { user_id: string })[] = [
  {
    id: "mem-1",
    user_id: mockAdmin.id,
    repo_id: "repo-uzi",
    repo_name: "vtmocanu/uzi",
    title: "Worker image bakes the gcc toolchain since 0.8.3",
    body: "Building the api no longer needs an apt-get for build-essential — the worker chart 0.8.3 bakes gcc/g++/make. Skip the toolchain-install step; it just wastes a couple of minutes.",
    run_id: "e2d7427b",
    created_at: minsAgo(30),
    basis: "observed",
    evidence: "worker chart values.yaml @0.8.3: base image ships gcc/g++/make",
  },
  {
    id: "mem-2",
    user_id: mockAdmin.id,
    repo_id: "repo-uzi",
    repo_name: "vtmocanu/uzi",
    title: "sqlc must be regenerated after touching queries/",
    body: "After editing internal/store/migrations or queries/, run the pinned `sqlc generate` before `go build` — otherwise the generated code and the schema drift and the build fails on a missing method.",
    run_id: "a1f09c34",
    created_at: daysAgo(2),
    basis: "observed",
    evidence: "go build failed on a missing store method until sqlc regen",
  },
  {
    id: "mem-3",
    user_id: mockAdmin.id,
    repo_id: "repo-atlas",
    repo_name: "vtmocanu/atlas-api",
    title: "Integration tests need POSTGRES_DSN pointed at the throwaway db",
    body: "The atlas integration suite reads POSTGRES_DSN; without it the tests silently skip. Point it at the ephemeral compose db, not the dev one, or you'll clobber local fixtures.",
    run_id: "b7734de1",
    created_at: daysAgo(5),
    basis: "inferred",
  },
];

// A seeded PENDING browser-login request so /cli-auth?request=<id> renders the
// consent form in the demo. The code is fixed so the happy path is walkable
// (a real flow prints it in the terminal; a pure-web demo has none). Approving
// requires typing MOCK_CLI_AUTH_CODE below.
export const MOCK_CLI_AUTH_REQUEST_ID = "req-demo";
export const MOCK_CLI_AUTH_CODE = "ABCD-2345"; // canonical "ABCD2345"

export const mockCliAuthRequest: CliAuthRequestMeta & { user_code: string } = {
  client_desc: "uzi CLI on demo-laptop (darwin/arm64)",
  status: "pending",
  expires_at: minsAgo(-5), // ~5 minutes out
  user_code: "ABCD2345",
};

// ── Build info (PRD #175) ───────────────────────────────────────────────────
// THREE fixtures, because these shapes exercise different code and only the first
// is type-enforced. `api: typeof realApi` typechecks mockApi against the real
// client, but every field below except version and founded is OPTIONAL — so a mock
// returning `{version}` alone compiles clean, and the degraded renders have no
// type safety at all.
//
// Each one names the situation that produces it, because "degraded" is not one
// shape and an earlier version of this comment got that wrong: it claimed a local
// `docker compose` stack reports `mockBuildInfoUnstamped`, which was measured
// FALSE against a live unstamped stack. `handler.New()` always sets `startedAt`
// (api/internal/handler/handler.go), and Version emits `uptime_seconds` whenever
// it is non-zero — so a laptop build omits the three LDFLAGS fields and keeps
// uptime. The two-key body is a struct-literal `Handler`, a test construction that
// no server ever serves. The comment had labelled the one shape a laptop never
// produces as "the COMMON case".
//
// The commit is this repo's real, public first commit rather than an invented
// 40-char hex string: a fixture should not carry a high-entropy literal that reads
// like a credential to a secret scanner, and a published SHA cannot be one.

// A stamped release build — what a published image serves. Matches a live stamped
// body key for key.
export const mockBuildInfo: BuildInfo = {
  version: "0.4.2", // matches the worker fleet's target release in this demo
  founded: "2026-07-03",
  built_at: daysAgo(2),
  commit: "366a282d52095312f54b99698b241ac872e20284",
  commits: 2105,
  uptime_seconds: 3 * 86_400 + 4 * 3_600 + 12 * 60, // 3d 4h 12m
  prds_done: 80,
  prds_open: 32,
};

// THE LAPTOP SHAPE, and the one a developer actually hits: `docker-compose.yml`
// builds the api with no ldflags, so version falls back to "dev" and built_at,
// commit and commits are all omitted — but the process is running, so uptime is
// there. Three keys, not two.
export const mockBuildInfoUnstamped: BuildInfo = {
  version: "dev",
  founded: "2026-07-03",
  uptime_seconds: 4 * 60 + 12, // 4m 12s — a stack somebody just brought up
};

// Uptime UNKNOWN as well: a `Handler` built as a struct literal leaves startedAt
// the zero time, and Version omits rather than reporting roughly two millennia.
// Kept as its own fixture because it is a real wire shape the renderer must handle
// and NOT a laptop's — conflating the two is what the note above is about.
export const mockBuildInfoNoUptime: BuildInfo = {
  version: "dev",
  founded: "2026-07-03",
};
