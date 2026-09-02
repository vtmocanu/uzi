# shellcheck shell=bash
# phase:    docker-sidecar
# title:    PRD #83 M2: rootless DinD sidecar + Decision-3 efficacy
# critical: no
# lane:     gitlab
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  $ENVFILE+=UZI_DIND_SOCKET (:104); starts dind sidecar + recreates agent (agent-docker profile only, self-skips when DOCKER_PROFILE empty)
# restores: -
# ── PRD #83 M2: docker-capable worker (rootless DinD sidecar) ────────────────────────
# Runs ONLY under `--profile agent-docker` (so the default suite is untouched). Two
# assertions justify M2: (a) a run's docker path reaches the sidecar and `docker compose
# up` works; (b) the LIVE Decision-3 efficacy test — a container started via the sidecar
# CANNOT read the worker's join-token path. (b) is driven OUTSIDE the guardrail (docker
# invoked directly, not through the SDK's guarded Bash) so it proves MOUNT-NAMESPACE
# isolation (the DinD daemon's fs holds none of the worker's secret/`/data`/`/nix`), NOT
# the guardrail substring check.
if [ -n "$DOCKER_PROFILE" ]; then
  say "PRD #83 M2: rootless DinD sidecar + Decision-3 efficacy"
  DH="unix:///run/dind/docker.sock"            # the path M1's resolveDockerWiring probes
  DTOK=/run/secrets/worker_token               # the worker join-token file (the canary)
  DIMG=alpine:3.22                             # the sidecar pulls this to run the attacks
  # Start the sidecar EXPLICITLY. The `agent` deliberately does NOT `depends_on: dind`
  # (that errors under the plain `--profile agent` path on the pinned engine — see the
  # compose comment), and the harness brings the stack up naming only `agent`, so `dind`
  # would otherwise never be created. `--wait` blocks on dind's `docker info` healthcheck,
  # so the daemon (not just the container) is ready before the assertions.
  "${COMPOSE[@]}" up -d --wait dind >/dev/null 2>&1 || fail "the DinD sidecar did not become healthy (up --wait dind)"
  # Root-client exec (bypasses socket perms) — proves the DAEMON's mount ns regardless of
  # client uid. `docker`/DOCKER_HOST is not in the agent's login env (only injected into the
  # SDK subprocess), so set it explicitly here.
  rootdk() { "${COMPOSE[@]}" exec -T -e DOCKER_HOST="$DH" agent "$@"; }
  # Runner-uid exec (uid 10002, via the SAME setpriv path the runtime uses) — proves the
  # split-uid agent's OWN docker reaches the daemon (needs the dind-init 0666 socket).
  runnerdk() { "${COMPOSE[@]}" exec -T -e DOCKER_HOST="$DH" agent \
    /bin/setpriv --reuid runner --regid runner --init-groups -- "$@"; }

  # 0) Liveness FIRST — else every negative below is vacuous (a down daemon also yields
  #    "no such file"). `docker info` must succeed against the sidecar.
  rootdk docker info >/dev/null 2>&1 || fail "DinD daemon not reachable (docker info failed) — the Decision-3 negatives would be vacuous"
  pass "DinD daemon reachable via the shared socket ($DH)"

  # (a) the split-uid agent path reaches the daemon AND runs a container; then compose v2.
  runnerdk docker run --rm "$DIMG" echo dind-run-ok 2>/dev/null | grep -q dind-run-ok \
    || fail "the runner uid (10002) could not run a container via the sidecar"
  pass "the runner uid runs a container through the sidecar (the agent's real docker path)"
  # Warm-up: pay the compose PLUGIN's cold-start (plugin load) before the timed
  # assertion. Cheap and client-side; orthogonal to the DAEMON's own cold path (first
  # network-create), which the retry below absorbs.
  rootdk docker compose version >/dev/null 2>&1 || true
  # The toy `docker compose up`, with a BOUNDED RETRY. On a cold rootless daemon the
  # FIRST compose invocation is transiently flaky (network create / compose-plugin
  # cold-start) — diagnosed live: the exact command passes once the daemon is warm, and
  # the full Decision-3 attack matrix below is clean. The old single attempt buried its
  # stderr under `2>/dev/null`, so a failure was a black box. Capture COMBINED output,
  # retry up to 3x (~2s apart), and on the FINAL failure surface a tail so the next real
  # breakage is diagnosable instead of silent.
  toy_compose='set -e; d=$(mktemp -d); printf "services:\n  toy:\n    image: '"$DIMG"'\n    command: [\"echo\",\"compose-ok\"]\n" > "$d/compose.yaml"; docker compose -f "$d/compose.yaml" up --abort-on-container-exit --exit-code-from toy'
  tc_ok=""; tc_out=""
  for tc_try in 1 2 3; do
    tc_out="$(rootdk sh -c "$toy_compose" 2>&1 || true)"
    if printf '%s' "$tc_out" | grep -q compose-ok; then tc_ok=1; break; fi
    if [ "$tc_try" -lt 3 ]; then sleep 2; fi
  done
  [ -n "$tc_ok" ] || fail "a toy 'docker compose up' did not run through the sidecar (compose v2 client-side) after 3 attempts. Last output tail:
$(printf '%s' "$tc_out" | tail -n 15)"
  pass "a toy 'docker compose up' runs through the sidecar (docker compose v2)"

  # (b) LIVE Decision-3 efficacy (deferred from M1, no daemon existed there).
  # ⛔ DO NOT DROP (PRD #97 M4 guard list — the full list is in the block above the
  # secret-hygiene phase). This is a DIFFERENT topology (rootless DinD sidecar) and it
  # reads a live daemon's mount namespace; no unit test can stand in for it.
  CANARY="$(rootdk cat "$DTOK" 2>/dev/null | tr -d '\r\n' || true)"
  [ -n "$CANARY" ] || fail "could not read the join-token canary from the agent — the Decision-3 assertion would be vacuous"
  # positive control: a sidecar container CAN read a file that IS in the DinD fs, so an
  # absent canary below reads as "not mounted", never "the exec path is broken".
  rootdk docker run --rm "$DIMG" cat /etc/hostname >/dev/null 2>&1 \
    || fail "positive control failed (a sidecar container could not read its OWN /etc/hostname)"
  pass "positive control: a sidecar container runs and reads its own fs"
  # attack matrix: bind-mount worker paths. Each `-v <src>` resolves <src> in the DinD
  # DAEMON's mount ns (which mounts NONE of them — Decision 3), so the token is absent.
  # Assert the canary value never appears (the mount is empty / the path is ENOENT there),
  # NOT a guardrail deny (we drive docker directly). A leak here = Decision 3 VIOLATED.
  for src in "$DTOK" /run / /data /nix; do
    OUT="$(rootdk docker run --rm -v "$src":/x "$DIMG" sh -c '
      cat /x 2>/dev/null
      cat /x/secrets/worker_token 2>/dev/null
      cat /x/run/secrets/worker_token 2>/dev/null
      cat /x/worker_token 2>/dev/null' 2>/dev/null || true)"
    printf '%s' "$OUT" | grep -qF "$CANARY" \
      && fail "Decision-3 VIOLATED: the join token leaked through 'docker run -v $src' (the DinD daemon mounted a worker path)"
  done
  pass "Decision-3: a sidecar container cannot read the join token via -v {token,/run,/,/data,/nix} — mount-ns isolation holds"

  # (3) The WORKER'S OWN path (the product path M2 exists to enable), NOT the -e DOCKER_HOST
  # exec bypass above: set UZI_DIND_SOCKET, recreate the agent, and assert the worker's
  # resolveDockerWiring auto-detects the live socket and its register reports
  # capabilities:["docker"]. Executor-independent (register+wiring are worker-level), so it
  # proves the real path under the stub. UZI_DIND_SOCKET marks the sidecar "expected", so the
  # keystone bounded-wait bridges any residual daemon-vs-worker start race.
  say "PRD #83 M2 (3): the worker self-detects the sidecar and reports the capability"
  printf 'UZI_DIND_SOCKET=/run/dind/docker.sock\n' >> "$ENVFILE"
  "${COMPOSE[@]}" up -d --no-deps --force-recreate agent >/dev/null 2>&1 \
    || fail "could not recreate the agent with UZI_DIND_SOCKET set"
  # Wait for the recreated worker to boot, probe (with the readiness wait), and log its wiring.
  #
  # 🔴 CAPTURE THEN MATCH IN BASH, never `compose logs agent | grep -q`. Same SIGPIPE defect
  # documented at the secret-hygiene corpus gate: under `set -euo pipefail`, `grep -q` exits
  # on the match, the writer upstream dies of SIGPIPE, and pipefail promotes that to the
  # pipeline's status — so a MATCH is reported as a failure whenever more than a pipe
  # buffer's worth of output FOLLOWS it (position, not total size; see that note).
  #
  # These sites sit squarely in the bad case, which is why they are not merely theoretical:
  # `docker_wired:true` and the register line are BOOT lines, so everything the agent logs
  # afterwards is data following the match — and the agent is the chattiest service here.
  # The false-red probability therefore RISES the longer the worker has been up, which is
  # the intermittency signature to expect if these ever regress.
  #
  # The CONSEQUENCE differs from the leak scan's, which is why it is worth spelling out
  # rather than cross-referencing. These are positive assertions (`… || fail`), so the bug
  # makes them fail SPURIOUSLY: the `&& break` below never fires, the loop burns its full 45s,
  # and the `|| fail` then reports a worker that had in fact wired itself correctly. A false
  # RED, and an intermittent one. The leak scan's `… && fail` failed the opposite way, silently
  # passing. Same mechanism, opposite outcome — do not assume one implies the other.
  #
  # ⚠️ THIS PATH IS NOT EXERCISED BY ANY RUN IN THIS BRANCH. It is inside the
  # `--profile agent-docker` block (PRD #83 M2), which is opt-in and off by default, so the
  # green e2e that accompanies this change never entered it. Verified by `bash -n`, by the
  # mechanism being identical to the one measured at the corpus gate, and by checking the
  # patterns below match the same literals the greps did — not by execution.
  det_end=$((SECONDS + 45))
  DIND_AGENT_LOGS=""
  while [ $SECONDS -lt $det_end ]; do
    DIND_AGENT_LOGS="$("${COMPOSE[@]}" logs agent 2>&1)"
    [[ "$DIND_AGENT_LOGS" == *'"docker_wired":true'* ]] && break
    sleep 1
  done
  # Quoted substrings are matched LITERALLY inside a `[[ == ]]` pattern, so the `[` and `]` of
  # capabilities:["docker"] are ordinary characters here and need none of the ERE escaping the
  # `grep -qE` form required.
  [[ "$DIND_AGENT_LOGS" == *'"docker_wired":true'* ]] \
    || fail "the worker did not self-detect the sidecar (no docker_wired:true) via UZI_DIND_SOCKET"
  [[ "$DIND_AGENT_LOGS" == *'"capabilities":["docker"]'* ]] \
    || fail 'the worker did not report capabilities:["docker"] at register'
  pass 'the worker self-detects the sidecar and registers capabilities:["docker"] (real product path, no DOCKER_HOST bypass)'
fi

