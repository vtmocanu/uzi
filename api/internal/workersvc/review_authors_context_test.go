package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

type authorContextLookup func(context.Context, int64, int64) (forge.AuthorEligibility, error)

func (f authorContextLookup) RepositoryAuthorEligibility(ctx context.Context, project, author int64) (forge.AuthorEligibility, error) {
	return f(ctx, project, author)
}

func TestTimeoutLookupIdentityDeadlineLeavesAssessmentLive(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var identityErr error
	var calls []int64
	lookup := timeoutLookup{inner: authorContextLookup(func(ctx context.Context, project, author int64) (forge.AuthorEligibility, error) {
		if project != 7 {
			t.Fatalf("unexpected project %d", project)
		}
		calls = append(calls, author)
		if author == 42 {
			// Synchronous entry avoids a race between HTTP scheduling and the
			// shortened real lookup timer. Driver context routing is covered
			// separately by the forge package's transport tests.
			<-ctx.Done()
			identityErr = ctx.Err()
			return forge.AuthorUnknown, identityErr
		}
		if ctx.Err() != nil {
			return forge.AuthorUnknown, ctx.Err()
		}
		return forge.AuthorEligible, nil
	}), d: 50 * time.Millisecond}
	assessment := forge.BeginAuthorAssessment(parent)
	got, err := lookup.RepositoryAuthorEligibility(assessment, 7, 42)
	if got != forge.AuthorUnknown || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(identityErr, context.DeadlineExceeded) {
		t.Fatalf("identity got=%v err=%v context err=%v", got, err, identityErr)
	}
	if parent.Err() != nil {
		t.Fatalf("assessment no longer live: %v", parent.Err())
	}
	got, err = lookup.RepositoryAuthorEligibility(assessment, 7, 43)
	if got != forge.AuthorEligible || err != nil {
		t.Fatalf("later identity got=%v err=%v", got, err)
	}
	if len(calls) != 2 || calls[0] != 42 || calls[1] != 43 {
		t.Fatalf("identity checks=%v; want [42 43]", calls)
	}
}
