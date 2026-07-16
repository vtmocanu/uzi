import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

// Every worker template image (PRD #18 + #51) must keep the guardrail-relevant
// layers the base image establishes: the root-entry setpriv drop to the non-root
// `worker` uid (with the distinct cap-less `runner` uid present), and tini as PID 1
// so SIGTERM reaches the worker. A variant may ADD packages but must never drop
// these — this test turns the "keep in lockstep with base" Dockerfile comment into
// an enforced invariant, cheaply (text parse, no docker), before more variants land.

const templatesDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../templates");

function templateDockerfiles(): { name: string; text: string }[] {
  return fs
    .readdirSync(templatesDir, { withFileTypes: true })
    .filter((e) => e.isDirectory())
    .map((e) => {
      const file = path.join(templatesDir, e.name, "Dockerfile");
      return { name: e.name, text: fs.readFileSync(file, "utf8") };
    });
}

describe("worker template Dockerfiles keep guardrail layers", () => {
  const dockerfiles = templateDockerfiles();

  it("finds at least the base template", () => {
    assert.ok(
      dockerfiles.some((d) => d.name === "base"),
      "expected agent/templates/base/Dockerfile to exist",
    );
  });

  for (const { name, text } of dockerfiles) {
    it(`${name}: drops to the non-root worker uid via the root-entry setpriv wrapper (PRD #51 A1)`, () => {
      // The image runs as root and the entrypoint setpriv-drops to `worker`; there
      // must be NO `USER` line (which would defeat the root startup window). Both the
      // credential-holding `worker` and the cap-less `runner` uid must exist, and the
      // util-linux setpriv drop wrapper must be installed (busybox's cannot --reuid).
      assert.doesNotMatch(text, /^\s*USER\s+/m, `${name}/Dockerfile must NOT set a USER — the entrypoint drops root -> worker`);
      assert.match(text, /adduser\s+-u\s+10001\s+-G\s+worker\b/, `${name}/Dockerfile must create the worker uid (10001)`);
      assert.match(text, /adduser\s+-u\s+10002\s+-G\s+runner\b/, `${name}/Dockerfile must create the runner uid (10002)`);
      assert.match(text, /apk add[^\n]*\bsetpriv\b/, `${name}/Dockerfile must install util-linux setpriv (the A1 drop wrapper)`);
      assert.match(
        text,
        /ENTRYPOINT\s*\[\s*"\/usr\/local\/sbin\/uzi-entrypoint"/,
        `${name}/Dockerfile ENTRYPOINT must be the root-entry drop wrapper`,
      );
    });

    it(`${name}: bakes its own template identity`, () => {
      // The reported-template drift signal depends on each image baking its name.
      assert.match(
        text,
        new RegExp(`ENV\\s+UZI_WORKER_TEMPLATE=${name}\\b`),
        `${name}/Dockerfile must bake ENV UZI_WORKER_TEMPLATE=${name}`,
      );
    });

    it(`${name}: installs the pinned devbox binary + nix at build (PRD #18)`, () => {
      // Every template gains the provisioning stack so it works regardless of which
      // image a worker runs. Pinned build-time installs only — no floating
      // `curl | bash`, no runtime download (the prior blockers). Mirrored across all
      // Dockerfiles (self-contained layout); this pins that.
      assert.match(text, /jetify-com\/devbox\/releases\/download/, `${name}/Dockerfile must download the pinned devbox release`);
      assert.match(text, /install -m 0755 .*\/usr\/local\/bin\/devbox/, `${name}/Dockerfile must install devbox mode 0755 (non-owner exec)`);
      assert.match(text, /nix-installer/, `${name}/Dockerfile must install nix at build time`);
      assert.doesNotMatch(text, /get\.jetify\.com\/devbox/, `${name}/Dockerfile must NOT use the floating devbox launcher`);
      // The checksum grep|sha256sum pipe must run under pipefail — BusyBox
      // sha256sum -c exits 0 on empty stdin, so a no-match grep would else skip
      // verification silently (audit L2).
      assert.match(text, /set -o pipefail/, `${name}/Dockerfile must guard the checksum pipe with 'set -o pipefail'`);
    });
  }
});

// The shared root-entry drop wrapper (PRD #51 A1) is the single mechanism both
// templates COPY in. It is the security-load-bearing script: it must drop root to
// `worker` keeping ONLY setuid/setgid (ambient), keep tini as PID 1 for SIGTERM, and
// force the join token to 0400 worker with chmod BEFORE chown (the runtime cap set
// has no CAP_FOWNER, so root can chmod the token only while it still owns it).
describe("the shared root-entry drop wrapper", () => {
  const entrypoint = fs.readFileSync(path.join(templatesDir, "entrypoint.sh"), "utf8");

  it("drops root -> worker retaining ONLY setuid/setgid (ambient), tini stays PID 1", () => {
    assert.match(entrypoint, /--reuid\s+"\$WORKER_USER"/, "must setpriv --reuid to the worker uid");
    assert.match(entrypoint, /--ambient-caps\s+-all,\+setuid,\+setgid/, "must keep ONLY setuid/setgid as ambient caps");
    assert.match(entrypoint, /--bounding-set\s+-all,\+setuid,\+setgid/, "must tighten the bounding set to setuid/setgid");
    assert.match(entrypoint, /"\$TINI"\s+--\s+"\$@"/, "tini must stay PID 1 (SIGTERM -> clean worker shutdown), execing the CMD");
  });

  it("forces the join token to 0400 worker with chmod BEFORE chown (no CAP_FOWNER)", () => {
    const chmodAt = entrypoint.search(/"\$CHMOD"\s+0400\s+"\$TOKEN"/);
    const chownAt = entrypoint.search(/"\$CHOWN"\s+"\$WORKER_OWNER"\s+"\$TOKEN"/);
    assert.ok(chmodAt >= 0, "must chmod 0400 the token");
    assert.ok(chownAt >= 0, "must chown the token to worker");
    assert.ok(chmodAt < chownAt, "chmod 0400 must precede chown (runtime has no CAP_FOWNER)");
  });

  it("keeps the root startup window off the runner-writable volumes", () => {
    // PATH excludes /nix and /data so root never resolves a binary from a volume.
    assert.match(entrypoint, /PATH=\/usr\/local\/sbin:\/usr\/local\/bin:\/usr\/sbin:\/usr\/bin:\/sbin:\/bin/);
    assert.doesNotMatch(entrypoint, /PATH=[^\n]*\/nix/, "the root-window PATH must not contain /nix");
  });

  it("tolerates a non-root start (PRD #58): single-uid exec precedes the setpriv drop", () => {
    // A non-root start (k8s runAsUser: 10001, no addable caps) must skip the root
    // window and exec tini directly — an unconditional `setpriv --reuid` would
    // EPERM -> CrashLoopBackOff. The guard tests uid != 0 via an ABSOLUTE `id` path.
    assert.match(entrypoint, /"\$ID"\s+-u.*!=\s*"0"|!=\s*"0"/, "must test for a non-root start (uid != 0)");
    const nonrootExecAt = entrypoint.search(/exec\s+"\$TINI"\s+--\s+"\$@"/);
    const setprivAt = entrypoint.search(/exec\s+"\$SETPRIV"/);
    assert.ok(nonrootExecAt >= 0, "must exec tini directly on the non-root path");
    assert.ok(setprivAt >= 0, "must setpriv-drop on the root path");
    assert.ok(nonrootExecAt < setprivAt, "the non-root single-uid exec must come before the setpriv drop");
  });
});

// The provisioning stack must be identical across templates (self-contained
// layout). Pin the devbox version equal everywhere so base/jvm can't drift — now
// meaningful because the version is a real pin, not a dead ENV.
describe("worker templates pin the same devbox version", () => {
  it("ARG DEVBOX_VERSION matches across every template", () => {
    const versions = templateDockerfiles().map(({ name, text }) => {
      const m = /ARG\s+DEVBOX_VERSION=(\S+)/.exec(text);
      assert.ok(m, `${name}/Dockerfile must pin ARG DEVBOX_VERSION`);
      return m![1];
    });
    assert.strictEqual(new Set(versions).size, 1, `templates pin different devbox versions: ${versions.join(", ")}`);
  });
});

// The template name set lives in THREE places (PRD #18): the agent/templates/<name>/
// dirs (the images), api/internal/workertmpl.Names (server registry, validates the
// declared choice), and web/src/lib/workerTemplates.ts (the issuance dropdown). This
// test pins them equal so a new template can't be added in one place and silently
// diverge — the duplication-drift + triple-registry concern.
function parseStringList(text: string, anchor: RegExp): string[] {
  const m = anchor.exec(text);
  if (!m || m[1] === undefined) return [];
  return [...m[1].matchAll(/["']([^"']+)["']/g)].map((x) => x[1]!).sort();
}

describe("worker template registry stays in sync (three sources)", () => {
  const dirNames = templateDockerfiles()
    .map((d) => d.name)
    .sort();

  it("agent/templates/ dirs match the server registry (workertmpl.Names)", () => {
    const goFile = path.resolve(templatesDir, "../../api/internal/workertmpl/workertmpl.go");
    const names = parseStringList(fs.readFileSync(goFile, "utf8"), /var Names = \[\]string\{([^}]*)\}/);
    assert.deepStrictEqual(names, dirNames, "template dirs must equal workertmpl.Names");
  });

  it("agent/templates/ dirs match the web registry (WORKER_TEMPLATES)", () => {
    const webFile = path.resolve(templatesDir, "../../web/src/lib/workerTemplates.ts");
    const names = parseStringList(fs.readFileSync(webFile, "utf8"), /WORKER_TEMPLATES\s*=\s*\[([^\]]*)\]/);
    assert.deepStrictEqual(names, dirNames, "template dirs must equal web WORKER_TEMPLATES");
  });
});
