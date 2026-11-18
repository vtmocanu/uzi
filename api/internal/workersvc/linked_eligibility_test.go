package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4173 D14: on a linked repo the run gate reads the link's stored eligibility tag
// name, both on the cached issue and on the fresh read at capture.

// freshTracker is an issue tracker whose fresh read carries the given labels.
type freshTracker struct {
	ytTracker
	labels []string
}

func (f *freshTracker) GetIssue(ctx context.Context, p, iid int64) (forge.Issue, error) {
	is, err := f.ytTracker.GetIssue(ctx, p, iid)
	is.Labels = f.labels
	return is, err
}

func linkedGateService(t *testing.T, tag string, cachedLabels []byte, fresh []string, assignees []byte) (*Service, *fakeStore) {
	t.Helper()
	row := linkedRepoRow()
	row.EligibilityTagName = pgtype.Text{String: tag, Valid: true}
	row.BotForgeUserID = 1
	fs := &fakeStore{repoRow: row, issueByID: store.Issue{Labels: cachedLabels, AssigneeIds: assignees}, createRunResult: store.Run{ID: uuid.New()}}
	svc := New(fs, newBox(t), testParams())
	svc.SetForges(m2Builder{forgetest.NewRecorder(&m2Forge{disposition: forge.AuthorEligible})})
	svc.SetIssueSources(&fakeSources{tr: forgetest.NewRecorder(&freshTracker{labels: fresh})})
	return svc, fs
}

func TestLinkedRunGateUsesTheStoredEligibilityTag(t *testing.T) {
	create := func(svc *Service) error {
		_, err := svc.CreateRun(context.Background(), uuid.New(), uuid.New(), 4, "", nil, nil, false, nil, nil, nil, nil, nil, nil)
		return err
	}
	t.Run("the stored tag on the cache and the fresh read starts a run", func(t *testing.T) {
		svc, fs := linkedGateService(t, "ready-for-uzi", []byte(`["ready-for-uzi"]`), []string{"ready-for-uzi"}, nil)
		if err := create(svc); err != nil {
			t.Fatal(err)
		}
		if fs.createRunParams == nil {
			t.Fatal("no run was created")
		}
	})
	t.Run("the configured uzi label does not stand in for the stored tag", func(t *testing.T) {
		svc, fs := linkedGateService(t, "ready-for-uzi", uziLabels(), []string{"ready-for-uzi"}, nil)
		if err := create(svc); !errors.Is(err, ErrNotPRDIssue) || fs.createRunParams != nil {
			t.Fatalf("err = %v, run created = %v", err, fs.createRunParams != nil)
		}
	})
	t.Run("bot assignment never makes a linked issue eligible", func(t *testing.T) {
		svc, fs := linkedGateService(t, "ready-for-uzi", []byte(`[]`), []string{"ready-for-uzi"}, []byte(`[1]`))
		if err := create(svc); !errors.Is(err, ErrNotPRDIssue) || fs.createRunParams != nil {
			t.Fatalf("err = %v, run created = %v", err, fs.createRunParams != nil)
		}
	})
	t.Run("a cached issue untagged since the last full sync is refused by the fresh read", func(t *testing.T) {
		svc, fs := linkedGateService(t, "ready-for-uzi", []byte(`["ready-for-uzi"]`), []string{"other-tag"}, nil)
		if err := create(svc); !errors.Is(err, ErrNotPRDIssue) || fs.createRunParams != nil {
			t.Fatalf("err = %v, run created = %v", err, fs.createRunParams != nil)
		}
	})
	t.Run("a linked row with no stored tag name starts nothing", func(t *testing.T) {
		svc, fs := linkedGateService(t, "", uziLabels(), []string{"uzi"}, nil)
		if err := create(svc); !errors.Is(err, ErrNotPRDIssue) || fs.createRunParams != nil {
			t.Fatalf("err = %v, run created = %v", err, fs.createRunParams != nil)
		}
	})
	t.Run("a linked row whose stored tag name is NULL starts nothing", func(t *testing.T) {
		svc, fs := linkedGateService(t, "", uziLabels(), []string{"uzi"}, nil)
		fs.repoRow.EligibilityTagName = pgtype.Text{}
		if err := create(svc); !errors.Is(err, ErrNotPRDIssue) || fs.createRunParams != nil {
			t.Fatalf("err = %v, run created = %v", err, fs.createRunParams != nil)
		}
	})
	t.Run("a stored tag name of only blanks starts nothing", func(t *testing.T) {
		svc, fs := linkedGateService(t, "  \t", []byte(`["  \t"]`), []string{"  \t"}, nil)
		if err := create(svc); !errors.Is(err, ErrNotPRDIssue) || fs.createRunParams != nil {
			t.Fatalf("err = %v, run created = %v", err, fs.createRunParams != nil)
		}
	})
	t.Run("a linked repo with no capture is refused, never started on the cache alone", func(t *testing.T) {
		svc, fs := linkedGateService(t, "ready-for-uzi", []byte(`["ready-for-uzi"]`), []string{"ready-for-uzi"}, nil)
		svc.SetForges(nil)
		if err := create(svc); !errors.Is(err, ErrForgeBuild) || fs.createRunParams != nil {
			t.Fatalf("err = %v, run created = %v", err, fs.createRunParams != nil)
		}
	})
}
