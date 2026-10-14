# Shared exact block grammar for RC deltas and the frozen-shipped-bullet guard.
# A block starts at top-level '- ' and ends at a bullet, heading, blank or EOF.
BEGIN { SEP = sprintf("%c", 30); previous_file = ARGV[1] }
function key(    value, i) {
    value = b[1]
    for (i = 2; i <= n; i++) value = value SEP b[i]
    return value
}
function flush(    value, i) {
    if (!n) return
    value = key()
    if (!current) { previous[value] = b[1]; previous_order[++previous_count] = value }
    else {
        present[value] = 1
        if (mode == "delta" && !(value in previous)) {
            if (pending != "") {
                if (emitted) print ""
                print pending; print ""; pending = ""; emitted = 1
            }
            for (i = 1; i <= n; i++) { print b[i]; emitted = 1 }
        }
    }
    n = 0
}
{
    if (FILENAME != previous_file && !current) { flush(); current = 1; pending = "" }
    if ($0 ~ /^- /) { flush(); b[1] = $0; n = 1 }
    else if ($0 ~ /^### / || $0 ~ /^## / || $0 ~ /^[[:space:]]*$/) {
        flush()
        if (current) pending = $0 ~ /^### / ? $0 : pending
        if (current && $0 ~ /^## /) pending = ""
    }
    else if (n) b[++n] = $0
}
END {
    flush()
    if (mode == "check") {
        for (i = 1; i <= previous_count; i++) {
            value = previous_order[i]
            if (!(value in present)) {
                print "changelog-section: frozen published RC bullet changed or removed: " previous[value] > "/dev/stderr"
                failed = 1
            }
        }
        if (failed) {
            print "A bullet a published candidate already carries is frozen; add a new bullet for the amendment, never edit or split the shipped one." > "/dev/stderr"
            exit 1
        }
    }
}
