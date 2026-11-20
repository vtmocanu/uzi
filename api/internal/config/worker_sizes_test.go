package config

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/workersize/builtin"
)

// sizeEnvBase sets the minimum env Load needs and clears every worker-size key, so each
// case states only what it sets.
func sizeEnvBase(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://uzi:pw@db:5432/uzi?sslmode=disable")
	t.Setenv("JWT_SECRET", "unit-test-jwt-signing-key-not-a-real-secret")
	varied := make([]byte, secretbox.KeySize)
	for i := range varied {
		varied[i] = byte(i + 1)
	}
	t.Setenv("UZI_SECRET_KEY", base64.StdEncoding.EncodeToString(varied))
	keys := []string{"UZI_WORKER_SIZE_TIERS", "UZI_WORKER_DIND_DATA_SIZE_OVERRIDDEN"}
	for tier := builtin.Tier(0); tier < builtin.TierCount; tier++ {
		for _, k := range ceilingEnvKeys[tier] {
			if k != "" {
				keys = append(keys, k)
			}
		}
		keys = append(keys, quotaEnvKeys[tier][:]...)
	}
	for _, k := range keys {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkerSizeLimitsUnsetMeansUnknownAndAbsent(t *testing.T) {
	sizeEnvBase(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkerSizeCeilings.TiersKnown {
		t.Error("TiersKnown with UZI_WORKER_SIZE_TIERS unset")
	}
	if cfg.WorkerSizeCeilings.Tier[builtin.TierRestricted].MemoryMiB.Set {
		t.Error("an unset ceiling must be absent")
	}
	if cfg.WorkerSizeQuota.Tier[builtin.TierDocker].Deployments.Set {
		t.Error("an unset quota must be absent (unavailable), never zero or unlimited")
	}
}

func TestWorkerSizeLimitsParse(t *testing.T) {
	sizeEnvBase(t)
	t.Setenv("UZI_WORKER_SIZE_TIERS", "restricted, docker")
	t.Setenv("UZI_WORKER_SIZE_CEILING_RESTRICTED_MEMORY_MIB", "32768")
	t.Setenv("UZI_WORKER_SIZE_CEILING_DOCKER_PVC_GIB", "50")
	t.Setenv("UZI_WORKER_DIND_DATA_SIZE_OVERRIDDEN", "1")
	t.Setenv("UZI_WORKER_SIZE_QUOTA_RESTRICTED_DEPLOYMENTS", "20")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c := cfg.WorkerSizeCeilings
	if !c.TiersKnown || !c.Enabled[builtin.TierRestricted] || !c.Enabled[builtin.TierDocker] || c.Enabled[builtin.TierIsolated] {
		t.Errorf("tiers = %+v", c.Enabled)
	}
	if got := c.Tier[builtin.TierRestricted].MemoryMiB; !got.Set || got.N != 32768 {
		t.Errorf("restricted memory ceiling = %+v", got)
	}
	if got := c.Tier[builtin.TierDocker].PVCGiB; !got.Set || got.N != 50 {
		t.Errorf("docker pvc ceiling = %+v", got)
	}
	if !c.DindOverridden {
		t.Error("DindOverridden not set from UZI_WORKER_DIND_DATA_SIZE_OVERRIDDEN=1")
	}
	if got := cfg.WorkerSizeQuota.Tier[builtin.TierRestricted].Deployments; !got.Set || got.N != 20 {
		t.Errorf("restricted deployments = %+v", got)
	}
}

// Each of the three quota values may legitimately be zero ("admit nothing"): it boots and
// is exposed as a SET zero, distinct from an absent value.
func TestWorkerSizeQuotaZeroBootsAndIsExposedAsZero(t *testing.T) {
	cases := []struct {
		key  string
		tier builtin.Tier
		get  func(builtin.TierQuota) builtin.Value
	}{
		{"UZI_WORKER_SIZE_QUOTA_DOCKER_REQUESTS_CPU_MILLICORES", builtin.TierDocker, func(q builtin.TierQuota) builtin.Value { return q.RequestsCPUMillicores }},
		{"UZI_WORKER_SIZE_QUOTA_DOCKER_REQUESTS_MEMORY_MIB", builtin.TierDocker, func(q builtin.TierQuota) builtin.Value { return q.RequestsMemoryMiB }},
		{"UZI_WORKER_SIZE_QUOTA_DOCKER_DEPLOYMENTS", builtin.TierDocker, func(q builtin.TierQuota) builtin.Value { return q.Deployments }},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			sizeEnvBase(t)
			t.Setenv(tc.key, "0")
			cfg, err := Load()
			if err != nil {
				t.Fatalf("a zero quota must boot: %v", err)
			}
			got := tc.get(cfg.WorkerSizeQuota.Tier[tc.tier])
			if !got.Set || got.N != 0 {
				t.Errorf("%s=0 exposed as %+v, want set zero", tc.key, got)
			}
		})
	}
}

// Operator constraint: unparsable quota data reads as unavailable, never as unlimited or
// fits, and does not refuse boot (the quota is advisory).
func TestWorkerSizeQuotaMalformedIsUnavailable(t *testing.T) {
	for _, bad := range []string{"abc", "-1", "1.5", "99999999999999999999"} {
		t.Run(bad, func(t *testing.T) {
			sizeEnvBase(t)
			t.Setenv("UZI_WORKER_SIZE_QUOTA_ISOLATED_REQUESTS_MEMORY_MIB", bad)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("malformed quota must not refuse boot: %v", err)
			}
			if cfg.WorkerSizeQuota.Tier[builtin.TierIsolated].RequestsMemoryMiB.Set {
				t.Errorf("malformed quota %q must read as unavailable", bad)
			}
		})
	}
}

func TestWorkerSizeCeilingRefusesBadValues(t *testing.T) {
	for _, bad := range []string{"0", "-5", "abc", "1.5", "32Gi", "99999999999999999999"} {
		t.Run(bad, func(t *testing.T) {
			sizeEnvBase(t)
			t.Setenv("UZI_WORKER_SIZE_CEILING_DOCKER_MEMORY_MIB", bad)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "UZI_WORKER_SIZE_CEILING_DOCKER_MEMORY_MIB") {
				t.Fatalf("ceiling %q: err = %v, want a refusal naming the key", bad, err)
			}
		})
	}
}

func TestWorkerSizeTiersRefusesUnknownTier(t *testing.T) {
	sizeEnvBase(t)
	t.Setenv("UZI_WORKER_SIZE_TIERS", "restricted,gpu")
	if _, err := Load(); err == nil {
		t.Fatal("an unknown tier must refuse boot")
	}
}
