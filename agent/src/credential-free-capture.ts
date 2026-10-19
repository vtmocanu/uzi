// Pure request policy; request issuance and lifecycle state are private to git.ts.
import { isAbsolute, resolve } from "node:path";
import type { BoundaryProcessRequest } from "./harness.js";
import { TrustedExecutionRefusal } from "./trusted-execution-refusal.js";
import { carriesForgeCredential } from "./residue-quarantine.js";

function refuse(): never {
  throw new TrustedExecutionRefusal("credential-free capture launch refused");
}

function forbiddenConfig(key: string): boolean {
  return /^(credential\.|core\.(?:.*\.)?askpass$|http\..*extraheader$|include\.|includeif\.)/i.test(key);
}
function credentialText(value: string): boolean {
  return /authorization:/i.test(value) || /[a-z][a-z0-9+.-]*:\/\/[^/\s]*@/i.test(value);
}

/** Complete canonical env comparison includes indexed config ORDER and every key.
 * The supplied variants are built by GitCache, never by a runner. */
export function validateCaptureGitRequest(
  request: BoundaryProcessRequest,
  environments: readonly NodeJS.ProcessEnv[],
  gitBin: string,
  helperBin: string,
  cancelContentHelper: string,
): void {
  if (request.recoverableTimeout || !isAbsolute(request.cwd) || resolve(request.cwd) !== request.cwd) refuse();
  const env = request.env;
  const trusted = environments.some(candidate =>
    Object.keys(env).length === Object.keys(candidate).length &&
    Object.entries(env).every(([key, value]) => candidate[key] === value));
  if (!trusted || carriesForgeCredential(env)) refuse();
  for (const [key, value] of Object.entries(env)) {
    if (/^(?:GIT_CONFIG_PARAMETERS|GIT_ASKPASS|SSH_ASKPASS|GIT_CREDENTIAL_.*|GCM_.*)$/i.test(key) ||
        credentialText(key) || (value !== undefined && credentialText(value))) refuse();
  }
  const count = Number(env.GIT_CONFIG_COUNT);
  if (!Number.isSafeInteger(count) || count < 2 || String(count) !== env.GIT_CONFIG_COUNT) refuse();
  let helpers = 0;
  let askpasses = 0;
  for (let i = 0; i < count; i++) {
    const key = env[`GIT_CONFIG_KEY_${i}`];
    const value = env[`GIT_CONFIG_VALUE_${i}`];
    if (key === "credential.helper" && value === "") helpers++;
    else if (key === "core.askpass" && value === "") askpasses++;
    else if (!key || forbiddenConfig(key)) refuse();
  }
  if (helpers !== 1 || askpasses !== 1) refuse();
  if (request.argv[0] === helperBin) {
    // This is the embedded cancellation content reader only. Its internal git
    // environment is not represented as the outer canonical config.
    if (request.identity !== "command" || request.argv[1] !== "-e" ||
        request.argv[2] !== cancelContentHelper || request.argv[3] !== request.cwd ||
        (request.argv.length !== 4 && (request.argv.length !== 5 || !/^\d+$/.test(request.argv[4]!)))) refuse();
    return;
  }
  if (request.argv[0] !== gitBin) refuse();
  for (let i = 1; i < request.argv.length; i++) {
    const arg = request.argv[i]!;
    if (credentialText(arg) || /^--config-env/i.test(arg) || forbiddenConfig(arg.split("=")[0]!)) refuse();
    let config: string | undefined;
    if (arg === "-c") { config = request.argv[++i]; if (config === undefined) refuse(); }
    else if (arg.startsWith("-c") && arg.length > 2) config = arg.slice(2);
    if (config !== undefined) {
      const canonical = environments[0]!;
      const trustedPins = Array.from({ length: Number(canonical.GIT_CONFIG_COUNT) }, (_, n) =>
        `${canonical[`GIT_CONFIG_KEY_${n}`]}=${canonical[`GIT_CONFIG_VALUE_${n}`]}`);
      const fixedPins = ["protocol.file.allow=user", "core.untrackedCache=false", "core.ignoreStat=false", "core.checkStat=default",
        "core.trustctime=true", "core.fileMode=true", "core.ignoreCase=false", "core.excludesFile=/dev/null",
        "commit.gpgsign=false"];
      if (forbiddenConfig(config.split("=")[0]!) || credentialText(config) ||
          (!trustedPins.includes(config) && !fixedPins.includes(config))) refuse();
    }
  }
}
