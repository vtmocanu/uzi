package config

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// UZI_WORKER_TOLERATIONS is a JSON array; every "none" spelling the chart can render
// (unset, empty, null, []) loads as nil, so a cleared list hashes as before.
func TestLoadParsesTheWorkerTolerations(t *testing.T) {
	sixty := int64(60)
	for raw, want := range map[string][]corev1.Toleration{
		"":     nil,
		"null": nil,
		"[]":   nil,
		`[{"key":"uzi","operator":"Equal","value":"true","effect":"NoSchedule"}]`: {
			{Key: "uzi", Operator: corev1.TolerationOpEqual, Value: "true", Effect: corev1.TaintEffectNoSchedule},
		},
		// Operator defaults to Equal; Exists with an empty key tolerates every taint.
		`[{"key":"uzi","value":"true"},{"operator":"Exists"}]`: {
			{Key: "uzi", Value: "true"},
			{Operator: corev1.TolerationOpExists},
		},
		`[{"key":"node.kubernetes.io/unreachable","operator":"Exists","effect":"NoExecute","tolerationSeconds":60}]`: {
			{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &sixty},
		},
	} {
		t.Run(raw, func(t *testing.T) {
			setWorkerEnv(t)
			t.Setenv("UZI_API_URL", "https://uzi.example.com")
			t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))
			t.Setenv("UZI_WORKER_TOLERATIONS", raw)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !reflect.DeepEqual(cfg.WorkerTolerations, want) {
				t.Errorf("WorkerTolerations = %#v, want %#v", cfg.WorkerTolerations, want)
			}
		})
	}
}

// A toleration the API server would reject, or a misspelt field that would silently
// widen it, fails at boot rather than as workers that never appear.
func TestLoadRejectsBadWorkerTolerations(t *testing.T) {
	for _, bad := range []string{
		`{"key":"uzi"}`,
		`[{"key":"uzi"}`,
		`[{"key":"uzi","efect":"NoSchedule"}]`,
		`[{"key":"bad key!","operator":"Exists"}]`,
		`[{"key":"uzi","operator":"In"}]`,
		`[{"key":"uzi","operator":"Exists","value":"true"}]`,
		`[{"value":"true"}]`,
		`[{"key":"uzi","value":"not a label value"}]`,
		`[{"key":"uzi","effect":"NoRun"}]`,
		`[{"key":"uzi","effect":"NoSchedule","tolerationSeconds":60}]`,
		`[{"key":"uzi"}] []`,
	} {
		t.Run(bad, func(t *testing.T) {
			setWorkerEnv(t)
			t.Setenv("UZI_API_URL", "https://uzi.example.com")
			t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))
			t.Setenv("UZI_WORKER_TOLERATIONS", bad)
			if _, err := Load(); err == nil {
				t.Fatalf("UZI_WORKER_TOLERATIONS=%s loaded without error", bad)
			}
		})
	}
}
