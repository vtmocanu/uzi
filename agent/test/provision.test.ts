import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { nullLogger, recordingLogger } from "./helpers.js";
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

  it("strips the `;` terminator real devbox shellenv puts on each export", () => {
    // devbox 0.17.x emits `export KEY="value";` — the quote must not survive into the value.
    const out = [
      'export PATH="/data/provision/r/.devbox/nix/profile/default/bin:/usr/bin:/bin";',
      'export NIX_SSL_CERT_FILE="/etc/ssl/certs/ca-certificates.crt";',
    ].join("\n");
    const filtered = filterShellenv(out, "/ignored");
    assert.strictEqual(filtered.PATH, "/data/provision/r/.devbox/nix/profile/default/bin:/usr/bin:/bin");
    assert.strictEqual(filtered.NIX_SSL_CERT_FILE, "/etc/ssl/certs/ca-certificates.crt");
    for (const v of Object.values(filtered)) assert.ok(!/["';]/.test(v), `stray quote/terminator in ${v}`);
  });

  it("resolves $PATH and strips the terminator together, quoted or not", () => {
    assert.strictEqual(filterShellenv('export PATH="/nix/bin:$PATH";\n', "/usr/bin:/bin").PATH, "/nix/bin:/usr/bin:/bin");
    assert.strictEqual(filterShellenv("export LOCALE_ARCHIVE=/nix/loc ;\n", "/b").LOCALE_ARCHIVE, "/nix/loc");
    // Whitespace AFTER the terminator must not hide it.
    assert.strictEqual(filterShellenv('export PATH="/a"; \n', "/b").PATH, "/a");
  });
});

describe("provisionTools", () => {
  async function provisionWithPath(
    pathValue: string,
    processEnv: NodeJS.ProcessEnv,
    runPathProbe: NonNullable<Parameters<typeof provisionTools>[1]["runPathProbe"]>,
    log = nullLogger(),
  ) {
    return provisionTools(
      { packages: [], runDir: path.join(tmp, "run"), homeDir: "/data/agent-home" },
      {
        log, processEnv, runPathProbe,
        run: async (_cmd, args) => ({ stdout: args[0] === "shellenv" ? `export PATH="${pathValue}"\n` : "", stderr: "" }),
      },
    );
  }

  for (const [deniedIdentity, deniedUid] of [["runner", "runner"], ["runner-cmd", "10003"]] as const) {
    it(`drops entries inaccessible to ${deniedIdentity} while retaining ENOENT and duplicates`, async () => {
      const previous = process.env.UZI_UID_SPLIT;
      process.env.UZI_UID_SPLIT = "1";
      try {
        const calls: string[] = [];
        const { logger, lines } = recordingLogger();
        const runPathProbe: NonNullable<Parameters<typeof provisionTools>[1]["runPathProbe"]> = async (cmd, args, opts) => {
          assert.equal(cmd, "/bin/setpriv");
          assert.equal(args[0], "--reuid");
          const uid = args[1] ?? "";
          calls.push(uid);
          assert.equal(args.at(-4), process.execPath);
          assert.equal(args.at(-3), "-e");
          assert.deepEqual(JSON.parse(args.at(-1) ?? ""), ["/kept", "/denied", "/missing", "/denied", "/kept"]);
          assert.deepEqual(Object.keys(opts.env).sort(), ["HOME", "PATH"]);
          assert.equal(opts.cwd, path.join(tmp, "run"));
          return { stdout: JSON.stringify(["ok", uid === deniedUid ? "EACCES" : "ok", "ENOENT", uid === deniedUid ? "EACCES" : "ok", "ok"]), stderr: "" };
        };
        const result = await provisionWithPath("/kept:/denied:/missing:/denied:/kept", { PATH: "/base", UZI_UID_SPLIT: "1", UZI_WORKER_TOKEN: "secret" }, runPathProbe, logger);
        assert.deepEqual(calls, ["runner", "10003"]);
        assert.equal(result.toolEnv.PATH, "/kept:/missing:/kept");
        assert.ok(lines.some((line) => {
          const logged = JSON.stringify(line);
          return logged.includes("removed inaccessible PATH entries") && logged.includes(deniedIdentity);
        }));
      } finally {
        if (previous === undefined) delete process.env.UZI_UID_SPLIT;
        else process.env.UZI_UID_SPLIT = previous;
      }
    });
  }

  for (const failure of ["spawn", "protocol"] as const) {
    it(`retains the whole PATH and warns on ${failure} failure`, async () => {
      const previous = process.env.UZI_UID_SPLIT;
      process.env.UZI_UID_SPLIT = "1";
      try {
        let calls = 0;
        const { logger, lines } = recordingLogger();
        const result = await provisionWithPath("/a:/b", { PATH: "/base", UZI_UID_SPLIT: "1" }, async () => {
          calls++;
          if (calls === 2) {
            if (failure === "spawn") throw new Error("spawn failed");
            return { stdout: '{"statuses":["ok","EACCES"]}', stderr: "" };
          }
          return { stdout: '["EACCES","ok"]', stderr: "" };
        }, logger);
        assert.equal(calls, 2);
        assert.equal(result.toolEnv.PATH, "/a:/b");
        assert.ok(lines.some((line) => JSON.stringify(line).includes("PATH probe failed; retaining original PATH")));
      } finally {
        if (previous === undefined) delete process.env.UZI_UID_SPLIT;
        else process.env.UZI_UID_SPLIT = previous;
      }
    });
  }

  it("probes once as the current identity without a UID split", async () => {
    const previous = process.env.UZI_UID_SPLIT;
    delete process.env.UZI_UID_SPLIT;
    try {
      let calls = 0;
      const result = await provisionWithPath("/a:/b", { PATH: "/base" }, async (cmd, args) => {
        calls++;
        assert.equal(cmd, process.execPath);
        assert.equal(args[0], "-e");
        assert.deepEqual(JSON.parse(args[2] ?? ""), ["/a", "/b"]);
        return { stdout: '["EACCES","ENOENT"]', stderr: "" };
      });
      assert.equal(calls, 1);
      assert.equal(result.toolEnv.PATH, "/b");
    } finally {
      if (previous === undefined) delete process.env.UZI_UID_SPLIT;
      else process.env.UZI_UID_SPLIT = previous;
    }
  });
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

    // Manifest is packages-only, written outside any clone, with every package VERSIONED
    // (unversioned ones pinned to @latest) so devbox emits no "legacy format" warning —
    // that warning is keyed on unversioned packages, not on the array shape.
    const manifest = JSON.parse(await fs.readFile(path.join(tmp, "run", "devbox.json"), "utf8"));
    assert.deepStrictEqual(manifest, { packages: ["kubectl@1.31", "jq@latest"] });
    // Guard the invariant directly, not just this fixture: a regression that stopped
    // pinning (the empty-string no-op an object reshape allows) would leave a bare name.
    for (const pkg of manifest.packages) {
      assert.ok(pkg.includes("@"), `package ${pkg} must be versioned to avoid the devbox legacy-format warning`);
    }

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

  it("retries a TRANSIENT devbox install error and succeeds (recording sleep, no real wait)", async () => {
    const delays: number[] = [];
    const sleep = async (ms: number) => {
      delays.push(ms);
    };
    let installCalls = 0;
    const run = async (_cmd: string, args: string[]): Promise<RunResult> => {
      if (args[0] === "install") {
        installCalls++;
        if (installCalls <= 2) {
          throw new Error("curl: (28) Timeout was reached while fetching nixpkgs metadata from api.github.com");
        }
        return { stdout: "", stderr: "" };
      }
      if (args[0] === "shellenv") return { stdout: 'export PATH="/nix/bin:$PATH"\n', stderr: "" };
      return { stdout: "", stderr: "" };
    };

    const res = await provisionTools(
      { packages: ["kubectl@1.31"], runDir: path.join(tmp, "run"), homeDir: "/data/agent-home" },
      { log: nullLogger(), run, sleep, processEnv: { PATH: "/usr/bin" } },
    );

    assert.strictEqual(installCalls, 3);
    assert.deepStrictEqual(delays, [1000, 4000]);
    assert.strictEqual(res.toolEnv.PATH, "/nix/bin:/usr/bin");
  });

  it("does NOT retry a deterministic (permanent) devbox install error", async () => {
    const delays: number[] = [];
    const sleep = async (ms: number) => {
      delays.push(ms);
    };
    let installCalls = 0;
    const run = async (_cmd: string, args: string[]): Promise<RunResult> => {
      if (args[0] === "install") {
        installCalls++;
        throw new Error("error: package 'nonesuch' not found");
      }
      return { stdout: "", stderr: "" };
    };

    await assert.rejects(
      provisionTools(
        { packages: ["nonesuch"], runDir: path.join(tmp, "run"), homeDir: "/data/agent-home" },
        { log: nullLogger(), run, sleep, processEnv: { PATH: "/usr/bin" } },
      ),
      /tool provisioning failed \(devbox install\)/,
    );
    assert.strictEqual(installCalls, 1);
    assert.deepStrictEqual(delays, []);
  });

  it("does NOT retry a worker-timeout SIGTERM kill (would give 4×10min otherwise)", async () => {
    const delays: number[] = [];
    const sleep = async (ms: number) => {
      delays.push(ms);
    };
    let installCalls = 0;
    const run = async (_cmd: string, args: string[]): Promise<RunResult> => {
      if (args[0] === "install") {
        installCalls++;
        const e = new Error("Command failed: devbox install");
        (e as unknown as { killed: boolean }).killed = true;
        (e as unknown as { signal: string }).signal = "SIGTERM";
        (e as unknown as { code: null }).code = null;
        throw e;
      }
      return { stdout: "", stderr: "" };
    };

    await assert.rejects(
      provisionTools(
        { packages: ["kubectl@1.31"], runDir: path.join(tmp, "run"), homeDir: "/data/agent-home" },
        { log: nullLogger(), run, sleep, processEnv: { PATH: "/usr/bin" } },
      ),
      /tool provisioning failed \(devbox install\)/,
    );
    assert.strictEqual(installCalls, 1);
    assert.deepStrictEqual(delays, []);
  });
});
