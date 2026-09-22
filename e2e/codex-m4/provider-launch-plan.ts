// PRD #1287 C3 — keep the production provider-root contract distinct from the executable used by
// the P-layer's injected direct-spawn seam. Production always names the immutable image path; a
// rootless contributor/CI provisioner may execute the same verified package from its cache path.

import type { CodexLaunchSpec } from "../../agent/src/codex/launcher.js";

export interface ProviderLaunchContract {
  readonly CODEX_BIN: string;
  readonly SUPERVISOR_BIN: string;
  readonly PROVIDER_CHILD_ARGV: readonly string[];
}

export interface ProviderLaunchPlanInput {
  readonly executableBin: string;
  readonly provider: CodexLaunchSpec["provider"];
  readonly model: string;
  readonly cwd: string;
  readonly ownedDataRoot: string;
  /** Trusted, launcher-fixed (mirrors {@link CodexLaunchSpec.codeModeHost}): when true the
   *  app-server-auth loopback config emits `code_mode_host = true`, enabling the real code-mode
   *  execution host (PRD #1533). Absent/false keeps the host disabled, byte-identical to before. */
  readonly codeModeHost?: boolean;
}

export interface ProviderLaunchPlan {
  readonly spec: CodexLaunchSpec;
  readonly executableBin: string;
}

/** Build the immutable production spec plus the separately verified P-layer test executable. */
export function buildProviderLaunchPlan(
  contract: ProviderLaunchContract,
  input: ProviderLaunchPlanInput,
): ProviderLaunchPlan {
  return {
    spec: {
      kind: "provider",
      provider: input.provider,
      model: input.model,
      codexBin: contract.CODEX_BIN,
      supervisorBin: contract.SUPERVISOR_BIN,
      childArgv: [...contract.PROVIDER_CHILD_ARGV],
      cwd: input.cwd,
      ownedDataRoot: input.ownedDataRoot,
      useAppServerAuth: true,
      authMode: "api_key",
      // Only set when explicitly requested, so an absent value keeps the pre-#1533 spec shape.
      ...(input.codeModeHost !== undefined ? { codeModeHost: input.codeModeHost } : {}),
    },
    executableBin: input.executableBin,
  };
}
