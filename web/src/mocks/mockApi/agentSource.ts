import type { AgentSourceApplyResult, AgentSourceView } from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import { mockAgentSource } from "../data";
import { delay, requireSession } from "./shared";

// Agent-source demo state (PRD #602 M5): a deep clone so mutations (config save,
// sync, apply) do not leak back into the shared fixture. The config half is edited
// through updateSettings' agent_source_* arm; the status/staged halves by sync/apply.
export let agentSource: AgentSourceView = structuredClone(mockAgentSource);

// Persisted REMOTE facts from the last update check (PRD #702 M4), mirroring the
// server's persist-facts/derive-at-read split: the update-check writes these, and
// getAgentSource DERIVES update_available/latest_ref from them + the LIVE config at
// read time — so a pin bump or apply self-clears the badge with no new egress. Null
// until a check has run, so a fresh install shows no badge.
let agentSourceRemote: { latestRef: string; tipSha: string; checkedAt: string } | null = null;

// parseAgentSourceSemver mirrors the server's re-prefix + IsValid guard (Decision 4):
// a non-`v`-prefixed or malformed ref is not a comparable semver (null), never treated
// as equal. Returns the [major, minor, patch] triple for a valid vX.Y.Z, else null.
function parseAgentSourceSemver(ref: string): [number, number, number] | null {
  const m = /^v?(\d+)\.(\d+)\.(\d+)(?:[-+].*)?$/.exec(ref.trim());
  if (!m) return null;
  return [Number(m[1]), Number(m[2]), Number(m[3])];
}

// compareAgentSourceSemver returns >0 when a is newer than b (numeric per-component,
// so v1.10.0 sorts ABOVE v1.2.0 — a lexical compare gets this wrong, the discriminating
// case Decision 4 calls out).
function compareAgentSourceSemver(a: [number, number, number], b: [number, number, number]): number {
  for (let i = 0; i < 3; i++) {
    if (a[i] !== b[i]) return a[i] - b[i];
  }
  return 0;
}

// deriveAgentSourceStatus computes the update-available fields at read time from the
// stored remote facts + the live config (Decision 6), mirroring the server:
//   - no check has run (remote null) → no update fields (no badge);
//   - 40-hex SHA pin → never an update;
//   - tag-pinned (valid semver ref) → update iff the newest remote semver tag is strictly
//     greater, naming it in latest_ref;
//   - branch-pinned/empty ref → update ("moved", latest_ref empty) iff the remote tip
//     differs from last_applied_sha.
function deriveAgentSourceStatus(): AgentSourceView["status"] {
  const base = agentSource.status;
  const remote = agentSourceRemote;
  if (!remote) return base;
  const ref = agentSource.config.ref.trim();
  let updateAvailable = false;
  let latestRef = "";
  if (/^[0-9a-f]{40}$/i.test(ref)) {
    // SHA-pinned: an immutable pin is intentionally frozen — no signal.
    updateAvailable = false;
  } else {
    const pinned = parseAgentSourceSemver(ref);
    if (pinned) {
      const cand = parseAgentSourceSemver(remote.latestRef);
      if (cand && compareAgentSourceSemver(cand, pinned) > 0) {
        updateAvailable = true;
        latestRef = remote.latestRef;
      }
    } else if (
      remote.tipSha &&
      remote.tipSha.toLowerCase() !== (base.last_applied_sha ?? "").toLowerCase()
    ) {
      // branch-pinned/empty: the advertised tip moved past what is applied. Compared
      // case-INSENSITIVELY to mirror the server's DeriveUpdate (strings.EqualFold), so a
      // mixed-case fixture can't drift the mock from the real derive.
      updateAvailable = true;
    }
  }
  return {
    ...base,
    update_available: updateAvailable,
    latest_ref: latestRef || undefined,
    update_checked_at: remote.checkedAt,
  };
}

// agentSourceView returns the current view with the update-available fields DERIVED at
// read time (Decision 6) — every read path returns this, so the badge is consistent.
function agentSourceView(): AgentSourceView {
  const clone = structuredClone(agentSource);
  clone.status = deriveAgentSourceStatus();
  return clone;
}

// mockAllowedAgentSourceHosts mirrors the server's SSRF allowlist: a save of a URL
// whose host is not on it is rejected with the same shaped 400 the real
// UpdateSettings surfaces. Empty URL (disable) is always allowed.
export const mockAllowedAgentSourceHosts = ["github.com", "gitlab.com"];

export const agentSourceApi = {
  // ── Agent source (PRD #602 M5) ───────────────────────────────────────────────
  getAgentSource: async () => {
    requireSession();
    // Derive update_available/latest_ref at read time from the stored remote facts +
    // the live config (PRD #702 M4), so a bump/apply self-clears the badge with no egress.
    return delay({ agent_source: agentSourceView() });
  },
  // "Sync now": re-runs the reconcile. In the mock it refreshes the sync timestamp
  // and marks the fetch healthy, leaving the staged snapshot pending for review. A
  // sync against no configured URL records an error rather than staging (mirrors the
  // server, which never fetches an empty URL).
  syncAgentSource: async () => {
    requireSession();
    const now = new Date().toISOString();
    if (agentSource.config.url.trim() === "") {
      agentSource.status = {
        ...agentSource.status,
        last_sync_at: now,
        last_sync_status: "error",
        last_sync_error: "No repository URL is configured.",
      };
      agentSource.staged = null;
    } else {
      agentSource.status = {
        ...agentSource.status,
        last_sync_at: now,
        last_sync_status: "ok",
        last_sync_error: undefined,
      };
      if (agentSource.staged) agentSource.staged.fetched_at = now;
    }
    return delay({ agent_source: agentSourceView() });
  },
  // Approve-and-apply. expected_sha binds the reviewed snapshot: a mismatch (or
  // nothing pending) is the 409 the real handler returns for ErrStaleApproval, so the
  // admin re-reviews rather than applying blind.
  applyAgentSource: async (expectedSha: string) => {
    requireSession();
    const staged = agentSource.staged;
    if (!staged || !staged.pending || staged.fetched_sha !== expectedSha) {
      throw new ApiError(409, "the staged snapshot changed since you reviewed it; re-review before applying");
    }
    // Tally the outcome from the diff, mirroring agentsource.ApplyResult.
    let applied = 0;
    let unchanged = 0;
    let conflicts = 0;
    let deprovisioned = 0;
    for (const d of staged.diff) {
      if (d.action === "add" || d.action === "override") applied++;
      else if (d.action === "unchanged") unchanged++;
      else if (d.action === "conflict") conflicts++;
      else if (d.action === "remove") deprovisioned++;
    }
    const skippedParse = staged.roles.filter((r) => !r.ok).length;
    const now = new Date().toISOString();
    agentSource.status = {
      ...agentSource.status,
      last_applied_at: now,
      last_applied_sha: staged.fetched_sha,
    };
    staged.pending = false;
    const result: AgentSourceApplyResult = {
      sha: staged.fetched_sha,
      applied,
      unchanged,
      conflicts,
      deprovisioned,
      skipped_parse: skippedParse,
      already_applied: false,
      message: "applied",
    };
    return delay({ result });
  },

  // Resolve the latest semver tag for a supplied (possibly unsaved) URL (PRD #702 M3).
  // Mirrors the real endpoint: an empty URL is a 400, a host off the SSRF allowlist is
  // the same 400 the ls-remote path refuses with (the resolve is anonymous, so it only
  // works against a public, allowlisted source). On success it returns a plausible
  // semver tag — v1.10.0 for the canonical skills repo, which sorts ABOVE a lexical
  // v1.2.0 (the discriminating fixture). Any other allowlisted URL returns the same.
  resolveAgentSourceLatest: async (url: string) => {
    requireSession();
    const trimmed = url.trim();
    if (trimmed === "") throw new ApiError(400, "url is required");
    let host = "";
    try {
      const parsed = new URL(trimmed);
      if (parsed.protocol !== "https:") throw new Error("scheme");
      host = parsed.hostname;
    } catch {
      throw new ApiError(400, "URL is not in the agent-source allowlist");
    }
    if (!mockAllowedAgentSourceHosts.includes(host)) {
      throw new ApiError(400, "URL is not in the agent-source allowlist");
    }
    return delay({ latest_ref: "v1.10.0" });
  },

  // Update check (PRD #702 M4): ls-remotes the CONFIGURED source with its sealed
  // credential and PERSISTS the remote facts (latest semver tag + tip sha + checked-at),
  // mirroring the server's persist-facts split. getAgentSource then DERIVES
  // update_available/latest_ref from these + the live config, so the badge self-clears
  // after a bump. The mock simulates a source ahead of the default v1.2.0 pin: a latest
  // tag of v1.10.0 (which sorts above v1.2.0 — the discriminating fixture) and a moved tip.
  updateCheckAgentSource: async () => {
    requireSession();
    const now = new Date().toISOString();
    if (agentSource.config.url.trim() === "") {
      // No configured source to check — record the failure in the last_sync_error slot
      // the update check reuses (per the server), so the card surfaces it.
      agentSource.status = {
        ...agentSource.status,
        last_sync_status: "error",
        last_sync_error: "No repository URL is configured.",
      };
      return delay({ agent_source: agentSourceView() });
    }
    // Persist the remote facts; the derived fields are computed at read time.
    agentSourceRemote = {
      latestRef: "v1.10.0",
      tipSha: "a1b2c3d4e5f60718293a",
      checkedAt: now,
    };
    agentSource.status = {
      ...agentSource.status,
      last_sync_status: agentSource.status.last_sync_status ?? "ok",
      last_sync_error: undefined,
    };
    return delay({ agent_source: agentSourceView() });
  },
};
