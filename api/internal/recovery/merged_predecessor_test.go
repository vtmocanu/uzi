package recovery

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestVerifiedMergeSnapshot(t *testing.T) {
	b := MergeBinding{RunID: uuid.New(), OwnerID: uuid.New(), RepoID: uuid.New(),
		ConnectionID: uuid.New(), ProjectID: 1, MRIID: 9, CompletedGeneration: 3,
		ForgeType: "github", BaseURL: "https://example.com"}
	at := time.Now().Add(-time.Second)
	for _, observed := range []*MergeObservation{{IID: 9, State: "merged"}} {
		if _, err := NewVerifiedMergeSnapshot(b, at, observed); err != nil {
			t.Fatal(err)
		}
	}
	for _, observed := range []*MergeObservation{nil, {IID: 8, State: "merged"}, {IID: 9, State: "closed"}, {IID: 9}, {State: "merged"}} {
		if _, err := NewVerifiedMergeSnapshot(b, at, observed); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("invalid forge observation %+v: %v", observed, err)
		}
	}
	for _, edit := range []func(*MergeBinding){
		func(b *MergeBinding) { b.RunID = uuid.Nil },
		func(b *MergeBinding) { b.OwnerID = uuid.Nil },
		func(b *MergeBinding) { b.RepoID = uuid.Nil },
		func(b *MergeBinding) { b.ConnectionID = uuid.Nil },
		func(b *MergeBinding) { b.ProjectID = 0 },
		func(b *MergeBinding) { b.MRIID = 0 },
		func(b *MergeBinding) { b.CompletedGeneration = 0 },
		func(b *MergeBinding) { b.ForgeType = "unknown" },
		func(b *MergeBinding) { b.BaseURL = "" },
	} {
		invalid := b
		edit(&invalid)
		if _, err := NewVerifiedMergeSnapshot(invalid, at, &MergeObservation{IID: invalid.MRIID, State: "merged"}); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("invalid binding %+v: %v", invalid, err)
		}
	}
	for _, at := range []time.Time{{}, time.Now().Add(time.Hour)} {
		if _, err := NewVerifiedMergeSnapshot(b, at, &MergeObservation{IID: b.MRIID, State: "merged"}); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("invalid time %v: %v", at, err)
		}
	}
	observed := &MergeObservation{IID: 9, State: "merged"}
	snapshot, err := NewVerifiedMergeSnapshot(b, at, observed)
	if err != nil {
		t.Fatal(err)
	}
	b.MRIID = 10
	observed.State = "open"
	if snapshot.binding.MRIID != 9 {
		t.Fatal("snapshot changed after caller mutated its inputs")
	}
	// The zero value cannot reach the database, even on an unwired service.
	if _, err := (&Service{}).DiscardMergedPredecessorHolds(context.Background(), VerifiedMergeSnapshot{}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("zero snapshot: %v", err)
	}
}
