# M4 owner surfaces — validation record

## Current acceptance — 2026-10-15

M4 is **complete** on integrated source `617f6f7d17561da249826403a8f974af42500a05`, with the evidence-only reconciliation following it. M1's approved merge-target/LiveDB deviations remain recorded in [its reconciliation record](worker-memory-m1-reconciliation.md): this branch merges default 4103fad7 and lacks #3039, which the lander must integrate before landing. Production defaults remain disabled and hosted sizing remains uncalibrated. M5a and M5b are unverified at this checkpoint; no command-UID/cgroup calibration or post-publication CI timing is claimed here.

The remainder of this file after “Historical partial record” preserves earlier source units, failed checks, repairs and completion-time statements at their named revisions. Those statements do not override this acceptance section.

### Delivered contract and configuration

Documentation unit `0014b37e9f605a7fb661a2c9bef23ae7866ec89c` creates ADR-2396 and links it from ARCHITECTURE.md. It records exact Codex enrollment and victim exclusions; frozen episode allowance and reservation identity/idempotency; uncertain cancellation/no refund; captured settlement and retained-source custody after authority retirement; bounded retry/owner-only release; operational disable versus destructive migration 00339 Down; long-command deadline/abort interaction; and Claude deferral to #2471. Both rollout orders are explicit: API first then enable, or worker first while disabled followed by fresh enabled registration/restart. Unsupported enabled startup refuses with UnsupportedMemoryAPI; heartbeat growth alone cannot create authority.

The approved bounded custody-attention extension is reflected in ADR-2445, ADR-1742's 2026-10-08 amendment, ARCHITECTURE.md and specs/human.md: after discarded/released precedence, an OPEN recovery_wait hold with worker_requeue_exhausted or worker_memory_pressure takes owner attention before archive readiness/capture progress. Other decisions remain intact. The human spec is AI-synced 2026-10-15. Root operator docs and their three embedded copies match; the existing changelog entry is updated without duplication. No PRD exists.

The seven-setting table matches worker/controller/chart parsing; six positive numeric settings remain explicit, with no production default. The literal sizing input remains `max(1Gi, 10-15% of memory.max)`, not a recommendation. Existing agent configuration tests were included in M1's green full agent gate. Controller unit `baaa07f99bf48e0e07e6a0e7c88cf25bd78579f5` extends the existing memory-specific test to persistent and ephemeral render paths, checking exact enabled/disabled environments and preserving all spec-hash assertions. The focused race-enabled check passed before commit.

### Fresh visual and interaction evidence

The lead generated fresh ANSI frames from the shipped tuiModel with TestGenerateUXLabFrames, then rendered known/missing allowance scenes in dark/light themes using pinned freeze v0.2.2 and the repository render flags. All four PNG mtimes were newer than their frames. Lead and tui-ux inspected them: status, zero versus unknown allowance, complete owner commands, terminal Cancel, no automatic Resume and source uncertainty were readable. The reviewer measured at most 99 visual columns with a single-line footer. No production TUI code changed in this unit.

Actual Chromium 155 reviewed mock-mode run pages at 1360px and 390px. Initial five captures covered known, unknown, source-only, non-owner and mobile states. The follow-up used the bundled, loaded Plex fonts in Ember/Hall themes, captured seven accessibility snapshots and verified Resume → Tab → Cancel for owner states. Keyboard Resume returned queued with no memory hold; keyboard Cancel returned failed/cancelled with no memory hold. Non-owner Resume/Cancel controls were absent. No tested document exceeded viewport width. Lead inspected the captures; web-ux independently reran the seven-scenario driver successfully. Screenshots precede actions; result JSON records the outcomes.

This was a real browser over mocks, not live-server authorization or mutation proof. M1 supplies the separate LiveDB owner-action contracts. Full keyboard traversal, measured contrast, screen-reader delivery, PTY navigation and a hosted deployment were not verified. The ordinary API gate includes the CLI wait/follow/final-drain and TUI profile regressions; visual review does not replace them.

Browser provisioning limits remain explicit: the vanilla downloaded Playwright binary could not launch on this musl host (ENOENT despite an existing ELF file); the already provisioned worker Chromium path succeeded and is the canonical browser config's automatic choice. The agent-browser CLI separately failed EPERM while attempting chmod. Its validation limitation was closed through actual Playwright/Chromium checks; the CLI itself was not repaired or reinstalled. No tracked package file changed. The initial TUI render wrapper failed shell parsing before rendering; the corrected POSIX wrapper produced all four fresh images.

### Failed web gate, parity-preserving repair and rerun

The first canonical web gate at baaa07f9 failed EXIT=201 only because the docsFragments corpus test exceeded its unchanged 30-second limit (46.554s). It reported zero invalid fragments; 7,135 other tests passed. Browser tests were not reached. Narrow equivalent coverage runs on candidate and default 4103fad7 both passed around 25.5s with matching manifests/lockfiles and installed dependencies. That comparison is **inconclusive** for the full-run timeout; no base-failure, flake or original-cause classification is claimed.

Repair `617f6f7d17561da249826403a8f974af42500a05` changes only docsFragments.test.tsx. After production heading IDs are assigned, a test-only HAST projection retains complete heading/anchor subtrees in order, reducing unrelated React/DOM rendering. Actual rendered IDs/hrefs, every corpus document, decoding/counters/assertions and the single corpus test's 30-second limit remain. A semantic fixture covers nested/linked headings, tables/autolinks, encoded hashes, images, raw HTML and fences. No dependency, production Markdown code, timeout or gate concurrency changed.

Implementer and tester independently compared exact ordered IDs/hrefs with full rendering across all 76 documents: 883 IDs and 1,138 hrefs matched. The implementer's uninstrumented, fixed old-then-new diagnostic measured collector time 7.792s → 4.711s and serialized DOM bytes 1,809,541 → 127,105; this is not a CI or full-gate causal measurement. A compiled tester mutation dropping standalone anchors failed the named semantic assertion, with missing nested/table/autolink hrefs; it was restored in the export. This establishes that specific refusal, not every possible future Markdown variation.

The unchanged full web rerun passed: 7,137 tests across 343 files, then 26 Chromium tests across seven files. The corpus test completed in 18.813s, checking 76 documents, 208 same-document fragments, 373 relative-document fragments and seven README headings, with zero failures.

### Executed checks and reviews

All heavy jobs ran serially, one at a time, against frozen applicable inputs. Logs are run-local scratch artifacts, not published files. Task was pinned to 3.54.0.

| Source / check | Observed result | Scratch log |
|---|---|---|
| 0014b37e docs:sync plus explicit check-docs:web | PASS, EXIT=0; 76 docs, 48 non-blocking length warnings | gate-log.Ql7cBV |
| 0014b37e npm run build in web | PASS, EXIT=0; production bundler inputs unchanged by later test-only commits | gate-log.bPdraQ |
| baaa07f9 focused controller memory render race test | PASS, EXIT=0 | gate-log.wCebBv |
| Offline chart render/assertions, external CNPG subchart stripped, simple Postgres | PASS, EXIT=0; memory defaults/aliases/explicit values/ceilings/controller-off and Compose inventory executed; not CNPG integration | gate-log.OW52AR |
| baaa07f9 gate:controller | PASS, EXIT=0; fmt/vet/build/lint/deadcode and race/count tests | gate-log.WV97Ne |
| baaa07f9 first gate:web | FAIL, EXIT=201; corpus timeout, browser stage unreached | gate-log.B4RPhD |
| Candidate/default narrow corpus coverage comparison | PASS each, EXIT=0; comparison inconclusive for full failure | gate-log.rMoR32 / gate-log.RfmLqC |
| Repair focused coverage / compiler | PASS each, EXIT=0; three tests; corpus 19.088s | gate-log.Ad1nzG / gate-log.9YjT7B |
| Implementer full-corpus old/new parity | PASS, EXIT=0; 76 comparisons plus summary | gate-log.0yrY3b |
| Tester focused / ordered parity / compiled mutation | Focused and parity PASS; mutant compiler PASS and intended semantic FAIL, EXIT=1 | gate-log.649sk3 / gate-log.8sDSGF / gate-log.xpwZX7 / gate-log.OH1hE5 |
| 617f6f7d gate:web rerun | PASS, EXIT=0; units, coverage and real-browser stage | gate-log.8eHTV2 |
| 617f6f7d gate:api after embedded-doc changes | PASS, EXIT=0; fmt/vet/build/sqlc drift/lint/deadcode and race/count tests | gate-log.uDvupY |
| spec numbering and changelog entry checks | PASS, EXIT=0; frozen-spec canaries tripped and entry found | gate-log.Zo7DyO |
| Shipped-model TUI generation / four PNG renders | PASS, EXIT=0 each; fresh dark/light images inspected | gate-log.pH0fVm / gate-log.Y6mVXJ |
| Initial five browser states / seven-state follow-up | PASS, EXIT=0 each; named checks only, mock data | gate-log.Ci2mOI / gate-log.KNOs25 |

Reviewer found no mandatory/optional issue in docs unit 60e6f26e..0014b37e or controller-test unit 0014b37e..baaa07f9. Reviewer and tester found no mandatory/optional issue in repair baaa07f9..617f6f7d. Their review does not replace the lead's full gates. The lead inspected the relevant raw parity, compiled-control and final gate summaries.

Tui-ux at 0014b37e reported one optional enhancement: strengthen the existing hostile-ID test by checking raw SGR before ANSI stripping. It is deferred; no current unsafe draw was demonstrated, and source cellText/renderer.Plain guards remain. Web-ux reported no product defect; its initial CLI validation limitation was closed by the independently executed browser follow-up, with the bounded residuals above.

M5a's required real-process lane, M5b's private finite cgroups/versioned CI-log reports/local worker-UID timing and final applicable gates remain unverified at this checkpoint. Production sizing and lander-owned post-publication CI timing are not claimed. No affected Compose phase was executed in M4.

## Historical partial record

M4 remains in progress. This record covers committed web and CLI surfaces and the repaired web dependency environment. Operator documentation, embedded-doc synchronization, configuration verification, actual memory-hold browser/TUI visual review and the CLI/API integration gate remain pending. Production defaults remain disabled; M5 calibration and lander-owned CI timing are not claimed.

## Committed surfaces

Web unit `d29dc767cad80274d193a83e27ac83fdadb326ad` changes only ten files under `web/src/**`. Memory holds have explicit badge, panel and announcement copy. Page Resume, Cancel, Stop and discard controls use confirmed ownership together with steering permission. Allowance comes from `memory_hold`, never automatic requeues; absent or null values remain unknown and zero remains zero. Historical source/checkpoint/capture wording does not promise current availability or latest edits. Mock cases and REST/socket refresh tests cover the exposed fields. Cause-omitting board projections remain generic and link to detail.

CLI/TUI unit `3ed822cdf08075378977e1da16276de0d1a660df` changes only eleven files under `api/cmd/uzi/**`. Separate memory predicates and allowance rendering feed get/list/inputs, default wait/follow and TUI rows. Default waiting stops at an owner-required hold; explicit `--until` retains its requested-status behavior. Memory counters do not substitute requeue counters, and untrusted IDs pass through cell sanitation. Known/missing-allowance uxlab scenes use the shipped model, but no scene was rendered or visually reviewed in this wave.

These surfaces consume the already executed M1/M3 API seam. This wave changed no DTO, API authority, SQL, chart/controller/Compose wiring, lockfile or dependency declaration. The earlier owner Resume/Cancel LiveDB results remain backend evidence, not browser execution or live CLI control proof.

## Focused checks and immutable reviews

The web implementer passed 243 focused tests (`gate-log.bTp99s`, EXIT=0). Reviewer, tester and auditor reviewed `09851a52..d29dc767` with no mandatory or optional code findings. Their focused selections passed 199, 300 and 199 tests respectively. Tester reported compiling isolated controls that fail ownership and allowance assertions, and a prior-production control that fails all ten named memory-hold cases. Those results are bounded to their selected tests; modal/archive gating also received source review, and arbitrary retry or action-path sensitivity is not claimed.

The CLI implementer passed its focused suite (`gate-log.7gNkfu`, EXIT=0). Tester reviewed the immutable CLI unit, executed 142 matching test/subtest names with zero skips/failures, and confirmed a compiling prior-consumer control fails missing hold guidance, default wait/follow stopping and stale checkpoint claims. Auditor reported no security findings. Reviewer found one mandatory Medium correctness defect: log-follow could omit a message persisted between the last drain and the status read observing an owner hold.

Repair `78f33965c5fb347079d8d04c3f7849ab4350145e` changes only `run_get.go` and `memory_hold_test.go`: the owner-hold exit performs the existing final drain, matching terminal exit behavior. Its eight regression cases cover memory/requeue holds, text/NDJSON, final sequence appearing once, drain-error propagation, one status poll and no Resume. The lead inspected pre-fix named assertion failures (`gate-log.yVFg6F`, EXIT=1, no timeout) and restored green output (`gate-log.xO1wVR`, EXIT=0). Reviewer/tester/auditor reviewed `3ed822cd..78f33965` and reported the finding closed with no new mandatory or optional findings. Tester independently compiled the exact pre-fix production file in an export and reproduced all eight semantic failures.

Available secret scans detected both canaries with zero tracked findings. Web auditor also reported successful Semgrep canary/tree checks and a dependency audit with zero vulnerabilities. Git-dependent scans ran on the unchanged checkout; web sources matched the reviewed web commit. CLI repair Semgrep had zero applicable Go targets, which supplies no Go SAST coverage. Go dependency vulnerability scanning remained unavailable because govulncheck was absent and validator installation was prohibited.

## Dependency failure and repaired canonical web gate

The first canonical `.uzi/scratch/task-bin/task gate:web` at frozen `3ed822cd` stopped at dependency validation (`gate-log.OnGWHq`, EXIT=201). Installed knip, Mermaid, Vite and transitive packages did not satisfy the committed manifest/lockfile. The lead reproduced the same narrow canonical `deps-check:web` failure on a fresh export of base `09851a52` with the same installed tree (`gate-log.mLUW8o`, EXIT=201). Web package files are identical across that range. The failed gate remains a reported failed check, with comparison evidence.

Earlier focused web runs borrowed that stale installed tree. Matching source manifests and lockfiles alone did not verify installed versions; their passing results do not establish the final environment. The lead followed the documented remedy, `cd web && npm ci --ignore-scripts` (`gate-log.MQHwTN`, EXIT=0), without changing tracked package files. Its install audit reported zero vulnerabilities. Canonical `deps-check:web` then passed with installed/locked versions matched (`gate-log.xcnaYZ`, EXIT=0).

With this relevant environment repair, the lead repeated the unchanged canonical full web gate over frozen `78f33965` (`gate-log.GUJ3xA`, EXIT=0): dependency validation, lint, dead code, docs/changelog/styles, typecheck, 6,861 unit tests across 336 files and 19 Chromium browser tests across five files passed. The ten memory-hold view tests and five mock tests executed and passed. No gate flags, environment settings or concurrency were changed. The browser suite covers its existing scenarios; it does not substitute an actual visual review of the new memory-hold scenes. A read-only log diagnostic and the returned command both confirmed the final exit; the lead inspected the summaries.

## Remaining M4 work

Complete the operator-facing settings/limitations documentation and CHANGELOG, synchronize embedded docs, verify preserved opt-in deployment wiring, build the affected frontend, obtain actual browser and fresh TUI image reviews, run the required CLI/API integration gate and review subsequent committed units. Keep M4 in progress until that work is verified. M5 retains the isolated real-adapter calibration, measured local timing and final canonical gate requirements.
