# #3430 M1 final-head image proof handoff

M1 real-image proof is **pending for BOTH base and JVM**. It is relocated to
the lander's storage host, not waived. This worker implements handoff tooling
and Docker-free checks only. Do not describe all M1 as verified while either
image proof is pending. Exact-head CI is supplementary and does not replace
the lander proof.

The lander must use the FINAL reviewed PR head, including all M2 code and this
tooling. Any later commit invalidates this evidence for landing and requires
a new invocation at the new final head. Do not commit or edit the checkout
while proof runs. The script checks exact HEAD and clean tracked/untracked
status before any build, between phases, and at the end. These checks detect
observed drift; the operator must prevent concurrent edits or commits, including
changes temporarily made and reverted between checks.

## Storage-host invocation

Start in a clean checkout of the final reviewed PR revision. Supply its actual
full lowercase 40-hex SHA in place of FINAL_FULL_40_HEX_PR_HEAD below. This is
deliberately not the handoff worker's starting SHA. Set a positive decimal
attempt explicitly for this local calibration run. The storage host needs
Docker with BuildKit, sufficient image/storage and private cgroup calibration
capacity, GNU timeout, Bash, Python 3, sha256sum, task, and the lane's existing
host dependencies. Provision these through the normal lander setup before
starting. Do not fake GITHUB_ACTIONS; this procedure requires local calibration.

Copy this exact invocation into the PR body, substituting the actual final
SHA and the chosen attempt. Run the committed script directly from that
checkout; it does no network Git operations.

```bash
export UZI_POD_EPOCH_PROOF_HEAD=FINAL_FULL_40_HEX_PR_HEAD
export WORKER_MEMORY_CALIBRATION_ATTEMPT=1
bash e2e/worker-uid/pod-epoch-proof.sh
```

The executable script is [pod-epoch-proof.sh](../e2e/worker-uid/pod-epoch-proof.sh).
Before changing directories, isolated Python validates the lexical script path,
physical working directory and resolved repository path, rejecting Unicode
categories Cc, Cf, Cs, Zl and Zp (including invalid UTF-8 surrogates).
Resolution failures and unsafe paths produce only a constant ASCII refusal
before Git, Docker or task runs; ordinary Unicode path names remain allowed.
It creates a unique evidence directory under .uzi/scratch and prints its
absolute location. Each phase retains merged stdout/stderr in a bounded .raw
artifact: opaque, untrusted binary, never a human log and never rendered directly.
Inspect capture is limited to 64 KiB; build, lane and package capture to 8 MiB
(the existing bounded.py REPORT_BYTES default). The isolated Python wrapper
imports the trusted sibling bounded.py through the explicit repository path,
without changing its capture API. It merges streams through a fixed shell
launcher with arguments passed separately, never interpolated as shell source.
Overflow fails the phase and stops subsequent phases, even if the producer
would otherwise exit successfully.

The existing per-phase .log artifact and command output on stdout contain only
ASCII JSON Lines: kind command_output for child output or capture_diagnostic
for helper stderr, controlled phase, and escaped text from fixed 16 KiB byte
chunks using json.dumps(ensure_ascii=True). Helper stderr is redirected into
a buffer retaining at most 64 KiB, then rendered through the same JSON boundary;
buffer overflow explicitly fails with status 125 after capture finishes its
normal shutdown. The helper API and bounded.py remain unchanged. Chunking is
independent of line count, so newline floods do not create a record per line.
Rendered output is additionally limited to raw-limit * 8 + 64 KiB per phase;
overflow fails. Terminal controls, Unicode direction controls and forged status
rows remain escaped text inside command_output records. Trusted command_phase
and command_status records, plus proof_evidence and proof_result records, are
separate from command output; separate .exit files and the final result file
retain actual statuses. Inspect validation reads
only .raw, at most 64 KiB plus one overflow sentinel before JSON parsing.
Python -I preserves validation despite inherited PYTHONOPTIMIZE.

Preserve that directory and the worker-UID/calibration report directories named
inside its escaped lane records as lander evidence. A preflight refusal occurs
before the evidence directory exists and exits nonzero.

## Commands and identity relationships

For each template in base, then jvm, the script builds the corresponding
Dockerfile with this argument structure (TAG is generated per invocation,
head, and template):

```bash
docker build --target release \
  --build-arg "UZI_SRC_SHA=$UZI_POD_EPOCH_PROOF_HEAD" \
  --label "io.uzi.pod-epoch.proof-revision=$UZI_POD_EPOCH_PROOF_HEAD" \
  --label "io.uzi.pod-epoch.template=$template" \
  --file "agent/templates/$template/Dockerfile" --tag "$tag" .
```

Both Dockerfiles currently declare FROM runtime AS release; the script verifies
both release targets before building either. This is explicitly a new build
using the Dockerfiles' default runtime source. There is no pinned runtime
override or prebuilt-image shortcut. A proposal to use another recorded source
needs separate exact-source approval and is outside this script.

The lander records actual Docker image IDs from image inspect; no registry
digest is inferred or invented. Each template's stable local tag must resolve
to the same immutable sha256 image ID before the probes, immediately before
the lane, after the lane, and at final completion. The ID must carry the exact
proof-revision and template labels, the baked OCI revision must equal the final
head, the baked template environment must match, and the runtime-base label
must be local. Base and JVM each have their own recorded identity; do not
substitute one image's result for the other.

The package/hash probe runs the immutable ID as UID/GID 10001, drops all caps,
sets no-new-privileges, disables network, and masks /data and /nix with tmpfs
to avoid anonymous-volume copy. It invokes absolute /usr/bin/flock --version,
asserts a util-linux version, verifies apk ownership by the flock package,
and records apk's installed flock package version. It verifies both baked
pod-epoch-init.cjs copies and the baked uzi-entrypoint against SHA-256 hashes
of the committed source at the required head, without source bind mounts.

For EACH checked tag, the script then executes exactly this lane environment:

```bash
WORKER_UID_SKIP_BUILD=1 \
WORKER_UID_IMAGE="$tag" \
WORKER_MEMORY_CALIBRATION_RUN_ID="$UZI_POD_EPOCH_PROOF_HEAD-$template" \
WORKER_MEMORY_CALIBRATION_ATTEMPT="$WORKER_MEMORY_CALIBRATION_ATTEMPT" \
task test:agent:worker-uid
```

This retains the existing two-process pod epoch tests and all worker UID
profiles. The existing image-layout.mjs import checks the actual baked helper
directory as root-owned 0755, helper files as root-owned 0644, equality of the
copies, and the actual baked wrapper import across the lane's UID profiles.
The lane's image-bound tool preparation and full private cgroup memory
calibration remain mandatory. WORKER_UID_SKIP_BUILD skips only run.sh's
parent-image build; the existing lane still prepares its owned tool image.
A failed or skipped required proof is not acceptance. The script relies on
the existing lane's test inventory and calibration rejection of incomplete
evidence.

Every build/lane has one attempt and a two-hour timeout plus ten-second kill
grace; metadata and package probes have a one-minute timeout plus the same
grace. Outer capture deadlines are 7220 seconds for build/lane and 80 seconds
for metadata/package, including timeout grace and bounded.py's five-second
owned-process-group shutdown/join. Each command is attempted once; failure
stops subsequent phases and exits nonzero. There are no extraction retries.
The script does not remove images or volumes; the existing lane owns and
cleans its own journaled resources. Preserve generated tags
until evidence review and remove only positively attributed resources later.

## Remaining acceptance

- M1 outstanding image-proof portion (base): pending; lander runs this procedure on the storage host
  at the final exact reviewed PR head and attaches the base ID, inspect logs,
  util-linux/package/hash log, worker-UID report and private cgroup calibration.
- M1 outstanding image-proof portion (JVM): pending; same actor, host, revision, and evidence for JVM.
- Exact-head CI: supplementary; lander links the successful final-head CI run
  alongside both image proofs.

This worker cannot perform these image-proof portions of M1: image builds,
extraction retries and real-image proof are explicitly prohibited here. This
handoff does not assert a formal owner partial-completion decision or accept
any pending milestone. Until the required lander evidence exists, both image
proofs remain pending and M1 must not be reported fully verified.

## Docker-free tooling checks

```bash
bash -n e2e/worker-uid/pod-epoch-proof.sh
shellcheck --severity=warning e2e/worker-uid/pod-epoch-proof.sh
python3 -B e2e/worker-uid/pod_epoch_proof_test.py
```

The Python tests use fake git/docker/task executables under invocation-owned
scratch. They inspect command arguments as data, never execute the in-image
probe payload, and exercise preflight refusal, both template invocations,
identity validation, failures, retagging and final source drift. Security
regressions also cover metadata overflow (including the previously accepted
2 MiB label and a 24 MiB response), continuous stderr floods with capture caps
and process shutdown, and escaped ANSI/C0/C1/bidi/forged status output.
Additional regressions cover lexical/physical unsafe paths, relative paths,
symlink bypasses, invalid UTF-8, legitimate Unicode, actual overflow diagnostics
as distinct JSON records, and bounded helper diagnostic overflow failing 125.
Their success is tooling evidence only, not image or M1 acceptance.

The lead observed the five-test tooling suite taking 13.076 seconds locally
at 1d0652df96391ec3a4a0afaf6823b035ca0b8666. The prior eight-test suite
took 25.573 seconds in the lead's local check. With path and helper-diagnostic
regressions, the twelve-test suite took 36.660 seconds in the lead's focused
check; Bash syntax and shellcheck also passed. These are local observations
for the serial new CI step's cost only. Whole-CI critical-path impact, queue time,
rerun cost and cache effects are unmeasured; approved M2c permits unavailable
CI timing to remain explicitly unmeasured. The actual full non-image agent
gate remains pending with the lead; its correct-UID environment preflight
already passed. This scoped security repair does not run that gate.
