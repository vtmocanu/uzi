#!/usr/bin/env bash
# Advisory, offline reproduction of identifiable Go test failures in the caller's repo.
set -euo pipefail
command -v jq >/dev/null || { printf 'attribute-failures: jq is required\n' >&2; exit 2; }
# Escape rendering only; raw diagnostic evidence retains its original bytes.
render() {
  local escaped
  escaped=$(jq -aRs .)
  printf '%s' "${escaped:1:${#escaped}-2}"
}
die() { printf 'attribute-failures: %s\n' "$(printf %s "$*" | render)" >&2; exit 2; }
# 8 MiB per input/stream and 32 candidates bound retained evidence and runs.
# Read one extra byte to prove truncation, never slurp the original gate log.
LIMIT=$((8 * 1024 * 1024))
MAX_CANDIDATES=32
declare -A args=()
while (($#)); do
  case "$1" in --log|--base|--head) ;; *) die "unknown argument: $1";; esac
  key=$1
  [[ ! ${args[$key]+present} && $# -ge 2 && -n $2 ]] || die "duplicate or missing argument: $key"
  args[$key]=$2
  shift 2
done
[[ ${#args[@]} -eq 3 ]] || die 'required: --log <file> --base <ref> --head <ref>'
log=${args[--log]}
[[ $log == /* ]] || log="$PWD/$log"
[[ -f $log && -r $log ]] || die 'log is not a readable file'
repo=$(git rev-parse --show-toplevel 2>/dev/null) || die 'caller is not in a repository'
base=$(git -C "$repo" rev-parse --verify --end-of-options "${args[--base]}^{commit}" 2>/dev/null) || die 'base is not a commit'
head=$(git -C "$repo" rev-parse --verify --end-of-options "${args[--head]}^{commit}" 2>/dev/null) || die 'head is not a commit'
evidence=$(mktemp -d "${TMPDIR:-/tmp}/attribute-evidence.XXXXXX") || die 'cannot create evidence'
evidence=$(cd "$evidence" && pwd -P)
trees=
cleanup() {
  local side
  if [[ -n $trees ]]; then
    # Only these two paths belong to this invocation, including a partially added tree.
    for side in base head; do
      git -C "$repo" worktree remove --force "$trees/$side" >/dev/null 2>&1 || true
    done
    rm -rf -- "$trees"
  fi
}
trap cleanup EXIT
trap 'exit 2' HUP INT TERM
: > "$evidence/base.jsonl"
: > "$evidence/head.jsonl"
printf 'base JSON: %s\nhead JSON: %s\n' "$(printf %s "$evidence/base.jsonl" | render)" "$(printf %s "$evidence/head.jsonl" | render)"
printf 'Evidence is advisory, not merge authority.\n'
jq -n --arg base "$base" --arg head "$head" '{base:$base,head:$head}' > "$evidence/refs.json"
row() {
  # JSON strings keep even hostile names/reasons on one escaped, reviewable line.
  local value separator=
  for value in "$@"; do
    printf '%s"%s"' "$separator" "$(printf %s "$value" | render)"
    separator=$'\t'
  done
  printf '\n'
}
printf 'name\tmodule/package\tbase\thead\tverdict\treason\n'
head -c "$((LIMIT + 1))" -- "$log" > "$evidence/input.log" || die 'cannot read input log'
if [[ $(wc -c < "$evidence/input.log") -gt $LIMIT ]]; then
  printf '{"input_truncated":true}\n' > "$evidence/limits.json"
  row '<input>' '-' unknown unknown inconclusive 'input exceeds byte limit'
  exit 0
fi
jq -Rs '
  split("\n") | map(select(test("--- FAIL: "))) |
  map(sub("^.*?--- FAIL: "; "") | sub(" \\([0-9.]+s\\)\\r?$"; "") |
      {raw:., name:split("/")[0]} |
      .valid = ((.name | test("^Test[A-Za-z0-9_]*$")) and
                (.raw | contains(";") or contains("$(") or contains("\r") | not) and
                (. as $c | .raw == .name or (.raw | startswith($c.name + "/"))))) |
  unique_by(if .valid then .name else .raw end)
' "$evidence/input.log" > "$evidence/candidates.json"
if [[ $(jq length "$evidence/candidates.json") -gt $MAX_CANDIDATES ]]; then
  printf '{"candidate_limit_exceeded":true}\n' > "$evidence/limits.json"
  row '<candidates>' '-' unknown unknown inconclusive 'candidate limit exceeded'
  exit 0
fi
# Unsupported evidence is reported explicitly, even alongside supported Go failures.
if jq -eRs 'test("(?im)(vitest|node:|node --|ERR_TEST|not ok |eslint|golangci|lint|gofmt|fmt-check|go vet|vet:|\\[build failed\\])")' "$evidence/input.log" >/dev/null; then
  row '<unsupported>' '-' unknown unknown inconclusive 'unsupported failure evidence'
fi
declare -A groups=() sources=()
while IFS= read -r candidate; do
  valid=$(jq -r '.valid' <<< "$candidate")
  name=$(jq -r 'if .valid then .name else .raw end' <<< "$candidate")
  if [[ $valid != true ]]; then
    row "$name" '-' unknown unknown inconclusive 'malformed test candidate'
    continue
  fi
  matches=()
  while IFS= read -r -d '' file; do matches+=("$file"); done < <(
    git -C "$repo" grep -z -l -F "func $name(" "$head" -- ':(glob)api/**/*_test.go' ':(glob)controller/**/*_test.go' || true
  )
  if [[ ${#matches[@]} -ne 1 ]]; then
    row "$name" '-' unknown unknown inconclusive 'head definition absent or ambiguous'
    continue
  fi
  file=${matches[0]#"$head:"}
  sources["head:$name"]=$file
  module=${file%%/*}
  directory=${file%/*}
  pkg=${directory#"$module"}
  pkg=.${pkg}
  [[ $pkg != . ]] || pkg=./
  base_matches=()
  while IFS= read -r -d '' file; do base_matches+=("$file"); done < <(
    git -C "$repo" grep -z -l -F "func $name(" "$base" -- ":(glob)$directory/*_test.go" || true
  )
  if [[ ${#base_matches[@]} -ne 1 ]]; then
    row "$name" "$module/$pkg" unknown unknown inconclusive 'base definition absent or ambiguous in owning package'
    continue
  fi
  sources["base:$name"]=${base_matches[0]#"$base:"}
  group="$module/$pkg"
  groups[$group]="${groups[$group]-} $name"
done < <(jq -c '.[]' "$evidence/candidates.json")
if [[ $(jq 'length' "$evidence/candidates.json") -eq 0 ]]; then
  row '<none>' '-' unknown unknown inconclusive 'no identifiable Go tests'
fi
[[ ${#groups[@]} -gt 0 ]] || exit 0
trees=$(mktemp -d "${TMPDIR:-/tmp}/attribute-worktrees.XXXXXX") || die 'cannot create worktree container'
trees=$(cd "$trees" && pwd -P)
git -C "$repo" worktree add --detach "$trees/base" "$base" > "$evidence/worktrees.log" 2>&1 || die 'base worktree setup failed'
git -C "$repo" worktree add --detach "$trees/head" "$head" >> "$evidence/worktrees.log" 2>&1 || die 'head worktree setup failed'

# FIFO readers close at LIMIT+1 bytes. Wait for the producer and both readers
# before returning; no file-size limit reaches Go build artifacts.
capture() {
  local output=$1 directory=$2 producer_rc=0 out_rc=0 err_rc=0 out_pid err_pid
  shift 2
  mkfifo "$output.out.pipe" "$output.err.pipe"
  head -c "$((LIMIT + 1))" < "$output.out.pipe" > "$output" & out_pid=$!
  head -c "$((LIMIT + 1))" < "$output.err.pipe" > "$output.stderr" & err_pid=$!
  (cd "$directory" && GOFLAGS=-mod=readonly GOPROXY=off GOTOOLCHAIN=local "$@") \
    > "$output.out.pipe" 2> "$output.err.pipe" || producer_rc=$?
  wait "$out_pid" || out_rc=$?
  wait "$err_pid" || err_rc=$?
  rm -- "$output.out.pipe" "$output.err.pipe"
  CAPTURE_RC=$producer_rc
  CAPTURE_TRUNCATED=false
  [[ $(wc -c < "$output") -le $LIMIT && $(wc -c < "$output.stderr") -le $LIMIT &&
     $out_rc -eq 0 && $err_rc -eq 0 ]] || CAPTURE_TRUNCATED=true
  jq -n --argjson producer "$producer_rc" --argjson stdout_reader "$out_rc" \
    --argjson stderr_reader "$err_rc" --argjson truncated "$CAPTURE_TRUNCATED" \
    '{producer:$producer,stdout_reader:$stdout_reader,stderr_reader:$stderr_reader,truncated:$truncated}' > "$output.capture.json"
}
# Each independent package runs once per side, without retries; go test retains its
# default ten-minute test timeout. Failure never prevents sibling packages.
index=0
for group in "${!groups[@]}"; do
  index=$((index + 1))
  module=${group%%/*}
  pkg=${group#*/}
  read -r -a names <<< "${groups[$group]}"
  pattern='^('
  separator=
  for name in "${names[@]}"; do pattern+="$separator$name"; separator='|'; done
  pattern+=')$'
  for side in base head; do
    prefix="$evidence/$index-$side"
    cwd="$trees/$side/$module"
    capture "$prefix.version" "$cwd" go version
    version_rc=$CAPTURE_RC
    version_truncated=$CAPTURE_TRUNCATED
    argv=(go test -json -race -count=1 -run "$pattern" "$pkg")
    printf -v quoted '%q ' GOFLAGS=-mod=readonly GOPROXY=off GOTOOLCHAIN=local "${argv[@]}"
    capture "$prefix.jsonl" "$cwd" "${argv[@]}"
    rc=$CAPTURE_RC
    mv -- "$prefix.jsonl.stderr" "$prefix.stderr"
    cat "$prefix.jsonl" >> "$evidence/$side.jsonl"
    jq -n --arg sha "$(if [[ $side == base ]]; then printf %s "$base"; else printf %s "$head"; fi)" \
      --arg cwd "$cwd" --arg command "$quoted" --argjson status "$rc" --argjson version_status "$version_rc" \
      --argjson truncated "$CAPTURE_TRUNCATED" --argjson version_truncated "$version_truncated" \
      --rawfile version "$prefix.version" --rawfile version_stderr "$prefix.version.stderr" \
      --rawfile stderr "$prefix.stderr" \
      '{sha:$sha,cwd:$cwd,command:$command,env:{GOFLAGS:"-mod=readonly",GOPROXY:"off",GOTOOLCHAIN:"local"},
        version_command:"GOFLAGS=-mod=readonly GOPROXY=off GOTOOLCHAIN=local go version",
        truncated:$truncated,version_truncated:$version_truncated,status:$status,version_status:$version_status,version:$version,version_stderr:$version_stderr,stderr:$stderr}' > "$prefix.meta.json"
    # Invalid JSON, setup failure, skips and incomplete package execution invalidate all
    # selected tests in that run. Exit status supports completeness, never test attribution.
    # Setup failures use stderr/build/incomplete evidence; arbitrary diagnostic words
    # (including real race warnings) do not override complete test outcomes.
    if ! jq -s --slurpfile meta "$prefix.meta.json" '
      $meta[0] as $m | . as $events |
      length > 0 and all(.[];
        type == "object" and (.Package | type == "string") and
        (.Test == null or (.Test | type == "string")) and
        (.Output == null or (.Output | type == "string")) and
        (.Action as $a | ["start","run","pause","cont","pass","bench","fail","skip","output"] | index($a) != null)) and
      ([.[].Package] | unique | length) == 1 and
      ([.[] | select(.Test == null and (.Action == "pass" or .Action == "fail"))] | length) == 1 and
      all(.[] | select(.Test != null and .Action == "run");
        . as $start |
        ([$events[] | select(.Package == $start.Package and .Test == $start.Test and
          (.Action == "pass" or .Action == "fail"))] | length) == 1) and
      all(.[] | select(.Test != null and (.Action == "pass" or .Action == "fail" or .Action == "skip"));
        . as $terminal |
        ([$events[] | select(.Package == $terminal.Package and .Test == $terminal.Test and .Action == "run")] | length) == 1) and
      $m.truncated == false and $m.version_truncated == false and
      $m.version_status == 0 and $m.version_stderr == "" and $m.stderr == "" and
      ($m.status == 0 or $m.status == 1) and
      (any(.[]; .Test == null and (.Action == "pass" or .Action == "fail"))) and
      (if $m.status == 0 then any(.[]; .Test == null and .Action == "pass") and
                              (any(.[]; .Test != null and .Action == "fail") | not)
       else any(.[]; .Test == null and .Action == "fail") and
            any(.[]; .Test != null and .Action == "fail") end) and
      (any(.[]; .Action == "skip" or
        ((.Output // "") | test("(?m)^(panic:|fatal error:)"))) | not)
    ' "$prefix.jsonl" > "$prefix.valid" 2> "$prefix.parse.stderr"; then
      printf 'false\n' > "$prefix.valid"
    fi
  done
  for name in "${names[@]}"; do
    for side in base head; do
      prefix="$evidence/$index-$side"
      jq -s --arg name "$name" --arg root "$trees/$side" --arg source "${sources["$side:$name"]}" --slurpfile valid "$prefix.valid" '
        def selected: .Test == $name or ((.Test // "") | startswith($name + "/"));
        # Go logger first lines have four spaces; continuations have eight.
        # Normalize only the discovered definition filename or its full path.
        def source_prefix:
          . as $line |
          [($source | split("/")[-1]), ($root + "/" + $source)] |
          map(("    " + . + ":") as $prefix |
              select($line | startswith($prefix)) |
              select($line | ltrimstr($prefix) | test("^[0-9]+: ")) | $prefix) | .[0];
        def normalize:
          source_prefix as $prefix |
          (if $prefix != null then $prefix + "<line>: " + (ltrimstr($prefix) | sub("^[0-9]+: "; ""))
           else . end) |
          split($root) | join("<worktree>");
        def diagnostic: split("\n") | map(normalize) | join("\n");
        # Classify whole events in the full ordered stream, before selecting failures.
        # test2json emits run/cont before output, pause after output, and may defer
        # terminal reports. NAME is consumed by test2json, without either event.
        def marker:
          .Output // "" |
          ([capture("\\A=== (?<action>RUN  |PAUSE|CONT ) (?<test>[^\\r\\n]+)\\r?\\n\\z") |
            .action |= (gsub(" "; "") | ascii_downcase)] +
           [capture("\\A(?:    )*--- (?<action>PASS|FAIL|SKIP): (?<test>[^\\r\\n]+) \\((?<elapsed>[0-9]+\\.[0-9]+)s\\)\\r?\\n\\z") |
            .action |= ascii_downcase | .elapsed |= tonumber] +
           [capture("\\A(?<action>PASS|FAIL)\\r?\\n\\z") |
            .action |= ascii_downcase | .test = null]) | .[0];
        # Track output line fragments per test/package so a fragment tail cannot
        # masquerade as a complete lifecycle event.
        reduce (to_entries[]) as $entry ({events:[],pending:{}};
          $entry.value as $e | ([$e.Package, $e.Test] | tojson) as $key |
          .events += [$e + {ordinal:$entry.key,fragment:(.pending[$key] // false)}] |
          if $e.Action == "output" then
            .pending[$key] = (($e.Output // "") | endswith("\n") | not)
          else . end) | .events as $full |
        [$full[] | select(.Action == "output") | . + {marker:marker}] as $outputs |
        [$outputs[] | . as $o |
          . + {correlations: [
            $full[] | select($o.marker != null and $o.fragment == false and
              .Package == $o.Package and .Test == $o.Test and
              .Test == $o.marker.test and .Action == $o.marker.action) |
            select(if .Action == "run" or .Action == "cont" then
              .ordinal < $o.ordinal and
              (any($full[]; .Package == $o.Package and .Test == $o.Test and
                (.Action == "pass" or .Action == "fail" or .Action == "skip") and
                .ordinal < $o.ordinal) | not)
            elif .Action == "pause" then .ordinal > $o.ordinal
            else .ordinal > $o.ordinal end) |
            select(.Test == null or any($full[];
              .Package == $o.Package and .Test == $o.Test and .Action == "run" and
              .ordinal < $o.ordinal)) |
            .ordinal
          ]}] as $candidates |
        [$candidates[] | . as $o |
          (($o.correlations | length) == 1 and
            ([$candidates[] | select(.correlations | index($o.correlations[0]) != null)] | length) == 1 and
            (if $o.marker.action == "run" or $o.marker.action == "cont" then
               $o.correlations[0] == $o.ordinal - 1
             elif $o.marker.action == "pause" then $o.correlations[0] == $o.ordinal + 1
             else $o.marker.elapsed == null or
               $full[$o.correlations[0]].Elapsed == $o.marker.elapsed end)) as $noise |
          . + {noise:$noise,uncertain:(($noise | not) and
            (.marker != null or
             ((.Output // "") | test("\\A(=== (RUN|PAUSE|CONT|NAME)|--- (PASS|FAIL|SKIP):|PASS\\r?\\n\\z|FAIL\\r?\\n\\z)"))))}] as $classified |
        [ $full[] | select(selected) ] as $events |
        [$events[] | select(.Test == $name and (.Action == "pass" or .Action == "fail"))] as $terminal |
        [$events[] | select(.Action == "fail") | {package:.Package,test:.Test}] | unique as $failed |
        [$classified[] | select(selected and .Output != null and .noise == false) |
          . as $event | select(any($failed[]; .package == $event.Package and .test == $event.Test)) |
          {package:.Package,test:.Test,text:(.Output | diagnostic)} | select(.text != "")] as $diagnostics |
        {result:(if $valid[0] != true or ($terminal | length) != 1 or
                    ([$events[].Package] | unique | length) != 1 or
                    any($events[]; .Action == "skip") then "unknown" else $terminal[0].Action end),
         failed:$failed,diagnostics:$diagnostics,
         uncertainty:any($classified[]; selected and .uncertain and
           (. as $event | any($failed[]; .package == $event.Package and .test == $event.Test))),
         # Recognition is independent of normalization sentinels in assertion text.
         assertion:any($events[]; .Action == "output" and
           (. as $event | any($failed[]; .package == $event.Package and .test == $event.Test)) and
           any((.Output // "" | split("\n"))[]; source_prefix != null))}
      ' "$prefix.jsonl" > "$prefix-$name.result" 2>/dev/null || printf '{"result":"unknown","uncertainty":true}\n' > "$prefix-$name.result"
    done
    verdict=$(jq -nr --slurpfile b "$evidence/$index-base-$name.result" --slurpfile h "$evidence/$index-head-$name.result" '
      $b[0] as $b | $h[0] as $h |
      if $b.result == "pass" and $h.result == "fail" then ["pr-introduced","base passes, head fails"]
      elif $b.result == "pass" and $h.result == "pass" then ["not-reproduced","both pass"]
      elif $b.result == "fail" and $h.result == "pass" then ["inconclusive","base fails, head passes"]
      elif $b.result == "fail" and $h.result == "fail" then
        if $b.uncertainty != false or $h.uncertainty != false
        then ["inconclusive","uncertain lifecycle output in compared failure diagnostics"]
        elif $b.failed == $h.failed and $b.assertion and $h.assertion and
           ($b.diagnostics | length) > 0 and $b.diagnostics == $h.diagnostics
        then ["pre-existing","matching failing descendants and diagnostics"]
        else ["inconclusive","different failure or insufficient assertion diagnostics"] end
      else ["inconclusive","incomplete, skipped or invalid run evidence"] end |
      join("\t")
    ')
    IFS=$'\t' read -r classification reason <<< "$verdict"
    row "$name" "$group" "$(jq -r .result "$evidence/$index-base-$name.result")" \
      "$(jq -r .result "$evidence/$index-head-$name.result")" "$classification" "$reason"
  done
done
