package workersvc

import "sort"

// JudgeSkipFailOrigins returns the failure origins that keep a failed run out of the judge's
// eligible population, as the Quality analytics statements bind them (issue #4730).
//
// always are skipped whatever the run's iteration_count: neverJudgeFailOrigins together with
// envPublishFailOrigins. prestartOnly are skipped only at iteration_count == 0:
// preStartInfraFailOrigins. Both mirror the gate in maybeEnqueueJudgeByID, so the analytics
// denominator is derived from the same sets the enqueue gate reads. Each call returns fresh,
// sorted slices.
func JudgeSkipFailOrigins() (always, prestartOnly []string) {
	always = make([]string, 0, len(neverJudgeFailOrigins)+len(envPublishFailOrigins))
	for o := range neverJudgeFailOrigins {
		always = append(always, o)
	}
	for o := range envPublishFailOrigins {
		if !neverJudgeFailOrigins[o] {
			always = append(always, o)
		}
	}
	prestartOnly = make([]string, 0, len(preStartInfraFailOrigins))
	for o := range preStartInfraFailOrigins {
		prestartOnly = append(prestartOnly, o)
	}
	sort.Strings(always)
	sort.Strings(prestartOnly)
	return always, prestartOnly
}
