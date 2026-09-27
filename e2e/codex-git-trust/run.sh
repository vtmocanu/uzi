#!/usr/bin/env bash
# Opt-in Issue #1716 worker-image fixture: git in the run's checkout from a real Codex command
# root (runner-cmd under the supervisor + command sandbox). Build and fixture are separate steps.
# Exit 77 is SKIP (no Docker, no image, or no uid split), never a pass. Exit 2 is a usage error
# (including a missing bind source); exit 3 is an unverified container cleanup.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
image="${CODEX_GIT_TRUST_IMAGE:-uzi-agent-codex-git-trust:base}"
action="${1:-fixture}"
name="codex-git-trust-$$"

# Bind hygiene (issue #1769): every bind is `--mount type=bind,...,readonly`, never `-v`. A `-v`
# whose source is missing is silently CREATED by dockerd, as root, on the host; `--mount` refuses
# instead. Each source is also checked here first, so a missing one is a named usage error
# (exit 2), never a SKIP (77) and never created. Checked before the Docker probe so a missing
# source cannot be reported as a skip on a host without Docker.
mount_args=()
bind_ro() {
  local label="$1" source="$2" target="$3"
  case "$source" in
    /*) ;;
    *)
      echo "ERROR: bind source for $label is not an absolute path: $source" >&2
      exit 2
      ;;
  esac
  case "$source" in
    *,*)
      echo "ERROR: bind source for $label contains a comma, which --mount cannot express: $source" >&2
      exit 2
      ;;
  esac
  if [ ! -e "$source" ]; then
    echo "ERROR: bind source for $label does not exist: $source" >&2
    exit 2
  fi
  mount_args+=(--mount "type=bind,source=$source,target=$target,readonly")
}

source_env=()
if [ "$action" = fixture ]; then
  bind_ro "the fixture dir" "$here" /work/codex-git-trust
  # CODEX_GIT_TRUST_SRC_DIR overrides the mounted source tree (e.g. a base-commit worktree's
  # agent/src for the before run); it implies CODEX_GIT_TRUST_MOUNT_SRC=1.
  src_dir="${CODEX_GIT_TRUST_SRC_DIR:-$repo/agent/src}"
  if [ "${CODEX_GIT_TRUST_MOUNT_SRC:-0}" = 1 ] || [ -n "${CODEX_GIT_TRUST_SRC_DIR:-}" ]; then
    bind_ro "CODEX_GIT_TRUST_SRC_DIR" "$src_dir" /app/src
    source_env=(-e CODEX_GIT_TRUST_SRC=/app/src)
  fi
  # CODEX_GIT_TRUST_BIN_DIR mounts this tree's statically built uzi-codex-supervisor,
  # uzi-codex-fileop and uzi-codex-command-sandbox over an OLDER image's copies, whose launch
  # protocol can lag a mounted agent/src. The image's base layers (git, node) stay the image's.
  if [ -n "${CODEX_GIT_TRUST_BIN_DIR:-}" ]; then
    for bin in uzi-codex-supervisor uzi-codex-fileop uzi-codex-command-sandbox; do
      bind_ro "CODEX_GIT_TRUST_BIN_DIR/$bin" "$CODEX_GIT_TRUST_BIN_DIR/$bin" "/usr/local/bin/$bin"
    done
  fi
fi

if ! command -v docker >/dev/null 2>&1 || ! timeout 10s docker info >/dev/null 2>&1; then
  echo "SKIP: Docker CLI or daemon is unavailable"
  exit 77
fi

# Container hygiene (issue #1769): a container that outlives this script (killed wrapper,
# interrupted run) once left root-owned directories in a clone. So removal is bounded and
# runs on every exit path, and is then VERIFIED by exact name. A failed removal or
# verification is never reported as a pass or a skip: the script exits 3 (or keeps its
# own failure status) and says cleanup is incomplete, so a caller keeps its staging.
cleaned=0
remove_container() {
  [ "$cleaned" -eq 1 ] && return 0
  timeout --kill-after=2s 10s docker rm -f "$name" >/dev/null 2>&1 || true
  local left
  if ! left="$(timeout --kill-after=2s 10s docker ps -a --filter "name=^/${name}\$" -q)"; then
    echo "ERROR: could not verify that container $name is gone; cleanup incomplete" >&2
    return 1
  fi
  if [ -n "$left" ]; then
    echo "ERROR: container $name ($left) still exists after docker rm -f; cleanup incomplete" >&2
    return 1
  fi
  cleaned=1
}
on_exit() {
  local status=$?
  trap - EXIT INT TERM
  if ! remove_container; then
    if [ "$status" -eq 0 ] || [ "$status" -eq 77 ]; then status=3; fi
  fi
  exit "$status"
}

case "$action" in
  build)
    # CODEX_GIT_TRUST_BUILD_NETWORK (issue #1769): explicit opt-in to `docker build --network host`,
    # forwarded ONLY when it is exactly "host". On some hosts the default bridge network's egress
    # hangs, so the build's package/artifact downloads stall until the build timeout. This is
    # build-only: the fixture container below always runs with --network none. Any other
    # non-empty value is a caller mistake; warn instead of forwarding it (the default, bridge,
    # is unchanged).
    build_network_args=()
    if [ -n "${CODEX_GIT_TRUST_BUILD_NETWORK:-}" ]; then
      if [ "$CODEX_GIT_TRUST_BUILD_NETWORK" = "host" ]; then
        build_network_args=(--network host)
      else
        echo "WARN: CODEX_GIT_TRUST_BUILD_NETWORK=$CODEX_GIT_TRUST_BUILD_NETWORK is not \"host\"; not forwarding it (the build uses the default network)" >&2
      fi
    fi
    # --foreground: same reason as the fixture's `timeout ... docker run` below (Ctrl-C reach).
    DOCKER_BUILDKIT=1 timeout --foreground --kill-after=10s "${CODEX_GIT_TRUST_BUILD_TIMEOUT:-600}" docker build \
      "${build_network_args[@]}" -f "$repo/agent/templates/base/Dockerfile" -t "$image" \
      --build-arg "UZI_SRC_SHA=$(git -c safe.directory="$repo" -C "$repo" rev-parse HEAD)" "$repo"
    ;;
  fixture)
    if ! timeout 10s docker image inspect "$image" >/dev/null 2>&1; then
      echo "SKIP: image $image is absent; run task test:codex-git-trust:build"
      exit 77
    fi
    # CODEX_GIT_TRUST_REQUIRE_LANDLOCK (issue #1769 m3 acceptance guard): forwarded ONLY when
    # it is exactly "1", so the fixture can turn a missing Landlock ABI into a hard FAIL
    # instead of a silent required-mode skip. Any other non-empty value is a caller mistake
    # (e.g. "true"/"yes"), which would otherwise silently forward and be ignored by the
    # fixture's own strict "=== '1'" check (see fixture.ts) rather than doing what the caller
    # meant; warn instead of forwarding it.
    landlock_args=()
    if [ -n "${CODEX_GIT_TRUST_REQUIRE_LANDLOCK:-}" ]; then
      if [ "$CODEX_GIT_TRUST_REQUIRE_LANDLOCK" = "1" ]; then
        landlock_args=(-e "CODEX_GIT_TRUST_REQUIRE_LANDLOCK=1")
      else
        echo "WARN: CODEX_GIT_TRUST_REQUIRE_LANDLOCK=$CODEX_GIT_TRUST_REQUIRE_LANDLOCK is not \"1\"; not forwarding it (required mode will silently skip on a kernel without Landlock)" >&2
      fi
    fi
    # INT/TERM: exit with the matching status; the EXIT trap then removes the container.
    # bash runs these traps only after the foreground `timeout ... docker run` returns, so a
    # signal sent to this script's PID alone waits up to CODEX_GIT_TRUST_TIMEOUT plus the
    # kill grace before cleanup starts. `timeout --foreground` keeps docker run in this
    # script's process group (without it, timeout calls setpgid and a group signal never
    # reaches `docker run`, the CLI), so a process-group signal (Ctrl-C) reaches docker run
    # directly and it returns promptly. On expiry --foreground kills only docker run itself,
    # which is the whole command here. The default CODEX_GIT_TRUST_TIMEOUT (420s) covers a
    # normal full run (~105s) plus two stalled finalize-import cases (~97s each, fixture.ts
    # CASE_BOUND_MS), so a stall ends in the fixture's named FAIL lines, not this timeout.
    trap on_exit EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM
    set +e
    timeout --foreground --kill-after=10s "${CODEX_GIT_TRUST_TIMEOUT:-420}" docker run --rm --network none \
      --cap-drop ALL \
      --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add SETPCAP --cap-add SETUID --cap-add SETGID \
      --security-opt no-new-privileges --tmpfs /data \
      --entrypoint /usr/local/sbin/uzi-entrypoint \
      "${mount_args[@]}" "${source_env[@]}" "${landlock_args[@]}" \
      --name "$name" "$image" /bin/sh -c \
      'cd /app && exec /usr/local/bin/node --import tsx /work/codex-git-trust/fixture.ts'
    rc=$?
    set -e
    # The explicit post-run removal is the one attempt on the normal path; the EXIT trap does
    # not repeat it. An unverified cleanup turns a pass or a skip into 3 and keeps a failure's
    # own status.
    if ! remove_container; then
      cleaned=1
      if [ "$rc" -eq 0 ] || [ "$rc" -eq 77 ]; then exit 3; fi
      exit "$rc"
    fi
    if [ "$rc" -eq 77 ]; then
      echo "SKIP: image entrypoint did not establish uid split"
      exit 77
    fi
    exit "$rc"
    ;;
  *)
    echo "usage: $0 {build|fixture}" >&2
    exit 2
    ;;
esac
