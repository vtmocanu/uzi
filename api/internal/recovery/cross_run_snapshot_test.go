package recovery

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func crossRunSnapshotFixture() (CrossRunTargetBinding, CrossRunWitnessBinding, time.Time) {
	created := time.Date(2025, 1, 2, 3, 4, 6, 654321987, time.FixedZone("offset", 3600))
	b := CrossRunWitnessBinding{MergeBinding: MergeBinding{
		RunID: uuid.New(), OwnerID: uuid.New(), RepoID: uuid.New(), ConnectionID: uuid.New(),
		ProjectID: 1, MRIID: 7, CompletedGeneration: 1, ForgeType: "github", BaseURL: "https://example.com"},
		Kind: "issue", IssueIID: 101, CreatedAt: created}
	target := CrossRunTargetBinding{RunID: uuid.New(), OwnerID: b.OwnerID, RepoID: b.RepoID,
		Kind: "issue", Status: "cancelled", Generation: 3, IssueIID: 101, FinishedAt: created.Add(-time.Second)}
	return target, b, created.Add(time.Hour)
}

func TestCrossRunSnapshotConstructor(t *testing.T) {
	target, b, at := crossRunSnapshotFixture()
	observed := &MergeObservation{IID: 7, State: "merged"}
	s, err := NewVerifiedCrossRunWitnessSnapshot(b, at, observed)
	if err != nil {
		t.Fatal(err)
	}
	wantCreated := time.Date(2025, 1, 2, 2, 4, 6, 654321000, time.UTC)
	wantVerified := time.Date(2025, 1, 2, 3, 4, 6, 654321000, time.UTC)
	if s.binding.CreatedAt != wantCreated || s.verifiedAt != wantVerified {
		t.Fatal("snapshot must store UTC microseconds")
	}
	if err := target.Validate(s); err != nil {
		t.Fatal(err)
	}
	b.IssueIID = 999
	observed.State = "open"
	if s.binding.IssueIID != 101 {
		t.Fatal("snapshot changed with caller input")
	}
	for _, tc := range []struct {
		name        string
		observation *MergeObservation
	}{
		{"nil", nil}, {"wrong_iid", &MergeObservation{IID: 8, State: "merged"}},
		{"unmerged", &MergeObservation{IID: 7, State: "closed"}}, {"bad_shape", &MergeObservation{State: "merged"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, good, at := crossRunSnapshotFixture()
			got, err := NewVerifiedCrossRunWitnessSnapshot(good, at, tc.observation)
			if !errors.Is(err, ErrBadRequest) || got != (VerifiedCrossRunWitnessSnapshot{}) {
				t.Fatal("bad merge must not produce a witness")
			}
		})
	}
	for _, tc := range []struct {
		name string
		edit func(*CrossRunWitnessBinding)
	}{
		{"kind", func(b *CrossRunWitnessBinding) { b.Kind = "task" }},
		{"missing_issue", func(b *CrossRunWitnessBinding) { b.IssueIID = 0 }},
		{"negative_issue", func(b *CrossRunWitnessBinding) { b.IssueIID = -1 }},
		{"run", func(b *CrossRunWitnessBinding) { b.RunID = uuid.Nil }},
		{"owner", func(b *CrossRunWitnessBinding) { b.OwnerID = uuid.Nil }},
		{"repo", func(b *CrossRunWitnessBinding) { b.RepoID = uuid.Nil }},
		{"connection", func(b *CrossRunWitnessBinding) { b.ConnectionID = uuid.Nil }},
		{"project", func(b *CrossRunWitnessBinding) { b.ProjectID = 0 }},
		{"mr", func(b *CrossRunWitnessBinding) { b.MRIID = 0 }},
		{"generation", func(b *CrossRunWitnessBinding) { b.CompletedGeneration = 0 }},
		{"forge", func(b *CrossRunWitnessBinding) { b.ForgeType = "unknown" }},
		{"url", func(b *CrossRunWitnessBinding) { b.BaseURL = "" }},
		{"created", func(b *CrossRunWitnessBinding) { b.CreatedAt = time.Time{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, b, at := crossRunSnapshotFixture()
			tc.edit(&b)
			if _, err := NewVerifiedCrossRunWitnessSnapshot(b, at, &MergeObservation{IID: b.MRIID, State: "merged"}); !errors.Is(err, ErrBadRequest) {
				t.Fatal("bad binding must not produce a witness")
			}
		})
	}
	for _, invalid := range []time.Time{{}, time.Now().Add(time.Hour)} {
		_, b, _ := crossRunSnapshotFixture()
		if _, err := NewVerifiedCrossRunWitnessSnapshot(b, invalid, &MergeObservation{IID: 7, State: "merged"}); !errors.Is(err, ErrBadRequest) {
			t.Fatal("invalid verification time accepted")
		}
	}
}

func TestCrossRunTargetSnapshotPolicy(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*CrossRunTargetBinding, CrossRunWitnessBinding)
	}{
		{"same_run", func(a *CrossRunTargetBinding, b CrossRunWitnessBinding) { a.RunID = b.RunID }},
		{"owner", func(a *CrossRunTargetBinding, b CrossRunWitnessBinding) { a.OwnerID = uuid.New() }},
		{"repo", func(a *CrossRunTargetBinding, b CrossRunWitnessBinding) { a.RepoID = uuid.New() }},
		{"issue", func(a *CrossRunTargetBinding, b CrossRunWitnessBinding) { a.IssueIID++ }},
		{"nonissue", func(a *CrossRunTargetBinding, b CrossRunWitnessBinding) { a.Kind = "task" }},
		{"completed", func(a *CrossRunTargetBinding, b CrossRunWitnessBinding) { a.Status = "completed" }},
		{"generation", func(a *CrossRunTargetBinding, b CrossRunWitnessBinding) { a.Generation = 0 }},
		{"missing_issue", func(a *CrossRunTargetBinding, b CrossRunWitnessBinding) { a.IssueIID = 0 }},
		{"negative_issue", func(a *CrossRunTargetBinding, b CrossRunWitnessBinding) { a.IssueIID = -1 }},
		{"equal_chronology", func(a *CrossRunTargetBinding, b CrossRunWitnessBinding) { a.FinishedAt = b.CreatedAt }},
		{"same_microsecond", func(a *CrossRunTargetBinding, b CrossRunWitnessBinding) {
			a.FinishedAt = b.CreatedAt.Add(-time.Nanosecond)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b, at := crossRunSnapshotFixture()
			s, err := NewVerifiedCrossRunWitnessSnapshot(b, at, &MergeObservation{IID: 7, State: "merged"})
			if err != nil {
				t.Fatal(err)
			}
			tc.edit(&a, b)
			// An unwired service proves refusal happens before any database access.
			if n, err := (&Service{}).DiscardCrossRunHolds(context.Background(), a, s); err == nil || n != 0 {
				t.Fatal("invalid target must retain custody")
			}
		})
	}
}
