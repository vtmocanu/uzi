// Shared tool-provisioning step (PRD #18 M3/M8). Extracted so BOTH executors run
// identical provisioning with no drift: the SDK executor folds the resulting tool
// env into the SDK env, and the stub executor exercises the same install path in
// the E2E (against a stubbed devbox, no substituter egress). The discipline is
// unchanged — synthesize a packages-only devbox.json OUTSIDE the clone, install in
// a secret-scrubbed subprocess, and filter `devbox shellenv` to the allowlist.

import fs from "node:fs/promises";
import path from "node:path";
import type { Logger } from "./log.js";
import type { RunContext } from "./executor.js";
import { provisionTools } from "./provision.js";
import { extractRepoDevboxPackages, mergeToolPackages } from "./repo-tools.js";
import { errMessage, RUN_ID_RE } from "./util.js";

/** Reason prefix for a FATAL provisioning failure. Tier-1 (uzi-stored) failure
 *  fails the run with this reason; a tier-2 (repo opt-in extra) failure does NOT —
 *  it degrades to a run-feed warning and a retry with tier-1 only. */
export const REASON_PROVISION_FAILED = "tool provisioning failed before the agent could start";

// ctx.runId becomes a path segment under provisionRoot; RUN_ID_RE (util.ts) rejects
// anything not UUID-shaped so a malformed id can never traverse out (defense in
// depth — the id is a server-issued UUID). Same guard the runner applies to the
// per-run HOME.

export interface ProvisionRunDeps {
  /** Root for per-run provisioning dirs, OUTSIDE any clone (Decision 3). */
  provisionRoot: string;
  /** HOME for the install subprocess (nix profile + devbox state, data volume). */
  homeDir: string;
  log: Logger;
  /** Injected in tests so no real devbox/nix egress happens; default = provisionTools. */
  provision?: typeof provisionTools;
}

export interface ProvisionRunResult {
  /** Allowlisted tool env to fold into the SDK env (empty when no packages). */
  toolEnv: Record<string, string>;
  /** The per-run dir to clean up after the run (undefined when nothing was provisioned). */
  provisionDir?: string;
}

/**
 * Resolve the run's tier-1 (∪ opted-in tier-2) package set and provision it,
 * emitting the same run-stream status messages regardless of executor. No
 * packages ⇒ a no-op returning an empty env.
 *
 * Failure handling is split by tier (PRD #278 M2, option b):
 *   - Tier-1 (uzi-stored) provisioning failure is FATAL — it throws
 *     REASON_PROVISION_FAILED so the run fails cleanly.
 *   - A tier-2 (repo opt-in extra) failure DEGRADES: the merged install is
 *     best-effort. If it fails and repo extras were in the set, emit a run-feed
 *     warning naming the dropped extras and retry with tier-1 only (skip the repo
 *     extras). The retry keeps tier-1 fatal.
 */
export async function provisionRunTools(ctx: RunContext, deps: ProvisionRunDeps): Promise<ProvisionRunResult> {
  const provision = deps.provision ?? provisionTools;

  const tier1 = ctx.config?.tool_packages ?? [];
  let toolPackages = tier1;
  let tier2Added = 0;
  // Tier-2 (PRD #18 M5): when the repo owner opted in, union the repo's own
  // devbox.json packages (packages-only, shape-validated, hooks/scripts/flakes
  // ignored) with tier-1 — tier-1 wins version conflicts. Extraction is a
  // comment-tolerant (JSONC) parse; nothing in the manifest is executed.
  if (ctx.config?.repo_devbox_opt_in) {
    const repoPackages = await extractRepoDevboxPackages(ctx.worktreePath);
    if (repoPackages.length > 0) {
      toolPackages = mergeToolPackages(tier1, repoPackages);
      // mergeToolPackages preserves tier-1 order then appends surviving tier-2
      // entries, so anything beyond tier1.length is a tier-2-only add.
      tier2Added = toolPackages.length - tier1.length;
      if (tier2Added > 0) {
        ctx.emit({ kind: "status", agent: "worker", payload: { text: `merged ${tier2Added} package(s) from this repo's devbox.json` } });
      }
    }
  }

  if (toolPackages.length === 0) return { toolEnv: {} };
  if (!RUN_ID_RE.test(ctx.runId)) throw new Error(`${REASON_PROVISION_FAILED}: invalid run id`);

  const provisionDir = path.join(deps.provisionRoot, ctx.runId);

  // Provision `packages` against provisionDir, emitting the run-feed status
  // messages. provisionTools re-creates the dir and writes a fresh packages-only
  // manifest on every call, so re-running against the same dir is self-contained;
  // the catch below also rm's the dir before any retry so no stale
  // devbox.json/lock from the failed attempt lingers.
  const install = async (packages: string[]): Promise<ProvisionRunResult> => {
    ctx.emit({ kind: "status", agent: "worker", payload: { text: `provisioning ${packages.length} tool(s): ${packages.join(", ")}` } });
    const res = await provision({ packages, runDir: provisionDir, homeDir: deps.homeDir }, { log: deps.log });
    ctx.emit({ kind: "status", agent: "worker", payload: { text: "tools provisioned" } });
    return { toolEnv: res.toolEnv, provisionDir };
  };

  try {
    return await install(toolPackages);
  } catch (err) {
    await fs.rm(provisionDir, { recursive: true, force: true }).catch(() => undefined);
    if (tier2Added > 0) {
      // The failed merged set carried this repo's opt-in extras — DEGRADE rather
      // than fail the run. Phrase the warning causation-neutrally: the extras may
      // not be what failed (a tier-1 package in the merged set could be), and a
      // genuine tier-1 failure still surfaces fatally via the tier-1-only retry
      // below. So we say the merged set failed and what we do next, not who caused it.
      const tier2Only = toolPackages.slice(tier1.length);
      ctx.emit({
        kind: "status",
        agent: "worker",
        payload: {
          text: `tool provisioning failed with this repo's opt-in extra tool(s) (${tier2Only.join(", ")}); ${tier1.length === 0 ? "skipping them" : "retrying without them"}: ${errMessage(err)}`,
        },
      });
      if (tier1.length === 0) return { toolEnv: {} };
      // Retry with tier-1 only — tier-1 stays fatal.
      try {
        return await install(tier1);
      } catch (retryErr) {
        await fs.rm(provisionDir, { recursive: true, force: true }).catch(() => undefined);
        throw new Error(`${REASON_PROVISION_FAILED}: ${errMessage(retryErr)}`);
      }
    }
    // Pure tier-1 (or opt-in off) — fatal, exactly as before.
    throw new Error(`${REASON_PROVISION_FAILED}: ${errMessage(err)}`);
  }
}
