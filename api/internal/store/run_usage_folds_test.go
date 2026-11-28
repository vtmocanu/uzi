package store

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Issue #4700 enforcement: run_usage fold freshness (migration 00549) holds only while EVERY
// run_usage writer first calls fn_run_usage_fold_lock for the run it writes. There are no
// triggers on run_usage, so this test is the guard. It checks, without a database:
//
//	(i)   every SQL statement in queries/*.sql, and in migrations numbered above 00549, that
//	      writes run_usage carries an fn_run_usage_fold_lock CTE the statement reads from;
//	(ii)  no non-test Go file outside the sqlc output embeds raw SQL writing run_usage;
//	(iii) the generated UpsertRunUsage / DeleteRunUsage methods are called only from the
//	      three vetted functions, never taken as method values or named by string;
//	(iv)  RefoldRunUsage takes the run lock (LockRunForUsageRefold) before DeleteRunUsage.
//
// The SQL is parsed with lexSQL (sql_lexer_test.go): comments, strings, quoted identifiers and
// dollar quotes are lexed away before any keyword is looked for. A DELETE of run_usage must also be
// scoped to the locked run (a top-level AND conjunct run_id = <the lock argument>, no top-level OR).
//
// Recorded limits (not detected): dynamic SQL built and run through EXECUTE format(...); COPY
// run_usage FROM; U&"..." identifiers; functions defined at or below migration 00549; the scope of
// UPDATE, INSERT and MERGE (checked for the lock only); TRUNCATE ... CASCADE on a table that
// run_usage references, which empties run_usage through its foreign key (migration 00062);
// DELETE FROM runs, which cascades to run_usage through the same foreign key; a dollar quote used
// as a string literal, which is lexed as code; ALTER TABLE ... USING, DROP TABLE, DML through views
// or rules, and triggers at or below migration 00549; a function or DO body given as a
// single-quoted string (AS 'DELETE FROM run_usage'); a catalog-qualified db.schema.run_usage (only
// one qualifier is matched); TRUNCATE public.on, run_usage (the ON stop fires after a dot); a
// plpgsql #variable_conflict use_variable directive with a variable named run_id; the refold check
// does not verify that the lock and the delete run on the transaction-bound queries (qtx, not q);
// and the shutdown-wiring check is a presence check that does not reject an extra Wait added before
// the drain. A cast in the DELETE scope conjunct (run_id = $1::uuid) fails closed (rejected).

var (
	// DML writers of run_usage itself, never run_usage_legs / _messages / _tail_state /
	// _model_totals / _fold_state: the table name (after an optional closing quote) must be
	// followed by a non-identifier byte (not "." or "$") or the end of the text. The target may be
	// quoted and/or schema-qualified ("run_usage", public.run_usage, public."run_usage") and may
	// follow ONLY, with or without a parenthesis. Group 1 is the statement keyword, group 2 the
	// byte after the name (empty at the end of the text).
	// It runs on the comment-stripped text WITH quoted identifiers intact (norm), and a match only
	// counts when its keyword is also visible in the mask, i.e. is not inside a string.
	runUsageDMLRE = regexp.MustCompile(`(?is)\b(INSERT\s+INTO|MERGE\s+INTO|UPDATE|DELETE\s+FROM)\s*(?:ONLY\b\s*\(?\s*)?(?:"?[A-Za-z_]\w*"?\s*\.\s*)?"?run_usage"?([^A-Za-z0-9_$.\x{80}-\x{10FFFF}]|$)`)
	// TRUNCATE [TABLE] [ONLY] t1 [*] [, ...]: run_usage may be anywhere in the target list, which
	// stops at ON (GRANT / CREATE TRIGGER ... TRUNCATE ON), CASCADE, RESTRICT, RESTART, CONTINUE,
	// ";" or the end of the statement.
	runUsageTruncateRE = regexp.MustCompile(`(?i)\bTRUNCATE\b`)
	truncateHeadRE     = regexp.MustCompile(`(?is)^\s*(?:TABLE\b\s*)?`)
	// CASCADE, RESTART etc. are unreserved keywords, so they only end the list in their option
	// position: CASCADE/RESTRICT before the end, ";" or ")", RESTART/CONTINUE before IDENTITY.
	truncateListEndRE = regexp.MustCompile(`(?i);|\bON\b|\b(?:CASCADE|RESTRICT)\b\s*(?:;|\)|$)|\b(?:RESTART|CONTINUE)\s+IDENTITY\b`)
	truncateTargetRE  = regexp.MustCompile(`(?is)(?:^|,)\s*(?:ONLY\b\s*\(?\s*)?(?:"?[A-Za-z_]\w*"?\s*\.\s*)?"?run_usage"?\s*\*?\s*(?:,|\)|$)`)
	// The lock CTE must be defined as lk AND be the one the main statement reads. These run on the
	// mask, so text in a string or quoted identifier never satisfies them.
	runUsageLockDefRE = regexp.MustCompile(`(?is)\blk\s+AS\s*\(\s*SELECT\s+fn_run_usage_fold_lock\s*\(`)
	runUsageFromLk    = regexp.MustCompile(`(?is)\bFROM\s+lk\b`)
	usageWithRE       = regexp.MustCompile(`(?i)\bWITH\b`)
	usageMainKwRE     = regexp.MustCompile(`(?i)^(?:INSERT|DELETE|UPDATE|MERGE|SELECT|TRUNCATE)\b`)
	usageLockArgRE    = regexp.MustCompile(`^(?:\$\d+|@\w+|sqlc\.n?arg\(\w+\))$`)
	usageLockExistsRE = regexp.MustCompile(`(?is)^\s*EXISTS\s*\(\s*SELECT\b.*\bFROM\s+lk\b.*\)\s*$`)
	usageAliasRE      = regexp.MustCompile(`(?is)^\s*\*?\s*(?:AS\s+)?([A-Za-z_]\w*)`)
)

// usageWrite is one run_usage writer found in a statement: its span in the statement, and its
// kind ("INSERT", "MERGE", "UPDATE", "DELETE" or "TRUNCATE").
type usageWrite struct {
	start, end int
	kind       string
}

// runUsageWrites finds the run_usage writers in a lexed statement (norm and mask of equal length).
// A keyword inside a string, or a comment, never counts.
func runUsageWrites(norm, mask string) []usageWrite {
	var out []usageWrite
	for _, m := range runUsageDMLRE.FindAllStringSubmatchIndex(norm, -1) {
		if norm[m[2]:m[3]] != mask[m[2]:m[3]] {
			continue
		}
		out = append(out, usageWrite{start: m[0], end: m[4], kind: strings.ToUpper(strings.Fields(norm[m[2]:m[3]])[0])})
	}
	for _, m := range runUsageTruncateRE.FindAllStringIndex(norm, -1) {
		if norm[m[0]:m[1]] != mask[m[0]:m[1]] {
			continue
		}
		head := truncateHeadRE.FindStringIndex(norm[m[1]:])
		if head == nil {
			continue
		}
		listStart := m[1] + head[1]
		listEnd := len(norm)
		if e := truncateListEndRE.FindStringIndex(mask[listStart:]); e != nil {
			listEnd = listStart + e[0]
		}
		if truncateTargetRE.MatchString(norm[listStart:listEnd]) {
			out = append(out, usageWrite{start: m[0], end: listEnd, kind: "TRUNCATE"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

// usageWord is an identifier-like token of a mask with the parenthesis depth it sits at.
type usageWord struct {
	text       string // upper-cased
	start, end int
	depth      int
}

func usageWords(mask string) []usageWord {
	var out []usageWord
	depth := 0
	for i := 0; i < len(mask); {
		c := mask[i]
		switch {
		case c == '(':
			depth++
			i++
		case c == ')':
			depth--
			i++
		case (c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') && (i == 0 || !isUsageWordByte(mask[i-1]) && mask[i-1] != '.'):
			j := i
			for j < len(mask) && isUsageWordByte(mask[j]) {
				j++
			}
			out = append(out, usageWord{text: strings.ToUpper(mask[i:j]), start: i, end: j, depth: depth})
			i = j
		default:
			i++
		}
	}
	return out
}

// splitWithClause finds the WITH clause of a masked statement: the CTE list is mask[cteStart:mainStart]
// and the main statement (the first top-level INSERT/DELETE/UPDATE/MERGE/SELECT after the list)
// starts at mainStart. ok is false without a WITH or when no main statement follows it. The mask
// carries no quote interiors, so parentheses inside strings never count.
func splitWithClause(mask string) (cteStart, mainStart int, ok bool) {
	loc := usageWithRE.FindStringIndex(mask)
	if loc == nil {
		return 0, 0, false
	}
	depth := 0
	for i := loc[1]; i < len(mask); i++ {
		switch c := mask[i]; c {
		case '(':
			depth++
		case ')':
			depth--
		default:
			if depth == 0 && !isUsageWordByte(mask[i-1]) && usageMainKwRE.MatchString(mask[i:]) {
				return loc[1], i, true
			}
		}
	}
	return 0, 0, false
}

func isUsageWordByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// balancedArg returns mask[open:close) where close is the ")" closing the "(" just before open.
func balancedArg(mask string, open int) (end int, ok bool) {
	depth := 1
	for i := open; i < len(mask); i++ {
		switch mask[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

// splitConjuncts splits a masked WHERE clause at its top-level ANDs, ignoring ANDs inside
// parentheses, inside BETWEEN x AND y and inside CASE ... END. It returns the [start,end) spans of
// the conjuncts and whether a top-level OR (outside CASE) is present.
func splitConjuncts(mask string) (spans [][2]int, hasOR bool) {
	start, caseDepth, between := 0, 0, 0
	for _, w := range usageWords(mask) {
		if w.depth != 0 {
			continue
		}
		switch w.text {
		case "CASE":
			caseDepth++
		case "END":
			if caseDepth > 0 {
				caseDepth--
			}
		case "BETWEEN":
			if caseDepth == 0 {
				between++
			}
		case "OR":
			if caseDepth == 0 {
				hasOR = true
			}
		case "AND":
			if caseDepth != 0 {
				continue
			}
			if between > 0 {
				between--
				continue
			}
			spans = append(spans, [2]int{start, w.start})
			start = w.end
		}
	}
	return append(spans, [2]int{start, len(mask)}), hasOR
}

// deleteScopeReason returns "" when the DELETE writer w (the leading statement of the lexed main
// statement) has a WHERE clause with no top-level OR and, as top-level AND conjuncts, both
// run_id = <arg> (or <arg> = run_id; run_id bare or qualified by the target name or its alias) and
// EXISTS (SELECT ... FROM lk ...); otherwise it names what is missing. arg is the token the
// statement passes to fn_run_usage_fold_lock.
func deleteScopeReason(norm, mask string, w usageWrite, arg string) string {
	quals := []string{`run_usage`, `"run_usage"`}
	// An alias hides the table name (PostgreSQL), so with one only <alias>.run_id qualifies; a
	// run_usage.run_id would resolve to some USING item.
	if m := usageAliasRE.FindStringSubmatch(mask[w.end:]); m != nil {
		switch strings.ToUpper(m[1]) {
		case "WHERE", "USING", "RETURNING":
		default:
			quals = []string{regexp.QuoteMeta(m[1])}
		}
	}
	q := `(?:(?:` + strings.Join(quals, "|") + `)\s*\.\s*)?run_id`
	a := regexp.QuoteMeta(arg)
	scope := regexp.MustCompile(`(?is)^\s*(?:` + q + `\s*=\s*` + a + `|` + a + `\s*=\s*` + q + `)\s*$`)

	whereStart := -1
	for _, wd := range usageWords(mask) {
		if wd.start >= w.end && wd.depth == 0 && wd.text == "WHERE" {
			whereStart = wd.end
			break
		}
	}
	if whereStart < 0 {
		return "DELETE has no top-level WHERE"
	}
	whereEnd := len(mask)
	if i := strings.IndexByte(mask[whereStart:], ';'); i >= 0 {
		whereEnd = whereStart + i
	}
	for _, wd := range usageWords(mask[:whereEnd]) {
		if wd.start >= whereStart && wd.depth == 0 && wd.text == "RETURNING" {
			whereEnd = wd.start
			break
		}
	}
	spans, hasOR := splitConjuncts(mask[whereStart:whereEnd])
	if hasOR {
		return "DELETE WHERE has a top-level OR"
	}
	var scoped, locked bool
	for _, sp := range spans {
		scoped = scoped || scope.MatchString(norm[whereStart+sp[0]:whereStart+sp[1]])
		locked = locked || usageLockExistsRE.MatchString(mask[whereStart+sp[0]:whereStart+sp[1]])
	}
	switch {
	case !scoped:
		return "DELETE WHERE lacks a run_id = <lock argument> conjunct"
	case !locked:
		return "DELETE WHERE lacks an EXISTS (SELECT ... FROM lk) conjunct"
	}
	return ""
}

// unguardedReason returns "" when the lexed statement is not a run_usage writer or is guarded, else
// why it is unguarded. Guarded means: the WITH clause defines lk as fn_run_usage_fold_lock of a
// parameter, no CTE itself writes run_usage, and the MAIN statement (not some other CTE or a
// later statement) is the single writer, starts the statement and reads FROM lk. A main DELETE
// must also be scoped to the locked run (deleteScopeReason). UPDATE, INSERT and MERGE are not
// scope-checked: a recorded limit of this guard.
func unguardedReason(norm, mask string) string {
	if len(runUsageWrites(norm, mask)) == 0 {
		return ""
	}
	cs, ms, ok := splitWithClause(mask)
	if !ok {
		return "no WITH clause"
	}
	loc := runUsageLockDefRE.FindStringIndex(mask[cs:ms])
	if loc == nil {
		return "no lk CTE calling fn_run_usage_fold_lock"
	}
	argStart := cs + loc[1]
	argEnd, ok := balancedArg(mask, argStart)
	if !ok {
		return "unbalanced fn_run_usage_fold_lock call"
	}
	arg := strings.TrimSpace(norm[argStart:argEnd])
	if len(runUsageWrites(norm[cs:ms], mask[cs:ms])) > 0 {
		return "a CTE writes run_usage"
	}
	mainWrites := runUsageWrites(norm[ms:], mask[ms:])
	if len(mainWrites) != 1 || mainWrites[0].start != 0 {
		return "the main statement is not the single run_usage writer"
	}
	if !runUsageFromLk.MatchString(mask[ms:]) {
		return "the main statement never reads FROM lk"
	}
	if !usageLockArgRE.MatchString(arg) {
		return "lock argument is not a parameter"
	}
	if mainWrites[0].kind == "DELETE" {
		return deleteScopeReason(norm[ms:], mask[ms:], mainWrites[0], arg)
	}
	return ""
}

func unguardedLexed(norm, mask string) bool { return unguardedReason(norm, mask) != "" }

// unguardedRunUsageWrite lexes stmt and classifies it; an error is a lexer error (unterminated
// block comment).
func unguardedRunUsageWrite(stmt string) (bool, error) {
	norm, mask, err := lexSQL(stmt)
	if err != nil {
		return false, err
	}
	return unguardedLexed(norm+" ", mask+" "), nil
}

// writesRunUsage reports whether s (SQL, or an arbitrary Go string literal) writes run_usage:
// either the raw text or the lexed scan matches, so a literal such as "'; DELETE FROM run_usage; --'"
// is caught even though the lexer reads its tail as a string, and a string the lexer rejects is
// scanned as is. More text counts, never less.
func writesRunUsage(s string) bool {
	raw := strings.ReplaceAll(s, "\v", " ") + " " // \v is whitespace in PostgreSQL, not in RE2 \s
	if len(runUsageWrites(raw, raw)) > 0 {
		return true
	}
	norm, mask, err := lexSQL(s)
	return err == nil && len(runUsageWrites(norm+" ", mask+" ")) > 0
}

func TestWritesRunUsageGoLiterals(t *testing.T) {
	for _, s := range []string{"'; DELETE FROM run_usage; --'", "x' DELETE FROM run_usage '", "DELETE FROM /* c */ run_usage WHERE x", "DELETE FROM run_usage /*", "DELETE\vFROM\vrun_usage\vWHERE x", "TRUNCATE run_usage\v;", "WITH d AS (DELETE FROM run_usage) SELECT 1", "DELETE FROM ONLY (run_usage)"} {
		if !writesRunUsage(s) {
			t.Errorf("writesRunUsage(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"SELECT 1", "DELETE FROM run_usage_legs", "glob/*"} {
		if writesRunUsage(s) {
			t.Errorf("writesRunUsage(%q) = true, want false", s)
		}
	}
}

func TestRunUsageWriterClassifier(t *testing.T) {
	const lkReal = "WITH lk AS (SELECT fn_run_usage_fold_lock(@run_id) AS ok) "
	good := []string{
		lkReal + "DELETE FROM run_usage WHERE run_id = @run_id AND EXISTS (SELECT 1 FROM lk WHERE lk.ok)",
		"WITH lk AS (SELECT fn_run_usage_fold_lock($1) AS ok)\nINSERT INTO run_usage (a)\nSELECT 1 FROM lk WHERE lk.ok",
		"INSERT INTO run_usage_legs (a) VALUES (1)",
		"DELETE FROM run_usage_messages WHERE x",
		"UPDATE run_usage_tail_state SET a = 1",
		"SELECT * FROM run_usage",
		"-- DELETE FROM run_usage WHERE x\nSELECT 1",
		// A scoped variant of the previously accepted unscoped fixture.
		"WITH lk AS (SELECT fn_run_usage_fold_lock($1) AS ok) DELETE FROM public.\"run_usage\" WHERE run_id = $1 AND EXISTS (SELECT 1 FROM lk WHERE lk.ok)",
		"WITH lk AS (SELECT fn_run_usage_fold_lock($1) AS ok) DELETE FROM run_usage AS ru WHERE $1 = ru.run_id AND EXISTS (SELECT 1 FROM lk WHERE lk.ok) RETURNING 1",
		"WITH lk AS (SELECT fn_run_usage_fold_lock(sqlc.arg(run_id)) AS ok) DELETE FROM run_usage WHERE run_usage.run_id = sqlc.arg(run_id) AND x BETWEEN 1 AND 2 AND EXISTS (SELECT 1 FROM lk WHERE lk.ok OR true)",
		// Not run_usage, quoted or qualified or not.
		"DELETE FROM run_usage_fold_state WHERE x",
		"DELETE FROM \"run_usage_model_folds\" WHERE x",
		"UPDATE public.run_usage_totals SET a = 1",
		"TRUNCATE run_usage_model_folds, run_usage_fold_state",
		// Comments and strings are not code.
		"/* DELETE FROM run_usage */ SELECT 1",
		"/* a /* DELETE FROM run_usage */ b */ SELECT 1",
		"SELECT E'DELETE FROM run_usage'",
		"SELECT 'DELETE FROM run_usage' -- note",
		"GRANT TRUNCATE ON run_usage TO x",
		"CREATE TRIGGER t AFTER TRUNCATE ON run_usage FOR EACH STATEMENT EXECUTE FUNCTION f()",
		"SELECT foo$bar$ FROM t",
		"SELECT 'DELETE FROM run_usage x'",
		"TRUNCATE run_usage_legs RESTART IDENTITY CASCADE",
	}
	for _, s := range good {
		bad, err := unguardedRunUsageWrite(s)
		if err != nil {
			t.Errorf("lexer error on good fixture %q: %v", s, err)
		} else if bad {
			t.Errorf("classified as unguarded writer, want clean: %q", s)
		}
	}
	bad := []string{
		"INSERT INTO run_usage (a) VALUES (1)",
		"INSERT INTO run_usage\n(a) VALUES (1)",
		"UPDATE run_usage SET a = 1",
		"DELETE FROM run_usage WHERE run_id = $1",
		"DELETE FROM run_usage",
		"WITH lk AS (SELECT fn_run_usage_fold_lock($1) AS ok) DELETE FROM run_usage WHERE run_id = $1", // CTE never read
		"DELETE FROM run_usage WHERE run_id = $1 -- fn_run_usage_fold_lock( FROM lk",                   // lock only in a comment
		"TRUNCATE run_usage;",
		// The target ends at ")" or the end of the text, not only at whitespace.
		"WITH d AS (DELETE FROM run_usage) SELECT 1",
		"WITH d AS (DELETE FROM \"run_usage\") SELECT 1",
		"DELETE FROM run_usage",
		"DELETE FROM \"run_usage\"",
		// ONLY with a parenthesis or an adjacent quote.
		"DELETE FROM ONLY (run_usage) WHERE run_id = $1",
		"UPDATE ONLY (run_usage) SET a = 1",
		"TRUNCATE ONLY (run_usage)",
		"DELETE FROM ONLY\"run_usage\" WHERE run_id = $1",
		// A write at the very end of a dollar body.
		"CREATE FUNCTION f() RETURNS void LANGUAGE sql AS $$DELETE FROM run_usage$$;",
		"CREATE FUNCTION f() RETURNS void LANGUAGE sql AS $$TRUNCATE run_usage$$;",
		// Vertical tab is whitespace in PostgreSQL 16+.
		"DELETE\vFROM\vrun_usage\vWHERE\vrun_id = 99",
		"TRUNCATE run_usage\v;",
		// lk is referenced only by a different, unused CTE: the main statement never reads it.
		"WITH lk AS (SELECT fn_run_usage_fold_lock($1) AS ok), other AS (SELECT 1 FROM lk) DELETE FROM run_usage WHERE run_id = $1",
		// Quoted and schema-qualified targets are still run_usage writers.
		"DELETE FROM \"run_usage\" WHERE run_id = $1",
		"DELETE FROM public.run_usage WHERE run_id = $1",
		"DELETE FROM public.\"run_usage\" WHERE run_id = $1",
		"INSERT INTO \"public\".\"run_usage\" (a) VALUES (1)",
		"WITH lk AS (SELECT fn_run_usage_fold_lock($1) AS ok) DELETE FROM \"run_usage\" WHERE run_id = $1",
		// The guard is on a different statement shape: a CTE does the write, the main reads lk.
		"WITH lk AS (SELECT fn_run_usage_fold_lock($1) AS ok), d AS (DELETE FROM run_usage WHERE run_id = $1) SELECT 1 FROM lk",
		// Comments between the keywords still leave a writer.
		"DELETE FROM /* c */ run_usage WHERE run_id = $1",
		"DELETE/**/FROM run_usage WHERE run_id = $1",
		"DELETE FROM /* outer /* inner */ tail */ run_usage WHERE run_id = $1",
		// Quote-looking text that must not hide the writer from the lexer.
		"WITH \"/*\" AS (SELECT 1) DELETE FROM run_usage WHERE run_id = $1",
		"WITH x AS (SELECT E'it\\'s /*' AS s) DELETE FROM run_usage WHERE run_id = $1",
		"WITH x AS (SELECT E'a''\\'/*' AS s) DELETE FROM run_usage WHERE run_id = $1",
		// TRUNCATE lists: run_usage anywhere, qualified or quoted or starred.
		"TRUNCATE run_usage_legs, run_usage",
		"TRUNCATE ONLY x, public.\"run_usage\"",
		"TRUNCATE run_usage, run_usage_legs",
		"DELETE FROM run_usage* WHERE run_id = $1",
		// A plpgsql body whose lock and FROM lk exist only in a comment.
		"DO $$ BEGIN -- WITH lk AS (SELECT fn_run_usage_fold_lock(1)) FROM lk\n DELETE FROM run_usage WHERE run_id = x; END $$",
		// DELETE scope: the lock is on another argument, the WHERE is not scoped, or has an OR.
		"WITH lk AS (SELECT fn_run_usage_fold_lock($1) AS ok) DELETE FROM public.\"run_usage\" WHERE EXISTS (SELECT 1 FROM lk WHERE lk.ok)",
		"WITH lk AS (SELECT fn_run_usage_fold_lock($1) AS ok) DELETE FROM run_usage WHERE run_id = $2 AND EXISTS (SELECT 1 FROM lk WHERE lk.ok)",
		"WITH lk AS (SELECT fn_run_usage_fold_lock($1) AS ok) DELETE FROM run_usage WHERE run_id = $1 OR true AND EXISTS (SELECT 1 FROM lk WHERE lk.ok)",
		"WITH lk AS (SELECT fn_run_usage_fold_lock($1) AS ok) DELETE FROM run_usage WHERE (run_id = $1) AND EXISTS (SELECT 1 FROM lk WHERE lk.ok)",
		"WITH lk AS (SELECT fn_run_usage_fold_lock($1) AS ok) DELETE FROM run_usage WHERE x BETWEEN run_id = $1 AND 2 AND EXISTS (SELECT 1 FROM lk WHERE lk.ok)",
		"WITH lk AS (SELECT fn_run_usage_fold_lock($1) AS ok) DELETE FROM run_usage WHERE CASE WHEN a THEN run_id = $1 AND b END AND EXISTS (SELECT 1 FROM lk WHERE lk.ok)",
	}
	for _, s := range bad {
		got, err := unguardedRunUsageWrite(s)
		if err != nil {
			t.Errorf("lexer error on bad fixture %q: %v", s, err)
		} else if !got {
			t.Errorf("classified as clean, want unguarded writer: %q", s)
		}
	}
	// Each of these must be unguarded for the named reason, so a fixture cannot pass by tripping an
	// earlier check than the one it exists to cover.
	const lk1 = "WITH lk AS (SELECT fn_run_usage_fold_lock($1) AS ok) "
	const lkOK = " EXISTS (SELECT 1 FROM lk)"
	reasons := []struct{ sql, why string }{
		// The writer is not the main statement.
		{"DO $$ BEGIN IF EXISTS (WITH lk AS (SELECT fn_run_usage_fold_lock(1) AS ok) SELECT 1 FROM lk) THEN DELETE FROM run_usage WHERE true; END IF; END $$", "not the single run_usage writer"},
		{"WITH lk AS (SELECT fn_run_usage_fold_lock(@id) AS ok) SELECT 1 FROM lk; DELETE FROM run_usage", "not the single run_usage writer"},
		// Lexer-level hiding.
		{"SELECT E'a'\n'\\' /* ';\nDELETE FROM run_usage;\nSELECT ' */ '", "no WITH clause"},
		{"SELECT 1 -- x\rDELETE FROM run_usage WHERE run_id = $1", "no WITH clause"},
		// Adjacent quotes: no whitespace before a quoted target.
		{"DELETE FROM\"run_usage\" WHERE run_id = $1", "no WITH clause"},
		{"TRUNCATE\"run_usage\"", "no WITH clause"},
		// TRUNCATE stop words are unreserved keywords outside their option position.
		{"TRUNCATE restart.run_usage", "no WITH clause"},
		{"TRUNCATE continue, run_usage", "no WITH clause"},
		{"TRUNCATE cascade.run_usage CASCADE", "no WITH clause"},
		{"TRUNCATE \"on\", run_usage", "no WITH clause"},
		// The lock argument must be a parameter.
		{"WITH lk AS (SELECT fn_run_usage_fold_lock(run_id) AS ok FROM run_usage_fold_state) DELETE FROM run_usage WHERE run_id = run_id AND EXISTS (SELECT 1 FROM lk WHERE lk.ok)", "not a parameter"},
		// DELETE scope: the lock conjunct, the run conjunct, the OR check and the clause end.
		{lk1 + "DELETE FROM run_usage WHERE run_id = $1 AND (true OR" + lkOK + ")", "lacks an EXISTS"},
		{lk1 + "DELETE FROM run_usage WHERE run_id = $1 RETURNING (SELECT 1 FROM lk)", "lacks an EXISTS"},
		{lk1 + "DELETE FROM run_usage WHERE run_id = $1 AND x OR true AND" + lkOK, "top-level OR"},
		{lk1 + "DELETE FROM run_usage WHERE run_id = $1 AND" + lkOK + " OR true", "top-level OR"},
		{lk1 + "DELETE FROM run_usage WHERE x BETWEEN y AND run_id = $1 AND" + lkOK, "lacks a run_id"},
		{lk1 + "DELETE FROM run_usage WHERE CASE WHEN a THEN b AND run_id = $1 AND c END AND" + lkOK, "lacks a run_id"},
		{lk1 + "DELETE FROM run_usage WHERE" + lkOK + " RETURNING a AND run_id = $1", "lacks a run_id"},
		{lk1 + "DELETE FROM run_usage WHERE run_id = $1::uuid AND" + lkOK, "lacks a run_id"},
		// An alias hides the table name: run_usage.run_id resolves to the USING item.
		{lk1 + "DELETE FROM run_usage AS ru USING other AS run_usage WHERE run_usage.run_id = $1 AND" + lkOK, "lacks a run_id"},
	}
	for _, c := range reasons {
		norm, mask, err := lexSQL(c.sql)
		if err != nil {
			t.Errorf("lexer error on %q: %v", c.sql, err)
			continue
		}
		if got := unguardedReason(norm+" ", mask+" "); !strings.Contains(got, c.why) {
			t.Errorf("%q: unguarded reason %q, want it to mention %q", c.sql, got, c.why)
		}
	}
}

func TestSqlcStatementsRejectTrailingStatement(t *testing.T) {
	if _, err := sqlcStatements("-- name: X :exec\nWITH lk AS (SELECT fn_run_usage_fold_lock(@id) AS ok) SELECT 1 FROM lk; DELETE FROM run_usage"); err == nil {
		t.Error("a statement after a top-level \";\" was accepted")
	}
	if _, err := sqlcStatements("-- name: Y :exec\nSELECT 1; -- done\n/* more */\n"); err != nil {
		t.Errorf("trailing comments after \";\" rejected: %v", err)
	}
	if _, err := sqlcStatements("-- name: Z :exec\nSELECT 'a; b';"); err != nil {
		t.Errorf("\";\" inside a string rejected: %v", err)
	}
}

// sqlStmt is one statement of a SQL file, lexed.
type sqlStmt struct {
	name       string // the sqlc query name, "" for migration statements
	norm, mask string
}

// sqlcStatements splits a sqlc query file at each "-- name:" marker (on the raw text, before
// lexing) and lexes each part.
func sqlcStatements(text string) ([]sqlStmt, error) {
	var out []sqlStmt
	for _, part := range regexp.MustCompile(`(?m)^-- name:`).Split(text, -1) {
		norm, mask, err := lexSQL(part)
		if err != nil {
			return nil, err
		}
		var name string
		if f := strings.Fields(strings.SplitN(strings.TrimSpace(part), "\n", 2)[0]); len(f) > 0 {
			name = f[0]
		}
		// A sqlc query is one statement: anything but whitespace and comments after a top-level ";"
		// would be a second statement that the single-statement classifier never splits off.
		if i := strings.IndexByte(mask, ';'); i >= 0 && strings.TrimSpace(norm[i+1:]) != "" {
			return nil, fmt.Errorf("query %q holds a statement after a top-level \";\"", name)
		}
		out = append(out, sqlStmt{name: name, norm: norm + " ", mask: mask + " "})
	}
	return out, nil
}

// migrationViolations returns the statements of a migration's text that write run_usage without
// the fold lock. The whole file is lexed first and split at the ";" positions of the mask, so a
// semicolon or comment marker inside a string or comment never moves a statement boundary.
func migrationViolations(text string) (bad []string, err error) {
	norm, mask, err := lexSQL(text)
	if err != nil {
		return nil, err
	}
	for start := 0; start <= len(mask); {
		end := strings.IndexByte(mask[start:], ';')
		if end < 0 {
			end = len(mask)
		} else {
			end += start
		}
		if unguardedLexed(norm[start:end]+" ", mask[start:end]+" ") {
			bad = append(bad, strings.TrimSpace(norm[start:end]))
		}
		start = end + 1
	}
	return bad, nil
}

func TestMigrationViolations(t *testing.T) {
	mustReport := []string{
		"SELECT E'\\'/*';\nDELETE FROM run_usage WHERE run_id = $1;",
		"SELECT '/*';\nDELETE FROM run_usage WHERE run_id = $1;",
		"DELETE FROM /* a; b */ run_usage;",
		"-- it's fine\nDELETE FROM run_usage WHERE run_id = $1;",
		"SELECT 1; TRUNCATE x, run_usage;",
		"DO $$ BEGIN IF EXISTS (WITH lk AS (SELECT fn_run_usage_fold_lock(1) AS ok) SELECT 1 FROM lk) THEN DELETE FROM run_usage WHERE true; END IF; END $$;",
		"SELECT E'a'\n'\\' /* ';\nDELETE FROM run_usage;\nSELECT ' */ ';",
		"SELECT 1 -- x\rDELETE FROM run_usage WHERE run_id = $1;",
		// A "--" comment between continued string segments, in E mode.
		"SELECT E'a' -- c\n'\\' ' ; DELETE FROM run_usage WHERE run_id = 2; SELECT 'b';",
		"SELECT E'a'\n-- c\n'\\' ' ; DELETE FROM run_usage WHERE run_id = 2; SELECT 'b';",
		"SELECT E'a' -- c\n'\\' /* ';\nDELETE FROM run_usage;\nSELECT ' */ ';",
		"WITH d AS (DELETE FROM run_usage) SELECT 1;",
		"CREATE FUNCTION f() RETURNS void LANGUAGE sql AS $$DELETE FROM run_usage$$;",
		"CREATE FUNCTION f() RETURNS void LANGUAGE sql AS $$TRUNCATE run_usage$$;",
		"DELETE\vFROM\vrun_usage\vWHERE\vrun_id = 99;",
		"TRUNCATE run_usage\v;",
	}
	for _, text := range mustReport {
		bad, err := migrationViolations(text)
		if err != nil {
			t.Errorf("%q: unexpected lexer error %v", text, err)
		} else if len(bad) == 0 {
			t.Errorf("%q: run_usage writer not reported", text)
		}
	}
	clean := []string{
		"SELECT 'a;b'; /* DELETE FROM run_usage; */ SELECT 2;",
		"-- DELETE FROM run_usage;\nSELECT 1;",
		"WITH lk AS (SELECT fn_run_usage_fold_lock($1) AS ok) DELETE FROM run_usage WHERE run_id = $1 AND EXISTS (SELECT 1 FROM lk);",
	}
	for _, text := range clean {
		if bad, err := migrationViolations(text); err != nil || len(bad) != 0 {
			t.Errorf("%q: got %v, %v want no violations", text, bad, err)
		}
	}
	if _, err := migrationViolations("SELECT 1; /* unterminated"); err == nil {
		t.Error("unterminated block comment: want a lexer error")
	}
}

func TestRunUsageWritersTakeFoldLock_SQL(t *testing.T) {
	var scanned, writers int
	files, err := filepath.Glob("queries/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("queries glob: %v (%d files)", err, len(files))
	}
	guardedQueries := map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // G304: f comes from a Glob over this package's own queries/migrations dirs, not from input.
		if err != nil {
			t.Fatal(err)
		}
		stmts, err := sqlcStatements(string(b))
		if err != nil {
			t.Errorf("%s: SQL lexer error: %v", f, err)
			continue
		}
		for _, st := range stmts {
			scanned++
			if len(runUsageWrites(st.norm, st.mask)) > 0 {
				writers++
				if unguardedLexed(st.norm, st.mask) {
					t.Errorf("%s: statement %q writes run_usage without an fn_run_usage_fold_lock CTE it reads (FROM lk), scoped to the locked run", f, st.name)
				} else {
					guardedQueries[st.name] = true
				}
			}
		}
	}
	for _, want := range []string{"UpsertRunUsage", "DeleteRunUsage"} {
		if !guardedQueries[want] {
			t.Errorf("non-vacuity: guarded writer %s not found in queries/*.sql (found %v)", want, guardedQueries)
		}
	}
	if scanned < 100 || writers == 0 {
		t.Errorf("non-vacuity: scanned %d statements, %d run_usage writers", scanned, writers)
	}

	migs, err := filepath.Glob("migrations/*.sql")
	if err != nil || len(migs) == 0 {
		t.Fatalf("migrations glob: %v", err)
	}
	foldMig := foldSchemaMigration(t, migs)
	var later int
	for _, f := range migs {
		base := filepath.Base(f)
		n, err := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
		if err != nil {
			t.Fatalf("migration %s has no numeric prefix", base)
		}
		if n <= foldMig {
			continue
		}
		later++
		b, err := os.ReadFile(f) //nolint:gosec // G304: f comes from a Glob over this package's own queries/migrations dirs, not from input.
		if err != nil {
			t.Fatal(err)
		}
		bad, err := migrationViolations(string(b))
		if err != nil {
			t.Errorf("%s: SQL lexer error: %v", f, err)
			continue
		}
		for _, st := range bad {
			t.Errorf("%s: statement writes run_usage without calling fn_run_usage_fold_lock (see the manual DML rule in migration 00549): %q", base, st)
		}
	}
	t.Logf("%d query statements (%d run_usage writers), %d migrations above 00549", scanned, writers, later)
}

// apiGoFiles lists the non-test Go files under api/ outside the sqlc output.
func apiGoFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir("../..", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		if strings.HasSuffix(p, ".sql.go") && filepath.Base(filepath.Dir(p)) == "store" {
			return nil
		}
		out = append(out, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 200 {
		t.Fatalf("non-vacuity: walked only %d Go files", len(out))
	}
	sort.Strings(out)
	return out
}

func TestRunUsageWritersTakeFoldLock_Go(t *testing.T) {
	allowed := map[string]bool{"foldUsageFrames": true, "foldPrefixedModelUsage": true, "RefoldRunUsage": true}
	guarded := map[string]bool{"UpsertRunUsage": true, "DeleteRunUsage": true}
	files := apiGoFiles(t)
	fset := token.NewFileSet()
	seenCalls := map[string]int{}
	for _, f := range files {
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		// (ii) raw SQL in string literals, and (iii) the guarded names as strings.
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if writesRunUsage(s) {
				t.Errorf("%s: raw SQL writing run_usage in a Go string; route it through the guarded sqlc queries", fset.Position(lit.Pos()))
			}
			if guarded[s] {
				t.Errorf("%s: string literal %q (reflection or by-name dispatch bypasses the call-site check)", fset.Position(lit.Pos()), s)
			}
			return true
		})
		// (iii) call sites.
		for _, decl := range file.Decls {
			fn, isFn := decl.(*ast.FuncDecl)
			name := ""
			if isFn {
				name = fn.Name.Name
			}
			callFuns := map[ast.Node]bool{}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					callFuns[x.Fun] = true
				case *ast.SelectorExpr:
					if !guarded[x.Sel.Name] {
						return true
					}
					pos := fset.Position(x.Pos())
					if !callFuns[x] {
						t.Errorf("%s: .%s used as a value, not called directly", pos, x.Sel.Name)
						return true
					}
					if !allowed[name] {
						t.Errorf("%s: .%s called from %q; only foldUsageFrames, foldPrefixedModelUsage and RefoldRunUsage may write run_usage", pos, x.Sel.Name, name)
					}
					seenCalls[name+"."+x.Sel.Name]++
				}
				return true
			})
		}
	}
	for _, want := range []string{"foldUsageFrames.UpsertRunUsage", "foldPrefixedModelUsage.UpsertRunUsage", "RefoldRunUsage.DeleteRunUsage"} {
		if seenCalls[want] == 0 {
			t.Errorf("non-vacuity: call site %s not found (found %v)", want, seenCalls)
		}
	}
}

// refoldProtocolViolations parses a Go source file declaring RefoldRunUsage and returns what is
// wrong with its transaction protocol. BeginTx and LockRunForUsageRefold must each be the WHOLE
// expression of a top-level statement of the function (an expression statement, an assignment
// right-hand side, or either as the Init of a top-level if): any other occurrence of either name
// (an operand, a call argument, defer, go, a func literal, an if/else body, a loop, a switch, a
// method value) is a violation, and so is any goto or label. The transaction must begin with a
// BeginTx whose options are a pgx.TxOptions literal with IsoLevel: pgx.ReadCommitted (never a bare
// Begin), the lock must follow BeginTx, and DeleteRunUsage must be called only after the lock.
func refoldProtocolViolations(t *testing.T, filename string, src any) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "RefoldRunUsage" || fn.Body == nil {
			continue
		}
		// The whole-expression calls of top-level statements.
		top := map[*ast.CallExpr]bool{}
		addCall := func(e ast.Expr) {
			if c, ok := e.(*ast.CallExpr); ok {
				top[c] = true
			}
		}
		addSimple := func(s ast.Stmt) {
			switch x := s.(type) {
			case *ast.ExprStmt:
				addCall(x.X)
			case *ast.AssignStmt:
				for _, r := range x.Rhs {
					addCall(r)
				}
			}
		}
		for _, s := range fn.Body.List {
			addSimple(s)
			if is, ok := s.(*ast.IfStmt); ok && is.Init != nil {
				addSimple(is.Init)
			}
		}
		var beginTx, lock *ast.CallExpr
		var delPos token.Pos
		var bareBegin, nested, jump bool
		// Selectors that are the Fun of a top-level call; every other mention of the names counts.
		topFun := map[ast.Node]*ast.CallExpr{}
		for c := range top {
			if sel, ok := c.Fun.(*ast.SelectorExpr); ok {
				topFun[sel] = c
			}
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BranchStmt:
				if x.Tok == token.GOTO {
					jump = true
				}
			case *ast.LabeledStmt:
				jump = true
			case *ast.SelectorExpr:
				c, isTop := topFun[x]
				switch x.Sel.Name {
				case "BeginTx":
					if !isTop || beginTx != nil {
						nested = true
					} else {
						beginTx = c
					}
				case "LockRunForUsageRefold":
					if !isTop || lock != nil {
						nested = true
					} else {
						lock = c
					}
				}
			case *ast.CallExpr:
				sel, ok := x.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "Begin":
					bareBegin = true
				case "DeleteRunUsage":
					if delPos == token.NoPos {
						delPos = x.Pos()
					}
				}
			}
			return true
		})
		if nested {
			out = append(out, "BeginTx and LockRunForUsageRefold must each be the whole expression of a top-level statement of RefoldRunUsage (no defer, go, func literal, branch, loop, operand or argument)")
		}
		if jump {
			out = append(out, "RefoldRunUsage must contain no goto or label: a jump could skip the lock")
		}
		if lock == nil || delPos == token.NoPos {
			return append(out, "RefoldRunUsage must CALL LockRunForUsageRefold and DeleteRunUsage")
		}
		if lock.Pos() > delPos {
			out = append(out, "DeleteRunUsage is called before LockRunForUsageRefold")
		}
		if bareBegin || beginTx == nil || !readCommittedOptions(beginTx) {
			out = append(out, "the transaction must start with BeginTx(..., pgx.TxOptions{IsoLevel: pgx.ReadCommitted}), not Begin")
		} else if beginTx.Pos() > lock.Pos() {
			out = append(out, "BeginTx must precede LockRunForUsageRefold")
		}
		return out
	}
	return append(out, "RefoldRunUsage not found")
}

// readCommittedOptions reports whether the BeginTx call's options argument is a pgx.TxOptions
// composite literal with the key-value IsoLevel: pgx.ReadCommitted.
func readCommittedOptions(call *ast.CallExpr) bool {
	if len(call.Args) != 2 {
		return false
	}
	lit, ok := call.Args[1].(*ast.CompositeLit)
	if !ok || !isPgxSelector(lit.Type, "TxOptions") {
		return false
	}
	for _, e := range lit.Elts {
		kv, ok := e.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "IsoLevel" && isPgxSelector(kv.Value, "ReadCommitted") {
			return true
		}
	}
	return false
}

func isPgxSelector(e ast.Expr, sel string) bool {
	s, ok := e.(*ast.SelectorExpr)
	if !ok || s.Sel.Name != sel {
		return false
	}
	x, ok := s.X.(*ast.Ident)
	return ok && x.Name == "pgx"
}

func TestRefoldRunUsageLocksRunBeforeDelete(t *testing.T) {
	const path = "../workersvc/usage_refold.go"
	for _, v := range refoldProtocolViolations(t, path, nil) {
		t.Error(v)
	}

	const head = "package p\nfunc RefoldRunUsage() {\n"
	const begin = "tx, _ := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})\n"
	const lock = "qtx.LockRunForUsageRefold(ctx, id)\n"
	const del = "qtx.DeleteRunUsage(ctx, id)\n}\n"
	bad := map[string]struct{ src, want string }{
		"method value, not a call":   {head + begin + "f := qtx.LockRunForUsageRefold\n_ = f\n" + del, "whole expression"},
		"lock after delete":          {head + begin + del[:len(del)-2] + lock + "}\n", "before LockRunForUsageRefold"},
		"no lock":                    {head + begin + del, "must CALL"},
		"bare Begin":                 {head + "tx, _ := pool.Begin(ctx)\n" + lock + del, "not Begin"},
		"BeginTx not read committed": {head + "tx, _ := pool.BeginTx(ctx, pgx.TxOptions{})\n" + lock + del, "ReadCommitted"},
		"defer lock":                 {head + begin + "defer qtx.LockRunForUsageRefold(ctx, id)\n" + del, "whole expression"},
		"lock in if body":            {head + begin + "if cond {\n" + lock + "}\n" + del, "whole expression"},
		"lock in func literal":       {head + begin + "func() {\n" + lock + "}()\n" + del, "whole expression"},
		"nested lock before real":    {head + begin + "if cond {\n" + lock + "}\n" + lock + del, "whole expression"},
		"lock as operand":            {head + begin + "ok := cond && qtx.LockRunForUsageRefold(ctx, id) != nil\n_ = ok\n" + del, "whole expression"},
		"lock as call argument":      {head + begin + "maybe(qtx.LockRunForUsageRefold(ctx, id))\n" + del, "whole expression"},
		"goto skips lock":            {head + begin + "if c {\ngoto del\n}\n" + lock + "del:\n" + del, "goto or label"},
		"label without goto":         {head + begin + lock + "done:\n" + del, "goto or label"},
		"go lock":                    {head + begin + "go qtx.LockRunForUsageRefold(ctx, id)\n" + del, "whole expression"},
		"BeginTx in if body":         {head + "if c {\ntx, _ := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})\n}\n" + lock + del, "whole expression"},
		"ReadCommitted in other arg": {head + "tx, _ := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: m(pgx.ReadCommitted)})\n" + lock + del, "ReadCommitted"},
		"ReadCommitted in ctx arg":   {head + "tx, _ := pool.BeginTx(withLevel(ctx, pgx.ReadCommitted), pgx.TxOptions{})\n" + lock + del, "ReadCommitted"},
	}
	for name, c := range bad {
		got := strings.Join(refoldProtocolViolations(t, "fixture.go", c.src), "; ")
		if got == "" {
			t.Errorf("fixture %q accepted, want rejected", name)
		} else if !strings.Contains(got, c.want) {
			t.Errorf("fixture %q rejected for the wrong reason: %q, want it to mention %q", name, got, c.want)
		}
	}
	goods := map[string]string{
		"plain":      head + begin + lock + del,
		"if init":    head + begin + "if _, err := qtx.LockRunForUsageRefold(ctx, id); err != nil {\nreturn\n}\n" + del,
		"assignment": head + begin + "_, err := qtx.LockRunForUsageRefold(ctx, id)\n_ = err\n" + del,
	}
	for name, src := range goods {
		if v := refoldProtocolViolations(t, "fixture.go", src); len(v) != 0 {
			t.Errorf("good fixture %q rejected: %v", name, v)
		}
	}
}

// foldSchemaMigration returns the number of the migration that creates the fold schema
// (run_usage_model_folds), so the writer guard scans every migration after it without a
// hard-coded number that a landing renumber would leave stale.
func foldSchemaMigration(t *testing.T, migs []string) int {
	t.Helper()
	for _, f := range migs {
		base := filepath.Base(f)
		if strings.HasSuffix(base, "_run_usage_model_folds.sql") {
			n, err := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
			if err != nil {
				t.Fatalf("fold schema migration %s has no numeric prefix", base)
			}
			return n
		}
	}
	t.Fatal("no *_run_usage_model_folds.sql migration found")
	return 0
}
