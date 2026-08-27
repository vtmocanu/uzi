import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { buildSdkEnv, type SdkEnv } from "../src/sdk-env.js";

// The sparse-env guarantee (primary directive): the SDK subprocess sees ONLY
// the Anthropic OAuth token + HOME + PATH. The worker's own secrets (join token,
// GitLab PAT) and any ANTHROPIC_* override must be provably absent. Proven the
// same structural way git-secret.test.ts proves PAT hygiene.

const FAKE_OAUTH = "dummy-oauth-token-do-not-scan-0000";
const FAKE_PAT = "dummy-forge-pat-do-not-scan-1111";
const FAKE_JOIN_TOKEN = "dummy-join-token-do-not-scan-2222";
const FAKE_API_KEY = "dummy-anthropic-api-key-do-not-scan-3333";
const HOME_DIR = "/data/agent-home";

let saved: Record<string, string | undefined>;

beforeEach(() => {
  // Seed the worker's own env with secrets that must NOT leak to the subprocess.
  saved = {
    UZI_WORKER_TOKEN: process.env.UZI_WORKER_TOKEN,
    UZI_FORGE_PAT: process.env.UZI_FORGE_PAT,
    ANTHROPIC_API_KEY: process.env.ANTHROPIC_API_KEY,
    ANTHROPIC_AUTH_TOKEN: process.env.ANTHROPIC_AUTH_TOKEN,
  };
  process.env.UZI_WORKER_TOKEN = FAKE_JOIN_TOKEN;
  process.env.UZI_FORGE_PAT = FAKE_PAT;
  process.env.ANTHROPIC_API_KEY = FAKE_API_KEY;
  process.env.ANTHROPIC_AUTH_TOKEN = FAKE_API_KEY;
});

afterEach(() => {
  for (const [k, v] of Object.entries(saved)) {
    if (v === undefined) delete process.env[k];
    else process.env[k] = v;
  }
});

describe("buildSdkEnv", () => {
  it("contains only the OAuth token, HOME, PATH, AGENT_BROWSER_ARGS, TMPDIR (plus explicitly-unset ANTHROPIC_*)", () => {
    const env = buildSdkEnv(FAKE_OAUTH, HOME_DIR);

    assert.strictEqual(env.CLAUDE_CODE_OAUTH_TOKEN, FAKE_OAUTH);
    assert.strictEqual(env.HOME, HOME_DIR);
    // The RUNNER PATH (PRD #51 M4): UZI_RUNNER_PATH under the split, else process.env.PATH.
    // Unset in THIS fixture, so it equals process.env.PATH. On a real single-uid worker it
    // is no longer unset — since PRD #120 the entrypoint pins it on both branches, so the
    // fallback is the non-entrypoint case only.
    assert.strictEqual(env.PATH, process.env.PATH);
    assert.strictEqual(env.ANTHROPIC_API_KEY, undefined);
    assert.strictEqual(env.ANTHROPIC_AUTH_TOKEN, undefined);

    // Exactly the core keys (+ TMPDIR, the 5-bis per-uid scratch dir, when the ambient
    // env has one) — no other WORKER env is spread in. TMPDIR is not a secret.
    const expected = new Set(["CLAUDE_CODE_OAUTH_TOKEN", "HOME", "PATH", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "AGENT_BROWSER_ARGS"]);
    if (process.env.UZI_RUNNER_TMPDIR || process.env.TMPDIR) expected.add("TMPDIR");
    assert.deepStrictEqual(new Set(Object.keys(env)), expected);
  });

  it("always sets AGENT_BROWSER_ARGS with --no-sandbox and --disable-dev-shm-usage", () => {
    // The mandatory browser launch flags cross into the agent env (a non-secret FLAG,
    // not a credential) so they survive a shim bypass — the real agent-browser CLI reads
    // AGENT_BROWSER_ARGS from its environment. --no-sandbox is required under the PRD #51
    // hardening (unprivileged uid + cap_drop:ALL make Chromium's setuid sandbox impossible).
    const env = buildSdkEnv(FAKE_OAUTH, HOME_DIR);
    assert.ok(env.AGENT_BROWSER_ARGS, "AGENT_BROWSER_ARGS must be present");
    assert.ok(env.AGENT_BROWSER_ARGS?.includes("--no-sandbox"), "must carry --no-sandbox");
    assert.ok(
      env.AGENT_BROWSER_ARGS?.includes("--disable-dev-shm-usage"),
      "must carry --disable-dev-shm-usage",
    );
  });

  it("never lets a provisioned AGENT_BROWSER_ARGS drop --no-sandbox", () => {
    // AGENT_BROWSER_ARGS is a PROTECTED key: a provisioned toolEnv value (e.g. from
    // allowlist drift) cannot overwrite the mandatory launch flags.
    const env = buildSdkEnv(FAKE_OAUTH, HOME_DIR, {
      AGENT_BROWSER_ARGS: "--something-else",
    } as Record<string, string>);
    assert.ok(
      env.AGENT_BROWSER_ARGS?.includes("--no-sandbox"),
      "the protected AGENT_BROWSER_ARGS must keep --no-sandbox",
    );
    assert.ok(
      env.AGENT_BROWSER_ARGS?.includes("--disable-dev-shm-usage"),
      "the protected AGENT_BROWSER_ARGS must keep --disable-dev-shm-usage",
    );
    assert.ok(!env.AGENT_BROWSER_ARGS?.includes("--something-else"));
  });

  it("never carries the join token or the bot PAT", () => {
    const env = buildSdkEnv(FAKE_OAUTH, HOME_DIR);
    const serialized = JSON.stringify(env);
    assert.ok(!serialized.includes(FAKE_JOIN_TOKEN), "join token must not appear in the SDK env");
    assert.ok(!serialized.includes(FAKE_PAT), "bot PAT must not appear in the SDK env");
  });

  it("does not let an inherited ANTHROPIC_API_KEY override the OAuth token", () => {
    // ANTHROPIC_API_KEY is set in process.env (above) yet the built env pins it
    // to undefined, so the OAuth token wins the SDK's auth precedence.
    const env: SdkEnv = buildSdkEnv(FAKE_OAUTH, HOME_DIR);
    assert.strictEqual(env.ANTHROPIC_API_KEY, undefined);
    assert.ok(!JSON.stringify(env).includes(FAKE_API_KEY));
  });

  it("folds in provisioned tool env (PRD #18 M3): PATH replaced, nix vars added", () => {
    const env = buildSdkEnv(FAKE_OAUTH, HOME_DIR, {
      PATH: "/nix/store/kubectl/bin:/usr/bin",
      NIX_SSL_CERT_FILE: "/etc/ssl/cert.pem",
    });
    assert.strictEqual(env.PATH, "/nix/store/kubectl/bin:/usr/bin");
    assert.strictEqual(env.NIX_SSL_CERT_FILE, "/etc/ssl/cert.pem");
    // Credentials + HOME are unchanged and still present.
    assert.strictEqual(env.CLAUDE_CODE_OAUTH_TOKEN, FAKE_OAUTH);
    assert.strictEqual(env.HOME, HOME_DIR);
  });

  it("omits DOCKER_HOST when no sidecar is wired (PRD #83 Q4)", () => {
    const env = buildSdkEnv(FAKE_OAUTH, HOME_DIR);
    assert.ok(!("DOCKER_HOST" in env), "no DOCKER_HOST key when the worker has no daemon wired");
    const withTools = buildSdkEnv(FAKE_OAUTH, HOME_DIR, { PATH: "/nix/bin" });
    assert.ok(!("DOCKER_HOST" in withTools));
  });

  it("injects DOCKER_HOST when a sidecar is wired, and a provisioned var cannot clobber it", () => {
    const DIND = "unix:///run/dind/docker.sock";
    const env = buildSdkEnv(FAKE_OAUTH, HOME_DIR, {}, DIND);
    assert.strictEqual(env.DOCKER_HOST, DIND);
    // Set AFTER the tool-env fold: even if a provisioned var carried a DOCKER_HOST (it
    // never does — not on the allowlist), the wired endpoint wins.
    const folded = buildSdkEnv(FAKE_OAUTH, HOME_DIR, { DOCKER_HOST: "tcp://evil:2375" } as Record<string, string>, DIND);
    assert.strictEqual(folded.DOCKER_HOST, DIND);
    // The credential + HOME are untouched by the new param.
    assert.strictEqual(env.CLAUDE_CODE_OAUTH_TOKEN, FAKE_OAUTH);
    assert.strictEqual(env.HOME, HOME_DIR);
  });

  it("never lets tool env overwrite a protected key (credential, HOME, ANTHROPIC_*)", () => {
    // filterShellenv already drops these, but buildSdkEnv is defensive too — the
    // ANTHROPIC_* keys must stay undefined so nothing outranks the OAuth token.
    const env = buildSdkEnv(FAKE_OAUTH, HOME_DIR, {
      CLAUDE_CODE_OAUTH_TOKEN: "attacker-token",
      HOME: "/tmp/evil",
      ANTHROPIC_API_KEY: "attacker-key",
      ANTHROPIC_AUTH_TOKEN: "attacker-auth",
      PATH: "/nix/bin",
    } as Record<string, string>);
    assert.strictEqual(env.CLAUDE_CODE_OAUTH_TOKEN, FAKE_OAUTH);
    assert.strictEqual(env.HOME, HOME_DIR);
    assert.strictEqual(env.ANTHROPIC_API_KEY, undefined);
    assert.strictEqual(env.ANTHROPIC_AUTH_TOKEN, undefined);
    assert.ok(!JSON.stringify(env).includes("attacker"));
    assert.strictEqual(env.PATH, "/nix/bin");
  });
});
