import type {
  Disposition,
  PendingJudge,
  RecommendationCategory,
  ReviewStatus,
  ReviewVerdict,
  TriageCounts,
} from "../../lib/api";
import { daysAgo, minsAgo, secsAgo } from "./time";

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
  // means a PERSON set it, "issue_close" means the M6 poller sync did, and "denied_cli"
  // (issue #167) means the system auto-dismissed a recommendation naming a policy-barred
  // credential-bearing CLI. It is a MOCK-side extension of Disposition because the run-page
  // DTO does not carry provenance — only the Judge menu's occurrence does — and without it
  // the mock cannot render an auto-done or an auto-dismissal at all, the two states the
  // "Done via #IID" and "Dismissed · barred CLI" labels exist for.
  dispositions: (Disposition & { set_via?: "issue_close" | "denied_cli" })[];
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
      "See the [run timeline](https://gitlab.example.com/myorg/uzi/-/issues/71) for the full trace.",
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
          "Installing it in the worker image would save the fallback. See the [worker image spec](https://gitlab.example.com/myorg/uzi/-/blob/main/docs/judge.md).",
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
        issue_url: "https://gitlab.example.com/myorg/uzi/-/issues/71",
        filed_at: minsAgo(20),
      },
      {
        category: "add_agent",
        target: "deploy-agent",
        issue_iid: 72,
        issue_url: "https://gitlab.example.com/myorg/uzi/-/issues/72",
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
      {
        id: "rc-4",
        category: "install_worker_tool",
        // Names a credential-bearing CLI uzi's policy permanently bars, so the M-issue-167
        // backstop auto-dismissed it — the auto-dismissal the Judge menu labels
        // "Dismissed · barred CLI", visibly distinct from rev-done's hand-dismissed
        // improve_agent/reviewer. Both grammars are seeded so the difference is demoable.
        target: "glab",
        rationale_md: "The agent needed to open a merge request and had no CLI; installing `glab` on the worker would let it.",
        confidence: "medium",
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
        issue_url: "https://gitlab.example.com/myorg/uzi/-/issues/91",
        filed_at: minsAgo(90),
      },
    ],
    dispositions: [
      // set_by_user_id would be NULL server-side: nobody clicked this.
      { category: "enable_tool", target: "ripgrep", status: "done", reason: "", set_at: minsAgo(30), stale: false, set_via: "issue_close" },
      // Also system-authored: the issue #167 backstop dismisses a barred-CLI recommendation
      // with wont_do provenance denied_cli. set_by_user_id would be NULL — nobody clicked it.
      { category: "install_worker_tool", target: "glab", status: "dismissed", reason: "wont_do", set_at: minsAgo(20), stale: false, set_via: "denied_cli" },
    ],
    // total 4: todo 2 (rc-1, rc-2), done 1 (rc-3 — auto, via the closed #91), dismissed 1
    // (rc-4 — auto, the barred-CLI backstop; wont_do, so it is NOT a false positive). The
    // done rung outranks filed on the shared ladder, so the filed link above does NOT make
    // ripgrep filed.
    triage: { total: 4, todo: 2, filed: 0, done: 1, dismissed: 1, false_positives: 0 },
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
        issue_url: "https://gitlab.example.com/myorg/uzi/-/issues/88",
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
