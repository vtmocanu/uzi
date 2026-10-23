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
		for _, mode := range []string{"ordinary", "issue", "unknown", "bare", "error", "prerequisite"} {
			overflow := mode == "issue"
			t.Run(fmt.Sprintf("%s/%s", origin, mode), func(t *testing.T) {
				concrete := &forge.ResponseTooLargeError{Provenance: forge.ResponseIssueSuccess}
				source := errors.New("temporary forge failure")
				switch mode {
				case "unknown":
					source = &forge.ResponseTooLargeError{}
				case "bare":
					source = forge.ErrResponseTooLarge
				case "error":
					source = &forge.ResponseTooLargeError{Provenance: forge.ResponseError, Outcome: forge.ErrorClassServerError}
				case "prerequisite":
					source = &forge.ResponseTooLargeError{Provenance: forge.ResponseSuccess}
				case "issue":
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
