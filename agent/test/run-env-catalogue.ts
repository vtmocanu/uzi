// The hostile catalogue of repository variables shared by the Claude and Codex environment tests
// (PRD 4416 M2): every allowed name the pinned SDK/CLI also reads, every denied name of the shared
// fixture, a sample for each denied prefix and suffix, and the package/build names that must be
// injected. Values are derived from the name so a leak identifies its source.

import { readFileSync } from "node:fs";
import type { RunEnvVar } from "../src/run-env-claim.js";
import { runEnvNameViolation } from "../src/run-env-names.js";

interface Fixture {
  denied_names: Array<{ value: string }>;
  denied_prefixes: Array<{ value: string }>;
  denied_suffixes: Array<{ value: string }>;
}
const fixture = JSON.parse(readFileSync(new URL("../../fixtures/run-env/name-rules.json", import.meta.url), "utf8")) as Fixture;

/** Names the pinned @anthropic-ai/claude-agent-sdk (0.3.319) reads through `process.env.NAME` and the
 *  rule set allows (found by grepping the *.mjs bundles, then dropping every denied name), plus the
 *  cloud, color and runtime names its bundles mention outside that spelling. */
export const SDK_READ_ALLOWED_NAMES: readonly string[] = [
  "ALACRITTY_LOG", "APP_URL", "AWS_EXECUTION_ENV", "AWS_LAMBDA_FUNCTION_NAME", "AZURE_FUNCTIONS_ENVIRONMENT", "BUILDKITE", "C9_PID",
  "C9_USER", "CF_PAGES", "CI", "CIRCLECI", "CODER", "CODER_WORKSPACE_NAME", "CODESPACES", "CURSOR_TRACE_ID", "DAYTONA_WS_ID", "DEBUG",
  "DENO_DEPLOYMENT_ID", "DEVPOD", "DEVPOD_WORKSPACE_UID", "DO_NOT_TRACK", "DYNO", "FLY_APP_NAME", "FLY_MACHINE_ID", "GITPOD_WORKSPACE_ID",
  "GNOME_TERMINAL_SERVICE", "GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_WORKSTATIONS", "KITTY_WINDOW_ID", "KONSOLE_VERSION",
  "KUBERNETES_SERVICE_HOST", "K_SERVICE", "LOCALAPPDATA", "MSYSTEM", "NETLIFY", "OSTYPE", "PATHEXT", "PROJECT_DOMAIN",
  "RAILWAY_ENVIRONMENT_NAME", "RAILWAY_SERVICE_NAME", "RENDER", "REPL_ID", "REPL_SLUG", "SESSIONNAME", "SPACE_CREATOR_USER_ID", "STY",
  "SYSTEMROOT", "TERM", "TERMINAL_EMULATOR", "TERMINATOR_UUID", "TERM_PROGRAM", "TILIX_ID", "TMUX", "USER", "VERCEL",
  "VSCODE_GIT_ASKPASS_MAIN", "VTE_VERSION", "WEBSITE_SITE_NAME", "WEBSITE_SKU", "WSL_DISTRO_NAME", "WT_SESSION", "XTERM_VERSION",
  "AWS_REGION", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE", "GOOGLE_APPLICATION_CREDENTIALS",
  "NODE_ENV", "COLORTERM", "FORCE_COLOR", "NO_COLOR",
];

/** Package and build names that are application configuration and must reach the command. */
export const INJECTED_PACKAGE_NAMES: readonly string[] = [
  "NPM_TOKEN", "NPM_CONFIG_REGISTRY", "PIP_INDEX_URL", "GRADLE_OPTS", "MAVEN_OPTS", "ORG_GRADLE_PROJECT_X", "TESTCONTAINERS_X",
  "DOCKER_AUTH_CONFIG",
];

/** Every denied name of the fixture plus one sample per denied prefix and suffix. */
export const DENIED_NAMES: readonly string[] = [
  ...fixture.denied_names.map((e) => e.value),
  ...fixture.denied_prefixes.map((e) => `${e.value}HOSTILE`),
  ...fixture.denied_suffixes.map((e) => `HOSTILE${e.value}`),
  "HTTPS_PROXY", "NO_PROXY", "BUN_BE_BUN", "GITHUB_TOKEN", "GH_TOKEN", "GITLAB_TOKEN", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0",
];

/** Name groups whose worker-built value must be byte-identical with and without user variables. */
export const PROVIDER_AUTH_NAMES = [
  "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL",
  "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL",
  "ANTHROPIC_SMALL_FAST_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
  "CLAUDE_CODE_USE_GATEWAY", "CLAUDE_CODE_MAX_CONTEXT_TOKENS", "OPENAI_API_KEY", "OPENAI_BASE_URL", "CODEX_HOME", "CODEX_API_KEY",
] as const;
export const GIT_AUTHORITY_NAMES = [
  "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1", "GIT_ASKPASS",
  "GIT_SSH_COMMAND", "GIT_DIR", "GIT_TERMINAL_PROMPT", "GH_TOKEN", "GH_HOST", "GITHUB_TOKEN", "GITLAB_TOKEN", "GLAB_TOKEN", "SSH_AUTH_SOCK",
] as const;
export const MANAGED_DIR_NAMES = [
  "HOME", "TMPDIR", "TMP", "TEMP", "GOCACHE", "GOMODCACHE", "GOTMPDIR", "npm_config_cache", "NPM_CONFIG_CACHE", "XDG_CACHE_HOME",
  "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR", "XDG_STATE_HOME",
] as const;
export const NETWORK_WIRING_NAMES = [
  "DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
  "NO_PROXY", "http_proxy", "https_proxy", "NODE_USE_ENV_PROXY", "NODE_EXTRA_CA_CERTS", "NODE_TLS_REJECT_UNAUTHORIZED", "SSL_CERT_FILE",
  "SSL_CERT_DIR", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE", "SSLKEYLOGFILE",
] as const;

/** A well-formed variable carrying a value that names its source. */
export function runEnvVar(name: string, over: Partial<RunEnvVar> = {}): RunEnvVar {
  const n = [...name].reduce((a, c) => (a * 31 + c.charCodeAt(0)) >>> 0, 7).toString(16).padStart(8, "0");
  return {
    var_id: `${n}-0000-4000-8000-${n}${n.slice(0, 4)}`,
    scope: "repo", rev: 1, name, value: `value-of-${name}`, secret: false, description: `description of ${name}`, ...over,
  };
}

/** The whole catalogue as claim variables. Names that fail the shape rule are skipped. */
export function catalogue(): RunEnvVar[] {
  const seen = new Set<string>();
  const out: RunEnvVar[] = [];
  for (const name of [...SDK_READ_ALLOWED_NAMES, ...INJECTED_PACKAGE_NAMES, ...DENIED_NAMES]) {
    if (seen.has(name)) continue;
    seen.add(name);
    out.push(runEnvVar(name));
  }
  return out;
}

/** Whether the shared rules allow a name. */
export function isAllowed(name: string): boolean {
  return runEnvNameViolation(name) === null;
}
