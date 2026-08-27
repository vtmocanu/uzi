package workersvc

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/anthropic"
)

// TestRateLimitSourceVocabularyMatchesCheck keeps anthropic.AllSources() and
// migration 00109's widened CHECK in step, mirroring
// TestSelectReasonVocabularyMatchesCheck (00089) and
// TestRateLimitTypeVocabularyMatchesCheck (00091). PRD #217 M1 adds a fourth home for
// this vocabulary (the migration, the Go constants, the TS union and the mock
// bundle); four homes and no drift test is the shape D21 exists to prevent.
//
// A value Go writes and 00109's CHECK rejects is a constraint violation at PARK time
// — a failed run instead of a parked one — and nothing else reads `source`, so
// nothing else would notice.
//
// MUTATION THIS CATCHES: adding a member to AllSources without widening 00109 (and
// the reverse).
func TestRateLimitSourceVocabularyMatchesCheck(t *testing.T) {
	const path = "../store/migrations/00109_rate_limit_source_limit_report.sql"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	// Strip comment lines first: the prose above the statement names every value, so a
	// whole-file regex would collect them and agree with itself. The `-- +goose Up` /
	// `-- +goose Down` markers go with them.
	var stripped strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		stripped.WriteString(line)
		stripped.WriteString("\n")
	}
	body := stripped.String()

	// Scope to the UP widening. The file carries TWO `ADD CONSTRAINT
	// anthropic_rate_limits_source_check` statements — Up widens to three values, Down
	// narrows back to two — so a whole-file regex would union both, and scoping to the
	// wrong one would silently read the two-value rollback. The FIRST ADD (after the
	// comment strip) is the Up statement; cut from it to the terminating semicolon.
	start := strings.Index(body, "ADD CONSTRAINT anthropic_rate_limits_source_check")
	if start < 0 {
		t.Fatalf("%s no longer declares an ADD CONSTRAINT for anthropic_rate_limits_source_check; "+
			"the guard is reading the wrong thing, or the CHECK was restructured", path)
	}
	end := strings.Index(body[start:], ";")
	if end < 0 {
		t.Fatalf("the UP ADD CONSTRAINT in %s has no terminating semicolon", path)
	}
	stmt := body[start : start+end]

	var fromSQL []string
	for _, m := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(stmt, -1) {
		fromSQL = append(fromSQL, m[1])
	}
	// The UP statement is the 3-value widening. A count of 2 means the slice landed on
	// the Down rollback instead — a distinct, legible failure from a value mismatch.
	if len(fromSQL) != 3 {
		t.Fatalf("parsed %d values out of the UP CHECK (%v); want the 3-value widening. A count of "+
			"2 means the guard scoped to the Down (narrowing) statement", len(fromSQL), fromSQL)
	}

	fromGo := anthropic.AllSources()
	sort.Strings(fromSQL)
	sort.Strings(fromGo)
	if strings.Join(fromSQL, ",") != strings.Join(fromGo, ",") {
		t.Fatalf("the source vocabulary has drifted.\n  Go:  %v\n  SQL: %v\nA value Go writes and "+
			"00109's CHECK rejects is a constraint violation at park time — a failed run instead of a "+
			"parked one — and nothing else reads this column, so nothing else would notice.", fromGo, fromSQL)
	}
}
