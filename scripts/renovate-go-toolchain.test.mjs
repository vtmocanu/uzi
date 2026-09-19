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
  (manager) => manager.datasourceTemplate === "github-release-attachments",
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
  { depName: "go", packageName: "go" },
  { depName: "go", packageName: "actions/go-versions" },
  { depName: "golang", packageName: "golang" },
];
for (const dep of [...goSurfaces, ...linterDeps]) {
  const result = resolved(dep, "minor");
  assertEqual(result.groupName, "Go toolchain", `${dep.depName} minor group`);
  assertEqual(result.separateMajorMinor, false, `${dep.depName} major/minor separation`);
}
for (const dep of linterDeps) {
  assertEqual(resolved(dep, "patch").groupName, "golangci-lint", `${dep.depName} patch group`);
}
assertEqual(resolved(goSurfaces[0], "patch").groupName, undefined, "Go patch grouping");
assertEqual(resolved(goSurfaces[2], "digest").groupName, undefined, "golang digest grouping");

console.log("Renovate Go-toolchain contract: PASS");
