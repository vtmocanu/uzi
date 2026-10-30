#!/usr/bin/env node
// Hermetic contract for the weekly Renovate window and the oasdiff / npm lint-tooling groups.
import { readFileSync } from "node:fs";
import assert from "node:assert/strict";
const read = (name) => readFileSync(new URL(`../${name}`, import.meta.url), "utf8");
const config = JSON.parse(read("renovate.json"));

// One weekly window: branch creation, updates, automerge and lock maintenance all wait for it.
const weekly = JSON.stringify(["* 0-6 * * 6"]);
assert.equal(config.timezone, "Europe/Bucharest");
assert.equal(JSON.stringify(config.schedule), weekly, "Renovate runs one night a week");
assert.equal(JSON.stringify(config.automergeSchedule), weekly, "automerge stays inside the weekly window");
assert.equal(config.updateNotScheduled, false, "no branch updates outside the window");
assert.equal(JSON.stringify(config.lockFileMaintenance.schedule), weekly,
  "lock maintenance has its own default schedule, so it must name the window");
for (const rule of config.packageRules) {
  if (!rule.schedule) continue;
  // Any narrower rule must fall inside the weekly window (Saturday, hours 0-6).
  for (const cron of rule.schedule) {
    const [, hours, , , dow] = cron.split(" ");
    assert.equal(dow, "6", `${rule.description.slice(0, 40)}: schedule must stay on Saturday`);
    assert.match(hours, /^0-[0-6]$/, `${rule.description.slice(0, 40)}: schedule must stay in the night window`);
  }
}

// Mirrors Renovate's packageRules order: later rules override groupName.
const matches = (rule, dep) =>
  (!rule.matchPackageNames || rule.matchPackageNames.includes(dep.packageName)) &&
  (!rule.matchDepNames || rule.matchDepNames.includes(dep.depName)) &&
  (!rule.matchManagers || rule.matchManagers.includes(dep.manager)) &&
  (!rule.matchDatasources || rule.matchDatasources.includes(dep.datasource)) &&
  (!rule.matchUpdateTypes || rule.matchUpdateTypes.includes(dep.updateType));
const groupOf = (dep) => config.packageRules.filter((r) => matches(r, dep) && "groupName" in r)
  .reduce((_, r) => r.groupName, undefined);

// oasdiff: every annotated pin, Taskfile and the three wrapper archives, must share one group.
const pins = [
  ...read("Taskfile.yml").matchAll(/renovate: datasource=\S+ depName=(oasdiff-\S+) packageName=(\S+)/g),
  ...read("scripts/oasdiff.sh").matchAll(/renovate-tarball: depName=(oasdiff-\S+) packageName=(\S+)/g),
].map((m) => ({ depName: m[1], packageName: m[2] }));
assert.deepEqual(pins.map((p) => p.depName).sort(),
  ["oasdiff-darwin-all", "oasdiff-linux-amd64", "oasdiff-linux-arm64", "oasdiff-taskfile"]);
for (const pin of pins) {
  for (const updateType of ["patch", "minor", "major"]) {
    assert.equal(groupOf({ ...pin, updateType }), "oasdiff", `${pin.depName} ${updateType}`);
  }
}

// knip and oxlint share agent/web package.json and lockfiles.
for (const name of ["knip", "oxlint"]) {
  for (const updateType of ["patch", "minor", "major"]) {
    assert.equal(groupOf({ depName: name, packageName: name, manager: "npm", updateType }),
      "npm lint tooling", `${name} ${updateType}`);
  }
}

console.log("Renovate schedule and groups contract: PASS");
