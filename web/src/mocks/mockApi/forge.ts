import type {
  AdminBlockedRepos,
  PrivilegeReport,
  ProjectSyncOwnerKind,
  ProjectSyncStatus,
  ToolAllowlistEntry,
  ToolAllowlistWriteInput,
} from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import { CAPABILITY_VOCABULARY } from "../../lib/capabilityVocabulary";
import {
  mockAdmin,
  mockBlockedRepoMeta,
  mockConnection,
  mockForgeConfig,
  mockRepoToolProfiles,
  mockRepos,
  mockToolAllowlist,
} from "../data";
import { delay, requireSession } from "./shared";

let connections = [{ ...mockConnection }];
export let repos = mockRepos.map((r) => ({ ...r }));
// PRD #534: a GitHub connection plus two GitHub repos so the Boards "Project
// sync" cell renders and its linked/unlinked panel states are exercisable under
// VITE_UZI_MOCK=1. gitlab (conn-1) stays FIRST so it remains the demo's default
// selection and the existing gitlab-oriented flows are untouched — select the
// GitHub bot to reach these rows. The sync cell keys off the SELECTED
// connection's forge_type, so both repos read as GitHub once conn-gh is picked.
connections = [
  ...connections,
  {
    ...mockConnection,
    id: "conn-gh",
    forge_type: "github",
    base_url: "https://github.com",
    bot_username: "uzi-bot-gh",
  },
];
repos = [
  ...repos,
  {
    ...repos[0],
    id: "repo-gh-linked",
    connection_id: "conn-gh",
    path_with_namespace: "vtmocanu/gh-linked",
    web_url: "https://github.com/vtmocanu/gh-linked",
    pipeline: null,
    guardrail_override: null,
    guardrail_blocked: false,
  },
  {
    ...repos[0],
    id: "repo-gh-unlinked",
    connection_id: "conn-gh",
    path_with_namespace: "vtmocanu/gh-unlinked",
    web_url: "https://github.com/vtmocanu/gh-unlinked",
    pipeline: null,
    guardrail_override: null,
    guardrail_blocked: false,
  },
];
// PRD #534: in-memory GitHub Projects v2 links, keyed by repo id. Seeded with
// repo-gh-linked so the "linked" readout is visible; repo-gh-unlinked is left
// absent so the provision/adopt state shows. Mirrors the server: getStatus 404s
// an unlinked repo, provision/adopt set the entry, disable deletes it.
const githubProjectLinks = new Map<string, ProjectSyncStatus>([
  [
    "repo-gh-linked",
    {
      project_number: 42,
      owned_by_uzi: true,
      last_synced_at: new Date(Date.now() - 5 * 60_000).toISOString(),
      last_error: null,
      item_count: 7,
    },
  ],
]);
// PRD #557: per-repo board visibility, keyed by repo id. A linked board defaults
// to private (false) — GitHub creates ProjectV2 boards private — and the toggle
// round-trips a mutable value here so the vitest toggle case is non-vacuous.
const githubProjectVisibility = new Map<string, boolean>();

// Tool allowlist + per-repo profiles (PRD #18 M4).
let toolAllowlist: ToolAllowlistEntry[] = mockToolAllowlist.map((e) => ({ ...e }));
const repoToolProfiles = new Map<string, string[]>(
  Object.entries(mockRepoToolProfiles).map(([k, v]) => [k, [...v]]),
);
let toolEntryCounter = 0;

export const forgeApi = {
  // ── Forge ───────────────────────────────────────────────────────────────────
  forgeConfig: async () => delay({ ...mockForgeConfig, allowed_base_urls: [...mockForgeConfig.allowed_base_urls] }),
  listConnections: async () => delay({ connections: connections.map((c) => ({ ...c })) }),
  createConnection: async (baseUrl: string, _token: string, forgeType = "gitlab") => {
    const conn = {
      ...mockConnection,
      id: `conn-${Date.now()}`,
      base_url: baseUrl,
      forge_type: forgeType,
      created_at: new Date().toISOString(),
      last_verified_at: new Date().toISOString(),
      // A freshly connected bot is unchecked until the first privilege check.
      privilege_status: null,
      privilege_checked_at: null,
      privilege_report: null,
    };
    connections = [conn];
    return delay({ connection: { ...conn } }, 600);
  },
  verifyConnection: async (id: string) => {
    const c = connections.find((x) => x.id === id);
    if (!c) throw new ApiError(404, "connection not found");
    c.last_verified_at = new Date().toISOString();
    return delay({ connection: { ...c } }, 500);
  },
  // Mirrors the real save path (PRD #19 M3): a collision on the same host is a hard
  // 409, an unknown username still saves but returns a warning (verified-or-warned),
  // and "" clears the mapping.
  updateConnection: async (id: string, humanUsername: string) => {
    const c = connections.find((x) => x.id === id);
    if (!c) throw new ApiError(404, "connection not found");
    const username = humanUsername.trim();
    if (username) {
      const clash = connections.some(
        (x) => x.id !== id && x.base_url === c.base_url && x.human_username === username,
      );
      if (clash) {
        throw new ApiError(409, "that forge username is already mapped by another user on this host");
      }
    }
    c.human_username = username || null;
    // Demo the warning branch for an obviously-fake username without a live forge.
    const warning =
      username && username.toLowerCase() === "ghost"
        ? "Saved, but no forge account with this username was found — double-check it matches your own forge username."
        : undefined;
    return delay({ connection: { ...c }, ...(warning ? { warning } : {}) }, 400);
  },
  privilegeCheck: async (id: string) => {
    const c = connections.find((x) => x.id === id);
    if (!c) throw new ApiError(404, "connection not found");
    const now = new Date().toISOString();
    const report: PrivilegeReport = {
      checked_at: now,
      status: "ok",
      token: { scopes: ["api"], active: true, violations: [], warnings: [] },
      repos: repos
        .filter((r) => r.enabled)
        .map((r) => ({
          repo_id: r.id,
          path: r.path_with_namespace,
          role: "write",
          member: true,
          findings: [],
        })),
    };
    c.privilege_status = "ok";
    c.privilege_checked_at = now;
    c.privilege_report = report;
    return delay({ report }, 500);
  },
  deleteConnection: async (id: string) => {
    connections = connections.filter((x) => x.id !== id);
    return delay(null);
  },
  listProjects: async (_connectionId: string) => delay({ repos: repos.map((r) => ({ ...r })) }, 350),

  listRepos: async () => delay({ repos: repos.filter((r) => r.enabled).map((r) => ({ ...r })) }),
  setRepoEnabled: async (id: string, enabled: boolean) => {
    const r = repos.find((x) => x.id === id);
    if (!r) throw new ApiError(404, "repo not found");
    // PRD #345 M2: mirror the server, which runs the enable guardrail (privcheck)
    // ONLY on the enable path (forge.go SetRepoEnabled). A refused enable returns
    // 422 { error, violations[] }; a disable is never gated. This makes the
    // refused-enable UX (Repos.tsx) reproducible under VITE_UZI_MOCK=1.
    if (enabled && r.guardrail_blocked) {
      const violations = mockBlockedRepoMeta[id]?.block_messages ?? [
        "this repository cannot be enabled until its guardrail violations are resolved",
      ];
      throw new ApiError(422, "repository cannot be enabled — guardrail violations", { violations });
    }
    r.enabled = enabled;
    return delay({ repo: { ...r } });
  },
  // Explicit per-repo remove (PRD #357). Mirrors deleteConnection: drops the repo
  // from the in-memory list so demo/browser mode reflects the deletion. Enforces
  // the server's disabled-only guard (409 on an enabled repo) so the flow behaves
  // like the real endpoint without any /api/ call escaping the mock.
  deleteRepo: async (id: string) => {
    const r = repos.find((x) => x.id === id);
    if (!r) throw new ApiError(404, "repo not found");
    if (r.enabled) throw new ApiError(409, "disable this repo before removing it");
    repos = repos.filter((x) => x.id !== id);
    return delay(null);
  },
  setRepoSkillsEnabled: async (id: string, enabled: boolean) => {
    const r = repos.find((x) => x.id === id);
    if (!r) throw new ApiError(404, "repo not found");
    r.repo_skills_enabled = enabled;
    return delay({ repo: { ...r } });
  },
  setRepoClaudemdEnabled: async (id: string, enabled: boolean) => {
    const r = repos.find((x) => x.id === id);
    if (!r) throw new ApiError(404, "repo not found");
    r.repo_claudemd_enabled = enabled;
    return delay({ repo: { ...r } });
  },
  // Trusted-repo master control (PRD #246): sets whichever of the two trust flags
  // are present in one call, mirroring the server's atomic both-flags path.
  setRepoTrustFlags: async (
    id: string,
    flags: { repo_skills_enabled?: boolean; repo_claudemd_enabled?: boolean },
  ) => {
    const r = repos.find((x) => x.id === id);
    if (!r) throw new ApiError(404, "repo not found");
    if (flags.repo_skills_enabled !== undefined) r.repo_skills_enabled = flags.repo_skills_enabled;
    if (flags.repo_claudemd_enabled !== undefined) r.repo_claudemd_enabled = flags.repo_claudemd_enabled;
    return delay({ repo: { ...r } });
  },
  setRepoDevboxOptIn: async (id: string, enabled: boolean) => {
    const r = repos.find((x) => x.id === id);
    if (!r) throw new ApiError(404, "repo not found");
    r.repo_devbox_opt_in = enabled;
    return delay({ repo: { ...r } });
  },
  // PRD #686: per-repo self-improve dogfooding capability. Owner-or-admin on the
  // server; the mock just flips the flag and echoes the repo.
  setRepoFoldImproveUziBacklog: async (id: string, enabled: boolean) => {
    const r = repos.find((x) => x.id === id);
    if (!r) throw new ApiError(404, "repo not found");
    r.repo_fold_improve_uzi_backlog = enabled;
    return delay({ repo: { ...r } });
  },
  // PRD #84 M2: static per-repo capability hint. Mirrors the server's
  // capability.Filter to the {docker, jvm} vocabulary so only valid names persist.
  setRepoRequiredCapabilities: async (id: string, caps: string[]) => {
    const r = repos.find((x) => x.id === id);
    if (!r) throw new ApiError(404, "repo not found");
    const vocab = new Set<string>(CAPABILITY_VOCABULARY);
    r.required_capabilities = caps.filter((c) => vocab.has(c));
    return delay({ repo: { ...r } });
  },
  // PRD #66 M9 (D8): admin per-repo guardrail override. Requires a non-empty reason
  // (mirrors the server 400). Setting it clears guardrail_blocked in the demo (the
  // override downgrades the waivable findings); revoking re-arms it as blocked.
  setRepoGuardrailOverride: async (id: string, reason: string) => {
    const r = repos.find((x) => x.id === id);
    if (!r) throw new ApiError(404, "repo not found");
    if (reason.trim() === "") throw new ApiError(400, "a non-empty reason is required to override the guardrail");
    r.guardrail_override = { reason: reason.trim(), by: "you@example.com", at: new Date().toISOString() };
    r.guardrail_blocked = false;
    return delay({ repo: { ...r } });
  },
  clearRepoGuardrailOverride: async (id: string) => {
    const r = repos.find((x) => x.id === id);
    if (!r) throw new ApiError(404, "repo not found");
    r.guardrail_override = null;
    // Re-arm the guardrail: absent the override, the repo is blocked again iff its
    // underlying findings are still waivable blocks (mock stand-in: seeded block
    // messages). This is what makes Revoke round-trip back to "runs blocked".
    r.guardrail_blocked = (mockBlockedRepoMeta[id]?.block_messages.length ?? 0) > 0;
    return delay({ repo: { ...r } });
  },

  // ── GitHub Projects v2 sync (PRD #534) ───────────────────────────────────────
  // Read the repo's link status. Mirrors the server: a linked repo returns its
  // status object; an unlinked one 404s ("project sync not enabled for this repo"),
  // the same 404 the server uses to hide existence from a non-owner.
  getProjectSyncStatus: async (id: string) => {
    const link = githubProjectLinks.get(id);
    if (!link) throw new ApiError(404, "project sync not enabled for this repo");
    return delay({ ...link });
  },
  // Owner type for the Adopt-first Provision nudge (PRD #576 M1). The mock treats
  // every repo as org-owned so Provision stays available in the demo/offline mode.
  getProjectSyncOwnerType: async (id: string) => {
    void id;
    return delay<{ owner_type: "User" | "Organization" }>({ owner_type: "Organization" });
  },
  // Provision a fresh project: record a uzi-owned link and return the created status.
  provisionProjectSync: async (
    id: string,
    body: { owner_kind: ProjectSyncOwnerKind; title?: string },
  ) => {
    void body;
    githubProjectLinks.set(id, {
      project_number: 1000 + githubProjectLinks.size,
      owned_by_uzi: true,
      last_synced_at: null,
      last_error: null,
      item_count: 0,
    });
    return delay({ status: "provisioned" });
  },
  // Adopt an existing project by number: record an adopted (not uzi-owned) link.
  adoptProjectSync: async (
    id: string,
    body: { project_number: number; owner_kind: ProjectSyncOwnerKind },
  ) => {
    githubProjectLinks.set(id, {
      project_number: body.project_number,
      owned_by_uzi: false,
      last_synced_at: null,
      last_error: null,
      item_count: 0,
    });
    return delay({ status: "linked" });
  },
  // Re-seed an already-linked board (PRD #576 M3). A 404 when not linked, mirroring
  // the server's not-linked sentinel; otherwise a no-op idempotent re-seed.
  resyncProjectSync: async (id: string) => {
    if (!githubProjectLinks.has(id))
      throw new ApiError(404, "this repo has no linked project to resync");
    return delay({ status: "resynced" });
  },
  // Safe column auto-create (PRD #576 M6): create a fresh uzi-owned field with all the
  // repo's columns and switch the link to it. In the mock, clear the unmatched set so the
  // panel reflects "all columns now sync".
  autocreateProjectSyncColumns: async (id: string) => {
    const link = githubProjectLinks.get(id);
    if (!link)
      throw new ApiError(404, "this repo has no linked project to auto-create columns for");
    githubProjectLinks.set(id, { ...link, unmatched_columns: [] });
    return delay({ status: "columns_created" });
  },
  // Unlink the repo from its project (empty 204 body).
  disableProjectSync: async (id: string) => {
    githubProjectLinks.delete(id);
    githubProjectVisibility.delete(id);
    return delay(null);
  },

  // ── Board access: visibility + write-only sharing (PRD #557) ────────────────
  // Read the linked board's public flag. Mirrors the server: an unlinked repo
  // 404s (same existence-hiding 404 as the status route); a linked one defaults
  // to private until the toggle flips it.
  getProjectSyncVisibility: async (id: string) => {
    if (!githubProjectLinks.has(id))
      throw new ApiError(404, "project sync not enabled for this repo");
    return delay({ public: githubProjectVisibility.get(id) ?? false });
  },
  // Flip the board's public flag. The value is stored so the toggle round-trips.
  setProjectSyncVisibility: async (id: string, isPublic: boolean) => {
    githubProjectVisibility.set(id, isPublic);
    return delay({ public: isPublic });
  },
  // Grant a GitHub user Reader access (empty 204 body). The designated login
  // "nouser" 422s (ErrProjectSyncUserNotFound) so the bad-username inline-error
  // vitest case exercises a real failure, not a resolved success.
  shareProjectSync: async (id: string, username: string) => {
    void id;
    if (username.trim() === "nouser")
      throw new ApiError(422, "no github user with that username");
    return delay(null);
  },
  // Revoke a GitHub user's access (empty 204 body).
  unshareProjectSync: async (id: string, username: string) => {
    void id;
    void username;
    return delay(null);
  },

  // ── Tool allowlist + repo tool profiles (PRD #18 M4) ─────────────────────────
  listToolAllowlist: async () => delay({ allowlist: toolAllowlist.map((e) => ({ ...e })) }),
  createToolAllowlistEntry: async (input: ToolAllowlistWriteInput) => {
    const name = (input.name ?? "").trim();
    if (name === "") throw new ApiError(400, "name is required");
    if (toolAllowlist.some((e) => e.name === name)) throw new ApiError(409, "that package is already on the allowlist");
    const now = new Date().toISOString();
    const entry: ToolAllowlistEntry = {
      id: `tal-${++toolEntryCounter}`,
      name,
      pinned_version: input.pinned_version?.trim() || null,
      note: input.note?.trim() || null,
      updated_by: requireSession().id,
      created_at: now,
      updated_at: now,
    };
    toolAllowlist = [...toolAllowlist, entry].sort((a, b) => a.name.localeCompare(b.name));
    return delay({ entry: { ...entry } });
  },
  updateToolAllowlistEntry: async (id: string, input: ToolAllowlistWriteInput) => {
    const entry = toolAllowlist.find((e) => e.id === id);
    if (!entry) throw new ApiError(404, "allowlist entry not found");
    entry.pinned_version = input.pinned_version?.trim() || null;
    entry.note = input.note?.trim() || null;
    entry.updated_at = new Date().toISOString();
    return delay({ entry: { ...entry } });
  },
  deleteToolAllowlistEntry: async (id: string) => {
    toolAllowlist = toolAllowlist.filter((e) => e.id !== id);
    return delay(null);
  },
  getRepoToolProfile: async (repoId: string) => {
    if (!repos.some((r) => r.id === repoId)) throw new ApiError(404, "repo not found");
    return delay({ packages: [...(repoToolProfiles.get(repoId) ?? [])] });
  },
  setRepoToolProfile: async (repoId: string, packages: string[]) => {
    if (!repos.some((r) => r.id === repoId)) throw new ApiError(404, "repo not found");
    // Mirror the server's allowlist validation so the demo rejects the same way.
    const allowed = new Set<string>();
    for (const e of toolAllowlist) allowed.add(e.pinned_version ? `${e.name}@${e.pinned_version}` : e.name);
    const rejected = packages.filter((p) => !allowed.has(p));
    if (rejected.length > 0) throw new ApiError(400, "these packages are not on the allowlist: " + rejected.join(", "));
    const cleaned = [...new Set(packages)].sort();
    repoToolProfiles.set(repoId, cleaned);
    return delay({ packages: cleaned });
  },
  // PRD #66 M9 (D8): the admin cross-user blocked-repos list. DERIVED from the shared,
  // mutable repos state (not a static deep-copy) joined with mockBlockedRepoMeta for the
  // owner/reasons the wire Repo doesn't carry — so an Allow or Revoke on that shared
  // state (setRepoGuardrailOverride / clearRepoGuardrailOverride) round-trips into this
  // list, and the repo an admin allows here actually exists in the mutation's target.
  // block_messages is never null (always []), matching the server contract. Typed as
  // the AdminBlockedRepos envelope — the mock is a second implementation of that wire
  // contract, so the annotation keeps the two shapes in lockstep.
  adminListBlockedRepos: async (): Promise<AdminBlockedRepos> =>
    delay({
      checks_unknown: false,
      repos: repos
        .filter((r) => r.guardrail_blocked || r.guardrail_override)
        .map((r) => {
          const meta = mockBlockedRepoMeta[r.id];
          return {
            id: r.id,
            path: r.path_with_namespace,
            owner_id: meta?.owner_id ?? mockAdmin.id,
            owner_email: meta?.owner_email ?? mockAdmin.email,
            forge_type: meta?.forge_type ?? mockConnection.forge_type,
            blocked: r.guardrail_blocked,
            // Emit the underlying reasons only while the repo is actually blocked; an
            // overridden-clean repo lists [] (the override downgraded them). Never null.
            block_messages: r.guardrail_blocked ? [...(meta?.block_messages ?? [])] : [],
            guardrail_override: r.guardrail_override ? { ...r.guardrail_override } : null,
            privilege_status: meta?.privilege_status ?? mockConnection.privilege_status,
            privilege_checked_at: meta?.privilege_checked_at ?? mockConnection.privilege_checked_at,
          };
        }),
    }),
};
