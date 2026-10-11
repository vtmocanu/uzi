package main

import (
	"fmt"
	"sort"
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

// compare evaluates arch p95 <= absMs (when absMs > 0) and <= liveMultiple * live tx p95 at `readers`.
// The archived figure is the worse of the before-vacuum and after-vacuum measurements.
func compare(arch, archPost []Cell, live *LiveBaseline, shapes, strata []string, readers int, absMs float64) ([]cmpRow, bool) {
	var rows []cmpRow
	all := true
	for _, sh := range shapes {
		for _, st := range strata {
			a := findCell(arch, sh, st, readers)
			l := findCell(live.Tx, sh, st, readers)
			if a == nil || l == nil || a.Iterations == 0 || l.Iterations == 0 {
				continue
			}
			p := a.Latency.P95Ms
			if b := findCell(archPost, sh, st, readers); b != nil && b.Iterations > 0 {
				p = max(p, b.Latency.P95Ms)
			}
			r := cmpRow{Shape: sh, Stratum: st, ArchP95: p, LiveP95: l.Latency.P95Ms, Ok: true}
			if l.Latency.P95Ms > 0 {
				r.Ratio = p / l.Latency.P95Ms
			}
			if absMs > 0 && p > absMs {
				r.Ok, r.Why = false, "over the absolute bound"
			}
			if p > liveMultiple*l.Latency.P95Ms {
				r.Ok = false
				r.Why += " over the live multiple"
			}
			all = all && r.Ok
			rows = append(rows, r)
		}
	}
	return rows, all
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
// constants here so a changed bar is a visible diff.
func evaluate(r *ConfigResult, live *LiveBaseline, a *Attrib, readers []int, largestMsg int64) {
	maxReaders := 1
	for _, n := range readers {
		maxReaders = max(maxReaders, n)
	}
	cr := critReaders(readers)
	var out []Criterion
	bound := int64(r.ByteBoundKiB) << 10

	// C1: footprint of archived runs.
	{
		d := map[string]any{"archived_pre_bytes": a.ArchivedPre, "archived_post_vacuum_full_bytes": r.archivedPost}
		ratio := 0.0
		if a.ArchivedPre > 0 {
			ratio = float64(r.archivedPost) / float64(a.ArchivedPre)
		}
		after, _ := r.Sizes["after_vacuum_run_messages"].(RelReport)
		free := after.Heap.FreeSpace
		if after.Toast != nil {
			free += after.Toast.FreeSpace
		}
		reusable := max(0, free-a.FreeSpaceBefore)
		reuse := 0.0
		if a.ArchivedPre > 0 {
			reuse = float64(reusable) / float64(a.ArchivedPre)
		}
		d["post_over_pre"] = ratio
		d["reusable_free_space_bytes_after_vacuum"] = reusable
		d["reusable_over_pre"] = reuse
		out = append(out, verdict("C1", "post-VACUUM-FULL footprint <= 50% of pre-rollup AND reusable space after plain VACUUM >= 40%", ratio <= 0.5 && reuse >= 0.4, d))
	}

	// C2: first/tail/backward pages, 1000-row pages and full replay.
	{
		rows, ok := compare(r.Latency.PreVacuum, r.Latency.PostVacuum, live, []string{"first_page_200", "tail_200", "mid_backward_200"}, allStrata, cr, 50)
		rows2, ok2 := compare(r.Latency.PreVacuum, r.Latency.PostVacuum, live, []string{"first_page_1000", "tail_1000"}, allStrata, cr, 0)
		rows3, ok3 := compare(r.Latency.PreVacuum, r.Latency.PostVacuum, live, []string{"full_replay"}, []string{"largest"}, cr, 0)
		d := map[string]any{"pages_200": rowsDetail(rows), "pages_1000": rowsDetail(rows2), "full_replay": rowsDetail(rows3)}
		c := verdict("C2", "200-row pages <= 50 ms and <= 3x live (tx) at 8 readers; 1000-row pages and full replay <= 3x live", ok && ok2 && ok3 && len(rows) > 0, d)
		c.Note = "32 readers are reported in the latency cells only"
		out = append(out, c)
	}

	// C3: judge, jobs, usage, chat.
	{
		var judge *Cell
		if c := findCell(r.Latency.PreVacuum, "judge_signal_scan_4000", "large", cr); c != nil {
			judge = c
		}
		p95 := 0.0
		if judge != nil {
			p95 = judge.Latency.P95Ms
			if b := findCell(r.Latency.PostVacuum, "judge_signal_scan_4000", "large", cr); b != nil {
				p95 = max(p95, b.Latency.P95Ms)
			}
		}
		rows, ok := compare(r.Latency.PreVacuum, r.Latency.PostVacuum, live, []string{"judge_trace_page_200", "jobs_text_200", "usage_frames_scan"}, allStrata, cr, 0)
		chatOK, chatRatio := true, 0.0
		for _, c := range r.Latency.Chat {
			if live.ChatTx.Latency.P95Ms > 0 {
				ratio := c.Latency.P95Ms / live.ChatTx.Latency.P95Ms
				chatRatio = max(chatRatio, ratio)
				chatOK = chatOK && ratio <= 2
			}
		}
		d := map[string]any{"judge_signal_scan_large_p95_ms": p95, "others": rowsDetail(rows), "chat_list_worst_ratio": chatRatio, "chat_runs": live.ChatTx.Chats, "latency_bars_evaluated_at_readers": cr}
		out = append(out, verdict("C3", "judge signal scan <= 500 ms on the large stratum; trace page, jobs read, usage scan <= 3x live; chat list <= 2x live",
			judge != nil && p95 <= 500 && ok && chatOK && len(rows) > 0, d))
	}

	// C4: correctness.
	{
		d := map[string]any{
			"roundtrip_runs": r.RoundTrip.Runs, "roundtrip_mismatches": r.RoundTrip.Mismatches, "roundtrip_errors": r.RoundTrip.Errors,
			"oversized_messages_verified": r.RoundTrip.OversizedVerified, "tx_roundtrip_failures": r.Backfill.RoundTripFails,
			"reader_digest_mismatches": r.Digests.Mismatches, "reader_digest_errors": r.Digests.Errors,
		}
		ok := r.RoundTrip.Runs > 0 && r.RoundTrip.Mismatches == 0 && r.RoundTrip.Errors == 0 && r.Backfill.RoundTripFails == 0 &&
			r.Digests.Mismatches == 0 && r.Digests.Errors == 0 && r.RoundTrip.OversizedVerified > 0
		note := ""
		if c := r.Concurrency; c != nil {
			d["concurrency"] = c
			ok = ok && c.Violations == 0 && c.MissingSeqs == 0 && c.UnexpectedSeqs == 0 && c.ReadErrors == 0 && c.StragglersLost == 0 &&
				c.StragglerInsertErrors == 0 && c.StragglerInserts > 0 && c.Reads > 0
			if c.MixedOnMultiTxRun == 0 {
				note = "mixed archived+live state of a multi-transaction run was NOT observed; the mixed-state half of the check is unproven for this config"
			}
		} else {
			note = "concurrent reader and straggler checks run on the first config of each codec only"
		}
		c := verdict("C4", "100% round trip incl. oversized messages; zero duplicate, missing or lost seqs under concurrency", ok, d)
		c.Note = note
		out = append(out, c)
	}

	// C5: cost.
	{
		fp := r.Backfill.Footprint
		walRatio := 0.0
		if a.ArchivedPre > 0 {
			walRatio = float64(r.WALTotal) / float64(a.ArchivedPre)
		}
		finalVsPre := 0.0
		if a.ArchivedPre > 0 {
			finalVsPre = float64(fp.FinalChunks) / float64(a.ArchivedPre)
		}
		extraBound := int64(1.1*float64(fp.FinalChunks)) + 64<<20
		d := map[string]any{
			"wal_total_bytes": r.WALTotal, "wal_over_archived_pre": walRatio, "wal_mb_per_s_rollup": r.Backfill.WAL.WALBytesPerSc,
			"peak_extra_bytes": fp.PeakExtra, "peak_extra_bound_bytes": extraBound, "final_chunk_table_bytes": fp.FinalChunks,
			"final_chunk_over_archived_pre": finalVsPre, "largest_tx_ms": r.Backfill.LargestTx.DurationMs,
			"rewrite_headroom_bytes_reported_only": r.Rewrite.Headroom,
		}
		ok := walRatio <= 2.5 && fp.PeakExtra <= extraBound && finalVsPre <= 0.35 && r.Backfill.LargestTx.DurationMs <= 30_000
		ll := r.Backfill.LiveLoad
		d["live_reader_p95_ratio_during_vs_idle"] = ll.Ratio
		var note string
		switch {
		case ll.NotMeasure != "":
			note = "live-reader ratio not evaluated: " + ll.NotMeasure
		case ll.IdleP95Ms > 0:
			ok = ok && ll.Ratio <= 1.5
		}
		c := verdict("C5", "WAL <= 2.5x pre-rollup bytes; peak extra disk <= 1.1x final chunks + 64 MiB; chunks <= 35% of archived pre; largest tx <= 30 s; live readers <= 1.5x idle", ok, d)
		c.Note = note
		out = append(out, c)
	}

	// C6: memory.
	{
		var worst float64
		var failing []string
		for _, c := range r.Latency.PreVacuum {
			if c.Readers != 1 || c.Iterations == 0 {
				continue
			}
			limit := 4*c.PayloadBytesPerRq + float64(bound)
			if c.AllocBytesPerReq > limit {
				failing = append(failing, fmt.Sprintf("%s/%s", c.Shape, c.Stratum))
			}
			if limit > 0 {
				worst = max(worst, c.AllocBytesPerReq/limit)
			}
		}
		sort.Strings(failing)
		var peak uint64
		for _, c := range r.Latency.PreVacuum {
			if c.Readers == maxReaders {
				peak = max(peak, c.PeakHeapInuse)
			}
		}
		heapBound := uint64(maxReaders)*uint64(bound+largestMsg) + 64<<20 //nolint:gosec // G115: positive sizes and a small reader count.
		d := map[string]any{"cells_over_the_alloc_bound": failing, "worst_alloc_over_bound": worst, "peak_heap_inuse_bytes": peak, "peak_heap_bound_bytes": heapBound, "readers_for_peak": maxReaders}
		out = append(out, verdict("C6", "per-request allocation <= 4x bytes returned + one chunk bound; peak heap at max readers <= readers*(chunk bound + largest message) + 64 MiB",
			len(failing) == 0 && peak > 0 && peak <= heapBound, d))
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
