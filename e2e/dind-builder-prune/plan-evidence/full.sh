#!/usr/bin/env bash
# Spike #4291: production argv `docker builder prune -f --filter type=regular --filter until=24h --max-used-space 4GiB`
# exercised on an OWNED nested daemon. Scaled variants are labelled SCALED.
set -u
source "$(dirname "$0")/lib.sh"
FLOOR=30s
log "posture=$POSTURE container=$NAME"
start_daemon || { log "BLOCKED: daemon did not start"; exit 2; }
versions
mk() { mkdir -p "$OUT/ctx/$1"; printf '%b' "$2" > "$OUT/ctx/$1/Dockerfile"; outer 20 cp "$OUT/ctx/$1" "$NAME:/spike/ctx/$1" || log "cp ctx $1 failed"; }
recs() { # label -> full JSON + tsv table
  cli 30 system df -v --format json > "$OUT/df-$1.json"
  echo "--- records [$1] (ID type shared inuse size lastUsed(UTC) created description)"
  jq -r '.BuildCache[]|[.ID,.CacheType,"shared="+.Shared,"inuse="+.InUse,.Size,.LastUsedAt[11:23],.CreatedAt[11:23],(.Description|.[0:70])]|@tsv' "$OUT/df-$1.json" | sort -k6
  cli 30 buildx du > "$OUT/du-$1.txt" 2>&1; echo "du[$1] $(grep -E '^(Total|Reclaimable|Shared|Private)' "$OUT/du-$1.txt" | tr -s '\t' ' ' | tr '\n' ' ')"
}
prune() { local label=$1; shift
  echo ">>> PRUNE[$label] argv: docker builder prune $*"
  cli 120 builder prune "$@" > "$OUT/prune-$label.out" 2>&1; local rc=$?
  sed 's/^/    | /' "$OUT/prune-$label.out"; echo ">>> PRUNE[$label] rc=$rc"
}
bytes() { awk -v s="$1" 'BEGIN{n=s+0; u=s; sub(/^[0-9.]+/,"",u); m=1; if(u=="kB")m=1e3; else if(u=="MB")m=1e6; else if(u=="GB")m=1e9; printf "%d", n*m}'; }
build() { local tag=$1; shift; local t=$SECONDS; cli 240 build --progress=quiet "$@" > "$OUT/build-$tag.out" 2>&1; local rc=$?; log "build[$tag] rc=$rc (${SECONDS}-${t}s)"; [[ $rc == 0 ]] || tail -20 "$OUT/build-$tag.out"; return $rc; }
dd1() { printf 'dd if=/dev/urandom of=%s bs=1M count=%s' "$1" "$2"; }
B="FROM $FIXTURE_IMAGE"

echo "===== CHECK 1a: EXACT production argv on a fresh-cache daemon"
recs fresh
prune exact-fresh -f --filter type=regular --filter until=24h --max-used-space 4GiB

echo "===== PHASE A fixtures (no image tagged; base not pre-pulled)"
mk held "$B\nRUN echo spike-held && $(dd1 /h 1)\n"
mk hold "$B\nRUN echo spike-held && $(dd1 /h 1)\nRUN echo spike-hold && sleep 80\n"
mk ctrl "$B\nRUN echo spike-ctrlold && $(dd1 /c 1)\n"
mk recent "$B\nRUN echo spike-recent && $(dd1 /r 1)\n"
mk cm "$B\nRUN --mount=type=cache,target=/c,id=spikecm echo spike-cm && $(dd1 /c/f 2)\n"
mk young "$B\nRUN echo spike-young && $(dd1 /y 1)\n"
mk capf "$B\nRUN echo spike-capF && $(dd1 /f 2)\n"
mk capg "$B\nRUN echo spike-capG && $(dd1 /g 2)\n"
build held --output type=cacheonly /spike/ctx/held
build ctrl --output type=cacheonly /spike/ctx/ctrl
build recent --output type=cacheonly /spike/ctx/recent
build cm --output type=cacheonly /spike/ctx/cm
recs built
hs=$SECONDS
( cli 240 build --progress=plain --output type=cacheonly /spike/ctx/hold > "$OUT/build-hold.out" 2>&1; echo "HOLD_BUILD_RC=$?" > "$OUT/hold.rc" ) &
holdpid=$!
log "held build started pid=$holdpid"
for ((i = 0; i < 40; i++)); do grep -q 'spike-hold' "$OUT/build-hold.out" 2>/dev/null && break; sleep 1; done
log "held build reached RUN sleep step: $(grep -m1 -E 'CACHED|spike-hold' "$OUT/build-hold.out")"
grep -E 'CACHED' "$OUT/build-hold.out" | sed 's/^/    hold-log| /'
recs during-hold-early
while ((SECONDS < hs + 37)); do sleep 1; done
build recent-bump --output type=cacheonly /spike/ctx/recent
build young --output type=cacheonly /spike/ctx/young
while ((SECONDS < hs + 45)); do sleep 1; done
recs before-P1
echo "===== CHECK 1b: EXACT production argv with data present (all records < 24h, usage << 4GiB)"
prune exact-loaded -f --filter type=regular --filter until=24h --max-used-space 4GiB
recs after-exact-loaded
echo "===== CHECKS 1,2,3,4,6a: SCALED prune (until=$FLOOR, cap=1MB below usage) while hold is running"
kill -0 "$holdpid" 2>/dev/null && log "hold build still running at prune time (t=+$((SECONDS-hs))s since hold start)"
prune P1-scaled -f --filter type=regular --filter until=$FLOOR --max-used-space 1MB
recs after-P1
wait "$holdpid"; cat "$OUT/hold.rc"; tail -4 "$OUT/build-hold.out" | sed 's/^/    hold-log| /'
recs after-hold
echo "===== CHECK 2 follow-up: same SCALED prune after the hold released"
prune P1b-scaled -f --filter type=regular --filter until=$FLOOR --max-used-space 1MB
recs after-P1b

echo "===== CHECK 6b/6c: cap behaviour"
build capF --output type=cacheonly /spike/ctx/capf
sleep 3
build capG --output type=cacheonly /spike/ctx/capg
cs=$SECONDS; while ((SECONDS < cs + 33)); do sleep 1; done
recs before-P2
prune P2-capabove -f --filter type=regular --filter until=$FLOOR --max-used-space 4GiB
recs after-P2
tot=$(grep -E '^Total' "$OUT/du-after-P2.txt" | awk '{print $2}'); totb=$(bytes "$tot"); cap=$((totb - 1000000))
log "du Total=$tot (~${totb} bytes); P3 cap=${cap} bytes"
prune P3-capjustbelow -f --filter type=regular --filter until=$FLOOR --max-used-space "$cap"
recs after-P3

echo "===== CHECK 5: tagged image (PHASE B)"
mk keep "$B\nRUN echo spike-keep > /proof\n"
mkdir -p "$OUT/ctx/scr"; head -c 1048576 /dev/urandom > "$OUT/ctx/scr/blob"; printf 'FROM scratch\nCOPY blob /blob\n' > "$OUT/ctx/scr/Dockerfile"; outer 20 cp "$OUT/ctx/scr" "$NAME:/spike/ctx/scr"
mk ctrl2 "$B\nRUN echo spike-ctrl2 && $(dd1 /c2 1)\n"
build keep -t spike/keep /spike/ctx/keep
build scr --output type=cacheonly /spike/ctx/scr
build ctrl2 --output type=cacheonly /spike/ctx/ctrl2
cli 10 image ls -a
ks=$SECONDS; while ((SECONDS < ks + 33)); do sleep 1; done
recs before-P4
prune P4-scaled -f --filter type=regular --filter until=$FLOOR --max-used-space 1MB
recs after-P4
cli 10 image ls spike/keep; echo "IMAGE_LS_RC=$?"
cli 60 run --rm spike/keep cat /proof; echo "RUN_KEEP_RC=$?"
cli 60 run --rm spike/keep true; echo "RUN_TRUE_RC=$?"
echo "===== EXACT production argv, final"
prune exact-final -f --filter type=regular --filter until=24h --max-used-space 4GiB
log "posture $POSTURE done in ${SECONDS}s"
