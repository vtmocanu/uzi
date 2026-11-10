#!/usr/bin/env node
// Hermetic extraction/grouping contract for uzi's Go-toolchain Renovate rules.
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(scriptDir, "..");
const read = (name) => readFileSync(path.join(repoRoot, name), "utf8");
const config = JSON.parse(read("renovate.json"));

const fail = (message) => {
  throw new Error(`renovate-go-toolchain.test.mjs: ${message}`);
};
const assert = (condition, message) => {
  if (!condition) fail(message);
};
const assertEqual = (actual, expected, message) => {
  if (actual !== expected) fail(`${message}: expected ${JSON.stringify(expected)}, got ${JSON.stringify(actual)}`);
};

const genericManager = config.customManagers.find(
  (manager) => manager.datasourceTemplate === "{{{datasource}}}",
);
const attachmentManager = config.customManagers.find(
  (manager) => manager.datasourceTemplate === "github-release-attachments" &&
    manager.packageNameTemplate?.includes("{{#if packageName}}"),
);
assert(genericManager, "generic annotated-version manager is missing");
assert(attachmentManager, "release-attachment manager is missing");
assertEqual(
  attachmentManager.packageNameTemplate,
  "{{#if packageName}}{{{packageName}}}{{else}}{{{depName}}}{{/if}}",
  "release-attachment packageNameTemplate",
);

const extract = (manager, content, packageFile) => {
  assertEqual(manager.matchStrings.length, 1, `${packageFile} manager matchStrings count`);
  const regex = new RegExp(manager.matchStrings[0], "g");
  return [...content.matchAll(regex)].map((match) => ({
    ...match.groups,
    packageName: match.groups.packageName || match.groups.depName,
    packageFile,
  }));
};

const taskDeps = extract(genericManager, read("Taskfile.yml"), "Taskfile.yml").filter(
  (dep) => dep.depName === "golangci-lint-taskfile",
);
const archiveDeps = extract(
  attachmentManager,
  read("scripts/golangci-lint.sh"),
  "scripts/golangci-lint.sh",
).filter((dep) => dep.depName.startsWith("golangci-lint-"));
const linterDeps = [...taskDeps, ...archiveDeps];

assertEqual(taskDeps.length, 1, "Taskfile linter dependency count");
assertEqual(archiveDeps.length, 2, "per-platform linter dependency count");
assertEqual(
  linterDeps.map((dep) => dep.depName).sort().join(","),
  [
    "golangci-lint-darwin-arm64",
    "golangci-lint-linux-amd64",
    "golangci-lint-taskfile",
  ].join(","),
  "linter dependency aliases",
);
for (const dep of linterDeps) {
  assertEqual(dep.packageName, "golangci/golangci-lint", `${dep.depName} packageName`);
}

const normalizedVersions = new Set(
  linterDeps.map((dep) => dep.currentValue.replace(/^v/, "")),
);
assertEqual(normalizedVersions.size, 1, "Taskfile and archive versions must agree");
assertEqual(
  new Set(archiveDeps.map((dep) => dep.currentDigest)).size,
  2,
  "platform archives need distinct digests",
);
for (const dep of archiveDeps) {
  assert(/^[0-9a-f]{64}$/.test(dep.currentDigest), `${dep.depName} digest is not sha256`);
}

const matchesPattern = (value, pattern) => {
  if (pattern.startsWith("/") && pattern.endsWith("/")) {
    return new RegExp(pattern.slice(1, -1)).test(value);
  }
  return value.toLowerCase() === pattern.toLowerCase();
};
const matchesAny = (value, patterns) => patterns.some((pattern) => matchesPattern(value, pattern));
const ruleMatches = (rule, dep, updateType) => {
  if (rule.matchDepNames && !matchesAny(dep.depName, rule.matchDepNames)) return false;
  if (rule.matchPackageNames && !matchesAny(dep.packageName, rule.matchPackageNames)) return false;
  if (rule.matchUpdateTypes && !rule.matchUpdateTypes.includes(updateType)) return false;
  if (rule.matchManagers && !rule.matchManagers.includes(dep.manager)) return false;
  if (rule.matchDatasources && !rule.matchDatasources.includes(dep.datasource)) return false;
  return true;
};
const resolved = (dep, updateType) => {
  const result = {};
  for (const rule of config.packageRules) {
    if (!ruleMatches(rule, dep, updateType)) continue;
    if (Object.hasOwn(rule, "groupName")) result.groupName = rule.groupName;
    if (Object.hasOwn(rule, "separateMajorMinor")) {
      result.separateMajorMinor = rule.separateMajorMinor;
    }
  }
  return result;
};

const goSurfaces = [
  { depName: "go", packageName: "go", manager: "gomod", datasource: "golang-version" },
  { depName: "go", packageName: "actions/go-versions", manager: "github-actions", datasource: "golang-version" },
  { depName: "golang", packageName: "golang", manager: "dockerfile", datasource: "docker" },
];
for (const dep of [...goSurfaces, ...linterDeps]) {
  const result = resolved(dep, "minor");
  assertEqual(result.groupName, "Go toolchain", `${dep.depName} minor group`);
  assertEqual(result.separateMajorMinor, false, `${dep.depName} major/minor separation`);
}
for (const dep of linterDeps) {
  assertEqual(resolved(dep, "patch").groupName, "golangci-lint", `${dep.depName} patch group`);
}

// govulncheck: the api + controller Taskfile pins are distinct aliases of one package, so a
// bump must resolve both into one group rather than two PRs that skew the pins (#1487).
const vulnDeps = extract(genericManager, read("Taskfile.yml"), "Taskfile.yml").filter(
  (dep) => dep.depName.startsWith("govulncheck-"),
);
assertEqual(
  vulnDeps.map((dep) => dep.depName).sort().join(","),
  "govulncheck-api,govulncheck-controller",
  "govulncheck dependency aliases",
);
assertEqual(new Set(vulnDeps.map((dep) => dep.currentValue)).size, 1, "govulncheck pins must agree");
for (const dep of vulnDeps) {
  assertEqual(dep.packageName, "golang.org/x/vuln", `${dep.depName} packageName`);
  for (const updateType of ["patch", "minor", "major"]) {
    assertEqual(resolved(dep, updateType).groupName, "govulncheck", `${dep.depName} ${updateType} group`);
  }
}
assertEqual(resolved(goSurfaces[0], "patch").groupName, undefined, "Go patch grouping");
assertEqual(resolved(goSurfaces[2], "digest").groupName, undefined, "golang digest grouping");

// Go module dependencies get one non-major group per module; the go/toolchain directives
// (golang-version datasource, also extracted by gomod) must stay out of it.
const goModule = { depName: "golang.org/x/net", packageName: "golang.org/x/net", manager: "gomod", datasource: "go" };
for (const updateType of ["patch", "minor"]) {
  assertEqual(resolved(goModule, updateType).groupName, "Go modules (non-major)", `Go module ${updateType} group`);
}
assertEqual(resolved(goModule, "major").groupName, undefined, "Go module majors stay separate");
const moduleRule = config.packageRules.find((rule) => rule.groupName === "Go modules (non-major)");
assertEqual(moduleRule.additionalBranchPrefix, "{{parentDir}}-", "Go module group is per module");
assertEqual(resolved(goSurfaces[0], "minor").groupName, "Go toolchain", "go directive stays in the toolchain group");

// sqlc's CI pin and per-platform wrapper archives must advance together.
const sqlcWrapperDeps = extract(
  attachmentManager,
  read("scripts/sqlc.sh"),
  "scripts/sqlc.sh",
);
assertEqual(sqlcWrapperDeps.length, 3, "sqlc platform dependency count");
assertEqual(
  sqlcWrapperDeps.map((dep) => dep.depName).sort().join(","),
  "sqlc-darwin-arm64,sqlc-linux-amd64,sqlc-linux-arm64",
  "sqlc dependency aliases",
);
const sqlcCIDeps = extract(
  attachmentManager,
  read(".github/workflows/ci.yml"),
  ".github/workflows/ci.yml",
).filter((dep) => dep.packageName === "sqlc-dev/sqlc");
assertEqual(sqlcCIDeps.length, 1, "sqlc CI dependency count");
const sqlcDeps = [...sqlcCIDeps, ...sqlcWrapperDeps];
assertEqual(new Set(sqlcDeps.map((dep) => dep.currentValue)).size, 1, "sqlc versions must agree");
assertEqual(
  new Set(sqlcWrapperDeps.map((dep) => dep.currentDigest)).size, 3,
  "sqlc platform archives need distinct digests",
);
assertEqual(
  sqlcCIDeps[0].currentDigest,
  sqlcWrapperDeps.find((dep) => dep.depName === "sqlc-linux-amd64").currentDigest,
  "sqlc Linux wrapper and CI digests must agree",
);
for (const dep of sqlcDeps) {
  assertEqual(dep.packageName, "sqlc-dev/sqlc", `${dep.depName} packageName`);
  assert(/^[0-9a-f]{64}$/.test(dep.currentDigest), `${dep.depName} digest is not sha256`);
  for (const updateType of ["patch", "minor", "major", "digest"]) {
    assertEqual(resolved(dep, updateType).groupName, "sqlc", `${dep.depName} ${updateType} group`);
    assertEqual(resolved(dep, updateType).separateMajorMinor, false, `${dep.depName} separation`);
  }
}

const flake = config.customManagers.find(manager => manager.depNameTemplate === "worker-go-nixpkgs");
assert(flake, "worker Go flake manager is missing");
assertEqual(flake.datasourceTemplate, "git-refs", "worker Go datasource");
assertEqual(flake.currentValueTemplate, "nixpkgs-unstable", "worker Go channel");
assertEqual(flake.packageNameTemplate, "https://github.com/NixOS/nixpkgs", "worker Go repository");
const digests = [];
for (const file of ["agent/devbox-global/devbox.json", "agent/devbox-global/toolchain-guard.tsv", "api/internal/toolseed/devbox.json"]) {
  assert(flake.managerFilePatterns.some(pattern => matchesPattern(file, pattern)), `${file} must be refreshed together`);
  const deps = extract(flake, read(file), file);
  assertEqual(deps.length, 1, `${file} worker Go dependency count`);
  assert(/^[a-f0-9]{40}$/.test(deps[0].currentDigest), `${file} immutable digest`);
  digests.push(deps[0].currentDigest);
}
assertEqual(new Set(digests).size, 1, "manifest, seed and guard Go revisions agree");
const manual = config.packageRules.find(rule => rule.matchDepNames?.includes("worker-go-nixpkgs"));
assertEqual(manual.automerge, false, "worker Go digest needs maintainer lock/image review");
assertEqual(JSON.stringify(manual.schedule), JSON.stringify(["* 0-5 1-7 * 6"]), "worker Go digest updates are monthly, on the first weekly window");
assertEqual(manual.minimumReleaseAgeBehaviour, "timestamp-optional", "git-refs has no release timestamp");
assert(manual.prBodyNotes.some(note => note.includes("lock")), "worker Go PR must explain lock work");


// Inline `module@vX.Y.Z` tool pins: Renovate sees only those with a `# renovate:` (or
// `# renovate-tarball:`) annotation on the line directly above. Scope matches the generic
// manager: top-level scripts/*.sh, not scripts/lib/. deadcode sat untracked on
// v0.48.0 for months while upstream fixed the memory blow-up it hit (#4108). Hermetic
// fixtures (*.test.sh) are not pins.
const { readdirSync, existsSync } = await import("node:fs");
const listed = (dir, keep) => readdirSync(path.join(repoRoot, dir)).filter(keep).map((name) => `${dir}/${name}`);
const pinFiles = [
  "Taskfile.yml",
  ...listed("scripts", (name) => name.endsWith(".sh") && !name.endsWith(".test.sh")),
  ...listed(".github/workflows", (name) => /\.ya?ml$/.test(name)),
  ...readdirSync(path.join(repoRoot, "agent/templates")).map((dir) => `agent/templates/${dir}/Dockerfile`)
    .filter((file) => existsSync(path.join(repoRoot, file))),
];
const inlinePin = /[a-z0-9-]+(?:\.[a-z0-9-]+)+\/[A-Za-z0-9._/-]+@v\d+\.\d+\.\d+/;
for (const file of pinFiles) {
  const lines = read(file).split("\n");
  lines.forEach((line, index) => {
    if (!inlinePin.test(line) || /^\s*#/.test(line)) return;
    assert(/#\s*renovate(?:-tarball)?:/.test(lines[index - 1] ?? ""),
      `${file}:${index + 1} pins a tool inline without a renovate annotation above it`);
  });
}
for (const file of pinFiles) {
  const patterns = genericManager.managerFilePatterns.filter((pattern) => matchesPattern(file, pattern));
  if (/renovate:\s*datasource=/.test(read(file))) assert(patterns.length > 0, `${file} carries annotations the generic manager does not read`);
}

const taskfileDeps = extract(genericManager, read("Taskfile.yml"), "Taskfile.yml");
const deadcodeDeps = taskfileDeps.filter((dep) => dep.depName === "deadcode-taskfile");
assertEqual(deadcodeDeps.length, 1, "one deadcode pin for both modules");
assertEqual(deadcodeDeps[0].packageName, "golang.org/x/tools", "deadcode packageName");
assertEqual(deadcodeDeps[0].datasource, "go", "deadcode datasource");
assertEqual((read("Taskfile.yml").match(/cmd\/deadcode@\{\{\.DEADCODE_VERSION\}\}/g) ?? []).length, 4, "deadcode uses the one pin");
const coberturaDeps = taskfileDeps.filter((dep) => dep.depName === "gocover-cobertura-taskfile");
assertEqual(coberturaDeps.length, 1, "one gocover-cobertura pin");
assertEqual(coberturaDeps[0].packageName, "github.com/boumenot/gocover-cobertura", "gocover-cobertura packageName");

// The worker images build gitleaks from source; one depName across the Taskfile and both
// image templates puts every copy in one Renovate PR, so the pins cannot skew.
const gitleaksDeps = [
  ...taskfileDeps,
  ...extract(genericManager, read("agent/templates/base/Dockerfile"), "agent/templates/base/Dockerfile"),
  ...extract(genericManager, read("agent/templates/jvm/Dockerfile"), "agent/templates/jvm/Dockerfile"),
].filter((dep) => dep.depName === "gitleaks/gitleaks");
assertEqual(gitleaksDeps.map((dep) => dep.packageFile).sort().join(","),
  "Taskfile.yml,agent/templates/base/Dockerfile,agent/templates/jvm/Dockerfile", "gitleaks pin files");
assertEqual(new Set(gitleaksDeps.map((dep) => dep.currentValue.replace(/^v/, ""))).size, 1, "gitleaks pins must agree");

console.log("Renovate Go-toolchain contract: PASS");
