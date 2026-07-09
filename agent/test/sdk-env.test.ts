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
  it("contains only the OAuth token, HOME, and PATH (plus explicitly-unset ANTHROPIC_*)", () => {
    const env = buildSdkEnv(FAKE_OAUTH, HOME_DIR);

    assert.strictEqual(env.CLAUDE_CODE_OAUTH_TOKEN, FAKE_OAUTH);
    assert.strictEqual(env.HOME, HOME_DIR);
    assert.strictEqual(env.PATH, process.env.PATH);
    assert.strictEqual(env.ANTHROPIC_API_KEY, undefined);
    assert.strictEqual(env.ANTHROPIC_AUTH_TOKEN, undefined);

    // Exactly these five keys — no other worker env is spread in.
    assert.deepStrictEqual(
      new Set(Object.keys(env)),
      new Set(["CLAUDE_CODE_OAUTH_TOKEN", "HOME", "PATH", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"]),
    );
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

  it("never lets tool env overwrite the credential or HOME keys", () => {
    // filterShellenv already drops these, but buildSdkEnv is defensive too.
    const env = buildSdkEnv(FAKE_OAUTH, HOME_DIR, {
      CLAUDE_CODE_OAUTH_TOKEN: "attacker-token",
      HOME: "/tmp/evil",
      PATH: "/nix/bin",
    } as Record<string, string>);
    assert.strictEqual(env.CLAUDE_CODE_OAUTH_TOKEN, FAKE_OAUTH);
    assert.strictEqual(env.HOME, HOME_DIR);
    assert.strictEqual(env.PATH, "/nix/bin");
  });
});
