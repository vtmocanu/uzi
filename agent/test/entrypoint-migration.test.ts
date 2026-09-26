// PRD #1493 M2 — BEHAVIOURAL coverage of the ROOT (uid-split) branch of the worker
// entrypoint's k8s-safe migration. The four couplings (read-only token, docker-lane TMPDIR,
// legacy /data migration, sticky carve-out) plus the PVC-root fsGroup alignment are exercised
// by RUNNING the real `agent/templates/entrypoint.sh` with:
//   * `id` stubbed to `echo 0`, so the ROOT branch is taken regardless of who runs the suite;
//   * the mutating binaries (chown/chmod/mkdir) and busybox (stat/cat) and setpriv/tini
//     stubbed, so the branch runs to completion under uid 10001 without needing real root;
//   * the volume roots + token path pointed at a sandbox via the entrypoint's own
//     DATA_DIR/NIX_DIR/TOKEN constants (the same string-replace seam the issue-#120 test uses
//     for ID/TINI).
//
// Group 1 runs under ANY uid: the stubs RECORD every chown/chmod/mkdir to an op-log and apply
// nothing (STUB_NOOP), so the ownership MAP and the split env are asserted from the log +
// captured env, and content survival is proven by the absence of any destructive op.
//
// Group 2 (gated on running AS WORKER_UID with WORKER+RUNNER membership, like
// codex-shared-dir.test.ts Group B) lets the stubs apply the ops best-effort (real chgrp
// succeeds; a give-away chown to another uid EPERMs and is recorded only), then makes REAL
// filesystem assertions: the migrated run HOME + codex-data validate through the production
// `ensureCodexSharedDirectory`, the worker-private codex-session-store is byte-identical and
// still worker:worker 0700/0600, and resume state survives.

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { ensureCodexSharedDirectory } from "../src/codex/codex-executor.js";
import { WORKER_UID, RUNNER_UID } from "../src/runner-uid.js";

const entrypointPath = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "../templates/entrypoint.sh",
);
const entrypointText = fs.readFileSync(entrypointPath, "utf8");

const MODE_MASK = 0o7777;

interface RunResult {
  status: number | null;
  stdout: string;
  stderr: string;
  ops: string[];
  env: Map<string, string>;
}

/** Write an executable stub. */
function writeStub(dir: string, name: string, body: string): string {
  const p = path.join(dir, name);
  fs.writeFileSync(p, body, { mode: 0o755 });
  return p;
}

/** A harness dir holding the sandbox volume roots, the stubs, and the op-log. */
interface Harness {
  root: string;
  data: string;
  nix: string;
  token: string;
  stubDir: string;
  opLog: string;
  script: string;
}

// tokenViaEnv (issue #1761): leave the entrypoint's TOKEN line as shipped, so the token
// path can only reach the script through UZI_WORKER_TOKEN_FILE (run() passes it). Proves
// the posture checks follow the env rather than a /run/secrets literal.
function makeHarness(opts: { mutate?: (patched: string) => string; tokenViaEnv?: boolean } = {}): Harness {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-entrypoint-m2-"));
  const data = path.join(root, "data");
  const nix = path.join(root, "nix");
  const token = path.join(root, "token");
  const stubDir = path.join(root, "stubs");
  const opLog = path.join(root, "ops.log");
  const codexCmdCache = path.join(root, "codex-cmd-cache");
  fs.mkdirSync(stubDir);
  fs.writeFileSync(opLog, "");

  // id -> echo 0 (force the ROOT branch under any uid).
  writeStub(stubDir, "id", "#!/bin/sh\necho 0\n");
  // tini -> dump env (the drop's exported env is what we inspect).
  writeStub(stubDir, "tini", "#!/bin/sh\nenv\n");
  // setpriv -> skip its own flags to `--`, then exec the target (so the token read-test runs
  // `busybox cat` and the final drop runs the tini stub).
  writeStub(
    stubDir,
    "setpriv",
    '#!/bin/sh\nwhile [ "$#" -gt 0 ] && [ "$1" != "--" ]; do shift; done\n[ "$1" = "--" ] && shift\nexec "$@"\n',
  );
  // chown/chmod/mkdir -> record to $OPLOG; apply best-effort unless $STUB_NOOP; $STUB_ROFS_TOKEN
  // makes `chown 0:0 <that path>` fail like an EROFS read-only Secret mount.
  writeStub(
    stubDir,
    "chown",
    '#!/bin/sh\nprintf "chown %s\\n" "$*" >> "$OPLOG"\n' +
      'if [ -n "${STUB_ROFS_TOKEN:-}" ] && [ "$1" = "0:0" ] && [ "$2" = "$STUB_ROFS_TOKEN" ]; then exit 1; fi\n' +
      '[ -n "${STUB_NOOP:-}" ] && exit 0\n/bin/chown "$@" 2>/dev/null || true\nexit 0\n',
  );
  writeStub(
    stubDir,
    "chmod",
    '#!/bin/sh\nprintf "chmod %s\\n" "$*" >> "$OPLOG"\n' +
      '[ -n "${STUB_NOOP:-}" ] && exit 0\n/bin/chmod "$@" 2>/dev/null || true\nexit 0\n',
  );
  writeStub(
    stubDir,
    "mkdir",
    '#!/bin/sh\nprintf "mkdir %s\\n" "$*" >> "$OPLOG"\n' +
      '[ -n "${STUB_NOOP:-}" ] && exit 0\n/bin/mkdir "$@" 2>/dev/null || true\nexit 0\n',
  );
  writeStub(
    stubDir,
    "rm",
    '#!/bin/sh\nprintf "rm %s\\n" "$*" >> "$OPLOG"\n' +
      '[ -n "${STUB_NOOP:-}" ] && exit 0\n/bin/rm "$@" 2>/dev/null || true\nexit 0\n',
  );
  // busybox stub. `stat` inspects its flags: with -L (dereferenced) it returns the TARGET
  // posture (STUB_TOKEN_TARGET_POSTURE, default a valid kube posture) but fails (exit 1) on a
  // dangling chain ([ -e ] false) or when STUB_TOKEN_STAT_FAIL is set; without -L (lstat) it
  // returns the symlink's own 0777 (STUB_TOKEN_LINK_POSTURE). `cat` honors STUB_TOKEN_UNREADABLE
  // (exit 1) as the dropped-worker read-denial seam, else reads for real. Issue #1696: a
  // `stat -c %h <path>` (the dot loops' link-count probe) is answered with the REAL link count,
  // read from `ls -ld` field 2 (no symlink follow, like `stat -c %h`). That field is the link
  // count on GNU, BusyBox and BSD alike; the host's own `stat` is not portable (BSD/macOS
  // `stat` rejects `-c`). Every other stat call keeps the token-posture behaviour above unchanged.
  writeStub(
    stubDir,
    "busybox",
    '#!/bin/sh\nsub=$1; shift\ncase "$sub" in\n' +
      "  stat)\n" +
      '    if [ "$#" -eq 3 ] && [ "$1" = "-c" ] && [ "$2" = "%h" ]; then\n' +
      '      n=$(ls -ld -- "$3" 2>/dev/null | awk \'{print $2}\')\n' +
      '      case "$n" in ""|*[!0-9]*) exit 1 ;; esac\n' +
      '      printf "%s\\n" "$n"\n' +
      "      exit 0\n" +
      "    fi\n" +
      "    follow=\n" +
      '    for a in "$@"; do [ "$a" = "-L" ] && follow=1; done\n' +
      '    for p in "$@"; do target=$p; done\n' +
      '    if [ -n "$follow" ]; then\n' +
      '      [ -n "${STUB_TOKEN_STAT_FAIL:-}" ] && exit 1\n' +
      '      [ -e "$target" ] || exit 1\n' +
      '      printf "%s\\n" "${STUB_TOKEN_TARGET_POSTURE:-0 10001 440}"\n' +
      "    else\n" +
      '      printf "%s\\n" "${STUB_TOKEN_LINK_POSTURE:-0 10001 777}"\n' +
      "    fi\n" +
      "    ;;\n" +
      "  cat)\n" +
      '    [ -n "${STUB_TOKEN_UNREADABLE:-}" ] && exit 1\n' +
      '    /bin/cat "$@" 2>/dev/null || exit 1\n' +
      "    ;;\n" +
      "  *) exit 1 ;;\nesac\n",
  );

  const script = path.join(root, "entrypoint.sh");
  const patched = entrypointText
    .replace("ID=/usr/bin/id", `ID=${path.join(stubDir, "id")}`)
    .replace("TINI=/sbin/tini", `TINI=${path.join(stubDir, "tini")}`)
    .replace("SETPRIV=/bin/setpriv", `SETPRIV=${path.join(stubDir, "setpriv")}`)
    .replace("CHOWN=/bin/chown", `CHOWN=${path.join(stubDir, "chown")}`)
    .replace("CHMOD=/bin/chmod", `CHMOD=${path.join(stubDir, "chmod")}`)
    .replace("MKDIR=/bin/mkdir", `MKDIR=${path.join(stubDir, "mkdir")}`)
    .replace("RM=/bin/rm", `RM=${path.join(stubDir, "rm")}`)
    .replace("BUSYBOX=/bin/busybox", `BUSYBOX=${path.join(stubDir, "busybox")}`)
    .replace("DATA_DIR=/data", `DATA_DIR=${data}`)
    .replace("NIX_DIR=/nix", `NIX_DIR=${nix}`)
    // Issue #1598: keep the Codex command-cache root inside the sandbox, never the host's
    // /var/cache. Its own behaviour is covered by entrypoint-codex-cache.test.ts.
    .replace("CODEX_CMD_CACHE_DIR=/var/cache/uzi-codex-cmd", `CODEX_CMD_CACHE_DIR=${codexCmdCache}`)
    .replace(opts.tokenViaEnv ? "\u0000no-match" : 'TOKEN="${UZI_WORKER_TOKEN_FILE:-/run/secrets/worker_token}"', `TOKEN=${token}`);
  // Every constant we depend on must actually have been rewritten.
  for (const marker of [stubDir, data, nix, ...(opts.tokenViaEnv ? [] : [token]), codexCmdCache]) {
    assert.ok(patched.includes(marker), `entrypoint patch did not apply for ${marker}`);
  }
  const finalText = opts.mutate ? opts.mutate(patched) : patched;
  fs.writeFileSync(script, finalText, { mode: 0o755 });
  return { root, data, nix, token, stubDir, opLog, script };
}

/** Build a Kubernetes atomic-writer-style symlink chain so h.token is the top symlink. */
function buildAtomicWriterChain(h: Harness, body = "join-token-body"): void {
  const dir = path.dirname(h.token);
  const base = path.basename(h.token);
  const realDir = path.join(dir, "..2026_09_21_00_00_00.000000000");
  fs.mkdirSync(realDir);
  fs.writeFileSync(path.join(realDir, base), body);
  fs.symlinkSync("..2026_09_21_00_00_00.000000000", path.join(dir, "..data"));
  fs.symlinkSync(path.join("..data", base), h.token);
}

function run(h: Harness, extraEnv: Record<string, string> = {}): RunResult {
  const r = spawnSync("/bin/sh", [h.script, "npm", "run", "start"], {
    encoding: "utf8",
    env: {
      // A minimal env: the entrypoint sets its own PATH in the root branch, so the outer PATH
      // only matters until then; keep the real one so `/bin/sh` and the stubs resolve.
      PATH: process.env.PATH ?? "/usr/bin:/bin",
      HOME: h.root,
      OPLOG: h.opLog,
      ...extraEnv,
    },
  });
  const env = new Map<string, string>(
    (r.stdout ?? "")
      .split("\n")
      .filter((l) => l.includes("="))
      .map((l) => [l.slice(0, l.indexOf("=")), l.slice(l.indexOf("=") + 1)] as [string, string]),
  );
  const ops = fs.existsSync(h.opLog)
    ? fs.readFileSync(h.opLog, "utf8").split("\n").filter((l) => l.trim() !== "")
    : [];
  return { status: r.status, stdout: r.stdout ?? "", stderr: r.stderr ?? "", ops, env };
}

/** True if some recorded op line contains ALL of the given fragments. */
function opMatches(ops: string[], ...fragments: string[]): boolean {
  return ops.some((op) => fragments.every((f) => op.includes(f)));
}

/** The last whitespace-delimited token of an op line (its path argument; test paths have no spaces). */
function opPathArg(op: string): string {
  const parts = op.trim().split(/\s+/);
  return parts[parts.length - 1] ?? "";
}

/**
 * Every op line for `verb` (chown OR chmod, recursive or not) whose path argument's REAL
 * (symlink-resolved) path is repos/ itself or under it — catching a root symlink dereferenced
 * ONTO repos/ (the chmod-dereference hole) as well as a mid-path prefix descending through it.
 */
function opsReachingRepos(ops: string[], reposReal: string, verb: "chown" | "chmod"): string[] {
  const hits: string[] = [];
  for (const op of ops) {
    if (!op.startsWith(verb + " ")) continue;
    let resolved: string;
    try {
      resolved = fs.realpathSync(opPathArg(op));
    } catch {
      continue; // no longer resolvable => cannot reach repos
    }
    if (resolved === reposReal || resolved.startsWith(reposReal + path.sep)) hits.push(`${op} -> ${resolved}`);
  }
  return hits;
}

// ─── Group 1: portable (any uid) — the ownership MAP + env, from the op-log ──────────────

describe("PRD #1493 M2: root-branch migration ownership map (portable, record-only)", () => {
  it("token (AC4): a READ-ONLY kube Secret symlink chain is DEREFERENCED and verified, NOT aborted", () => {
    const h = makeHarness();
    try {
      fs.mkdirSync(h.data);
      fs.mkdirSync(h.nix);
      buildAtomicWriterChain(h);
      // STUB_ROFS_TOKEN forces the read-only (else) branch; the default target posture is valid.
      const r = run(h, { STUB_NOOP: "1", STUB_ROFS_TOKEN: h.token });
      assert.equal(r.status, 0, `read-only token must NOT abort the entrypoint (stderr: ${r.stderr})`);
      assert.match(r.stderr, /read-only kube Secret .*worker-readable/, "must log the accepted read-only posture");
      // The split still activates (we reached the drop's env dump).
      assert.equal(r.env.get("UZI_UID_SPLIT"), "1", "the split must still activate after the read-only token path");
    } finally {
      fs.rmSync(h.root, { recursive: true, force: true });
    }
  });

  it("token (AC5): MUTATION CONTROL — reverting `stat -L` to lstat sees the symlink's own 0777 and fails closed", () => {
    // Prove `-L` is load-bearing: with lstat (no -L) the posture read returns the atomic-writer
    // symlink's OWN 0777 rather than the target's 0440, so the fail-closed check rejects it.
    const h = makeHarness({ mutate: (t) => t.replace("stat -L -c", "stat -c") });
    try {
      fs.mkdirSync(h.data);
      fs.mkdirSync(h.nix);
      buildAtomicWriterChain(h);
      const r = run(h, { STUB_NOOP: "1", STUB_ROFS_TOKEN: h.token });
      assert.notEqual(r.status, 0, "lstat of the projected-Secret symlink must fail closed");
      assert.match(r.stderr, /refusing to start \(posture:/, "must log the fail-closed refusal");
      assert.ok(r.stderr.includes("0 10001 777"), "the observed symlink lstat posture proves -L is load-bearing");
    } finally {
      fs.rmSync(h.root, { recursive: true, force: true });
    }
  });

  it("token (#1761): the posture check FOLLOWS UZI_WORKER_TOKEN_FILE to a relocated Secret", () => {
    // OpenShift mounts the join-token Secret away from /run/secrets (CRI-O shadows that path)
    // and the controller points UZI_WORKER_TOKEN_FILE at it. The TOKEN line is left as
    // shipped, so only the env can lead the entrypoint to the chain.
    const h = makeHarness({ tokenViaEnv: true });
    try {
      fs.mkdirSync(h.data);
      fs.mkdirSync(h.nix);
      buildAtomicWriterChain(h);
      const r = run(h, { STUB_NOOP: "1", STUB_ROFS_TOKEN: h.token, UZI_WORKER_TOKEN_FILE: h.token });
      assert.equal(r.status, 0, `a valid relocated token must not abort (stderr: ${r.stderr})`);
      assert.match(r.stderr, /read-only kube Secret .*worker-readable/, "the posture check must run on the env-named path");
    } finally {
      fs.rmSync(h.root, { recursive: true, force: true });
    }
  });

  it("token (#1761): a relocated Secret with a WRONG posture still FAILS CLOSED", () => {
    const h = makeHarness({ tokenViaEnv: true });
    try {
      fs.mkdirSync(h.data);
      fs.mkdirSync(h.nix);
      buildAtomicWriterChain(h);
      const r = run(h, { STUB_NOOP: "1", STUB_ROFS_TOKEN: h.token, UZI_WORKER_TOKEN_FILE: h.token, STUB_TOKEN_TARGET_POSTURE: "0 0 444" });
      assert.notEqual(r.status, 0, "a world-readable relocated token must fail closed");
      assert.match(r.stderr, /refusing to start \(posture:/, "must log the fail-closed refusal at the relocated path");
    } finally {
      fs.rmSync(h.root, { recursive: true, force: true });
    }
  });

  it("token (AC3): FAILS CLOSED on a chain whose TARGET owner is wrong", () => {
    const h = makeHarness();
    try {
      fs.mkdirSync(h.data);
      fs.mkdirSync(h.nix);
      buildAtomicWriterChain(h);
      const r = run(h, { STUB_NOOP: "1", STUB_ROFS_TOKEN: h.token, STUB_TOKEN_TARGET_POSTURE: "1 10001 440" });
      assert.notEqual(r.status, 0, "a wrong target owner must fail closed (non-zero exit)");
      assert.match(r.stderr, /refusing to start \(posture:/, "must log the fail-closed refusal");
    } finally {
      fs.rmSync(h.root, { recursive: true, force: true });
    }
  });

  it("token (AC3): FAILS CLOSED on a chain whose TARGET group is wrong", () => {
    const h = makeHarness();
    try {
      fs.mkdirSync(h.data);
      fs.mkdirSync(h.nix);
      buildAtomicWriterChain(h);
      const r = run(h, { STUB_NOOP: "1", STUB_ROFS_TOKEN: h.token, STUB_TOKEN_TARGET_POSTURE: "0 0 440" });
      assert.notEqual(r.status, 0, "a wrong target group must fail closed (non-zero exit)");
      assert.match(r.stderr, /refusing to start \(posture:/, "must log the fail-closed refusal");
    } finally {
      fs.rmSync(h.root, { recursive: true, force: true });
    }
  });

  it("token (AC3): FAILS CLOSED on a chain whose TARGET mode is wrong", () => {
    const h = makeHarness();
    try {
      fs.mkdirSync(h.data);
      fs.mkdirSync(h.nix);
      buildAtomicWriterChain(h);
      const r = run(h, { STUB_NOOP: "1", STUB_ROFS_TOKEN: h.token, STUB_TOKEN_TARGET_POSTURE: "0 10001 400" });
      assert.notEqual(r.status, 0, "a wrong target mode must fail closed (non-zero exit)");
      assert.match(r.stderr, /refusing to start \(posture:/, "must log the fail-closed refusal");
    } finally {
      fs.rmSync(h.root, { recursive: true, force: true });
    }
  });

  it("token (AC3/AC6): a dereference/stat FAILURE on an otherwise-valid chain fails closed", () => {
    // Distinct from the dangling case below: the chain fully resolves, but the `stat -L` itself
    // fails. An empty/non-accepted posture must fail closed, not be treated as valid.
    const h = makeHarness();
    try {
      fs.mkdirSync(h.data);
      fs.mkdirSync(h.nix);
      buildAtomicWriterChain(h);
      const r = run(h, { STUB_NOOP: "1", STUB_ROFS_TOKEN: h.token, STUB_TOKEN_STAT_FAIL: "1" });
      assert.notEqual(r.status, 0, "a stat/dereference failure must fail closed (non-zero exit)");
      assert.match(r.stderr, /refusing to start \(posture:/, "must log the fail-closed refusal");
    } finally {
      fs.rmSync(h.root, { recursive: true, force: true });
    }
  });

  it("token (AC1): a DANGLING symlink is routed into the fail-closed branch by the new `[ -L ]` outer guard", () => {
    // No ..data target exists, so `[ -e "$TOKEN" ]` is false; the added `[ -L "$TOKEN" ]` outer
    // guard is what routes the dangling link into validation instead of skipping it (fail-open).
    const h = makeHarness();
    try {
      fs.mkdirSync(h.data);
      fs.mkdirSync(h.nix);
      fs.symlinkSync("..data/token", h.token); // dangling: no ..data created
      const r = run(h, { STUB_NOOP: "1", STUB_ROFS_TOKEN: h.token });
      assert.notEqual(r.status, 0, "a dangling token symlink must fail closed (non-zero exit)");
      assert.match(r.stderr, /refusing to start \(posture:/, "must log the fail-closed refusal");
    } finally {
      fs.rmSync(h.root, { recursive: true, force: true });
    }
  });

  it("token (AC6): FAILS CLOSED when the target posture is valid but the worker read-proof fails", () => {
    const h = makeHarness();
    try {
      fs.mkdirSync(h.data);
      fs.mkdirSync(h.nix);
      buildAtomicWriterChain(h);
      const r = run(h, { STUB_NOOP: "1", STUB_ROFS_TOKEN: h.token, STUB_TOKEN_UNREADABLE: "1" });
      assert.notEqual(r.status, 0, "an unreadable target must fail closed even with a valid posture");
      assert.match(r.stderr, /refusing to start \(posture:/, "must log the fail-closed refusal");
    } finally {
      fs.rmSync(h.root, { recursive: true, force: true });
    }
  });

  it("token: the COMPOSE (writable) path keeps reclaim -> chmod 0400 -> hand-over unchanged", () => {
    const h = makeHarness();
    try {
      fs.mkdirSync(h.data);
      fs.mkdirSync(h.nix);
      fs.writeFileSync(h.token, "join-token-body");
      // STUB_ROFS_TOKEN unset -> `chown 0:0 token` succeeds -> the compose branch runs.
      const r = run(h, { STUB_NOOP: "1" });
      assert.equal(r.status, 0, `compose token path must succeed (stderr: ${r.stderr})`);
      const reclaim = r.ops.findIndex((o) => o.startsWith("chown 0:0 ") && o.includes(h.token));
      const chmod0400 = r.ops.findIndex((o) => o.startsWith("chmod 0400 ") && o.includes(h.token));
      const handover = r.ops.findIndex((o) => o.startsWith("chown worker:worker ") && o.includes(h.token));
      assert.ok(reclaim >= 0 && chmod0400 >= 0 && handover >= 0, "compose token needs reclaim + chmod 0400 + hand-over");
      assert.ok(reclaim < chmod0400 && chmod0400 < handover, "order must be reclaim -> chmod 0400 -> hand-over");
    } finally {
      fs.rmSync(h.root, { recursive: true, force: true });
    }
  });

  it("tmpdir: reclaims prior-boot entries before removing them through a sticky parent", () => {
    const h = makeHarness();
    try {
      fs.mkdirSync(h.data);
      fs.mkdirSync(h.nix);
      fs.writeFileSync(h.token, "t");
      const sharedTmp = path.join(h.root, "shared-tmp");
      const dirs = [
        [path.join(sharedTmp, "uzi-worker"), "worker:worker"],
        [path.join(sharedTmp, "uzi-runner"), "runner:runner"],
      ] as const;
      // Two-boot equivalent: the prior boot left each persistent entry owned by its runtime uid.
      // Include attacker-controlled nested sticky content: reclaiming only the top-level entry
      // is insufficient without CAP_FOWNER because rm cannot unlink another owner inside it.
      for (const [dir] of dirs) fs.mkdirSync(dir, { recursive: true });
      const nestedSticky = path.join(dirs[1][0], "sticky");
      fs.mkdirSync(nestedSticky);
      fs.chmodSync(nestedSticky, 0o1777);
      fs.writeFileSync(path.join(nestedSticky, "runner-owned"), "scratch");
      const r = run(h, { STUB_NOOP: "1", TMPDIR: sharedTmp });
      assert.equal(r.status, 0, `persistent tmpdir restart must succeed (stderr: ${r.stderr})`);
      for (const [dir, owner] of dirs) {
        const preclaim = r.ops.findIndex((o) => o.startsWith("chown -h 0:0 ") && o.includes(dir));
        const rm = r.ops.findIndex((o) => o.startsWith("rm ") && o.includes(dir));
        const mkdir = r.ops.findIndex((o) => o.startsWith("mkdir ") && o.includes(dir));
        const reclaim = r.ops.findIndex((o, i) => i > mkdir && o.startsWith("chown 0:0 ") && o.includes(dir));
        const chmod0700 = r.ops.findIndex((o, i) => i > reclaim && o.startsWith("chmod 0700 ") && o.includes(dir));
        const handover = r.ops.findIndex((o) => o.startsWith(`chown ${owner} `) && o.includes(dir));
        assert.ok(
          preclaim >= 0 && rm >= 0 && mkdir >= 0 && reclaim >= 0 && chmod0700 >= 0 && handover >= 0,
          `${dir} needs preclaim + rm + mkdir + reclaim + chmod 0700 + hand-over (${owner})`,
        );
        assert.ok(preclaim < rm, `${dir}: no-dereference reclaim must precede sticky-parent removal`);
        assert.ok(rm < mkdir, `${dir}: rm must precede mkdir`);
        assert.ok(mkdir < reclaim, `${dir}: mkdir must precede the root reclaim`);
        assert.ok(reclaim < chmod0700, `${dir}: chown 0:0 must precede chmod 0700`);
        assert.ok(chmod0700 < handover, `${dir}: chmod 0700 must precede the owner hand-over`);
      }
      const nestedReclaim = r.ops.findIndex((o) => o.startsWith("chown 0:0 ") && o.includes(nestedSticky));
      const nestedChmod = r.ops.findIndex((o) => o.startsWith("chmod 0700 ") && o.includes(nestedSticky));
      const runnerRm = r.ops.findIndex((o) => o.startsWith("rm ") && o.includes(dirs[1][0]));
      assert.ok(nestedReclaim >= 0 && nestedChmod >= 0, "nested sticky directories must be reclaimed and de-stickied");
      assert.ok(nestedReclaim < nestedChmod && nestedChmod < runnerRm, "nested directory hardening must precede recursive removal");
    } finally {
      fs.rmSync(h.root, { recursive: true, force: true });
    }
  });

  it("tmpdir: an ambient (pod) TMPDIR derives BOTH private tmpdirs beneath it and exports them", () => {
    const h = makeHarness();
    try {
      fs.mkdirSync(h.data);
      fs.mkdirSync(h.nix);
      fs.writeFileSync(h.token, "t");
      const workdir = path.join(h.data, "runner"); // the DinD-shared workdir the pod pre-sets
      const r = run(h, { STUB_NOOP: "1", TMPDIR: workdir });
      assert.equal(r.status, 0, `ambient-TMPDIR run must succeed (stderr: ${r.stderr})`);
      // Both private tmpdirs are created BENEATH the ambient TMPDIR (bind sources resolve in
      // the DinD-shared workdir), and the worker's is exported as TMPDIR across the drop.
      assert.ok(opMatches(r.ops, "mkdir", `${workdir}/uzi-worker`, `${workdir}/uzi-runner`), "tmpdirs must derive beneath the ambient TMPDIR");
      assert.equal(r.env.get("TMPDIR"), `${workdir}/uzi-worker`, "TMPDIR must be the derived worker tmp");
      assert.equal(r.env.get("UZI_RUNNER_TMPDIR"), `${workdir}/uzi-runner`, "UZI_RUNNER_TMPDIR must be the derived runner tmp");
    } finally {
      fs.rmSync(h.root, { recursive: true, force: true });
    }
  });

  it("tmpdir: with NO ambient TMPDIR (compose) the tmpdirs stay /tmp/uzi-worker + /tmp/uzi-runner", () => {
    const h = makeHarness();
    try {
      fs.mkdirSync(h.data);
      fs.mkdirSync(h.nix);
      fs.writeFileSync(h.token, "t");
      // STUB_NOOP so the /tmp targets are only RECORDED, never really created (never touch a
      // live worker's /tmp/uzi-*). No TMPDIR in the env => the compose fallback.
      const r = run(h, { STUB_NOOP: "1" });
      assert.equal(r.status, 0, `compose tmpdir run must succeed (stderr: ${r.stderr})`);
      assert.ok(opMatches(r.ops, "mkdir", "/tmp/uzi-worker", "/tmp/uzi-runner"), "compose tmpdirs must stay under /tmp");
      assert.equal(r.env.get("TMPDIR"), "/tmp/uzi-worker", "compose TMPDIR is /tmp/uzi-worker");
      assert.equal(r.env.get("UZI_RUNNER_TMPDIR"), "/tmp/uzi-runner", "compose runner tmp is /tmp/uzi-runner");
    } finally {
      fs.rmSync(h.root, { recursive: true, force: true });
    }
  });

  it("sticky carve-out: the three parents get 3775 (setgid+group-write+STICKY), never 2775", () => {
    const h = makeHarness();
    try {
      fs.mkdirSync(h.nix);
      // Pre-create the parents so the every-boot carve-out's `[ -O ]` guard (owned by the
      // effective uid) is satisfied AND the legacy-migration `[ -d ]` guard fires; STUB_NOOP
      // means mkdir would not create them itself.
      for (const d of ["runner", "agent-home", "provision"]) fs.mkdirSync(path.join(h.data, d), { recursive: true });
      fs.writeFileSync(h.token, "t");
      const r = run(h, { STUB_NOOP: "1" });
      assert.equal(r.status, 0, `carve-out run must succeed (stderr: ${r.stderr})`);
      for (const d of ["runner", "agent-home", "provision"]) {
        assert.ok(opMatches(r.ops, "chmod 3775", `${h.data}/${d}`), `${d} parent must be chmod 3775 (sticky)`);
      }
      assert.ok(!r.ops.some((o) => o.startsWith("chmod 2775") && o.includes(`${h.data}/`)), "no carve-out parent may be plain 2775");
    } finally {
      fs.rmSync(h.root, { recursive: true, force: true });
    }
  });

  it("PVC-root alignment RUNS on the k8s fsGroup fingerprint (setgid /nix), NO-OP on compose", () => {
    // k8s: /nix carries the kubelet fsGroup setgid bit -> alignment reclaims + chmod 2775 +
    // restores group 10001 on BOTH mount roots without touching content ownership.
    const k = makeHarness();
    try {
      fs.mkdirSync(k.data);
      fs.mkdirSync(k.nix);
      fs.chmodSync(k.nix, 0o2755); // setgid: the fsGroup fingerprint
      fs.writeFileSync(k.token, "t");
      const r = run(k, { STUB_NOOP: "1" });
      assert.equal(r.status, 0, `k8s alignment run must succeed (stderr: ${r.stderr})`);
      for (const root of [k.nix, k.data]) {
        const reclaim = r.ops.findIndex((o) => o === `chown 0:0 ${root}`);
        const chmod = r.ops.findIndex((o) => o === `chmod 2775 ${root}`);
        assert.ok(reclaim >= 0 && chmod >= 0 && reclaim < chmod, `${root} must be reclaimed then chmod 2775 (setgid+group-rwx)`);
      }
      assert.ok(opMatches(r.ops, "chown runner:10001", k.nix), "/nix root group restored to the fsGroup (10001), owner preserved");
      assert.ok(opMatches(r.ops, "chown worker:10001", k.data), "/data root group restored to the fsGroup (10001)");
    } finally {
      fs.rmSync(k.root, { recursive: true, force: true });
    }

    const c = makeHarness();
    try {
      fs.mkdirSync(c.data);
      fs.mkdirSync(c.nix);
      // CLEAR any setgid the sandbox inherited from a setgid os.tmpdir() (the gate's TMPDIR is
      // /data/runner at 2777): a compose /nix mount root carries NO setgid (the image bakes it
      // with `chmod -R a+rX`), so the fingerprint must be false and the alignment a no-op.
      fs.chmodSync(c.nix, 0o755);
      fs.writeFileSync(c.token, "t");
      const r = run(c, { STUB_NOOP: "1" });
      assert.equal(r.status, 0, `compose run must succeed (stderr: ${r.stderr})`);
      assert.ok(!r.ops.some((o) => o === `chmod 2775 ${c.nix}` || o === `chmod 2775 ${c.data}`), "compose must NOT run the PVC-root alignment (byte-for-byte compose behaviour)");
    } finally {
      fs.rmSync(c.root, { recursive: true, force: true });
    }
  });

  it("legacy /data map: run HOME + codex-data stay worker:runner, descendants -> runner, session-store UNTOUCHED", () => {
    const h = makeHarness();
    try {
      // A populated legacy single-uid volume: everything worker-created, one run HOME with a
      // Codex resume store + Claude SDK state, plus a plain-lane clone and provision state.
      fs.mkdirSync(h.nix);
      const runHome = path.join(h.data, "agent-home", "run-legacy");
      fs.mkdirSync(path.join(runHome, "codex-data", "epoch-1"), { recursive: true });
      fs.mkdirSync(path.join(runHome, "codex-session-store"), { recursive: true });
      fs.writeFileSync(path.join(runHome, "codex-session-store", "session.json"), "resume");
      fs.mkdirSync(path.join(runHome, ".claude", "projects"), { recursive: true });
      fs.writeFileSync(path.join(runHome, ".claude.json"), "sdk-config");
      fs.mkdirSync(path.join(h.data, "runner", "clone-1"), { recursive: true });
      fs.writeFileSync(path.join(h.data, "runner", "clone-1", "work.txt"), "unpublished");
      fs.mkdirSync(path.join(h.data, "provision"), { recursive: true });
      fs.writeFileSync(path.join(h.data, "provision", "cache"), "pkgs");
      fs.writeFileSync(h.token, "t");

      const r = run(h, { STUB_NOOP: "1" });
      assert.equal(r.status, 0, `legacy migration run must succeed (stderr: ${r.stderr})`);

      // Run HOME root + codex-data root STAY worker-owned, gid runner (a non-recursive chgrp,
      // NOT a give-away `chown -R runner:runner`, so ensureCodexSharedDirectory still validates).
      assert.ok(r.ops.some((o) => o === `chown worker:runner ${runHome}`), "run HOME root -> worker:runner (non-recursive)");
      assert.ok(r.ops.some((o) => o === `chown worker:runner ${runHome}/codex-data`), "codex-data root -> worker:runner (non-recursive)");
      assert.ok(!r.ops.includes(`chown -R runner:runner ${runHome}`), "the run HOME root is never a give-away chown -R target");
      // Provider epoch trees + ordinary SDK descendants -> runner-owned.
      assert.ok(opMatches(r.ops, "chown -R runner:runner", `${runHome}/codex-data/epoch-1`), "epoch tree -> runner");
      assert.ok(opMatches(r.ops, "chown -R runner:runner", `${runHome}/.claude`), "the Claude SDK .claude tree -> runner");
      assert.ok(opMatches(r.ops, "chown -R runner:runner", `${runHome}/.claude.json`), "the Claude SDK config -> runner");
      // Plain-lane clone + provision content -> runner (preserved, not purged).
      assert.ok(opMatches(r.ops, "chown -R runner:runner", `${h.data}/runner/clone-1`), "the retained clone -> runner");
      assert.ok(opMatches(r.ops, "chown -R runner:runner", `${h.data}/provision/cache`), "provision content -> runner");
      // codex-session-store is NEVER a chown/chmod target.
      assert.ok(!r.ops.some((o) => o.includes("codex-session-store")), "codex-session-store must never be touched by the migration");
      // Nothing was purged (no rm is issued; content survives — proven byte-for-byte below).
      assert.equal(fs.readFileSync(path.join(runHome, "codex-session-store", "session.json"), "utf8"), "resume", "session store content survives");
      assert.equal(fs.readFileSync(path.join(h.data, "runner", "clone-1", "work.txt"), "utf8"), "unpublished", "clone content survives byte-for-byte");
      assert.equal(fs.readFileSync(path.join(runHome, ".claude.json"), "utf8"), "sdk-config", "SDK config survives");

      // The one-time sentinel is written.
      assert.ok(opMatches(r.ops, "chown worker:worker", `${h.data}/.uzi-legacy-split-migrated`) || fs.existsSync(path.join(h.data, ".uzi-legacy-split-migrated")), "the legacy migration writes its sentinel");
    } finally {
      fs.rmSync(h.root, { recursive: true, force: true });
    }
  });

  it("legacy migration is SENTINEL-GATED: a second boot does not re-run the ownership walk", () => {
    const h = makeHarness();
    try {
      fs.mkdirSync(h.nix);
      fs.mkdirSync(path.join(h.data, "agent-home", "run-x"), { recursive: true });
      fs.writeFileSync(h.token, "t");
      // Pre-place the sentinel: the walk must be skipped entirely.
      fs.writeFileSync(path.join(h.data, ".uzi-legacy-split-migrated"), "");
      const r = run(h, { STUB_NOOP: "1" });
      assert.equal(r.status, 0, `sentinel-gated run must succeed (stderr: ${r.stderr})`);
      assert.ok(!opMatches(r.ops, "chown worker:runner", `${h.data}/agent-home/run-x`), "the run HOME walk must be skipped when the sentinel exists");
      assert.doesNotMatch(r.stderr, /one-time ownership-aware migration/, "the migration banner must not print on a sentinel-gated boot");
    } finally {
      fs.rmSync(h.root, { recursive: true, force: true });
    }
  });

  // Issue #1696: /data/agent-home is ALSO the shared HOME of runner-uid processes (provisioning's
  // devbox/nix, the chat SDK CLI). Its own top-level dot entries are that shared state, never a
  // per-run HOME, so they must end up runner-owned: wholesale (chown -R) inside the legacy walk,
  // and top-level-inode-only (non-recursive) in the separate one-time repair for volumes the old
  // walk already damaged. Planted symlinks at the dot level (and one nested in .cache) and a
  // top-level HARDLINK to a repos/ file are planted too. The harness only RECORDS ops and never
  // walks the tree, so these tests prove only that no op NAMES a planted link; that the recursive
  // chown does not traverse the NESTED .cache/devbox link rests on the measured BusyBox behaviour
  // noted in entrypoint.sh, not on this test.
  const LEGACY_SENTINEL_NAME = ".uzi-legacy-split-migrated";
  const PROVISION_SENTINEL_NAME = ".uzi-provision-home-repaired";
  const LEGACY_BANNER = /one-time ownership-aware migration/;
  const PROVISION_BANNER = /one-time repair of shared provisioning HOME/;

  /** Plant the shared-HOME dot state, a run HOME, the repos cache, and the attacker symlinks. */
  function plantSharedHome(h: Harness): { ah: string; runX: string; reposReal: string } {
    fs.mkdirSync(h.nix);
    const ah = path.join(h.data, "agent-home");
    fs.mkdirSync(path.join(ah, ".cache", "jetify"), { recursive: true });
    fs.mkdirSync(path.join(ah, ".local", "state", "nix"), { recursive: true });
    fs.mkdirSync(path.join(ah, ".claude", "projects"), { recursive: true });
    fs.writeFileSync(path.join(ah, ".claude.json"), "chat-cli-config");
    // devbox's profile link: dangling here (no profiles/profile), which must be skipped too.
    fs.symlinkSync(".local/state/nix/profiles/profile", path.join(ah, ".nix-profile"));
    const runX = path.join(ah, "run-x");
    fs.mkdirSync(path.join(runX, "codex-session-store"), { recursive: true });
    fs.writeFileSync(path.join(runX, "codex-session-store", "session.json"), "resume");
    fs.mkdirSync(path.join(h.data, "repos"), { recursive: true });
    fs.writeFileSync(path.join(h.data, "repos", "x"), "bare-config\n");
    const reposReal = fs.realpathSync(path.join(h.data, "repos"));
    // Attacker-planted escapes into the worker-only repos/ cache.
    fs.symlinkSync("../repos", path.join(ah, ".config"));
    fs.symlinkSync("../repos/x", path.join(ah, ".claude.json.bak"));
    fs.symlinkSync("../../repos", path.join(ah, ".cache", "devbox"));
    // A HARDLINK to the worker's bare-repo config: it passes [ -L ] / [ -e ] / [ -f ], so only the
    // link-count filter keeps root from handing that shared inode to runner.
    fs.mkdirSync(path.join(h.data, "repos", "r.git"), { recursive: true });
    fs.writeFileSync(path.join(h.data, "repos", "r.git", "config"), "[core]\n");
    fs.linkSync(path.join(h.data, "repos", "r.git", "config"), path.join(ah, ".gitconfig-planted"));
    // A FIFO: a single-link, non-regular entry, so only the `[ -f ]` filter keeps it from a chown.
    const fifo = spawnSync("mkfifo", [path.join(ah, ".planted-fifo")], { encoding: "utf8" });
    assert.equal(fifo.status, 0, `mkfifo must succeed to plant the fifo fixture (${fifo.stderr})`);
    fs.writeFileSync(h.token, "t");
    return { ah, runX, reposReal };
  }

  function sentinelWritten(h: Harness, r: RunResult, name: string): boolean {
    const p = path.join(h.data, name);
    return r.ops.includes(`chown worker:worker ${p}`) || fs.existsSync(p);
  }

  it("issue #1696: an ALREADY-MIGRATED volume gets a one-time NON-recursive repair of agent-home's dot entries", () => {
    const h = makeHarness();
    try {
      fs.mkdirSync(h.data);
      fs.writeFileSync(path.join(h.data, LEGACY_SENTINEL_NAME), "");
      const { ah, reposReal } = plantSharedHome(h);

      const r = run(h, { STUB_NOOP: "1" });
      assert.equal(r.status, 0, `repair run must succeed (stderr: ${r.stderr})`);

      // Exactly the four real top-level dot entries, re-owned non-recursively (dirs AND the
      // single-link file); no other agent-home dot entry is chowned.
      const dotChowns = r.ops.filter((o) => o.startsWith("chown ") && o.includes(`${ah}/.`)).sort();
      assert.deepEqual(
        dotChowns,
        [".cache", ".claude", ".claude.json", ".local"].map((n) => `chown runner:runner ${ah}/${n}`),
        "exactly the real dirs and the single-link dot file are re-owned (top-level inode only)",
      );
      assert.ok(
        !r.ops.some((o) => o.startsWith("chown -R") && o.includes(`${ah}/`)),
        `the repair must never recurse under agent-home (ops: ${r.ops.join(" | ")})`,
      );
      for (const frag of ["run-x", "codex-session-store", ".nix-profile", `${ah}/.config`, ".claude.json.bak", ".cache/devbox", ".gitconfig-planted", ".planted-fifo"]) {
        assert.ok(!r.ops.some((o) => o.includes(frag)), `no op may touch ${frag}`);
      }
      assert.deepEqual(opsReachingRepos(r.ops, reposReal, "chown"), [], "no chown may reach repos/");
      assert.deepEqual(opsReachingRepos(r.ops, reposReal, "chmod"), [], "no chmod may reach repos/");
      assert.doesNotMatch(r.stderr, LEGACY_BANNER, "the legacy walk must stay sentinel-gated");
      assert.match(r.stderr, PROVISION_BANNER, "the one-time repair announces itself");
      assert.ok(sentinelWritten(h, r, PROVISION_SENTINEL_NAME), "the repair writes its own sentinel");
    } finally {
      fs.rmSync(h.root, { recursive: true, force: true });
    }
  });

  it("issue #1696: a FIRST migration re-owns agent-home's dot entries wholesale, never as run HOMEs", () => {
    const h = makeHarness();
    try {
      fs.mkdirSync(h.data);
      const { ah, runX, reposReal } = plantSharedHome(h);
      fs.mkdirSync(path.join(runX, "codex-data", "epoch-1"), { recursive: true });
      fs.mkdirSync(path.join(runX, ".claude", "projects"), { recursive: true });

      const r = run(h, { STUB_NOOP: "1" });
      assert.equal(r.status, 0, `first migration must succeed (stderr: ${r.stderr})`);

      // The real run HOME keeps the existing map.
      assert.ok(r.ops.includes(`chown worker:runner ${runX}`), "run HOME root -> worker:runner");
      assert.ok(r.ops.includes(`chown worker:runner ${runX}/codex-data`), "codex-data root -> worker:runner");
      assert.ok(r.ops.includes(`chown -R runner:runner ${runX}/codex-data/epoch-1`), "epoch tree -> runner");
      assert.ok(r.ops.includes(`chown -R runner:runner ${runX}/.claude`), "run HOME .claude -> runner");
      assert.ok(!r.ops.some((o) => o.includes("codex-session-store")), "codex-session-store is never touched");

      // The shared HOME's dot entries are NOT treated as run HOMEs...
      for (const name of [".cache", ".local", ".claude"]) {
        assert.ok(!r.ops.includes(`chown worker:runner ${ah}/${name}`), `${name} must not be kept worker-owned as a run HOME`);
      }
      // ...but re-owned wholesale to runner, the top-level dot FILE included.
      for (const name of [".cache", ".local", ".claude", ".claude.json"]) {
        assert.ok(r.ops.includes(`chown -R runner:runner ${ah}/${name}`), `${name} must be re-owned to runner recursively`);
      }
      for (const frag of [".nix-profile", `${ah}/.config`, ".claude.json.bak", ".cache/devbox", ".gitconfig-planted", ".planted-fifo"]) {
        assert.ok(!r.ops.some((o) => o.includes(frag)), `planted entry ${frag} must never be an op target`);
      }
      assert.deepEqual(opsReachingRepos(r.ops, reposReal, "chown"), [], "no chown may reach repos/");
      assert.deepEqual(opsReachingRepos(r.ops, reposReal, "chmod"), [], "no chmod may reach repos/");
      assert.ok(sentinelWritten(h, r, LEGACY_SENTINEL_NAME), "the legacy migration writes its sentinel");
      assert.ok(sentinelWritten(h, r, PROVISION_SENTINEL_NAME), "the repair writes its sentinel");
    } finally {
      fs.rmSync(h.root, { recursive: true, force: true });
    }
  });

  it("issue #1696: with BOTH sentinels present, agent-home's dot entries are left alone", () => {
    const h = makeHarness();
    try {
      fs.mkdirSync(h.data);
      fs.writeFileSync(path.join(h.data, LEGACY_SENTINEL_NAME), "");
      fs.writeFileSync(path.join(h.data, PROVISION_SENTINEL_NAME), "");
      const { ah } = plantSharedHome(h);

      const r = run(h, { STUB_NOOP: "1" });
      assert.equal(r.status, 0, `sentinel-gated run must succeed (stderr: ${r.stderr})`);
      assert.ok(!r.ops.some((o) => o.includes(`${ah}/.`)), `no op may touch an agent-home dot entry (ops: ${r.ops.join(" | ")})`);
      assert.doesNotMatch(r.stderr, LEGACY_BANNER, "no legacy banner");
      assert.doesNotMatch(r.stderr, PROVISION_BANNER, "no repair banner");
    } finally {
      fs.rmSync(h.root, { recursive: true, force: true });
    }
  });
});

// ─── Group 1b: symlink give-away DEFENSE (portable, record-only) ──────────────────────────
// A legacy single-uid /data let the untrusted agent uid write /data, so it could plant a symlink
// either as a DESCENDANT of a carve-out tree (e.g. agent-home/evil -> ../repos, or a run HOME's
// codex-data -> ../../repos) OR AT a carve-out ROOT itself (e.g. agent-home -> repos, runner ->
// ../repos). Without the `[ -L ]` guards a chmod/chown on the root DEREFERENCES onto the link's
// target (chmod follows symlinks) and the migration's globs walk THROUGH such a link, so a
// `chown -R runner:runner` (or the root chmod) re-owns/re-modes the worker-only bare-repo cache
// /data/repos (the B2 code-exec surface) to `runner`, defeating the uid split. These cases run the
// REAL entrypoint with the links planted and assert the op-log records NO chown/chmod whose argument
// RESOLVES onto/under repos/ (proving the direct-argument, the mid-path-prefix, AND the
// symlinked-root escapes are all closed). Reddens on the pre-fix entrypoint (the guardless ops record
// a chown -R / chmod reaching repos); passes with the guards. Portable/record-only because the gate
// runs as uid 10001 and cannot observe a give-away to uid 10002 on the real fs — the op-log is the
// cross-uid signal.

describe("PRD #1493 M2: legacy migration resists a symlink give-away (portable, record-only)", () => {
  const CHOWN_R_PREFIX = "chown -R runner:runner ";

  /** The path argument of every recursive `chown -R runner:runner <path>` op. */
  function chownRArgs(ops: string[]): string[] {
    return ops.filter((o) => o.startsWith(CHOWN_R_PREFIX)).map((o) => o.slice(CHOWN_R_PREFIX.length));
  }

  /** Every recursive-chown argument whose REAL (symlink-resolved) path lands inside `reposReal`. */
  function chownRReaching(ops: string[], reposReal: string): string[] {
    const hits: string[] = [];
    for (const arg of chownRArgs(ops)) {
      let resolved: string;
      try {
        resolved = fs.realpathSync(arg);
      } catch {
        continue; // no longer resolvable => cannot reach repos
      }
      if (resolved === reposReal || resolved.startsWith(reposReal + path.sep)) hits.push(`${arg} -> ${resolved}`);
    }
    return hits;
  }

  it("a planted agent-home/evil -> ../repos is NOT walked; no chown -R reaches repos/, real HOMEs still migrate", () => {
    const h = makeHarness();
    try {
      fs.mkdirSync(h.nix);
      fs.mkdirSync(h.data);
      // The worker-only bare-repo cache the give-away targets, with a known witness file.
      const bare = path.join(h.data, "repos", "mybare");
      fs.mkdirSync(bare, { recursive: true });
      const witness = path.join(bare, "config");
      const witnessBody = "hooksPath=/usr/share/uzi-git-nohooks\n";
      fs.writeFileSync(witness, witnessBody);
      const reposReal = fs.realpathSync(path.join(h.data, "repos"));
      const before = fs.statSync(witness);

      // A legitimate per-run HOME (must still be migrated) beside the attacker-planted sibling symlink.
      const legit = path.join(h.data, "agent-home", "run-legit");
      fs.mkdirSync(path.join(legit, ".claude"), { recursive: true });
      fs.writeFileSync(path.join(legit, ".claude", "p.json"), "history");
      fs.symlinkSync("../repos", path.join(h.data, "agent-home", "evil")); // sibling of the per-run homes
      fs.writeFileSync(h.token, "t");

      const r = run(h, { STUB_NOOP: "1" });
      assert.equal(r.status, 0, `migration run must succeed (stderr: ${r.stderr})`);

      // (a) NO recursive chown resolves under repos/, and the planted link is never a chown target.
      assert.deepEqual(
        chownRReaching(r.ops, reposReal),
        [],
        "no chown -R may resolve under repos/ via the planted agent-home/evil symlink",
      );
      assert.ok(
        !r.ops.some((o) => o.includes(`${h.data}/agent-home/evil`)),
        "the planted agent-home/evil symlink must never be a chown/chmod target",
      );
      // The guard skips ONLY the symlink, not real work: the legitimate HOME is still migrated.
      assert.ok(opMatches(r.ops, CHOWN_R_PREFIX, `${legit}/.claude`), "a real per-run HOME is still migrated");

      // (b) the witness under repos/ is byte-identical with unchanged ownership/ctime.
      const after = fs.statSync(witness);
      assert.equal(fs.readFileSync(witness, "utf8"), witnessBody, "repos witness bytes intact");
      assert.equal(after.uid, before.uid, "repos witness owner unchanged");
      assert.equal(after.gid, before.gid, "repos witness group unchanged");
      assert.equal(after.ctimeMs, before.ctimeMs, "repos witness ctime unchanged (never re-owned)");
    } finally {
      fs.rmSync(h.root, { recursive: true, force: true });
    }
  });

  it("a run HOME whose codex-data is a symlink to ../../repos does not recurse the epoch give-away into repos/", () => {
    const h = makeHarness();
    try {
      fs.mkdirSync(h.nix);
      fs.mkdirSync(h.data);
      const bare = path.join(h.data, "repos", "mybare");
      fs.mkdirSync(bare, { recursive: true });
      const witness = path.join(bare, "config");
      fs.writeFileSync(witness, "bare-config\n");
      const reposReal = fs.realpathSync(path.join(h.data, "repos"));
      const before = fs.statSync(witness);

      // A REAL per-run HOME (so the [ -L "$home" ] guard does NOT skip it) whose codex-data is a
      // symlink escape into the bare cache.
      const runX = path.join(h.data, "agent-home", "run-x");
      fs.mkdirSync(runX, { recursive: true });
      fs.symlinkSync("../../repos", path.join(runX, "codex-data"));
      fs.writeFileSync(h.token, "t");

      const r = run(h, { STUB_NOOP: "1" });
      assert.equal(r.status, 0, `migration run must succeed (stderr: ${r.stderr})`);

      assert.deepEqual(
        chownRReaching(r.ops, reposReal),
        [],
        "the codex-data symlink must not let a chown -R recurse into repos/",
      );
      assert.ok(
        !r.ops.some((o) => o.startsWith(CHOWN_R_PREFIX) && o.includes(`${runX}/codex-data/`)),
        "no epoch under the codex-data symlink may be recursively chowned",
      );

      const after = fs.statSync(witness);
      assert.equal(after.uid, before.uid, "repos witness owner unchanged");
      assert.equal(after.ctimeMs, before.ctimeMs, "repos witness ctime unchanged (never re-owned)");
    } finally {
      fs.rmSync(h.root, { recursive: true, force: true });
    }
  });

  it("a TOP-LEVEL agent-home -> repos symlink ROOT fails before migration or runtime launch", () => {
    const h = makeHarness();
    try {
      fs.mkdirSync(h.nix);
      fs.mkdirSync(h.data);
      // The worker-only bare-repo cache the give-away targets, with a known witness file.
      const bare = path.join(h.data, "repos", "mybare");
      fs.mkdirSync(bare, { recursive: true });
      const witness = path.join(bare, "config");
      const witnessBody = "hooksPath=/usr/share/uzi-git-nohooks\n";
      fs.writeFileSync(witness, witnessBody);
      const reposReal = fs.realpathSync(path.join(h.data, "repos"));
      const before = fs.statSync(witness);

      // The WHOLE carve-out ROOT is a symlink to repos: a chmod/chown keyed on it dereferences onto
      // the bare cache, and any glob keyed on it descends through it into repos/.
      fs.symlinkSync("repos", path.join(h.data, "agent-home"));
      // A real sibling carve-out with content: the positive control that real work still happens.
      fs.mkdirSync(path.join(h.data, "provision"), { recursive: true });
      fs.writeFileSync(path.join(h.data, "provision", "cache"), "pkgs");
      fs.writeFileSync(h.token, "t");

      const r = run(h, { STUB_NOOP: "1" });
      assert.notEqual(r.status, 0, "a symlinked agent-home root must fail startup");
      assert.match(r.stderr, /refusing to start: .*agent-home.* is a symlink/, "the refusal names the planted root");
      assert.ok(!fs.existsSync(path.join(h.data, ".uzi-legacy-split-migrated")), "failed migration must not record its sentinel");
      assert.equal(r.env.size, 0, "startup must fail before the runtime privilege drop can follow the planted root");

      // (a) NO chown -R runner:runner resolves under repos/ (the auditor's demonstrated give-away
      //     `chown -R runner:runner .../repos/mybare/config`).
      assert.deepEqual(chownRReaching(r.ops, reposReal), [], "no chown -R may resolve under repos/ via the agent-home root symlink");
      // (b) NO chmod op resolves onto/under repos/ (the chmod-dereference hole on the root itself).
      assert.deepEqual(opsReachingRepos(r.ops, reposReal, "chmod"), [], "no chmod may dereference onto repos/ via the agent-home root symlink");
      // (c) NO chown of ANY kind (recursive or not) resolves onto/under repos/.
      assert.deepEqual(opsReachingRepos(r.ops, reposReal, "chown"), [], "no chown may dereference onto repos/ via the agent-home root symlink");
      // The symlinked root is never a chown/chmod target (mkdir -p records it; nothing else may).
      assert.ok(
        !r.ops.some((o) => (o.startsWith("chown") || o.startsWith("chmod")) && o.includes(`${h.data}/agent-home`)),
        "the symlinked agent-home root must never be a chown/chmod target",
      );
      assert.ok(
        !opMatches(r.ops, CHOWN_R_PREFIX, `${h.data}/provision/cache`),
        "migration must stop before touching a real sibling after the rejected root",
      );

      // (d) the witness under repos/ is byte-identical with unchanged ownership/ctime.
      const after = fs.statSync(witness);
      assert.equal(fs.readFileSync(witness, "utf8"), witnessBody, "repos witness bytes intact");
      assert.equal(after.uid, before.uid, "repos witness owner unchanged");
      assert.equal(after.gid, before.gid, "repos witness group unchanged");
      assert.equal(after.ctimeMs, before.ctimeMs, "repos witness ctime unchanged (never re-owned)");
    } finally {
      fs.rmSync(h.root, { recursive: true, force: true });
    }
  });

  it("a TOP-LEVEL runner -> ../repos symlink ROOT fails before migration or runtime launch", () => {
    const h = makeHarness();
    try {
      fs.mkdirSync(h.nix);
      fs.mkdirSync(h.data);
      // A repos cache OUTSIDE the data volume that `runner -> ../repos` escapes to — a symlinked
      // root can point anywhere the attacker chooses, not only at a sibling of the carve-outs.
      const reposDir = path.join(h.root, "repos");
      const bare = path.join(reposDir, "mybare");
      fs.mkdirSync(bare, { recursive: true });
      const witness = path.join(bare, "config");
      const witnessBody = "bare-config\n";
      fs.writeFileSync(witness, witnessBody);
      const reposReal = fs.realpathSync(reposDir);
      const before = fs.statSync(witness);

      // The runner carve-out ROOT is a symlink escaping the data volume (../repos from /data).
      fs.symlinkSync("../repos", path.join(h.data, "runner"));
      // A real sibling per-run HOME: the positive control that real migration still happens.
      const legit = path.join(h.data, "agent-home", "run-legit");
      fs.mkdirSync(path.join(legit, ".claude"), { recursive: true });
      fs.writeFileSync(path.join(legit, ".claude", "p.json"), "history");
      fs.writeFileSync(h.token, "t");

      const r = run(h, { STUB_NOOP: "1" });
      assert.notEqual(r.status, 0, "a symlinked runner root must fail startup");
      assert.match(r.stderr, /refusing to start: .*runner.* is a symlink/, "the refusal names the planted root");
      assert.ok(!fs.existsSync(path.join(h.data, ".uzi-legacy-split-migrated")), "failed migration must not record its sentinel");
      assert.equal(r.env.size, 0, "startup must fail before the runtime privilege drop can follow the planted root");

      // The auditor's demonstrated give-away `chown -R runner:runner .../repos/mybare`.
      assert.deepEqual(chownRReaching(r.ops, reposReal), [], "no chown -R may resolve under repos/ via the runner root symlink");
      assert.deepEqual(opsReachingRepos(r.ops, reposReal, "chmod"), [], "no chmod may dereference onto repos/ via the runner root symlink");
      assert.deepEqual(opsReachingRepos(r.ops, reposReal, "chown"), [], "no chown may dereference onto repos/ via the runner root symlink");
      assert.ok(
        !r.ops.some((o) => (o.startsWith("chown") || o.startsWith("chmod")) && o.includes(`${h.data}/runner`)),
        "the symlinked runner root must never be a chown/chmod target",
      );
      assert.ok(
        !opMatches(r.ops, CHOWN_R_PREFIX, `${legit}/.claude`),
        "migration must stop before touching a real sibling after the rejected root",
      );

      const after = fs.statSync(witness);
      assert.equal(fs.readFileSync(witness, "utf8"), witnessBody, "repos witness bytes intact");
      assert.equal(after.uid, before.uid, "repos witness owner unchanged");
      assert.equal(after.gid, before.gid, "repos witness group unchanged");
      assert.equal(after.ctimeMs, before.ctimeMs, "repos witness ctime unchanged (never re-owned)");
    } finally {
      fs.rmSync(h.root, { recursive: true, force: true });
    }
  });
});

// ─── Group 2: REAL ownership (gated: run AS WORKER_UID with WORKER+RUNNER membership) ─────

const uidNow = typeof process.getuid === "function" ? process.getuid() : undefined;
const memberGroups = typeof process.getgroups === "function" ? [...new Set(process.getgroups())] : [];
const GROUP2_SKIP =
  uidNow === WORKER_UID && memberGroups.includes(WORKER_UID) && memberGroups.includes(RUNNER_UID)
    ? false
    : "requires running as WORKER_UID with WORKER_UID + RUNNER_UID group membership";

describe("PRD #1493 M2: legacy run HOME survives migration and still validates (real ownership)", { skip: GROUP2_SKIP }, () => {
  it("migrated run HOME + codex-data validate via ensureCodexSharedDirectory; session store stays worker-private", async () => {
    const h = makeHarness();
    try {
      fs.mkdirSync(h.nix);
      // A populated legacy run HOME, all worker:worker (== the test uid's own uid+primary gid
      // under the gate), mirroring a single-uid volume after migrate_tree.
      const runHome = path.join(h.data, "agent-home", "run-legacy");
      fs.mkdirSync(path.join(runHome, "codex-data"), { recursive: true });
      fs.mkdirSync(path.join(runHome, "codex-session-store"), { recursive: true });
      fs.chmodSync(path.join(runHome, "codex-session-store"), 0o700);
      fs.writeFileSync(path.join(runHome, "codex-session-store", "session.json"), "resume-state");
      fs.chmodSync(path.join(runHome, "codex-session-store", "session.json"), 0o600);
      fs.mkdirSync(path.join(runHome, ".claude", "projects"), { recursive: true });
      fs.writeFileSync(path.join(runHome, ".claude", "projects", "p.json"), "history");
      fs.writeFileSync(h.token, "t");

      // Real (best-effort) ops: `chown worker:runner` is a chgrp to a member group (succeeds);
      // a give-away `chown -R runner:runner` EPERMs under uid 10001 and is recorded only.
      // Point the ambient TMPDIR at this test's own mkdtemp root so the (a3) tmpdir block's
      // REAL `rm -rf`/`mkdir` land under h.root/ambient-tmp — NEVER a real `rm -rf /tmp/uzi-*`
      // against a live worker's scratch tmp when the gate runs as WORKER_UID (this test has no
      // STUB_NOOP). It exercises the identical rm/reclaim/chmod lines; only the path differs, and
      // nothing here asserts on the tmpdir, so the run-HOME assertions below are unaffected.
      const r = run(h, { TMPDIR: path.join(h.root, "ambient-tmp") });
      assert.equal(r.status, 0, `real migration run must succeed (stderr: ${r.stderr})`);

      // (a) the run HOME + codex-data are now worker:runner and PASS the production validator
      // (it force-asserts 3770 — sticky + setgid, PRD #1493 M3 — and rejects any owner/group mismatch).
      await ensureCodexSharedDirectory(runHome);
      await ensureCodexSharedDirectory(path.join(runHome, "codex-data"));
      const home = await fsp.lstat(runHome);
      assert.equal(home.uid, WORKER_UID, "run HOME owner stays worker");
      assert.equal(home.gid, RUNNER_UID, "run HOME group is runner");

      // (b) resume state is byte-identical.
      assert.equal(fs.readFileSync(path.join(runHome, "codex-session-store", "session.json"), "utf8"), "resume-state", "session store bytes intact");
      assert.equal(fs.readFileSync(path.join(runHome, ".claude", "projects", "p.json"), "utf8"), "history", "SDK resume state intact");

      // (c) codex-session-store is UNTOUCHED: still worker:worker, dir 0700, file 0600 — so a
      // non-owner non-group process (uid 10003, in neither the owner nor group `worker`) is
      // DAC-denied read/modify. Expressed as a mode/ownership check (the gate has no uid 10003).
      const store = await fsp.lstat(path.join(runHome, "codex-session-store"));
      assert.equal(store.uid, WORKER_UID, "session store owner stays worker");
      assert.equal(store.gid, WORKER_UID, "session store group stays worker (NOT runner)");
      assert.equal(store.mode & MODE_MASK, 0o700, "session store dir stays 0700 (worker-private)");
      const storeFile = await fsp.lstat(path.join(runHome, "codex-session-store", "session.json"));
      assert.equal(storeFile.mode & MODE_MASK, 0o600, "session store file stays 0600");

      // (d) the migration RE-OWNS the ordinary SDK HOME state to the runner (uid 10002) that
      // runs the SDK under the split, so the runner can read AND update it. Under the gate uid
      // 10001 cannot GIVE AWAY ownership, so the effected transition is asserted from the
      // recorded intent (a mode/ownership statement about what the root-window migration does).
      assert.ok(opMatches(r.ops, "chown -R runner:runner", `${runHome}/.claude`), "SDK HOME state is re-owned to the runner");
    } finally {
      fs.rmSync(h.root, { recursive: true, force: true });
    }
  });
});
