#!/usr/bin/env node
// Deterministic-sync gate for the in-app changelog (PRD #415 M4). Asserts the
// newest RELEASED CHANGELOG.md section names the version that ships (Model B:
// `deploy/chart/Chart.yaml`'s `version` == `appVersion` == the release git tag).
// If someone bumps the chart without adding a matching CHANGELOG section (or vice
// versa), the in-app changelog would advertise a version the release does not
// build — this fails the gate before that lands.
//
// Pure text, NO parser import: like check-docs.mjs this stays dependency-free
// node so it runs identically in the gate and (as a no-op) in the Docker image
// build. Path note: this file lives at web/scripts/, so the repo root is `../..`
// from here — the SAME depth check-docs.mjs uses, and a different depth than the
// viewer's `../../..` in web/src/lib/changelog.ts.
//
// Self-skips outside a full checkout (no repo-root `.git`), so it is a no-op in
// the web image build, whose /app holds only web/ and docs/ (see check-docs.mjs's
// long note on why `fullCheckout` is the right predicate there).
import { readFileSync, existsSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(scriptDir, "..", "..");
const fullCheckout = existsSync(path.join(repoRoot, ".git"));

if (!fullCheckout) {
  console.log("check-changelog: SKIP - not a full checkout (no repo-root .git); no-op in the image build");
  process.exit(0);
}

const die = (msg) => {
  console.error(`ERROR ${msg}`);
  console.error("check-changelog: FAILED");
  process.exit(1);
};

const changelogPath = path.join(repoRoot, "CHANGELOG.md");
const chartPath = path.join(repoRoot, "deploy", "chart", "Chart.yaml");

if (!existsSync(changelogPath)) die(`CHANGELOG.md not found at ${changelogPath}`);
if (!existsSync(chartPath)) die(`Chart.yaml not found at ${chartPath}`);

// Newest RELEASED section: the first `## [<token>]` heading whose token is not
// `Unreleased` and whose line does not carry a `[NOT RELEASED]` marker. Matches
// check-docs/changelog.ts's `^## \[` notion of a section heading.
const changelog = readFileSync(changelogPath, "utf8");
let changelogVersion = null;
let changelogLine = null;
for (const line of changelog.split("\n")) {
  if (!/^## \[/.test(line)) continue;
  if (/\[NOT RELEASED\]/.test(line)) continue;
  const m = /^## \[([^\]]*)\]/.exec(line);
  if (!m) continue;
  const token = m[1].trim();
  if (token === "Unreleased") continue;
  changelogVersion = token;
  changelogLine = line;
  break;
}

if (changelogVersion === null) {
  die("no released `## [X.Y.Z]` section found in CHANGELOG.md (only Unreleased / [NOT RELEASED]?)");
}

// Chart.yaml top-level `version:` (Model B release version).
const chart = readFileSync(chartPath, "utf8");
let chartVersion = null;
let chartLine = null;
for (const line of chart.split("\n")) {
  const m = /^version:\s*(\S+)/.exec(line);
  if (m) {
    chartVersion = m[1].trim().replace(/^["']|["']$/g, "");
    chartLine = line;
    break;
  }
}

if (chartVersion === null) {
  die("no top-level `version:` line found in deploy/chart/Chart.yaml");
}

if (changelogVersion !== chartVersion) {
  console.error(`ERROR changelog/chart version mismatch:`);
  console.error(`  CHANGELOG.md newest released section: ${changelogVersion}  (${changelogLine.trim()})`);
  console.error(`  deploy/chart/Chart.yaml version:      ${chartVersion}  (${chartLine.trim()})`);
  console.error(
    "\ncheck-changelog: FAILED - the newest released CHANGELOG section must match Chart.yaml `version`",
  );
  process.exit(1);
}

console.log(`check-changelog: OK - newest released CHANGELOG section ${changelogVersion} matches Chart.yaml version`);
