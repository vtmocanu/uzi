package store_test

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// Issue #3937: CancelRunByWorkerSettlingCancel copies CancelRunByWorker's candidates /
// parent-lock / eligibility chain and its whole runs transition into its `cancelled` CTE, and
// only a comment demanded that the two copies stay identical. This DB-free guard reads both
// queries from queries/runtime.sql (the promote_limit_wait_now_parity_test.go precedent),
// normalizes away comments and formatting whitespace, and requires the shared chain and the
// full `UPDATE runs SET … WHERE …` text to be byte-equal: every assignment, value, guard and
// the lock order. A SET-column-only comparison would miss a changed predicate, assignment value
// or lock order. The ONLY excluded text is the settling variant's own suffix: the `cancelled`
// wrapper, RETURNING runs.id, the settled-input CTE and the final count.

const (
	cancelBaseQuery     = "CancelRunByWorker"
	cancelSettlingQuery = "CancelRunByWorkerSettlingCancel"
)

var (
	cancelBaseShape     = regexp.MustCompile(`^(WITH .*?\))(UPDATE runs SET .*);$`)
	cancelSettlingShape = regexp.MustCompile(`^(WITH .*?\)),cancelled AS\((UPDATE runs SET .*?)RETURNING runs\.id\),settled AS\((.+)\)SELECT count\(\*\)FROM cancelled;$`)
)

// cancelSQLTight are the characters next to which formatting whitespace carries no meaning.
const cancelSQLTight = "(),;=<>!"

// normalizeCancelSQL is a quote-aware normalizer. Inside a single-quoted literal every byte is
// kept verbatim (whitespace, commas, `--`, `/*` and the doubled-quote escape). Outside a literal,
// `--` line comments and `/* */` block comments are dropped, whitespace runs collapse to one
// space, and whitespace next to cancelSQLTight characters is dropped, so a single space between
// two word tokens (IS NOT NULL) survives. An unterminated literal or block comment is an error.
func normalizeCancelSQL(sql string) (string, error) {
	tight := func(c byte) bool { return strings.IndexByte(cancelSQLTight, c) >= 0 }
	var out []byte
	space := false
	emit := func(c byte) {
		if space && len(out) > 0 && !tight(out[len(out)-1]) && !tight(c) {
			out = append(out, ' ')
		}
		space = false
		out = append(out, c)
	}
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch {
		case c == '\'':
			emit(c)
			for i++; ; i++ {
				if i >= len(sql) {
					return "", errors.New("unterminated string literal")
				}
				out = append(out, sql[i])
				if sql[i] == '\'' {
					if i+1 < len(sql) && sql[i+1] == '\'' {
						i++
						out = append(out, '\'')
						continue
					}
					break
				}
			}
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			space = true
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				return "", errors.New("unterminated block comment")
			}
			i += 2 + end + 1
			space = true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			space = true
		default:
			emit(c)
		}
	}
	return string(out), nil
}

// cancelQueryParity compares the two query blocks (as returned by readQueryBlock) and returns a
// non-nil error naming the first divergence.
func cancelQueryParity(base, settling string) error {
	parts := map[string][]string{}
	for _, q := range []struct {
		name, block string
		shape       *regexp.Regexp
	}{{cancelBaseQuery, base, cancelBaseShape}, {cancelSettlingQuery, settling, cancelSettlingShape}} {
		// readQueryBlock starts after the `-- name: <query> ` marker: drop the `:kind` rest.
		_, body, ok := strings.Cut(q.block, "\n")
		if !ok {
			return fmt.Errorf("%s: query block has no body", q.name)
		}
		norm, err := normalizeCancelSQL(body)
		if err != nil {
			return fmt.Errorf("%s: %w", q.name, err)
		}
		if norm == "" {
			return fmt.Errorf("%s: query body is empty after normalization", q.name)
		}
		m := q.shape.FindStringSubmatch(norm)
		if m == nil {
			return fmt.Errorf("%s: normalized SQL does not match its expected shape %s:\n%s", q.name, q.shape, norm)
		}
		parts[q.name] = m[1:]
	}
	b, s := parts[cancelBaseQuery], parts[cancelSettlingQuery]
	if b[0] != s[0] {
		return fmt.Errorf("candidates/parent-lock/eligibility chain diverged:\n%s: %s\n%s: %s", cancelBaseQuery, b[0], cancelSettlingQuery, s[0])
	}
	if b[1] != s[1] {
		return fmt.Errorf("runs transition (SET/WHERE) diverged:\n%s: %s\n%s: %s", cancelBaseQuery, b[1], cancelSettlingQuery, s[1])
	}
	for _, anchor := range []struct{ text, in string }{
		{"FOR UPDATE OF lead", b[0]},
		{"ORDER BY lead.id", b[0]},
		{"runs.claim_released_at IS NULL", b[0]},
		{"status='cancelled'", b[1]},
		{"runs.claim_released_at IS NULL", b[1]},
	} {
		if !strings.Contains(anchor.in, anchor.text) {
			return fmt.Errorf("non-vacuity anchor %q missing — the extraction is looking at the wrong text", anchor.text)
		}
	}
	return nil
}

func readCancelQueries(t *testing.T) (string, string) {
	t.Helper()
	const file = "queries/runtime.sql"
	return readQueryBlock(t, file, cancelBaseQuery), readQueryBlock(t, file, cancelSettlingQuery)
}

func TestCancelRunByWorkerSettlingCancelMirrorsCancelRunByWorker(t *testing.T) {
	base, settling := readCancelQueries(t)
	if err := cancelQueryParity(base, settling); err != nil {
		t.Fatalf("%s must keep %s's chain and transition verbatim: %v", cancelSettlingQuery, cancelBaseQuery, err)
	}
}

// The guard must catch a one-copy drift in a guard, an assigned value, a literal or the lock
// order, and must allow formatting-only and settlement-suffix-only changes.
func TestCancelRunByWorkerSettlingParityMutants(t *testing.T) {
	base, settling := readCancelQueries(t)
	mutate := func(t *testing.T, s, old, repl string) string {
		t.Helper()
		got := strings.Replace(s, old, repl, 1)
		if got == s {
			t.Fatalf("mutant did not apply: %q not found", old)
		}
		return got
	}
	for _, tc := range []struct {
		name     string
		apply    func(t *testing.T) (string, string)
		wantFail bool
	}{
		{"release guard in settling chain", func(t *testing.T) (string, string) {
			return base, mutate(t, settling, "runs.claim_released_at IS NULL", "runs.claim_released_at IS NOT NULL")
		}, true},
		{"SET value in settling transition", func(t *testing.T) (string, string) {
			return base, mutate(t, settling, "health = 'ok'", "health = 'degraded'")
		}, true},
		{"parent-lock ordering in base", func(t *testing.T) (string, string) {
			return mutate(t, base, "ORDER BY lead.id", "ORDER BY lead.id DESC"), settling
		}, true},
		{"literal content in settling SET", func(t *testing.T) (string, string) {
			return base, mutate(t, settling, "= 'cancelled',", "= 'cancel led',")
		}, true},
		{"WHERE guard in base transition", func(t *testing.T) (string, string) {
			return mutate(t, base, "AND runs.id IN (SELECT run_id FROM eligible_candidates);", "AND runs.id IN (SELECT run_id FROM candidates);"), settling
		}, true},
		{"settling wrapper missing RETURNING", func(t *testing.T) (string, string) {
			return base, mutate(t, settling, "RETURNING runs.id", "")
		}, true},
		{"formatting only", func(t *testing.T) (string, string) {
			s := mutate(t, settling, "health = 'ok'", "health='ok' /* block\ncomment */")
			s = mutate(t, s, "status             = 'cancelled',", "status = 'cancelled', -- extra comment\n")
			return strings.ReplaceAll(base, "\n", "\n\n    "), s
		}, false},
		{"settlement suffix only", func(t *testing.T) (string, string) {
			return base, mutate(t, settling, "AND applied_at IS NULL", "AND applied_at IS NOT NULL")
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, s := tc.apply(t)
			err := cancelQueryParity(b, s)
			if tc.wantFail && err == nil {
				t.Fatal("parity guard passed a drifted copy")
			}
			if !tc.wantFail && err != nil {
				t.Fatalf("parity guard rejected an allowed change: %v", err)
			}
		})
	}
}

func TestNormalizeCancelSQL(t *testing.T) {
	norm := func(s string) string {
		t.Helper()
		got, err := normalizeCancelSQL(s)
		if err != nil {
			t.Fatalf("normalize %q: %v", s, err)
		}
		return got
	}
	for _, pair := range [][2]string{
		{"x = 'two  words'", "x = 'two words'"},
		{"x = 'a,b'", "x = 'a, b'"},
		{"x = 'a -- b'", "x = 'a'"},
	} {
		if norm(pair[0]) == norm(pair[1]) {
			t.Errorf("literal contents folded: %q and %q both normalize to %q", pair[0], pair[1], norm(pair[0]))
		}
	}
	for _, pair := range [][2]string{
		{"health = 'ok'", "health='ok'"},
		{"a <> 'b'", "a<>'b'"},
		{"a  -- c\nb", "a b"},
		{"a /* c\n d */ b", "a b"},
		{"f( x , y ) ;", "f(x,y);"},
	} {
		if norm(pair[0]) != norm(pair[1]) {
			t.Errorf("formatting not ignored: %q -> %q, %q -> %q", pair[0], norm(pair[0]), pair[1], norm(pair[1]))
		}
	}
	if got, want := norm("x = 'it''s  x'"), "x='it''s  x'"; got != want {
		t.Errorf("doubled quote: got %q, want %q", got, want)
	}
	if got, want := norm("a IS  NOT\n NULL"), "a IS NOT NULL"; got != want {
		t.Errorf("token boundary: got %q, want %q", got, want)
	}
	for _, bad := range []string{"x = 'open", "x /* open"} {
		if _, err := normalizeCancelSQL(bad); err == nil {
			t.Errorf("normalize %q: want an error", bad)
		}
	}
}
