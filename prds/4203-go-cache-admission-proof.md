# PRD #4203: Go cache admission proof — incomplete evidence report

**Status**: Incomplete; nonproducing scaffold and evidence only; pending formal owner disposition.
**Issue**: #4203; parent #2013 remains open.
**Scope**: Authorized M4 documentation of actual work, not a completed proof or production authorization.

## Result and revision boundary

Overall: **not viable yet**. M1 is unfinished; M2 and M3 have not started.
Missing prerequisites are UNVERIFIED, not a mechanism-level negative for A.
No producing Go workload, cache program, A/B experiment or production eviction was launched.
This report does not establish an all-cache remedy, zero disk parks, or bounds for arbitrary working sets.

Baseline: 18da9c670f4403f4fcab3891f8bb8c59757243ef.
The nonproducing base unit is 4aef534db7756633708552b61461f323418a0673;
the metadata-bound repair is 6d521c57a352351085379cbe39c1d013e2627236 (scaffold revision).
The dispatch records independent reviewer and auditor review of both units, with mandatory
findings closed at the repair revision. That review does not complete the missing proof.
This report is a subsequent documentation-only unit; it does not change that scaffold revision.

Evidence starts at the [preflight README](../e2e/go-cache-admission/README.md),
[read-only inventory](../e2e/go-cache-admission/inventory.md),
[implementation](../e2e/go-cache-admission/preflight.py) and
[pure tests](../e2e/go-cache-admission/test_preflight.py).
The [parent Decision Log](2013-run-cache-growth-mid-turn.md#decision-log)
retains D1–D20, including D13, D20 and the Docker-wired owner answer.

## Requirement and safety evidence

PROVEN means the stated observation, with its boundary. FIXTURE-PROVEN means pure,
injected metadata tests, not live enforcement. NEGATIVE would require evidence
against the mechanism under a valid trial; no such A verdict exists here.
UNVERIFIED includes unavailable prerequisites and legs not reached.

| Requirement or safety leg | Status | Evidence and boundary |
| --- | --- | --- |
| Opt-in experimental harness | FIXTURE-PROVEN, partial | Preflight JSON and pure tests exist. Default exit 2; experimental exit 0 permits inspection of incomplete results. Neither exit enables a producer; report fields keep producer_launched, production_authorized and ab_trigger false. |
| Pinned Go/source inventory | PROVEN, host observation only | Inventory records installed Go 1.27.3 linux/amd64, executable path/hash and source fingerprints; not a worker-image binding. |
| Current safe, nonsecret worker image provenance | UNVERIFIED | No current image identity/source binding established. Local image metadata matching, if supplied, would not prove runtime use. |
| Docker endpoint | PROVEN, version observation only | Inventory records client 29.9.2/server 29.8.0; no allocation, container execution or mount-topology proof. |
| Live soft-pressure threshold | UNVERIFIED | No current nonsecret configuration observation with provenance. Manifest shape/freshness tests do not authenticate the live setting; no source default substituted. |
| Aggregate storage and memory resource gate | UNVERIFIED | No independent enforcement authority covering the write sinks and their bytes/inodes. Declared budgets and endpoint availability are insufficient. |
| Per-launch projected headroom | FIXTURE-PROVEN calculation; live use UNVERIFIED | Injected statfs tests cover re-sampling, per-device aggregation, byte/inode rejection and missing sinks. No producer calls it; no enforceable all-sink charge map established. |
| Private child environment | FIXTURE-PROVEN builder; child effects UNVERIFIED | Empty allowlist construction and scratch paths tested. Actual Go/vet/test/race descendants, executable trust, directory provisioning and confinement not exercised. |
| Source/caller bypass inventory | PROVEN, host source observations only | Inventory identifies disk initialization before cache-program startup, disk-backed FuzzDir, DiskCache-only executable path, modules/toolchain, GOTMPDIR, linked and exported outputs. No all-writes or runtime bypass-closure proof. |
| A admission/protocol, retained/staging bounds and leases | UNVERIFIED, not reached | No cache producer or protocol implementation; get/put, path lifetime, writer overlap and admission behavior not demonstrated. |
| Cleanup, crash recovery and interruption | UNVERIFIED beyond fixture cleanup | Tests clean their owned scratch fixtures. Producer cleanup, sessions/leases, process crashes, interrupted writes and recovery not exercised. |
| Stream limits, exhaustion and adversarial protocol tests | UNVERIFIED beyond metadata tests | JSON numeric/nesting rejection and source enumeration bounds tested; not protocol framing/body exhaustion, producer storage exhaustion or crash-stream proof. |
| Representative tiny and runkind small trials | UNVERIFIED, not launched | Resource-fit prerequisites missing. Tiny example.com/cacheproof leaf/middle/app remains a proposed fixture. |
| Heavy handler/workersvc race matrix | UNVERIFIED/resource-incompatible | Owner-reported archives exceed frozen body/staging limits; not newly measured, not launched and not a B trigger. |
| Completed A comparison | UNVERIFIED, not reached | No meaningful completed A failure or baseline comparison exists. |
| Performance, overlap, warm reuse and overshoot | UNVERIFIED, not reached | No hit rate, slowdown, recompilation time, peak storage, free-space delta or overlapping-producer/bypass measurement. |
| B consideration | UNVERIFIED, not reached | Missing environment prerequisites and incompatible fixtures are not a failed A experiment. |
| Docker aliases and isolation | UNVERIFIED, not run | No bind/symlink/hard-link/cache-variable alias fixture; Docker-wired boundary-only answer remains unchanged. |
| Report and parent D21 | Proposed, pending owner | Evidence documentation only; formal owner disposition still needed. No deferred scope acceptance inferred. |
| Required tests actually performed | FIXTURE-PROVEN | Nine pure tests passed at repair revision; watched repair red/green and integration checks are recorded below with their limits. |

## Frozen resource and comparison envelope

The scaffold declares aggregate 2 GiB: retained 768 MiB, staging 128 MiB,
work/modules/output/logs 1152 MiB; memory 1 GiB; clients 2; writers 1;
objects 4096; records 8192; frames 2; body 64 MiB; header 16 KiB; chunk 64 KiB.
Tiny budget is 16 MiB/30 seconds, workload 180 seconds, series 30 minutes.
These are declarations, not enforced allocation quotas.

Comparison constants remain T=256 MiB, 2T=512 MiB, overshoot=256 MiB,
slowdown=1.25, warm reuse=0.90 and near-threshold=5%. No limit was raised.
The [owner observation](https://github.com/vtmocanu/uzi/issues/2013#issuecomment-6705606355)
attributes 269 MiB to handler and 576 MiB to workersvc heavy race archives.
Both exceed body 64 MiB and staging 128 MiB; workersvc also exceeds 2T.
This incompatibility needs an owner resource/scope decision before the heavy matrix;
it is not a measured failure of A.

## Recorded validation

The following outcomes are supplied by the dispatch for the base/repair work.
Scratch logs are ephemeral, so the durable record is command, revision and outcome
here rather than a requirement to retain or follow scratch links.

| Revision / phase | Command or check | Recorded outcome |
| --- | --- | --- |
| Base 4aef534db7756633708552b61461f323418a0673 with repair regressions, watched RED | python3 -B -m unittest discover -s e2e/go-cache-admission -v | EXIT 1: large JSON timestamp escaped as OverflowError; enumeration consumed 20,005 entries against the 20,000 bound. Regression candidate was tested before repair; not a claim that base already contained the added tests. |
| Repair 6d521c57a352351085379cbe39c1d013e2627236, watched GREEN | python3 -B -m unittest discover -s e2e/go-cache-admission -v | EXIT 0; 9 pure tests, 0.052 s. Enumeration regression allows the rejecting 20,001st observation, not retention of an unbounded listing. |
| Repair, root integration | task check-docs:web | Recorded EXIT 0 in successful integration run; preexisting warnings are not a branch improvement claim. |
| Repair, root integration | TMPDIR=$PWD/.uzi/scratch task check:token-literals | Recorded EXIT 0. |
| Repair, root integration | TMPDIR=$PWD/.uzi/scratch task scan:secrets | Recorded EXIT 0; gitleaks canaries detected, zero tracked findings. |
| Repair, root integration | task sast:semgrep | Recorded EXIT 0; canary seen, zero tree findings. |
| Repair | git diff --check HEAD | Recorded pass. |

The watched RED log was gate-log.behSGp, GREEN gate-log.2e2z1y and latest
root integration gate-log.u7rjtq, all under the run's ephemeral scratch directory.
The integration run recorded EXIT 0. These results do not validate this later
documentation edit or an eventual exact-head CI run.
No agent/** change was made by the scaffold units, so check:agent was not applicable.
The full suite remains pending applicable exact-head CI; no pass is claimed.
No repo-wide gate or producing validation is part of this M4 documentation work.

## Proposed decision delta and smallest next step

A-first consideration records a possible cooperative Go cache admission approach;
this run failed to establish prerequisites for trying it. A proposes no change to
D13 or D20. B was not reached. Any production decision remains pending the owner. The parent production PRD remains incomplete and open;
its original status, D1–D20, Docker-wired answer and boundary-only trim remain unchanged.

The smallest justified next step is independent verification of an existing bounded
fixture, current safe nonsecret image and live-threshold provenance, and an owner
resource/scope decision for the heavy representative matrix. This is a prerequisite
proposal, not production enablement, a new privilege policy or a D13 exception.

M1 unfinished and M2/M3 unstarted remain required scope awaiting formal owner
disposition. Documenting that boundary does not accept deferral or complete those
milestones. The owner must explicitly decide the retained and deferred scope;
no such formal disposition is recorded here. Hosted proof and production acceptance
are not established by this report.
