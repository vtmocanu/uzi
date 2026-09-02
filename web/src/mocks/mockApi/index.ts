// The in-browser mock implementation of the API client. Same surface, same
// response shapes, zero network: every method resolves from the in-memory store
// after a small jittered delay (so loading states render believably). Board
// moves, template CRUD, worker tokens, run inputs — all work locally.

import {
  type CatalogEntry,
  type Schedule,
  type ScheduleInput,
  type SchedulePreviewInput,
} from "../../lib/api";
// ApiError is imported from its own leaf module (not the `../lib/api` barrel) so
// this mock-mode client introduces no runtime import edge back to lib/api.ts —
// the api → mockApi → api cycle behind issue #165.
import { ApiError } from "../../lib/apiError";
import { mockRepos } from "../data";
import { nextRunId, patchRun } from "../store";
import { delay, requireSession } from "./shared";
import { agentSourceApi } from "./agentSource";
import { agentsApi } from "./agents";
import { cliTokensApi } from "./cliTokens";
import { forgeApi, repos } from "./forge";
import { judgeApi } from "./judge";
import { memoryApi } from "./memory";
import { notificationsApi } from "./notifications";
import { findingsApi } from "./findings";
import { runsApi } from "./runs";
import { chatApi } from "./chat";
import { secretsApi } from "./secrets";
import { workersApi } from "./workers";
import { settingsApi } from "./settings";
import { boardsApi } from "./boards";
import { usersApi } from "./users";

// ── Scheduled runs (PRD #241) demo fixtures + helpers ──────────────────────
// schedulePreviewCap mirrors the server's clamp on the preview N (PRD #241 M4).
const schedulePreviewCap = 10;
let scheduleSeq = 700;
const nextScheduleId = () => `sch-${(scheduleSeq++).toString(36)}`;

// The server caps owner guidance at 8 KiB (MaxGuidanceBytes) and 422s an oversize value on
// every write path (validateScheduleConfig, shared by create and update). The Textarea's
// maxLength caps CHARACTERS, not UTF-8 BYTES, so a multibyte value can pass the input yet
// exceed the byte cap — validate bytes on both mock write paths so mock mode reproduces the
// production 422 rather than silently accepting oversized guidance.
const MAX_GUIDANCE_BYTES = 8 * 1024;
function assertGuidanceWithinCap(guidance: string | null | undefined): void {
  if (guidance != null && new TextEncoder().encode(guidance).length > MAX_GUIDANCE_BYTES) {
    throw new ApiError(422, "guidance is too large");
  }
}

// mockScheduleFires computes the next N fire instants (UTC ISO) for a 5-field
// cron string. It handles the canonical preset shapes (specific min/hour, `1-5`,
// single dow, `*/N` steps) — enough for the demo + tests — and returns [] for
// anything it does not understand (a day-of-month/month restriction), which the
// UI renders as an empty preview exactly as a real invalid cron would.
function mockScheduleFires(cron: string, n: number, from = new Date()): string[] {
  const fields = cron.trim().split(/\s+/);
  if (fields.length !== 5) return [];
  const [minF, hrF, domF, monF, dowF] = fields;
  if (domF !== "*" || monF !== "*") return [];
  const expand = (f: string, max: number): number[] => {
    if (f === "*") return Array.from({ length: max + 1 }, (_, i) => i);
    const step = /^\*\/(\d{1,2})$/.exec(f);
    if (step) {
      const s = Number(step[1]);
      const out: number[] = [];
      for (let i = 0; i <= max; i += s) out.push(i);
      return out;
    }
    const range = /^(\d{1,2})-(\d{1,2})$/.exec(f);
    if (range) {
      const out: number[] = [];
      for (let i = Number(range[1]); i <= Number(range[2]); i++) out.push(i);
      return out;
    }
    if (/^\d{1,2}$/.test(f)) return [Number(f)];
    return [];
  };
  const minutes = expand(minF, 59);
  const hours = expand(hrF, 23);
  const dows = dowF === "*" ? null : expand(dowF, 7).map((d) => d % 7);
  if (minutes.length === 0 || hours.length === 0) return [];
  const out: string[] = [];
  const start = new Date(Date.UTC(from.getUTCFullYear(), from.getUTCMonth(), from.getUTCDate()));
  for (let day = 0; day < 400 && out.length < n; day++) {
    const base = new Date(start.getTime() + day * 86_400_000);
    if (dows && !dows.includes(base.getUTCDay())) continue;
    for (const h of hours) {
      for (const mi of minutes) {
        const t = Date.UTC(base.getUTCFullYear(), base.getUTCMonth(), base.getUTCDate(), h, mi);
        if (t > from.getTime() && out.length < n) out.push(new Date(t).toISOString());
      }
    }
  }
  return out.slice(0, n);
}

// scheduleDTO recomputes the live next-fire preview at read time, exactly as the
// server does — the list and the modal preview then agree by construction.
function scheduleDTO(s: Schedule): Schedule {
  let nextFires: string[] = [];
  let nextFireAt: string | null = null;
  if (s.status === "active" && s.enabled) {
    if (s.timing === "recurring") {
      nextFires = mockScheduleFires(s.cron_expr, 3);
      nextFireAt = nextFires[0] ?? null;
    } else if (s.run_at && new Date(s.run_at).getTime() > Date.now()) {
      nextFireAt = s.run_at;
    }
  }
  return { ...s, next_fire_at: nextFireAt, next_fires: nextFires };
}

const daysFromNow = (d: number, h: number, m = 0): string => {
  const t = new Date();
  t.setUTCHours(h, m, 0, 0);
  t.setUTCDate(t.getUTCDate() + d);
  return t.toISOString();
};

// The owner's user-authored schedules (origin='user'). Materialized default rows
// (origin='default') are appended below from the catalog fixture; keeping the two
// lists separate lets the user rows stay free of the three origin fields, which the
// map injects uniformly.
const userSchedules: Omit<
  Schedule,
  "origin" | "catalog_slug" | "customized" | "sibling_group_id" | "baked_guidance"
>[] = [
  {
    id: "sch-7kd2", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi",
    target: "sweep", issue_iid: null, labels: null, prompt: "",
    timing: "recurring", cron_expr: "0 2 * * 1-5", run_at: null,
    timezone: "Europe/Bucharest", next_fire_at: null,
    last_fired_at: daysFromNow(-1, 2), auto_approve: true, wait_on_limit: true,
    max_issues: 1,
    guidance: "Keep the diff small and add a failing test first.",
    model: "fable",
    override_subagent_model: true,
    enabled: true, status: "active", created_at: daysFromNow(-14, 9),
    updated_at: daysFromNow(-1, 2), next_fires: [],
    // Fired on time, started nothing: the one candidate within the cap was skipped
    // for a benign reason, and `capped` says there were older candidates behind it —
    // the amber cell + the cap hint (Goal 2). The whole point of PRD #308.
    last_fire: {
      fired_at: daysFromNow(-1, 2), matched: 1, capped: true,
      started: [],
      skips: [
        {
          issue_iid: 96,
          title: "Mid-run worker restart discards all un-pushed commits on resume",
          reason: "not_eligible",
          web_url: "https://gitlab.example.com/vtmocanu/uzi/-/issues/96",
        },
      ],
    },
  },
  {
    id: "sch-3bf1", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi",
    target: "issue", issue_iid: 142, labels: null, prompt: "",
    timing: "recurring", cron_expr: "0 3 * * *", run_at: null,
    timezone: "Europe/Bucharest", next_fire_at: null,
    last_fired_at: daysFromNow(0, 3), auto_approve: false, wait_on_limit: true,
    max_issues: null,
    guidance: "Prefer the smallest change that closes the issue; no new deps.",
    model: null,
    override_subagent_model: false,
    enabled: true, status: "active", created_at: daysFromNow(-9, 10),
    updated_at: daysFromNow(0, 3), next_fires: [],
    // A healthy fire: it started the run it matched (green "1 started").
    last_fire: {
      fired_at: daysFromNow(0, 3), matched: 1, capped: false,
      started: [
        {
          issue_iid: 142,
          run_id: "3f1a2b7c-9d4e-4a1b-8c6d-1e2f3a4b5c6d",
          title: "RunKind (TypeScript) omits 'chat', which the DB CHECK allows",
          web_url: "https://gitlab.example.com/vtmocanu/uzi/-/issues/142",
        },
      ],
      skips: [],
    },
  },
  {
    id: "sch-9qm4", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi",
    target: "issue", issue_iid: 158, labels: null, prompt: "",
    timing: "once", cron_expr: "", run_at: daysFromNow(1, 9),
    timezone: "Europe/Bucharest", next_fire_at: null,
    last_fired_at: null, auto_approve: true, wait_on_limit: false,
    max_issues: null,
    guidance: null,
    model: null,
    override_subagent_model: false,
    enabled: true, status: "active", created_at: daysFromNow(-1, 20),
    updated_at: daysFromNow(-1, 20), next_fires: [], last_fire: null,
  },
  {
    id: "sch-pr0m", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi",
    target: "prompt", issue_iid: null, labels: null,
    prompt: "hunt for flaky tests and open an MR",
    timing: "recurring", cron_expr: "0 9 * * 1", run_at: null,
    timezone: "Europe/Bucharest", next_fire_at: null,
    last_fired_at: daysFromNow(-7, 9), auto_approve: true, wait_on_limit: false,
    max_issues: null,
    guidance: null,
    model: null,
    override_subagent_model: false,
    enabled: true, status: "active", created_at: daysFromNow(-21, 11),
    updated_at: daysFromNow(-7, 9), next_fires: [], last_fire: null,
  },
  {
    id: "sch-zt88", repo_id: "repo-atlas", repo_path: "vtmocanu/atlas-api",
    target: "sweep", issue_iid: null, labels: ["bug"], prompt: "",
    timing: "recurring", cron_expr: "0 */6 * * *", run_at: null,
    timezone: "UTC", next_fire_at: null,
    last_fired_at: daysFromNow(-3, 18), auto_approve: true, wait_on_limit: false,
    max_issues: 3,
    guidance: null,
    model: null,
    override_subagent_model: false,
    enabled: false, status: "active", created_at: daysFromNow(-30, 8),
    updated_at: daysFromNow(-3, 18), next_fires: [],
    // A healthy sweep: every matched candidate started a run (green "3 started",
    // each pairing issue ↔ run in the expanded panel).
    last_fire: {
      fired_at: daysFromNow(-3, 18), matched: 3, capped: false,
      started: [
        { issue_iid: 124, run_id: "a20b4e51-77c8-4d2a-9f10-2b3c4d5e6f70", title: "web: judge free text renders without Unicode Cf stripping", web_url: "https://gitlab.example.com/vtmocanu/atlas-api/-/issues/124" },
        { issue_iid: 139, run_id: "c7d5f0a2-1e34-4b56-88a9-0c1d2e3f4a5b", title: "Poller sync timeouts against forge-fake in the e2e stack" },
        { issue_iid: 151, run_id: "e91f6b03-42d7-4c88-b1a2-3c4d5e6f7a80", title: "Board card CI badge flickers on refetch" },
      ],
      skips: [],
    },
  },
  {
    // A parked schedule (status='error'): the last fire failed and the scheduler
    // stopped advancing it, so the list shows the red "parked" badge and an "error"
    // Next-run pill. Demoing this state is the whole reason it's a seed row.
    id: "sch-er0r", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi",
    target: "issue", issue_iid: 173, labels: null, prompt: "",
    timing: "recurring", cron_expr: "30 1 * * *", run_at: null,
    timezone: "Europe/Bucharest", next_fire_at: null,
    last_fired_at: daysFromNow(-1, 1, 30), auto_approve: true, wait_on_limit: false,
    max_issues: null,
    guidance: null,
    model: null,
    override_subagent_model: false,
    enabled: true, status: "error", created_at: daysFromNow(-12, 15),
    updated_at: daysFromNow(-1, 1, 30), next_fires: [], last_fire: null,
  },
];

// The builtin default-jobs catalog (PRD #589), mirroring
// api/internal/schedtmpl/catalog/*.md. A prompt entry carries the file body as
// `prompt` (labels/guidance empty, max_issues 0); a sweep entry carries its selector
// `labels` plus the body as `guidance` (prompt empty). auto_approve/wait_on_limit are
// the fixed run flags every default is seeded with (schedtmpl.AutoApprove/WaitOnLimit).
const scheduleCatalog: CatalogEntry[] = [
  {
    slug: "test-improvement",
    name: "Weekly test improvement",
    description: "Weekly pass that finds one under-tested area and strengthens its tests.",
    target: "prompt", cron: "0 8 * * 1", timezone: "UTC", model: "",
    prompt:
      "Spend this run improving the project's automated tests. Pick ONE area that is meaningfully under-tested — a module with thin coverage, an important branch with no assertion, or a bug-prone path — and add focused, genuinely useful tests for it. Prefer a small number of high-value tests over many shallow ones, and run the project's test suite to confirm your additions pass.\n\nMake every test earn its place: prove each assertion is non-vacuous by identifying a plausible defect that would make it fail (if you sanity-check by mutating the code under test, do it in a throwaway copy and never a production file, and change it to a value another case already produces, not a fresh sentinel); assert the observable end-state, not an intermediate call; prefer positive assertions over negative ones; never weaken or delete an existing assertion to make a suite pass; and do not re-touch a test another recent run just changed — pick a different area so parallel runs do not collide.\n\nGuardrail: change TEST files only — no production (non-test) file; a behavior needing a production change to be testable is out of scope (pick another area), and a real production bug found while testing is reported, not fixed. Commit your new tests and open a merge request; if nothing worthwhile this week an empty week is acceptable — do not invent low-value tests to hit a number — open no MR and leave a note on what you looked at.",
    labels: [], guidance: "", max_issues: 0, auto_approve: true, wait_on_limit: true,
  },
  {
    slug: "docs-hygiene",
    name: "Docs hygiene",
    description: "Weekly sweep for mechanical documentation defects — dead links, stale references, drift.",
    target: "prompt", cron: "0 3 * * 1", timezone: "UTC", model: "",
    prompt:
      "Audit the project's documentation for mechanical defects: broken or moved links, references to files or commands that no longer exist, stale frontmatter, and obvious typos. Focus on correctness, not rewriting for style. Verify each problem against the actual repository before fixing it; when a broken link has more than one plausible target, describe it rather than guessing the repoint.\n\nApply the mechanical corrections and open a merge request. Guardrail: mechanical fixes only (links, stale refs, frontmatter, typos), documentation files only — no prose rewrites and no source/build/CI/agent-config edits, keep the diff to mechanical corrections. If there is nothing to fix, open no MR and leave a note.",
    labels: [], guidance: "", max_issues: 0, auto_approve: true, wait_on_limit: true,
  },
  {
    slug: "bug-hunt",
    name: "Bug hunt — deep audit",
    description: "Deep audit of one subsystem for correctness bugs, confirmed by a reviewer and an auditor.",
    target: "prompt", cron: "0 4 * * 3", timezone: "UTC", model: "",
    prompt:
      "Pick ONE subsystem and audit it deeply for real correctness bugs: unhandled errors, race conditions, off-by-one and boundary mistakes, incorrect edge-case handling, and broken invariants. For every candidate bug, construct the concrete input or state that triggers it and confirm the wrong behavior by reading the code carefully; have a reviewer and an auditor confirm each finding before you rely on it, and discard anything you cannot substantiate.\n\nFor the single highest-confidence bug, apply the smallest correct fix backed by a deterministic test that would have caught it (fails reliably before, passes after; skip the test only for a non-code fix or a genuinely contrived reproduction, and say why), commit it, and open one merge request. If you find no clearly-real bug, open no MR and leave your audit notes as a report.",
    labels: [], guidance: "", max_issues: 0, auto_approve: true, wait_on_limit: true,
  },
  {
    slug: "feature-bingo",
    name: "Feature bingo",
    description: "Weekly brainstorm that proposes one concrete new feature and opens an MR adding it as an idea file.",
    target: "prompt", cron: "0 3 * * 2", timezone: "UTC", model: "fable",
    prompt:
      "Brainstorm ONE concrete, genuinely useful new feature or improvement for this project. Ground it in what the codebase actually does: name the problem it solves, sketch how it would work, and note roughly where it would live and what it would touch.\n\nFirst read the existing files under the `ideas/` folder to avoid duplicates, and check the codebase so you do not propose something that already exists. Write your proposal to a single new idea file under the `ideas/` folder at the repository root, commit it, and open a merge request titled `bingo: <feature>`. If nothing worthwhile comes to mind, open no MR and leave a note.",
    labels: [], guidance: "", max_issues: 0, auto_approve: true, wait_on_limit: true,
  },
  {
    slug: "refactor-scout",
    name: "Refactor scout",
    description: "Biweekly propose-only scout that surveys the repo for one high-value structural refactor and opens an MR adding it as a proposal file.",
    target: "prompt", cron: "0 5 1,15 * *", timezone: "UTC", model: "fable",
    prompt:
      "Survey this repository for ONE high-value structural refactor worth proposing — and PROPOSE it, never implement it. This job never changes the code under refactor; its only output is a single proposal file. Pick a candidate from these shapes: duplication with 3+ occurrences of the same responsibility, an oversized file with a natural cohesion seam, dead branches or config the gates cannot see, or a costly consistency defect — the one with the best impact-to-effort ratio.\n\nDedup before you propose: read the existing `ideas/refactors/` folder INCLUDING already-declined proposals and their decline reasons, and do not re-propose a recorded idea unless the evidence has materially changed (say exactly what changed). Carry your own rigour — derive every count from a named command you ran, cite each claim as `file:line @ <sha>`, and mark it verified or plausible. File it ONLY if it passes its own rubric (impact >= effort, behavior-preserving or flagged, a dedup needs 3+ same-responsibility occurrences, a split needs a real seam).\n\nStay on the propose-only, structural side of the self-improvement job, which IMPLEMENTS small fixes — this one proposes refactors too big or risky for one unattended MR. Write the proposal to a single new file `ideas/refactors/YYYY-MM-DD-<slug>.md` (create the folder if needed), commit it, and open a merge request titled `refactor-scout: <slug>`. If nothing clears the bar this cycle, make no change and open no MR: leave a short note on what you surveyed and why nothing qualified.",
    labels: [], guidance: "", max_issues: 0, auto_approve: true, wait_on_limit: true,
  },
  {
    slug: "bug-triage",
    name: "Bug triage sweep",
    description: "Daily sweep over open issues labelled \"bug\", starting a run for the oldest few.",
    target: "sweep", cron: "0 2 * * *", timezone: "UTC", model: "",
    prompt: "", labels: ["bug"], max_issues: 3, auto_approve: true, wait_on_limit: true,
    guidance:
      "Triage the sweep's bug issue. Reproduce or confirm the reported problem, find its root cause, and fix it if the fix is small and well-contained; otherwise document the diagnosis and the minimal reproduction so a maintainer can act. Keep changes scoped to the bug at hand and back any fix with a test that would have caught it.",
  },
  {
    slug: "planned-sweep",
    name: "Planned-work sweep",
    description: "Daily sweep over open issues labelled \"Planned\", starting a run for the oldest few.",
    target: "sweep", cron: "0 2 * * *", timezone: "UTC", model: "",
    prompt: "", labels: ["Planned"], max_issues: 3, auto_approve: true, wait_on_limit: true,
    guidance:
      "Implement the sweep's planned-work issue. Treat the issue description (and any linked spec) as the specification, deliver the change end to end with tests, and run the project's gate before finishing. Keep the work scoped to what the issue asks for and stop to report if it turns out to depend on something not yet in place.",
  },
  {
    slug: "assigned-sweep",
    name: "Assigned-work sweep",
    description: "Daily sweep over open issues assigned to the uzi bot account, starting a run for the oldest few.",
    target: "sweep", cron: "0 2 * * *", timezone: "UTC", model: "",
    prompt: "", labels: null, max_issues: 3, auto_approve: true, wait_on_limit: true,
    guidance:
      "Implement the sweep's assigned issue. This sweep selects by assignee rather than a label, so there is no selector label to match. Treat the issue description (and any linked spec) as the specification, deliver the change end to end with tests, and run the project's gate before finishing. Keep the work scoped to what the issue asks for and stop to report if it turns out to depend on something not yet in place.",
  },
  {
    // A self_improve entry (PRD #590) carries neither a prompt nor labels/guidance: the
    // orchestration lead resolves its own tracking issue at fire time. So it edits like a
    // prompt default (cadence/model only) but has no baked text to show.
    slug: "self-improve",
    name: "Self-improvement",
    description: "Autonomous self-improvement — audit uzi's own codebase and open one improvement MR per cycle.",
    target: "self_improve", cron: "0 4 */2 * *", timezone: "UTC", model: "",
    prompt: "", labels: [], guidance: "", max_issues: 0, auto_approve: true, wait_on_limit: true,
  },
];

// materializeDefault builds an origin='default' schedule row from a catalog entry, as
// the server does when the owner enables a default on a repo (PRD #589 M2): the
// resolved catalog values are carried on the row so the modal can show the baked
// prompt read-only, even though the real DB stores NULL for those columns.
function materializeDefault(
  entry: CatalogEntry,
  repoId: string,
  id: string,
  over: Partial<Schedule> = {},
): Schedule {
  const repo = mockRepos.find((r) => r.id === repoId);
  const now = new Date().toISOString();
  return {
    id,
    repo_id: repoId,
    repo_path: repo?.path_with_namespace ?? "",
    target: entry.target,
    issue_iid: null,
    labels: entry.target === "sweep" && entry.labels ? [...entry.labels] : null,
    prompt: entry.target === "prompt" ? entry.prompt : "",
    timing: "recurring",
    cron_expr: entry.cron,
    run_at: null,
    timezone: entry.timezone,
    next_fire_at: null,
    last_fired_at: null,
    last_fire: null,
    auto_approve: entry.auto_approve,
    wait_on_limit: entry.wait_on_limit,
    // PRD #841: match the real ScheduleDTO shape — mr_rework_enabled is a non-omitempty
    // *bool, so the API always emits it (null = inherit), never omits it. A catalog default
    // is inherit until an override sets it, so seed the explicit null sentinel here rather
    // than leaving the field undefined (which would diverge from the server response shape).
    mr_rework_enabled: null,
    max_issues: entry.target === "sweep" ? entry.max_issues : null,
    // Owner OVERLAY (issue #675): null by default; a seed sets it via `...over`.
    guidance: null,
    // Resolved catalog guidance for a sweep default, shown read-only (issue #675).
    baked_guidance: entry.target === "sweep" ? entry.guidance || null : null,
    model: entry.model || null,
    override_subagent_model: false,
    enabled: true,
    status: "active",
    origin: "default",
    catalog_slug: entry.slug,
    customized: false,
    // Defaults never group (grouping is a custom-row view concept, PRD #636 Decision 2).
    sibling_group_id: null,
    created_at: now,
    updated_at: now,
    next_fires: [],
    ...over,
  };
}

const catalogBySlug = (slug: string): CatalogEntry | undefined =>
  scheduleCatalog.find((e) => e.slug === slug);

// Seed a few materialized defaults so the Default-jobs UX is visible under
// VITE_UZI_MOCK=1: bug-triage enabled on TWO repos (Layout A — one summary row
// expanding to two per-repo sub-rows, one active + one paused so the resume
// affordance shows), and docs-hygiene enabled + customized on one repo (the
// "customized" indicator + a prominent Reset).
const seededDefaults: Schedule[] = [
  materializeDefault(catalogBySlug("bug-triage")!, "repo-uzi", "sch-def-bt-uzi", {
    last_fired_at: daysFromNow(-1, 2),
    last_fire: {
      fired_at: daysFromNow(-1, 2), matched: 2, capped: false,
      started: [
        { issue_iid: 96, run_id: "b1c0ffee-0000-0000-0000-000000000096", title: "Worker restart drops commits", web_url: "https://gitlab.example.com/vtmocanu/uzi/-/issues/96" },
        { issue_iid: 88, run_id: "b1c0ffee-0000-0000-0000-000000000088", title: "Race in the run poller", web_url: "https://gitlab.example.com/vtmocanu/uzi/-/issues/88" },
      ],
      skips: [],
    },
  }),
  // Already materialized on repo-atlas but PAUSED: re-enabling is a server no-op that
  // returns this paused row, so the UI must offer resume here, never a fresh "enable".
  materializeDefault(catalogBySlug("bug-triage")!, "repo-atlas", "sch-def-bt-atlas", {
    enabled: false,
  }),
  // A customized default (owner shifted the cadence): the "customized" indicator + a
  // prominent Reset that restores the catalog cron.
  materializeDefault(catalogBySlug("docs-hygiene")!, "repo-uzi", "sch-def-dh-uzi", {
    cron_expr: "0 6 * * 1",
    customized: true,
  }),
  // A sweep default carrying an owner GUIDANCE OVERLAY (issue #675): its baked_guidance
  // is the catalog planned-sweep text while `guidance` is a distinct, owner-set overlay,
  // so the modal must show the two independently. The two values MUST differ so a test
  // cannot pass vacuously against the old single-field shape. planned-sweep on repo-ledger
  // is used by no other seed/test, so this adds no enablement collision.
  materializeDefault(catalogBySlug("planned-sweep")!, "repo-ledger", "sch-def-ps-overlay", {
    guidance: "prefer a failing test first, then the smallest fix",
    customized: true,
  }),
];

let schedules: Schedule[] = [
  ...userSchedules.map(
    (s): Schedule => ({
      ...s,
      origin: "user",
      catalog_slug: null,
      customized: false,
      sibling_group_id: null,
      // Baked catalog guidance is default-sweep-only (issue #675); a user row never has it.
      baked_guidance: null,
    }),
  ),
  ...seededDefaults,
];

// The forge labels that already exist on each repo (PRD #589 M4, sweep-label WARN).
// repo-atlas is deliberately missing "bug" and "Planned" so enabling bug-triage /
// planned-sweep there surfaces the missing-label warning + the "Create label" confirm.
const repoLabels: Record<string, string[]> = {
  "repo-uzi": ["bug", "Planned", "PRD", "enhancement"],
  "repo-atlas": ["PRD", "enhancement"],
  "repo-payments": ["PRD"],
};

export const mockApi = {
  ...usersApi,

  ...settingsApi,

  // ── Agent source (PRD #602 M5) ───────────────────────────────────────────────
  ...agentSourceApi,

  ...notificationsApi,

  // ── Secrets ─────────────────────────────────────────────────────────────────
  ...secretsApi,

  ...agentsApi,

  ...forgeApi,

  ...boardsApi,

  // ── Workers ─────────────────────────────────────────────────────────────────
  ...workersApi,

  ...runsApi,

  ...judgeApi,

  ...findingsApi,

  ...chatApi,

  ...cliTokensApi,

  ...memoryApi,

  // ── Scheduled runs (PRD #241) ──────────────────────────────────────────────
  listSchedules: async () => {
    requireSession();
    return delay(schedules.map(scheduleDTO));
  },
  createSchedule: async (repoId: string, input: ScheduleInput) => {
    requireSession();
    const repo = repos.find((r) => r.id === repoId);
    if (!repo) throw new ApiError(404, "repo not found");
    assertGuidanceWithinCap(input.guidance);
    const target = input.target ?? "issue";
    const timing = input.timing ?? "recurring";
    const now = new Date().toISOString();
    const s: Schedule = {
      id: nextScheduleId(),
      repo_id: repoId,
      repo_path: repo.path_with_namespace,
      target,
      issue_iid: target === "issue" ? (input.issue_iid ?? null) : null,
      labels: target === "sweep" && input.labels && input.labels.length ? input.labels : null,
      prompt: target === "prompt" ? (input.prompt ?? "") : "",
      timing,
      cron_expr: timing === "recurring" ? (input.cron_expr ?? "") : "",
      run_at: timing === "once" ? (input.run_at ?? null) : null,
      timezone: input.timezone || "UTC",
      next_fire_at: null,
      last_fired_at: null,
      last_fire: null,
      auto_approve: input.auto_approve ?? true,
      wait_on_limit: input.wait_on_limit ?? true,
      // PRD #841: a create stamps the explicit tri-state override, or leaves it null = inherit.
      mr_rework_enabled: input.mr_rework_enabled ?? null,
      // Sweep-only; new sweeps default to 10 (mirrors the server), unlimited otherwise.
      max_issues: target === "sweep" ? (input.max_issues ?? 10) : null,
      // Guidance on issue/sweep only; null (none) for prompt (re-nulled per target).
      guidance: target === "issue" || target === "sweep" ? (input.guidance ?? null) : null,
      // Baked catalog guidance is a default-sweep-only field (issue #675); null for a user row.
      baked_guidance: null,
      // Model applies to ALL targets (unlike guidance); null = inherit.
      model: input.model ?? null,
      // PRD #305: omitted ≡ false (replace-semantics), default off.
      override_subagent_model: input.override_subagent_model ?? false,
      enabled: input.enabled ?? true,
      status: "active",
      // A create always makes a user-authored row (PRD #589): defaults are born only
      // via enableCatalogSchedule, and a clone via cloneSchedule.
      origin: "user",
      catalog_slug: null,
      customized: false,
      // A bare create sends no group id (standalone). Multi-repo fan-out (PRD #636 M2)
      // stamps a shared id via the create input; a single-repo create stays NULL here.
      sibling_group_id: null,
      created_at: now,
      updated_at: now,
      next_fires: [],
    };
    schedules = [s, ...schedules];
    return delay(scheduleDTO(s), 250);
  },
  getSchedule: async (id: string) => {
    requireSession();
    const s = schedules.find((x) => x.id === id);
    if (!s) throw new ApiError(404, "schedule not found");
    return delay(scheduleDTO(s));
  },
  updateSchedule: async (id: string, input: ScheduleInput) => {
    requireSession();
    const cur = schedules.find((x) => x.id === id);
    if (!cur) throw new ApiError(404, "schedule not found");
    // A catalog default is catalog-owned (PRD #589): the server's patchDefaultScheduleConfig
    // 400s ANY default patch whose body carries a catalog-owned field. Mirror that here so
    // the mock and the server agree — the drift that hid the buildDefaultInput `timing` bug.
    // The rejected set is target/prompt/labels/timing/repo_id/issue_iid (run_at too, but the
    // modal never sends it on a default). Guidance is the exception for a PROMPT default
    // (issue #662) and a SWEEP default (issue #675): it is owner-editable there (overlaid on
    // the catalog prompt/guidance at fire time), so allow it for prompt+sweep defaults and
    // keep rejecting it for issue/self_improve defaults. The 400 message mirrors the server's
    // per-target locked-set string. Only cron/tz/model/flags/max_issues (+prompt/sweep-default
    // guidance) edit.
    if (cur.origin === "default") {
      const guidanceEditable = cur.target === "prompt" || cur.target === "sweep";
      if (
        input.target !== undefined ||
        input.prompt !== undefined ||
        input.labels !== undefined ||
        (!guidanceEditable && input.guidance !== undefined) ||
        input.timing !== undefined ||
        (input.repo_id !== undefined && input.repo_id !== "") ||
        input.issue_iid !== undefined ||
        input.run_at !== undefined
      ) {
        throw new ApiError(
          400,
          guidanceEditable
            ? "a default schedule's prompt, labels, target, timing and repo are catalog-owned and cannot be edited"
            : "a default schedule's prompt, labels, guidance, target, timing and repo are catalog-owned and cannot be edited",
        );
      }
    }
    assertGuidanceWithinCap(input.guidance);
    const m: Schedule = { ...cur };
    if (input.target !== undefined) m.target = input.target;
    if (input.timing !== undefined) m.timing = input.timing;
    if (input.issue_iid !== undefined) m.issue_iid = input.issue_iid;
    if (input.labels !== undefined) m.labels = input.labels.length ? input.labels : null;
    if (input.prompt !== undefined) m.prompt = input.prompt;
    if (input.cron_expr !== undefined) m.cron_expr = input.cron_expr;
    if (input.run_at !== undefined) m.run_at = input.run_at;
    if (input.timezone !== undefined) m.timezone = input.timezone;
    if (input.auto_approve !== undefined) m.auto_approve = input.auto_approve;
    if (input.wait_on_limit !== undefined) m.wait_on_limit = input.wait_on_limit;
    // PRD #841: replace-semantics — apply when present (explicit null clears to inherit).
    if (input.mr_rework_enabled !== undefined) m.mr_rework_enabled = input.mr_rework_enabled;
    // Replace-semantics: apply when the key is present (explicit null = unlimited).
    if (input.max_issues !== undefined) m.max_issues = input.max_issues;
    // Same replace-semantics for guidance (explicit null/"" clears to none).
    if (input.guidance !== undefined) m.guidance = input.guidance;
    // Model applies to all targets, so it is not re-nulled per target below.
    if (input.model !== undefined) m.model = input.model;
    // PRD #305: replace-semantics, applied when the key is present.
    if (input.override_subagent_model !== undefined)
      m.override_subagent_model = input.override_subagent_model;
    if (input.enabled !== undefined) m.enabled = input.enabled;
    // PRD #344 Feature A: a non-empty repo_id that differs from the current one repoints the
    // schedule. Mirror the server: an issue-target schedule is rejected (422, D4 restrict);
    // an unknown/unowned repo is a 404; otherwise move repo_id and refresh repo_path.
    if (input.repo_id !== undefined && input.repo_id !== "" && input.repo_id !== cur.repo_id) {
      if (m.target === "issue")
        throw new ApiError(422, "repointing an issue-target schedule is not supported; delete and recreate it against the new repo");
      const repo = repos.find((r) => r.id === input.repo_id);
      if (!repo) throw new ApiError(404, "repo not found");
      m.repo_id = repo.id;
      m.repo_path = repo.path_with_namespace;
    }
    // Re-null the fields the (possibly changed) target/timing does not use, so the
    // stored shape matches the DB's field-presence CHECK.
    m.issue_iid = m.target === "issue" ? m.issue_iid : null;
    m.labels = m.target === "sweep" ? m.labels : null;
    m.max_issues = m.target === "sweep" ? m.max_issues : null;
    // A prompt DEFAULT keeps its owner guidance (issue #662); a USER prompt schedule still
    // nulls it (guidance is issue/sweep-only for user rows, catalog-owned/baked otherwise).
    m.guidance =
      m.target === "issue" || m.target === "sweep" || (m.origin === "default" && m.target === "prompt")
        ? m.guidance
        : null;
    m.prompt = m.target === "prompt" ? m.prompt : "";
    m.cron_expr = m.timing === "recurring" ? m.cron_expr : "";
    m.run_at = m.timing === "once" ? m.run_at : null;
    // A default row that edits an editable field away from its catalog values reads as
    // "customized" (PRD #589 M2), which lights the indicator and Reset. Only the enable
    // pause-flip leaves it untouched (a bare { enabled } PATCH changes nothing else).
    if (m.origin === "default" && m.catalog_slug) {
      const entry = catalogBySlug(m.catalog_slug);
      if (entry) {
        m.customized =
          m.cron_expr !== entry.cron ||
          m.timezone !== entry.timezone ||
          (m.model ?? "") !== entry.model ||
          m.auto_approve !== entry.auto_approve ||
          m.wait_on_limit !== entry.wait_on_limit ||
          // mr_rework_enabled (PRD #841): the catalog baseline is inherit (null), so ANY
          // explicit override flips customized — mirroring the server's `if mrRework.Valid`
          // in defaultEditableDiverges (api/internal/handler/schedules.go).
          m.mr_rework_enabled != null ||
          (m.target === "sweep" && (m.max_issues ?? 0) !== entry.max_issues) ||
          // Owner guidance-overlay divergence for a prompt default (issue #662) or a sweep
          // default (issue #675): the OVERLAY (m.guidance) is null until the owner sets one,
          // so any non-empty overlay flips customized. The baked catalog guidance is NOT
          // compared (only the overlay's presence matters), matching the server.
          ((m.target === "prompt" || m.target === "sweep") && (m.guidance ?? "") !== "");
      }
    }
    m.updated_at = new Date().toISOString();
    schedules = schedules.map((x) => (x.id === id ? m : x));
    return delay(scheduleDTO(m));
  },
  deleteSchedule: async (id: string) => {
    requireSession();
    const s = schedules.find((x) => x.id === id);
    if (!s) throw new ApiError(404, "schedule not found");
    schedules = schedules.filter((x) => x.id !== id);
    return delay(null);
  },
  runScheduleNow: async (id: string) => {
    requireSession();
    const s = schedules.find((x) => x.id === id);
    if (!s) throw new ApiError(404, "schedule not found");
    // The demo does not spin up a live worker run; it reports one fired, matching
    // the seam's typical single-run outcome for a pinned issue / prompt.
    const runId = nextRunId();
    return delay(
      {
        created: 1,
        run_ids: [runId],
        matched: 1,
        capped: false,
        started: [{ issue_iid: s.issue_iid, run_id: runId, title: s.prompt || `#${s.issue_iid ?? ""}` }],
        skips: [],
      },
      250,
    );
  },
  previewSchedule: async (input: SchedulePreviewInput) => {
    requireSession();
    const n = Math.min(Math.max(input.n ?? 3, 1), schedulePreviewCap);
    if (input.timing === "once") {
      const fires = input.run_at && new Date(input.run_at).getTime() > Date.now()
        ? [new Date(input.run_at).toISOString()]
        : input.run_at
          ? [new Date(input.run_at).toISOString()]
          : [];
      return delay({ fires }, 80);
    }
    return delay({ fires: mockScheduleFires(input.cron_expr ?? "", n) }, 80);
  },

  // ── Default scheduled jobs (PRD #589) ──────────────────────────────────────
  listScheduleCatalog: async () => {
    requireSession();
    // Derive enablements from the live default rows so enable/reset/clone stay in
    // sync (presence of a default row for (repo, slug) IS the enablement).
    const enablements = schedules
      .filter((s) => s.origin === "default" && s.catalog_slug)
      .map((s) => ({
        repo_id: s.repo_id,
        slug: s.catalog_slug as string,
        schedule_id: s.id,
        enabled: s.enabled,
      }));
    return delay({ entries: scheduleCatalog.map((e) => ({ ...e, labels: e.labels ? [...e.labels] : null })), enablements });
  },
  enableCatalogSchedule: async (repoId: string, slug: string, timezone?: string) => {
    requireSession();
    const repo = repos.find((r) => r.id === repoId);
    if (!repo) throw new ApiError(404, "repo not found");
    const entry = catalogBySlug(slug);
    if (!entry) throw new ApiError(404, "catalog entry not found");
    // Idempotent (server 200): an already-materialized (repo, slug) — even paused —
    // returns its existing row untouched, never a fresh enable. Mirroring the server's
    // ON CONFLICT DO NOTHING, a re-enable ignores any timezone override (issue #660).
    const existing = schedules.find(
      (s) => s.origin === "default" && s.catalog_slug === slug && s.repo_id === repoId,
    );
    if (existing) return delay(scheduleDTO(existing));
    const s = materializeDefault(entry, repoId, nextScheduleId());
    // On the fresh-materialize path only, an optional detected browser timezone (issue
    // #660) overrides the catalog zone; an empty/absent tz keeps the catalog zone. Mirror
    // the production handler: trim, and reject an invalid IANA name (Intl throws a
    // RangeError on both a bogus name and the "Local" sentinel) with a 400.
    const tz = timezone?.trim();
    if (tz) {
      try {
        Intl.DateTimeFormat(undefined, { timeZone: tz });
      } catch {
        throw new ApiError(400, "invalid timezone");
      }
      s.timezone = tz;
    }
    schedules = [s, ...schedules];
    return delay(scheduleDTO(s), 200);
  },
  resetSchedule: async (id: string) => {
    requireSession();
    const cur = schedules.find((x) => x.id === id);
    if (!cur) throw new ApiError(404, "schedule not found");
    if (cur.origin !== "default" || !cur.catalog_slug)
      throw new ApiError(422, "only a default schedule can be reset");
    const entry = catalogBySlug(cur.catalog_slug);
    if (!entry) throw new ApiError(404, "catalog entry not found");
    // Restore the editable fields to the catalog values, keep the pause flag + repo,
    // and clear customized. Prompt/labels/guidance are already the resolved catalog
    // values on a default row, so re-materializing is exact.
    const restored = materializeDefault(entry, cur.repo_id, cur.id, {
      enabled: cur.enabled,
      last_fired_at: cur.last_fired_at,
      last_fire: cur.last_fire,
      created_at: cur.created_at,
    });
    schedules = schedules.map((x) => (x.id === id ? restored : x));
    return delay(scheduleDTO(restored));
  },
  cloneSchedule: async (id: string, repoId?: string) => {
    requireSession();
    const src = schedules.find((x) => x.id === id);
    if (!src) throw new ApiError(404, "schedule not found");
    const targetRepoId = repoId && repoId.trim() !== "" ? repoId : src.repo_id;
    const repo = repos.find((r) => r.id === targetRepoId);
    if (!repo) throw new ApiError(404, "repo not found");
    const now = new Date().toISOString();
    // A user-owned copy with the source's resolved fields baked in and catalog_slug
    // cleared, so its prompt/labels/guidance become editable (origin='user').
    const clone: Schedule = {
      ...src,
      id: nextScheduleId(),
      repo_id: repo.id,
      repo_path: repo.path_with_namespace,
      origin: "user",
      catalog_slug: null,
      customized: false,
      enabled: true,
      status: "active",
      last_fired_at: null,
      last_fire: null,
      created_at: now,
      updated_at: now,
      next_fires: [],
      // A clone is always a user row, which never carries the read-only baked catalog
      // guidance (issue #675). For a SWEEP default, fold the baked catalog guidance into
      // the editable guidance and discard the owner overlay, mirroring the server clone.
      guidance:
        src.origin === "default" && src.target === "sweep" ? src.baked_guidance : src.guidance,
      baked_guidance: null,
    };
    schedules = [clone, ...schedules];
    return delay(scheduleDTO(clone), 200);
  },
  addScheduleRepo: async (id: string, repoId: string) => {
    requireSession();
    // Owner-scoped, origin='user' sources only (PRD #636 Decision 5), matching the server
    // (handler/schedules.go AddScheduleRepo): a foreign/absent source 404s, but a non-user
    // source is a 409 ("only a custom schedule can add a repo; clone a default first"),
    // mirroring ResetSchedule's origin-mismatch conflict — NOT a 404.
    const src = schedules.find((x) => x.id === id);
    if (!src) throw new ApiError(404, "schedule not found");
    if (src.origin !== "user")
      throw new ApiError(409, "only a custom schedule can add a repo; clone a default schedule first");
    const repo = repos.find((r) => r.id === repoId);
    if (!repo) throw new ApiError(404, "repo not found");
    // Coalesce a group id onto the source (the server does this under a row lock so two
    // racing calls settle on one id; a single-writer mock just assigns it once).
    const groupId = src.sibling_group_id ?? crypto.randomUUID();
    if (!src.sibling_group_id) {
      schedules = schedules.map((x) => (x.id === src.id ? { ...x, sibling_group_id: groupId } : x));
    }
    // The partial unique index (sibling_group_id, repo_id) rejects a second sibling on a
    // repo already in the group — an idempotent-safe 409, no duplicate row created.
    if (schedules.some((x) => x.sibling_group_id === groupId && x.repo_id === repoId)) {
      throw new ApiError(409, "that schedule is already on that repo");
    }
    const now = new Date().toISOString();
    // Copy the source's current config onto the new repo as an independent sibling. The
    // server copies the SOURCE's `enabled` (cur.Enabled) — so a repo added from a PAUSED
    // custom schedule yields a PAUSED sibling — while `status` takes the CreateRunSchedule
    // default ("active"), the same fresh-row status the clone/materialize paths use.
    const sibling: Schedule = {
      ...src,
      id: nextScheduleId(),
      repo_id: repo.id,
      repo_path: repo.path_with_namespace,
      sibling_group_id: groupId,
      enabled: src.enabled,
      status: "active",
      last_fired_at: null,
      last_fire: null,
      created_at: now,
      updated_at: now,
      next_fires: [],
    };
    schedules = [sibling, ...schedules];
    return delay(scheduleDTO(sibling), 200);
  },
  checkRepoLabels: async (repoId: string, labels: string[]) => {
    requireSession();
    const repo = repos.find((r) => r.id === repoId);
    if (!repo) throw new ApiError(404, "repo not found");
    const have = repoLabels[repoId] ?? [];
    const missing = [...new Set(labels.map((l) => l.trim()).filter(Boolean))].filter(
      (l) => !have.includes(l),
    );
    return delay({ missing }, 150);
  },
  ensureRepoLabels: async (repoId: string, labels: string[]) => {
    requireSession();
    const repo = repos.find((r) => r.id === repoId);
    if (!repo) throw new ApiError(404, "repo not found");
    const ensured = [...new Set(labels.map((l) => l.trim()).filter(Boolean))];
    const have = repoLabels[repoId] ?? (repoLabels[repoId] = []);
    for (const l of ensured) if (!have.includes(l)) have.push(l);
    return delay({ ensured }, 200);
  },
};

// A run patch helper other mock surfaces can use (kept for symmetry/tests).
export { patchRun };
// sameContent is imported by lib/agentTemplateDriftContract.test.ts through the index.
export { sameContent } from "./agents";
// Judge-backlog internals imported by the fidelity/truncation fixtures through the index.
export {
  bucketOf,
  filterGroups,
  groupJudgeRecommendations,
  capBacklogRows,
  MOCK_BACKLOG_MAX_ROWS,
  type JudgeBacklogRow,
} from "./judge";
