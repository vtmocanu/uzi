// Issue #3906: the REAL pnpm, driven through the worker's real js-deps install path.
//
// Nothing here mocks pnpm. Each protection (`--ignore-scripts`, `--ignore-pnpmfile`,
// `--config.manage-package-manager-versions=false`, `--config.virtual-store-dir=
// node_modules/.pnpm`) is proven twice: a PROTECTED run (through startDepsInstall, the
// provisioning seam the executor uses) must be ok with the vector NOT taken, and a
// POSITIVE CONTROL in a fresh copy of the same fixture drops exactly that one flag and
// must show the vector live: the repo-authored code DOES execute, the handoff fails, or
// (virtual store) package files land outside the clone. A protected run alone is not proof: a fixture whose hook can
// never fire passes it vacuously.
//
// The test also runs inside the worker image build (`UZI_PNPM_IMAGE_PROOF=1`, as the
// runner uid, network none, cwd /app). There it lives at /opt/uzi-src/agent/test with NO
// node_modules beside it, so this file and its src import closure use only `node:`
// builtins and relative imports. Keep it that way.
//
// Outside the image the file skips unless the pnpm on the check PATH is the pinned
// version, so a developer machine with another pnpm (or none) stays green. Under
// UZI_PNPM_IMAGE_PROOF=1 a missing or different pnpm FAILS instead of skipping.

import { after, before, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import http from "node:http";
import type { AddressInfo } from "node:net";
import os from "node:os";
import path from "node:path";
import zlib from "node:zlib";

import { execInstall, installJsDeps, type InstallExec, type JsDepsInstall } from "../src/js-deps.js";
import { startDepsInstall } from "../src/js-deps-provision.js";
import { buildCheckEnv } from "../src/sdk-env.js";

/** Must match the pnpm pinned in agent/templates/{base,jvm}/Dockerfile. */
const PIN = "10.34.9";

const FLAG_SCRIPTS = "--ignore-scripts";
const FLAG_PNPMFILE = "--ignore-pnpmfile";
const FLAG_MANAGE = "--config.manage-package-manager-versions=false";
const FLAG_VSTORE = "--config.virtual-store-dir=node_modules/.pnpm";

const imageProof = process.env.UZI_PNPM_IMAGE_PROOF === "1";

const scratchRoot = fs.mkdtempSync(path.join(os.tmpdir(), "js-deps-pnpm-real-"));
after(() => fs.rmSync(scratchRoot, { recursive: true, force: true }));

let counter = 0;
function mkdir(prefix: string): string {
  counter += 1;
  const dir = path.join(scratchRoot, `${prefix}-${counter}`);
  fs.mkdirSync(dir, { recursive: true });
  return dir;
}

/** The pnpm the worker's install would resolve: PATH comes from buildCheckEnv. */
function probePnpm(): { version: string | null; reason: string } {
  const home = mkdir("probe-home");
  const env = buildCheckEnv(process.env, home);
  try {
    const version = execFileSync("pnpm", ["--version"], { env, encoding: "utf8", timeout: 30_000 }).trim();
    return { version, reason: `pnpm ${version} on the check PATH` };
  } catch (err) {
    return { version: null, reason: `no runnable pnpm on the check PATH (${(err as Error).message.split("\n")[0]})` };
  }
}

const probed = probePnpm();
const skipReason =
  imageProof || probed.version === PIN ? false : `needs pnpm ${PIN} on the check PATH (${probed.reason})`;

const LOCKFILE = `lockfileVersion: '9.0'

settings:
  autoInstallPeers: true
  excludeLinksFromLockfile: false

importers:

  .:
    dependencies:
      dep:
        specifier: file:./vendor/dep
        version: file:vendor/dep

packages:

  dep@file:vendor/dep:
    resolution: {directory: vendor/dep, type: directory}

snapshots:

  dep@file:vendor/dep: {}
`;

interface FixtureOptions {
  /** Extra files, relative path to content. Written after the defaults (so may override). */
  files?: Record<string, string>;
  /** Extra package.json fields. */
  pkg?: Record<string, unknown>;
  /** .npmrc content; defaults to a dead-port registry so nothing can leave the host. */
  npmrc?: string;
}

/** A fresh clone-like dir: one `file:` dependency, an inline lockfile, no network needed. */
function mkFixture(opts: FixtureOptions = {}): string {
  const root = mkdir("clone");
  const files: Record<string, string> = {
    ".npmrc": opts.npmrc ?? "registry=http://127.0.0.1:9/\n",
    "package.json": JSON.stringify({
      name: "pnpm-real-fixture",
      version: "1.0.0",
      private: true,
      dependencies: { dep: "file:./vendor/dep" },
      ...opts.pkg,
    }),
    "pnpm-lock.yaml": LOCKFILE,
    "vendor/dep/package.json": JSON.stringify({ name: "dep", version: "1.0.0" }),
    "vendor/dep/index.js": 'module.exports = "dep-installed";\n',
    ...opts.files,
  };
  for (const [rel, content] of Object.entries(files)) {
    const abs = path.join(root, rel);
    fs.mkdirSync(path.dirname(abs), { recursive: true });
    fs.writeFileSync(abs, content);
  }
  return root;
}

/** A hook body that records its execution at an absolute path outside the fixture. */
function markerWriter(marker: string): string {
  return `require("node:fs").writeFileSync(${JSON.stringify(marker)}, "executed");\n`;
}

/**
 * A fresh per-case HOME. Its user-level .npmrc also pins the registry: pnpm resolves a
 * `packageManager`-declared pnpm through the USER config, not the project's .npmrc
 * (measured with pnpm 10.34.9: a project-only dead registry still let the handoff reach
 * registry.npmjs.org), so without this the (d) control would depend on the host network.
 */
function mkHome(registry = "http://127.0.0.1:9/"): string {
  const home = mkdir("home");
  fs.writeFileSync(path.join(home, ".npmrc"), `registry=${registry}\n`);
  return home;
}

function newMarker(): string {
  return path.join(mkdir("marker"), "ran");
}

type ProvisionArgs = Parameters<typeof startDepsInstall>;

/** PROTECTED run: the real provisioning seam, with the worker's real flag set. */
async function protectedRun(root: string, home = mkHome()): Promise<JsDepsInstall> {
  const ctx: ProvisionArgs[0] = { runId: "pnpm-real-proof", worktreePath: root, emit() {} };
  const quiet = (): void => {};
  const logger: ProvisionArgs[1] = {
    debug: quiet,
    info: quiet,
    warn: quiet,
    error: quiet,
    addSecret: quiet,
    removeSecret: quiet,
    child: () => logger,
  };
  const controller = new AbortController();
  return startDepsInstall(ctx, logger, home, undefined, controller.signal);
}

/** POSITIVE CONTROL: the same real install minus exactly one flag, which must have been there. */
async function controlRun(root: string, dropped: string, home = mkHome()): Promise<JsDepsInstall> {
  const seen: string[][] = [];
  const exec: InstallExec = (cmd) => {
    assert.ok(cmd.args.includes(dropped), `the install argv must carry ${dropped} before the control drops it`);
    seen.push([...cmd.args]);
    return execInstall({ ...cmd, args: cmd.args.filter((a) => a !== dropped) });
  };
  const result = await installJsDeps(root, buildCheckEnv(process.env, home), {
    timeoutMs: 100_000,
    exec,
  });
  assert.equal(seen.length, 1, "the control must have run exactly one install");
  return result;
}

function assertPnpmResult(result: JsDepsInstall, ok: boolean): void {
  assert.equal(result.truncated, false);
  // Exactly one install, at the root. Without a pnpm-workspace.yaml the lockfile-less
  // vendored `file:` dependency is also discovered as a project; it is never installed.
  const [r, ...rest] = result.results;
  assert.equal(r!.dir, ".");
  assert.equal(r!.manager, "pnpm");
  assert.equal(r!.ok, ok, r!.detail);
  for (const other of rest) {
    assert.deepEqual([other.dir, other.manager, other.ok], ["vendor/dep", "none", false]);
  }
}

function modulesYaml(root: string): { packageManager?: string } {
  return JSON.parse(fs.readFileSync(path.join(root, "node_modules", ".modules.yaml"), "utf8")) as {
    packageManager?: string;
  };
}

describe("real pnpm through js-deps (issue #3906)", { skip: skipReason }, () => {
  it(`the check PATH resolves pnpm ${PIN}`, () => {
    assert.equal(probed.version, PIN, probed.reason);
  });

  it("(a) baseline: installs the lockfile's file: dependency with the installer's own pnpm", async () => {
    const root = mkFixture();
    assertPnpmResult(await protectedRun(root), true);
    const required = execFileSync(process.execPath, ["-p", 'require("dep")'], { cwd: root, encoding: "utf8" }).trim();
    assert.equal(required, "dep-installed");
    assert.equal(modulesYaml(root).packageManager, `pnpm@${PIN}`);
  });

  describe("(b) lifecycle scripts", () => {
    const variants: { label: string; files: (m: { pre: string; post: string }) => Record<string, string> }[] = [
      { label: "root preinstall/postinstall", files: () => ({}) },
      {
        label: "root scripts with ignoreScripts: false in pnpm-workspace.yaml",
        files: () => ({ "pnpm-workspace.yaml": "ignoreScripts: false\n" }),
      },
    ];
    for (const v of variants) {
      const build = () => {
        const pre = newMarker();
        const post = newMarker();
        const root = mkFixture({
          pkg: { scripts: { preinstall: "node pre.js", postinstall: "node post.js" } },
          files: { "pre.js": markerWriter(pre), "post.js": markerWriter(post), ...v.files({ pre, post }) },
        });
        return { root, pre, post };
      };
      it(`${v.label}: protected run does not run them`, async () => {
        const f = build();
        assertPnpmResult(await protectedRun(f.root), true);
        assert.equal(fs.existsSync(f.pre), false, "preinstall ran");
        assert.equal(fs.existsSync(f.post), false, "postinstall ran");
      });
      it(`${v.label}: control without ${FLAG_SCRIPTS} runs them`, async () => {
        const f = build();
        await controlRun(f.root, FLAG_SCRIPTS);
        assert.equal(fs.existsSync(f.pre), true, "control preinstall did not run: the fixture proves nothing");
        assert.equal(fs.existsSync(f.post), true, "control postinstall did not run: the fixture proves nothing");
      });
    }
  });

  describe("(c) pnpmfile", () => {
    const variants: { label: string; fixture: (marker: string) => FixtureOptions }[] = [
      { label: ".pnpmfile.cjs", fixture: (m) => ({ files: { ".pnpmfile.cjs": markerWriter(m) } }) },
      {
        label: "pnpmfile: hook.cjs in pnpm-workspace.yaml",
        fixture: (m) => ({ files: { "pnpm-workspace.yaml": "pnpmfile: hook.cjs\n", "hook.cjs": markerWriter(m) } }),
      },
      {
        label: "global-pnpmfile in .npmrc",
        fixture: (m) => {
          const hook = path.join(mkdir("global-hook"), "global.cjs");
          fs.writeFileSync(hook, markerWriter(m));
          return { npmrc: `registry=http://127.0.0.1:9/\nglobal-pnpmfile=${hook}\n` };
        },
      },
      {
        label: "pnpmfile=hook.cjs in .npmrc",
        fixture: (m) => ({ npmrc: "registry=http://127.0.0.1:9/\npnpmfile=hook.cjs\n", files: { "hook.cjs": markerWriter(m) } }),
      },
      {
        label: "ignorePnpmfile: false in pnpm-workspace.yaml with .pnpmfile.cjs",
        fixture: (m) => ({ files: { "pnpm-workspace.yaml": "ignorePnpmfile: false\n", ".pnpmfile.cjs": markerWriter(m) } }),
      },
    ];
    for (const v of variants) {
      it(`${v.label}: protected run does not load it`, async () => {
        const marker = newMarker();
        const root = mkFixture(v.fixture(marker));
        assertPnpmResult(await protectedRun(root), true);
        assert.equal(fs.existsSync(marker), false, "pnpmfile ran");
      });
      it(`${v.label}: control without ${FLAG_PNPMFILE} loads it`, async () => {
        const marker = newMarker();
        const root = mkFixture(v.fixture(marker));
        // ok is deliberately not asserted: a loaded pnpmfile changes the lockfile's
        // pnpmfileChecksum expectation, so the control install may legitimately refuse
        // AFTER the hook ran. The marker is the proof the hook is reachable.
        await controlRun(root, FLAG_PNPMFILE);
        assert.equal(fs.existsSync(marker), true, "control hook did not run: the fixture proves nothing");
      });
    }
  });

  describe("(d) packageManager handoff", () => {
    // A loopback registry that answers every request 404 and records it: the declared
    // pnpm cannot be fetched, deterministically and fast (a dead port makes pnpm retry
    // for over a minute), and the log shows whether the handoff was even attempted.
    let registry: http.Server | undefined;
    let registryUrl = "";
    let seen: string[] = [];
    before(async () => {
      registry = http.createServer((req, res) => {
        seen.push(`${req.method} ${req.url}`);
        res.statusCode = 404;
        res.end("{}");
      });
      await new Promise<void>((resolve) => registry!.listen(0, "127.0.0.1", resolve));
      registryUrl = `http://127.0.0.1:${(registry.address() as AddressInfo).port}/`;
    });
    after(async () => {
      if (registry) await new Promise<void>((resolve) => registry!.close(() => resolve()));
    });

    const variants: { label: string; files: Record<string, string> }[] = [
      { label: "packageManager: pnpm@9.15.9", files: {} },
      {
        label: "packageManager: pnpm@9.15.9 with managePackageManagerVersions: true in pnpm-workspace.yaml",
        files: { "pnpm-workspace.yaml": "managePackageManagerVersions: true\n" },
      },
    ];
    const asksForPnpm = (): boolean => seen.some((r) => r === "GET /pnpm");
    for (const v of variants) {
      const build = () => mkFixture({ pkg: { packageManager: "pnpm@9.15.9" }, files: v.files });
      it(`${v.label}: protected run stays on the installer's own pnpm`, async () => {
        seen = [];
        const root = build();
        assertPnpmResult(await protectedRun(root, mkHome(registryUrl)), true);
        assert.equal(modulesYaml(root).packageManager, `pnpm@${PIN}`);
        assert.equal(asksForPnpm(), false, `protected run went looking for a pnpm: ${seen.join(", ")}`);
      });
      it(`${v.label}: control without ${FLAG_MANAGE} fails trying to fetch the declared pnpm`, async () => {
        seen = [];
        const root = build();
        assertPnpmResult(await controlRun(root, FLAG_MANAGE, mkHome(registryUrl)), false);
        assert.equal(asksForPnpm(), true, `control never tried to fetch the declared pnpm: ${seen.join(", ")}`);
      });
    }
  });

  describe("(e) configDependencies through a loopback registry", () => {
    const PLUGIN = "pnpm-plugin-uzi-probe";

    /** Minimal deterministic ustar archive (mtime 0, fixed modes) gzip-compressed. */
    function tarball(files: [string, string][]): Buffer {
      const blocks: Buffer[] = [];
      for (const [name, content] of files) {
        const body = Buffer.from(content);
        const header = Buffer.alloc(512);
        header.write(name, 0, "utf8");
        header.write("0000644\0", 100);
        header.write("0000000\0", 108);
        header.write("0000000\0", 116);
        header.write(`${body.length.toString(8).padStart(11, "0")}\0`, 124);
        header.write("00000000000\0", 136);
        header.write("        ", 148);
        header.write("0", 156);
        header.write("ustar\0", 257);
        header.write("00", 263);
        let sum = 0;
        for (const byte of header) sum += byte;
        header.write(`${sum.toString(8).padStart(6, "0")}\0 `, 148);
        blocks.push(header, body, Buffer.alloc((512 - (body.length % 512)) % 512));
      }
      blocks.push(Buffer.alloc(1024));
      return zlib.gzipSync(Buffer.concat(blocks), { level: 9, mtime: 0 } as zlib.ZlibOptions);
    }

    let server: http.Server | undefined;
    let port = 0;
    let requests: string[] = [];
    let archive: Buffer = Buffer.alloc(0);
    let integrity = "";
    let marker = "";

    function startServer(): Promise<void> {
      return new Promise((resolve, reject) => {
        server = http.createServer((req, res) => {
          const url = req.url ?? "";
          requests.push(`${req.method} ${url}`);
          if (url === `/${PLUGIN}/-/${PLUGIN}-1.0.0.tgz`) {
            res.setHeader("content-type", "application/octet-stream");
            res.end(archive);
          } else if (url === `/${PLUGIN}`) {
            res.setHeader("content-type", "application/json");
            res.end(
              JSON.stringify({
                name: PLUGIN,
                "dist-tags": { latest: "1.0.0" },
                versions: {
                  "1.0.0": {
                    name: PLUGIN,
                    version: "1.0.0",
                    dist: { tarball: `http://127.0.0.1:${port}/${PLUGIN}/-/${PLUGIN}-1.0.0.tgz`, integrity },
                  },
                },
              }),
            );
          } else {
            res.statusCode = 404;
            res.end("{}");
          }
        });
        server.once("error", reject);
        server.listen(0, "127.0.0.1", () => {
          port = (server!.address() as AddressInfo).port;
          resolve();
        });
      });
    }

    function buildFixture(): string {
      return mkFixture({
        npmrc: `registry=http://127.0.0.1:${port}/\n`,
        files: { "pnpm-workspace.yaml": `configDependencies:\n  ${PLUGIN}: "1.0.0+${integrity}"\n` },
      });
    }

    const tarballGets = (): string[] => requests.filter((r) => r.startsWith("GET ") && r.endsWith(".tgz"));

    after(async () => {
      if (server) await new Promise<void>((resolve) => server!.close(() => resolve()));
    });

    it("setup: serves a fixed plugin package", async () => {
      marker = newMarker();
      archive = tarball([
        ["package/package.json", JSON.stringify({ name: PLUGIN, version: "1.0.0" })],
        ["package/pnpmfile.cjs", `${markerWriter(marker)}module.exports = { hooks: {} };\n`],
      ]);
      integrity = `sha512-${crypto.createHash("sha512").update(archive).digest("base64")}`;
      await startServer();
      assert.ok(port > 0);
    });

    it("protected run fetches the config dependency but never loads its pnpmfile", async () => {
      requests = [];
      const root = buildFixture();
      assertPnpmResult(await protectedRun(root, mkHome(`http://127.0.0.1:${port}/`)), true);
      assert.equal(tarballGets().length, 1, `expected the plugin tarball fetch, saw: ${requests.join(", ")}`);
      assert.equal(fs.existsSync(marker), false, "the config dependency's pnpmfile ran");
    });

    it(`control without ${FLAG_PNPMFILE} loads the config dependency's pnpmfile`, async () => {
      requests = [];
      fs.rmSync(marker, { force: true });
      const root = buildFixture();
      // ok is not asserted: loading the plugin pnpmfile adds a pnpmfileChecksum the
      // fixture lockfile (correct for the protected run) lacks, so the frozen install
      // may refuse after the hook ran. The marker is the proof.
      await controlRun(root, FLAG_PNPMFILE, mkHome(`http://127.0.0.1:${port}/`));
      assert.equal(tarballGets().length, 1, `expected the plugin tarball fetch, saw: ${requests.join(", ")}`);
      assert.equal(fs.existsSync(marker), true, "control plugin hook did not run: the fixture proves nothing");
    });
  });

  describe("(f) repo-chosen write locations outside the clone", () => {
    // Each variant points a pnpm location setting at an absolute path in the test tmp root.
    // PROTECTED: ok, dep resolves inside the clone, nothing exists at the path. CONTROL: the
    // pin dropped writes there (the vector is live in this fixture).
    const variants: { label: string; flag: string; npmrcKey: string; yamlKey: string }[] = [
      { label: "virtual-store-dir", flag: FLAG_VSTORE, npmrcKey: "virtual-store-dir", yamlKey: "virtualStoreDir" },
    ];
    for (const v of variants) {
      for (const source of [".npmrc", "pnpm-workspace.yaml"]) {
        const build = () => {
          const target = path.join(mkdir("escape"), "x");
          const root =
            source === ".npmrc"
              ? mkFixture({ npmrc: `registry=http://127.0.0.1:9/\n${v.npmrcKey}=${target}\n` })
              : mkFixture({ files: { "pnpm-workspace.yaml": `${v.yamlKey}: ${target}\n` } });
          return { root, target };
        };
        it(`${v.label} in ${source}: protected run writes nothing outside the clone`, async () => {
          const f = build();
          assertPnpmResult(await protectedRun(f.root), true);
          assert.equal(fs.existsSync(f.target), false, `something was written at ${f.target}`);
          const required = execFileSync(process.execPath, ["-p", 'require("dep")'], { cwd: f.root, encoding: "utf8" }).trim();
          assert.equal(required, "dep-installed");
        });
        it(`${v.label} in ${source}: control without ${v.flag} writes there`, async () => {
          const f = build();
          await controlRun(f.root, v.flag);
          assert.equal(fs.existsSync(f.target), true, "control wrote nothing outside: the fixture proves nothing");
        });
      }
    }
  });
});
