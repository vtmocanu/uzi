package workersvc

import "github.com/vtmocanu/uzi/api/internal/forge"

// testNS is the comment namespace the single-namespace tests use (a GitLab note stream).
const testNS = forge.CommentNSNote

// marksJSON is a ledger row's comment_marks holding one mark in testNS.
func marksJSON(mark string) []byte { return []byte(`{"` + testNS + `":` + mark + `}`) }

// reviewResultOf wraps an already-assembled snapshot as the finished assessment the on-demand
// path consumes, treating every comment in it as eligible (a snapshot only ever carries
// eligible comments, here all in testNS) and using the same actionable test the assessor applies.
func reviewResultOf(snap *ReviewCommentsSnapshot) *ReviewSnapshotResult {
	res := &ReviewSnapshotResult{Snapshot: snap, class: map[CommentKey]reviewClass{}, inSnapshot: map[CommentKey]bool{}}
	for _, c := range snap.Comments {
		k := CommentKey{Namespace: testNS, ID: c.ID}
		res.class[k] = classEligible
		res.inSnapshot[k] = true
		if IsActionableReviewComment(c) {
			res.eligibleActionable = append(res.eligibleActionable, k)
		}
	}
	return res
}
