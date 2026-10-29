package pushbroker_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

func TestPrereqTransportBoundaries(t *testing.T) {
	for _, failure := range []string{"overflow", "redirect"} {
		for _, stage := range []string{"prepare", "send", "readback", "delete-list", "delete-receive", "delete-readback"} {
			t.Run(failure+"/"+stage, func(t *testing.T) {
				trustLoopbackTLS(t)
				bad := newForbiddenRemote(t, true)
				ref := "refs/uzi-prereq/" + heldRunID.String()
				base := strings.Repeat("a", 40)
				uploads, receives, posts := 0, 0, 0
				deleting := strings.HasPrefix(stage, "delete")
				active := false
				srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					svc := r.URL.Query().Get("service")
					enc := pktline.NewEncoder(w)
					if r.Method == http.MethodPost {
						posts++
						w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
						_ = enc.EncodeString("unpack ok\n")
						_ = enc.EncodeString("ok " + ref + "\n")
						_ = enc.Flush()
						return
					}
					if svc == "git-upload-pack" {
						uploads++
					}
					if svc == "git-receive-pack" {
						receives++
					}
					hit := active && (stage == "prepare" && uploads == 1 ||
						stage == "send" && receives == 1 ||
						stage == "readback" && uploads == 1 ||
						stage == "delete-list" && uploads == 1 ||
						stage == "delete-receive" && receives == 1 ||
						stage == "delete-readback" && uploads == 2)
					if hit {
						if failure == "redirect" {
							http.Redirect(w, r, bad.srv.URL+r.URL.Path, http.StatusFound) // #nosec G710 -- Intentional off-origin redirect to a fixture-owned TLS server; the test requires zero target hits.
							return
						}
						w.Header().Set("Content-Type", "application/x-"+svc+"-advertisement")
						_, _ = w.Write([]byte(strings.Repeat("x", pushbroker.MaxRefAdvertisementBytes+1)))
						return
					}
					w.Header().Set("Content-Type", "application/x-"+svc+"-advertisement")
					_ = enc.EncodeString("# service=" + svc + "\n")
					_ = enc.Flush()
					_ = enc.EncodeString(base + " refs/heads/main\x00report-status delete-refs\n")
					if deleting && posts == 0 || !deleting && posts > 0 {
						_ = enc.EncodeString(base + " " + ref + "\n")
					}
					_ = enc.Flush()
				}))
				defer srv.Close()
				o := prereqOpts(srv.URL + "/repo")
				var err error
				if stage == "prepare" {
					active = true
					_, err = pushbroker.PreparePrereqCreate(context.Background(), o)
				} else if deleting {
					active = true
					err = pushbroker.Delete(context.Background(), pushbroker.DeleteOptions{CloneURL: o.CloneURL, Ref: ref, ExpectedOldTip: base})
				} else {
					p, prepErr := pushbroker.PreparePrereqCreate(context.Background(), o)
					if prepErr != nil {
						t.Fatal(prepErr)
					}
					uploads = 0
					active = true
					_, err = pushbroker.SendPrereqCreate(context.Background(), p)
				}
				if err == nil {
					t.Fatal("boundary failure was accepted")
				}
				if failure == "overflow" && !errors.Is(err, pushbroker.ErrRefAdvertisementTooLarge) {
					t.Fatalf("lost byte-bound cause: %v", err)
				}
				if bad.hits.Load() != 0 {
					t.Fatal("credential boundary crossed")
				}
				if stage != "readback" && stage != "delete-readback" && posts != 0 {
					t.Fatal("command sent despite failed discovery")
				}
			})
		}
	}
}

func TestPrereqPreexistingNeverAdopted(t *testing.T) {
	f := newGitFixture(t)
	base := f.commit("base.txt", "base\n", "base")
	f.pushMain()
	ref := "refs/uzi-prereq/" + heldRunID.String()
	f.git("push", "origin", base+":"+ref)
	_, err := pushbroker.PreparePrereqCreate(context.Background(), prereqOpts(f.cloneURL()))
	if !errors.Is(err, pushbroker.ErrRefExists) {
		t.Fatalf("preexisting same-tip: %v", err)
	}
}
