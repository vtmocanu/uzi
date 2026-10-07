package pushbroker_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/file"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

// A test-owned executable speaks the real file client's advertisement/request/
// response protocol. The production registry's file transport remains untouched.
func TestPublishFileRawDisposition(t *testing.T) {
	const ref = "refs/uzi-checkpoints/main"
	for _, tc := range []struct {
		name   string
		lines  []string
		flush  bool
		want   pushbroker.PublishDisposition
		mapped error
	}{
		{"acknowledged", []string{"unpack ok", "ok " + ref}, true, pushbroker.PublishAdvanced, nil},
		{"rejection reason exactly ok", []string{"unpack ok", "ng " + ref + " ok"}, true, pushbroker.PublishUnclassified, nil},
		{"workflow rejection", []string{"unpack ok", "ng " + ref + " missing workflow scope"}, true, pushbroker.PublishUnclassified, pushbroker.ErrWorkflowScopeRejected},
		{"unpack rejection", []string{"unpack invalid fixture"}, true, pushbroker.PublishUnclassified, nil},
		{"partial acknowledgement", []string{"unpack ok", "ok " + ref}, false, pushbroker.PublishOutcomeUnknown, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGitFixture(t)
			tip := f.commit("tip.txt", "tip\n", "tip")
			pack := []byte(f.git("pack-objects", "--all", "--stdout"))
			dir := t.TempDir()
			var advertisement, response bytes.Buffer
			ar := packp.NewAdvRefs()
			if err := ar.Capabilities.Set(capability.ReportStatus); err != nil {
				t.Fatal(err)
			}
			if err := ar.Encode(&advertisement); err != nil {
				t.Fatal(err)
			}
			enc := pktline.NewEncoder(&response)
			for _, line := range tc.lines {
				if err := enc.EncodeString(line + "\n"); err != nil {
					t.Fatal(err)
				}
			}
			if tc.flush {
				if err := enc.Flush(); err != nil {
					t.Fatal(err)
				}
			}
			for name, data := range map[string][]byte{"advertisement": advertisement.Bytes(), "response": response.Bytes()} {
				if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			bin := filepath.Join(dir, "receive-pack")
			// All commands are literal; paths come from the executable's own directory.
			script := "#!/bin/sh\nfixture_dir=${0%/*}\ncat \"$fixture_dir/advertisement\"\ncat > /dev/null\ncat \"$fixture_dir/response\"\n"
			if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // G306: the test-owned receive-pack executable in t.TempDir must be runnable.
				t.Fatal(err)
			}
			const protocol = "rawfilefixture"
			previous := client.Protocols[protocol]
			client.InstallProtocol(protocol, file.NewClient("git-upload-pack", bin))
			defer func() {
				if previous == nil {
					delete(client.Protocols, protocol)
				} else {
					client.InstallProtocol(protocol, previous)
				}
			}()
			res, err := pushbroker.Publish(context.Background(), pushbroker.Options{
				CloneURL: protocol + "://" + f.bare, Branch: "main", DeclaredTip: tip, Pack: pack,
			})
			if res.Disposition != tc.want {
				t.Errorf("Disposition = %v, want %v (error %v)", res.Disposition, tc.want, err)
			}
			if (err != nil) != (tc.want != pushbroker.PublishAdvanced) {
				t.Errorf("error = %v", err)
			}
			if tc.mapped != nil && !errors.Is(err, tc.mapped) {
				t.Errorf("error = %v, want %v", err, tc.mapped)
			}
		})
	}
}
