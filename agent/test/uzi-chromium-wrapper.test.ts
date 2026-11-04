import { describe, it, before, after } from "node:test";
import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

// Issue #3907 (PRD #87) — the uzi-chromium launch wrapper (agent/bin/uzi-chromium). Hermetic:
// a stub stands in for Chromium via UZI_CHROMIUM_WRAPPER_TARGET (a seam used only here) and
// records its argv and the env the wrapper established; the wrapper runs under /bin/sh with a
// fully controlled env.

const wrapper = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../bin/uzi-chromium");

interface Run {
  code: number;
  stdout: string;
  stderr: string;
}

let tmp: string;
let stub: string;
let record: string;

function run(args: string[], env: Record<string, string>): Promise<Run> {
  return new Promise((resolve) => {
    execFile("/bin/sh", [wrapper, ...args], { env: { PATH: "/usr/bin:/bin", TMPDIR: tmp, UZI_CHROMIUM_WRAPPER_TARGET: stub, ...env } }, (err, stdout, stderr) => {
      const code = err && typeof (err as { code?: unknown }).code === "number" ? (err as { code: number }).code : err ? 1 : 0;
      resolve({ code, stdout, stderr });
    });
  });
}

interface Seen {
  argv: string[];
  cfg: string;
  cache: string;
  fc: string;
}

function seen(): Seen {
  const lines = fs.readFileSync(record, "utf8").split("\n");
  const argv: string[] = [];
  let cfg = "";
  let cache = "";
  let fc = "";
  for (const l of lines) {
    if (l.startsWith("ARG=")) argv.push(l.slice(4));
    else if (l.startsWith("CFG=")) cfg = l.slice(4);
    else if (l.startsWith("CACHE=")) cache = l.slice(6);
    else if (l.startsWith("FC=")) fc = l.slice(3);
  }
  return { argv, cfg, cache, fc };
}

// Fresh HOME per call so tests never share state.
let homeN = 0;
function newHome(): string {
  const h = path.join(tmp, `home-${homeN++}`);
  fs.mkdirSync(h);
  return h;
}

before(() => {
  tmp = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-chromwrap-"));
  stub = path.join(tmp, "chromium-stub");
  record = path.join(tmp, "record");
  // Args with spaces cannot contain newlines here, so one-per-line is unambiguous.
  fs.writeFileSync(
    stub,
    `#!/bin/sh\n{ for a in "$@"; do printf 'ARG=%s\\n' "$a"; done; echo "CFG=$XDG_CONFIG_HOME"; echo "CACHE=$XDG_CACHE_HOME"; echo "FC=$FONTCONFIG_FILE"; } > "${record}"\nexit 0\n`,
  );
  fs.chmodSync(stub, 0o755);
});

after(() => {
  fs.rmSync(tmp, { recursive: true, force: true });
});

describe("uzi-chromium wrapper (issue #3907)", () => {
  it("passes the mandatory flags, an injected profile under HOME/.uzi-chromium, then caller args verbatim", async () => {
    const home = newHome();
    const res = await run(["--headless", "--window-size=320,240", "data:text/html,<p>a b</p>", "two words"], { HOME: home });
    assert.equal(res.code, 0, res.stderr);
    const s = seen();
    assert.equal(s.argv[0], "--no-sandbox");
    assert.equal(s.argv[1], "--disable-dev-shm-usage");
    const m = /^--user-data-dir=(.*)$/.exec(s.argv[2] ?? "");
    assert.ok(m, `third arg must be the injected profile; got ${JSON.stringify(s.argv)}`);
    const profile = m[1] ?? "";
    assert.equal(path.dirname(profile), path.join(home, ".uzi-chromium"));
    assert.match(path.basename(profile), /^profile\./);
    assert.ok(fs.statSync(profile).isDirectory(), "the injected profile dir must exist");
    assert.deepEqual(s.argv.slice(3), ["--headless", "--window-size=320,240", "data:text/html,<p>a b</p>", "two words"]);
    assert.equal(s.cfg, path.join(home, ".uzi-chromium", "config"));
    assert.equal(s.cache, path.join(home, ".uzi-chromium", "cache"));
    assert.ok(fs.existsSync(s.cfg) && fs.existsSync(s.cache), "XDG dirs must be created");
  });

  it("keeps a caller-supplied `--user-data-dir X` (two args) and injects no second profile", async () => {
    const home = newHome();
    const res = await run(["--user-data-dir", "/some/profile", "about:blank"], { HOME: home });
    assert.equal(res.code, 0, res.stderr);
    assert.deepEqual(seen().argv, ["--no-sandbox", "--disable-dev-shm-usage", "--user-data-dir", "/some/profile", "about:blank"]);
    assert.ok(!fs.readdirSync(path.join(home, ".uzi-chromium")).some((n) => n.startsWith("profile.")), "no profile dir should be created");
  });

  it("keeps a caller-supplied `--user-data-dir=X` and injects no second profile", async () => {
    const home = newHome();
    const res = await run(["--user-data-dir=/some/profile", "about:blank"], { HOME: home });
    assert.equal(res.code, 0, res.stderr);
    assert.deepEqual(seen().argv, ["--no-sandbox", "--disable-dev-shm-usage", "--user-data-dir=/some/profile", "about:blank"]);
  });

  it("gives two invocations under one HOME distinct profiles, both under HOME/.uzi-chromium", async () => {
    const home = newHome();
    await run(["about:blank"], { HOME: home });
    const p1 = seen().argv[2] ?? "";
    await run(["about:blank"], { HOME: home });
    const p2 = seen().argv[2] ?? "";
    assert.notEqual(p1, p2);
    for (const p of [p1, p2]) assert.ok(p.startsWith(`--user-data-dir=${path.join(home, ".uzi-chromium")}/profile.`), p);
  });

  it("gives two HOMEs disjoint bases", async () => {
    const h1 = newHome();
    const h2 = newHome();
    await run(["about:blank"], { HOME: h1 });
    const a = seen();
    await run(["about:blank"], { HOME: h2 });
    const b = seen();
    assert.ok(a.cfg.startsWith(h1) && b.cfg.startsWith(h2));
    assert.ok(!b.cfg.startsWith(h1) && !a.cfg.startsWith(h2));
    assert.notEqual(a.argv[2], b.argv[2]);
  });

  it("falls back to a per-invocation mktemp base under TMPDIR when HOME is unset", async () => {
    const t = path.join(tmp, "fallback-unset");
    fs.mkdirSync(t);
    await run(["about:blank"], { TMPDIR: t });
    const a = seen();
    await run(["about:blank"], { TMPDIR: t });
    const b = seen();
    const re = new RegExp(`^${t}/uzi-chromium\\.[^/]+/config$`);
    assert.match(a.cfg, re);
    assert.match(b.cfg, re);
    assert.notEqual(a.cfg, b.cfg, "the fallback base must differ per invocation");
    assert.match(a.argv[2] ?? "", new RegExp(`^--user-data-dir=${t}/uzi-chromium\\.[^/]+/profile\\.`));
  });

  it("falls back to TMPDIR when HOME is empty", async () => {
    const t = path.join(tmp, "fallback-empty");
    fs.mkdirSync(t);
    await run(["about:blank"], { HOME: "", TMPDIR: t });
    assert.match(seen().cfg, new RegExp(`^${t}/uzi-chromium\\.`));
  });

  it("falls back to TMPDIR when HOME is not writable", async (tc) => {
    if (process.getuid!() === 0) {
      tc.skip("root can write a 0555 dir");
      return;
    }
    const ro = newHome();
    fs.chmodSync(ro, 0o555);
    const t = path.join(tmp, "fallback-ro");
    fs.mkdirSync(t);
    try {
      await run(["about:blank"], { HOME: ro, TMPDIR: t });
      const a = seen();
      await run(["about:blank"], { HOME: ro, TMPDIR: t });
      const b = seen();
      assert.match(a.cfg, new RegExp(`^${t}/uzi-chromium\\.[^/]+/config$`));
      assert.notEqual(a.cfg, b.cfg);
      assert.ok(!fs.existsSync(path.join(ro, ".uzi-chromium")));
    } finally {
      fs.chmodSync(ro, 0o755);
    }
  });

  it("falls back to TMPDIR when HOME is writable but an existing .uzi-chromium is not", async (tc) => {
    if (process.getuid!() === 0) {
      tc.skip("root can write a 0555 dir");
      return;
    }
    const home = newHome();
    const stale = path.join(home, ".uzi-chromium");
    fs.mkdirSync(stale);
    fs.chmodSync(stale, 0o555);
    const t = path.join(tmp, "fallback-stale");
    fs.mkdirSync(t);
    try {
      const r = await run(["about:blank"], { HOME: home, TMPDIR: t });
      assert.equal(r.code, 0, r.stderr);
      assert.match(seen().cfg, new RegExp(`^${t}/uzi-chromium\\.[^/]+/config$`));
    } finally {
      fs.chmodSync(stale, 0o755);
    }
  });

  it("lets caller-set XDG_CONFIG_HOME, XDG_CACHE_HOME and FONTCONFIG_FILE win", async () => {
    await run(["about:blank"], { HOME: newHome(), XDG_CONFIG_HOME: "/c/cfg", XDG_CACHE_HOME: "/c/cache", FONTCONFIG_FILE: "/c/fonts.conf" });
    const s = seen();
    assert.equal(s.cfg, "/c/cfg");
    assert.equal(s.cache, "/c/cache");
    assert.equal(s.fc, "/c/fonts.conf");
  });

  it("defaults FONTCONFIG_FILE to /etc/fonts/fonts.conf", async () => {
    await run(["about:blank"], { HOME: newHome() });
    assert.equal(seen().fc, "/etc/fonts/fonts.conf");
  });

  it("exits 127 with a clear stderr message, without running anything, when the baked Chromium is missing", async () => {
    fs.rmSync(record, { force: true });
    const missing = path.join(tmp, "no-such-chromium");
    const res = await run(["about:blank"], { HOME: newHome(), UZI_CHROMIUM_WRAPPER_TARGET: missing });
    assert.equal(res.code, 127);
    assert.match(res.stderr, new RegExp(`uzi-chromium: baked Chromium not found at ${missing}`));
    assert.ok(!fs.existsSync(record), "the stub must not have run");
  });

  it("is committed with the execute bit", () => {
    fs.accessSync(wrapper, fs.constants.X_OK);
  });
});
