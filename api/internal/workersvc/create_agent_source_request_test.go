package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestCreateRunExplicitAgentSourceParams(t *testing.T) {
	for _, source := range []string{AgentSourceOwn, AgentSourceRepo} {
		fs := seededIssueStore()
		svc := New(fs, newBox(t), testParams())
		if _, err := svc.CreateRun(context.Background(), uuid.New(), uuid.New(), 4, "desc", nil, nil, false, nil, &AgentSelection{Source: source}, nil, nil, nil, nil); err != nil {
			t.Fatalf("CreateRun(%s): %v", source, err)
		}
		p := fs.createRunParams
		if p == nil {
			t.Fatalf("CreateRun(%s) never reached the insert", source)
		}
		if !p.AgentSourceRequested.Valid || p.AgentSourceRequested.String != source {
			t.Fatalf("agent_source_requested = %+v, want %s", p.AgentSourceRequested, source)
		}
		if p.AgentSource.Valid || p.AgentExclusions != nil || p.PlanSource != planSourceAgent || p.PlanMd.Valid {
			t.Fatalf("an unseeded explicit create must not seed a plan or a selection: %+v", p)
		}
	}
}

func TestCreateRunWithoutRequestPassesNoExplicitSource(t *testing.T) {
	fs := seededIssueStore()
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.CreateRun(context.Background(), uuid.New(), uuid.New(), 4, "desc", nil, nil, false, nil, nil, nil, nil, nil, nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if p := fs.createRunParams; p == nil || p.AgentSourceRequested.Valid {
		t.Fatalf("agent_source_requested = %+v, want NULL so the repo default applies", p)
	}
}

func TestCreateRunExplicitAgentSourceRefusals(t *testing.T) {
	for _, tc := range []struct {
		name      string
		seed      *SeededPlan
		requested AgentSelection
		want      error
	}{
		{"exclusions", nil, AgentSelection{Source: AgentSourceRepo, Exclusions: []string{"tester"}}, ErrCreateExclusionsNeedPlan},
		{"unknown source", nil, AgentSelection{Source: "bogus"}, ErrInvalidSelection},
		{"empty source", nil, AgentSelection{}, ErrInvalidSelection},
		{"alongside a seeded plan", &SeededPlan{PlanMD: "# Plan\nDo it."}, AgentSelection{Source: AgentSourceOwn}, ErrInvalidSelection},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := seededIssueStore()
			svc := New(fs, newBox(t), testParams())
			requested := tc.requested
			_, err := svc.CreateRun(context.Background(), uuid.New(), uuid.New(), 4, "desc", nil, nil, false, tc.seed, &requested, nil, nil, nil, nil)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if fs.createRunParams != nil {
				t.Fatalf("a refused request reached the insert: %+v", fs.createRunParams)
			}
		})
	}
}

func TestNonManualCreatePathsPassNoExplicitSource(t *testing.T) {
	for name, create := range map[string]func(*Service) error{
		"schedule": func(s *Service) error {
			_, err := s.CreateScheduledRun(context.Background(), uuid.New(), uuid.New(), 4, "desc", nil, nil, nil, false, nil, nil, nil)
			return err
		},
		"autopilot": func(s *Service) error {
			_, err := s.CreateAutopilotRun(context.Background(), uuid.New(), uuid.New(), 4, "desc", nil)
			return err
		},
		"scheduled autopilot": func(s *Service) error {
			_, err := s.CreateScheduledAutopilotRun(context.Background(), uuid.New(), uuid.New(), 4, "desc", nil, nil, nil, false, nil, nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			fs := seededIssueStore()
			svc := New(fs, newBox(t), testParams())
			if err := create(svc); err != nil {
				t.Fatalf("create: %v", err)
			}
			if p := fs.createRunParams; p == nil || p.AgentSourceRequested.Valid {
				t.Fatalf("agent_source_requested = %+v, want NULL so the repo default applies", p)
			}
		})
	}
}
