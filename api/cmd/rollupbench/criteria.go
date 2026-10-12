package main

import (
	"fmt"
	"sort"
	"strings"
)

// Criterion is one pass/fail bar of the plan evaluated for one config. Pass is nil when the
// harness could not evaluate it.
type Criterion struct {
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	Pass   *bool          `json:"pass"`
	Detail map[string]any `json:"detail,omitempty"`
	Note   string         `json:"note,omitempty"`
}

func verdict(id, name string, pass bool, detail map[string]any) Criterion {
	return Criterion{ID: id, Name: name, Pass: &pass, Detail: detail}
}

// LiveBaseline holds the live-path measurements the archived cells are compared against.
type LiveBaseline struct {
	Auto     []Cell   `json:"autocommit"`
	Tx       []Cell   `json:"in_repeatable_read_tx"`
	ChatAuto ChatCell `json:"chat_list_autocommit"`
	ChatTx   ChatCell `json:"chat_list_in_tx"`
}

func findCell(cells []Cell, shape, stratum string, readers int) *Cell {
	for i := range cells {
		c := &cells[i]
		if c.Shape == shape && c.Stratum == stratum && c.Readers == readers {
			return c
		}
	}
	return nil
}

// cmpRow is one archived-vs-live comparison.
type cmpRow struct {
	Shape, Stratum string
	ArchP95        float64
	LiveP95        float64
	Ratio          float64
	Ok             bool
	Why            string
}

// liveMultiple is the bar for an archived read against the same read on the live table in a transaction.
const liveMultiple = 3

// minSamplesP95 is the sample count below which a p95 is not trusted for a shape that opted in
// (the full replay): the evaluation then uses p50 and says so.
const minSamplesP95 = 50

// cellProblem says why a measured cell cannot support a verdict ("" when it can).
func cellProblem(c *Cell, what string) string {
	switch {
	case c == nil:
		return what + " cell missing"
	case c.Iterations == 0:
		return what + " cell has no samples"
	case c.Errors > 0:
		return fmt.Sprintf("%s cell had %d errors", what, c.Errors)
	}
	return ""
}

// compare evaluates arch p95 <= absMs (when absMs > 0) and <= liveMultiple * live tx p95 at
// `readers`. The archived figure is the worse of the before-vacuum and after-vacuum
// measurements. A stratum measured on neither side is skipped (its pool was empty); a cell that
// is missing on one side, has no samples or had errors fails its row, and a group with no row at
// all fails. With needN > 0, a cell pair with fewer than needN samples is judged on p50 instead.
func compare(arch, archPost []Cell, live *LiveBaseline, shapes, strata []string, readers int, absMs float64, needN int) ([]cmpRow, bool) {
	var rows []cmpRow
	all := true
	for _, sh := range shapes {
		for _, st := range strata {
			a := findCell(arch, sh, st, readers)
			b := findCell(archPost, sh, st, readers)
			l := findCell(live.Tx, sh, st, readers)
			if a == nil && l == nil && b == nil {
				continue
			}
			r := cmpRow{Shape: sh, Stratum: st, Ok: true}
			if why := firstNonEmpty(cellProblem(a, "archived"), cellProblem(b, "archived after-vacuum"), cellProblem(l, "live")); why != "" {
				r.Ok, r.Why = false, why
				all = false
				rows = append(rows, r)
				continue
			}
			pick := func(l Latency) float64 { return l.P95Ms }
			if needN > 0 && (a.Latency.N < needN || b.Latency.N < needN || l.Latency.N < needN) {
				pick = func(l Latency) float64 { return l.P50Ms }
				r.Why = fmt.Sprintf("fewer than %d samples: judged on p50", needN)
			}
			p := max(pick(a.Latency), pick(b.Latency))
			r.ArchP95, r.LiveP95 = p, pick(l.Latency)
			if r.LiveP95 <= 0 {
				r.Ok, r.Why = false, "live latency not measured (zero)"
				all = false
				rows = append(rows, r)
				continue
			}
			r.Ratio = p / r.LiveP95
			if absMs > 0 && p > absMs {
				r.Ok, r.Why = false, "over the absolute bound"
			}
			if p > liveMultiple*r.LiveP95 {
				r.Ok = false
				r.Why += " over the live multiple"
			}
			all = all && r.Ok
			rows = append(rows, r)
		}
	}
	if len(rows) == 0 {
		return []cmpRow{{Shape: fmt.Sprint(shapes), Why: "no cell was measured for this group"}}, false
	}
	return rows, all
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func rowsDetail(rows []cmpRow) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{"shape": r.Shape, "stratum": r.Stratum, "archived_p95_ms": r.ArchP95, "live_tx_p95_ms": r.LiveP95, "ratio": r.Ratio, "ok": r.Ok, "why": r.Why})
	}
	return out
}

var allStrata = []string{"small", "median", "large"}

// critReaders is the reader count the latency bars are evaluated at: 8 when it was measured,
// else the largest measured count below 8, else the smallest measured.
func critReaders(rs []int) int {
	best, smallest := 0, rs[0]
	for _, r := range rs {
		smallest = min(smallest, r)
		if r <= 8 {
			best = max(best, r)
		}
	}
	if best == 0 {
		return smallest
	}
	return best
}

// evaluate fills Criteria and Overall for a config. Thresholds are the plan's, written as
// constants here so a changed bar is a visible diff. A bar whose inputs were not measured fails
// with the reason; it is never skipped. largestTerminalMsg is the largest payload among the
// runs that get rolled up, largestMsg the largest of the whole corpus.
func evaluate(r *ConfigResult, live *LiveBaseline, a *Attrib, readers []int, largestMsg, largestTerminalMsg int64) {
	maxReaders := 1
	for _, n := range readers {
		maxReaders = max(maxReaders, n)
	}
	cr := critReaders(readers)
	var out []Criterion
	bound := int64(r.ByteBoundKiB) << 10
	pre := r.C1.Pre

	// C1: footprint of archived runs, real bytes on both sides (see C1Basis).
	{
		d := map[string]any{"basis": r.C1.Basis, "archived_pre_bytes": pre, "archived_post_bytes": r.C1.Post, "detail": r.C1}
		ratio := 0.0
		if pre > 0 {
			ratio = float64(r.C1.Post) / float64(pre)
		}
		after, _ := r.Sizes["after_vacuum_run_messages"].(RelReport)
		free := after.Heap.FreeSpace
		if after.Toast != nil {
			free += after.Toast.FreeSpace
		}
		reusable := max(0, free-a.FreeSpaceBefore)
		reuse := 0.0
		if pre > 0 {
			reuse = float64(reusable) / float64(pre)
		}
		d["post_over_pre"] = ratio
		d["reusable_free_space_bytes_after_vacuum"] = reusable
		d["reusable_over_pre"] = reuse
		c := verdict("C1", "footprint <= 50% (pre: VACUUM FULL baseline of the archived runs; post: chunk + watermark tables after plain VACUUM, insert-only) AND reusable space after plain VACUUM >= 40%", pre > 0 && ratio <= 0.5 && reuse >= 0.4, d)
		c.Note = r.C1.Note
		if pre <= 0 {
			c.Note = "pre-rollup footprint of the archived runs is zero: nothing was archived or measured"
		}
		out = append(out, c)
	}

	// C2: first/tail/backward pages, 1000-row pages and full replay.
	{
		rows, ok := compare(r.Latency.PreVacuum, r.Latency.PostVacuum, live, []string{"first_page_200", "tail_200", "mid_backward_200"}, allStrata, cr, 50, 0)
		rows2, ok2 := compare(r.Latency.PreVacuum, r.Latency.PostVacuum, live, []string{"first_page_1000", "tail_1000"}, allStrata, cr, 0, 0)
		rows3, ok3 := compare(r.Latency.PreVacuum, r.Latency.PostVacuum, live, []string{"full_replay"}, []string{"largest"}, cr, 0, minSamplesP95)
		d := map[string]any{"pages_200": rowsDetail(rows), "pages_1000": rowsDetail(rows2), "full_replay": rowsDetail(rows3)}
		c := verdict("C2", "200-row pages <= 50 ms and <= 3x live (tx) at 8 readers; 1000-row pages and full replay <= 3x live", ok && ok2 && ok3, d)
		c.Note = "32 readers are reported in the latency cells only"
		out = append(out, c)
	}

	// C3: judge, jobs, usage, chat.
	{
		var problems []string
		judge := findCell(r.Latency.PreVacuum, "judge_signal_scan_4000", "large", cr)
		judgePost := findCell(r.Latency.PostVacuum, "judge_signal_scan_4000", "large", cr)
		p95 := 0.0
		if why := firstNonEmpty(cellProblem(judge, "judge signal scan"), cellProblem(judgePost, "judge signal scan after-vacuum")); why != "" {
			problems = append(problems, why)
		} else {
			p95 = max(judge.Latency.P95Ms, judgePost.Latency.P95Ms)
		}
		rows, ok := compare(r.Latency.PreVacuum, r.Latency.PostVacuum, live, []string{"judge_trace_page_200", "jobs_text_200", "usage_frames_scan"}, allStrata, cr, 0, 0)
		chatRatio := 0.0
		switch {
		case live.ChatTx.Latency.P95Ms <= 0 || live.ChatTx.Latency.N == 0 || live.ChatTx.Errors > 0:
			problems = append(problems, "chat list: live baseline not measured")
		case len(r.Latency.Chat) == 0:
			problems = append(problems, "chat list: no archived cell")
		default:
			for _, c := range r.Latency.Chat {
				if c.Latency.N == 0 || c.Errors > 0 {
					problems = append(problems, "chat list: an archived cell has no samples or had errors")
					continue
				}
				ratio := c.Latency.P95Ms / live.ChatTx.Latency.P95Ms
				chatRatio = max(chatRatio, ratio)
				if ratio > 2 {
					problems = append(problems, "chat list over 2x live")
				}
			}
		}
		d := map[string]any{"judge_signal_scan_large_p95_ms": p95, "others": rowsDetail(rows), "chat_list_worst_ratio": chatRatio, "chat_runs": live.ChatTx.Chats, "latency_bars_evaluated_at_readers": cr}
		if p95 > 500 {
			problems = append(problems, "judge signal scan over 500 ms")
		}
		if len(problems) > 0 {
			d["problems"] = problems
		}
		out = append(out, verdict("C3", "judge signal scan <= 500 ms on the large stratum; trace page, jobs read, usage scan <= 3x live; chat list <= 2x live",
			len(problems) == 0 && ok, d))
	}

	// C4: correctness.
	{
		d := map[string]any{
			"roundtrip_runs": r.RoundTrip.Runs, "roundtrip_mismatches": r.RoundTrip.Mismatches, "roundtrip_errors": r.RoundTrip.Errors,
			"oversized_messages_verified": r.RoundTrip.OversizedVerified, "tx_roundtrip_failures": r.Backfill.RoundTripFails,
			"reader_digest_mismatches": r.Digests.Mismatches, "reader_digest_errors": r.Digests.Errors,
		}
		ok := r.RoundTrip.Runs > 0 && r.RoundTrip.Mismatches == 0 && r.RoundTrip.Errors == 0 && r.Backfill.RoundTripFails == 0 &&
			r.Digests.Mismatches == 0 && r.Digests.Errors == 0
		var notes []string
		oversizedExpected := largestTerminalMsg > bound
		if oversizedExpected {
			d["oversized"] = "verified: the corpus has a message larger than the chunk byte bound"
			if r.RoundTrip.OversizedVerified == 0 {
				ok = false
				notes = append(notes, "the corpus has a message larger than the chunk byte bound but none was verified")
			}
		} else {
			d["oversized"] = "not applicable (corpus has none larger than the chunk byte bound in a rolled-up run)"
		}
		if c := r.Concurrency; c != nil {
			d["concurrency"] = c
			ok = ok && c.Failure == "" && c.Violations == 0 && c.MissingSeqs == 0 && c.UnexpectedSeqs == 0 && c.ReadErrors == 0 && c.StragglersLost == 0 &&
				c.StragglerInsertErrors == 0 && c.StragglerInserts > 0 && c.Reads > 0 &&
				c.RerollRoundTripFails == 0 && c.BackfillRoundTripFail == 0
			if c.Failure != "" {
				notes = append(notes, "the concurrency check did not complete: "+c.Failure)
			}
			if c.RoundTrip == nil || c.Digests == nil {
				ok = false
				notes = append(notes, "the round trip and reader digests after the concurrent backfill were not recorded")
			} else {
				ok = ok && c.RoundTrip.Runs > 0 && c.RoundTrip.Mismatches == 0 && c.RoundTrip.Errors == 0 && c.Digests.Mismatches == 0 && c.Digests.Errors == 0
			}
			if c.StragglerOverlapped == 0 {
				ok = false
				notes = append(notes, "straggler inserts overlapping an open rollup transaction were not demonstrated (none was blocked by one)")
			}
			if c.MixedOnMultiTxRun == 0 {
				ok = false
				notes = append(notes, "mixed archived+live state of a run rolled in several transactions was NOT observed; that half of the check is unproven for this config")
			}
		} else {
			notes = append(notes, "concurrent reader and straggler checks run on the first config of each codec only")
		}
		c := verdict("C4", "100% round trip incl. oversized messages; zero duplicate, missing or lost seqs under concurrency", ok, d)
		c.Note = strings.Join(notes, "; ")
		out = append(out, c)
	}

	// C5: cost, from the clean clone.
	{
		fp := r.Backfill.Footprint
		walRatio := 0.0
		finalVsPre := 0.0
		if pre > 0 {
			walRatio = float64(r.WALTotal) / float64(pre)
			finalVsPre = float64(fp.FinalChunks) / float64(pre)
		}
		extraBound := int64(1.1*float64(fp.FinalChunks)) + 64<<20
		d := map[string]any{
			"wal_total_bytes": r.WALTotal, "wal_over_archived_pre": walRatio, "wal_mb_per_s_rollup": r.Backfill.WAL.WALBytesPerSc,
			"pg_stat_wal_settled_rollup_phase": r.Backfill.WAL.StatsSettled,
			"peak_extra_bytes":                 fp.PeakExtra, "peak_extra_bound_bytes": extraBound, "final_chunk_table_bytes": fp.FinalChunks,
			"final_chunk_over_archived_pre": finalVsPre, "largest_tx_ms": r.Backfill.LargestTx.DurationMs,
			"rewrite_headroom_bytes_reported_only": r.Rewrite.Headroom, "footprint_basis": r.C1.Basis,
		}
		var problems []string
		if pre <= 0 || r.Backfill.Txs == 0 {
			problems = append(problems, "nothing was rolled up or the pre-rollup footprint is zero")
		}
		ok := pre > 0 && r.Backfill.Txs > 0 && walRatio <= 2.5 && fp.PeakExtra <= extraBound && finalVsPre <= 0.35 && r.Backfill.LargestTx.DurationMs <= 30_000
		ll := r.Backfill.LiveLoad
		d["live_reader_p95_ratio_during_vs_idle"] = ll.Ratio
		var note string
		switch {
		case ll.NotMeasure != "":
			// An unevaluated sub-bar is not a pass: a corpus without live runs (every
			// --corpus-ndjson run is terminal) must not report C5 green.
			ok = false
			problems = append(problems, "live-reader ratio not evaluated: "+ll.NotMeasure)
		case ll.IdleP95Ms <= 0 || ll.DuringN == 0:
			ok = false
			problems = append(problems, "live-reader ratio not measured (no idle or no during-backfill samples)")
		default:
			ok = ok && ll.Ratio <= 1.5
		}
		if len(problems) > 0 {
			d["problems"] = problems
			note = strings.TrimSpace(note + " " + strings.Join(problems, "; "))
		}
		c := verdict("C5", "WAL <= 2.5x pre-rollup bytes; peak extra disk <= 1.1x final chunks + 64 MiB; chunks <= 35% of archived pre; largest tx <= 30 s; live readers <= 1.5x idle", ok, d)
		c.Note = note
		out = append(out, c)
	}

	// C6: memory.
	{
		var worst float64
		var failing, problems []string
		single := 0
		for _, c := range r.Latency.PreVacuum {
			if c.Readers != 1 {
				continue
			}
			if c.Iterations == 0 || c.Errors > 0 {
				problems = append(problems, fmt.Sprintf("%s/%s: no samples or errors", c.Shape, c.Stratum))
				continue
			}
			single++
			limit := 4*c.PayloadBytesPerRq + float64(bound)
			if c.AllocBytesPerReq > limit {
				failing = append(failing, fmt.Sprintf("%s/%s", c.Shape, c.Stratum))
			}
			if limit > 0 {
				worst = max(worst, c.AllocBytesPerReq/limit)
			}
		}
		sort.Strings(failing)
		var peak, base uint64
		peakCells := 0
		for _, c := range r.Latency.PreVacuum {
			if c.Readers == maxReaders && c.Iterations > 0 && c.PeakHeapInuse > 0 {
				peakCells++
				peak = max(peak, c.PeakHeapOverBaseline)
				base = max(base, c.HeapBaseline)
			}
		}
		if single == 0 {
			problems = append(problems, "no single-reader cell was measured")
		}
		if peakCells == 0 {
			problems = append(problems, "no heap peak was sampled at the highest reader count")
		}
		heapBound := uint64(maxReaders)*uint64(bound+largestMsg) + 64<<20 //nolint:gosec // G115: positive sizes and a small reader count.
		d := map[string]any{"cells_over_the_alloc_bound": failing, "worst_alloc_over_bound": worst,
			"peak_heap_over_cell_baseline_bytes": peak, "largest_cell_baseline_heap_inuse_bytes": base, "peak_heap_bound_bytes": heapBound, "readers_for_peak": maxReaders}
		if len(problems) > 0 {
			d["problems"] = problems
		}
		c := verdict("C6", "per-request allocation <= 4x bytes returned + one chunk bound; peak heap above the cell's own baseline at max readers <= readers*(chunk bound + largest message) + 64 MiB",
			len(failing) == 0 && len(problems) == 0 && peak <= heapBound, d)
		c.Note = strings.Join(problems, "; ")
		out = append(out, c)
	}

	out = append(out, Criterion{ID: "C7", Name: "reader inventory", Note: "C7: not evaluated by harness"})
	r.Criteria = out
	r.Overall = "pass"
	for _, c := range out {
		if c.Pass != nil && !*c.Pass {
			r.Overall = "fail"
			break
		}
	}
}
