package workersvc

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestM2RunOriginsResponseOverflow(t *testing.T) {
	for _, origin := range []string{"composite", "fallback"} {
		for _, overflow := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/overflow=%t", origin, overflow), func(t *testing.T) {
				concrete := &forge.ResponseTooLargeError{}
				source := errors.New("temporary forge failure")
				if overflow {
					source = fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", concrete))
				}
				fs := &fakeStore{repoRow: store.GetRepoForUserRow{ID: uuid.New()}, issueByID: store.Issue{Labels: uziLabels()}}
				svc := New(fs, newBox(t), testParams())
				svc.SetForges(&fakeForges{f: &fakeForge{getIssueErr: source}})
				var err error
				if origin == "composite" {
					_, err = svc.StartRunForUser(context.Background(), uuid.New(), uuid.New(), 4, nil, nil, false, nil, nil, nil)
				} else {
					_, err = svc.CreateRun(context.Background(), uuid.New(), uuid.New(), 4, "body", nil, nil, false, nil, nil, nil)
				}
				if overflow {
					var got *forge.ResponseTooLargeError
					if !errors.Is(err, ErrDescriptionTooLarge) || !errors.Is(err, forge.ErrResponseTooLarge) || !errors.As(err, &got) || got != concrete || errors.Is(err, ErrForgeIssueRead) {
						t.Fatalf("lost overflow chain: %v", err)
					}
				} else if !errors.Is(err, ErrForgeIssueRead) || errors.Is(err, ErrDescriptionTooLarge) {
					t.Fatalf("ordinary failure changed: %v", err)
				}
				if fs.createRunParams != nil {
					t.Fatal("inserted run on failed capture")
				}
			})
		}
	}
}
