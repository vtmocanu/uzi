#!/usr/bin/env bash
# Advisory, offline reproduction of identifiable Go test failures in the caller's repo.
set -euo pipefail
die() { printf 'attribute-failures: %s\n' "$*" >&2; exit 2; }
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
command -v jq >/dev/null || die 'jq is required'
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
printf 'base JSON: %s\nhead JSON: %s\n' "$evidence/base.jsonl" "$evidence/head.jsonl"
printf 'Evidence is advisory, not merge authority.\n'
jq -n --arg base "$base" --arg head "$head" '{base:$base,head:$head}' > "$evidence/refs.json"
row() {
  # JSON strings keep even hostile names/reasons on one escaped, reviewable line.
  jq -nr --args '$ARGS.positional | map(tojson) | join("\t")' -- "$@"
}
printf 'name\tmodule/package\tbase\thead\tverdict\treason\n'
jq -Rs '
  split("\n") | map(select(test("--- FAIL: "))) |
  map(sub("^.*?--- FAIL: "; "") | sub(" \\([0-9.]+s\\)\\r?$"; "") |
      {raw:., name:split("/")[0]} |
      .valid = ((.name | test("^Test[A-Za-z0-9_]*$")) and
                (.raw | contains(";") or contains("$(") or contains("\r") | not) and
                (. as $c | .raw == .name or (.raw | startswith($c.name + "/"))))) |
  unique_by(if .valid then .name else .raw end)
' "$log" > "$evidence/candidates.json"
# Unsupported evidence is reported explicitly, even alongside supported Go failures.
if jq -eRs 'test("(?im)(vitest|node:|node --|ERR_TEST|not ok |eslint|golangci|lint|gofmt|fmt-check|go vet|vet:|\\[build failed\\])")' "$log" >/dev/null; then
  row '<unsupported>' '-' unknown unknown inconclusive 'unsupported failure evidence'
fi
declare -A groups=()
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
    rc=0
    (cd "$cwd" && GOFLAGS=-mod=readonly GOPROXY=off GOTOOLCHAIN=local go version) > "$prefix.version" 2> "$prefix.version.stderr" || rc=$?
    version_rc=$rc
    argv=(go test -json -race -count=1 -run "$pattern" "$pkg")
    printf -v quoted '%q ' GOFLAGS=-mod=readonly GOPROXY=off GOTOOLCHAIN=local "${argv[@]}"
    rc=0
    (cd "$cwd" && GOFLAGS=-mod=readonly GOPROXY=off GOTOOLCHAIN=local "${argv[@]}") > "$prefix.jsonl" 2> "$prefix.stderr" || rc=$?
    cat "$prefix.jsonl" >> "$evidence/$side.jsonl"
    jq -n --arg sha "$(if [[ $side == base ]]; then printf %s "$base"; else printf %s "$head"; fi)" \
      --arg cwd "$cwd" --arg command "$quoted" --argjson status "$rc" --argjson version_status "$version_rc" \
      --rawfile version "$prefix.version" --rawfile version_stderr "$prefix.version.stderr" \
      --rawfile stderr "$prefix.stderr" \
      '{sha:$sha,cwd:$cwd,command:$command,env:{GOFLAGS:"-mod=readonly",GOPROXY:"off",GOTOOLCHAIN:"local"},
        version_command:"GOFLAGS=-mod=readonly GOPROXY=off GOTOOLCHAIN=local go version",
        status:$status,version_status:$version_status,version:$version,version_stderr:$version_stderr,stderr:$stderr}' > "$prefix.meta.json"
    # Invalid JSON, setup failure, skips and incomplete package execution invalidate all
    # selected tests in that run. Exit status supports completeness, never test attribution.
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
      $m.version_status == 0 and $m.version_stderr == "" and $m.stderr == "" and
      ($m.status == 0 or $m.status == 1) and
      (any(.[]; .Test == null and (.Action == "pass" or .Action == "fail"))) and
      (if $m.status == 0 then any(.[]; .Test == null and .Action == "pass") and
                              (any(.[]; .Test != null and .Action == "fail") | not)
       else any(.[]; .Test == null and .Action == "fail") and
            any(.[]; .Test != null and .Action == "fail") end) and
      (any(.[]; .Action == "skip" or
        ((.Output // "") | test("(?i)(panic:|fatal error:|timed out|DATA RACE|race detector|build failed|setup failed|no required module|toolchain|GOPROXY=off|cannot find package)"))) | not)
    ' "$prefix.jsonl" > "$prefix.valid" 2> "$prefix.parse.stderr"; then
      printf 'false\n' > "$prefix.valid"
    fi
  done
  for name in "${names[@]}"; do
    for side in base head; do
      prefix="$evidence/$index-$side"
      jq -s --arg name "$name" --arg root "$trees/$side" --slurpfile valid "$prefix.valid" '
        def selected: .Test == $name or ((.Test // "") | startswith($name + "/"));
        def normalize:
          split($root) | join("<worktree>") |
          gsub("^(?<file>\\s*(?:<worktree>/)?[A-Za-z0-9_./-]+\\.go):[0-9]+(?=:)"; "\(.file):<line>");
        def diagnostic:
          split("\n") | map(select(length > 0) |
            select(test("^\\s*(=== (RUN|PAUSE|CONT|NAME)\\s+Test[A-Za-z0-9_]*(/.*)?|--- (PASS|FAIL|SKIP): Test[A-Za-z0-9_]*(/.*)? \\([0-9.]+s\\)|PASS|FAIL)$") | not) |
            normalize) | join("\n");
        [ .[] | select(selected) ] as $events |
        [$events[] | select(.Test == $name and (.Action == "pass" or .Action == "fail"))] as $terminal |
        [$events[] | select(.Action == "fail") | {package:.Package,test:.Test}] | unique as $failed |
        [$events[] | select(.Action == "output" and .Output != null) |
          . as $event | select(any($failed[]; .package == $event.Package and .test == $event.Test)) |
          {package:.Package,test:.Test,text:(.Output | diagnostic)} | select(.text != "")] as $diagnostics |
        {result:(if $valid[0] != true or ($terminal | length) != 1 or
                    ([$events[].Package] | unique | length) != 1 or
                    any($events[]; .Action == "skip") then "unknown" else $terminal[0].Action end),
         failed:$failed,diagnostics:$diagnostics,
         assertion:any($diagnostics[]; .text | test("(?i)(\\.go:<line>|assert|error|expected|want|got|fatal)"))}
      ' "$prefix.jsonl" > "$prefix-$name.result" 2>/dev/null || printf '{"result":"unknown"}\n' > "$prefix-$name.result"
    done
    verdict=$(jq -nr --slurpfile b "$evidence/$index-base-$name.result" --slurpfile h "$evidence/$index-head-$name.result" '
      $b[0] as $b | $h[0] as $h |
      if $b.result == "pass" and $h.result == "fail" then ["pr-introduced","base passes, head fails"]
      elif $b.result == "pass" and $h.result == "pass" then ["not-reproduced","both pass"]
      elif $b.result == "fail" and $h.result == "pass" then ["inconclusive","base fails, head passes"]
      elif $b.result == "fail" and $h.result == "fail" then
        if $b.failed == $h.failed and $b.assertion and $h.assertion and
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
