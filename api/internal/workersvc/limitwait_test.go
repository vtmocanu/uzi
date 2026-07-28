package workersvc

import (
	"context"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestRateLimitTypeVocabularyMatchesCheck is the instrument migration 00091's
// comment promises, copied from TestSelectReasonVocabularyMatchesCheck (00089's) for
// the same reason it exists there.
//
// runs.rate_limit_type is DISPLAY-ONLY: nothing in the state machine, the claim
// path, any sweep gate or the promotion predicate reads it. So a value Go writes and
// the CHECK rejects has no failing consumer anywhere except Postgres, at park time,
// on a user's run — where it turns a display nicety into a failed run. And a value
// in the CHECK that Go never writes is a promise nothing keeps.
//
// It parses the migration rather than restating the list, because a second
// hand-typed copy is exactly the thing it is trying to prevent.
//
// MUTATION THIS CATCHES: adding or removing a member on either side without the
// other. Measured in both directions.
func TestRateLimitTypeVocabularyMatchesCheck(t *testing.T) {
	const path = "../store/migrations/00091_run_limit_wait.sql"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	// Strip comment lines first. The prose above the statement names several members,
	// and a regex over the whole file would happily collect them and agree with
	// itself.
	var stripped strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		stripped.WriteString(line)
		stripped.WriteString("\n")
	}
	body := stripped.String()

	// Then scope to THIS constraint's statement. Unlike 00089, whose file holds one
	// CHECK, 00091 also widens runs_status_check — twice, counting the Down section —
	// so a whole-file regex would collect eight status values and eleven more from the
	// rollback. Cut from the constraint name to the terminating semicolon.
	start := strings.Index(body, "ADD CONSTRAINT runs_rate_limit_type_check")
	if start < 0 {
		t.Fatalf("%s no longer declares runs_rate_limit_type_check; the guard is reading "+
			"the wrong thing, or the CHECK was dropped without dropping this test", path)
	}
	end := strings.Index(body[start:], ";")
	if end < 0 {
		t.Fatalf("runs_rate_limit_type_check's statement in %s has no terminating semicolon", path)
	}
	stmt := body[start : start+end]

	var fromSQL []string
	for _, m := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(stmt, -1) {
		fromSQL = append(fromSQL, m[1])
	}
	if len(fromSQL) == 0 {
		t.Fatalf("parsed no quoted values out of runs_rate_limit_type_check in %s; the guard "+
			"is reading the wrong thing", path)
	}

	fromGo := AllRateLimitTypes()
	sort.Strings(fromSQL)
	sort.Strings(fromGo)
	if strings.Join(fromSQL, ",") != strings.Join(fromGo, ",") {
		t.Fatalf("the rate_limit_type vocabulary has drifted.\n  Go:  %v\n  SQL: %v\n"+
			"A value Go writes and 00091's CHECK rejects is a constraint violation at park "+
			"time — i.e. a failed run instead of a parked one — and nothing else reads this "+
			"column, so nothing else would notice.", fromGo, fromSQL)
	}
}

// TestAllRateLimitTypesIsNotAliasable pins the fresh-slice contract. Cheap, and the
// bug it prevents (one caller's append rewriting a CLOSED set for everyone) is
// invisible until it is catastrophic.
func TestAllRateLimitTypesIsNotAliasable(t *testing.T) {
	got := AllRateLimitTypes()
	if len(got) == 0 {
		t.Fatal("the vocabulary is empty")
	}
	got[0] = "clobbered"
	if AllRateLimitTypes()[0] == "clobbered" {
		t.Fatal("AllRateLimitTypes returns a view of package state, so one caller can " +
			"rewrite the vocabulary for every other reader")
	}
}

func TestCoerceRateLimitType(t *testing.T) {
	strp := func(s string) *string { return &s }

	t.Run("absent stays absent", func(t *testing.T) {
		if got := CoerceRateLimitType(nil); got != nil {
			t.Fatalf("nil coerced to %q; a report that carried no type must not invent "+
				"'unknown' — a NULL column and an 'unknown' column are different facts", *got)
		}
	})

	t.Run("every vocabulary member passes through", func(t *testing.T) {
		for _, want := range AllRateLimitTypes() {
			got := CoerceRateLimitType(strp(want))
			if got == nil || *got != want {
				t.Fatalf("CoerceRateLimitType(%q) = %v, want %q — a legal SDK member was "+
					"flattened, which loses the only fact this column carries", want, got, want)
			}
		}
	})

	// The hostile inputs are the point: this allowlist is the ENTIRE sanitizer for
	// this field (no sanitizeSelfReported, no stripNULParam), so a byte surviving it
	// reaches the column, the DTO, the feed and Slack.
	t.Run("hostile input becomes the literal unknown", func(t *testing.T) {
		for name, in := range map[string]string{
			"empty":             "",
			"unknown member":    "eight_hour",
			"case variant":      "FIVE_HOUR",
			"whitespace-padded": " five_hour ",
			"NUL byte":          "five_hour\x00drop",
			"control char":      "five_hour\r\ninjected",
			"oversized":         strings.Repeat("a", 5009),
			"sql-ish":           "five_hour'); DROP TABLE runs; --",
		} {
			got := CoerceRateLimitType(strp(in))
			if got == nil || *got != RateLimitTypeUnknown {
				t.Fatalf("%s: CoerceRateLimitType(%q) = %v, want %q — this allowlist is the "+
					"only filter this field gets, so anything it passes reaches the column",
					name, in, got, RateLimitTypeUnknown)
			}
		}
	})

	t.Run("unknown is itself in the stored vocabulary", func(t *testing.T) {
		// Otherwise the coercion's own output would violate 00091's CHECK, which is a
		// failure mode with no other test position to catch it.
		for _, v := range AllRateLimitTypes() {
			if v == RateLimitTypeUnknown {
				return
			}
		}
		t.Fatalf("%q is what every unrecognised report becomes but is not in the CHECK's "+
			"value set; every coerced park would raise 23514", RateLimitTypeUnknown)
	})
}

// TestEveryStoredRateLimitTypeGoesThroughTheAllowlist closes the loop from the
// PRODUCER side — 00089 shipped two tests and this is the analogue of its
// TestEveryProducedReasonIsInTheVocabulary, which the first cut of this file omitted.
//
// The comment on 00091's CHECK claims "the CHECK is strictly WEAKER than the Go
// allowlist, so on the shipped path it can never fire". That holds ONLY IF every
// write goes through CoerceRateLimitType. If SetRunLimitWait ever passed
// req.RateLimitType into the params struct directly, the CHECK would become the only
// guard and a hostile value would 23514 the park — turning "validate early, never at
// render" into a failed run on a user's work. The vocabulary parse test above cannot
// see that: both lists would still agree.
//
// So this asserts the property at the SEAM the claim depends on: whatever the state
// path is handed, what reaches store.SetRunLimitWaitParams is a member of the stored
// vocabulary or NULL. Driven through the real SetState, not through decideLimitPark,
// because the bypass being guarded against is precisely a caller that skips the
// decision function.
//
// MUTATION THIS CATCHES: replacing the coerced value in setLimitWait with
// req.RateLimitType. Measured.
func TestEveryStoredRateLimitTypeGoesThroughTheAllowlist(t *testing.T) {
	legal := map[string]bool{}
	for _, v := range AllRateLimitTypes() {
		legal[v] = true
	}

	// Every shape a worker can put on the wire: legal members, plausible-looking
	// near-misses, and outright hostile bytes.
	reported := append(AllRateLimitTypes(),
		"", "eight_hour", "FIVE_HOUR", " five_hour ", "five_hour\x00drop",
		"five_hour\r\ninjected", strings.Repeat("z", 5009), "'); DROP TABLE runs; --",
	)
	for _, in := range reported {
		run := runningRun(true)
		fs, svc, wkr := limitParkFixture(t, run)
		fs.setLimitWaitRows = 1

		v := in
		if _, _, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{
			State: "limit_wait", RateLimitType: &v,
		}); err != nil {
			t.Fatalf("SetState(%q): %v", in, err)
		}
		if fs.setLimitWait == nil {
			t.Fatalf("%q: the park never reached the store", in)
		}
		got := fs.setLimitWait.RateLimitType
		if !got.Valid {
			t.Fatalf("%q: reached the column as NULL; a reported type must be stored, "+
				"coerced if need be — NULL means 'the worker said nothing'", in)
		}
		if !legal[got.String] {
			t.Fatalf("%q reached store.SetRunLimitWaitParams as %q, which 00091's CHECK "+
				"rejects. The migration's 'the CHECK is strictly weaker than the Go allowlist' "+
				"claim holds only while every write goes through CoerceRateLimitType; this "+
				"write does not, so the CHECK is now the only guard and this value 23514s the "+
				"park on a user's run", in, got.String)
		}
	}

	// The absent case is the other half: nil must stay NULL rather than becoming
	// "unknown", because the two mean different things to a support query.
	run := runningRun(true)
	fs, svc, wkr := limitParkFixture(t, run)
	fs.setLimitWaitRows = 1
	if _, _, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{State: "limit_wait"}); err != nil {
		t.Fatalf("SetState with no type: %v", err)
	}
	if fs.setLimitWait.RateLimitType.Valid {
		t.Fatalf("an absent type reached the column as %q; NULL and 'unknown' are "+
			"different facts", fs.setLimitWait.RateLimitType.String)
	}
}
