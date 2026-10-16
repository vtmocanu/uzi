package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

type dindMTUInterface struct {
	name  string
	value string
}

// dindMTUFixture deliberately includes shell metacharacters in the path.
func dindMTUFixture(t *testing.T, interfaces []dindMTUInterface) string {
	t.Helper()
	netDir := filepath.Join(t.TempDir(), "net ' $fixture; [interfaces]")
	if err := os.Mkdir(netDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, iface := range interfaces {
		dir := filepath.Join(netDir, iface.name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "mtu"), []byte(iface.value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return netDir
}

type dindLaunch struct {
	argv       []string
	configPath string
	config     map[string]any
}

type dindLaunchOptions struct {
	expectDiscoveryFailure bool
	configFailure          string
}

// runRenderedDind executes the actual rendered Command and Args, replacing only
// the sysfs positional argument. Discovery failures must launch the original daemon argv without config artifacts.
func runRenderedDind(t *testing.T, dind corev1.Container, netDir string, options ...dindLaunchOptions) (dindLaunch, string, error) {
	t.Helper()
	if len(dind.Command) != 5 || dind.Command[0] != "/bin/sh" || dind.Command[1] != "-c" ||
		dind.Command[3] != "dind-mtu" || dind.Command[4] != "/sys/class/net" {
		t.Fatalf("unexpected production command: %q", dind.Command)
	}
	binDir := t.TempDir()
	capture := filepath.Join(binDir, "argv")
	configPath := filepath.Join(binDir, "daemon ' $config.json")
	configCapture := filepath.Join(binDir, "config-capture")
	fake := `#!/bin/sh
printf '%s\0' "$@" > "$DIND_ARGV"
for arg do
    case "$arg" in
        --config-file=*) cat "${arg#--config-file=}" > "$DIND_CONFIG_CAPTURE" || exit 1 ;;
    esac
done
`
	mktemp := `#!/bin/sh
[ "$#" -eq 1 ] && [ "$1" = /tmp/uzi-dind-daemon.XXXXXX ] || exit 2
(set -C; : > "$DIND_CONFIG_PATH") || exit 1
printf '%s\n' "$DIND_CONFIG_PATH"
`
	var opts dindLaunchOptions
	if len(options) > 1 {
		t.Fatal("at most one launch options value is allowed")
	}
	if len(options) == 1 {
		opts = options[0]
	}
	if opts.expectDiscoveryFailure && opts.configFailure != "" {
		t.Fatal("discovery and config failure expectations are mutually exclusive")
	}
	if opts.configFailure != "" {
		switch opts.configFailure {
		case "mktemp":
			mktemp = "#!/bin/sh\nexit 1\n"
		case "write":
			mktemp = "#!/bin/sh\nprintf '%s\\n' \"$DIND_CONFIG_PATH\"\n"
			if err := os.Mkdir(configPath, 0o700); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unknown fixture failure: %q", opts.configFailure)
		}
	}
	if err := os.WriteFile(filepath.Join(binDir, "mktemp"), []byte(mktemp), 0o700); err != nil { //nolint:gosec // G306: executable mock under t.TempDir.
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "dockerd"), []byte(fake), 0o700); err != nil { //nolint:gosec // G306: executable fake daemon under t.TempDir.
		t.Fatal(err)
	}
	command := append([]string(nil), dind.Command...)
	command[4] = netDir
	args := append(command[1:], dind.Args...)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, command[0], args...) //nolint:gosec // G204: executes the rendered shell with test-controlled fixture paths and a fake daemon.
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"), "DIND_ARGV="+capture,
		"DIND_CONFIG_PATH="+configPath, "DIND_CONFIG_CAPTURE="+configCapture)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if opts.expectDiscoveryFailure {
		for _, path := range []string{configPath, configCapture} {
			if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
				t.Errorf("discovery fallback must not create config artifacts %q: %v", path, statErr)
			}
		}
	}
	data, readErr := os.ReadFile(capture) //nolint:gosec // G304: reads the fake daemon capture under t.TempDir.
	if os.IsNotExist(readErr) {
		return dindLaunch{}, stderr.String(), err
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(data) == 0 || data[len(data)-1] != 0 {
		t.Fatalf("invalid fake daemon argv capture: %q", data)
	}
	argv := strings.Split(string(data[:len(data)-1]), "\x00")
	if opts.expectDiscoveryFailure {
		return dindLaunch{argv: argv}, stderr.String(), err
	}
	configData, readErr := os.ReadFile(configCapture) //nolint:gosec // G304: fake daemon capture under t.TempDir.
	if readErr != nil {
		t.Fatal(readErr)
	}
	var config map[string]any
	if decodeErr := json.Unmarshal(configData, &config); decodeErr != nil {
		t.Fatalf("invalid daemon config %q: %v", configData, decodeErr)
	}
	info, statErr := os.Stat(configPath)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("daemon config permissions = %o, want 600", info.Mode().Perm())
	}
	return dindLaunch{
		argv:       argv,
		configPath: configPath,
		config:     config,
	}, stderr.String(), err
}

func renderedRootfulDind(t *testing.T) corev1.Container {
	t.Helper()
	dep := RenderDeployment(dockerTestConfigNonRootless(), desiredDocker("abc"), testSpec(t, "base", "m"))
	return containerByName(t, dep.Spec.Template.Spec.InitContainers, dindContainerName)
}

func TestNonRootlessDindMTUProductionPath(t *testing.T) {
	dind := renderedRootfulDind(t)
	if len(dind.Command) != 5 || dind.Command[4] != "/sys/class/net" {
		t.Fatalf("production sysfs path must be fixed: %q", dind.Command)
	}
	if len(dind.Args) != 0 {
		t.Fatalf("unexpected production Args: %q", dind.Args)
	}
}

func TestNonRootlessDindMTU(t *testing.T) {
	for _, tt := range []struct {
		name       string
		interfaces []dindMTUInterface
		want       string
	}{
		{"1400", []dindMTUInterface{{"ens5", "1400\n"}}, "1400"},
		{"control-file", []dindMTUInterface{{"ens5", "1400\n"}}, "1400"},
		{"symlink-directory", []dindMTUInterface{{"ens5", "1500\n"}}, "1400"},
		{"1500", []dindMTUInterface{{"ens5", "1500\n"}}, "1500"},
		{"jumbo", []dindMTUInterface{{"ens5", "9000\n"}}, "1500"},
		{"minimum-first", []dindMTUInterface{{"a", "1400\n"}, {"z", "9000\n"}}, "1400"},
		{"minimum-last", []dindMTUInterface{{"a", "9000\n"}, {"z", "1400\n"}}, "1400"},
		{"dot-minimum", []dindMTUInterface{{"ens5", "1500\n"}, {".low", "1400\n"}}, "1400"},
		{"double-dot-minimum", []dindMTUInterface{{"ens5", "1500\n"}, {"..low", "1400\n"}}, "1400"},
		{"bounds-minimum-first", []dindMTUInterface{{"a", "1\n"}, {"z", "65535\n"}}, "1"},
		{"bounds-minimum-last", []dindMTUInterface{{"a", "65535\n"}, {"z", "1\n"}}, "1"},
		{"loopback-excluded", []dindMTUInterface{{"lo", "invalid\n"}, {"net1", "1500\n"}}, "1500"},
		{"minimum-bound", []dindMTUInterface{{"veth0", "1\n"}}, "1"},
		{"maximum-bound", []dindMTUInterface{{"veth0", "65535\n"}}, "1500"},
		{"without-newline", []dindMTUInterface{{"ens5", "1400"}}, "1400"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			netDir := dindMTUFixture(t, tt.interfaces)
			switch tt.name {
			case "control-file":
				if err := os.WriteFile(filepath.Join(netDir, "bonding_masters"), []byte("bond0\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink-directory":
				deviceDir := dindMTUFixture(t, []dindMTUInterface{{"device", "1400\n"}})
				if err := os.Symlink(filepath.Join(deviceDir, "device"), filepath.Join(netDir, "net1")); err != nil {
					t.Fatal(err)
				}
			}
			launch, stderr, err := runRenderedDind(t, renderedRootfulDind(t), netDir)
			if err != nil || stderr != "" {
				t.Fatalf("launch: %v; stderr: %s", err, stderr)
			}
			want := []string{
				"--host=tcp://127.0.0.1:2375", "--tls=false", "--mtu=" + tt.want,
				"--config-file=" + launch.configPath,
			}
			if !reflect.DeepEqual(launch.argv, want) {
				t.Fatalf("dockerd argv = %q, want %q", launch.argv, want)
			}
			wantConfig := map[string]any{
				"default-network-opts": map[string]any{
					"bridge": map[string]any{"com.docker.network.driver.mtu": tt.want},
				},
			}
			if !reflect.DeepEqual(launch.config, wantConfig) {
				t.Fatalf("daemon config = %#v, want %#v", launch.config, wantConfig)
			}
		})
	}
}

func TestNonRootlessDindMTUConfigFailClosed(t *testing.T) {
	for _, tt := range []struct {
		failure string
		want    string
	}{
		{"mktemp", "cannot create daemon config in /tmp"},
		{"write", "cannot write daemon config:"},
	} {
		t.Run(tt.failure, func(t *testing.T) {
			netDir := dindMTUFixture(t, []dindMTUInterface{{"ens5", "1400\n"}})
			launch, stderr, err := runRenderedDind(t, renderedRootfulDind(t), netDir, dindLaunchOptions{configFailure: tt.failure})
			if err == nil {
				t.Fatalf("config failure must stop startup; stderr=%q", stderr)
			}
			if launch.argv != nil {
				t.Fatalf("daemon launched on config failure: %q", launch.argv)
			}
			if !strings.Contains(stderr, "dind MTU: "+tt.want) {
				t.Fatalf("missing config failure diagnostic: %q", stderr)
			}
			if tt.failure == "write" && !strings.Contains(stderr, "daemon ' $config.json") {
				t.Fatalf("config write diagnostic must identify the path: %q", stderr)
			}
		})
	}
}

func TestNonRootlessDindMTUDiscoveryFallback(t *testing.T) {
	for _, tt := range []struct {
		name  string
		value string
	}{
		{"empty", ""},
		{"newline-only", "\n"},
		{"malformed", "abc\n"},
		{"negative", "-1400\n"},
		{"plus-sign", "+1400\n"},
		{"whitespace", " 1400\n"},
		{"trailing-whitespace", "1400 \n"},
		{"multiline", "1400\n1500\n"},
		{"extra-newline", "1400\n\n"},
		{"zero", "0\n"},
		{"leading-zero", "01400\n"},
		{"multiple-zeroes", "00\n"},
		{"above-bound", "65536\n"},
		{"oversized", "999999999999999999999999999999999999999\n"},
		{"arithmetic-expression", "1+1399\n"},
		{"missing-mtu", ""},
		{"unreadable-mtu-directory", ""},
		{"no-interfaces", ""},
		{"only-control-file", ""},
		{"only-loopback", ""},
		{"missing-net-directory", ""},
		{"invalid-before-valid", "abc\n"},
		{"dot-invalid", "abc\n"},
		{"double-dot-invalid", "abc\n"},
		{"dot-dangling-symlink", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var interfaces []dindMTUInterface
			switch tt.name {
			case "no-interfaces", "only-control-file", "missing-net-directory":
			case "invalid-before-valid":
				interfaces = []dindMTUInterface{{"a-broken", tt.value}, {"z-valid", "1400\n"}}
			case "dot-invalid":
				interfaces = []dindMTUInterface{{"ens5", "1500\n"}, {".low", tt.value}}
			case "double-dot-invalid":
				interfaces = []dindMTUInterface{{"ens5", "1500\n"}, {"..low", tt.value}}
			case "dot-dangling-symlink":
				interfaces = []dindMTUInterface{{"ens5", "1500\n"}}
			case "only-loopback":
				interfaces = []dindMTUInterface{{"lo", "65536\n"}}
			default:
				// A valid sibling must never mask a broken interface.
				interfaces = []dindMTUInterface{{"a-valid", "1400\n"}, {"z-broken", tt.value}}
			}
			netDir := dindMTUFixture(t, interfaces)
			mtuPath := filepath.Join(netDir, "z-broken", "mtu")
			switch tt.name {
			case "dot-invalid", "dot-dangling-symlink":
				mtuPath = filepath.Join(netDir, ".low", "mtu")
				if tt.name == "dot-dangling-symlink" {
					if err := os.Symlink("absent", filepath.Join(netDir, ".low")); err != nil {
						t.Fatal(err)
					}
				}
			case "double-dot-invalid":
				mtuPath = filepath.Join(netDir, "..low", "mtu")
			case "missing-mtu", "unreadable-mtu-directory":
				if err := os.Remove(mtuPath); err != nil {
					t.Fatal(err)
				}
				if tt.name == "unreadable-mtu-directory" {
					// Deterministically unreadable as a value even when tests run as root.
					if err := os.Mkdir(mtuPath, 0o700); err != nil {
						t.Fatal(err)
					}
				}
			case "only-control-file":
				if err := os.WriteFile(filepath.Join(netDir, "bonding_masters"), []byte("bond0\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing-net-directory":
				netDir = filepath.Join(netDir, "absent")
			}
			launch, stderr, err := runRenderedDind(t, renderedRootfulDind(t), netDir, dindLaunchOptions{expectDiscoveryFailure: true})
			if err != nil {
				t.Fatalf("discovery failure must preserve startup: %v; stderr=%q", err, stderr)
			}
			wantArgs := []string{"--host=tcp://127.0.0.1:2375", "--tls=false"}
			if !reflect.DeepEqual(launch.argv, wantArgs) || launch.config != nil || launch.configPath != "" {
				t.Fatalf("fallback launch = %#v, want exact original argv %q and no config", launch, wantArgs)
			}
			reason := "invalid MTU (expected decimal 1..65535): " + mtuPath
			switch tt.name {
			case "invalid-before-valid":
				reason = "invalid MTU (expected decimal 1..65535): " + filepath.Join(netDir, "a-broken", "mtu")
			case "above-bound", "oversized":
				reason = "MTU exceeds 65535: " + mtuPath
			case "missing-mtu", "unreadable-mtu-directory", "dot-dangling-symlink":
				reason = "cannot read MTU: " + mtuPath
			case "no-interfaces", "only-control-file", "only-loopback":
				reason = "no non-loopback interface MTU in " + netDir
			case "missing-net-directory":
				reason = "network interface directory unavailable: " + netDir
			}
			wantDiagnostic := "dind MTU warning: " + reason + "; starting dockerd with unchanged defaults\n"
			if stderr != wantDiagnostic {
				t.Fatalf("discovery diagnostic = %q, want %q", stderr, wantDiagnostic)
			}
		})
	}
}
