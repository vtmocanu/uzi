#!/usr/bin/env bash
# Lander-only real-image proof. No network Git, runtime-image override, or retries.
set -euo pipefail
REPO="$(python3 -I - "${BASH_SOURCE[0]}" <<'PY'
import os
from pathlib import Path
import sys
import unicodedata

def validate(value):
    if any(unicodedata.category(c) in {"Cc", "Cf", "Cs", "Zl", "Zp"} for c in value):
        raise ValueError("unsafe path")

try:
    validate(sys.argv[1])
    validate(os.getcwd())
    repo = str(Path(sys.argv[1]).resolve(strict=True).parents[2])
    validate(repo)
except (OSError, ValueError, RuntimeError, IndexError):
    print("pod epoch proof FAIL: unsafe or unresolved checkout path", file=sys.stderr)
    sys.exit(1)
print(repo)
PY
)"
cd "$REPO"
die() { printf 'pod epoch proof FAIL: %s\n' "$*" >&2; exit 1; }
HEAD_REQUIRED=${UZI_POD_EPOCH_PROOF_HEAD:?set the FINAL reviewed full 40-hex PR head}
[[ "$HEAD_REQUIRED" =~ ^[0-9a-f]{40}$ ]] || die "head must be full lowercase 40-hex"
ATTEMPT=${WORKER_MEMORY_CALIBRATION_ATTEMPT:?set a positive local calibration attempt}
[[ "$ATTEMPT" =~ ^[1-9][0-9]*$ ]] || die "attempt must be a positive decimal integer"
[[ "${GITHUB_ACTIONS:-}" != true ]] || die "run on the storage host, using local calibration"
assert_source() {
  local actual status
  actual="$(git rev-parse HEAD)" || die "cannot read actual HEAD"
  [[ "$actual" == "$HEAD_REQUIRED" ]] || die "actual HEAD differs from required FINAL head"
  status="$(git status --porcelain=v1 --untracked-files=all)" || die "cannot inspect tree"
  [[ -z "$status" ]] || die "tracked/untracked tree must be clean"
}
assert_source
# Validate BOTH targets before starting either expensive build.
for template in base jvm; do
  awk 'toupper($1) == "FROM" && toupper($3) == "AS" && tolower($4) == "release" { found=1 }
       END { exit !found }' "agent/templates/$template/Dockerfile" ||
    die "$template Dockerfile has no release target"
done
mkdir -p "$REPO/.uzi/scratch"
EVIDENCE="$(mktemp -d "$REPO/.uzi/scratch/pod-epoch-proof.XXXXXXXX")"
TOKEN=${EVIDENCE##*/}
python3 -I - "$EVIDENCE" <<'PY'
import json
import sys
print(json.dumps({"kind": "proof_evidence", "path": sys.argv[1]}, ensure_ascii=True))
PY
printf '%s\n' "$HEAD_REQUIRED" > "$EVIDENCE/proof-head"
printf '%s\n' "$ATTEMPT" > "$EVIDENCE/attempt"
finish() {
  local rc=$?
  trap - EXIT
  printf 'EXIT=%s\n' "$rc" > "$EVIDENCE/result"
  python3 -I - "$rc" "$HEAD_REQUIRED" "$EVIDENCE" <<'PY'
import json
import sys
rc, head, evidence = sys.argv[1:]
print(json.dumps({"kind": "proof_result", "exit": int(rc),
                  "status": "PASS" if rc == "0" else "FAIL",
                  "head": head, "evidence": evidence}, ensure_ascii=True),
      file=sys.stdout if rc == "0" else sys.stderr)
PY
  exit "$rc"
}
trap finish EXIT
run() {
  local name=$1 rc=0
  shift
  local limit=default seconds=80
  case "$name" in
    *-inspect) limit=65536 ;;
    *-build|*-worker-uid) seconds=7220 ;;
  esac
  python3 -I - "$REPO" "$EVIDENCE" "$name" "$limit" "$seconds" "$@" <<'PY' || rc=$?
import contextlib
import io
import json
from pathlib import Path
import sys

repo, evidence, phase, limit, seconds, *argv = sys.argv[1:]
sys.path.insert(0, str(Path(repo) / "e2e/worker-uid"))
from bounded import REPORT_BYTES, capture

limit = REPORT_BYTES if limit == "default" else int(limit)
base = Path(evidence) / phase
print(json.dumps({"kind": "command_phase", "phase": phase}), flush=True)
class DiagnosticBuffer:
    # Keep only 64 KiB; overflow fails after capture has reaped its producer.
    def __init__(self):
        self.data = bytearray()
        self.overflow = False

    def write(self, text):
        data = text.encode("utf-8", errors="backslashreplace")
        available = 65536 - len(self.data)
        self.data.extend(data[:available])
        self.overflow |= len(data) > available
        return len(text)

    def flush(self):
        pass

diagnostics = DiagnosticBuffer()
with contextlib.redirect_stderr(diagnostics):
    rc = capture(['/bin/sh', '-c', 'exec "$@" 2>&1', 'proof', *argv],
                 str(base) + ".raw", float(seconds), limit)
if diagnostics.overflow:
    rc = 125
# Raw artifacts are opaque, untrusted binary. Only escaped records render.
rendered = 0
render_limit = limit * 8 + 65536
with open(str(base) + ".raw", "rb") as raw, open(str(base) + ".log", "w", encoding="ascii") as log:
    # Both streams use the same chunks and one shared rendered-byte allowance.
    for kind, source in (("command_output", raw),
                         ("capture_diagnostic", io.BytesIO(diagnostics.data))):
        while chunk := source.read(16384):
            record = json.dumps({"kind": kind, "phase": phase,
                                 "text": chunk.decode("utf-8", errors="replace")},
                                ensure_ascii=True) + "\n"
            rendered += len(record)
            if rendered > render_limit:
                rc = rc or 125
                break
            log.write(record)
            sys.stdout.write(record)
    if diagnostics.overflow:
        record = json.dumps({"kind": "capture_diagnostic", "phase": phase,
                             "text": "helper diagnostic overflow: limit 65536 bytes"}) + "\n"
        if rendered + len(record) <= render_limit:
            log.write(record)
            sys.stdout.write(record)
print(json.dumps({"kind": "command_status", "phase": phase, "exit": rc}), flush=True)
sys.exit(rc)
PY
  printf '%s\n' "$rc" > "$EVIDENCE/$name.exit"
  return "$rc"
}
# Each command gets one attempt; failure stops siblings. Outer capture includes
# GNU timeout's grace and capture's own five-second shutdown/join allowance.
inspect_image() {
  local template=$1 tag=$2 stage=$3
  run "$template-$stage-inspect" timeout --kill-after=10s 60s docker image inspect "$tag"
  python3 -I - "$EVIDENCE/$template-$stage-inspect.raw" "$HEAD_REQUIRED" "$template" \
    "$EVIDENCE/$template-$stage.id" <<'PY'
import json
import re
import sys
from pathlib import Path

raw, head, template, output = sys.argv[1:]
with Path(raw).open("rb") as source:
    data = source.read(65536 + 1)
if len(data) > 65536:
    raise ValueError("image inspect exceeds 64 KiB")
items = json.loads(data)
assert isinstance(items, list) and len(items) == 1, "ambiguous image inspect"
item = items[0]
assert re.fullmatch(r"sha256:[0-9a-f]{64}", item["Id"]), "invalid image ID"
labels = item["Config"]["Labels"]
assert labels["io.uzi.pod-epoch.proof-revision"] == head, "proof revision label"
assert labels["io.uzi.pod-epoch.template"] == template, "proof template label"
assert labels["org.opencontainers.image.revision"] == head, "baked source revision"
assert labels["io.github.vtmocanu.uzi.runtime-base"] == "local", "unexpected pinned runtime"
assert "UZI_WORKER_TEMPLATE=" + template in item["Config"]["Env"], "baked template"
Path(output).write_text(item["Id"] + "\n")
PY
}
HELPER_HASH="$(git show "$HEAD_REQUIRED:agent/templates/pod-epoch-init.cjs" | sha256sum)"
HELPER_HASH=${HELPER_HASH%% *}
ENTRY_HASH="$(git show "$HEAD_REQUIRED:agent/templates/entrypoint.sh" | sha256sum)"
ENTRY_HASH=${ENTRY_HASH%% *}
printf 'helper=%s\nentrypoint=%s\n' "$HELPER_HASH" "$ENTRY_HASH" > "$EVIDENCE/committed-hashes"
for template in base jvm; do
  assert_source
  tag="pod-epoch-proof:$HEAD_REQUIRED-$template-$TOKEN"
  printf '%s\n' "$tag" > "$EVIDENCE/$template.tag"
  run "$template-build" timeout --kill-after=10s 7200s docker build \
    --target release --build-arg "UZI_SRC_SHA=$HEAD_REQUIRED" \
    --label "io.uzi.pod-epoch.proof-revision=$HEAD_REQUIRED" \
    --label "io.uzi.pod-epoch.template=$template" \
    --file "agent/templates/$template/Dockerfile" --tag "$tag" .
  inspect_image "$template" "$tag" before
  image_id="$(< "$EVIDENCE/$template-before.id")"
  # Probe immutable image content, without source mounts or anonymous-volume copy.
  run "$template-package-hashes" timeout --kill-after=10s 60s docker run \
    --rm --pull never --network none --user 10001:10001 --cap-drop ALL \
    --security-opt no-new-privileges --tmpfs /data --tmpfs /nix \
    --entrypoint /bin/sh "$image_id" -eu -c '
      test "$(id -u)" = 10001
      version=$(/usr/bin/flock --version)
      printf "%s\n" "$version"
      case "$version" in "flock from util-linux "[0-9]*) ;; *) exit 1 ;; esac
      owner=$(apk info --who-owns /usr/bin/flock)
      printf "%s\n" "$owner"
      case "$owner" in "/usr/bin/flock is owned by flock-"[0-9]*) ;; *) exit 1 ;; esac
      package=$(apk info -e -v flock)
      printf "%s\n" "$package"
      case "$package" in flock-[0-9]*) ;; *) exit 1 ;; esac
      printf "%s  %s\n" "$1" /usr/local/lib/uzi/pod-epoch-init.cjs \
        "$1" /app/templates/pod-epoch-init.cjs \
        "$2" /usr/local/sbin/uzi-entrypoint | sha256sum -c -
    ' proof "$HELPER_HASH" "$ENTRY_HASH"
  assert_source
  inspect_image "$template" "$tag" prelane
  cmp -s "$EVIDENCE/$template-before.id" "$EVIDENCE/$template-prelane.id"
  run "$template-worker-uid" timeout --kill-after=10s 7200s env \
    WORKER_UID_SKIP_BUILD=1 "WORKER_UID_IMAGE=$tag" \
    "WORKER_MEMORY_CALIBRATION_RUN_ID=$HEAD_REQUIRED-$template" \
    "WORKER_MEMORY_CALIBRATION_ATTEMPT=$ATTEMPT" task test:agent:worker-uid
  inspect_image "$template" "$tag" after
  cmp -s "$EVIDENCE/$template-before.id" "$EVIDENCE/$template-after.id"
  assert_source
done
# Recheck BOTH tags at completion, including base after the JVM lane.
for template in base jvm; do
  tag="$(< "$EVIDENCE/$template.tag")"
  inspect_image "$template" "$tag" final
  cmp -s "$EVIDENCE/$template-before.id" "$EVIDENCE/$template-final.id"
done
assert_source
