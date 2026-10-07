package workersvc

// reviewResultOf wraps an already-assembled snapshot as the finished assessment the on-demand
// path consumes, treating every comment in it as eligible (a snapshot only ever carries
// eligible comments) and using the same actionable test the assessor applies.
func reviewResultOf(snap *ReviewCommentsSnapshot) *ReviewSnapshotResult {
	res := &ReviewSnapshotResult{Snapshot: snap, class: map[int64]reviewClass{}, inSnapshot: map[int64]bool{}}
	for _, c := range snap.Comments {
		res.class[c.ID] = classEligible
		res.inSnapshot[c.ID] = true
		if IsActionableReviewComment(c) {
			res.eligibleActionable = append(res.eligibleActionable, c.ID)
		}
	}
	return res
}
