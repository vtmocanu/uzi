# One grammar for fragment validation and the pending-view renderer.
# Render buffers all input and emits only after validation, so errors cannot
# silently truncate legacy text. Only category grouping/blank separators change;
# the physical lines of every legacy bullet remain byte-for-byte and in order.
BEGIN {
    split("Added Changed Deprecated Removed Fixed Security", categories, " ")
    for (i = 1; i <= 6; i++) allowed[categories[i]] = 1
}
function fail(message) {
    print "changelog-fragments: " FILENAME ":" FNR ": " message > "/dev/stderr"
    failed = 1
}
function flush() {
    if (block != "") {
        entries[category, ++count[category]] = block
        block = ""
    }
}
{
    # UTF-8 encodings of ALM, LRM/RLM, embeddings/overrides and isolates.
    if ($0 ~ /\330\234|\342\200[\216\217\252-\256]|\342\201[\246-\251]/) fail("direction control in input")
    printable = $0
    gsub(/\t/, "", printable)
    if (printable ~ /[[:cntrl:]]/) fail("control byte in input")
}
mode == "validate" {
    if (FNR == 1) {
        if (substr($0, 1, 4) != "### " || !(substr($0, 5) in allowed)) fail("expected one canonical category")
    } else if (FNR == 2) {
        if ($0 != "") fail("expected blank line after category")
    } else if (FNR == 3) {
        if ($0 !~ /^- \*\*[^[:space:]].*\.\*\*$/ || index(substr($0, 5, length($0) - 6), "**")) fail("expected a bold outcome title ending in a period")
    } else if (FNR == 4) {
        if ($0 !~ /^  [^[:space:]].*$/) fail("expected one nonempty description line indented two spaces")
        if ($0 ~ /^  ([-*+] |[0-9]+[.)] |#+ )/) fail("description cannot be another bullet or heading")
    } else if ($0 != "") fail("only trailing blank lines are allowed")
    next
}
# The shell validates every fragment using the same parser before rendering.
FILENAME != legacy {
    if (FNR == 1) { flush(); category = substr($0, 5) }
    if (FNR == 3 || FNR == 4) block = block $0 "\n"
    next
}
{
    if ($0 ~ /^[[:space:]]*$/) { flush(); next }
    if ($0 ~ /^<!--[[:space:]]*release-title:.*-->[[:space:]]*$/) {
        flush()
        if (marker != "" || category != "") fail("misplaced or duplicate release-title marker")
        marker = $0
        next
    }
    if ($0 ~ /^[[:space:]]*<!--[[:space:]]*release-title/) {
        fail("malformed or misplaced release-title marker")
        next
    }
    if ($0 ~ /^#/) {
        flush()
        if (substr($0, 1, 4) != "### " || !(substr($0, 5) in allowed)) fail("unknown heading")
        category = substr($0, 5)
        next
    }
    if ($0 ~ /^- /) {
        flush()
        if (!(category in allowed)) fail("bullet outside a category")
        block = $0 "\n"
        next
    }
    # Match changelog-bullets.awk: continuations end at blank/heading/bullet.
    if (block != "") block = block $0 "\n"
    else fail("unknown or misplaced legacy content")
}
END {
    if (mode == "validate") {
        if (NR < 4) fail("fragment is empty or incomplete")
        exit failed ? 1 : 0
    }
    flush()
    if (failed) exit 1
    emitted = 0
    if (marker != "") { print marker; emitted = 1 }
    for (i = 1; i <= 6; i++) {
        c = categories[i]
        if (!count[c]) continue
        if (emitted) print ""
        print "### " c
        print ""
        for (j = 1; j <= count[c]; j++) {
            if (j > 1) print ""
            printf "%s", entries[c, j]
        }
        emitted = 1
    }
}
