import type {
  ForgeConnection,
  GuardrailOverrideMeta,
  Repo,
} from "../../lib/api";
import { daysAgo, minsAgo } from "./time";

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

// PRD #337 M2: a multi-forge config exercising the connect-form URL⇄type sync —
// three recognized hosts (github/gitlab/forgejo) plus one unrecognized self-hosted
// host that stays under manual control in both directions.
export const mockForgeConfigSyncForges = {
  allowed_base_urls: [
    "https://gitlab.example.com",
    "https://forgejo.example.com",
    "https://github.com",
    "https://git.example.com",
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
    web_url: "https://gitlab.example.com/myorg/uzi",
    default_branch: "main",
    enabled: true,
    repo_skills_enabled: true,
    // A deliberate mix: repo skills on, repo instructions off — so the Trusted-repo
    // panel's sub-toggle independence is exercisable under VITE_UZI_MOCK=1.
    repo_claudemd_enabled: false,
    repo_devbox_opt_in: true,
    // The uzi repo dogfoods self-improve (PRD #686), so the fold capability is on.
    repo_fold_improve_uzi_backlog: true,
    pipeline: {
      status: "failed",
      web_url: "https://gitlab.example.com/myorg/uzi/-/pipelines/4242",
      ref: "main",
      pipeline_id: 4242,
      synced_at: minsAgo(1),
    },
    // PRD #66 M8 (D8): no admin guardrail override active. The M9 UI renders off this.
    guardrail_override: null,
    // PRD #66 M9 (D8): not refused by the guardrail (server-computed).
    guardrail_blocked: false,
    // PRD #361 M1: this repo is on the global Docker-worker allowlist (the fully
    // set-up row), so its Setup chip's Docker capability reads on.
    docker_allowlisted: true,
    // PRD #361 M3: not blocked (an allowlisted repo makes every worker eligible).
    docker_blocked: false,
  },
  {
    id: "repo-atlas",
    connection_id: "conn-1",
    forge_project_id: 204,
    path_with_namespace: "vtmocanu/atlas-api",
    web_url: "https://gitlab.example.com/myorg/atlas-api",
    default_branch: "main",
    enabled: true,
    repo_skills_enabled: false,
    repo_claudemd_enabled: false,
    repo_devbox_opt_in: false,
    repo_fold_improve_uzi_backlog: false,
    pipeline: {
      status: "success",
      web_url: "https://gitlab.example.com/myorg/atlas-api/-/pipelines/3311",
      ref: "main",
      pipeline_id: 3311,
      synced_at: minsAgo(2),
    },
    // PRD #66 M9 (D8): an admin has explicitly allowed this repo through the
    // guardrail — the "allowed by admin" badge + Revoke path in the demo.
    guardrail_override: { ...mockAtlasOverride },
    guardrail_blocked: false,
    // PRD #361 M1: not on the Docker-worker allowlist.
    docker_allowlisted: false,
    // PRD #361 M3: actively blocked — an enabled repo with a queued run whose only
    // online workers are Docker workers, so its Setup chip escalates to the info tone.
    docker_blocked: true,
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
    repo_fold_improve_uzi_backlog: false,
    pipeline: null,
    guardrail_override: null,
    guardrail_blocked: true,
    // PRD #361 M1: not on the Docker-worker allowlist.
    docker_allowlisted: false,
    // PRD #361 M3: not actively blocked by the Docker-allowlist gap.
    docker_blocked: false,
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
    repo_fold_improve_uzi_backlog: false,
    pipeline: null,
    guardrail_override: null,
    guardrail_blocked: false,
    // PRD #361 M1: not on the Docker-worker allowlist.
    docker_allowlisted: false,
    // PRD #361 M3: not actively blocked by the Docker-allowlist gap.
    docker_blocked: false,
  },
  {
    // PRD #345 M2: the disabled+blocked row that makes the refused-enable demo
    // reachable under VITE_UZI_MOCK=1. No pre-existing fixture was BOTH
    // enabled:false AND guardrail_blocked:true, so clicking Enable here is the
    // one-click path that trips mockApi.setRepoEnabled's 422 guardrail block
    // (reasons from mockBlockedRepoMeta below). We add this row rather than
    // flipping repo-payments, whose enabled:true state the Boards "runs blocked"
    // badge and admin Allow-anyway demo depend on.
    id: "repo-ledger",
    connection_id: "conn-1",
    forge_project_id: 641,
    path_with_namespace: "team-beta/ledger-service",
    web_url: "https://gitlab.example.com/team-beta/ledger-service",
    default_branch: "main",
    enabled: false,
    repo_skills_enabled: false,
    repo_claudemd_enabled: false,
    repo_devbox_opt_in: false,
    repo_fold_improve_uzi_backlog: false,
    pipeline: null,
    guardrail_override: null,
    guardrail_blocked: true,
    // PRD #361 M1: not on the Docker-worker allowlist.
    docker_allowlisted: false,
    // PRD #361 M3: not actively blocked by the Docker-allowlist gap.
    docker_blocked: false,
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
  // PRD #345 M2: the disabled+blocked row (repo-ledger) whose enable is refused.
  // These block_messages are the enable-guardrail (privcheck) reasons rendered as
  // the 422 violations when a member clicks Enable on it under VITE_UZI_MOCK=1.
  "repo-ledger": {
    owner_id: "u-dana",
    owner_email: "dana@example.com",
    forge_type: "gitlab",
    block_messages: [
      "could not read default-branch protection on this repo",
      "the default branch is protected but the write role (Developer) may push to it",
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
