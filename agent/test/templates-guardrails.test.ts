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

    it(`${name}: ships the docker CLI (client only) with NO daemon and NO baked socket (PRD #83 M1)`, () => {
      // Decision 1: docker-cli + compose + buildx go into `base` (both templates); the
      // CLIENT only — there is NO `dockerd` in any image, the daemon is the sidecar.
      assert.match(text, /apk add[^\n]*\bdocker-cli\b/, `${name}/Dockerfile must apk add docker-cli`);
      assert.match(text, /apk add[^\n]*\bdocker-cli-compose\b/, `${name}/Dockerfile must apk add docker-cli-compose (compose v2)`);
      assert.match(text, /apk add[^\n]*\bdocker-cli-buildx\b/, `${name}/Dockerfile must apk add docker-cli-buildx`);
      assert.doesNotMatch(text, /\bapk add[^\n]*\bdockerd\b/, `${name}/Dockerfile must NOT install dockerd (the daemon is the sidecar)`);
      // C1: base must bake NO docker socket (the CLI defaults to /var/run/docker.sock when
      // DOCKER_HOST is unset), and DOCKER_HOST must NOT be baked — set at run time only by
      // the keystone resolver when a sidecar is wired, so a sidecar-less docker fails loudly.
      assert.doesNotMatch(text, /docker\.sock/, `${name}/Dockerfile must bake no docker.sock (C1)`);
      assert.doesNotMatch(text, /^\s*ENV\s+DOCKER_HOST\b/m, `${name}/Dockerfile must NOT bake ENV DOCKER_HOST (set at run time only)`);
    });

    it(`${name}: bakes the go/python default toolchain via devbox-global at build time (PRD #83 Q2)`, () => {
      // The owner's default worker toolchain (go + python + pip) goes through the ONE
      // nix/devbox path (no second apk package path), realized into the baked /nix at build
      // time from the committed pinned manifest, and its profile bin added to ENV PATH.
      // Mirrored across both templates (jvm is NOT `FROM base`).
      assert.match(text, /COPY\s+agent\/devbox-global\/devbox\.json\b/, `${name}/Dockerfile must COPY the committed global manifest`);
      // The committed lock PINS the nixpkgs revision; without this COPY the image never
      // sees it and go/python/pip float per rebuild (violating the "everything PINNED"
      // rule). It must precede `devbox global install`.
      assert.match(text, /COPY\s+agent\/devbox-global\/devbox\.lock\b/, `${name}/Dockerfile must COPY the committed global lock (pins nixpkgs)`);
      // Anchor on the concrete COPY/RUN lines (not the prose, which also names
      // "devbox global install") so the ordering check is real: the lock COPY must precede
      // the install RUN, else the image never sees the lock.
      assert.ok(
        text.indexOf("COPY agent/devbox-global/devbox.lock") < text.indexOf("RUN devbox global install"),
        `${name}/Dockerfile must COPY the lock BEFORE 'RUN devbox global install'`,
      );
      assert.match(text, /devbox global install/, `${name}/Dockerfile must realize the global manifest at build time`);
      // PRD #92 M1: PATH points at the STABLE, immutable /opt/uzi-toolchain/bin handle
      // (readlink -f of the realized profile), NOT the mutable devbox `default` profile
      // symlink that advances every image roll and strands stale-seeded /nix volumes.
      assert.match(
        text,
        /ENV PATH="\/opt\/uzi-toolchain\/bin/,
        `${name}/Dockerfile must put /opt/uzi-toolchain/bin on PATH (the stable, immutable toolchain handle)`,
      );
      assert.doesNotMatch(
        text,
        /ENV PATH="[^"]*devbox\/global\/default\/\.devbox\/nix\/profile\/default\/bin/,
        `${name}/Dockerfile must NOT point PATH at the mutable devbox global profile symlink (PRD #92 M1)`,
      );
      // The stable symlink is created from the readlink -f'd realized profile.
      assert.match(
        text,
        /ln -sfn "\$PROFILE" \/opt\/uzi-toolchain/,
        `${name}/Dockerfile must create the stable /opt/uzi-toolchain symlink to the realized profile`,
      );
      // The store-hash marker M2's version-aware seed keys on (the contract this milestone freezes).
      assert.match(
        text,
        /readlink -f \/opt\/uzi-toolchain > \/etc\/uzi-toolchain-profile/,
        `${name}/Dockerfile must bake the /etc/uzi-toolchain-profile store-hash marker (M2 contract)`,
      );
      // The INDIRECT nix GC root so the seed tar carries the profile and a runner
      // nix-collect-garbage (issue #91) can't reap it.
      assert.match(
        text,
        /ln -sfn \/opt\/uzi-toolchain \/nix\/var\/nix\/gcroots\/uzi-toolchain/,
        `${name}/Dockerfile must register the /nix/var/nix/gcroots/uzi-toolchain GC root`,
      );
      // The fail-closed build assertion: a broken toolchain fails the BUILD, not a silent 127 at
      // run time. `command -v` checks each tool SEPARATELY (multi-operand `command -v` validates
      // only the first), so all four must be present as their own `command -v` clause.
      for (const tool of ["python3", "go", "gcc", "pip"]) {
        assert.match(
          text,
          new RegExp(`command -v ${tool}\\b`),
          `${name}/Dockerfile must carry a fail-closed 'command -v ${tool}' build assertion`,
        );
      }
    });

    it(`${name}: wires the PRD #87 browser (shim + M4 launch ENV + fail-closed guard) in lockstep`, () => {
      // The browser rides the shared toolchain, so EVERY template must carry the same runtime
      // wiring. jvm is NOT `FROM base` and does NOT inherit ENV, so forgetting this block on
      // jvm re-creates the M0 SUID abort on every jvm worker — pin it as an invariant here.

      // M1 crash-close shim baked onto PATH (/usr/local/bin, ahead of the un-PATH'd npm bin).
      assert.match(
        text,
        /COPY --chmod=0755 agent\/bin\/agent-browser \/usr\/local\/bin\/agent-browser/,
        `${name}/Dockerfile must bake the agent-browser crash-close shim onto PATH (M1)`,
      );

      // M4 launch ENV: executable path pinned to the STABLE /opt/uzi-toolchain handle (never a
      // rolling /nix/store path), --no-sandbox mandatory + --disable-dev-shm-usage, idle timeout.
      assert.match(
        text,
        /ENV AGENT_BROWSER_EXECUTABLE_PATH=\/opt\/uzi-toolchain\/bin\/chromium/,
        `${name}/Dockerfile must point AGENT_BROWSER_EXECUTABLE_PATH at the stable toolchain handle (M4)`,
      );
      assert.doesNotMatch(
        text,
        /ENV AGENT_BROWSER_EXECUTABLE_PATH=[^\n]*\/nix\/store/,
        `${name}/Dockerfile must NOT point AGENT_BROWSER_EXECUTABLE_PATH at a rolling /nix/store path`,
      );
      assert.match(
        text,
        /ENV AGENT_BROWSER_ARGS="--no-sandbox,--disable-dev-shm-usage"/,
        `${name}/Dockerfile must force --no-sandbox (mandatory) + --disable-dev-shm-usage (M4)`,
      );
      assert.match(
        text,
        /ENV AGENT_BROWSER_IDLE_TIMEOUT_MS=\d+/,
        `${name}/Dockerfile must set a stale-daemon idle timeout (M4)`,
      );

      // Fonts: fontconfig config so screenshots render glyphs, not tofu.
      assert.match(text, /ENV FONTCONFIG_FILE=/, `${name}/Dockerfile must set FONTCONFIG_FILE so fonts resolve`);

      // Fail-closed build guard: agent-browser resolves AND an exit-bounded headless launch.
      // A bare `--version` would never start Chromium, so require the real headless launch
      // wrapped in `timeout` (a hung browser must FAIL the build, not hang the CI job).
      assert.match(text, /command -v agent-browser\b/, `${name}/Dockerfile must assert agent-browser resolves at build (M3)`);
      // The launch is `timeout N "$AGENT_BROWSER_EXECUTABLE_PATH" \ --headless --no-sandbox …`
      // (the flags wrap onto a continuation line), so check the pieces, not one flat line.
      assert.match(
        text,
        /timeout \d+ "\$AGENT_BROWSER_EXECUTABLE_PATH"/,
        `${name}/Dockerfile must exit-bound the headless Chromium launch with 'timeout' (M3)`,
      );
      assert.match(text, /--headless\b/, `${name}/Dockerfile build guard must launch Chromium headless (M3)`);
      assert.match(text, /--dump-dom/, `${name}/Dockerfile build guard must actually launch Chromium (--dump-dom), not a bare --version`);

      // BUG 1 (issue #114): the build guard launches chromium AS ROOT, which creates its
      // bundled `libexec/chromium/extensions` dir root:root 0700 inside the read-only nix
      // store — AFTER `chown -R worker:runner /nix` — and the non-root (uid 10001) seed-nix
      // tar then CrashLoops on it. The store-perm normalization MUST exist and MUST come
      // AFTER the guard's chromium launch (placing it at the earlier chown is a no-op: the
      // dir does not exist yet). Pin BOTH the presence and that ordering.
      assert.match(
        text,
        /chmod -R a\+rX \/nix/,
        `${name}/Dockerfile must normalize /nix store perms with 'chmod -R a+rX /nix' (BUG 1 seed-nix fix)`,
      );
      assert.ok(
        text.indexOf("chmod -R a+rX /nix") > text.search(/timeout \d+ "\$AGENT_BROWSER_EXECUTABLE_PATH"/),
        `${name}/Dockerfile must run the /nix store-perm chmod AFTER the build-guard chromium launch (the guard, running as root, is what creates the root:root 0700 dir — normalizing before it is a no-op)`,
      );
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
    // PRD #92 M3 prohibition: /opt/uzi-toolchain/bin dereferences into runner-writable
    // /nix, so it must NEVER be on any worker PATH= line (it belongs only on the image
    // PATH that becomes UZI_RUNNER_PATH). This pins the guardrail comment as an invariant.
    assert.doesNotMatch(
      entrypoint,
      /PATH=[^\n]*\/opt\/uzi-toolchain/,
      "no PATH= line may carry /opt/uzi-toolchain (PRD #92: it dereferences into runner-writable /nix)",
    );
  });

  it("tolerates a non-root start (PRD #58): single-uid exec precedes the setpriv drop", () => {
    // A non-root start (k8s runAsUser: 10001, no addable caps) must skip the root
    // window and exec tini directly — an unconditional `setpriv --reuid` would
    // EPERM -> CrashLoopBackOff. The uid is read via an ABSOLUTE `id` and branched on.
    assert.match(entrypoint, /uid="\$\("\$ID"\s+-u\)"/, "must read uid via the absolute id binary into a var");
    assert.match(entrypoint, /\[\s*"\$uid"\s*!=\s*"0"\s*\]/, "must branch the non-root path on uid != 0");
    const nonrootExecAt = entrypoint.search(/exec\s+"\$TINI"\s+--\s+"\$@"/);
    const setprivAt = entrypoint.search(/exec\s+"\$SETPRIV"/);
    assert.ok(nonrootExecAt >= 0, "must exec tini directly on the non-root path");
    assert.ok(setprivAt >= 0, "must setpriv-drop on the root path");
    assert.ok(nonrootExecAt < setprivAt, "the non-root single-uid exec must come before the setpriv drop");
    // Fail-safe against operator misconfig (audit M4 LOW): the non-root path clears the
    // split-activation env BEFORE exec, so a stray UZI_UID_SPLIT=1 on a #58 deploy can't
    // make the single-uid worker try (and EPERM) to setpriv-wrap runner spawns.
    const unsetAt = entrypoint.search(/unset\s+UZI_UID_SPLIT\s+UZI_RUNNER_PATH\s+UZI_RUNNER_TMPDIR/);
    assert.ok(unsetAt >= 0 && unsetAt < nonrootExecAt, "the non-root path must unset the split env before exec tini");
  });

  it("fails CLOSED on an unreadable uid (never silently takes the non-root branch)", () => {
    // If `id -u` yields empty/non-numeric, the entrypoint must exit — not default to
    // the non-root branch, which would run a ROOT start unhardened (token 0444, full
    // cap_add). The validation must precede the uid != 0 branch.
    const validateAt = entrypoint.search(/''\|\*\[!0-9\]\*\)/);
    const branchAt = entrypoint.search(/\[\s*"\$uid"\s*!=\s*"0"\s*\]/);
    assert.ok(validateAt >= 0, "must reject an empty/non-numeric uid");
    assert.match(entrypoint, /''\|\*\[!0-9\]\*\)[^\n]*exit 1/, "the non-numeric-uid case must exit 1 (fail closed)");
    assert.ok(branchAt >= 0 && validateAt < branchAt, "uid validation must precede the uid != 0 branch");
  });

  it("logs the active containment posture (operator-visible; A1 vs single-uid)", () => {
    assert.match(entrypoint, /A1 uid-split active \(root-started\)/, "root path must log the A1 posture");
    assert.match(entrypoint, /single-uid non-root mode \(PRD #58\)/, "non-root path must log the single-uid posture");
  });

  it("carves out the runner-owned /data subtree ROOTS to worker:runner 2775, keeping repos/ worker-only (PRD #51 M4 (b))", () => {
    // The runner clone store + SDK/provision HOMEs are runner-writable (worker:runner,
    // setgid+group-write); the worker bare cache repos/ (the B2 code-exec surface) is
    // NOT in the carve-out list.
    assert.match(entrypoint, /RUNNER_TREE_OWNER=worker:runner/, "must own the runner subtrees worker:runner");
    assert.match(entrypoint, /for d in runner agent-home provision; do/, "must carve out runner/agent-home/provision");
    assert.doesNotMatch(
      entrypoint,
      /for d in [^\n]*\brepos\b/,
      "the worker bare cache repos/ must NOT be in the runner-writable carve-out",
    );
    // chmod 2775 (setgid+group-write) BEFORE chown (no CAP_FOWNER; setgid survives a
    // dir chown), so children inherit group `runner` for the runner's per-run dirs.
    const chmodAt = entrypoint.search(/"\$CHMOD"\s+2775\s+"\/data\/\$d"/);
    const chownAt = entrypoint.search(/"\$CHOWN"\s+"\$RUNNER_TREE_OWNER"\s+"\/data\/\$d"/);
    assert.ok(chmodAt >= 0 && chownAt >= 0 && chmodAt < chownAt, "chmod 2775 must precede the chown (no CAP_FOWNER)");
    // Resume guard (PRD #51 M4): the carve-out chown is NON-recursive (roots only) so an
    // every-boot re-own never clobbers the runner's own per-run resume state.
    assert.doesNotMatch(
      entrypoint,
      /"\$CHOWN"\s+-R\s+"\$RUNNER_TREE_OWNER"/,
      "the carve-out chown must be NON-recursive (resume guard) — never chown -R the runner subtrees",
    );
    // Restart-safety (tester e2e crash-on-restart): the carve-out chmod is guarded on
    // root-ownership (`[ -O ]`), so it runs ONLY on a fresh (root-owned) dir. An
    // unconditional chmod on a persisted, now-worker-owned dir EPERMs (no CAP_FOWNER) ->
    // set -eu -> deterministic crash on every restart.
    assert.match(
      entrypoint,
      /\[\s*-O\s+"\/data\/\$d"\s*\]\s*&&\s*"\$CHMOD"\s+2775\s+"\/data\/\$d"/,
      "the carve-out chmod must be guarded on root-ownership ([ -O ]) for restart safety",
    );
    assert.doesNotMatch(
      entrypoint,
      /^\s*"\$CHMOD"\s+2775\s+"\/data\/\$d"/m,
      "there must be NO unconditional chmod 2775 of the runner subtree (the restart-crash)",
    );
  });

  it("activates the M4 uid split: /nix runner-owned, worker PATH stripped, split env exported (PRD #51 M4)", () => {
    // /nix is runner-OWNED under the split (worker fully off /nix); the worker→runner owner
    // change re-triggers migrate_tree's sentinel (handling the owner-keyed group guard).
    assert.match(entrypoint, /NIX_OWNER=runner:runner/, "/nix must be runner:runner under the A1 split");
    // The dropped worker keeps ONLY the stripped root-owned PATH — the full image PATH (nix
    // + JDK) is handed to the RUNNER via UZI_RUNNER_PATH, never restored onto the worker.
    assert.doesNotMatch(entrypoint, /export\s+PATH="\$IMAGE_PATH"/, "the worker PATH must NOT be restored to the /nix-bearing image PATH");
    assert.match(entrypoint, /export UZI_UID_SPLIT=1/, "must export UZI_UID_SPLIT=1 to activate the split");
    assert.match(entrypoint, /export UZI_RUNNER_PATH="\$IMAGE_PATH"/, "must hand the full image PATH to the runner via UZI_RUNNER_PATH");
    assert.match(entrypoint, /export UZI_RUNNER_TMPDIR="\$RUNNER_TMPDIR"/, "must export the runner's private TMPDIR");
    // These split-activating exports must be on the ROOT (A1) path, AFTER the non-root
    // single-uid exec (which never activates the split).
    const nonrootExecAt = entrypoint.search(/exec\s+"\$TINI"\s+--\s+"\$@"/);
    const splitAt = entrypoint.search(/export UZI_UID_SPLIT=1/);
    assert.ok(nonrootExecAt >= 0 && splitAt > nonrootExecAt, "the split must activate only on the root path, after the non-root exec");
  });

  it("gives the worker and runner distinct 0700 TMPDIR trees, exporting only the worker's (5-bis)", () => {
    // The worker's private tmp is exported across the drop; the runner's is runner-owned
    // 0700 (owner-only — the worker, a `runner`-GROUP member, still cannot reach it) and
    // is wired into the runner env builders in M4.
    assert.match(entrypoint, /WORKER_TMPDIR=\/tmp\/uzi-worker/, "must define the worker tmp");
    assert.match(entrypoint, /RUNNER_TMPDIR=\/tmp\/uzi-runner/, "must define the runner tmp");
    assert.match(entrypoint, /"\$CHOWN"\s+runner:runner\s+"\$RUNNER_TMPDIR"/, "runner tmp must be runner:runner (0700 owner-only)");
    assert.match(entrypoint, /export TMPDIR="\$WORKER_TMPDIR"/, "must export only the worker tmp across the drop");
  });

  it("gates the volume migration on a post-completion sentinel (not the top-level owner)", () => {
    // The reviewer NIT: the skip decision must be a sentinel written AFTER the chown,
    // so it is robust to the base image's chown -R traversal order (not the top-level
    // owner, which a pre-order chown could set before the children are done).
    assert.match(entrypoint, /sentinel="\$path\/\.uzi-migrated-/, "must key the sentinel by the target owner");
    const skipAt = entrypoint.search(/\[\s*-f\s+"\$sentinel"\s*\]\s*&&\s*return 0/);
    const chownAt = entrypoint.search(/"\$CHOWN"\s+-R\s+"\$owner"\s+"\$path"/);
    const writeAt = entrypoint.search(/:\s*>\s*"\$sentinel"/);
    assert.ok(skipAt >= 0, "must skip when the sentinel exists");
    assert.ok(chownAt >= 0 && writeAt >= 0 && chownAt < writeAt, "the sentinel must be written AFTER the recursive chown");
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

// The rootless-DinD sidecar image pin (PRD #83) is a literal in TWO places: the
// compose track (docker-compose.yml `dind`, M2) and the k8s track
// (deploy/chart/values.yaml workers.docker.image, M3). Both tracks must run the
// IDENTICAL daemon — the compose Decision-3 efficacy result is only evidence for k8s
// if the image is the same — so pin them byte-equal here, mirroring the
// DEVBOX_VERSION lockstep above, so a bump to one that misses the other is a red
// build rather than a silent divergence.
describe("the DinD sidecar image is pinned identically on the compose and k8s tracks", () => {
  const repoRoot = path.resolve(templatesDir, "../..");
  const dindPin = /docker:[\w.-]+-dind-rootless@sha256:[0-9a-f]{64}/;
  const pinIn = (rel: string): string => {
    const text = fs.readFileSync(path.resolve(repoRoot, rel), "utf8");
    const m = dindPin.exec(text);
    assert.ok(m, `${rel} must carry a pinned docker:<tag>-dind-rootless@sha256 image`);
    return m![0];
  };
  it("docker-compose.yml == deploy/chart/values.yaml", () => {
    assert.strictEqual(
      pinIn("docker-compose.yml"),
      pinIn("deploy/chart/values.yaml"),
      "the compose and chart DinD image pins have drifted — bump both together (arch §Q6: one pin, two tracks)",
    );
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

// The committed devbox GLOBAL manifest (PRD #83 M1 Q2) is the single source of truth for
// the default worker toolchain both templates bake. Pin that it lists go + python + pip so
// a drop can't silently ship a worker without them. Read as text (devbox.json is HuJSON —
// it carries `//` comments — so JSON.parse would reject it).
describe("devbox-global default toolchain manifest", () => {
  const manifest = fs.readFileSync(path.resolve(templatesDir, "../devbox-global/devbox.json"), "utf8");
  const lock = fs.readFileSync(path.resolve(templatesDir, "../devbox-global/devbox.lock"), "utf8");
  it("lists go + python3 + pip", () => {
    assert.match(manifest, /"go"/, "manifest must list go");
    assert.match(manifest, /"python3"/, "manifest must list python3");
    assert.match(manifest, /python3Packages\.pip|"pip"/, "manifest must list pip");
  });
  // openssl is a baked base crypto/TLS CLI (cert gen / `openssl rand` / hashing), promoted
  // from tier-2 (run dd06c0ed) so its absence can't silently ship a worker that 127s any
  // repo's e2e/cert step.
  it("lists openssl.bin (baked crypto/TLS CLI, run dd06c0ed)", () => {
    // MUST be the `.bin` output — bare `openssl` installs only libs/man (no CLI), so the
    // build guard's `command -v openssl` 127s. See the manifest comment.
    assert.match(manifest, /"openssl\.bin"/, "manifest must list openssl.bin (the CLI-bearing output)");
    assert.doesNotMatch(manifest, /"openssl"\s*[,\]]/, "must NOT list bare openssl (installs no CLI)");
  });
  // PRD #87: the browser for the web-ux role rides this SAME shared toolchain, so a drop can't
  // silently ship a worker without a browser or its fonts (which would render every screenshot
  // as tofu). chromium is the browser; fontconfig + a font package make text legible.
  it("lists chromium + fontconfig + a font package (PRD #87)", () => {
    assert.match(manifest, /"chromium"/, "manifest must list chromium (the web-ux browser)");
    assert.match(manifest, /"fontconfig"/, "manifest must list fontconfig");
    assert.match(manifest, /"dejavu_fonts"|"liberation_ttf"/, "manifest must list a font package (else screenshots are tofu)");
  });
  // Every manifest package must be pinned in the sibling lock at the SAME nixpkgs rev (the
  // pin the Dockerfiles COPY in before `devbox global install`); an unpinned package floats.
  it("pins every add-on package in devbox.lock at the toolchain nixpkgs rev", () => {
    const revMatch = /github:NixOS\/nixpkgs\/([0-9a-f]{40})#go\b/.exec(lock);
    assert.ok(revMatch, "lock must pin go at a concrete nixpkgs rev to anchor the toolchain pin");
    const rev = revMatch![1];
    for (const pkg of ["chromium", "fontconfig", "dejavu_fonts", "openssl.bin"]) {
      assert.match(
        lock,
        new RegExp(`github:NixOS/nixpkgs/${rev}#${pkg}\\b`),
        `devbox.lock must pin ${pkg} at the same rev (${rev}) as the rest of the toolchain`,
      );
    }
  });
});

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
