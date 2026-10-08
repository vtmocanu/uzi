package main

import (
	"strings"
	"testing"
	"time"
)

func TestUXLabPRScenesRenderDetail(t *testing.T) {
	now := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	scenes := []struct {
		name   string
		render func(bool, time.Time) string
		login  string
		state  string
	}{
		{"pr-live", prLive, "coderabbitai", "commented"},
		{"pr-changes-requested", prChangesRequested, "coderabbitai", "changes requested"},
		{"pr-failing", prFailing, "vtmocanu", "review requested"},
	}
	themes := []struct {
		name string
		dark bool
	}{
		{"dark", true},
		{"light", false},
	}

	for _, scene := range scenes {
		for _, theme := range themes {
			t.Run(scene.name+"/"+theme.name, func(t *testing.T) {
				frame := stripANSI(scene.render(theme.dark, now))
				if !strings.Contains(frame, "CI / lint-api") {
					t.Errorf("missing CI / lint-api in frame:\n%s", frame)
				}
				if strings.Contains(frame, " loading…") {
					t.Errorf("detail still loading in frame:\n%s", frame)
				}

				reviews := strings.Index(frame, " REVIEWS")
				merge := strings.Index(frame, " MERGE")
				if reviews < 0 || merge < 0 || reviews >= merge {
					t.Fatalf("expected REVIEWS section before MERGE in frame:\n%s", frame)
				}
				for _, row := range strings.Split(frame[reviews:merge], "\n") {
					if strings.Contains(row, scene.login) && strings.Contains(row, scene.state) {
						return
					}
				}
				t.Errorf("missing review row with %q and %q between REVIEWS and MERGE:\n%s",
					scene.login, scene.state, frame)
			})
		}
	}
}
