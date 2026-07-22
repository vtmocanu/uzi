import { describe, it, before, after } from "node:test";
import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

// PRD #87 M1 — the baked crash-close shim (agent/bin/agent-browser). These tests are
// hermetic: a temp dir holds executable stubs, PATH is pointed at a controlled dir, and the
// real-CLI target is redirected to a recording stub via AGENT_BROWSER_SHIM_TARGET (a seam
// used only here). They assert the two load-bearing behaviors from the brief — exit 0 with
// the clear message when NO browser resolves, and transparent exec-passthrough when one does
// — plus the launch-config injection M4 depends on (the sparse SDK env drops the baked
// AGENT_BROWSER_* / FONTCONFIG_FILE, so the shim must re-establish them).

const shim = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../bin/agent-browser");

interface Run {
  code: number;
  stdout: string;
  stderr: string;
}

// Invoke the shim via /bin/sh (so the test never depends on the file's execute bit) with a
// FULLY CONTROLLED env — no inheritance — so the host's own PATH/chromium can't leak in.
function runShim(args: string[], env: Record<string, string>): Promise<Run> {
  return new Promise((resolve) => {
    execFile("/bin/sh", [shim, ...args], { env }, (err, stdout, stderr) => {
      const code = err && typeof (err as { code?: unknown }).code === "number" ? (err as { code: number }).code : err ? 1 : 0;
      resolve({ code, stdout, stderr });
    });
  });
}

let tmp: string;
let emptyBin: string;
let stubExec: string; // a stand-in "chromium" executable (target of AGENT_BROWSER_EXECUTABLE_PATH)
let recorder: string; // records the argv it was exec'd with, then exits 0 (the real-CLI stand-in)
let capture: string; // where the recorder writes its captured argv

function writeExec(p: string, body: string): void {
  fs.writeFileSync(p, body);
  fs.chmodSync(p, 0o755);
}

before(() => {
  tmp = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-abshim-"));
  emptyBin = path.join(tmp, "empty-bin");
  fs.mkdirSync(emptyBin);

  stubExec = path.join(tmp, "chromium-stub");
  writeExec(stubExec, "#!/bin/sh\nexit 0\n");

  capture = path.join(tmp, "captured-args");
  recorder = path.join(tmp, "recorder");
  // Record each received arg on its own line so passthrough (and its absence) is checkable.
  writeExec(recorder, `#!/bin/sh\n: > "${capture}"\nfor a in "$@"; do printf '%s\\n' "$a" >> "${capture}"; done\nexit 0\n`);
});

after(() => {
  fs.rmSync(tmp, { recursive: true, force: true });
});

function captured(): string[] {
  const raw = fs.readFileSync(capture, "utf8");
  return raw.length === 0 ? [] : raw.replace(/\n$/, "").split("\n");
}

describe("agent-browser crash-close shim (PRD #87 M1)", () => {
  it("exits 0 with the clear message when NO browser resolves", async () => {
    const res = await runShim(["open", "example.com"], { PATH: emptyBin });
    assert.equal(res.code, 0, "must exit 0 (clean), not the raw Chromium abort");
    assert.match(res.stderr, /no browser in this runtime — skipping browser validation/);
    // It must degrade BEFORE reaching any real CLI — the recorder must not have run.
    assert.ok(!fs.existsSync(capture), "must not exec the real CLI when no browser resolves");
  });

  it("execs the real CLI, passing args through, when AGENT_BROWSER_EXECUTABLE_PATH resolves", async () => {
    const res = await runShim(["snapshot", "-i", "--json"], {
      PATH: emptyBin,
      AGENT_BROWSER_EXECUTABLE_PATH: stubExec,
      AGENT_BROWSER_SHIM_TARGET: recorder,
    });
    assert.equal(res.code, 0);
    assert.deepEqual(captured(), ["snapshot", "-i", "--json"], "every arg must pass through verbatim");
  });

  it("detects a chromium on PATH (auto-discovery) and execs the real CLI", async () => {
    const chromiumDir = path.join(tmp, "with-chromium");
    fs.mkdirSync(chromiumDir);
    writeExec(path.join(chromiumDir, "chromium"), "#!/bin/sh\nexit 0\n");
    const res = await runShim(["open", "http://localhost:8080"], {
      PATH: chromiumDir,
      AGENT_BROWSER_EXECUTABLE_PATH: path.join(tmp, "does-not-exist"),
      AGENT_BROWSER_SHIM_TARGET: recorder,
    });
    assert.equal(res.code, 0);
    assert.deepEqual(captured(), ["open", "http://localhost:8080"]);
  });

  it("never shadows an explicit --cdp / --executable-path invocation even with no local browser", async () => {
    const res = await runShim(["--cdp", "9222", "snapshot"], {
      PATH: emptyBin,
      AGENT_BROWSER_EXECUTABLE_PATH: path.join(tmp, "does-not-exist"),
      AGENT_BROWSER_SHIM_TARGET: recorder,
    });
    assert.equal(res.code, 0);
    assert.deepEqual(captured(), ["--cdp", "9222", "snapshot"], "an explicit endpoint must not be degraded away");
  });

  it("injects the M4 launch config as DEFAULTS (executable-path, --no-sandbox args, idle timeout, fontconfig)", async () => {
    // The recorder here echoes the launch-config env the shim exported, proving the shim
    // re-establishes what the sparse SDK env drops.
    const envDump = path.join(tmp, "env-dump");
    writeExec(
      recorder,
      `#!/bin/sh\n{ echo "EXEC=$AGENT_BROWSER_EXECUTABLE_PATH"; echo "ARGS=$AGENT_BROWSER_ARGS"; echo "IDLE=$AGENT_BROWSER_IDLE_TIMEOUT_MS"; echo "FC=$FONTCONFIG_FILE"; } > "${envDump}"\nexit 0\n`,
    );
    const res = await runShim(["open", "about:blank"], {
      PATH: emptyBin,
      AGENT_BROWSER_EXECUTABLE_PATH: stubExec, // present so the shim reaches exec
      AGENT_BROWSER_SHIM_TARGET: recorder,
    });
    assert.equal(res.code, 0);
    const dump = fs.readFileSync(envDump, "utf8");
    assert.match(dump, /ARGS=--no-sandbox,--disable-dev-shm-usage/, "must default --no-sandbox (mandatory under PRD #51 hardening) + --disable-dev-shm-usage");
    assert.match(dump, /IDLE=60000/, "must default a sane idle timeout so a wedged daemon self-closes");
    assert.match(dump, /FC=\/etc\/fonts\/fonts\.conf/, "must default FONTCONFIG_FILE so screenshots render fonts, not tofu");
    // restore the recorder for any later use
    writeExec(recorder, `#!/bin/sh\n: > "${capture}"\nfor a in "$@"; do printf '%s\\n' "$a" >> "${capture}"; done\nexit 0\n`);
  });

  it("respects an explicit caller-set AGENT_BROWSER_ARGS (defaults never clobber intent)", async () => {
    const envDump = path.join(tmp, "env-dump2");
    writeExec(recorder, `#!/bin/sh\necho "ARGS=$AGENT_BROWSER_ARGS" > "${envDump}"\nexit 0\n`);
    const res = await runShim(["open", "about:blank"], {
      PATH: emptyBin,
      AGENT_BROWSER_EXECUTABLE_PATH: stubExec,
      AGENT_BROWSER_ARGS: "--headless,--custom-flag",
      AGENT_BROWSER_SHIM_TARGET: recorder,
    });
    assert.equal(res.code, 0);
    assert.match(fs.readFileSync(envDump, "utf8"), /ARGS=--headless,--custom-flag/, "an explicit value must win over the default");
    writeExec(recorder, `#!/bin/sh\n: > "${capture}"\nfor a in "$@"; do printf '%s\\n' "$a" >> "${capture}"; done\nexit 0\n`);
  });

  it("defaults XDG_CONFIG_HOME/XDG_CACHE_HOME to a UID-SCOPED writable dir under TMPDIR and creates them (crashpad rc=133 fix)", async () => {
    // Chromium 150 traps rc=133 at the crashpad handler (BEFORE the SUID check, so --no-sandbox
    // can't help) when it can't create its crashpad DB under a non-writable $HOME/.config — the
    // image bakes /home/worker/.config root-owned, not writable by uid 10001. The shim points
    // the XDG base dirs at a writable location regardless of inherited HOME. mkdir needs a real
    // PATH (the production runner PATH always has /bin); the stub keeps the run browser-free
    // WITHOUT leaking a host browser, since AGENT_BROWSER_EXECUTABLE_PATH short-circuits the
    // PATH probe before any chromium on /usr/bin could match.
    //
    // Issue #114 BUG 2a: the default dir is UID-SCOPED (`uzi-agent-browser-$(id -u)`) so the
    // root (uid 0) build-guard invocation and the runtime uid never collide on one baked
    // root-owned dir. The shim runs here under the TEST process uid, so that is the expected
    // scope suffix.
    const xdgTmp = path.join(tmp, "xdg-tmpdir");
    fs.mkdirSync(xdgTmp);
    const envDump = path.join(tmp, "xdg-dump");
    writeExec(recorder, `#!/bin/sh\n{ echo "CFG=$XDG_CONFIG_HOME"; echo "CACHE=$XDG_CACHE_HOME"; } > "${envDump}"\nexit 0\n`);
    const res = await runShim(["open", "about:blank"], {
      PATH: `${emptyBin}:/usr/bin:/bin`,
      TMPDIR: xdgTmp,
      AGENT_BROWSER_EXECUTABLE_PATH: stubExec,
      AGENT_BROWSER_SHIM_TARGET: recorder,
    });
    assert.equal(res.code, 0);
    const dump = fs.readFileSync(envDump, "utf8");
    const uid = process.getuid!();
    const cfg = path.join(xdgTmp, `uzi-agent-browser-${uid}`, "config");
    const cache = path.join(xdgTmp, `uzi-agent-browser-${uid}`, "cache");
    assert.ok(dump.includes(`CFG=${cfg}`), `XDG_CONFIG_HOME must default under a uid-scoped dir in TMPDIR; got: ${dump}`);
    assert.ok(dump.includes(`CACHE=${cache}`), `XDG_CACHE_HOME must default under a uid-scoped dir in TMPDIR; got: ${dump}`);
    assert.ok(fs.existsSync(cfg), "the config dir must be created (best-effort mkdir)");
    assert.ok(fs.existsSync(cache), "the cache dir must be created (best-effort mkdir)");
    writeExec(recorder, `#!/bin/sh\n: > "${capture}"\nfor a in "$@"; do printf '%s\\n' "$a" >> "${capture}"; done\nexit 0\n`);
  });

  it("respects an explicit XDG_CONFIG_HOME/XDG_CACHE_HOME (defaults never clobber intent)", async () => {
    const envDump = path.join(tmp, "xdg-dump2");
    writeExec(recorder, `#!/bin/sh\n{ echo "CFG=$XDG_CONFIG_HOME"; echo "CACHE=$XDG_CACHE_HOME"; } > "${envDump}"\nexit 0\n`);
    const res = await runShim(["open", "about:blank"], {
      PATH: emptyBin,
      AGENT_BROWSER_EXECUTABLE_PATH: stubExec,
      XDG_CONFIG_HOME: "/custom/cfg",
      XDG_CACHE_HOME: "/custom/cache",
      AGENT_BROWSER_SHIM_TARGET: recorder,
    });
    assert.equal(res.code, 0);
    const dump = fs.readFileSync(envDump, "utf8");
    assert.match(dump, /CFG=\/custom\/cfg\b/, "an explicit XDG_CONFIG_HOME must win over the default");
    assert.match(dump, /CACHE=\/custom\/cache\b/, "an explicit XDG_CACHE_HOME must win over the default");
    writeExec(recorder, `#!/bin/sh\n: > "${capture}"\nfor a in "$@"; do printf '%s\\n' "$a" >> "${capture}"; done\nexit 0\n`);
  });

  it("degrades cleanly when a browser resolves but the baked CLI is absent (never crashes the run)", async () => {
    const res = await runShim(["open", "about:blank"], {
      PATH: emptyBin,
      AGENT_BROWSER_EXECUTABLE_PATH: stubExec,
      AGENT_BROWSER_SHIM_TARGET: path.join(tmp, "no-such-cli"),
    });
    assert.equal(res.code, 0);
    assert.match(res.stderr, /baked agent-browser CLI is missing/);
  });

  it("is committed with the execute bit (so it works when invoked by name on PATH)", () => {
    fs.accessSync(shim, fs.constants.X_OK);
  });
});
