package config

import (
	"strings"
	"testing"
)

func TestLoadWorkerGoConcurrency(t *testing.T) {
	const (
		build = "UZI_WORKER_GO_BUILD_PARALLELISM"
		procs = "UZI_WORKER_GO_MAXPROCS"
	)
	setup := func(t *testing.T, buildVal, procsVal string) {
		t.Helper()
		setWorkerEnv(t)
		t.Setenv("UZI_API_URL", "https://uzi.example.com")
		t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "controller-token"))
		t.Setenv(build, buildVal)
		t.Setenv(procs, procsVal)
	}

	valid := []struct {
		raw  string
		want int
	}{
		{"", 0}, {"   ", 0}, {"4", 4}, {" 4 ", 4}, {"007", 7}, {"2147483647", 2147483647},
	}
	for _, v := range valid {
		t.Run("build/"+v.raw, func(t *testing.T) {
			setup(t, v.raw, "")
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.WorkerGoBuildParallelism != v.want || cfg.WorkerGoMaxProcs != 0 {
				t.Fatalf("got build=%d procs=%d", cfg.WorkerGoBuildParallelism, cfg.WorkerGoMaxProcs)
			}
		})
		t.Run("procs/"+v.raw, func(t *testing.T) {
			setup(t, "", v.raw)
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.WorkerGoMaxProcs != v.want || cfg.WorkerGoBuildParallelism != 0 {
				t.Fatalf("got build=%d procs=%d", cfg.WorkerGoBuildParallelism, cfg.WorkerGoMaxProcs)
			}
		})
	}

	t.Run("both", func(t *testing.T) {
		setup(t, " 2 ", "009")
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.WorkerGoBuildParallelism != 2 || cfg.WorkerGoMaxProcs != 9 {
			t.Fatalf("got build=%d procs=%d", cfg.WorkerGoBuildParallelism, cfg.WorkerGoMaxProcs)
		}
	})

	invalid := []string{"0", "-1", "+1", "1.5", "2147483648", "abc", "2 -toolexec=x", "99999999999999999999", "1e2"}
	for _, raw := range invalid {
		for _, name := range []string{build, procs} {
			t.Run("invalid/"+name+"/"+raw, func(t *testing.T) {
				setup(t, "", "")
				t.Setenv(name, raw)
				if _, err := Load(); err == nil || !strings.Contains(err.Error(), name) {
					t.Fatalf("%s=%q: %v", name, raw, err)
				}
			})
		}
	}
}
