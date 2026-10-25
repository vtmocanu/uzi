import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { spawnSync } from "node:child_process";

const repo = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const source = fs.readFileSync(path.join(repo, "agent/templates/entrypoint.sh"), "utf8");
const uid = process.getuid!();
const gid = process.getgid!();

// As in entrypoint-migration.test.ts, transport image constants to a private fixture.
// Translate the image's worker ID to the test process ID; production comparisons
// still exercise actual lstat owner, group and complete mode (including special bits).
function fixture(rootMode = false) {
  const root = fs.mkdtempSync(path.join(repo, ".uzi/scratch/epoch-entrypoint-"));
  const data = path.join(root, "data");
  const bootstrap = path.join(root, "bootstrap");
  const home = path.join(root, "home");
  const app = path.join(root, "app");
  const nix = path.join(root, "nix");
  const runtime = path.join(data, "runner/.tmp");
  const log = path.join(root, "ops");
  for (const dir of [data, bootstrap, home, app, nix, runtime]) {
    fs.mkdirSync(dir, { recursive: true });
    fs.chownSync(dir, uid, gid);
  }
  fs.chmodSync(data, 0o2770);
  fs.chmodSync(bootstrap, 0o700);
  fs.chmodSync(home, 0o700);
  fs.chmodSync(runtime, 0o755);
  fs.writeFileSync(path.join(data, ".uzi-epoch.lock"), "");
  function stub(name: string, body: string) {
    const file = path.join(root, name);
    fs.writeFileSync(file, "#!/bin/sh\n" + body, { mode: 0o755 });
    return file;
  }
  const id = stub("id", `echo ${rootMode ? 0 : uid}\n`);
  const tini = stub("tini", `
[ ! -e /proc/self/fd/9 ] || { echo leaked-fd9 >&2; exit 8; }
printf 'runtime=%s\\nbootstrap=%s\\ncache=%s\\ntsx=%s\\n' "$UZI_EPOCH_RUNTIME_TMPDIR" "$TMPDIR" "$npm_config_cache" "$TSX_DISABLE_CACHE"
printf 'arg=%s\\n' "$@"
`);
  const chown = stub("chown", 'printf "chown %s\\n" "$*" >> "$OPLOG"\n');
  const helper = path.join(root, "helper.cjs");
  fs.writeFileSync(helper, `
const fs = require('node:fs');
fs.appendFileSync(process.env.OPLOG, process.argv[2] + '\\n');
if (process.argv[2] === 'prepare') console.log('1:1');
if (process.argv[2] === 'hold') {
  fs.fstatSync(9);
  if (process.env.FAIL_HOLD) process.exit(7);
}
`);
  let script = source
    .replace("ID=/usr/bin/id", `ID=${id}`)
    .replace("TINI=/sbin/tini", `TINI=${tini}`)
    .replace("CHOWN=/bin/chown", `CHOWN=${chown}`)
    .replace("NODE=/usr/local/bin/node", `NODE=${process.execPath}`)
    .replace("EPOCH_HELPER=/usr/local/lib/uzi/pod-epoch-init.cjs", `EPOCH_HELPER=${helper}`)
    .replace("EPOCH_BOOTSTRAP=/tmp/uzi-epoch-bootstrap", `EPOCH_BOOTSTRAP=${bootstrap}`)
    .replace("DATA_DIR=/data", `DATA_DIR=${data}`)
    .replace("NIX_DIR=/nix", `NIX_DIR=${nix}`)
    .replaceAll("/home/worker", home)
    .replaceAll("/app", app)
    .replaceAll("10001", String(uid))
    // UID/GID are distinct in some developer environments.
    .replaceAll(`s.gid !== ${uid}`, `s.gid !== ${gid}`)
    .replaceAll(`data.gid !== ${uid}`, `data.gid !== ${gid}`);
  const file = path.join(root, "entrypoint.sh");
  function run(extra: Record<string, string> = {}, args = ["npm", "run", "start"]) {
    fs.writeFileSync(file, script);
    const inherited = fs.openSync("/dev/null", "r");
    try {
      return spawnSync("/bin/sh", [file, ...args], {
        cwd: app,
        env: { PATH: "/usr/local/bin:/usr/bin:/bin", HOME: home, TMPDIR: runtime,
          UZI_EPOCH_LOCK: "1", OPLOG: log, ...extra },
        stdio: ["ignore", "pipe", "pipe", "ignore", "ignore", "ignore", "ignore", "ignore", "ignore", inherited],
        encoding: "utf8", timeout: 10000,
      });
    } finally { fs.closeSync(inherited); }
  }
  return { root, data, bootstrap, home, runtime, log, run,
    rewrite: (change: (text: string) => string) => { script = change(script); },
    close: () => fs.rmSync(root, { recursive: true, force: true }) };
}

describe("pod epoch entrypoint", () => {
  it("confines single-uid bootstrap and closes inherited FD9 before tini", () => {
    const f = fixture();
    try {
      const result = f.run();
      assert.equal(result.status, 0, result.stderr);
      assert.ok(result.stdout.includes("runtime=" + f.runtime));
      assert.ok(result.stdout.includes("bootstrap=" + f.bootstrap));
      assert.ok(result.stdout.includes("cache=" + f.bootstrap));
      assert.match(result.stdout, /tsx=1/);
      assert.match(result.stdout, /arg=\/usr\/local\/bin\/npm/);
      assert.equal(fs.existsSync(f.log), false, "non-root branch makes no mutations or lock calls");
    } finally { f.close(); }
  });

  for (const name of ["NODE_OPTIONS", "NODE_PATH", "NODE_V8_COVERAGE", "NODE_COMPILE_CACHE",
    "npm_config_cache", "NPM_CONFIG_USERCONFIG", "nPm_CoNfIg_ScRiPt_ShElL", "LD_PRELOAD",
    "TSX_TSCONFIG_PATH", "BASH_ENV"]) {
    it(`rejects ${name} before any Node or root write`, () => {
      const f = fixture(true);
      try {
        const result = f.run({ [name]: "" });
        assert.notEqual(result.status, 0);
        assert.match(result.stderr, /unsupported loader or npm configuration/);
        assert.equal(fs.existsSync(f.log), false);
      } finally { f.close(); }
    });
  }

  it("rejects unsupported participation, CMD and scratch configurations", () => {
    const f = fixture();
    try {
      const invalid: Record<string, string>[] = [{ UZI_EPOCH_LOCK: "0" }, { UZI_EPOCH_LOCK: "true" },
        { TMPDIR: f.data + "/runner" }, { UZI_EPOCH_RUNTIME_TMPDIR: f.runtime }];
      for (const extra of invalid) {
        const result = f.run(extra);
        assert.notEqual(result.status, 0, result.stderr);
      }
      assert.notEqual(f.run({}, ["node", "src/main.ts"]).status, 0);
    } finally { f.close(); }
  });

  it("saves split runtime scratch and closes temporary SH before setpriv", () => {
    const f = fixture(true);
    try {
      fs.mkdirSync(path.join(f.runtime, "uzi-worker"), { mode: 0o700 });
      fs.chownSync(path.join(f.runtime, "uzi-worker"), uid, gid);
      f.rewrite(text => {
        const prefix = text.slice(0, text.indexOf("# --- (a) B4"));
        const launch = text.slice(text.indexOf('export TMPDIR="$WORKER_TMPDIR"'));
        return (prefix + '\nWORKER_TMPDIR="$DATA_DIR/runner/.tmp/uzi-worker"\nRUNNER_TMPDIR="$DATA_DIR/runner/.tmp/uzi-runner"\n' + launch)
          .replace("SETPRIV=/bin/setpriv", `SETPRIV=${path.join(f.root, "tini")}`);
      });
      const result = f.run();
      assert.equal(result.status, 0, result.stderr);
      assert.ok(result.stdout.includes("runtime=" + f.runtime + "/uzi-worker"));
      assert.ok(result.stdout.includes("bootstrap=" + f.bootstrap));
      assert.equal(fs.readFileSync(f.log, "utf8"), "prepare\nhold\n");
    } finally { f.close(); }
  });

  it("requires exact bootstrap owner/group/mode and real parents", () => {
    const f = fixture();
    try {
      for (const mode of [0o755, 0o2700]) {
        fs.chmodSync(f.bootstrap, mode);
        const result = f.run();
        assert.notEqual(result.status, 0);
        assert.match(result.stderr, /unexpected owner\/mode/);
      }
      fs.chmodSync(f.bootstrap, 0o700);
      f.rewrite(text => text.replace(`s.uid !== ${uid}`, `s.uid !== ${uid + 1}`));
      assert.match(f.run().stderr, /unexpected owner\/mode/);
      f.rewrite(text => text.replace(`s.uid !== ${uid + 1}`, `s.uid !== ${uid}`)
        .replace(`s.gid !== ${gid}`, `s.gid !== ${gid + 1}`));
      assert.match(f.run().stderr, /unexpected owner\/mode/);
      f.rewrite(text => text.replace(`s.gid !== ${gid + 1}`, `s.gid !== ${gid}`));
      fs.renameSync(f.bootstrap, f.bootstrap + "-real");
      fs.symlinkSync(f.bootstrap + "-real", f.bootstrap);
      assert.match(f.run().stderr, /non-real directory/);
    } finally { f.close(); }
  });

  it("rejects data-root fsGroup drift, runtime aliases and npm config", () => {
    const f = fixture();
    try {
      fs.chmodSync(f.data, 0o0770);
      assert.match(f.run().stderr, /fsGroup/);
      fs.chmodSync(f.data, 0o2770);
      fs.writeFileSync(path.join(f.home, ".npmrc"), "cache=/data/cache");
      assert.match(f.run().stderr, /unsupported npm config/);
      fs.unlinkSync(path.join(f.home, ".npmrc"));
      fs.renameSync(f.runtime, f.runtime + "-real");
      fs.symlinkSync(f.runtime + "-real", f.runtime);
      assert.match(f.run().stderr, /non-real directory/);
    } finally { f.close(); }
  });

  it("refuses root writes when temporary SH fails, before even /nix migration", () => {
    const f = fixture(true);
    try {
      const result = f.run({ FAIL_HOLD: "1" });
      assert.notEqual(result.status, 0);
      assert.equal(fs.readFileSync(f.log, "utf8"), "prepare\nhold\n");
      assert.equal(fs.existsSync(path.join(f.data, ".uzi-migrated-worker-worker")), false);
    } finally { f.close(); }
  });

  it("rejects token directory aliases before token metadata writes", () => {
    const f = fixture(true);
    try {
      const alias = path.join(f.root, "token");
      fs.symlinkSync(f.data, alias);
      f.rewrite(text => text.slice(0, text.indexOf("# --- (a) B4")) +
        text.slice(text.indexOf('TOKEN="'), text.indexOf("# --- (b2)")));
      const result = f.run({ UZI_WORKER_TOKEN_FILE: alias });
      assert.notEqual(result.status, 0);
      assert.match(result.stderr, /token target must be a regular file/);
      assert.equal(fs.readFileSync(f.log, "utf8"), "prepare\nhold\n");
    } finally { f.close(); }
  });

  it("migrates descendants including ledger under SH without touching the data root", () => {
    const f = fixture(true);
    try {
      fs.writeFileSync(path.join(f.data, ".uzi-epoch-consumed"), "ledger");
      // Stop after the actual migration window; later initializer behavior has its
      // own entrypoint-migration coverage. No production branch is replaced.
      f.rewrite(text => text.slice(0, text.indexOf("# --- (a1)")) + "\nexec 9>&-\n");
      const before = fs.statSync(f.data);
      const result = f.run();
      assert.equal(result.status, 0, result.stderr);
      const ops = fs.readFileSync(f.log, "utf8").trim().split("\n");
      assert.deepEqual(ops.slice(0, 2), ["prepare", "hold"]);
      assert.ok(ops.includes(`chown -R worker:worker ${f.data}/.uzi-epoch-consumed`));
      assert.ok(!ops.includes(`chown -R worker:worker ${f.data}`));
      assert.equal(fs.statSync(f.data).mode, before.mode);
      assert.equal(fs.statSync(f.data).gid, before.gid);
      assert.equal(fs.statSync(f.data).ino, before.ino);
    } finally { f.close(); }
  });
});

for (const template of ["base", "jvm"]) {
  it(`${template} image bakes util-linux flock and both root-owned helper paths`, () => {
    const text = fs.readFileSync(path.join(repo, `agent/templates/${template}/Dockerfile`), "utf8");
    assert.match(text, /apk add --no-cache flock;/);
    assert.match(text, /\/usr\/bin\/flock --version \| grep -F -q util-linux/);
    assert.match(text, /chown worker:worker \/tmp\/uzi-epoch-bootstrap/);
    assert.match(text, /chmod 0700 \/tmp\/uzi-epoch-bootstrap/);
    for (const target of ["/usr/local/lib/uzi/pod-epoch-init.cjs", "/app/templates/pod-epoch-init.cjs"]) {
      assert.ok(text.includes("COPY --chown=0:0 --chmod=0644 agent/templates/pod-epoch-init.cjs " + target));
    }
    assert.ok(text.indexOf("COPY --chown=0:0") > text.lastIndexOf("chown -R worker:worker /app"));
  });
}
