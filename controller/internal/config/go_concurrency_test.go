package config

import (
	"strings"
	"testing"
)

func TestLoadWorkerGoMemLimit(t *testing.T) {
	const name = "UZI_WORKER_GO_MEMLIMIT"
	load := func(t *testing.T, raw string) (Config, error) {
		t.Helper()
		setWorkerEnv(t)
		t.Setenv("UZI_API_URL", "https://uzi.example.com")
		t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "controller-token"))
		t.Setenv(name, raw)
		return Load()
	}

	for _, v := range []struct{ raw, want string }{
		{"6GiB", "6GiB"}, {"512MiB", "512MiB"}, {"007GiB", "7GiB"},
		{"\t 6GiB\n", "6GiB"}, {"2147483647GiB", "2147483647GiB"},
		{"", ""}, {"  \t ", ""},
	} {
		t.Run("ok/"+v.raw, func(t *testing.T) {
			cfg, err := load(t, v.raw)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.WorkerGoMemLimit != v.want {
				t.Fatalf("got %q, want %q", cfg.WorkerGoMemLimit, v.want)
			}
		})
	}

	const msg = "UZI_WORKER_GO_MEMLIMIT must be a positive integer <= 2147483647 followed by MiB or GiB"
	for _, raw := range []string{
		"0GiB", "00MiB", "6gib", "6G", "6GB", "6", "1.5GiB", "+6GiB", "-6GiB",
		"2147483648GiB", "off", "\ufeff6GiB", "\u00856GiB",
	} {
		t.Run("invalid/"+raw, func(t *testing.T) {
			if _, err := load(t, raw); err == nil || !strings.Contains(err.Error(), msg) {
				t.Fatalf("%q: %v", raw, err)
			}
		})
	}
}

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
