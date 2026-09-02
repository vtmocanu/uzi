import type {
  Board,
  LatestRun,
} from "../../lib/api";
import { daysAgo, minsAgo } from "./time";

// ── Boards ───────────────────────────────────────────────────────────────────

// LIVE_RUN_ID is the seeded run whose message stream is SIMULATED live: the mock
// engine starts a timed script the first time the run view subscribes to it.
// Declared here (above the boards) because a card's latest_run references it.
export const LIVE_RUN_ID = "run-live";

const uziUrl = (iid: number) => `https://gitlab.example.com/myorg/uzi/-/issues/${iid}`;
const atlasUrl = (iid: number) => `https://gitlab.example.com/myorg/atlas-api/-/issues/${iid}`;

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
    stop_reason: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    owner_name: "Robin Diaz",
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
    web_url: "https://gitlab.example.com/myorg/uzi",
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
        labels: ["uzi"],
        web_url: uziUrl(31),
        author: "mira",
        forge_type: "gitlab",
        has_prd_link: false,
        column: "",
        closed: false,
        conflict: false,
        assignee_ids: [],
        forge_updated_at: minsAgo(5),
        latest_run: null,
        pipeline: null,
      },
      {
        iid: 29,
        title: "Retry failed forge column moves with backoff",
        state: "opened",
        labels: ["uzi"],
        web_url: uziUrl(29),
        author: "vlad",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "",
        closed: false,
        conflict: false,
        assignee_ids: [],
        forge_updated_at: minsAgo(90),
        latest_run: null,
        // "canceled" → the neutral tone (also covers skipped / no-CI).
        pipeline: {
          ref: "agent/issue-29",
          status: "canceled",
          web_url: "https://gitlab.example.com/myorg/uzi/-/pipelines/4188",
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
        labels: ["uzi", "Ready", "enhancement"],
        web_url: uziUrl(27),
        author: "vlad",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "Ready",
        closed: false,
        conflict: false,
        assignee_ids: [],
        forge_updated_at: minsAgo(20),
        latest_run: null,
        // "manual" → the attention tone (a human must click play in GitLab).
        pipeline: {
          ref: "agent/issue-27",
          status: "manual",
          web_url: "https://gitlab.example.com/myorg/uzi/-/pipelines/4190",
          pipeline_id: 4190,
          synced_at: minsAgo(4),
        },
      },
      {
        iid: 26,
        title: "Board card badges for MR pipeline status",
        state: "opened",
        labels: ["uzi", "Ready"],
        web_url: uziUrl(26),
        author: "andrei",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "Ready",
        closed: false,
        conflict: false,
        // Freshly queued, not yet claimed by a worker: renders the "queued" badge
        // (violet under the mission theme, gray under ember) on the board card.
        assignee_ids: [],
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
        labels: ["uzi", "In progress"],
        web_url: uziUrl(24),
        author: "vlad",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "In progress",
        closed: false,
        conflict: false,
        assignee_ids: [],
        forge_updated_at: minsAgo(45),
        latest_run: latestRun({
          id: LIVE_RUN_ID,
          status: "running",
          // issue #321: this run is running at iteration 0 with no persisted plan, so the
          // server derives is_planning — the board card shows the indigo "planning" badge.
          is_planning: true,
          worker_name: "laptop",
          created_at: minsAgo(2),
          updated_at: minsAgo(1),
        }),
        // The agent branch's MR pipeline is still running.
        pipeline: {
          status: "running",
          web_url: "https://gitlab.example.com/myorg/uzi/-/pipelines/4239",
          ref: "agent/issue-24",
          pipeline_id: 4239,
          synced_at: minsAgo(1),
        },
      },
      {
        iid: 22,
        title: "Per-run cost budget with hard stop",
        state: "opened",
        labels: ["uzi", "In progress", "Review", "bug"],
        web_url: uziUrl(22),
        author: "mira",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "Review",
        closed: false,
        conflict: true,
        assignee_ids: [],
        forge_updated_at: minsAgo(1500),
        latest_run: null,
        // A red per-card pipeline: the Fix CI affordance (M6) will hang off this.
        pipeline: {
          status: "failed",
          web_url: "https://gitlab.example.com/myorg/uzi/-/pipelines/4201",
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
        labels: ["uzi", "In progress"],
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
        assignee_ids: [],
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
        labels: ["uzi", "Review"],
        web_url: uziUrl(21),
        author: "vlad",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "Review",
        closed: false,
        conflict: false,
        assignee_ids: [],
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
        // PRD #517: the board's only follow-up-parked card. It is what makes runBadge's
        // `awaiting_followup` arm and the board's follow-up attention strip reachable in
        // mock mode. A parked-awaiting-you run, so it sits alongside the awaiting_approval
        // sibling in Review.
        iid: 25,
        title: "Debounce the board poll while a drag is in flight",
        state: "opened",
        labels: ["uzi", "Review"],
        web_url: uziUrl(25),
        author: "mira",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "Review",
        closed: false,
        conflict: false,
        assignee_ids: [],
        forge_updated_at: minsAgo(19),
        latest_run: latestRun({
          id: "run-awaiting-followup",
          status: "awaiting_followup",
          worker_name: "laptop",
          created_at: minsAgo(19),
          updated_at: minsAgo(3),
        }),
        pipeline: null,
      },
      {
        // Issue #754: the board's only pool-empty parked card — what makes runBadge's
        // `pool_wait` arm reachable in mock mode. STATIC like the limit_wait card:
        // there is no reset window, so nothing counts down here or on the run view.
        iid: 28,
        title: "Backfill run credential history for the audit log",
        state: "opened",
        labels: ["uzi", "In progress"],
        web_url: uziUrl(28),
        author: "mira",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "In progress",
        closed: false,
        conflict: false,
        assignee_ids: [],
        forge_updated_at: minsAgo(52),
        latest_run: latestRun({
          id: "run-pool-wait",
          status: "pool_wait",
          worker_name: "ci",
          created_at: minsAgo(52),
          updated_at: minsAgo(9),
        }),
        pipeline: null,
      },
      {
        iid: 18,
        title: "Run view: fold tool results under their calls",
        state: "closed",
        labels: ["uzi"],
        web_url: uziUrl(18),
        author: "vlad",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "",
        closed: true,
        conflict: false,
        assignee_ids: [],
        forge_updated_at: minsAgo(3000),
        latest_run: latestRun({
          id: "run-done",
          status: "completed",
          mr_iid: 42,
          mr_web_url: "https://gitlab.example.com/myorg/uzi/-/merge_requests/42",
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
        labels: ["uzi", "In progress"],
        web_url: uziUrl(12),
        author: "vlad",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "In progress",
        closed: false,
        conflict: false,
        assignee_ids: [],
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
        labels: ["uzi"],
        web_url: uziUrl(15),
        author: "vlad",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "",
        closed: true,
        conflict: false,
        assignee_ids: [],
        forge_updated_at: minsAgo(4200),
        // run-unjudged works this issue, so the card carries its snapshot — every other
        // card whose issue has a run in mockRuns does, and a card claiming no run for an
        // issue that has one is the cross-fixture contradiction this file keeps tripping
        // over. Same shape as iid 18's: a closed issue whose run merged.
        latest_run: latestRun({
          id: "run-unjudged",
          status: "completed",
          mr_iid: 39,
          mr_web_url: "https://gitlab.example.com/myorg/uzi/-/merge_requests/39",
          mr_state: "merged",
          worker_name: "laptop",
          created_at: minsAgo(520),
          updated_at: minsAgo(470),
        }),
        pipeline: null,
      },
      // ── Non-uzi issues (PRD #764, #767) ─────────────────────────────────────
      // The "Show all other issues" toggle is default-off, so without these the demo
      // build ships a control that visibly does nothing. These two (iid 32, 33) are
      // ordinary open issues of the kind any repo has, neither carrying the `uzi` label
      // nor assigned to the bot — so they are hidden until "Show all" is ticked, and each
      // offers Promote (add `uzi`) rather than Start run. iid 32 carries content labels
      // and iid 33 a `documentation` label. iid 34 (just below) is the CONTRAST case
      // added by PRD #767 M5: no label at all, but assigned to the bot, so it IS runnable
      // and hides Promote — the runnable-by-assignment headline of the mock.
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
        assignee_ids: [],
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
        assignee_ids: [],
        forge_updated_at: minsAgo(20),
        latest_run: null,
        pipeline: null,
      },
      // PRD #767 M5: assigned to the uzi-bot (4021) with NO `uzi` label. Assignment alone
      // makes it uzi's to run — it shows the runnable marker, passes the "uzi's" filter, and
      // hides Promote, exactly as a labelled card does. This is the runnable-by-assignment
      // headline of the mock; the two cards above stay non-runnable Promote demos.
      {
        iid: 34,
        title: "Sidebar scrolls twice on a narrow window",
        state: "opened",
        labels: [],
        assignee_ids: [4021],
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
        assignee_ids: [],
        forge_updated_at: minsAgo(45),
        latest_run: null,
        pipeline: null,
      },
    ],
    pipeline: {
      status: "failed",
      web_url: "https://gitlab.example.com/myorg/uzi/-/pipelines/4242",
      ref: "main",
      pipeline_id: 4242,
      synced_at: minsAgo(1),
    },
    // The board's single connection's bot forge user id (PRD #767 M5), matching the
    // ForgeConnection mock (bot_forge_user_id: 4021) so the demo is coherent. Card iid 34
    // is assigned to this id with NO `uzi` label, exercising the runnable-by-assignment path.
    bot_forge_user_id: 4021,
  },
  "repo-atlas": {
    repo_id: "repo-atlas",
    path_with_namespace: "vtmocanu/atlas-api",
    forge_type: "gitlab",
    web_url: "https://gitlab.example.com/myorg/atlas-api",
    columns: [
      { label_name: "Ready", position: 0 },
      { label_name: "Doing", position: 1 },
    ],
    cards: [
      {
        iid: 9,
        title: "Rate-limit the public search endpoint",
        state: "opened",
        labels: ["uzi"],
        web_url: atlasUrl(9),
        author: "andrei",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "",
        closed: false,
        conflict: false,
        assignee_ids: [],
        forge_updated_at: minsAgo(60),
        latest_run: null,
        pipeline: null,
      },
      {
        iid: 8,
        title: "OpenAPI spec drift check in CI",
        state: "opened",
        labels: ["uzi", "Ready"],
        web_url: atlasUrl(8),
        author: "mira",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "Ready",
        closed: false,
        conflict: false,
        assignee_ids: [],
        forge_updated_at: minsAgo(12),
        latest_run: null,
        pipeline: null,
      },
      {
        iid: 7,
        title: "Postgres connection pool tuning",
        state: "opened",
        labels: ["uzi", "Doing"],
        web_url: atlasUrl(7),
        author: "vlad",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "Doing",
        closed: false,
        conflict: false,
        assignee_ids: [],
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
        labels: ["uzi"],
        web_url: atlasUrl(5),
        author: "vlad",
        forge_type: "gitlab",
        has_prd_link: true,
        column: "",
        closed: true,
        conflict: false,
        assignee_ids: [],
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
    // A distinct connection's bot id (PRD #767 M5); no atlas card is bot-assigned, so this
    // only proves the field rides every board, not just repo-uzi.
    bot_forge_user_id: 5107,
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
