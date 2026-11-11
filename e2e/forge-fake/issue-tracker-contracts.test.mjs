import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import { once } from "node:events";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import path from "node:path";
import test, { run as runTests } from "node:test";
import { tap } from "node:test/reporters";
import { fileURLToPath } from "node:url";

// Runs the shared issue tracker contract (issuetrackertest.RunContract) against
// the real GitLab, GitHub and Forgejo drivers over the Node HTTPS fake started
// with FORGE_FAKE_ISSUE_CONTRACT=1. A separate launcher from
// operation-contracts.test.mjs because that one pins a single Go argv.
const root = fileURLToPath(new URL("../../", import.meta.url));
const separator = process.argv.indexOf("--");
const command = process.argv.slice(separator + 1);
assert.ok(separator >= 2, "precondition: pass the Go command after --");
assert.equal(command[0], "go", "precondition: expected go executable");
assert.equal(command[1], "test");
const flags = command.slice(2, -1);
assert.equal(command.at(-1), "./internal/issuetracker");
for (const pattern of [/^-buildvcs=false$/, /^-count=1$/, /^-v$/, /^-timeout=[1-9]\d*s$/, /^-run=\^TestIssueTrackerForgeFakeContracts\$$/]) {
  assert.equal(flags.filter((flag) => pattern.test(flag)).length, 1, `precondition: Go command missing/duplicated ${pattern}`);
}
assert.equal(flags.length, 5);
console.log("# Go command:", command.join(" "));

function run(executable, args) {
  try {
    return execFileSync(executable, args, { cwd: root, encoding: "utf8", timeout: 10000, stdio: "pipe" }).trim();
  } catch (error) {
    throw new Error(`precondition/fixture command failed: ${executable} ${args.join(" ")}\n${error.stderr ?? ""}`, { cause: error });
  }
}

// Only owned children are signalled. TERM has a two-second grace, then KILL and reap.
async function stop(child) {
  if (!child || child.exitCode !== null || child.signalCode !== null) return;
  const closed = once(child, "close");
  child.kill("SIGTERM");
  const timer = setTimeout(() => child.kill("SIGKILL"), 2000);
  try { await closed; } finally { clearTimeout(timer); }
}

// The only subtests that may skip: the forge subjects supply no MoveIssue seam,
// so the moved-issue case runs on YouTrack only, and forge labels have no
// owners, so the same-name tag case (Subject.SameName) does too. ProjectGuard
// must pass on every forge.
const ALLOWED_SKIPS = new Set(["MovedIssueIsRefused", "SameNameTagUsesEligibilityTag"]);
const CASES = [
  "EnsureLabelsIsIdempotent", "CreateIssueRoundTrips", "ListIssuesFilters", "UpdateLabelsAndDescription",
  "SetIssueStateClosesAndReopens", "CommentsOldestFirst", "LabelEventsOldestFirst", "MissingIssueIsNotFound", "MovedIssueIsRefused",
  "ProjectGuard", "SameNameTagUsesEligibilityTag",
];
const DIALECTS = ["gitlab", "github", "forgejo"];

async function contracts(t) {
  console.log("#", run("go", ["version"]));
  console.log("#", run("openssl", ["version"]));
  mkdirSync(path.join(root, ".uzi/scratch"), { recursive: true });
  const scratch = mkdtempSync(path.join(root, ".uzi/scratch/issue-tracker-contracts-"));
  let fake;
  let go;
  let diagnostics = "";
  t.after(async () => {
    await stop(go);
    await stop(fake);
    rmSync(scratch, { recursive: true, force: true });
  });
  const cert = path.join(scratch, "cert.pem");
  const key = path.join(scratch, "key.pem");
  run("openssl", ["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-subj", "/CN=localhost", "-addext", "subjectAltName=IP:127.0.0.1", "-keyout", key, "-out", cert]);
  const gitRoot = path.join(scratch, "gitroot");
  mkdirSync(gitRoot);
  const statePath = path.join(scratch, "state.json");
  writeFileSync(statePath, "{}");
  fake = spawn(process.execPath, [path.join(root, "e2e/forge-fake/forge-fake.mjs")], {
    cwd: root, env: { PATH: process.env.PATH, FORGE_FAKE_PORT: "0", FORGE_FAKE_CERT: cert, FORGE_FAKE_KEY: key,
      FORGE_FAKE_STATE: statePath, FORGE_FAKE_BASE_URL: "https://configured.invalid",
      FORGE_FAKE_PROJECT: "group/repo", FORGE_FAKE_GIT_ROOT: gitRoot, FORGE_FAKE_ISSUE_CONTRACT: "1" },
    stdio: ["ignore", "pipe", "pipe"],
  });
  fake.stdout.on("data", (data) => { diagnostics = (diagnostics + data).slice(-65536); });
  fake.stderr.on("data", (data) => { diagnostics = (diagnostics + data).slice(-65536); });
  const port = await new Promise((resolve, reject) => {
    const timer = setTimeout(() => finish(new Error(`fake startup timed out\n${diagnostics}`)), 10000);
    const onData = () => { const match = diagnostics.match(/listening on :(\d+)/); if (match) finish(null, Number(match[1])); };
    const onExit = (code, signal) => finish(new Error(`fake startup exit ${code}/${signal}\n${diagnostics}`));
    const onError = (error) => finish(error);
    function finish(error, value) {
      clearTimeout(timer);
      fake.stdout.off("data", onData);
      fake.off("exit", onExit);
      fake.off("error", onError);
      if (error) reject(error); else resolve(value);
    }
    fake.stdout.on("data", onData);
    fake.once("exit", onExit);
    fake.once("error", onError);
    onData();
  });
  const endpoint = `https://127.0.0.1:${port}`;

  await t.test("Go issue tracker contract over trusted HTTPS", async () => {
    let output = "";
    const started = performance.now();
    go = spawn(command[0], command.slice(1), { cwd: path.join(root, "api"),
      env: { ...process.env, SSL_CERT_FILE: cert, HTTPS_PROXY: "", HTTP_PROXY: "", ALL_PROXY: "", NO_PROXY: "127.0.0.1",
        UZI_FORGE_FAKE_REQUIRED: "1", UZI_FORGE_FAKE_ENDPOINT: endpoint },
      stdio: ["ignore", "pipe", "pipe"] });
    for (const stream of [go.stdout, go.stderr]) stream.on("data", (data) => {
      output += data;
      if (output.length > 2 * 1024 * 1024) go.kill("SIGKILL");
    });
    const timer = setTimeout(() => { console.error("Go child exceeded 240s including compilation"); go.kill("SIGKILL"); }, 240000);
    let code;
    try { [code] = await once(go, "close"); } finally { clearTimeout(timer); }
    console.log(output.trim().split("\n").map((line) => "# " + line).join("\n"));
    console.log(`# Go child wall ${((performance.now() - started) / 1000).toFixed(2)}s (includes compilation)`);
    assert.equal(code, 0, `Go contract failed\n${output}\nfake diagnostics:\n${diagnostics}`);
    assert.match(output, /^=== RUN\s+TestIssueTrackerForgeFakeContracts$/m);
    assert.match(output, /^--- PASS: TestIssueTrackerForgeFakeContracts /m);
    for (const dialect of DIALECTS) {
      for (const name of CASES) {
        const full = `TestIssueTrackerForgeFakeContracts/${dialect}/${name}`;
        const escaped = full.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
        assert.match(output, new RegExp(`^\\s*--- (PASS|SKIP): ${escaped} `, "m"), `subtest did not run: ${full}`);
        if (!ALLOWED_SKIPS.has(name)) {
          assert.match(output, new RegExp(`^\\s*--- PASS: ${escaped} `, "m"), `subtest must pass, not skip: ${full}`);
        }
      }
    }
    for (const line of output.split("\n").filter((l) => /--- SKIP:/.test(l))) {
      const match = line.match(/--- SKIP: TestIssueTrackerForgeFakeContracts\/(gitlab|github|forgejo)\/(\w+) /);
      assert.ok(match && ALLOWED_SKIPS.has(match[2]), `unexpected skip: ${line.trim()}`);
    }
  });
}

// Select TAP explicitly: Node versions differ in their default reporter.
const results = runTests({ files: [], isolation: "none", setup() {
  test("issue tracker contract against the forge fake over HTTPS", { timeout: 330000 }, contracts);
} });
results.on("test:fail", () => { process.exitCode = 1; });
results.compose(tap).pipe(process.stdout);
