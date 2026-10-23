// M3: real production provisioning/command seams. The host wrapper removes
// networking only after setup; it never supplies a replacement command PATH.
import assert from "node:assert/strict";
import { displayJSON } from "./display.js";
import { createCriteria } from "./criteria.js";
import { captureOutput } from "./capture.js";
import { spawnSync } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import fs from "node:fs/promises";
import path from "node:path";
import { runRunner, runnerFailure } from "./runner.js";
import { pathToFileURL } from "node:url";
import { inventory, verifyPNGs } from "./tree.js";
import { boundaryProbe, homeSnapshot } from "./probes.js";
import { readRegularFile, sourceId, HELPER_MAX_BYTES } from "./inputs.js";

const load = (name: string) => import(pathToFileURL(path.join("/app/src", name)).href);
const start = performance.now();
const record = (kind: string, data: Record<string, unknown>) =>
  console.log(displayJSON({ kind, elapsedMs: Math.round(performance.now() - start), ...data }));
const labRelative = "api/cmd/uzi/uxlab";
const sha = (bytes: Buffer) => createHash("sha256").update(bytes).digest("hex");

async function main() {
  assert(process.env.UZI_UID_SPLIT === "1" && process.getuid?.() === 10001,
    "real entrypoint uid split is required (harness absence is not base-red)");
  process.umask(0o002);
  const cell = process.env.UXLAB_RENDER_CELL;
  const mode = process.env.UXLAB_RENDER_MODE;
  assert(cell === "base" || cell === "candidate" || cell === "local");
  assert(mode === "off" || mode === "required");
  const provision = await load("provision-run.js");
  const codex = await load("codex/codex-executor.js");
  const launcher = await load("codex/launcher.js");
  const registry = await load("codex/registry.js");
  const runner = await load("runner-uid.js");
  const { createLogger } = await load("log.js");
  const warnings: string[] = [];
  const log = createLogger("info");
  const warn = log.warn.bind(log);
  log.warn = (message: string, fields?: Record<string, unknown>) => { warnings.push(message); warn(message, fields); };
  const runAsRunner = async (command: string, args: string[], timeout = 30_000, cwd = "/app") => {
    return runRunner(runner.runnerCommand, command, args, { cwd,
      env: { PATH: runner.runnerPath(process.env), HOME: "/data/agent-home",
        TMPDIR: runner.runnerTmpdir(process.env) },
      timeout, record, killRunnerGroupOnly: runner.killRunnerGroupOnly });
  };
  const runnerUid = (await runAsRunner("/usr/bin/id", ["-u"])).stdout.trim();
  assert.equal(runnerUid, "10002");
  const helpers: Record<string, string> = {};
  for (const name of ["uzi-codex-supervisor", "uzi-codex-fileop", "uzi-codex-command-sandbox"])
    helpers[name] = sha(await readRegularFile(`/usr/local/bin/${name}`, HELPER_MAX_BYTES));
  record("identity", { cell, mode, runnerUid, imageId: process.env.UXLAB_RENDER_IMAGE_ID,
    sourceSha256: await sourceId("/app/src"), helpers, node: process.version,
    goPolicy: "GOTOOLCHAIN=local, >= go1.27.3; optional #3305 fixture symlink to readonly /usr/local/lib/uxlab-go distro" });
  if (mode === "required") {
    const probe = spawnSync("/usr/local/bin/uzi-codex-command-sandbox", ["--probe"],
      { encoding: "utf8", timeout: 10_000, maxBuffer: 1024 * 1024 });
    assert.ifError(probe.error);
    assert.equal(probe.status, 0, "required Landlock unavailable");
  }
  const runId = randomUUID(), checkout = `/data/runner/uxlab-${runId}`;
  const lab = path.join(checkout, labRelative);
  const copyStarted = performance.now();
  record("copy-output", { ...(await runAsRunner("/usr/local/bin/node",
    ["--import", "tsx", "/work/uxlab-render/copy.ts", "/work/repository", checkout,
      ...(cell === "base" ? ["/work/candidate-build.sh"] : [])], 120_000)) });
  const copyMs = Math.round(performance.now() - copyStarted);
  record("copy", { durationMs: copyMs, timeoutMs: 120_000 });
  assert.equal((await fs.stat(checkout)).uid, 10002);
  await runAsRunner("git", ["init", "-q", checkout]);
  if (cell === "base") {
    // copy.ts adds ONLY candidate build.sh, as runner; base manifests stay intact.
    record("build-overlay", { sha256: sha(await readRegularFile(path.join(lab, "build.sh"), 1024 * 1024)) });
    assert.equal((await fs.stat(path.join(lab, "build.sh"))).uid, 10002);
  }
  const manifestIds = {};
  for (const relative of ["devbox.json", `${labRelative}/devbox.json`]) {
    Object.assign(manifestIds, { [relative]: sha(await readRegularFile(path.join(checkout, relative), 1024 * 1024)) });
  }
  if (cell === "base") {
    assert.equal((manifestIds as Record<string, string>)["devbox.json"], "d3b4242b28ce03f6fafa548aff5edc8c67ce28d5c0b708f259676f78a578e33b", "frozen 3fb1fdbf root manifest differs");
    assert.equal((manifestIds as Record<string, string>)[`${labRelative}/devbox.json`], "c3fdf4d3e91ce9fe73acde30b51e2042e827c3f33dbf5485407579952be1182c", "frozen nested manifest differs");
  }
  record("checkout", { checkout, copyMs, manifests: manifestIds });
  const { extractRepoDevboxPackages } = await load("repo-tools.js");
  const packages: string[] = await extractRepoDevboxPackages(checkout);
  assert(packages.includes("ruby@4.0.6"), "expected root Ruby package");
  assert.equal(packages.includes("charm-freeze@0.2.2"), cell !== "base", "root freeze policy differs");
  const events: string[] = [];
  const provisionStarted = performance.now();
  const result = await provision.provisionRunTools({
    runId, worktreePath: checkout, config: { tool_packages: [], repo_devbox_opt_in: true },
    emit: (message: { payload: Record<string, unknown> }) => {
      events.push(String(message.payload.text));
      record("provision-event", { text: message.payload.text });
    },
  }, { provisionRoot: "/data/provision", homeDir: "/data/agent-home", log });
  record("provision", { cell, mode, durationMs: Math.round(performance.now() - provisionStarted), ...result });
  assert.equal(warnings.length, 0, "provisioning warning/degradation");
  const expectedEvents = [`merged ${packages.length} package(s) from this repo's devbox.json`,
    `provisioning ${packages.length} tool(s): ${packages.join(", ")}`, "tools provisioned"];
  assert.equal(events.length, expectedEvents.length, "provisioning degraded or emitted unexpected events");
  for (let i = 0; i < events.length; i++) assert(events[i] === expectedEvents[i], `provision event ${i} differs; see event receipt`);
  assert.equal(result.provisionDir, `/data/provision/${runId}`);
  assert(result.toolEnv.PATH?.includes(`/data/provision/${runId}/.devbox`));
  const commandEnv = codex.withCommandGitTrust(codex.buildCommandEnv("/tmp", result.toolEnv),
    await codex.canonicalCheckoutPath(checkout));
  const roots = new registry.ExecutionRegistry(registry.newLocalExecutionEpoch(1));
  const holder = await launcher.startCommandCacheHolder(randomUUID());
  const cache = new codex.HeldRunCommandCache(holder);
  cache.watchRegistry(roots);
  const spawn = codex.makeDefaultSpawnCommand(roots, launcher.launchCodexEffectRoot,
    5000, checkout, commandEnv, mode, log, cache, 900_000);
  const homes = new Set<string>();
  async function command(label: string, script: string, cwd = checkout,
    commandSpawn = spawn, commandRoots = roots, commandMode = mode) {
    const begun = performance.now();
    record("command-start", { label, mode: commandMode, cwd });
    const output = await commandSpawn(["/bin/bash", "-ceu", [
      'test "$(id -u)" = 10003',
      'printf "COMMAND_UID=%s COMMAND_GID=%s COMMAND_GROUPS=%s\\n" "$(id -u)" "$(id -g)" "$(id -G)"',
      'test "$HOME" = "$TMPDIR"',
      'test "$(stat -c %u "$HOME")" = 10003',
      'test "$(stat -c %a "$HOME")" = 700',
      'entries="$(ls -A "$HOME")"; test -z "$entries"',
      'printf "COMMAND_HOME=%s\\n" "$HOME"',
      'printf "COMMAND_HOME_OWNER=%s COMMAND_HOME_MODE=%s COMMAND_HOME_EMPTY=yes\\n" "$(stat -c %u "$HOME")" "$(stat -c %a "$HOME")"',
      script,
    ].join("\n")], { cwd });
    record("command", { label, mode: commandMode, durationMs: Math.round(performance.now() - begun), ...output });
    const home = /^COMMAND_HOME=(.+)$/m.exec(output.stdout)?.[1];
    assert(home && home.startsWith("/tmp/uzi-codex-command-"), "missing fresh command HOME receipt");
    assert(!homes.has(home), "command HOME reused");
    homes.add(home);
    assert.equal(commandRoots.hasLiveCommandRoot(), false, "command root not drained");
    return output;
  }
  const criteria = createCriteria((label) => record("criterion-failure", { label, cell, mode }));
  const { requireCriterion } = criteria;
  const goPolicy = 'export GOTOOLCHAIN=local GOFLAGS=-buildvcs=false\ncommand -v go\ngo version';
  try {
    const versionRoots = new registry.ExecutionRegistry(registry.newLocalExecutionEpoch(4));
    cache.watchRegistry(versionRoots);
    const versionSpawn = codex.makeDefaultSpawnCommand(versionRoots, launcher.launchCodexEffectRoot,
      5000, checkout, commandEnv, mode, log, cache, 15_000);
    const versionLog = path.join(checkout, ".uxlab-tool-versions");
    try {
      await command("nix-devbox-versions",
        "{ set +e\ncommand -v nix\nls -l \"$(command -v nix)\"\nreadlink -f \"$(command -v nix)\"\nnix --version\nprintf 'NIX_VERSION_STATUS=%s\\n' \"$?\"\ncommand -v devbox\ndevbox version\n} 2>&1 | " + captureOutput + " > " + quote(versionLog),
        checkout, versionSpawn, versionRoots);
    } catch (error) { record("version-diagnostic-error", { ...runnerFailure(error) }); }
    assert.equal(versionRoots.hasLiveCommandRoot(), false);
    const versionBytes = await readRegularFile(versionLog, 1048576).catch(() => Buffer.alloc(0));
    record("tool-version-output", { bytes: versionBytes.length, prefix: versionBytes.toString("utf8").slice(0, 16384) });
    const setupStarted = performance.now();
    const setup = await command("network-enabled-go-prewarm", `${goPolicy}
go mod download
go test ./cmd/uzi/ -run '^$' -count=1`, path.join(checkout, "api"));
    assert.equal(setup.code, 0, "Go setup failed; never count harness/toolchain failure as base-red");
    const version = /go version go(\d+)\.(\d+)\.(\d+)/.exec(setup.stdout);
    assert(version && (Number(version[1]) > 1 || Number(version[1]) === 1 &&
      (Number(version[2]) > 27 || Number(version[2]) === 27 && Number(version[3]) >= 3)), "Go >= 1.27.3 required");
    record("setup", { durationMs: Math.round(performance.now() - setupStarted), cache: cache.current() });
    if (cell === "local") {
      // Separate runner-owned nested Devbox store leg; never a model command.
      await runAsRunner("/bin/rm", ["-rf", "--", path.join(lab, "frames"), path.join(lab, "png")]);
      const positive = await runAsRunner("/bin/bash", ["-ceu",
        `export GOTOOLCHAIN=local GOFLAGS=-buildvcs=false\ndevbox run -- go version\ndevbox run build`], 900_000, lab);
      record("local-devbox", positive);
      assert(positive.stdout.includes(version[0]), "local Devbox leg must use exactly the command setup Go version");
      record("artifacts", { phase: "local", pngs: await verifyPNGs(lab) });
      await runAsRunner("/bin/bash", ["-ceu", "mkdir proof; cp -a png proof/local"], 30_000, lab);
      await fs.writeFile("/data/uxlab-artifacts", path.join(lab, "proof"));
      record("result", { status: "PASS", cell, mode });
      return;
    }
    const sentinel = `/nix/uxlab-sentinel-${runId}`;
    await runAsRunner("/usr/local/bin/node", ["-e",
      'process.umask(0); require("node:fs").writeFileSync(process.argv[1], "", { flag: "wx", mode: 0o666 });', sentinel]);
    try {
      // Calibration uses exactly the same path/flags, off-mode command identity.
      const calibrationRoots = new registry.ExecutionRegistry(registry.newLocalExecutionEpoch(2));
      const calibrationSpawn = codex.makeDefaultSpawnCommand(calibrationRoots, launcher.launchCodexEffectRoot,
        5000, checkout, commandEnv, "off", log, cache, 30_000);
      cache.watchRegistry(calibrationRoots);
      const calibration = await command("sentinel-calibration",
        "/usr/local/bin/node -e " + quote(boundaryProbe) + " " + quote(sentinel) + " off sentinel",
        checkout, calibrationSpawn, calibrationRoots, "off");
      assert.equal(calibration.code, 0, "sentinel must be writable by cmd without Landlock");
      const boundary = await command("nix-boundary", "/usr/local/bin/node -e " + quote(boundaryProbe) + " " + quote(sentinel) + " " + mode);
      requireCriterion("Nix open/sentinel boundary", boundary.code === 0);
    } finally { await runAsRunner("/bin/rm", ["--", sentinel]); }
    const controlRoots = new registry.ExecutionRegistry(registry.newLocalExecutionEpoch(3));
    cache.watchRegistry(controlRoots);
    const controlSpawn = codex.makeDefaultSpawnCommand(controlRoots, launcher.launchCodexEffectRoot,
      5000, checkout, commandEnv, mode, log, cache, 60_000);
    const controlLog = path.join(lab, ".uxlab-devbox-control-" + randomUUID());
    let negative: { code: number | null } | undefined;
    try {
      negative = await command("command-devbox-negative",
        "set +e\nset -o pipefail\ndevbox run build 2>&1 | " + captureOutput + " > " + quote(controlLog) +
        '\nexit "${PIPESTATUS[0]}"', lab, controlSpawn, controlRoots);
    } catch (error) {
      record("control-error", { ...runnerFailure(error) });
    }
    assert.equal(controlRoots.hasLiveCommandRoot(), false, "control root must drain after error");
    const controlBytes = await readRegularFile(controlLog, 1048576).catch(() => Buffer.alloc(0));
    const controlText = controlBytes.toString("utf8");
    record("control-output", { bytes: controlBytes.length, prefix: controlText.slice(0, 16384) });
    requireCriterion("command Devbox exits nonzero", negative !== undefined && negative.code !== 0 && negative.code !== null);
    requireCriterion("command Devbox specifically denies Nix big-lock",
      negative !== undefined && /big-lock[^\n]*(?:Permission denied|permission denied)|(?:Permission denied|permission denied)[^\n]*big-lock/.test(controlText));
    // Host only disconnects this exact owned container after this marker.
    await fs.writeFile("/data/uxlab-network-ready", "ready");
    const waitStarted = performance.now();
    while (!(await fs.stat("/data/uxlab-offline").then(() => true, () => false))) {
      assert(performance.now() - waitStarted < 120_000, "host offline handoff timed out");
      await new Promise((resolve) => setTimeout(resolve, 250));
    }
    const offline = 'export GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GOFLAGS=-buildvcs=false\n';
    if (cell === "base") {
      assert.equal((await command("reset-base-outputs", "rm -rf -- frames png", lab)).code, 0);
      await runAsRunner("/bin/bash", ["-ceu",
        'mkdir frames png; printf "seed frame" > frames/seed-dark.ansi; printf "seed png" > png/seed-dark.png'], 30_000, lab);
      const seedInventory = async () => {
        const entries: Awaited<ReturnType<typeof inventory>> = {};
        for (const dir of ["frames", "png"])
          for (const [name, receipt] of Object.entries(await inventory(path.join(lab, dir), undefined, undefined, false)))
            Object.assign(entries, { [`${dir}/${name}`]: receipt });
        return entries;
      };
      const before = await seedInventory();
      const red = await command("offline-base-build", offline + "./build.sh", lab);
      assert(red.code !== 0 && red.code !== null, "base must reject missing freeze");
      assert.equal(red.stderr, "uxlab: freeze is missing. Check that devbox opt-in is enabled for this repo in uzi and that run provisioning reported no package warnings, or run devbox run build locally.\n");
      assert(!/frames written|rendered \d+ PNG/.test(red.stdout), "base generated output before preflight");
      const after = await seedInventory();
      const keys = Object.keys(before);
      const afterKeys = Object.keys(after);
      assert.equal(keys.length, afterKeys.length, "seed inventory count changed");
      for (let i = 0; i < keys.length; i++) assert(keys[i] === afterKeys[i], `seed inventory first mismatch at index ${i}`);
      for (const key of keys) assert(JSON.stringify(before[key]) === JSON.stringify(after[key]), `seed bytes/mtime changed: ${key}`);
      record("base-red", { seeds: keys.map((name) => ({ name, ...after[name] })) });
    } else {
      const freeze = await command("freeze-version", "command -v freeze; freeze --version");
      assert.equal(freeze.code, 0);
      assert.match(freeze.stdout, /\b0\.2\.2\b/);
      assert.equal((await command("reset-candidate-outputs", "rm -rf -- frames png", lab)).code, 0);
      const buildLog = path.join(lab, ".uxlab-build-output");
      let green;
      try {
        green = await command("offline-full-build",
          offline + "set -o pipefail\n./build.sh 2>&1 | " + captureOutput + " > " + quote(buildLog), lab);
      } catch (error) {
        for (const directory of ["frames", "png"]) {
          let count = 0;
          const entries = await fs.opendir(path.join(lab, directory)).catch(() => undefined);
          if (entries) {
            try { while (await entries.read()) { if (++count > 2048) break; } }
            finally { await entries.close(); }
          }
          record("build-failure-inventory", { directory, count });
        }
        const bytes = await readRegularFile(buildLog, 1048576).catch(() => Buffer.alloc(0));
        record("build-failure-output", { bytes: bytes.length, prefix: bytes.toString("utf8").slice(0, 16384) });
        throw error;
      }
      assert.equal(green.code, 0, "full real generation/render failed");
      record("artifacts", { phase: "full-build", pngs: await verifyPNGs(lab) });
      await runAsRunner("/bin/bash", ["-ceu", "mkdir proof; cp -a png proof/full-build"], 30_000, lab);
      await fs.writeFile("/data/uxlab-artifacts", path.join(lab, "proof"));
      assert.equal((await command("reset-render-outputs", "rm -rf -- png", lab)).code, 0);
      const render = await command("offline-render-no-home-writes", offline +
        "/usr/local/bin/node -e " + quote(homeSnapshot) + ' > .uxlab-home-before\n./render.sh\n' +
        "/usr/local/bin/node -e " + quote(homeSnapshot) + ' > .uxlab-home-after\ncmp .uxlab-home-before .uxlab-home-after\nrm .uxlab-home-before .uxlab-home-after', lab);
      assert.equal(render.code, 0, "offline render wrote HOME or failed");
      record("artifacts", { phase: "offline-render", pngs: await verifyPNGs(lab) });
      await runAsRunner("/bin/cp", ["-a", path.join(lab, "png"), path.join(lab, "proof", "offline-render")]);
      record("rendering-complete", { cell, mode });
    }
    for (const [relative, expected] of Object.entries(manifestIds))
      assert.equal(sha(await readRegularFile(path.join(checkout, relative), 1024 * 1024)), expected, `manifest mutated: ${relative}`);
    const { exitCode, ...criterionResult } = criteria.result();
    record("result", { ...criterionResult, cell, mode, commands: homes.size });
    if (exitCode) process.exitCode = exitCode;
  } finally {
    assert(cache.drained(), "cache roots did not drain");
    await cache.settle(log, (token: string, timeoutMs: number) => launcher.removeCommandCache(token, { timeoutMs }),
      { releaseMs: 10_000, removeMs: 10_000 });
    const settled = await holder.release(cache.drained(), 10_000);
    record("cache-settled", settled);
    assert(settled.state === "removed" || settled.state === "absent", "cache cleanup must be complete, not pending");
    assert.equal(warnings.length, 0, "cache/provision warning");
  }
}
function quote(value: string) { return "'" + value.replaceAll("'", "'\\''") + "'"; }
main().catch((error) => { record("result", { status: "FAIL", ...runnerFailure(error),
  stack: String(error?.stack ?? "").slice(0, 4096) }); process.exitCode = 1; });
