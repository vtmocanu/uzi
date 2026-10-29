#!/usr/bin/env bash
# Shared fail-closed migration inspection for takeover and the final merge guard.

migration_pr_files() { # OWNER/REPO PR -> complete paginated file array
  # GitHub caps this endpoint at 3000 files, even with pagination.
  local pages
  pages=$(gh api --paginate "repos/$1/pulls/$2/files" 2>/dev/null) || return 1
  printf '%s' "$pages" | jq -cse '
    if length > 0 and all(.[]; type == "array")
    then add else error("unreadable PR files") end
    | if length >= 3000 then error("possibly truncated PR files") else . end
    | if all(.[]; type == "object" and (.filename|type) == "string" and (.status|type) == "string")
      then . else error("unreadable PR file entry") end' 2>/dev/null
}

migration_report() { # PR file array, base contents array -> {head,added,collisions}
  # added includes rename/copy destinations; previous_filename is never the candidate.
  { printf '%s\n' "$1"; printf '%s\n' "$2"; } | jq -cse '
    def version:
      if test("^[0-9]+_.+\\.sql$") then capture("^(?<n>[0-9]+)_").n | tonumber
      else error("unreadable migration version") end;
    if length != 2 then error("unreadable migration inputs") else . end
    | .[0] as $files | .[1] as $base
    | if ($files|type) != "array" or ($base|type) != "array"
        then error("unreadable migration listings") else . end
    # The contents API returns at most 1000 entries for a directory.
    | if ($base|length) >= 1000 then error("possibly truncated base directory") else . end
    | if ($files|all(.[]; type == "object" and (.filename|type) == "string" and (.status|type) == "string"))
         and ($base|all(.[]; type == "object" and (.name|type) == "string"))
        then . else error("unreadable migration entry") end
    | [$base[].name | select(endswith(".sql")) | version] | (max // 0) as $head
    | [$files[] | select((.status|IN("added", "renamed", "copied")) and (.filename|test("^api/internal/store/migrations/[^/]+\\.sql$")))
         | .filename | split("/") | last] as $added
    | {head:$head, added:$added, collisions:[$added[] | select(version <= $head)]}' 2>/dev/null
}
