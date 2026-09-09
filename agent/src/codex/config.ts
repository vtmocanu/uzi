// PRD #1156 (M3a) — the STOCK, native-DISABLED Codex app-server config builder.
//
// This reproduces the environment-replacement stock `config.toml` characterized by
// the frozen M0 fixture (`e2e/codex-m0/harness.mjs:208-248`), MINUS the two
// fixture-only affordances that must never reach production:
//
//   * the fixture sets `hooks = true` and passes `--dangerously-bypass-hook-trust`
//     / a per-thread `bypass_hook_trust` so its own probe hooks arm. Production
//     ships hooks OFF, so there is nothing to bypass: `hooks = false`, and NO
//     `bypass_hook_trust`, NO `--dangerously-bypass-hook-trust` anywhere.
//   * the fixture accepts arbitrary caller-shaped knobs (code_mode / unified_exec /
//     multi_agent variables, `write_stdin_approval`). The generic M3a launcher builder
//     accepts only a trusted provider + model + canonical project path and pins every
//     native surface disabled. The production authentication builder below is narrower:
//     it fixes the built-in OpenAI provider and accepts no endpoint/env-key injection.
//
// The ADR (`adr/1106-codex-harness.md:337-344`) requires the canonical project be
// provisioned EXPLICITLY `untrusted` with `project_doc_max_bytes = 0`: pinned
// 0.153.2 `thread_processor.rs` promotes trust when `trust_level.is_none()`, so an
// unset value is unsafe. We emit an explicit `[projects."<cwd>"] trust_level =
// "untrusted"` to avoid that branch.

import { lstatSync } from "node:fs";

import type { CodexAppServerAuthMode } from "./appserver-auth.js";

/**
 * A model provider the stock config points Codex at. `wireApi` is pinned to the
 * `responses` wire in M3a (the only characterized transport); `envKey` names the
 * environment variable Codex reads the credential from — the launcher, not this
 * builder, decides whether that variable is populated (provider) or absent (command).
 */
export interface CodexConfigProvider {
  readonly name: string;
  readonly baseUrl: string;
  readonly envKey: string;
  readonly wireApi: "responses";
}

/** Inputs to {@link buildCodexConfigToml}. Everything here is launcher-fixed / a
 *  trusted spec value; NONE of it is model- or repo-controlled. */
export interface CodexConfigOptions {
  readonly model: string;
  readonly provider: CodexConfigProvider;
  /** The canonical project (the launch cwd) provisioned EXPLICITLY untrusted. */
  readonly projectPath: string;
}

/** A provider name is used as a bare/quoted TOML table key AND as the
 *  `model_provider` reference; keep it a simple identifier so the two always agree
 *  and no injection is possible through the table header. */
const PROVIDER_NAME_RE = /^[A-Za-z0-9._-]+$/;

/** TOML basic-string encoder. TOML basic strings share JSON's escaping for the
 *  characters that occur in paths/URLs/identifiers, so `JSON.stringify` yields a
 *  valid quoted TOML string (the frozen fixture relies on the same equivalence at
 *  `harness.mjs:209`). */
function toml(value: string): string {
  return JSON.stringify(value);
}

function nativeDisabledConfigLines(model: string, providerName: string, projectPath: string): string[] {
  return [
    `model = ${toml(model)}`,
    `model_provider = ${toml(providerName)}`,
    `project_doc_max_bytes = 0`,
    `check_for_update_on_startup = false`,
    `web_search = "disabled"`,
    ``,
    `[analytics]`,
    `enabled = false`,
    ``,
    `[feedback]`,
    `enabled = false`,
    ``,
    `[agents]`,
    `enabled = false`,
    ``,
    `[features]`,
    `apps = false`,
    `plugins = false`,
    `shell_snapshot = false`,
    `shell_snapshot_v2 = false`,
    `code_mode = false`,
    `code_mode_only = false`,
    `code_mode_host = false`,
    `code_mode_prewarm = false`,
    `remote_models = false`,
    `unified_exec = false`,
    `hooks = false`,
    `multi_agent = false`,
    `multi_agent_v2 = false`,
    `enable_request_compression = false`,
    ``,
    // Explicit untrusted state for the canonical project (ADR:337-344). A quoted
    // key so any path is represented safely.
    `[projects.${toml(projectPath)}]`,
    `trust_level = "untrusted"`,
    ``,
  ];
}

/**
 * Build the `config.toml` TEXT for a stock, native-DISABLED Codex app-server.
 *
 * Every native surface is pinned to its stock disabled value: `project_doc_max_bytes
 * = 0`, `web_search = "disabled"`, analytics/feedback/agents off, the whole
 * `[features]` family (apps / plugins / shell_snapshot* / code_mode* / remote_models
 * / unified_exec / hooks / multi_agent* / enable_request_compression) off, and
 * `check_for_update_on_startup = false`. The canonical project is provisioned
 * explicitly `untrusted`. The provider block carries zero retries and no websockets.
 */
export function buildCodexConfigToml(opts: CodexConfigOptions): string {
  const { model, provider, projectPath } = opts;
  if (!PROVIDER_NAME_RE.test(provider.name)) {
    throw new Error(`Codex provider name must match ${PROVIDER_NAME_RE} (got ${JSON.stringify(provider.name)})`);
  }
  if (provider.wireApi !== "responses") {
    throw new Error(`Codex M3a supports only the "responses" wire (got ${JSON.stringify(provider.wireApi)})`);
  }
  // A single deterministic template — no caller-shaped branches, no repo config.
  return [
    ...nativeDisabledConfigLines(model, provider.name, projectPath),
    `[model_providers.${toml(provider.name)}]`,
    `name = ${toml(provider.name)}`,
    `base_url = ${toml(provider.baseUrl)}`,
    `env_key = ${toml(provider.envKey)}`,
    `wire_api = "responses"`,
    `supports_websockets = false`,
    `requires_openai_auth = false`,
    `request_max_retries = 0`,
    `stream_max_retries = 0`,
    ``,
  ].join("\n");
}

/** Inputs for the production app-server-auth config. There is deliberately no provider
 * endpoint/name/env-key field: pinned Codex's built-in `openai` provider owns those
 * values, and chooses its backend from the authenticated mode. */
export interface CodexProductionConfigOptions {
  readonly model: string;
  readonly projectPath: string;
  readonly authMode: CodexAppServerAuthMode;
}

/**
 * Build the fixed production config used with `account/login/start` authentication.
 *
 * Pinned 0.153.2's built-in `openai` provider has `requires_openai_auth = true` and no
 * configured base URL. That absence is load-bearing: API-key auth selects
 * `https://api.openai.com/v1`, while `chatgptAuthTokens` selects the ChatGPT Codex
 * backend. Pinned 0.153.2 merges configured providers with `or_insert`, so a caller cannot
 * override the built-in `openai` provider with a partial table. Its production request and
 * stream retry defaults therefore remain active by design; the zero-retry custom-provider
 * settings belong only to deterministic fake-provider tests.
 *
 * `authMode` is required even though the TOML is identical for both modes. It forces the
 * trusted composition to make an explicit no-fallback choice, which the auth session then
 * enforces on the protocol. Unknown runtime keys are rejected so an endpoint cannot be
 * smuggled into this production builder from a claim, repo or test fixture.
 */
export function buildCodexProductionConfigToml(opts: CodexProductionConfigOptions): string {
  const keys = Object.keys(opts);
  if (keys.some((key) => key !== "model" && key !== "projectPath" && key !== "authMode")) {
    throw new Error("Codex production config received an unsupported option");
  }
  if (opts.authMode !== "api_key" && opts.authMode !== "subscription") {
    throw new Error("Codex production config requires an explicit supported auth mode");
  }
  return nativeDisabledConfigLines(opts.model, "openai", opts.projectPath).join("\n");
}

/** Thrown when {@link assertNoUnexpectedSystemConfig} finds a system config root the
 *  shipped images never install; a fresh HOME does NOT neutralize `/etc/codex`. */
export class CodexUnexpectedSystemConfigError extends Error {
  constructor(path: string) {
    super(
      `Unexpected system Codex configuration at ${path}: the shipped worker images install none, `
      + `and a fresh HOME/CODEX_HOME does not neutralize a system config root. Refusing to launch.`,
    );
    this.name = "CodexUnexpectedSystemConfigError";
  }
}

/**
 * Fail-closed guard: reject (throw) if a system Codex config root EXISTS. The
 * shipped images install no `/etc/codex`; a present one is unexpected system config
 * that a fresh per-launch HOME/CODEX_HOME does not override, so we refuse rather than
 * launch against it. The path is injectable so unit tests point it at a temp dir.
 *
 * A broken symlink at the path also counts as "present" (lstat, not stat) — the same
 * discipline the M0 fixture applies at `harness.mjs:122`.
 */
export function assertNoUnexpectedSystemConfig(etcCodexDir = "/etc/codex"): void {
  try {
    lstatSync(etcCodexDir);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return; // absent — the expected shipped state
    throw error; // an unexpected stat error (EACCES, ELOOP, …) is itself a refusal to launch blind
  }
  throw new CodexUnexpectedSystemConfigError(etcCodexDir);
}
