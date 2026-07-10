import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { nullLogger } from "./helpers.js";
import {
  buildProvisionEnv,
  filterShellenv,
  provisionTools,
  PROVISION_ENV_ALLOWLIST,
  type RunResult,
} from "../src/provision.js";

let tmp: string;
beforeEach(async () => {
  tmp = await fs.mkdtemp(path.join(os.tmpdir(), "uzi-provision-"));
});
afterEach(async () => {
  await fs.rm(tmp, { recursive: true, force: true });
});

describe("buildProvisionEnv (Decision 3 scrub)", () => {
  it("is a replacement env with NO worker secrets", () => {
    const source: NodeJS.ProcessEnv = {
      PATH: "/usr/bin:/bin",
      UZI_WORKER_TOKEN: "join-token-should-not-leak",
      UZI_WORKER_TOKEN_FILE: "/run/secrets/worker_token",
      // Simulate an accidentally-present credential in the worker env.
      SOME_PAT: "glpat-secret",
      NIX_SSL_CERT_FILE: "/etc/ssl/cert.pem",
    };
    const env = buildProvisionEnv(source, "/data/agent-home");

    assert.strictEqual(env.PATH, "/usr/bin:/bin");
    assert.strictEqual(env.HOME, "/data/agent-home");
    assert.strictEqual(env.NIX_SSL_CERT_FILE, "/etc/ssl/cert.pem");
    // The join token (env + file path) and any stray credential must be absent.
    assert.strictEqual(env.UZI_WORKER_TOKEN, undefined);
    assert.strictEqual(env.UZI_WORKER_TOKEN_FILE, undefined);
    assert.strictEqual(env.SOME_PAT, undefined);
    // Nothing in the whole env may contain the secret values.
    const blob = JSON.stringify(env);
    assert.ok(!blob.includes("join-token-should-not-leak"));
    assert.ok(!blob.includes("glpat-secret"));
  });
});

describe("filterShellenv (output allowlist)", () => {
  it("keeps only allowlisted keys and resolves $PATH", () => {
    const out = [
      'export PATH="/nix/store/abc/bin:$PATH"',
      'export NIX_SSL_CERT_FILE="/etc/ssl/certs/ca.pem"',
      'export LOCALE_ARCHIVE="/nix/store/loc/lib/locale-archive"',
      'export SECRET_TOKEN="leak-me"',
      'export HOME="/somewhere/else"',
      "refresh_aliases() { :; }",
    ].join("\n");
    const filtered = filterShellenv(out, "/usr/bin:/bin");

    assert.deepStrictEqual(Object.keys(filtered).sort(), ["LOCALE_ARCHIVE", "NIX_SSL_CERT_FILE", "PATH"]);
    assert.strictEqual(filtered.PATH, "/nix/store/abc/bin:/usr/bin:/bin");
    // Non-allowlisted vars are dropped, including HOME (never overridden by tools).
    assert.strictEqual(filtered.SECRET_TOKEN, undefined);
    assert.strictEqual(filtered.HOME, undefined);
    for (const k of Object.keys(filtered)) assert.ok(PROVISION_ENV_ALLOWLIST.has(k));
  });

  it("inserts a $-containing base PATH literally (no replacement-pattern interpretation)", () => {
    // A basePath with `$&`/`$1` must not be interpreted by String.replace.
    const filtered = filterShellenv('export PATH="/nix/bin:$PATH"\n', "/weird/$&/$1/bin");
    assert.strictEqual(filtered.PATH, "/nix/bin:/weird/$&/$1/bin");
  });
});

describe("provisionTools", () => {
  it("writes a packages-only devbox.json and installs in a scrubbed env", async () => {
    const calls: Array<{ cmd: string; args: string[]; env: NodeJS.ProcessEnv }> = [];
    const run = async (cmd: string, args: string[], opts: { cwd: string; env: NodeJS.ProcessEnv }): Promise<RunResult> => {
      calls.push({ cmd, args, env: opts.env });
      if (args[0] === "shellenv") return { stdout: 'export PATH="/nix/bin:$PATH"\n', stderr: "" };
      return { stdout: "", stderr: "" };
    };

    const res = await provisionTools(
      { packages: ["kubectl@1.31", "jq"], runDir: path.join(tmp, "run"), homeDir: "/data/agent-home" },
      {
        log: nullLogger(),
        run,
        processEnv: { PATH: "/usr/bin", UZI_WORKER_TOKEN: "nope", ANTHROPIC_OAUTH: "sk-secret" },
      },
    );

    // Manifest is packages-only, written outside any clone.
    const manifest = JSON.parse(await fs.readFile(path.join(tmp, "run", "devbox.json"), "utf8"));
    assert.deepStrictEqual(manifest, { packages: ["kubectl@1.31", "jq"] });

    // install ran first, in a scrubbed env (no join token / anthropic token anywhere).
    const install = calls[0];
    assert.ok(install);
    assert.strictEqual(install.cmd, "devbox");
    assert.deepStrictEqual(install.args, ["install"]);
    const envBlob = JSON.stringify(install.env);
    assert.ok(!envBlob.includes("nope"), "join token leaked into provision env");
    assert.ok(!envBlob.includes("sk-secret"), "anthropic token leaked into provision env");
    assert.strictEqual(install.env.UZI_WORKER_TOKEN, undefined);

    // shellenv output filtered to the allowlist, $PATH resolved against scrubbed PATH.
    assert.strictEqual(res.toolEnv.PATH, "/nix/bin:/usr/bin");
  });

  it("fails the run (throws a clear message) when devbox install fails", async () => {
    const run = async (_cmd: string, args: string[]): Promise<RunResult> => {
      if (args[0] === "install") throw new Error("error: package 'nonesuch' not found");
      return { stdout: "", stderr: "" };
    };
    await assert.rejects(
      provisionTools(
        { packages: ["nonesuch"], runDir: path.join(tmp, "run"), homeDir: "/data/agent-home" },
        { log: nullLogger(), run, processEnv: { PATH: "/usr/bin" } },
      ),
      /tool provisioning failed \(devbox install\)/,
    );
  });
});
