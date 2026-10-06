package hostedsvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestEphemeralDockerPreferenceDecision(t *testing.T) {
	repo := uuid.New()
	valid := pgtype.UUID{Bytes: repo, Valid: true}
	for _, tc := range []struct {
		name             string
		preference, tier bool
		repo             pgtype.UUID
		kind             string
		profile          pgtype.UUID
		list             []uuid.UUID
		want             bool
	}{
		{"ordinary", true, true, valid, "issue", pgtype.UUID{}, []uuid.UUID{repo}, true},
		{"judge with repo", true, true, valid, "judge", pgtype.UUID{}, []uuid.UUID{repo}, true},
		{"preference off", false, true, valid, "issue", pgtype.UUID{}, []uuid.UUID{repo}, false},
		{"tier off", true, false, valid, "issue", pgtype.UUID{}, []uuid.UUID{repo}, false},
		{"repo-less judge", true, true, pgtype.UUID{}, "judge", pgtype.UUID{}, []uuid.UUID{repo}, false},
		{"job", true, true, valid, "job", pgtype.UUID{}, []uuid.UUID{repo}, false},
		{"null kind", true, true, valid, "", pgtype.UUID{}, []uuid.UUID{repo}, false},
		{"lane", true, true, valid, "issue", valid, []uuid.UUID{repo}, false},
		{"nil list", true, true, valid, "issue", pgtype.UUID{}, nil, false},
		{"empty list", true, true, valid, "issue", pgtype.UUID{}, []uuid.UUID{}, false},
		{"unallowlisted", true, true, valid, "issue", pgtype.UUID{}, []uuid.UUID{uuid.New()}, false},
		{"membership after nonmatch", true, true, valid, "issue", pgtype.UUID{}, []uuid.UUID{uuid.New(), repo}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := EphemeralDockerPreferenceApplies(tc.preference, tc.tier, tc.repo, pgtype.Text{String: tc.kind, Valid: tc.kind != ""}, tc.profile, tc.list); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

type dockerSettingsProbe struct {
	enabled bool
	err     error
	reads   int
}

func (s *dockerSettingsProbe) EphemeralWorkersEnabled(context.Context) (bool, error) {
	return s.enabled, s.err
}
func (s *dockerSettingsProbe) DockerRepoAllowlist(context.Context) ([]uuid.UUID, error) {
	s.reads++
	return nil, errors.New("unexpected allowlist read")
}

func TestEphemeralDockerKillSwitchAvoidsReads(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"off", nil}, {"read error", errors.New("settings unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &dockerSettingsProbe{err: tc.err}
			p := NewEphemeralProvisioner(nil, nil, nil, s, EphemeralConfig{DockerEnabled: true})
			n, err := p.ProvisionPass(context.Background())
			if n != 0 || (err != nil) != (tc.err != nil) {
				t.Fatalf("ProvisionPass = %d, %v", n, err)
			}
			if s.reads != 0 {
				t.Fatalf("allowlist reads=%d, want 0", s.reads)
			}
		})
	}
}
