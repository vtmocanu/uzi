package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// TestLoadClampsJobInputLimitsToTheWorkerCeilings (PRD #1909 D1): an operator limit above what a
// worker accepts is clamped to the worker ceiling; a value at or below it is kept.
func TestLoadClampsJobInputLimitsToTheWorkerCeilings(t *testing.T) {
	autoselectEnv(t)
	t.Setenv("UZI_JOB_INPUT_FILE_MAX_BYTES", strconv.FormatInt(WorkerJobInputFileMaxBytes*4, 10))
	t.Setenv("UZI_JOB_INPUTS_MAX_FILES", "100000")
	t.Setenv("UZI_JOB_INPUTS_MAX_BYTES", strconv.FormatInt(WorkerJobInputsMaxBytes*8, 10))
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.JobInputFileMaxBytes != WorkerJobInputFileMaxBytes || int64(cfg.JobInputsMaxFiles) != WorkerJobInputsMaxFiles || cfg.JobInputsMaxBytes != WorkerJobInputsMaxBytes {
		t.Fatalf("limits = %d / %d / %d, want the worker ceilings %d / %d / %d",
			cfg.JobInputFileMaxBytes, cfg.JobInputsMaxFiles, cfg.JobInputsMaxBytes,
			WorkerJobInputFileMaxBytes, WorkerJobInputsMaxFiles, WorkerJobInputsMaxBytes)
	}

	t.Setenv("UZI_JOB_INPUT_FILE_MAX_BYTES", "1048576")
	t.Setenv("UZI_JOB_INPUTS_MAX_FILES", strconv.FormatInt(WorkerJobInputsMaxFiles, 10))
	t.Setenv("UZI_JOB_INPUTS_MAX_BYTES", "2097152")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.JobInputFileMaxBytes != 1<<20 || int64(cfg.JobInputsMaxFiles) != WorkerJobInputsMaxFiles || cfg.JobInputsMaxBytes != 2<<20 {
		t.Fatalf("in-range limits changed: %d / %d / %d", cfg.JobInputFileMaxBytes, cfg.JobInputsMaxFiles, cfg.JobInputsMaxBytes)
	}
}

// TestWorkerJobInputCeilingsMatchTheAgent pins the Go ceilings to JOB_INPUT_CEILINGS in
// agent/src/job-workspace.ts by reading that source, so the two cannot drift silently.
func TestWorkerJobInputCeilingsMatchTheAgent(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "agent", "src", "job-workspace.ts"))
	if err != nil {
		t.Fatalf("read agent/src/job-workspace.ts: %v", err)
	}
	block := regexp.MustCompile(`(?s)export const JOB_INPUT_CEILINGS = \{(.*?)\} as const`).FindSubmatch(src)
	if block == nil {
		t.Fatal("JOB_INPUT_CEILINGS not found in agent/src/job-workspace.ts")
	}
	// Each value is a product of integer literals: `256 * 1024 * 1024`.
	value := func(field string) int64 {
		m := regexp.MustCompile(field + `:\s*([0-9 *]+),`).FindSubmatch(block[1])
		if m == nil {
			t.Fatalf("JOB_INPUT_CEILINGS.%s not found", field)
		}
		prod := int64(1)
		for _, f := range regexp.MustCompile(`[0-9]+`).FindAll(m[1], -1) {
			n, err := strconv.ParseInt(string(f), 10, 64)
			if err != nil {
				t.Fatalf("JOB_INPUT_CEILINGS.%s: %v", field, err)
			}
			prod *= n
		}
		return prod
	}
	for _, c := range []struct {
		field string
		want  int64
	}{
		{"fileBytes", WorkerJobInputFileMaxBytes},
		{"files", WorkerJobInputsMaxFiles},
		{"totalBytes", WorkerJobInputsMaxBytes},
	} {
		if got := value(c.field); got != c.want {
			t.Errorf("JOB_INPUT_CEILINGS.%s = %d in agent/src/job-workspace.ts, Go constant is %d", c.field, got, c.want)
		}
	}
}
