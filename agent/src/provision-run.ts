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
import { errMessage } from "./util.js";

/** Reason prefix for a provisioning failure (the run fails, never degrades). */
export const REASON_PROVISION_FAILED = "tool provisioning failed before the agent could start";

// ctx.runId becomes a path segment under provisionRoot; reject anything not
// UUID-shaped so a malformed id can never traverse out (defense in depth — the id
// is a server-issued UUID).
const RUN_ID_RE = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;

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
 * packages ⇒ a no-op returning an empty env. A provision failure throws
 * (REASON_PROVISION_FAILED) so the run fails cleanly rather than degrading.
 */
export async function provisionRunTools(ctx: RunContext, deps: ProvisionRunDeps): Promise<ProvisionRunResult> {
  const provision = deps.provision ?? provisionTools;

  let toolPackages = ctx.config?.tool_packages ?? [];
  // Tier-2 (PRD #18 M5): when the repo owner opted in, union the repo's own
  // devbox.json packages (packages-only, shape-validated, hooks/scripts/flakes
  // ignored) with tier-1 — tier-1 wins version conflicts. Pure JSON extraction.
  if (ctx.config?.repo_devbox_opt_in) {
    const repoPackages = await extractRepoDevboxPackages(ctx.worktreePath);
    if (repoPackages.length > 0) {
      const before = toolPackages.length;
      toolPackages = mergeToolPackages(toolPackages, repoPackages);
      const added = toolPackages.length - before;
      if (added > 0) {
        ctx.emit({ kind: "status", agent: "worker", payload: { text: `merged ${added} package(s) from this repo's devbox.json` } });
      }
    }
  }

  if (toolPackages.length === 0) return { toolEnv: {} };
  if (!RUN_ID_RE.test(ctx.runId)) throw new Error(`${REASON_PROVISION_FAILED}: invalid run id`);

  const provisionDir = path.join(deps.provisionRoot, ctx.runId);
  ctx.emit({ kind: "status", agent: "worker", payload: { text: `provisioning ${toolPackages.length} tool(s): ${toolPackages.join(", ")}` } });
  try {
    const res = await provision({ packages: toolPackages, runDir: provisionDir, homeDir: deps.homeDir }, { log: deps.log });
    ctx.emit({ kind: "status", agent: "worker", payload: { text: "tools provisioned" } });
    return { toolEnv: res.toolEnv, provisionDir };
  } catch (err) {
    await fs.rm(provisionDir, { recursive: true, force: true }).catch(() => undefined);
    throw new Error(`${REASON_PROVISION_FAILED}: ${errMessage(err)}`);
  }
}
