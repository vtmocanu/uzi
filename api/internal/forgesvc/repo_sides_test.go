package forgesvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
)

type sidesFake struct{ forgetest.BaseFake }

func TestRepoSidesResolveRepoForge(t *testing.T) {
	const token = "plain-token-value"
	const baseURL = "https://forge.example.test"
	box, err := secretbox.New([]byte("0123456789abcdefghijklmnopqrstuv"))
	if err != nil {
		t.Fatal(err)
	}
	fake := &sidesFake{}
	var gotType forge.Type
	var gotURL, gotToken string
	s := NewWithForgeBuilder(nil, box, time.Second, nil, func(kind forge.Type, u, pat string, _ time.Duration) (forge.Forge, error) {
		gotType, gotURL, gotToken = kind, u, pat
		return fake, nil
	})
	sealed, err := s.EncryptToken(token)
	if err != nil {
		t.Fatal(err)
	}
	repoID := uuid.New()

	tr, err := s.IssueTrackerForRepo(context.Background(), repoID, string(forge.TypeGitLab), baseURL, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if tr != forge.IssueTracker(fake) {
		t.Errorf("IssueTrackerForRepo = %v, want the built driver", tr)
	}
	if gotType != forge.TypeGitLab || gotURL != baseURL || gotToken != token {
		t.Errorf("builder got (%q, %q, %q)", gotType, gotURL, gotToken)
	}

	gotType, gotURL, gotToken = "", "", ""
	ch, err := s.CodeHostForRepo(context.Background(), repoID, string(forge.TypeGitLab), baseURL, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if ch != forge.CodeHost(fake) {
		t.Errorf("CodeHostForRepo = %v, want the built driver", ch)
	}
	if gotType != forge.TypeGitLab || gotURL != baseURL || gotToken != token {
		t.Errorf("builder got (%q, %q, %q)", gotType, gotURL, gotToken)
	}
}

func TestRepoSidesPassBuilderErrorThrough(t *testing.T) {
	box, err := secretbox.New([]byte("0123456789abcdefghijklmnopqrstuv"))
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("builder failed")
	s := NewWithForgeBuilder(nil, box, time.Second, nil, func(forge.Type, string, string, time.Duration) (forge.Forge, error) {
		return nil, boom
	})
	sealed, err := s.EncryptToken("t")
	if err != nil {
		t.Fatal(err)
	}
	tr, err := s.IssueTrackerForRepo(context.Background(), uuid.New(), string(forge.TypeGitLab), "https://x.test", sealed)
	if !errors.Is(err, boom) || tr != nil {
		t.Errorf("IssueTrackerForRepo = (%v, %v), want (nil, builder error)", tr, err)
	}
	ch, err := s.CodeHostForRepo(context.Background(), uuid.New(), string(forge.TypeGitLab), "https://x.test", sealed)
	if !errors.Is(err, boom) || ch != nil {
		t.Errorf("CodeHostForRepo = (%v, %v), want (nil, builder error)", ch, err)
	}
}
