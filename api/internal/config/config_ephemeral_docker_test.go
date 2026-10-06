package config

import (
	"strings"
	"testing"
)

func TestWorkerDockerEnabled(t *testing.T) {
	for _, tc := range []struct {
		name, hosting, docker string
		want, invalid         bool
	}{
		{"default", "false", "", false, false},
		{"available", "true", "true", true, false},
		{"tier off", "true", "false", false, false},
		{"hosting off", "false", "true", false, false},
		{"invalid", "false", "typo", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WORKER_HOSTING_ENABLED", tc.hosting)
			t.Setenv("WORKER_DOCKER_ENABLED", tc.docker)
			hash := ""
			if tc.hosting == "true" {
				hash = strings.Repeat("ab", 32)
			}
			t.Setenv("WORKER_HOSTING_CONTROLLER_TOKEN_SHA256", hash)
			var cfg Config
			err := loadWorkerHosting(&cfg)
			if (err != nil) != tc.invalid {
				t.Fatalf("error=%v", err)
			}
			if cfg.WorkerDockerEnabled != tc.want {
				t.Fatalf("docker=%v", cfg.WorkerDockerEnabled)
			}
		})
	}
}
