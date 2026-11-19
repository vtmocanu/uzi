package workersize

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/workersize/builtin"
)

var okSpec = Spec{CPURequestMillicores: 1000, CPULimitMillicores: 4000, MemoryRequestMiB: 8192, MemoryLimitMiB: 12288, DataGiB: 25, DinDDataGiB: 20}

func chartCeilings() builtin.Ceilings {
	c := builtin.Ceilings{TiersKnown: true}
	c.Enabled = [builtin.TierCount]bool{true, true, true}
	for t := builtin.Tier(0); t < builtin.TierCount; t++ {
		c.Tier[t].CPUMillicores = builtin.Present(4000)
		c.Tier[t].MemoryMiB = builtin.Present(32768)
	}
	c.Tier[builtin.TierRestricted].PVCGiB = builtin.Present(50)
	c.Tier[builtin.TierDocker].PVCGiB = builtin.Present(50)
	return c
}

func fullQuota(cpu, mem, deployments int64) builtin.Quota {
	var q builtin.Quota
	for t := builtin.Tier(0); t < builtin.TierCount; t++ {
		q.Tier[t] = builtin.TierQuota{
			RequestsCPUMillicores: builtin.Present(cpu), RequestsMemoryMiB: builtin.Present(mem), Deployments: builtin.Present(deployments)}
	}
	return q
}

func TestValidateRefusals(t *testing.T) {
	c := chartCeilings()
	q := fullQuota(1_000_000, 1_000_000, 10)
	with := func(f func(*Spec)) Spec { s := okSpec; f(&s); return s }
	cases := []struct {
		name  string
		spec  Spec
		field string
		want  string // substring of the field message
	}{
		{"zero cpu request", with(func(s *Spec) { s.CPURequestMillicores = 0 }), FieldCPURequest, "greater than 0"},
		{"negative memory", with(func(s *Spec) { s.MemoryLimitMiB = -1 }), FieldMemLimit, "greater than 0"},
		{"cpu above absolute cap", with(func(s *Spec) { s.CPULimitMillicores = 64001 }), FieldCPULimit, "absolute maximum of 64000"},
		{"memory above absolute cap", with(func(s *Spec) { s.MemoryLimitMiB = 524289 }), FieldMemLimit, "absolute maximum of 524288"},
		{"disk above absolute cap", with(func(s *Spec) { s.DataGiB = 2049 }), FieldData, "absolute maximum of 2048"},
		{"cpu request above limit", with(func(s *Spec) { s.CPURequestMillicores = 5000 }), FieldCPURequest, "exceeds the cpu limit"},
		{"memory request above limit", with(func(s *Spec) { s.MemoryRequestMiB = 20000 }), FieldMemRequest, "exceeds the memory limit"},
		{"memory over ceiling names value and chart path", with(func(s *Spec) { s.MemoryLimitMiB = 65536 }), FieldMemLimit,
			"memory limit 65536 MiB exceeds the 32768 MiB ceiling (workers.limitRange.max.memory)"},
		{"cpu over ceiling", with(func(s *Spec) { s.CPULimitMillicores = 8000 }), FieldCPULimit,
			"exceeds the 4000 millicores ceiling (workers.limitRange.max.cpu)"},
		{"data over pvc ceiling", with(func(s *Spec) { s.DataGiB = 60 }), FieldData,
			"data disk 60 GiB exceeds the 50 GiB ceiling (workers.limitRange.maxPVCStorage)"},
		{"dind over docker pvc ceiling", with(func(s *Spec) { s.DinDDataGiB = 60 }), FieldDindData,
			"(workers.docker.limitRange.maxPVCStorage)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Validate("m", tc.spec, nil, c, q)
			if r.OK() {
				t.Fatal("want a refusal")
			}
			if got := r.Fields[tc.field]; !strings.Contains(got, tc.want) {
				t.Errorf("fields[%s] = %q, want substring %q", tc.field, got, tc.want)
			}
		})
	}
}

func TestValidateAcceptsAtTheCeilingBoundary(t *testing.T) {
	s := Spec{CPURequestMillicores: 4000, CPULimitMillicores: 4000, MemoryRequestMiB: 32768, MemoryLimitMiB: 32768, DataGiB: 50, DinDDataGiB: 50}
	if r := Validate("x", s, nil, chartCeilings(), fullQuota(1e9, 1e9, 1)); !r.OK() {
		t.Fatalf("values equal to the ceilings must be accepted: %v", r.Fields)
	}
}

func TestValidateBindingCeilingIsTheMinimumOverEnabledTiers(t *testing.T) {
	c := chartCeilings()
	c.Tier[builtin.TierDocker].MemoryMiB = builtin.Present(16384)
	r := Validate("m", with(okSpec, func(s *Spec) { s.MemoryLimitMiB = 20000 }), nil, c, builtin.Quota{})
	if got := r.Fields[FieldMemLimit]; !strings.Contains(got, "16384 MiB ceiling (workers.docker.limitRange.max.memory)") {
		t.Fatalf("binding ceiling = %q", got)
	}
	// A disabled tier no longer binds.
	c.Enabled[builtin.TierDocker] = false
	if r := Validate("m", with(okSpec, func(s *Spec) { s.MemoryLimitMiB = 20000 }), nil, c, builtin.Quota{}); !r.OK() {
		t.Fatalf("a disabled tier's ceiling must not bind: %v", r.Fields)
	}
}

func with(s Spec, f func(*Spec)) Spec { f(&s); return s }

func TestValidateUnknownTiersAppliesEveryPresentCeiling(t *testing.T) {
	c := chartCeilings()
	c.TiersKnown = false
	c.Enabled = [builtin.TierCount]bool{}
	if r := Validate("m", with(okSpec, func(s *Spec) { s.MemoryLimitMiB = 65536 }), nil, c, builtin.Quota{}); r.OK() {
		t.Fatal("with TIERS unset every present ceiling must apply")
	}
}

func TestValidateNoCeilingsOnlyAbsoluteCaps(t *testing.T) {
	big := with(okSpec, func(s *Spec) { s.MemoryLimitMiB = 524288; s.MemoryRequestMiB = 524288 })
	if r := Validate("m", big, nil, builtin.Ceilings{}, builtin.Quota{}); !r.OK() {
		t.Fatalf("compose has no ceilings, only the absolute caps: %v", r.Fields)
	}
}

func TestValidateDindOverrideSkipsDockerPVCCheckOnly(t *testing.T) {
	c := chartCeilings()
	c.DindOverridden = true
	s := with(okSpec, func(s *Spec) { s.DinDDataGiB = 60 })
	if r := Validate("m", s, nil, c, builtin.Quota{}); !r.OK() {
		t.Fatalf("an overridden dind size is not validated against the PVC ceiling: %v", r.Fields)
	}
	s.DinDDataGiB = 2049
	if r := Validate("m", s, nil, c, builtin.Quota{}); r.OK() {
		t.Fatal("the absolute cap still applies when overridden")
	}
}

func TestValidateDiskLoweredWarning(t *testing.T) {
	prev := with(okSpec, func(s *Spec) { s.DataGiB = 40; s.DinDDataGiB = 30 })
	r := Validate("m", okSpec, &prev, chartCeilings(), builtin.Quota{})
	if !r.OK() {
		t.Fatalf("lowering a disk is accepted: %v", r.Fields)
	}
	if len(r.Warnings) != 2 {
		t.Fatalf("warnings = %+v, want data and dind", r.Warnings)
	}
	for _, w := range r.Warnings {
		if w.Kind != KindDiskLowered || !strings.Contains(w.Message, "existing claims keep their size; only new claims use the lower value") {
			t.Errorf("warning = %+v", w)
		}
	}
	// Raising or keeping a disk does not warn.
	up := with(okSpec, func(s *Spec) { s.DataGiB = 10 })
	if r := Validate("m", okSpec, &up, chartCeilings(), builtin.Quota{}); len(r.Warnings) != 0 {
		t.Errorf("unexpected warnings %+v", r.Warnings)
	}
}

func advisoryKinds(r Result) map[string]int {
	m := map[string]int{}
	for _, a := range r.Advisories {
		m[a.Tier+"/"+a.Kind]++
	}
	return m
}

func TestQuotaAdvisoryUnavailableIsNeverFits(t *testing.T) {
	// Tiers unknown: one advisory, no tier.
	c := chartCeilings()
	c.TiersKnown = false
	r := Validate("m", okSpec, nil, c, fullQuota(1e9, 1e9, 1))
	if len(r.Advisories) != 1 || r.Advisories[0].Kind != KindQuotaUnavailable || !strings.Contains(r.Advisories[0].Message, "quota advisory unavailable") {
		t.Fatalf("advisories = %+v", r.Advisories)
	}
	// A tier missing any of its three values is unavailable, whichever one is missing.
	for i := 0; i < 3; i++ {
		q := fullQuota(1e9, 1e9, 10)
		tq := &q.Tier[builtin.TierDocker]
		[]*builtin.Value{&tq.RequestsCPUMillicores, &tq.RequestsMemoryMiB, &tq.Deployments}[i].Set = false
		got := advisoryKinds(Validate("m", okSpec, nil, chartCeilings(), q))
		if got["docker/"+KindQuotaUnavailable] != 1 {
			t.Errorf("missing value %d: advisories %v", i, got)
		}
		if got["docker/"+KindQuotaExceeded] != 0 || got["docker/"+KindQuotaZero] != 0 {
			t.Errorf("missing value %d must not read as fits or exceeded: %v", i, got)
		}
	}
	// A disabled tier yields nothing.
	c = chartCeilings()
	c.Enabled[builtin.TierIsolated] = false
	if got := advisoryKinds(Validate("m", okSpec, nil, c, builtin.Quota{})); got["isolated/"+KindQuotaUnavailable] != 0 {
		t.Errorf("disabled tier advised: %v", got)
	}
}

// A docker tier whose worker requests alone fit exactly (1000m x 10 = 10000), so any
// sidecar request on top would exceed, must say the estimate is incomplete, never stay
// silent (#4517). The api holds no sidecar requests, so the test pins only the note.
func TestQuotaAdvisoryDockerSidecarExcludedIsIncomplete(t *testing.T) {
	q := fullQuota(1e9, 1e9, 10)
	q.Tier[builtin.TierDocker] = builtin.TierQuota{
		RequestsCPUMillicores: builtin.Present(10000), RequestsMemoryMiB: builtin.Present(1e9), Deployments: builtin.Present(10)}
	r := Validate("m", okSpec, nil, chartCeilings(), q)
	if !r.OK() {
		t.Fatalf("an advisory never refuses: %v", r.Fields)
	}
	var incomplete int
	for _, a := range r.Advisories {
		if a.Tier == "docker" && a.Kind == KindQuotaUnavailable {
			incomplete++
			if !strings.Contains(a.Message, "incomplete") || !strings.Contains(a.Message, "sidecar") || a.Field != "" {
				t.Errorf("docker advisory = %+v, want an incomplete/sidecar message with no field", a)
			}
		}
	}
	got := advisoryKinds(r)
	for _, a := range r.Advisories {
		if a.Kind == KindQuotaUnavailable && a.Tier != "docker" {
			t.Errorf("complete %s tier must not read unavailable: %+v", a.Tier, a)
		}
	}
	if incomplete != 1 || got["docker/"+KindQuotaExceeded] != 0 || got["docker/"+KindQuotaZero] != 0 {
		t.Fatalf("docker advisories = %v, want exactly one incomplete and no exceeded/zero", got)
	}
	// The restricted tier's complete estimate is unchanged: identical to docker disabled.
	c := chartCeilings()
	c.Enabled[builtin.TierDocker] = false
	var withDocker, without []Advisory
	for _, a := range r.Advisories {
		if a.Tier != "docker" {
			withDocker = append(withDocker, a)
		}
	}
	for _, a := range Validate("m", okSpec, nil, c, q).Advisories {
		if a.Tier == "docker" {
			t.Errorf("disabled docker tier advised: %+v", a)
		}
		without = append(without, a)
	}
	if fmt.Sprint(withDocker) != fmt.Sprint(without) || got["restricted/"+KindQuotaUnavailable] != 0 {
		t.Errorf("non-docker advisories changed: %+v vs %+v", withDocker, without)
	}
	// Definite warnings are preserved alongside the incomplete note.
	zero := fullQuota(1e9, 1e9, 10)
	zero.Tier[builtin.TierDocker].RequestsCPUMillicores = builtin.Present(0)
	if got := advisoryKinds(Validate("m", okSpec, nil, chartCeilings(), zero)); got["docker/"+KindQuotaZero] != 1 || got["docker/"+KindQuotaUnavailable] != 1 {
		t.Errorf("zero docker quota: %v", got)
	}
	over := fullQuota(1e9, 1e9, 10)
	over.Tier[builtin.TierDocker].RequestsCPUMillicores = builtin.Present(9999)
	if got := advisoryKinds(Validate("m", okSpec, nil, chartCeilings(), over)); got["docker/"+KindQuotaExceeded] != 1 || got["docker/"+KindQuotaUnavailable] != 1 {
		t.Errorf("worker-only exceeded docker quota: %v", got)
	}
}

func TestQuotaAdvisoryZeroEachValue(t *testing.T) {
	for i := 0; i < 3; i++ {
		q := fullQuota(1e9, 1e9, 10)
		tq := &q.Tier[builtin.TierRestricted]
		*[]*builtin.Value{&tq.RequestsCPUMillicores, &tq.RequestsMemoryMiB, &tq.Deployments}[i] = builtin.Present(0)
		r := Validate("m", okSpec, nil, chartCeilings(), q)
		if !r.OK() {
			t.Fatalf("a zero quota never refuses: %v", r.Fields)
		}
		var found bool
		for _, a := range r.Advisories {
			if a.Tier == "restricted" && a.Kind == KindQuotaZero && strings.Contains(a.Message, "no worker can be admitted on this tier") {
				found = true
			}
		}
		if !found {
			t.Errorf("zero value %d: advisories %+v", i, r.Advisories)
		}
	}
}

func TestQuotaAdvisoryExceededBoundary(t *testing.T) {
	// request 1000 x 10 deployments = 10000: equal to the quota is NOT exceeded.
	at := advisoryKinds(Validate("m", okSpec, nil, chartCeilings(), fullQuota(10000, 1e9, 10)))
	if at["restricted/"+KindQuotaExceeded] != 0 {
		t.Errorf("exact boundary must not be exceeded: %v", at)
	}
	over := Validate("m", okSpec, nil, chartCeilings(), fullQuota(9999, 1e9, 10))
	var cpu bool
	for _, a := range over.Advisories {
		if a.Kind == KindQuotaExceeded && a.Field == FieldCPURequest && a.Tier == "restricted" && strings.Contains(a.Message, "workers.quota.requestsCPU") {
			cpu = true
		}
		if a.Field == FieldMemRequest {
			t.Errorf("memory fits and must not be flagged: %+v", a)
		}
	}
	if !cpu {
		t.Fatalf("one over the quota must be exceeded: %+v", over.Advisories)
	}
	// Memory separately.
	mem := Validate("m", okSpec, nil, chartCeilings(), fullQuota(1e9, 81919, 10))
	if got := advisoryKinds(mem)["restricted/"+KindQuotaExceeded]; got != 1 {
		t.Errorf("memory exceeded advisories = %d, want 1: %+v", got, mem.Advisories)
	}
}

func TestQuotaExceededSingleRequestAboveQuota(t *testing.T) {
	// deployments = 1 and a request above the quota is exceeded even though the product equals it.
	if !quotaExceeded(11, 1, 10) {
		t.Error("a single request above the quota is exceeded")
	}
}

// math.MaxInt64 must not wrap: a naive int64 multiply of 2 x MaxInt64 wraps to -2 and
// reads as "fits".
func TestQuotaExceededDoesNotOverflow(t *testing.T) {
	cases := []struct {
		name                        string
		request, deployments, quota int64
		want                        bool
	}{
		{"2 x MaxInt64 against MaxInt64", 2, math.MaxInt64, math.MaxInt64, true},
		{"MaxInt64 x MaxInt64", math.MaxInt64, math.MaxInt64, math.MaxInt64, true},
		{"MaxInt64 x 1 equals quota", math.MaxInt64, 1, math.MaxInt64, false},
		{"huge deployments, tiny request fits", 1, math.MaxInt64, math.MaxInt64, false},
		{"zero deployments never exceeds by product", 5, 0, 10, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := quotaExceeded(tc.request, tc.deployments, tc.quota); got != tc.want {
				t.Errorf("quotaExceeded(%d, %d, %d) = %v, want %v", tc.request, tc.deployments, tc.quota, got, tc.want)
			}
		})
	}
	// End to end through Validate with MaxInt64 deployments.
	q := fullQuota(1e9, 1e9, math.MaxInt64)
	if got := advisoryKinds(Validate("m", okSpec, nil, chartCeilings(), q))["restricted/"+KindQuotaExceeded]; got != 2 {
		t.Errorf("exceeded advisories with MaxInt64 deployments = %d, want 2 (cpu, memory)", got)
	}
}
