package schedsvc

import (
	"cmp"
	"slices"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/pipelinestatus"
)

// CI scout aggregation (#3225): pure functions over the collected CI history. Every
// figure is computed only from timestamps the forge reported; a value that cannot be
// computed is counted as unknown, never guessed or shown as zero. Nothing here talks to
// the forge.

const (
	scoutPopMain = "main"
	scoutPopPR   = "pr"

	// scoutWeek splits the window for the regression comparison.
	scoutWeek = 7 * 24 * time.Hour
	// scoutMinSamples is the sample count below which a comparison is flagged as thin.
	scoutMinSamples = 3
	// scoutRefsKept bounds the evidence references a row keeps per list (newest last).
	scoutRefsKept = 3
)

// scoutRun is one collected CI run with its jobs.
type scoutRun struct {
	Pop string
	Run forge.WorkflowRun
	// Jobs are every attempt's jobs when HistoryKnown, else only the latest attempt's.
	Jobs []forge.Job
	// HistoryKnown is false when the forge could not return complete attempt history
	// (forge.ErrJobAttemptsUnsupported): earlier attempts are then unknown, not absent.
	HistoryKnown bool
	// JobsMissing is true when the job listing was not collected (budget, deadline, error).
	JobsMissing bool
	// CreatedReported is false where the forge's run creation time is really its start
	// (Forgejo), so latency from creation is unknown.
	CreatedReported bool
}

func jobElapsed(j forge.Job) (time.Duration, bool) {
	if j.StartedAt.IsZero() || j.FinishedAt.IsZero() || j.FinishedAt.Before(j.StartedAt) {
		return 0, false
	}
	return j.FinishedAt.Sub(j.StartedAt), true
}

func jobFailed(j forge.Job) bool   { return pipelinestatus.IsFailed(j.Status) }
func jobPassed(j forge.Job) bool   { return pipelinestatus.IsSuccess(j.Status) }
func jobTerminal(j forge.Job) bool { return jobFailed(j) || jobPassed(j) }

// jobCancelled reports a cancelled job execution; it still consumed runner time.
func jobCancelled(j forge.Job) bool { return j.Status == "canceled" || j.Status == "cancelled" }

// jobEnded reports an execution that finished one way or another, so its duration counts
// towards execution totals (or towards their unknown count).
func jobEnded(j forge.Job) bool { return jobTerminal(j) || jobCancelled(j) }

func runCancelled(r forge.WorkflowRun) bool {
	for _, s := range []string{r.Status, r.Conclusion} {
		if s == "canceled" || s == "cancelled" {
			return true
		}
	}
	return false
}

// costSum is a duration total that also counts the executions whose duration is unknown,
// so a renderer can show a lower bound instead of a false exact figure.
type costSum struct {
	Known   time.Duration
	Unknown int
}

func (c *costSum) add(j forge.Job) {
	if d, ok := jobElapsed(j); ok {
		c.Known += d
	} else {
		c.Unknown++
	}
}

func (c *costSum) merge(o costSum) { c.Known += o.Known; c.Unknown += o.Unknown }

// executions groups a run's jobs by name (the identity within one workflow run; GitHub
// job names carry the matrix values) and orders each group oldest first: by reported
// attempt, then creation time, then id. The last element is the final execution.
func executions(jobs []forge.Job) map[string][]forge.Job {
	groups := map[string][]forge.Job{}
	for _, j := range jobs {
		groups[j.Name] = append(groups[j.Name], j)
	}
	for _, g := range groups {
		slices.SortStableFunc(g, func(a, b forge.Job) int {
			if a.Attempt != nil && b.Attempt != nil && *a.Attempt != *b.Attempt {
				return cmp.Compare(*a.Attempt, *b.Attempt)
			}
			if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 && !a.CreatedAt.IsZero() && !b.CreatedAt.IsZero() {
				return c
			}
			return cmp.Compare(a.ID, b.ID)
		})
	}
	return groups
}

func sortedNames(groups map[string][]forge.Job) []string {
	names := make([]string, 0, len(groups))
	for n := range groups {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// jobRef points at one job execution of one run, the evidence behind a figure.
type jobRef struct{ Run, Job int64 }

// flakePair is one suspected flake: the failed execution and the passing one after it,
// in the same run (a retry) or in a later run of the same workflow on the same commit.
type flakePair struct{ Failed, Passed jobRef }

// runFacts is what one run contributes to the population aggregates.
type runFacts struct {
	// FailedExec is the execution of failed attempts of identities that later passed;
	// RepeatedExec the additional execution reruns caused (every execution after an
	// identity's first, successful repeats included).
	FailedExec, RepeatedExec costSum
	// Recovery is the observed delay from the first attempt's last job end to the final
	// attempt's last job end; RecoveryKnown is false without reported attempt boundaries.
	Recovery      time.Duration
	RecoveryKnown bool
	Retried       bool
	// Start/End are the observed execution bounds from reported job timestamps; Complete
	// is false when any ended job lacks either timestamp.
	Start, End time.Time
	Complete   bool
	// FirstFailure is the earliest failed job end minus the run's reported creation, and
	// FirstFailureJob the job behind it.
	FirstFailure      time.Duration
	FirstFailureKnown bool
	FirstFailureJob   int64
	Compute           costSum
}

func analyzeRun(r scoutRun) runFacts {
	f := runFacts{Complete: true}
	for _, j := range r.Jobs {
		if jobEnded(j) && (j.StartedAt.IsZero() || j.FinishedAt.IsZero()) {
			f.Complete = false
		}
		if !j.StartedAt.IsZero() && (f.Start.IsZero() || j.StartedAt.Before(f.Start)) {
			f.Start = j.StartedAt
		}
		if j.FinishedAt.After(f.End) {
			f.End = j.FinishedAt
		}
		if jobEnded(j) {
			f.Compute.add(j)
		}
		if jobFailed(j) && r.CreatedReported && !r.Run.CreatedAt.IsZero() && !j.FinishedAt.IsZero() && !j.FinishedAt.Before(r.Run.CreatedAt) {
			if d := j.FinishedAt.Sub(r.Run.CreatedAt); !f.FirstFailureKnown || d < f.FirstFailure {
				f.FirstFailure, f.FirstFailureKnown, f.FirstFailureJob = d, true, j.ID
			}
		}
	}
	groups := executions(r.Jobs)
	for _, name := range sortedNames(groups) {
		g := groups[name]
		if len(g) < 2 {
			continue
		}
		f.Retried = true
		for _, e := range g[1:] {
			f.RepeatedExec.add(e)
		}
		if final := g[len(g)-1]; jobPassed(final) {
			for _, e := range g[:len(g)-1] {
				if jobFailed(e) {
					f.FailedExec.add(e)
				}
			}
		}
	}
	f.Recovery, f.RecoveryKnown = attemptRecovery(r.Jobs)
	return f
}

// runFlake is one identity's within-run suspected flake with the executions behind its
// costs; the row charges each execution once (see jobRow.charge).
type runFlake struct {
	Pair          flakePair
	Failed, Rerun []forge.Job
}

// withinRunFlakes returns, per identity that failed and then passed in this run, the
// pair (last failed execution, final execution) and that identity's own costs.
func withinRunFlakes(r scoutRun) map[string]runFlake {
	out := map[string]runFlake{}
	for name, g := range executions(r.Jobs) {
		final := g[len(g)-1]
		if len(g) < 2 || !jobPassed(final) {
			continue
		}
		var fl runFlake
		var last int64
		for _, e := range g[:len(g)-1] {
			if jobFailed(e) {
				fl.Failed = append(fl.Failed, e)
				last = e.ID
			}
		}
		if last == 0 {
			continue
		}
		fl.Rerun = g[1:]
		fl.Pair = flakePair{jobRef{r.Run.ID, last}, jobRef{r.Run.ID, final.ID}}
		out[name] = fl
	}
	return out
}

// attemptRecovery measures first-attempt end to final-attempt end when the forge
// reported attempt numbers on every job and every job has a reported finish.
func attemptRecovery(jobs []forge.Job) (time.Duration, bool) {
	ends := map[int64]time.Time{}
	for _, j := range jobs {
		if j.Attempt == nil || j.FinishedAt.IsZero() {
			return 0, false
		}
		if j.FinishedAt.After(ends[*j.Attempt]) {
			ends[*j.Attempt] = j.FinishedAt
		}
	}
	if len(ends) < 2 {
		return 0, false
	}
	attempts := make([]int64, 0, len(ends))
	for a := range ends {
		attempts = append(attempts, a)
	}
	slices.Sort(attempts)
	first, last := ends[attempts[0]], ends[attempts[len(attempts)-1]]
	if last.Before(first) {
		return 0, false
	}
	return last.Sub(first), true
}

// percentile returns the nearest-rank p-th percentile (0 < p <= 100) of ds.
func percentile(ds []time.Duration, p int) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := slices.Clone(ds)
	slices.Sort(s)
	rank := (p*len(s) + 99) / 100
	return s[max(rank, 1)-1]
}

// keepRef appends ref, keeping at most scoutRefsKept: the last appended, which are the
// newest because scoutAggregate folds runs oldest first.
func keepRef(refs []jobRef, ref jobRef) []jobRef {
	refs = append(refs, ref)
	return refs[max(len(refs)-scoutRefsKept, 0):]
}

// jobKey identifies a job across runs: population, workflow identity (WorkflowKey, never
// the display name, which two workflows may share) and job name.
type jobKey struct {
	Pop, Workflow, Job string
}

// jobRow aggregates one job identity's final executions across runs.
type jobRow struct {
	Key          jobKey
	WorkflowName string
	// Elapsed holds known final-execution durations; ElapsedUnknown counts terminal
	// final executions without both timestamps.
	Elapsed                 []time.Duration
	ElapsedUnknown          int
	Recent, Prior           []time.Duration
	RecentRefs, PriorRefs   []jobRef
	Failures                int
	FailedRefs              []jobRef
	Flakes                  []flakePair
	FlakeFailed, FlakeRerun costSum
	Waits, Queued           []time.Duration
	// charged records, per metric, the executions already counted, so a job seen by both
	// the within-run and the cross-run pairing is charged once per metric. The sets are
	// separate: a failed retry is both failed execution and rerun execution.
	charged map[*costSum]map[jobRef]bool
}

// charge adds each execution of run to c unless c already counted it.
func (r *jobRow) charge(c *costSum, run int64, jobs ...forge.Job) {
	if r.charged == nil {
		r.charged = map[*costSum]map[jobRef]bool{}
	}
	seen := r.charged[c]
	if seen == nil {
		seen = map[jobRef]bool{}
		r.charged[c] = seen
	}
	for _, j := range jobs {
		ref := jobRef{run, j.ID}
		if seen[ref] {
			continue
		}
		seen[ref] = true
		c.add(j)
	}
}

// firstFailure is one run's time to first failure with the job behind it.
type firstFailure struct {
	D   time.Duration
	Ref jobRef
}

// popRow aggregates one population's runs.
type popRow struct {
	Pop                                  string
	Runs, Partial, NoHistory, Incomplete int
	FirstFailures                        []firstFailure
	FirstFailureUnknown                  int
	Cancelled                            int
	CancelledCompute                     costSum
	CancelledRuns                        []int64
	Overlaps                             int
	OverlapRuns                          []int64
	FailedExec, RepeatedExec             costSum
	Recovery                             []time.Duration
	RecoveryUnknown, RetriedRuns         int
}

type scoutStats struct {
	Jobs []*jobRow
	Pops []*popRow
}

func workflowName(r forge.WorkflowRun) string {
	if r.Name != "" {
		return r.Name
	}
	return r.WorkflowKey
}

func workflowID(r forge.WorkflowRun) string {
	if r.WorkflowKey != "" {
		return r.WorkflowKey
	}
	return r.Name
}

// scoutAggregate folds the runs into per-job and per-population rows. A row's Flakes
// lists every observed pair (a within-run retry and a cross-run repeat of one failure are
// both observations), while its costs charge each execution once. now anchors the
// regression split: runs created within the last week are "recent", the week before
// "prior".
func scoutAggregate(runs []scoutRun, now time.Time) scoutStats {
	// Fold oldest first, whatever order collection used: every "keep the last N" list
	// (evidence refs, flake pairs) then holds the newest records.
	runs = slices.Clone(runs)
	slices.SortStableFunc(runs, func(a, b scoutRun) int {
		return cmp.Or(a.Run.CreatedAt.Compare(b.Run.CreatedAt), cmp.Compare(a.Run.ID, b.Run.ID))
	})
	jobs := map[jobKey]*jobRow{}
	pops := map[string]*popRow{}
	rowFor := func(r scoutRun, name string) *jobRow {
		k := jobKey{Pop: r.Pop, Workflow: workflowID(r.Run), Job: name}
		row := jobs[k]
		if row == nil {
			row = &jobRow{Key: k, WorkflowName: workflowName(r.Run)}
			jobs[k] = row
		}
		return row
	}
	type span struct {
		run   scoutRun
		facts runFacts
	}
	byLane := map[[3]string][]span{}
	for _, r := range runs {
		p := pops[r.Pop]
		if p == nil {
			p = &popRow{Pop: r.Pop}
			pops[r.Pop] = p
		}
		p.Runs++
		if r.JobsMissing {
			p.Partial++
			continue
		}
		if !r.HistoryKnown {
			p.NoHistory++
		}
		f := analyzeRun(r)
		if !f.Complete {
			p.Incomplete++
		}
		if f.FirstFailureKnown {
			p.FirstFailures = append(p.FirstFailures, firstFailure{f.FirstFailure, jobRef{r.Run.ID, f.FirstFailureJob}})
		} else if slices.ContainsFunc(r.Jobs, jobFailed) {
			p.FirstFailureUnknown++
		}
		if runCancelled(r.Run) {
			p.Cancelled++
			p.CancelledCompute.merge(f.Compute)
			p.CancelledRuns = append(p.CancelledRuns, r.Run.ID)
		}
		p.FailedExec.merge(f.FailedExec)
		p.RepeatedExec.merge(f.RepeatedExec)
		if f.Retried {
			p.RetriedRuns++
			if f.RecoveryKnown {
				p.Recovery = append(p.Recovery, f.Recovery)
			} else {
				p.RecoveryUnknown++
			}
		}
		lane := [3]string{r.Pop, workflowID(r.Run), r.Run.Branch}
		byLane[lane] = append(byLane[lane], span{r, f})

		flakes := withinRunFlakes(r)
		recent := now.Sub(r.Run.CreatedAt) <= scoutWeek
		groups := executions(r.Jobs)
		for _, name := range sortedNames(groups) {
			g := groups[name]
			final := g[len(g)-1]
			if !jobTerminal(final) {
				continue
			}
			row := rowFor(r, name)
			ref := jobRef{r.Run.ID, final.ID}
			if d, ok := jobElapsed(final); ok {
				row.Elapsed = append(row.Elapsed, d)
				if recent {
					row.Recent = append(row.Recent, d)
					row.RecentRefs = keepRef(row.RecentRefs, ref)
				} else {
					row.Prior = append(row.Prior, d)
					row.PriorRefs = keepRef(row.PriorRefs, ref)
				}
			} else {
				row.ElapsedUnknown++
			}
			if jobFailed(final) {
				row.Failures++
				row.FailedRefs = keepRef(row.FailedRefs, ref)
			}
			if fl, ok := flakes[name]; ok {
				row.Flakes = append(row.Flakes, fl.Pair)
				row.charge(&row.FlakeFailed, r.Run.ID, fl.Failed...)
				row.charge(&row.FlakeRerun, r.Run.ID, fl.Rerun...)
			}
			if !final.CreatedAt.IsZero() && !final.StartedAt.IsZero() && !final.StartedAt.Before(final.CreatedAt) {
				row.Waits = append(row.Waits, final.StartedAt.Sub(final.CreatedAt))
			}
			if final.QueuedDuration != nil {
				row.Queued = append(row.Queued, *final.QueuedDuration)
			}
		}
	}
	crossRunFlakes(runs, rowFor)
	for _, spans := range byLane {
		slices.SortFunc(spans, func(a, b span) int { return a.facts.Start.Compare(b.facts.Start) })
		for i := 1; i < len(spans); i++ {
			older, newer := spans[i-1], spans[i]
			if older.facts.Start.IsZero() || newer.facts.Start.IsZero() || older.facts.End.IsZero() {
				continue
			}
			if newer.facts.Start.Before(older.facts.End) && !runCancelled(older.run.Run) {
				p := pops[older.run.Pop]
				p.Overlaps++
				p.OverlapRuns = append(p.OverlapRuns, older.run.Run.ID)
			}
		}
	}
	var st scoutStats
	for _, r := range jobs {
		st.Jobs = append(st.Jobs, r)
	}
	slices.SortFunc(st.Jobs, func(a, b *jobRow) int {
		return cmp.Or(cmp.Compare(a.Key.Pop, b.Key.Pop), cmp.Compare(a.Key.Workflow, b.Key.Workflow), cmp.Compare(a.Key.Job, b.Key.Job))
	})
	for _, p := range []string{scoutPopMain, scoutPopPR} {
		if pops[p] != nil {
			st.Pops = append(st.Pops, pops[p])
		}
	}
	return st
}

// crossRunFlakes pairs runs of the same population and workflow on the same full SHA: a
// job identity whose final execution failed in an earlier run and passed in a later one
// is a suspected flake. Every failed final execution since the first failure counts as
// failed execution, and every execution after the first failure (later failures and the
// pass) as rerun execution, each charged once per metric. The pair records the first
// failure and the pass. Different workflows, populations or SHAs never pair.
func crossRunFlakes(runs []scoutRun, rowFor func(scoutRun, string) *jobRow) {
	type lane struct{ pop, workflow, sha string }
	byLane := map[lane][]scoutRun{}
	for _, r := range runs {
		if r.JobsMissing || r.Run.SHA == "" || workflowID(r.Run) == "" {
			continue
		}
		l := lane{r.Pop, workflowID(r.Run), r.Run.SHA}
		byLane[l] = append(byLane[l], r)
	}
	for _, rs := range byLane {
		if len(rs) < 2 {
			continue
		}
		slices.SortFunc(rs, func(a, b scoutRun) int {
			return cmp.Or(a.Run.CreatedAt.Compare(b.Run.CreatedAt), cmp.Compare(a.Run.ID, b.Run.ID))
		})
		type failed struct {
			run int64
			job forge.Job
		}
		// failedIn holds, per identity, the failed final executions since its first
		// failure on this commit, oldest first.
		failedIn := map[string][]failed{}
		for _, r := range rs {
			groups := executions(r.Jobs)
			for _, name := range sortedNames(groups) {
				final := groups[name][len(groups[name])-1]
				switch {
				case jobFailed(final):
					failedIn[name] = append(failedIn[name], failed{r.Run.ID, final})
				case jobPassed(final):
					fails := failedIn[name]
					if len(fails) == 0 {
						continue
					}
					row := rowFor(r, name)
					row.Flakes = append(row.Flakes, flakePair{jobRef{fails[0].run, fails[0].job.ID}, jobRef{r.Run.ID, final.ID}})
					for i, fl := range fails {
						row.charge(&row.FlakeFailed, fl.run, fl.job)
						if i > 0 {
							row.charge(&row.FlakeRerun, fl.run, fl.job)
						}
					}
					row.charge(&row.FlakeRerun, r.Run.ID, final)
					delete(failedIn, name)
				}
			}
		}
	}
}
