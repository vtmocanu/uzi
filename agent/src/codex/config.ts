// PRD #1156 (M3a) — fixed Codex config; execution denial also needs protocol/launcher controls.
// #1566: async questions and read-only UTC remain reachable; the run code host routes callbacks.
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
//     accepts only a trusted provider + model + canonical project path and pins the
//     supported suppression settings. The production authentication builder below is narrower:
//     it fixes the built-in OpenAI provider and accepts no endpoint/env-key injection.
//
// #4509, measured at 0.167.0 (pin 0.169.0, not re-measured): apply_patch_freeform is removed/ignored; model catalog metadata
// overrides code_mode/code_mode_only. Native apply_patch remains advertised under the
// code-mode host. Registration is not execution authority: explicit environments: []
// at root thread/start and turn/start (including child, advice, cross-check and cold
// resumed turns), plus launcher CODEX_EXEC_SERVER_URL=none in the replaced environment,
// deny native execution. thread/resume has no environments field; the following turn
// reasserts []. Worker callbacks remain subject to the broker, not these feature flags.
//
// ADR-1106 requires the canonical project be
// provisioned EXPLICITLY `untrusted` with `project_doc_max_bytes = 0`: characterized
// 0.160.1 `thread_processor.rs` (unchanged since 0.160.0) promotes trust when
// `trust_level.is_none()` (and, since 0.156, the
// config is not projectless), so an
// unset value is unsafe. We emit an explicit `[projects."<cwd>"] trust_level =
// "untrusted"` to avoid that branch.

import { TrustedExecutionRefusal } from "../trusted-execution-refusal.js";
import { lstatSync } from "node:fs";
import { createHash } from "node:crypto";
import { decodeProviderBinding } from "../provider-binding.js";
import type { ClaimCodexProviderBinding } from "../protocol.js";

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
/** One fixed provider identity shared by the M3b config and thread/start seam. */
export const CODEX_M3B_LOOPBACK_PROVIDER_NAME = "uzi-m3b-openai";

/** TOML basic-string encoder. TOML basic strings share JSON's escaping for the
 *  characters that occur in paths/URLs/identifiers, so `JSON.stringify` yields a
 *  valid quoted TOML string (the frozen fixture relies on the same equivalence at
 *  `harness.mjs:209`). */
function toml(value: string): string {
  return JSON.stringify(value);
}

function nativeDisabledConfigLines(
  model: string,
  providerName: string,
  projectPath: string,
  codeModeHost: boolean,
): string[] {
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
    `goals = false`,
    `apps = false`,
    `plugins = false`,
    `shell_tool = false`,
    `view_image = false`,
    `sleep_tool = false`,
    `apply_patch_freeform = false`,
    `shell_snapshot = false`,
    `shell_snapshot_v2 = false`,
    `code_mode = false`,
    `code_mode_only = false`,
    `code_mode_host = ${codeModeHost}`,
    `code_mode_prewarm = false`,
    `unified_exec = false`,
    `hooks = false`,
    `multi_agent = false`,
    `multi_agent_v2 = false`,
    `enable_request_compression = false`,
    ``,
    // Explicit untrusted state for the canonical project (ADR-1106). A quoted
    // key so any path is represented safely.
    `[projects.${toml(projectPath)}]`,
    `trust_level = "untrusted"`,
    ``,
  ];
}

/**
 * Build the fixed `config.toml` TEXT (#1566 exceptions above).
 *
 * The supported controls suppress native extensions/hooks, project docs, web
 * search, analytics, feedback, agents and startup
 * update checks. The canonical project is explicitly `untrusted`; the provider
 * block carries zero retries and no websockets.
 *
 * `remote_models` remains a removed, useless
 * compatibility flag, so it is omitted. Model metadata may refresh under the
 * supported runtime authentication/discovery/cache policy; uzi's curated picker
 * and validation policy remain independent of that metadata refresh.
 * Upstream RemoteModels and Stage::Removed documentation:
 * https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/features/src/lib.rs
 * Upstream should_refresh_models and refresh_after_auth_change:
 * https://github.com/openai/codex/blob/83086188ae0614bd882818ff1aa23f3194962012/codex-rs/models-manager/src/manager.rs
 */
export function buildCodexConfigToml(opts: CodexConfigOptions): string {
  const { model, provider, projectPath } = opts;
  if (!PROVIDER_NAME_RE.test(provider.name)) {
    throw new TrustedExecutionRefusal(`Codex provider name must match ${PROVIDER_NAME_RE} (got ${JSON.stringify(provider.name)})`);
  }
  if (provider.wireApi !== "responses") {
    throw new TrustedExecutionRefusal(`Codex M3a supports only the "responses" wire (got ${JSON.stringify(provider.wireApi)})`);
  }
  // A single deterministic template — no caller-shaped branches, no repo config.
  return [
    ...nativeDisabledConfigLines(model, provider.name, projectPath, false),
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
  /** A trusted, launcher-fixed value (never model/repo/claim input) that enables ONLY the
   *  code-mode execution host; absent/false keeps the host disabled. */
  readonly codeModeHost?: boolean;
}

function validateProductionConfigOptions(opts: CodexProductionConfigOptions): void {
  const keys = Object.keys(opts);
  if (keys.some((key) => key !== "model" && key !== "projectPath" && key !== "authMode" && key !== "codeModeHost")) {
    throw new TrustedExecutionRefusal("Codex production config received an unsupported option");
  }
  if (opts.authMode !== "api_key" && opts.authMode !== "subscription") {
    throw new TrustedExecutionRefusal("Codex production config requires an explicit supported auth mode");
  }
  if (opts.codeModeHost !== undefined && typeof opts.codeModeHost !== "boolean") {
    throw new TrustedExecutionRefusal("Codex production config codeModeHost must be a boolean");
  }
}

/**
 * Build the fixed production config used with `account/login/start` authentication.
 *
 * The built-in `openai` provider has `requires_openai_auth = true` and no
 * configured base URL. That absence is load-bearing: API-key auth selects
 * `https://api.openai.com/v1`, while `chatgptAuthTokens` selects the ChatGPT Codex
 * backend. The runtime merges configured providers with `or_insert`, so a caller cannot
 * override the built-in `openai` provider with a partial table. Its production request and
 * stream retry defaults therefore remain active by design; the zero-retry custom-provider
 * settings are supplied by the separate custom-provider builder when using a binding.
 *
 * `authMode` is required even though the TOML is identical for both modes. It forces the
 * trusted composition to make an explicit no-fallback choice, which the auth session then
 * enforces on the protocol. Unknown runtime keys are rejected so an endpoint cannot be
 * smuggled into this production builder from a claim, repo or test fixture.
 */
export function buildCodexProductionConfigToml(opts: CodexProductionConfigOptions): string {
  validateProductionConfigOptions(opts);
  return nativeDisabledConfigLines(opts.model, "openai", opts.projectPath, opts.codeModeHost ?? false).join("\n");
}

/** Trusted provider-only inputs. Model selects a map slot, never an arbitrary gateway ID.
 * Destination authority remains with the API; decoding here checks shape only. */
export interface CodexProviderConfigOptions {
  readonly binding: ClaimCodexProviderBinding;
  readonly model?: keyof ClaimCodexProviderBinding["model_map"];
  readonly projectPath: string;
  readonly codeModeHost?: boolean;
}

/** Build managed-login Responses HTTP config without granting execution authority.
 * The credential is delivered separately through account/login/start, never TOML. */
export function buildCodexProviderConfigToml(opts: CodexProviderConfigOptions): string {
  if (Object.keys(opts).some(key => !["binding", "model", "projectPath", "codeModeHost"].includes(key))) {
    throw new TrustedExecutionRefusal("Codex provider config received an unsupported option");
  }
  if (typeof opts.projectPath !== "string"
    || (opts.codeModeHost !== undefined && typeof opts.codeModeHost !== "boolean")) {
    throw new TrustedExecutionRefusal("Codex provider config requires a project path and boolean codeModeHost");
  }
  const binding = decodeProviderBinding({ provider: opts.binding });
  if (binding?.engine !== "codex") {
    throw new TrustedExecutionRefusal("Codex provider config requires a Codex binding");
  }
  // Unknown config/header/identity fields are refused, rather than forwarded.
  if (Object.keys(opts.binding).some(key => ![
    "provider_id", "revision", "engine", "protocol", "base_url", "auth_style", "credential", "model_map",
  ].includes(key))) {
    throw new TrustedExecutionRefusal("Codex provider binding received an unsupported option");
  }
  const slot = opts.model ?? "default";
  if (!Object.hasOwn(binding.model_map, slot)) {
    throw new TrustedExecutionRefusal("Codex provider model slot is unmapped");
  }
  const entry = binding.model_map[slot]!;
  if (Object.keys(opts.binding.model_map[slot]!).some(key => key !== "model" && key !== "context_window")) {
    throw new TrustedExecutionRefusal("Codex provider model received an unsupported option");
  }
  // Hash only non-secret authority identifiers; caller text never becomes a table identity.
  const identity = createHash("sha256").update(JSON.stringify([binding.provider_id, binding.revision])).digest("hex");
  const providerName = `uzi-provider-${identity}`;
  return [
    ...(entry.context_window === undefined ? [] : [`model_context_window = ${entry.context_window}`]),
    ...nativeDisabledConfigLines(entry.model, providerName, opts.projectPath, opts.codeModeHost ?? false),
    `[model_providers.${toml(providerName)}]`,
    `name = ${toml(providerName)}`,
    `base_url = ${toml(binding.base_url)}`,
    'wire_api = "responses"',
    "supports_websockets = false",
    "requires_openai_auth = true",
    "request_max_retries = 0",
    "stream_max_retries = 0",
    "",
  ].join("\n");
}

/**
 * Build an authenticated custom-provider config for the in-container M3b fake. This
 * is deliberately separate from {@link buildCodexProductionConfigToml}: production
 * vendor production keeps the built-in `openai` provider and emits no endpoint override.
 *
 * The built-in provider has WebSockets enabled and cannot be
 * overridden by a same-name provider table (`merge_configured_model_providers` uses
 * `or_insert`). Its app-server tests instead use a distinct custom provider with
 * `requires_openai_auth = true` and `supports_websockets = false`; this reproduces
 * that exact HTTP-fake shape while preserving `account/login/start`. Keep the URL
 * narrower than a general provider seam so a test dependency can never redirect
 * credentials to a network peer.
 */
export function buildCodexLoopbackTestConfigToml(
  opts: CodexProductionConfigOptions,
  openAIBaseUrl: string,
): string {
  let parsed: URL;
  try {
    parsed = new URL(openAIBaseUrl);
  } catch {
    throw new TrustedExecutionRefusal("Codex loopback test base URL is invalid");
  }
  const normalizedBaseUrl = `http://127.0.0.1:${parsed.port}/v1`;
  if (
    parsed.protocol !== "http:"
    || parsed.hostname !== "127.0.0.1"
    || parsed.port === ""
    || parsed.pathname !== "/v1"
    || parsed.username !== ""
    || parsed.password !== ""
    || parsed.search !== ""
    || parsed.hash !== ""
    || openAIBaseUrl !== normalizedBaseUrl
  ) {
    throw new TrustedExecutionRefusal("Codex loopback test base URL must be http://127.0.0.1:<port>/v1 with no credentials or query");
  }
  validateProductionConfigOptions(opts);
  const providerName = CODEX_M3B_LOOPBACK_PROVIDER_NAME;
  return [
    ...nativeDisabledConfigLines(opts.model, providerName, opts.projectPath, opts.codeModeHost ?? false),
    `[model_providers.${toml(providerName)}]`,
    `name = "OpenAI"`,
    `base_url = ${toml(normalizedBaseUrl)}`,
    `wire_api = "responses"`,
    `supports_websockets = false`,
    `requires_openai_auth = true`,
    `request_max_retries = 0`,
    `stream_max_retries = 0`,
    ``,
  ].join("\n");
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
