package kube

import (
	"strings"
	"testing"

	"gitlab.example.com/vtmocanu/uzi/controller/internal/preset"
)

// ValidatePVCCeilings is what stands between a raised preset (or a lowered cluster
// ceiling) and a fleet of workers that provision and never appear (issue #224). It is
// tested as an ordinary function over synthetic ceilings rather than against the
// chart's shipped numbers, which is the point: production reads the real ceiling from
// the env, so nothing here has to hold a copy of it that can go stale.
func TestValidatePVCCeilings(t *testing.T) {
	resolver := testResolver(t)
	// The largest claims in the shipped tables, named so a failure below is readable:
	// nixSize is 20Gi flat, `l`'s DataSize is 20Gi, the dind default is 20Gi.
	for _, tc := range []struct {
		name    string
		cfg     RenderConfig
		wantErr []string // substrings that must ALL appear; empty = must succeed
	}{
		{
			name: "no ceilings supplied: both tiers skipped",
			cfg:  RenderConfig{},
		},
		{
			name: "shipped ceilings: everything fits exactly",
			cfg:  RenderConfig{MaxPVCStorage: "20Gi", DockerMaxPVCStorage: "20Gi"},
		},
		{
			name:    "restricted ceiling below nixSize",
			cfg:     RenderConfig{MaxPVCStorage: "10Gi"},
			wantErr: []string{"restricted tier", "-nix", "20Gi", "UZI_WORKER_MAX_PVC_STORAGE", "10Gi"},
		},
		{
			name:    "restricted ceiling below the largest preset's DataSize",
			cfg:     RenderConfig{MaxPVCStorage: "15Gi"},
			wantErr: []string{"restricted tier", "-data", `"l"`},
		},
		{
			name: "docker ceiling below the dind data root's EFFECTIVE default",
			// The knob is UNSET, so the controller supplies its own 20Gi. Checking only
			// an explicit override would leave exactly this path unguarded, which is the
			// hole the chart guard originally had.
			cfg:     RenderConfig{DockerMaxPVCStorage: "10Gi"},
			wantErr: []string{"docker tier", "-dind-data", "UZI_WORKER_DOCKER_MAX_PVC_STORAGE"},
		},
		{
			name:    "docker ceiling below an explicit dind override",
			cfg:     RenderConfig{DockerMaxPVCStorage: "30Gi", DinDDataSize: "40Gi"},
			wantErr: []string{"docker tier", "-dind-data", "40Gi", "30Gi"},
		},
		{
			name: "an override that FITS a raised ceiling is accepted",
			cfg:  RenderConfig{MaxPVCStorage: "40Gi", DockerMaxPVCStorage: "40Gi", DinDDataSize: "40Gi"},
		},
		{
			name: "the two tiers are INDEPENDENT: a lowered docker ceiling does not implicate the restricted tier",
			cfg:  RenderConfig{MaxPVCStorage: "20Gi", DockerMaxPVCStorage: "10Gi"},
			// dind-data is docker-only, but /nix and /data are claimed in BOTH tiers, so
			// a 10Gi docker ceiling must implicate them for the DOCKER tier and leave the
			// restricted tier alone.
			wantErr: []string{"docker tier"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePVCCeilings(tc.cfg, resolver)
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("want no error, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want an error naming %v, got nil — an oversized claim that boots is a fleet that "+
					"provisions and never appears", tc.wantErr)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not mention %q, so an operator cannot act on it.\ngot: %v", want, err)
				}
			}
		})
	}
}

// The independence claim above, asserted directly rather than inferred from a
// substring: a lowered DOCKER ceiling must not produce a restricted-tier complaint.
func TestValidatePVCCeilingsKeepsTheTiersSeparate(t *testing.T) {
	err := ValidatePVCCeilings(RenderConfig{MaxPVCStorage: "20Gi", DockerMaxPVCStorage: "10Gi"}, testResolver(t))
	if err == nil {
		t.Fatal("a 10Gi docker ceiling must be rejected: /nix alone is 20Gi")
	}
	if strings.Contains(err.Error(), "restricted tier") {
		t.Errorf("a lowered DOCKER ceiling implicated the RESTRICTED tier; the two are separate namespaces "+
			"with separate LimitRanges and must be checked independently.\ngot: %v", err)
	}
}

// A ceiling this controller cannot parse must be an error, never a silent skip — a
// skip would restore exactly the unguarded state this function exists to remove.
// Config validates the var at boot, so reaching this means someone bypassed it.
func TestValidatePVCCeilingsRejectsAnUnparseableCeiling(t *testing.T) {
	err := ValidatePVCCeilings(RenderConfig{MaxPVCStorage: "20GB"}, testResolver(t))
	if err == nil || !strings.Contains(err.Error(), "UZI_WORKER_MAX_PVC_STORAGE") {
		t.Fatalf("want an error naming the var, got: %v", err)
	}
}

// Vacuity guard, the idiom this repo states in three places: a check that iterates an
// empty collection passes while asserting nothing. If the preset tables ever resolve
// to nothing, every case above goes green over zero claimants.
func TestPresetTablesAreNotEmpty(t *testing.T) {
	if len(preset.SizeNames()) == 0 || len(preset.TemplateNames()) == 0 {
		t.Fatalf("preset tables are empty (%d sizes, %d templates); every PVC-ceiling assertion would pass "+
			"vacuously over zero claims", len(preset.SizeNames()), len(preset.TemplateNames()))
	}
}
