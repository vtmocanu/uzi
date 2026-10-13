#!/usr/bin/env bash
# Hermetic public-CLI fixtures: no real Go executable or network is used.
set -euo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT="$HERE/attribute-failures.sh"
ROOT=$(cd "$HERE/../../../.." && pwd)
mkdir -p "$ROOT/.uzi/scratch"
WORK=$(mktemp -d "$ROOT/.uzi/scratch/attribute-fixtures.XXXXXX")
trap 'rm -rf -- "$WORK"' EXIT
trap 'exit 1' HUP INT TERM
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
mkdir -p "$WORK/bin" "$WORK/tmp" "$WORK/repo"
export TMPDIR="$WORK/tmp"
export REAL_GIT
REAL_GIT=$(command -v git)
git -C "$WORK/repo" init -q
git -C "$WORK/repo" config user.name fixture
git -C "$WORK/repo" config user.email fixture@example.com
mkdir -p "$WORK/repo/api/pkg" "$WORK/repo/controller"
printf 'module fixture/api\n\ngo 1.22\n' > "$WORK/repo/api/go.mod"
printf 'module fixture/controller\n\ngo 1.22\n' > "$WORK/repo/controller/go.mod"
printf 'package pkg\nfunc TestAlpha(t *testing.T) {}\nfunc TestBeta(t *testing.T) {}\n' > "$WORK/repo/api/pkg/a_test.go"
printf 'package root\nfunc TestRoot(t *testing.T) {}\n' > "$WORK/repo/controller/root_test.go"
printf 'package root\nfunc TestAPIRoot(t *testing.T) {}\n' > "$WORK/repo/api/root_test.go"
printf 'package pkg\nfunc TestBaseAmbiguous(t *testing.T) {}\n' > "$WORK/repo/api/pkg/amb1_test.go"
printf 'package pkg\nfunc TestBaseAmbiguous(t *testing.T) {}\n' > "$WORK/repo/api/pkg/amb2_test.go"
git -C "$WORK/repo" add api controller
git -C "$WORK/repo" commit -qm base
BASE=$(git -C "$WORK/repo" rev-parse HEAD)
rm "$WORK/repo/api/pkg/amb2_test.go"
printf 'package pkg\nfunc TestAbsent(t *testing.T) {}\nfunc TestAmbiguous(t *testing.T) {}\n' > "$WORK/repo/api/pkg/new_test.go"
printf 'package other\nfunc TestAmbiguous(t *testing.T) {}\n' > "$WORK/repo/api/other_test.go"
printf 'head\n' > "$WORK/repo/revision"
git -C "$WORK/repo" add api revision
git -C "$WORK/repo" commit -qm head
HEAD_SHA=$(git -C "$WORK/repo" rev-parse HEAD)
ORIGINAL_HEAD=$HEAD_SHA
git -C "$WORK/repo" mv api/pkg/a_test.go api/pkg/renamed_test.go
git -C "$WORK/repo" commit -qm renamed
RENAMED_HEAD=$(git -C "$WORK/repo" rev-parse HEAD)
export BASE HEAD_SHA WORK
cat > "$WORK/bin/go" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
[[ $GOFLAGS == -mod=readonly && $GOPROXY == off && $GOTOOLCHAIN == local ]] || exit 97
sha=$("$REAL_GIT" rev-parse HEAD)
[[ $sha == "$BASE" || $sha == "$HEAD_SHA" ]] || exit 97
[[ $("$REAL_GIT" rev-parse --show-toplevel) != "$WORK/repo" ]] || exit 97
module=${PWD##*/}
[[ $module == api || $module == controller ]] || exit 97
jq -nc --arg sha "$sha" --arg cwd "$PWD" --arg flags "$GOFLAGS" --arg proxy "$GOPROXY" \
  --arg toolchain "$GOTOOLCHAIN" --args '{sha:$sha,cwd:$cwd,flags:$flags,proxy:$proxy,toolchain:$toolchain,argv:$ARGS.positional}' \
  -- "$@" >> "$WORK/invocations"
if [[ $1 == version ]]; then
  [[ $# -eq 1 ]] || exit 97
  if [[ $MODE == toolchain ]]; then echo 'go: toolchain unavailable offline' >&2; exit 1; fi
  if [[ $MODE == oversized_version* ]]; then
    if [[ $MODE == oversized_version_stderr ]]; then
      head -c 16777217 /dev/zero | tr '\000' x >&2
    else
      head -c 16777217 /dev/zero | tr '\000' x
    fi
    exit 0
  fi
  echo 'go version fixture linux/amd64'
  exit 0
fi
[[ $# -eq 7 && $1 == test && $2 == -json && $3 == -race && $4 == -count=1 && $5 == -run ]] || exit 97
[[ $6 == "$EXPECTED_PATTERN" && $7 == "$EXPECTED_PKG" ]] || exit 97
[[ $module == "$EXPECTED_MODULE" || $MODE == multi ]] || exit 97
package="fixture/$module/${7#./}"
if [[ $MODE == oversized_stdout || $MODE == oversized_stderr ]]; then
  if [[ $MODE == oversized_stderr ]]; then head -c 16777217 /dev/zero | tr '\000' x >&2
  else head -c 16777217 /dev/zero | tr '\000' x; fi
  exit 1
fi
if [[ $MODE == invalid ]]; then printf 'not JSON\n'; exit 1; fi
if [[ $MODE == truncated ]]; then
  printf '{"Action":"run","Package":"fixture/api/pkg","Test":"TestAlpha"}\n{"Action":'
  exit 1
fi
if [[ $MODE == dependency || $MODE == race_setup ]]; then
  printf 'offline dependency or race setup unavailable\n' >&2
  exit 1
fi
event() {
  local output=${3-}
  # Legacy callers provide whole lines. Fragment fixtures opt out explicitly.
  if [[ $1 == output && $output != *$'\n' && ${4-} != fragment ]]; then output+=$'\n'; fi
  jq -nc --arg action "$1" --arg package "$package" --arg test "$2" --arg output "$output" \
    --argjson elapsed "${duration:-0}" '
    {Action:$action,Package:$package} + (if $test == "" then {} else {Test:$test} end) +
      (if $output == "" then {} else {Output:$output} end) +
      (if $action == "pass" or $action == "fail" or $action == "skip" then {Elapsed:$elapsed} else {} end)'
}
if [[ $MODE == build ]]; then event output '' 'FAIL fixture [build failed]'; event fail ''; exit 1; fi
names=(TestAlpha)
[[ $MODE == batch ]] && names=(TestAlpha TestBeta)
[[ $EXPECTED_MODULE == controller ]] && names=(TestRoot)
[[ $MODE == api_root ]] && names=(TestAPIRoot)
any_fail=false
for name in "${names[@]}"; do
  outcome=fail
  duration=0.01
  [[ $sha == "$HEAD_SHA" ]] && duration=0.09
  case "$MODE" in
    introduced|race_introduced|diagnostic_keywords|batch|controller|api_root|subselect|no_run_introduced|selected_no_run_introduced|child_no_run_introduced|lifecycle_uncertain_introduced) [[ $sha == "$BASE" ]] && outcome=pass ;;
    pass|livedb) outcome=pass ;;
    base_fail) [[ $sha == "$HEAD_SHA" ]] && outcome=pass ;;
    skip) outcome=skip ;;
  esac
  if [[ $MODE != no_run* && $MODE != selected_no_run* ||
        $MODE == *_introduced && $sha == "$BASE" ]]; then event run "$name"; fi
  if [[ $MODE == lifecycle_noise || $MODE == lifecycle_deferred ]]; then
    event output "$name" "=== RUN   $name"$'\n'
  fi
  if [[ $MODE == livedb ]]; then
    event run "$name/LiveDB"
    event skip "$name/LiveDB"
  fi
  case "$MODE" in
    panic) event output "$name" 'panic: fixture crashed' ;;
    timeout) event output "$name" 'panic: test timed out after 10m0s' ;;
    race_warning|race_introduced) [[ $outcome != fail ]] || event output "$name" 'WARNING: DATA RACE' ;;
  esac
  if [[ $outcome == fail ]]; then
    any_fail=true
    child="$name/child"
    [[ $MODE == subtests && $sha == "$HEAD_SHA" ]] && child="$name/other"
    [[ $MODE == no_run* || $MODE == child_no_run* ]] || event run "$child"
    [[ $MODE != duplicate_run ]] || event run "$child"
    if [[ $MODE == lifecycle_noise || $MODE == lifecycle_deferred ]]; then
      event output "$child" "=== RUN   $child"$'\n'
      event output "$child" "=== PAUSE $child"$'\n'
      event pause "$child"
      event cont "$child"
      event output "$child" "=== CONT  $child"$'\n'
    fi
    if [[ $MODE != markers ]]; then
      root=$("$REAL_GIT" rev-parse --show-toplevel)
      line=12
      [[ $sha == "$HEAD_SHA" ]] && line=87
      value=41
      [[ $MODE == changed && $sha == "$HEAD_SHA" ]] && value=42
      if [[ $MODE == lifecycle_multiline_terminal || $MODE == lifecycle_split_terminal || $MODE == lifecycle_fragment_tail ]]; then
        value=0.02
        [[ $sha == "$HEAD_SHA" ]] && value=0.03
        if [[ $MODE == lifecycle_multiline_terminal ]]; then
          event output "$child" "$(printf '    a_test.go:%s: expected output:\n        --- FAIL: %s (%ss)\n' "$line" "$child" "$value")"$'\n'
        elif [[ $MODE == lifecycle_fragment_tail ]]; then
          event output "$child" "    a_test.go:$line: expected output: " fragment
          event output "$child" "--- FAIL: $child (${value}s)"$'\n' fragment
        else
          event output "$child" "$(printf '    a_test.go:%s: expected output:\n' "$line")"$'\n'
          event output "$child" "        --- FAIL: $child (${value}s)"$'\n'
        fi
      elif [[ $MODE == lifecycle_regression || $MODE == lifecycle_multiline || $MODE == lifecycle_fragmented ]]; then
        value=TestOne
        [[ $sha == "$HEAD_SHA" ]] && value=TestTwo
        if [[ $MODE == lifecycle_multiline ]]; then
          event output "$child" "$(printf '    a_test.go:%s: expected output:\n        === RUN   %s\n' "$line" "$value")"$'\n'
        else
          event output "$child" "$(printf '    a_test.go:%s: expected output:\n' "$line")"$'\n'
          if [[ $MODE == lifecycle_fragmented ]]; then
            event output "$child" '        === RU' fragment
            event output "$child" "N   $value"$'\n' fragment
          else
            event output "$child" "        === RUN   $value"$'\n'
          fi
        fi
      elif [[ $MODE == continuation* ]]; then
        value=41
        [[ $sha == "$HEAD_SHA" ]] && value=42
        source=file.go
        [[ $MODE == *test* ]] && source=a_test.go
        continuation="$source:$value: got different path"
        [[ $MODE == *indent* ]] && continuation="        $continuation"
        if [[ $MODE == *split* ]]; then
          event output "$child" "    a_test.go:$line: expected path"
          event output "$child" "$continuation"
        else
          event output "$child" "$(printf '    a_test.go:%s: expected path\n%s' "$line" "$continuation")"
        fi
      elif [[ $MODE == unknown_location ]]; then
        event output "$child" '    unknown_test.go:12: expected 7 got 41'
      elif [[ $MODE == location_value ]]; then
        value=41
        [[ $sha == "$HEAD_SHA" ]] && value=42
        event output "$child" "    a_test.go:$line: expected file.go:$value: got different path"
      elif [[ $MODE == diagnostic_keywords ]]; then
        event output "$child" "    a_test.go:$line: expected toolchain/GOPROXY=off/build failed/setup failed/cannot find package/timed out/panic: values unchanged"
      elif [[ $MODE == body_root ]]; then
        event output "$child" "    a_test.go:$line: expected path $root/api/pkg/example"
      else
        source_file=a_test.go
        [[ $MODE != renamed || $sha != "$HEAD_SHA" ]] || source_file=renamed_test.go
        event output "$child" "    $root/$module/pkg/$source_file:$line: expected 7 got $value"
      fi
    fi
    # Each uncertain case has complete typed outcomes and a recognized assertion.
    if [[ $MODE == lifecycle_uncertain_both ||
          $MODE == lifecycle_uncertain_base && $sha == "$BASE" ||
          $MODE == lifecycle_uncertain_head && $sha == "$HEAD_SHA" ||
          $MODE == lifecycle_uncertain_introduced ]]; then
      event output "$child" "=== NAME  $child"$'\n'
    fi
    if [[ $MODE == lifecycle_competing ]]; then
      event output "$child" "        --- FAIL: $child (0.02s)"$'\n'
    fi
    if [[ $MODE == lifecycle_wrong_order ]]; then
      event output "$child" "=== RUN   $child"$'\n'
    fi
    # Legacy verbose conversion defers the parent's typed report past its child.
    if [[ $MODE == lifecycle_deferred ]]; then
      event output "$name" "--- FAIL: $name (${duration}s)"$'\n'
    fi
    if [[ $MODE == passing_diagnostic ]]; then
      event output "$child" "    --- PASS: $child (${duration}s)"
      event pass "$child"
      event output "$name" 'setup complete'
    else
      event output "$child" "    --- FAIL: $child (${duration}s)"
      [[ $MODE == child_missing ]] || event fail "$child"
    fi
    [[ $MODE == lifecycle_deferred ]] || event output "$name" "--- FAIL: $name (${duration}s)"
  fi
  [[ $MODE == missing ]] || event "$outcome" "$name"
  if [[ $MODE == lifecycle_wrong_order ]]; then
    event output "$name" "--- FAIL: $name (${duration}s)"$'\n'
  fi
done
if [[ $MODE == contradictory ]]; then event pass ''; exit 0; fi
if $any_fail; then event fail ''; exit 1; else event pass ''; fi
STUB
cat > "$WORK/bin/git" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
if [[ ${3-} == worktree && ${4-} == add ]]; then
  [[ ${MODE-} != before_head || ${6-} != */head ]] || exit 1
  [[ ${MODE-} != partial_base || ${6-} != */base ]] || { "$REAL_GIT" "$@"; exit 1; }
fi
if [[ ${MODE-} == partial && ${3-} == worktree && ${4-} == add && ${6-} == */head ]]; then
  # Register and populate the head, then report failure: cleanup must remove it too.
  "$REAL_GIT" "$@"
  exit 1
fi
exec "$REAL_GIT" "$@"
STUB
chmod +x "$WORK/bin/go" "$WORK/bin/git"
export PATH="$WORK/bin:$PATH"
export EXPECTED_PATTERN='^(TestAlpha)$' EXPECTED_PKG=./pkg EXPECTED_MODULE=api
export MODE
cd "$WORK/repo"

clean_trees() {
  [[ $(git worktree list --porcelain | jq -Rs 'split("\n") | map(select(startswith("worktree "))) | length') -eq 1 ]] ||
    fail "$MODE leaked a registered worktree"
  local path
  for path in "$TMPDIR"/attribute-worktrees.*; do
    [[ ! -e $path ]] || fail "$MODE leaked worktree container"
  done
}
run() {
  MODE=$1
  : > "$WORK/invocations"
  printf '%s\n' "${2---- FAIL: TestAlpha (0.01s)}" > "$WORK/failure.log"
  rc=0
  "$SCRIPT" --log "$WORK/failure.log" --base "$BASE" --head "$HEAD_SHA" > "$WORK/out" 2> "$WORK/err" || rc=$?
  [[ $rc == 0 ]] || fail "$MODE exit $rc: $(<"$WORK/err")"
  clean_trees
  # Both aggregate logs must survive tree cleanup, including unmappable/no-test cases.
  evidence_paths=()
  while IFS= read -r line; do
    case "$line" in 'base JSON: '*|'head JSON: '*) evidence_paths+=("${line#*: }");; esac
  done < "$WORK/out"
  [[ ${#evidence_paths[@]} == 2 ]] || fail "$MODE missing evidence paths"
  local path
  for path in "${evidence_paths[@]}"; do
    [[ $path == "$WORK/tmp/"* && -f $path && -r $path ]] || fail "$MODE evidence was removed or not absolute"
  done
  if [[ -s $WORK/invocations ]]; then
    jq -es --arg base "$BASE" --arg head "$HEAD_SHA" --arg pattern "$EXPECTED_PATTERN" \
      --arg pkg "$EXPECTED_PKG" --arg module "$EXPECTED_MODULE" '
      length == 4 and
      all(.[]; (.sha == $base or .sha == $head) and .flags == "-mod=readonly" and
        .proxy == "off" and .toolchain == "local" and
        (.cwd | endswith("/" + $module)) and
        (.argv == ["version"] or .argv == ["test","-json","-race","-count=1","-run",$pattern,$pkg])) and
      ([.[] | select(.argv == ["version"])] | length) == 2 and
      ([.[] | select(.sha == $base)] | length) == 2
    ' "$WORK/invocations" >/dev/null || fail "$MODE incorrect invocation contract"
    local metadata
    metadata="${evidence_paths[0]%/*}/1-base.meta.json"
    jq -e --arg sha "$BASE" '
      .sha == $sha and .status >= 0 and .version_status >= 0 and
      (.command | contains("GOFLAGS=-mod=readonly GOPROXY=off GOTOOLCHAIN=local go test -json -race -count=1 -run")) and
      (.version_command | contains("GOPROXY=off GOTOOLCHAIN=local go version")) and
      (.cwd | endswith("/base/api") or endswith("/base/controller"))
    ' "$metadata" >/dev/null || fail "$MODE missing run metadata"
  fi
}
has() {
  [[ $(<"$WORK/out") == *"$1"* ]] || fail "$MODE missing $1: $(<"$WORK/out")"
}
verdict() { has "\"$1\""; }
no_go() { [[ ! -s $WORK/invocations ]] || fail "$MODE unexpectedly ran go"; }

for mode in lifecycle_regression lifecycle_multiline lifecycle_fragmented; do
  run "$mode"; verdict inconclusive; has 'different failure'
  directory=${evidence_paths[0]%/*}
  for side in base head; do
    value=TestOne
    [[ $side == head ]] && value=TestTwo
    jq -e --arg value "$value" --arg mode "$mode" '
      .result == "fail" and .assertion and .uncertainty == false and
      .diagnostics == (
        [{package:"fixture/api/pkg",test:"TestAlpha/child",
          text:"    a_test.go:<line>: expected output:\n"}] +
        (if $mode == "lifecycle_multiline" then [] else
          if $mode == "lifecycle_fragmented" then
            [{package:"fixture/api/pkg",test:"TestAlpha/child",text:"        === RU"},
             {package:"fixture/api/pkg",test:"TestAlpha/child",text:("N   " + $value + "\n")}]
          else [{package:"fixture/api/pkg",test:"TestAlpha/child",text:("        === RUN   " + $value + "\n")}] end
        end) |
        if $mode == "lifecycle_multiline" then
          .[0].text += "        === RUN   " + $value + "\n" else . end)
    ' "$directory/1-$side-TestAlpha.result" >/dev/null || fail "$mode compared continuation bytes or flag"
    jq -es --arg value "$value" '
      [.[] | select(.Action == "output") | .Output] | join("") |
      contains("expected output:\n        === RUN   " + $value + "\n")
    ' "$directory/1-$side.jsonl" >/dev/null || fail 'raw continuation lost'
  done
done
for mode in lifecycle_multiline_terminal lifecycle_split_terminal lifecycle_fragment_tail; do
  run "$mode"; verdict inconclusive
  if [[ $mode == lifecycle_multiline_terminal ]]; then has 'different failure'
  else has 'uncertain lifecycle output'; fi
  directory=${evidence_paths[0]%/*}
  for side in base head; do
    value=0.02
    duration=0.01
    [[ $side != head ]] || { value=0.03; duration=0.09; }
    jq -e --arg value "$value" --arg duration "$duration" --arg mode "$mode" '
      .result == "fail" and .assertion and
      .uncertainty == ($mode != "lifecycle_multiline_terminal") and
      .diagnostics == (
        [{package:"fixture/api/pkg",test:"TestAlpha/child",
          text:("    a_test.go:<line>: expected output:" +
            if $mode == "lifecycle_fragment_tail" then " " else "\n" end)}] +
        [{package:"fixture/api/pkg",test:"TestAlpha/child",
          text:((if $mode == "lifecycle_fragment_tail" then "" else "        " end) +
            "--- FAIL: TestAlpha/child (" + $value + "s)\n")}] |
        if $mode == "lifecycle_multiline_terminal" then
          [.[0] + {text:(.[0].text + .[1].text)}]
        elif $mode == "lifecycle_split_terminal" then
          . + [{package:"fixture/api/pkg",test:"TestAlpha/child",
            text:("    --- FAIL: TestAlpha/child (" + $duration + "s)\n")}]
        else . end)
    ' "$directory/1-$side-TestAlpha.result" >/dev/null || fail "$mode terminal continuation bytes/flag"
  done
done
for mode in lifecycle_noise lifecycle_deferred; do
  run "$mode"; verdict pre-existing
  directory=${evidence_paths[0]%/*}
  for side in base head; do
    jq -e --arg mode "$mode" '
      .result == "fail" and .assertion and .uncertainty == false and
      .diagnostics ==
        [{package:"fixture/api/pkg",test:"TestAlpha/child",
          text:"    <worktree>/api/pkg/a_test.go:<line>: expected 7 got 41\n"}]
    ' "$directory/1-$side-TestAlpha.result" >/dev/null || fail "$mode lifecycle correlation"
    if [[ $mode == lifecycle_deferred ]]; then
      jq -es '
        to_entries |
        [.[] | select(.value.Action == "output" and .value.Test == "TestAlpha" and
          (.value.Output | startswith("--- FAIL:")))] as $output |
        [.[] | select(.value.Action == "fail" and .value.Test == "TestAlpha")] as $terminal |
        $output[0].key + 1 < $terminal[0].key
      ' "$directory/1-$side.jsonl" >/dev/null || fail 'deferred terminal fixture was adjacent'
    fi
  done
done
for mode in lifecycle_uncertain_base lifecycle_uncertain_head lifecycle_uncertain_both lifecycle_competing; do
  run "$mode"; verdict inconclusive; has 'uncertain lifecycle output'
  directory=${evidence_paths[0]%/*}
  for side in base head; do
    uncertain=true
    [[ $mode != lifecycle_uncertain_base || $side != head ]] || uncertain=false
    [[ $mode != lifecycle_uncertain_head || $side != base ]] || uncertain=false
    duration=0.01
    [[ $side == head ]] && duration=0.09
    jq -e --argjson uncertain "$uncertain" --arg mode "$mode" --arg duration "$duration" '
      .result == "fail" and .assertion and .uncertainty == $uncertain and
      .diagnostics == (
        [{package:"fixture/api/pkg",test:"TestAlpha/child",
          text:"    <worktree>/api/pkg/a_test.go:<line>: expected 7 got 41\n"}] +
        (if $uncertain and $mode != "lifecycle_competing" then
          [{package:"fixture/api/pkg",test:"TestAlpha/child",text:"=== NAME  TestAlpha/child\n"}]
         else [] end) +
        (if $mode == "lifecycle_competing" then
          [{package:"fixture/api/pkg",test:"TestAlpha/child",
            text:"        --- FAIL: TestAlpha/child (0.02s)\n"},
           {package:"fixture/api/pkg",test:"TestAlpha/child",
            text:("    --- FAIL: TestAlpha/child (" + $duration + "s)\n")}]
         else [] end))
    ' "$directory/1-$side-TestAlpha.result" >/dev/null || fail "$mode uncertainty/retained diagnostics"
  done
  if [[ $mode == lifecycle_uncertain_both ]]; then
    jq -e --slurpfile head "$directory/1-head-TestAlpha.result" '
      .uncertainty and $head[0].uncertainty and .diagnostics == $head[0].diagnostics
    ' "$directory/1-base-TestAlpha.result" >/dev/null || fail 'matching ambiguous evidence was not exercised'
  fi
done
run lifecycle_uncertain_introduced; verdict pr-introduced
jq -e '.result == "fail" and .uncertainty == true' "${evidence_paths[0]%/*}/1-head-TestAlpha.result" >/dev/null ||
  fail 'uncertainty changed typed introduced verdict'
run lifecycle_wrong_order; verdict inconclusive; has 'uncertain lifecycle output'
for side in base head; do
  jq -e '.result == "fail" and .uncertainty == true and
    any(.diagnostics[]; .text == "=== RUN   TestAlpha/child\n") and
    any(.diagnostics[]; .test == "TestAlpha" and (.text | startswith("--- FAIL: TestAlpha (")))' \
    "${evidence_paths[0]%/*}/1-$side-TestAlpha.result" >/dev/null || fail 'ordering uncertainty lost'
done

run introduced; verdict pr-introduced
run race_introduced; verdict pr-introduced
run diagnostic_keywords; verdict pr-introduced
run race_warning; verdict pre-existing
cd "$WORK/repo/api/pkg"
run introduced; verdict pr-introduced
cd "$WORK/repo"
run pass; verdict not-reproduced
run matching; verdict pre-existing
run passing_diagnostic; verdict inconclusive; has 'insufficient assertion'
HEAD_SHA=$RENAMED_HEAD
run renamed; verdict inconclusive; has 'different failure'
HEAD_SHA=$ORIGINAL_HEAD
run body_root; verdict pre-existing
run changed; verdict inconclusive; has 'different failure'
jq -es 'any(.[]; (.Output // "") | contains("got 41"))' "${evidence_paths[0]}" >/dev/null || fail 'base diagnostic lost'
jq -es 'any(.[]; (.Output // "") | contains("got 42"))' "${evidence_paths[1]}" >/dev/null || fail 'head diagnostic value lost'
run location_value; verdict inconclusive; has 'different failure'
for mode in continuation continuation_test continuation_split continuation_test_split continuation_indent continuation_test_indent continuation_indent_split continuation_test_indent_split no_run no_run_introduced selected_no_run selected_no_run_introduced child_no_run child_no_run_introduced duplicate_run; do
  run "$mode"; verdict inconclusive
  [[ $(<"$WORK/out") != *'"pre-existing"'* && $(<"$WORK/out") != *'"pr-introduced"'* ]] ||
    fail "$mode overclaimed evidence"
done
run unknown_location; verdict inconclusive
for mode in oversized_stdout oversized_stderr oversized_version oversized_version_stderr; do
  run "$mode"; verdict inconclusive
  directory=${evidence_paths[0]%/*}
  for side in base head; do
    jq -e '.truncated or .version_truncated' "$directory/1-$side.meta.json" >/dev/null ||
      fail "$mode missing truncation metadata"
    for suffix in jsonl stderr version version.stderr; do
      [[ $(wc -c < "$directory/1-$side.$suffix") -le 8388609 ]] || fail "$mode unbounded capture"
    done
    [[ ! -e $directory/1-$side.jsonl.out.pipe && ! -e $directory/1-$side.version.err.pipe ]] ||
      fail "$mode leaked FIFO"
  done
done
run oversized_input "$(printf '%s' '--- FAIL: TestAlpha (0.01s)'; head -c 8388610 /dev/zero | tr '\000' x)"
verdict inconclusive; has 'input exceeds byte limit'; no_go
directory=${evidence_paths[0]%/*}
[[ $(wc -c < "$directory/input.log") -eq 8388609 ]] || fail 'input snapshot consumed beyond cap'
jq -e .input_truncated "$directory/limits.json" >/dev/null || fail 'missing input truncation'
candidates=
for ((i=0; i<33; i++)); do candidates+="--- FAIL: TestLimit$i (0.01s)"$'\n'; done
run candidate_limit "$candidates"; verdict inconclusive; has 'candidate limit exceeded'; no_go
jq -e .candidate_limit_exceeded "${evidence_paths[0]%/*}/limits.json" >/dev/null || fail 'missing candidate limit'
run bidi $'--- FAIL: TestBad\342\200\256 (0.01s)'; no_go
has '\u202e'
[[ $(<"$WORK/out") != *$'\342\200\256'* ]] || fail 'raw bidi rendered'
run subtests; verdict inconclusive; has 'different failure'
run base_fail; verdict inconclusive; has 'base fails, head passes'
run markers; verdict inconclusive; has 'insufficient assertion'
for mode in skip livedb missing child_missing build invalid truncated dependency toolchain race_setup panic timeout contradictory; do
  run "$mode"; verdict inconclusive
  [[ $(<"$WORK/out") != *'"pre-existing"'* && $(<"$WORK/out") != *'"pr-introduced"'* ]] ||
    fail "$mode overclaimed evidence"
done
# matching already varies absolute root, source line and lifecycle duration, but not values.
run subselect '--- FAIL: TestAlpha/child (0.01s)'; verdict pr-introduced; has '"TestAlpha"'
EXPECTED_PATTERN='^(TestAlpha|TestBeta)$'
run batch $'--- FAIL: TestAlpha (0.01s)\n--- FAIL: TestBeta (0.01s)\n--- FAIL: TestAlpha/child (0.01s)'
has '"TestBeta"'; verdict pr-introduced
[[ $(jq -s 'map(select(.argv[0] == "test")) | length' "$WORK/invocations") == 2 ]] || fail 'not batched'
EXPECTED_PATTERN='^(TestRoot)$'; EXPECTED_MODULE=controller; EXPECTED_PKG=./
run controller '--- FAIL: TestRoot (0.01s)'; verdict pr-introduced; has '"controller/./"'
EXPECTED_PATTERN='^(TestAPIRoot)$'; EXPECTED_MODULE=api
run api_root '--- FAIL: TestAPIRoot (0.01s)'; verdict pr-introduced; has '"api/./"'
EXPECTED_PATTERN='^(TestAlpha)$'; EXPECTED_PKG=./pkg
run absent '--- FAIL: TestAbsent (0.01s)'; no_go; has 'base definition absent'
run ambiguous '--- FAIL: TestAmbiguous (0.01s)'; no_go; has 'head definition absent or ambiguous'
run base_ambiguous '--- FAIL: TestBaseAmbiguous (0.01s)'; no_go; has 'base definition absent or ambiguous'
run empty ''; no_go; has 'no identifiable Go tests'
run unsupported 'vitest FAIL src/example.test.ts'; no_go; has 'unsupported failure evidence'; has 'no identifiable Go tests'
for evidence in 'node: ERR_TEST_FAILURE' 'lint failed' 'fmt-check failed' 'go vet failed'; do
  run unsupported "$evidence"; no_go; has 'unsupported failure evidence'
done
# Shell-looking log content is data. These literal inert strings are never executed.
run malformed $'--- FAIL: TestAlpha;touch sentinel (0.01s)\n--- FAIL: TestAlpha$(touch sentinel) (0.01s)\n--- FAIL: --help (0.01s)\n--- FAIL: TestBad\tname (0.01s)'
no_go; has 'malformed test candidate'
[[ ! -e sentinel ]] || fail 'executed log content'
[[ $(jq -Rs 'split("\n") | map(select(contains("malformed test candidate"))) | length' "$WORK/out") == 4 ]] ||
  fail 'malformed names were not separate single-line rows'
has 'TestBad\tname'

expect_two() {
  rc=0
  "$SCRIPT" "$@" > "$WORK/out" 2> "$WORK/err" || rc=$?
  [[ $rc == 2 ]] || fail "validation expected exit 2, got $rc"
  clean_trees
}
expect_two
expect_two --base "$BASE" --head "$HEAD_SHA"
expect_two --log "$WORK/missing" --base "$BASE" --head "$HEAD_SHA"
expect_two --log "$WORK/failure.log" --base unknown --head "$HEAD_SHA"
expect_two --log "$WORK/failure.log" --base "$BASE" --head unknown
blob=$(git rev-parse "$BASE:api/go.mod")
expect_two --log "$WORK/failure.log" --base "$blob" --head "$HEAD_SHA"
expect_two --log "$WORK/failure.log" --base --help --head "$HEAD_SHA"
expect_two --log "$WORK/failure.log" --base "$BASE" --head "$HEAD_SHA" --base "$BASE"
expect_two --log "$WORK/failure.log" --base "$BASE" --head "$HEAD_SHA" --other x
expect_two $'--bad\nINJECT\033[31m\342\200\256'
[[ $(wc -l < "$WORK/err") -eq 1 && $(<"$WORK/err") != *$'\033'* &&
   $(<"$WORK/err") != *$'\342\200\256'* ]] || fail 'unsafe CLI error rendering'
has_paths_tmp=$TMPDIR
TMPDIR="$WORK/tmp/"$'path\n\033[31m\342\200\256'
mkdir -p "$TMPDIR"
MODE=matching
printf '%s\n' '--- FAIL: TestAlpha (0.01s)' > "$WORK/failure.log"
"$SCRIPT" --log "$WORK/failure.log" --base "$BASE" --head "$HEAD_SHA" > "$WORK/out" 2> "$WORK/err"
verdict inconclusive
[[ $(wc -l < "$WORK/out") -eq 5 && $(<"$WORK/out") != *$'\033'* &&
   $(<"$WORK/out") != *$'\342\200\256'* ]] || fail 'unsafe path rendering'
while IFS= read -r line; do
  case "$line" in
    'base JSON: '*|'head JSON: '*)
      decoded=$(printf '"%s"\n' "${line#*: }" | jq -r .)
      [[ -r $decoded ]] || fail 'escaped evidence path unusable' ;;
  esac
done < "$WORK/out"
clean_trees
TMPDIR=$has_paths_tmp
expect_two --log "$WORK/failure.log" --base "$BASE" --head
expect_two --log "$WORK/repo" --base "$BASE" --head "$HEAD_SHA"
MODE=introduced
printf '%s\n' '--- FAIL: TestAlpha (0.01s)' > ./--help
rc=0
"$SCRIPT" --log --help --base "$BASE" --head "$HEAD_SHA" > "$WORK/out" 2> "$WORK/err" || rc=$?
[[ $rc == 0 ]] || fail 'option-shaped log filename was not treated as data'
verdict pr-introduced
clean_trees
MODE=partial
printf '%s\n' '--- FAIL: TestAlpha (0.01s)' > "$WORK/failure.log"
expect_two --log "$WORK/failure.log" --base "$BASE" --head "$HEAD_SHA"
[[ $(<"$WORK/err") == *'head worktree setup failed'* ]] || fail 'partial setup was not exercised'
has 'base JSON:'; has 'head JSON:'
for MODE in partial_base before_head; do
  expect_two --log "$WORK/failure.log" --base "$BASE" --head "$HEAD_SHA"
  has 'base JSON:'; has 'head JSON:'
done
printf 'PASS attribute-failures: verdicts, diagnostics, offline invocation, validation, evidence and cleanup\n'
