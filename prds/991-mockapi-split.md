# PRD #991: mockApi split — per-domain modules composed into one `typeof realApi` object

**GitHub Issue**: [#991](https://github.com/vtmocanu/uzi/issues/991)
**Status**: Draft (created 2026-09-02)
**Priority**: Low (the epic's own ranking: "lowest payoff of the four", last)
**Parent**: epic #915 (Batch 2, P15; finding W7). Prerequisite #960 (P9, `mocks/data.ts` → `mocks/data/`) merged in PR #978. File-disjoint from the two runs in flight: #983 (P11) touches no `web/src/mocks/**` (its D9 records mock content as out of scope), and #982 (P16) confines its TS production edits to `web/src/lib/apiTypes.ts` plus one page annotation and excludes `web/src/mocks/**` from its own sweep. See *Risks* for the one way #982 can still brush this tree.
**Line refs**: at `74d2509` (current main). The implementer re-derives at their base; anchors are identifiers, not offsets.

## Problem

`web/src/mocks/mockApi.ts` is the largest hand-written file in the repo (5001 lines) and the only piece of the mock tree still shaped as one file after #960:

- `:1-127` header + imports (types from `../lib/api`, type-only; runtime values from the leaf modules `../lib/apiError`, `../lib/runStatus`, `../lib/theme`, `../lib/judge`, `../lib/skills`, `../lib/capabilityVocabulary`, and from `./data`, `./engine`, `./store`).
- `:128-1828` module-level state and helpers: **116 declarations**, among them **26 mutable `let` bindings** (`templates`, `users`, `notifications`, `secrets`, `userSettings`, `appSettings`, `workers`, `connections`, `repos`, `schedules`, `skills`, `allocations`, `slackLink`, `toolAllowlist`, `cliTokens`, `memories`, seven counters, and four agent-source/release fields), the judge-backlog reimplementation (`bucketOf` … `computeBacklog`, `:413-779`), the schedule catalog fixtures (`userSchedules`, `scheduleCatalog`, `seededDefaults`, `:1175-1506`), and the settings persistence layer (`:150-305`).
- `:1829-5000` `export const mockApi = { … }`: **187 methods** in one object literal, in roughly domain order (auth → settings → notifications → secrets → agents/skills → forge/repos/project-sync/tool-allowlist → boards → workers → runs → judge → findings → chat → CLI tokens → memory → schedules).
- `:5001` `export { patchRun };` (a re-export of the `./store` import).

Consequences: every mock change diffs against the whole file; the 18 `mockApi.*.test.ts` files each test one domain of a single 5001-line unit; and the tree has two shapes, `mocks/data/` per domain (19 modules since #960) beside one monolithic api. The epic decided the shape on 2026-09-01: *per-domain partial objects spread into one `mockApi` with `typeof realApi` as the guard, after #960's `data/` split so both mock trees share domain names.*

### Measured facts that constrain the split (all at `74d2509`, scripts in the Appendix)

1. **No `this`, one intra-object call.** `grep -nE '\bthis\.'` over the file returns nothing. The only method that calls another through the object is `startRunFromChat`, `return mockApi.createRun(repoId, card.iid);` at `:4443`. The two other `mockApi.` mentions (`:3315`, `:3610`) are comments naming test files.
2. **ESM makes state ownership a hard constraint, not a taste.** An imported binding is read-only: a module that does `schedules = […]` on a binding it imported fails `tsc` (TS2632, "Cannot assign to 'schedules' because it is an import"). Reads through an import are live bindings and work from anywhere, and in-place mutation (`push`, `splice`, `Map.set`, property writes) works from anywhere. So **each `let` must live in the module that holds every method that reassigns it**; everything else is free. The reassignment map, code-only (`^\s+<name> = ` inside a declaration's range):

   | `let` | reassigned by (method) | owner module |
   |---|---|---|
   | `templates` | `deleteAgentTemplate` | `agents` |
   | `skills` | `deleteSkill` | `agents` |
   | `secrets` | `deleteAnthropicTokenById`, `deleteAnthropicToken` | `secrets` |
   | `userSettings` | `putMySettings` (7 sites) | `settings` |
   | `appSettings` | `updateSettings` | `settings` |
   | `slackLink` | `setMySlackNotify`, `setMySlackOverride` | `settings` |
   | `agentSourceRemote` | `updateCheckAgentSource` | `agentSource` |
   | `releaseBannerSnoozeTag` | `snoozeReleaseBanner` | `settings` (see D6) |
   | `workers` | `deleteWorker` | `workers` |
   | `connections` | `createConnection`, `deleteConnection` | `forge` |
   | `repos` | `deleteRepo` | `forge` |
   | `toolAllowlist` | `createToolAllowlistEntry`, `deleteToolAllowlistEntry` | `forge` |
   | `schedules` | `createSchedule`, `updateSchedule`, `deleteSchedule`, `enableCatalogSchedule`, `resetSchedule`, `cloneSchedule`, `addScheduleRepo` | `schedules` |
   | `cliTokens` | `createCliToken`, `revokeAllCliTokens` | `cliTokens` |
   | `memories` | `deleteMemory` | `memory` |
   | `users`, `notifications`, `agentSource`, `allocations`, `nextFiledIssueIid`, `scheduleSeq`, the five counters | **never reassigned** (mutated in place or `++`) | free to place; `users` goes to `shared` (D5) |

   Every reassigning method of one binding already sits in one domain, so the grouping below is forced by the table, not chosen.
3. **The cross-module reference graph for that grouping is a DAG with one exception.** Every module reads `shared` (`delay` 189 call sites, `requireSession` 70, `mockScenario`/`oidcDemo`, the `users` roster). Beyond that the measured edges (function-body reads, all of them): `users → settings` (`sessionBody`, called by `register`/`login`/`me`), `settings → agentSource` (`updateSettings` writes `agentSource.config`), `settings → secrets` (`setJudgeEnabled`, `:2343`), `boards → settings` (`promoteIssue` and `createIssue` read `appSettings`), `runs → agents` (`getRun` builds `own_agents` from `templates`, `:3836`), `findings → forge` (`mockIssueDraft` reads `repos`), `findings → judge` (`fileIssue` reads `reviews`), `schedules → forge` (`repos`), `chat → runs` (the one call in fact 1). **The one cycle: `secrets ↔ workers`** — `deleteAnthropicTokenById` unbinds workers pinned to the deleted token (`workers.forEach(…)`, `:2590`) and `setWorkerBindMode` resolves a token label against `secrets` (`:3535`). Both are inside function bodies. **No module-level initializer reads a sibling module's binding**: the initializers that read state at import time (`pendingJudges` ← `mockPendingJudges` from `data` (`:356`), `allocations` ← `mockAllocations` from `data` (`:1514`), `templateGlobalDefaults` ← `templates`, `schedules` ← `seededDefaults`/`userSchedules`, `loadedSettings` ← `localStorage`) all read same-module or `data`/`store` bindings, so the cycle cannot hit a TDZ. The Appendix script over-approximates (it counts identifier collisions such as a local `const me`, the `./store` `getRun`, object keys like `secrets:`); the list above is the code-level truth after reading each hit.
4. **The typecheck guard and its one blind spot.** `web/src/lib/api.ts:1390` is `export const api: typeof realApi = MOCK_MODE ? mockApi : realApi;` (`realApi` is an unexported object literal at `:322`). Assigning the composed object there checks that every `realApi` key exists on `mockApi` with a compatible signature. It does **not** flag an extra key (the conditional expression is not a fresh literal, so no excess-property check — same as today) and it **cannot** see a key contributed by two spread partials (`{ ...a, ...b }` with a shared key is legal TS; the later silently wins). The second is the one hole spreading opens, and M1/M3 close it with a runtime test. Under vitest `MOCK_MODE` is off by default (no `web/.env*` exists and `vite.config.ts` sets no `VITE_UZI_MOCK`; three tests mention the variable — `mocks/data.test.ts:201` and `mocks/mockApi.lanes.test.ts:6` in comments, and `App.routes.test.tsx:60` flips it on with `vi.stubEnv` inside its own `beforeAll`, file-local), so a test that imports `api` from `../lib/api` gets **`realApi`** — and that one file-local flip is exactly why M1's non-vacuity assertion exists — — which is what makes a runtime key-set comparison possible from a `.test.ts` file (the api-acyclic guard skips test files, and page tests already import `api` from the barrel at runtime, e.g. `pages/AdminSettings.test.tsx:6`, `pages/Settings.test.tsx:13`; the two `mocks/*.test.ts` files that name the barrel import only types).
5. **Importers are few and all go through one specifier.** Nine files import from `./mockApi` / `../mocks/mockApi` (measured with `git grep -n -E "from ['\"](\.\./)*mocks/mockApi['\"]|from ['\"]\./mockApi['\"]" -- web/src`): `lib/api.ts:10` (`mockApi`), `components/ScheduleModal.test.tsx:17` (`mockApi as realMockApi`), `lib/agentTemplateDriftContract.test.ts:4` (`sameContent`), `mocks/judgeBacklogFidelity.test.ts:3-8` (`bucketOf`, `filterGroups`, `groupJudgeRecommendations`, `type JudgeBacklogRow`), `mocks/judgeBacklogTruncation.test.ts:3` (`capBacklogRows`, `mockApi`, `MOCK_BACKLOG_MAX_ROWS`, `type JudgeBacklogRow`), `mocks/mockApi.limitWait.test.ts:4`, `mocks/mockApi.oidc.test.ts:3`, `mocks/mockApi.poolWait.test.ts:4`, `mocks/scheduleCatalog.test.ts:9` (`mockApi`). `mocks/mockApi.test.ts` reaches it dynamically: `vi.resetModules(); (await import("./mockApi")).mockApi` (`:28-33`), which re-evaluates the whole graph — a directory index keeps that behaviour byte-for-byte. The other ~80 files that `git grep -l mockApi` lists use `mockApi` as a local identifier (`vi.hoisted` fakes) and never import the module. `patchRun` re-exported at `:5001` has **no importer** outside `store.ts`/`engine.ts`; it stays as-is (motion).
6. **The gates already reach into a subdirectory.** `mocks/api-acyclic.test.ts` walks `src/mocks/` recursively and resolves every specifier against the importing file's directory (#960 D4), so `mockApi/*.ts` files importing types via `../../lib/api` are guarded with **no test edit**. `web/knip.jsonc` gates `exports`/`types` at `error` with `ignoreExportsUsedInFile` for types only, so every value a domain module exports must have an importer (the index, a sibling, or a test). `tsconfig.json` has `moduleResolution: bundler`, the same resolution that already serves `./data` → `data/index.ts`.
7. **Nothing depends on key order.** `git grep -n -E 'Object\.keys\((api|mockApi|realMockApi)\)' -- web/src` returns nothing, and no test spies on `createRun` while exercising `startRunFromChat` (`git grep -n -E 'spyOn\((api|mockApi|realMockApi), "createRun"' -- web/src` returns nothing), which is what makes D7 behaviour-preserving.

## Solution

Replace the file with a directory, the `data.ts` → `data/` recipe from #960 M3:

```
web/src/mocks/mockApi/
  index.ts          header (verbatim) + composition + the re-exports the 9 importers need
  shared.ts         jitter, delay, requireSession, mockScenario, OidcDemo, oidcDemo, users
  users.ts          register, login, authConfig, version, logout, me, listUsers, setUserActive
  settings.ts       settings persistence (:150-305), userSettings/appSettings, brandingAssets,
                    slackSecrets, slackLink, slackLinkResponse, settingsResponse, sessionBody,
                    release-check facts/status (:865-934), and 24 methods: getSettings,
                    vaultMigration, getReleaseCheck, checkReleaseNow, snoozeReleaseBanner,
                    updateSettings, the 8 bool setters, getMySettings, putMySettings, getMySlack,
                    setMySlackNotify, setMySlackOverride, testMySlackDM, getSlackStatus,
                    branding, uploadBrandingLogo, deleteBrandingLogo
  agentSource.ts    agentSource, agentSourceRemote, the semver/status helpers (:935-1006),
                    getAgentSource, syncAgentSource, applyAgentSource, resolveAgentSourceLatest,
                    updateCheckAgentSource
  notifications.ts  notifications, notifDTO, listNotifications, unreadNotificationCount,
                    runsInProgressCount, markNotificationRead
  secrets.ts        secrets, requireUnlockedVault, pooledFixtureStatus, rejectInvisibleLabel,
                    listSecrets … deleteAnthropicToken, vaultUnlock/CreatePassphrase/Lock/Status
  agents.ts         templates, skills, allocations, counters, the template/skill helpers
                    (:1658-1748 incl. sameContent), listAgentTemplates … setTemplateSkills (17)
  forge.ts          connections, repos, githubProjectLinks/Visibility, toolAllowlist,
                    repoToolProfiles, forgeConfig … setRepoToolProfile (36) + adminListBlockedRepos
  boards.ts         boardResponse, getBoard … setBoardPrefs (10)
  workers.ts        workers, workerCounter, workerUpgradeSummary, listWorkers … provisionHostedWorker,
                    adminListWorkers
  runs.ts           listRunsFor, createRun, createCIFixRun, listRuns, getUsage, getAdminUsage,
                    getMyRateLimits, getAdminRateLimits, getRun, setRunWaitOnLimit, setRunMrRework,
                    resumeRunNow, expediteRun, getRunMessages, getRunInputs, submitRunInput,
                    adminListRuns
  judge.ts          reviews, pendingJudges, the backlog reimplementation (:347-779 minus the
                    findings decls), getRunReview … rerunJudge (8)
  findings.ts       findings, findingDTO, matchFindingBucket, nextFiledIssueIid, mockIssueDraft,
                    listFindings, findingIssueDraft, fileFinding, dismissFinding, getIssueDraft,
                    fileIssue
  chat.ts           listChats … steerRunFromChat (10)
  cliTokens.ts      cliTokens, cliAuthRequests, CROCKFORD32, normalizeMockUserCode, stripOwner,
                    listCliTokens … denyCliAuth (7)
  memory.ts         memories, stripMemoryOwner, listMemory, deleteMemory
  schedules.ts      schedule helpers + catalog fixtures (:1085-1512), listSchedules … ensureRepoLabels (14)
```

Approximate sizes from the declaration ranges at `74d2509`, methods + their helpers: `schedules` ≈ 840, `settings` ≈ 750, `judge` ≈ 640, `forge` ≈ 420, `agents` ≈ 360, `runs` ≈ 350, `agentSource` ≈ 230, `secrets` ≈ 225, `findings` ≈ 210, `boards` ≈ 190, `chat` ≈ 180, `workers` ≈ 175, `cliTokens` ≈ 120, `notifications` ≈ 65, `users` ≈ 60, `shared` ≈ 60, `memory` ≈ 25, plus each file's import block. **Every module lands under ~900 lines**, and none needs an internal seam. Module names mirror `mocks/data/` where a counterpart exists (`agents`, `boards`, `chat`, `cliTokens`, `findings`, `forge`, `judge`, `memory`, `notifications`, `runs`, `secrets`, `users`, `workers`); `settings`, `agentSource`, `schedules`, `shared` have no data twin. The implementer may regroup, subject to fact 2 (ownership), fact 3 (no top-level read of a sibling binding), and the size cap.

**Mechanics of one domain module.** Each exports one partial object named `<domain>Api` whose members are the method text moved verbatim, plus the state and helpers only it (and its importers) use:

```ts
// web/src/mocks/mockApi/memory.ts
import type { Memory } from "../../lib/api";
import { mockMemories } from "../data";
import { delay, requireSession } from "./shared";

type OwnedMemory = Memory & { user_id: string };
let memories: OwnedMemory[] = mockMemories.map((m) => ({ ...m }));
const stripMemoryOwner = ({ user_id: _user_id, ...m }: OwnedMemory): Memory => m;

export const memoryApi = {
  listMemory: async () => { … verbatim … },
  deleteMemory: async (id: string) => { … verbatim; `memories = memories.filter(…)` stays here … },
};
```

and `index.ts` composes them:

```ts
// (the four-line header from mockApi.ts:1-4, verbatim, then a short note that the
//  surface is composed per domain and that lib/api.ts's `api: typeof realApi` is the
//  completeness guard while mockApi.parity.test.ts is the duplicate-key guard)
import { usersApi } from "./users";
…
export const mockApi = {
  ...usersApi,
  ...settingsApi,
  … (one spread per module, in the file's original domain order)
};
export { bucketOf, filterGroups, groupJudgeRecommendations, capBacklogRows, MOCK_BACKLOG_MAX_ROWS, type JudgeBacklogRow } from "./judge";
export { sameContent } from "./agents";
export { patchRun } from "../store";
```

A helper a sibling needs (`sessionBody` for `users`, `settingsResponse` if anything outside `settings` calls it, `rejectInvisibleLabel` if its callers end up split) gains `export` — that is scaffolding, and knip holds every such export to "has an importer". The method bodies are untouched, with **one** exception, D7: `mockApi.createRun(repoId, card.iid)` at `:4443` becomes `runsApi.createRun(repoId, card.iid)` with `import { runsApi } from "./runs"` in `chat.ts`, because the composed object does not exist from inside a partial.

## Milestones

- [ ] **M1 — Guard first (characterization before motion): `web/src/mocks/mockApi.parity.test.ts`.** On the *unsplit* file, add a runtime key-set parity test: `import { api } from "../lib/api"` (which is `realApi` under vitest, fact 4) and `import { mockApi } from "./mockApi"`, assert `expect(api).not.toBe(mockApi)` first with a message saying the test is vacuous under `VITE_UZI_MOCK=1` (so a future env change cannot green it silently), then `expect(Object.keys(mockApi).sort()).toEqual(Object.keys(api).sort())`. This is strictly stronger than the `typeof` guard on one axis (it sees an extra key) and the runtime half of the duplicate-key guard M3 completes. **Positive controls, recorded red in the PR before the assertion is trusted:** (a) a temporary stray method added to `mockApi` reddens the test naming the key while `tsc` stays green — the case `typeof` cannot see; (b) a temporary deletion of one method reddens both `tsc` (the `typeof` guard, proving it still fires) and the test. Both reverted in the same commit. `task gate:web` green.
- [ ] **M2 — Scaffold, a pure rename: `mockApi.ts` → `mockApi/index.ts`, plus the `shared.ts` leaf.** `git mv web/src/mocks/mockApi.ts web/src/mocks/mockApi/index.ts`; inside the moved file rewrite only its own relative specifiers (`../lib/*` → `../../lib/*`; `./data`, `./engine`, `./store` → `../data`, `../engine`, `../store`), which is the whole diff git's rename detection sees. Then move `jitter`, `delay`, `requireSession`, `mockScenario`, `OidcDemo`, `oidcDemo` and the `users` roster (D5) verbatim into `mockApi/shared.ts`, exported, and import them back into `index.ts`. Every one of the nine importers and `mockApi.test.ts`'s dynamic import resolve to the directory index unchanged; `api-acyclic.test.ts` now lists `mockApi/index.ts` and `mockApi/shared.ts` in its walk (its floor of `> 5` files is unaffected). Verification: `task gate:web` green; `cd web && VITE_UZI_MOCK=1 npm run build` green (bundler-level resolution of the directory index; `task gate:web` deliberately excludes `vite build`, `.claude/rules/web.md:22`); `git diff --stat` touches exactly the two new files and nothing else under `web/src`.
- [ ] **M3 — Domain extraction, one commit per module, dependency order.** Extract the sixteen domain modules from `index.ts` leaf-first so every intermediate commit typechecks with no forward reference: `memory`, `cliTokens`, `notifications`, `judge`, `agents`, `forge` (which `findings` and `schedules` read), `findings`, `runs`, `chat`, `boards`, `workers`, `secrets`, `agentSource`, `settings`, `users`, `schedules` — the implementer re-derives the order from the graph in fact 3 at their base and may batch modules that share no edge into one commit, but every commit must be `task gate:web` green and show as pure motion under `git diff --color-moved=dimmed-zebra`. Each commit: (1) the module's `let`s, helpers and constants move verbatim (their reassigning methods travel with them, fact 2 — `tsc` refuses anything else with TS2632, which is the enforcement); (2) its methods move verbatim into `export const <domain>Api = { … }`; (3) `index.ts` replaces those members with one `...<domain>Api` spread at the same position; (4) an `import type { … } from "../../lib/api"` block and the leaf-module imports the moved code needs (let `tsc` + `noUnusedLocals` produce the exact list; no `import * as`). Comments that name a former line position or "above/below" in the moved text are checked for positional rot the way #963 did (`git grep -n -E 'above|below|earlier in this file' -- web/src/mocks/mockApi/`). The **only** non-scaffolding text edit in the whole milestone is D7 (`chat.ts`'s `runsApi.createRun`). Finish by extending `mockApi.parity.test.ts` with the duplicate-key half: enumerate every module via `import.meta.glob("./mockApi/*.ts", { eager: true })` (the eager-glob-under-vitest pattern `lib/sourceBytes.test.ts:28` already uses), collect each export whose name ends in `Api`, assert no key appears in two partials (`expect(duplicates).toEqual([])`, so a failure names the key) and that the union of their keys equals `Object.keys(mockApi)` (so a partial that is not spread, or is mis-named, fails too). **Positive control, recorded red:** the same method temporarily present in two partials reddens the test while `tsc` stays green (fact 4's hole, now closed). Final state: `index.ts` is the header, the imports, the composed literal and the three re-export lines, ≈ 60 lines.
- [ ] **M4 — Doc-sync and the proofs.** Present-tense claims about where the code lives (fix-the-doc; a past-tense record is left alone): `docs/dev-conventions.md:596` ("swap in `src/mocks/mockApi.ts`" → `src/mocks/mockApi/`), `:604` ("`src/mocks/mockApi.ts`'s `mockScenario()`" → `src/mocks/mockApi/shared.ts`), `:627` stays true (the `api: typeof realApi` guard is still in `lib/api.ts`) — verify, do not edit; `.claude/rules/web.md:24` (`src/mocks/mockApi.ts` → `src/mocks/mockApi/`); `specs/ai.md:10341` ("`mockApi.ts` hardcodes `truncated: false`" → `mockApi/judge.ts`), `:11345` ("`web/src/mocks/mockApi.ts` is not a stub: it reimplements the judge backlog's …" → `web/src/mocks/mockApi/judge.ts`), `:21218` ("`mockApi.ts` (the demo backend) persists `Omit<SlackLink…>`" → `mockApi/settings.ts`, where `slackLink` lives), `:22285` ("The mock copies in `web/src/mocks/mockApi.ts` are kept in lockstep" → `web/src/mocks/mockApi/schedules.ts`). Six source comments in `web/` name the old path in the present tense and are repointed as comment-only edits: `lib/apiError.ts:2` and `lib/runStatus.ts:2` ("the mock-mode client `mocks/mockApi.ts` can import it as a runtime value" → `mocks/mockApi/`, the directory), `lib/demoMode.ts:4` ("the `uzi_mock_scenario` convention in `web/src/mocks/mockApi.ts`" → `web/src/mocks/mockApi/shared.ts`), `lib/capabilityVocabulary.ts:2` ("the mock (`mockApi.ts`)" → `mockApi/forge.ts`, the `setRepoRequiredCapabilities` site at `:3129`), `mocks/data/workers.ts:103` ("see the note in `mockApi.ts`" → `mockApi/workers.ts`: the note is the "exactly ONE slot of headroom" comment at `:3555`, in the `hostedConfig`/`provisionHostedWorker` block), and `lib/agentTemplateDriftContract.test.ts:16` ("`mockApi.ts`'s `sameContent`" → `mockApi/agents.ts`'s, the one comment-only edit in a test file, carved out in SC3). Leave `specs/ai.md:6236` and `:18473`: both are decision records about the commit that made the change (`:6236` still names `mocks/data.ts`, which #960 split away under the same frozen-record rule), and `:18473`'s "the now-`export`ed `sameContent` in `web/src/mocks/mockApi.ts`" stays materially true because `sameContent` is still exported from `mocks/mockApi` through the index re-export. Leave every mention of `mockApi` as an object, a method, or a test file. The sweep that finds these is the bare `git grep -n -F 'mockApi.ts' -- CLAUDE.md ARCHITECTURE.md docs .claude specs web ':!web/src/mocks/mockApi.ts'` (SC6); a grep on `mocks/mockApi.ts` misses the bare-name sites and was the instrument the first draft of this milestone used, which is how `:21218` and the six comments went unlisted. **`docs/*.md` is mirrored into `api/internal/uzidocs/embed/` and `TestEmbeddedDocsMatchSource` (`api/internal/uzidocs/uzidocs_test.go`, part of `test:api`) reddens on an unsynced edit that `task gate:web` cannot see: after editing `docs/dev-conventions.md` run `task docs:sync`, commit the regenerated `api/internal/uzidocs/embed/dev-conventions.md` in the same commit, and run `cd api && go test ./internal/uzidocs/ -run TestEmbeddedDocsMatchSource`** (the one `api/` check this PRD needs; the #960 M3 precedent). Then the PR description carries the measurements in *Success criteria* 2, 5 and 6.

## Success criteria

1. **Importers untouched.** `git diff --stat origin/main..HEAD -- web/src/lib/api.ts web/src/components/ScheduleModal.test.tsx web/src/lib/agentTemplateDriftContract.test.ts web/src/mocks/judgeBacklogFidelity.test.ts web/src/mocks/judgeBacklogTruncation.test.ts web/src/mocks/mockApi.limitWait.test.ts web/src/mocks/mockApi.oidc.test.ts web/src/mocks/mockApi.poolWait.test.ts web/src/mocks/scheduleCatalog.test.ts web/src/mocks/mockApi.test.ts` is empty.
2. **Pure motion.** For every M2/M3 commit, `git diff --color-moved=dimmed-zebra <parent>..<commit>` shows the moved blocks, and the non-moved residue is exactly: the specifier rewrites in M2, the per-module import blocks, the `export` keyword on the partials and on helpers a sibling imports, the `...<domain>Api` spreads and re-export lines in `index.ts`, the header note in `index.ts`, and D7's one-line call-site change. The PR description enumerates that residue, the #955/#960 precedent. `git diff --stat origin/main..HEAD -- web/src` lists only `web/src/mocks/mockApi.ts` (deleted), `web/src/mocks/mockApi/*.ts`, `web/src/mocks/mockApi.parity.test.ts`, and the six comment-only repoints M4 names (`lib/apiError.ts`, `lib/runStatus.ts`, `lib/demoMode.ts`, `lib/capabilityVocabulary.ts`, `mocks/data/workers.ts`, `lib/agentTemplateDriftContract.test.ts`), each a one-line comment change.
3. **Gate.** `task gate:web` green after every commit; the only `*.test.*` file with a non-comment change in the whole diff is the new `web/src/mocks/mockApi.parity.test.ts` (the one other test file touched, `lib/agentTemplateDriftContract.test.ts`, carries a single comment-line repoint from M4 and nothing else), and the PR records the parity test's three positive controls (M1 a, M1 b, M3) going red. `api-acyclic.test.ts` passes unmodified with the new files in its walk.
4. **Bundle.** `cd web && VITE_UZI_MOCK=1 npm run build` succeeds locally after M2 and after M3, and `build:web-mock` is green on the PR.
5. **Shape.** `Object.keys(mockApi).length` is 187 before and after (the parity test pins it to `realApi`, whatever the number is at the base); `index.ts` ≤ ~80 lines; every `mockApi/*.ts` ≤ ~900 lines; no `let` declared in one module is assigned in another (enforced by `tsc`, re-stated so the reviewer looks for it in the diff rather than trusting the gate blindly); the one import cycle is `secrets ↔ workers` and no other, re-measured with the Appendix script at the final tree.
6. **Docs.** Every site in M4 is repointed or verified still true, and the bare-name sweep `git grep -n -F 'mockApi.ts' -- CLAUDE.md ARCHITECTURE.md docs .claude specs web ':!web/src/mocks/mockApi.ts'` returns only test-file names (`mockApi.test.ts`, `mockApi.*.test.ts`, `mockApi.parity.test.ts`) and the two past-tense decision records (`specs/ai.md:6236`, `:18473`) — read every hit. The pattern is deliberately the bare `mockApi.ts`, not `mocks/mockApi.ts`: the prefixed form cannot see `specs/ai.md:10341`/`:21218` or `lib/capabilityVocabulary.ts:2`, and it does not match `mockApi/` paths, so it is both wider and still precise.
7. No `.github/workflows/**` in the branch diff (implementation or validation).

## Decision Log

- **D1 — a directory with an `index.ts`, not a `mocks/api/` sibling of a kept `mockApi.ts`.** Mirrors `data/` (#960 D3) so both halves of the mock tree have the same shape, keeps all nine importers and the dynamic `import("./mockApi")` byte-identical, and avoids a `mocks/api/` path that reads as `lib/api`'s twin in every relative specifier and in the acyclic guard's output.
- **D2 — spread partials, and a runtime test for what spreading cannot typecheck.** `{ ...a, ...b }` is the epic's decided shape and keeps `api: typeof realApi` as the completeness guard with no change to `lib/api.ts`. `Object.assign({}, ...parts)` was rejected: its rest overload returns `any` and would silently disable that guard. A `satisfies Pick<typeof realApi, …>` per partial was rejected: `realApi` is unexported, and a hand-kept key list per module is a second surface to drift. The duplicate-key blind spot (fact 4) is closed by `mockApi.parity.test.ts`, discovered by directory glob so a new module cannot dodge it, exactly the argument #960 D4 made for the acyclic guard.
- **D3 — state stays with its reassigners; no `state` container.** Folding the 26 `let`s into `store.ts`'s `MockState` (which already holds `session`, `vaultUnlocked`, runs) would rewrite every reference and stop being motion. It is a reasonable *later* PRD; recorded here so nobody re-derives it as a missed option.
- **D4 — the `secrets ↔ workers` cycle is accepted, not engineered away.** It is a real bidirectional relation in the product (a token pins workers; a bind resolves a token), both reads are inside function bodies, and no top-level initializer crosses a module (fact 3), so it cannot throw at import. Merging the two domains, or re-homing `setWorkerBindMode` beside the token code, would trade an honest edge for a misleading file. If a future top-level read ever crosses it, the failure is a `ReferenceError` at import that reddens every mock-mode test at once — loud, not silent.
- **D5 — `users` and the session helpers live in `shared.ts`.** `users` is never reassigned and is read by five domains; keeping it in `users.ts` makes `settings → users → settings` a cycle through `sessionBody`. `requireSession`/`delay`/`mockScenario`/`oidcDemo` are the leaf everything calls.
- **D6 — the release-check helpers and methods go with `settings`, the agent-source ones with `agentSource`.** `releaseCheckStatus` reads `appSettings` (`:891-892`) and nothing else in the `:847-1006` block does, so this placement is what makes `settings → agentSource` one-directional.
- **D7 — the one edited line.** `startRunFromChat`'s `mockApi.createRun(…)` becomes `runsApi.createRun(…)` (`chat.ts` imports `runsApi`). Importing `mockApi` back from `./index` inside a partial would create a cycle through the aggregator purely to preserve a spy-through-the-object semantics that no test uses (fact 7). Recorded as the PRD's single behaviour-adjacent edit so a reviewer finds it by name.
- **D8 — labels: `PRD` + `uzi`, deliberately not `refactor`.** Hand-driven in Auto mode with `--mr-rework=false` today (the #949/#950/#982 shape), so tonight's refactor sweep must not be able to double-fire on it; the `refactor` label is the sweep's selector and stays off.
- **D9 — `Object.keys` order changes and nothing may depend on it.** The composed object's key order follows spread order, not the original literal's exact member order within a domain; fact 7 measured zero consumers, and the parity test compares sorted sets on purpose.

## Risks & mitigations

- **#982 (P16) lands first and adds `Run` fields as required.** Its PRD says TS production edits are confined to `apiTypes.ts` and the drifted fields are unread anywhere in `web/src`, which only holds if they are added optional; a required addition would redden `mocks/data/runs.ts` and the `Run` literals `createRun`/`createCIFixRun` build in what becomes `mockApi/runs.ts`. Mitigation: those literals move verbatim here, so the collision is a mechanical rebase of moved lines; the coder rebases onto `main` before opening the MR and re-runs the gate. #983 cannot collide (no `mocks/**` in its scope).
- **A future top-level read across modules (TDZ).** Loud by construction (D4). The `index.ts` spread order and the M3 commit order both follow the dependency order anyway, and the Appendix script is the re-check.
- **knip on a helper exported for a sibling that later loses its importer.** That is `task deadcode:web` doing its job; drop the `export`, never suppress.
- **`vi.resetModules()` reload semantics (`mockApi.test.ts`).** Unchanged: every module under `mockApi/` is re-evaluated on the dynamic import, so `loadSettings()` re-runs at the top of `settings.ts` exactly as it did at the top of the old file. If the persistence tests go red in M3, the cause is a `settings` initializer that moved out of module scope, not the reload.
- **`import.meta.glob` in the parity test** needs vitest/vite (not plain node); the suite already runs under vitest and `lib/sourceBytes.test.ts:28` (an eager `import.meta.glob` over `../**`) is the precedent. If the eager glob resolves lazily on some vitest version, switch to the `node:fs` walk `api-acyclic.test.ts` uses plus dynamic imports — never to a hand-listed module array.
- **Offline worker.** Nothing here needs the open internet; every fact is in-repo and re-derivable with the Appendix scripts. The one `api/` command (`go test ./internal/uzidocs/`) is module-local.
- **In-flight overlap: none by construction** (header). The four doc files this PRD edits (`docs/dev-conventions.md` + its embed, `.claude/rules/web.md`, `specs/ai.md`) are not touched by #983 or #982.

## Appendix: how the constraints were measured (re-runnable offline)

**Declaration table** (`decls.txt`: `line kind name`, `H` = module-level helper/state before the object literal, `M` = method inside it):

```sh
F=web/src/mocks/mockApi.ts
awk 'NR>=128 && NR<1829 && /^(export )?(const|let|function|async function|type|interface) /{s=$0; sub(/^export /,"",s); sub(/^async /,"",s); split(s,a,/[ (:=<]/); printf "%d H %s\n", NR, a[2]}
     NR>=1829 && /^  [a-zA-Z_][A-Za-z0-9_]*(: async|: \(|\(|: [a-zA-Z]+ =>|: async \()/{s=$0; sub(/^  /,"",s); sub(/[:(].*$/,"",s); printf "%d M %s\n", NR, s}' $F > decls.txt
```

**Reassignment ownership** (fact 2): for every `^let <name>` before `:1829`, the declarations whose range contains a `^\s+<name> = ` line:

```sh
for v in $(awk 'NR<1829 && /^let /{sub(/^let /,""); split($0,a,/[ :=]/); print a[1]}' $F); do
  printf '%-24s' "$v"; grep -nE "^\s+$v = " $F | cut -d: -f1 | while read -r ln; do
    awk -v ln="$ln" '$1<=ln{d=$3} END{printf "%s ", d}' decls.txt; done; echo; done
```

**Cross-module reference graph** (fact 3) for a grouping file `map.txt` (`module name name …`, one line per module): count, per declaration range, identifiers that belong to another module. Read every reported edge before believing it — identifier collisions (`me`, `getRun`, `version`, object keys like `secrets:`) inflate the count, and a top-level initializer is what matters for TDZ, so also read the initializers of any `const`/`let` whose range reports an edge:

```sh
awk -v F=$F 'BEGIN{while((getline l < F)>0) src[++N]=l}
  FNR==NR{for(i=2;i<=NF;i++) mod[$i]=$1; next}
  {ln[NR]=$1; kind[NR]=$2; nm[NR]=$3; n=NR}
  END{for(i=1;i<=n;i++){ end=(i<n)?ln[i+1]:5001; if(kind[i]=="H"&&(i==n||kind[i+1]=="M")) end=1829; A=mod[nm[i]]; if(A=="") continue;
        for(l=ln[i]; l<end; l++){ line=src[l]; sub(/\/\/.*$/,"",line);
          for(name in mod){ B=mod[name]; if(B==A) continue;
            if(line ~ ("(^|[^A-Za-z0-9_.])" name "([^A-Za-z0-9_]|$)")) { k=A" -> "B; cnt[k]++; if(!(k in ex)) ex[k]=name"@"l } } } }
      for(k in cnt) printf "%-28s %3d  e.g. %s\n", k, cnt[k], ex[k]}' map.txt decls.txt | sort
```

**Size per module**: sum each declaration's span (next declaration's line minus its own; helpers end at `:1829`, the last method at `:5001`) by the module `map.txt` assigns it.
