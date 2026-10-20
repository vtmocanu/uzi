package kube

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

const runnerInitGolden = "testdata/runner-shared-init.sh"

// The rendered runner-init script is pinned byte for byte. Drift here must be a
// deliberate, reviewed change: runner_init_exec_test.go executes the rendered script
// and the worker-uid lane (e2e/worker-uid) exercises it under real uids.
// Regenerate with `UPDATE_GOLDEN=1 go test ./internal/kube/ -run RunnerInitScriptMatchesGolden`.
func TestRunnerInitScriptMatchesGolden(t *testing.T) {
	got := runnerInitScript()
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(runnerInitGolden, []byte(got), 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(runnerInitGolden)
	if err != nil {
		t.Fatalf("read golden (regenerate with UPDATE_GOLDEN=1 go test ./internal/kube/ -run RunnerInitScriptMatchesGolden): %v", err)
	}
	if string(want) != got {
		t.Fatalf("rendered runner-init script differs from %s (regenerate with UPDATE_GOLDEN=1 go test ./internal/kube/ -run RunnerInitScriptMatchesGolden)\n--- want ---\n%s\n--- got ---\n%s", runnerInitGolden, want, got)
	}
}

// The script must at least parse as POSIX sh (`sh -n` never executes it).
func TestRunnerInitScriptParses(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "init.sh")
	if err := os.WriteFile(path, []byte(runnerInitScript()), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(sh, "-n", path).CombinedOutput(); err != nil { //nolint:gosec // G204: sh from exec.LookPath, path under t.TempDir.
		t.Fatalf("sh -n: %v\n%s", err, out)
	}
}

// The image gate is the first EFFECTIVE statement: nothing but `set -eu` and the marker
// variable precede it, and no mkdir/chown/chmod appears before its refusal exits.
func TestRunnerInitScriptGatesOnTheImageMarkerBeforeAnyWrite(t *testing.T) {
	var lines []string
	for _, l := range strings.Split(runnerInitScript(), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		lines = append(lines, l)
	}
	if len(lines) < 5 || lines[0] != "set -eu" || lines[1] != "M="+runnerLayoutMarkerFile {
		t.Fatalf("script must open with `set -eu` then the marker path, got %q", lines[:2])
	}
	if !strings.HasPrefix(lines[2], "if [ -L \"$M\" ]") || !strings.Contains(lines[2], `"`+runnerLayoutMarkerValue+`"`) {
		t.Fatalf("third effective statement must be the marker check, got %q", lines[2])
	}
	if !strings.Contains(lines[3], "worker image lacks runner-shared layout support (want "+runnerLayoutMarkerValue+")") ||
		!strings.HasSuffix(lines[3], ">&2") || lines[4] != "exit 1" {
		t.Fatalf("marker refusal must print the message to stderr and exit 1, got %q / %q", lines[3], lines[4])
	}
	// Everything that mutates the volume comes after the gate's `fi`.
	body := strings.Join(lines[6:], "\n")
	for _, w := range []string{"mkdir", "chown", "chmod"} {
		if strings.Contains(strings.Join(lines[:6], "\n"), w) {
			t.Errorf("%s appears before the image gate completes", w)
		}
		if !strings.Contains(body, w) {
			t.Errorf("script no longer uses %s; update this test with the script", w)
		}
	}
	if lines[5] != "fi" {
		t.Errorf("expected the gate's closing fi at statement 6, got %q", lines[5])
	}
}

// runner-init is a regular init container between seed-nix and the sidecars, on the
// worker image, mounting the whole data volume at the seed path, with seed-nix's exact
// security context in both uid modes and no ephemeral request or limit.
func TestRunnerInitContainerShape(t *testing.T) {
	for _, p := range dindPostures() {
		for _, split := range []bool{false, true} {
			cfg := p.cfg
			cfg.UIDSplit = split
			spec := testSpec(t, "base", "m")
			pod := RenderDeployment(cfg, desiredDocker("abc"), spec).Spec.Template.Spec
			names := initNames(pod.InitContainers)
			idx := func(n string) int {
				for i, x := range names {
					if x == n {
						return i
					}
				}
				return -1
			}
			seed, ri, dind := idx(seedContainerName), idx(runnerInitContainerName), idx(dindContainerName)
			if seed < 0 || seed >= ri || ri >= dind {
				t.Fatalf("[%s split=%v] order = %v, want seed-nix < runner-init < dind", p.name, split, names)
			}
			if di := idx(dindInitContainerName); di >= 0 && di < ri {
				t.Errorf("[%s split=%v] dind-init precedes runner-init: %v", p.name, split, names)
			}
			c, s := pod.InitContainers[ri], pod.InitContainers[seed]
			if c.Image != spec.Image {
				t.Errorf("runner-init image = %q, want the worker image %q", c.Image, spec.Image)
			}
			if c.RestartPolicy != nil {
				t.Errorf("runner-init must be a regular init container")
			}
			if !reflect.DeepEqual(c.SecurityContext, s.SecurityContext) {
				t.Errorf("[%s split=%v] runner-init securityContext differs from seed-nix:\n%+v\n%+v", p.name, split, c.SecurityContext, s.SecurityContext)
			}
			if split && (c.SecurityContext.RunAsUser == nil || *c.SecurityContext.RunAsUser != 0) {
				t.Errorf("uid-split runner-init must run as root")
			}
			if !split && c.SecurityContext.RunAsUser != nil {
				t.Errorf("single-uid runner-init must inherit the pod uid, got %d", *c.SecurityContext.RunAsUser)
			}
			wantMounts := []corev1.VolumeMount{{Name: "data", MountPath: dataSeedMountPath}}
			if !reflect.DeepEqual(c.VolumeMounts, wantMounts) {
				t.Errorf("runner-init mounts = %+v, want exactly the whole data volume at %s", c.VolumeMounts, dataSeedMountPath)
			}
			if len(c.Command) != 3 || c.Command[2] != runnerInitScript() {
				t.Errorf("runner-init command is not the golden script: %v", c.Command)
			}
			for _, r := range []corev1.ResourceList{c.Resources.Requests, c.Resources.Limits} {
				if _, ok := r[corev1.ResourceEphemeralStorage]; ok {
					t.Errorf("runner-init declares ephemeral storage: %v", r)
				}
			}
			// The mount-root stays the whole data volume only for runner-init and seed-nix.
			for _, ic := range pod.InitContainers {
				if ic.Name == seedContainerName || ic.Name == runnerInitContainerName {
					continue
				}
				for _, vm := range ic.VolumeMounts {
					if vm.Name == "data" && vm.SubPath != dindWorkdirSubPath {
						t.Errorf("%q mounts data without the runner subPath: %+v", ic.Name, vm)
					}
				}
			}
		}
	}
}

// A plain worker is untouched by issue #3153: no runner-init, no subPath mount, no
// TMPDIR, no run-workdir volume anywhere in its rendered template.
func TestPlainWorkerHasNoRunnerSharedSurface(t *testing.T) {
	for _, split := range []bool{false, true} {
		cfg := dockerTestConfig()
		cfg.UIDSplit = split
		raw, err := json.Marshal(RenderDeployment(cfg, desired("abc"), testSpec(t, "base", "m")).Spec.Template)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{runnerInitContainerName, "runner-shared", "TMPDIR", "run-workdir", "subPath"} {
			if strings.Contains(string(raw), bad) {
				t.Errorf("plain worker template (uidSplit=%v) mentions %q", split, bad)
			}
		}
	}
}

// The single-uid ownership check must fail CLOSED: positive tests joined by && with the
// refusal in the else branch, so a shell lacking -O/-k (test errors) still refuses.
func TestRunnerInitScriptOwnershipCheckFailsClosed(t *testing.T) {
	got := runnerInitScript()
	if !strings.Contains(got, `if [ -O "$R" ] && [ -O "$T" ] && [ -k "$R" ]; then`+"\n    :\n  else\n    fail ") {
		t.Errorf("single-uid ownership check is not in the fail-closed positive form:\n%s", got)
	}
	if strings.Contains(got, `[ ! -O`) || strings.Contains(got, `[ ! -k`) {
		t.Errorf("negated ownership tests fail open on a shell lacking -O/-k")
	}
	if !strings.Contains(got, "chown -h 0:0 \"$R\"\n  chmod 3775 \"$R\"\n  chown -h worker:runner \"$R\"") {
		t.Errorf("uid-split must chmod 3775 before the final chown (no CAP_FSETID)")
	}
}
