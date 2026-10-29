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

// Complete report evidence is independent of installed native HTTP backends.
func TestPrereqOutcomeEvidence(t *testing.T) {
	for _, kind := range []string{"ok", "ng-absent", "ng-same", "ng-foreign", "lost-same", "ok-absent", "readback-overflow", "receive-overflow"} {
		t.Run(kind, func(t *testing.T) {
			trustLoopbackTLS(t)
			ref := "refs/uzi-prereq/" + heldRunID.String()
			tip := strings.Repeat("a", 40)
			posts := 0
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				enc := pktline.NewEncoder(w)
				if r.Method == http.MethodPost {
					posts++
					w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
					if kind == "lost-same" {
						return
					}
					_ = enc.EncodeString("unpack ok\n")
					if strings.HasPrefix(kind, "ng-") {
						_ = enc.EncodeString("ng " + ref + " refused\x1b[2J\u202e\n")
					} else {
						_ = enc.EncodeString("ok " + ref + "\n")
					}
					_ = enc.Flush()
					return
				}
				svc := r.URL.Query().Get("service")
				w.Header().Set("Content-Type", "application/x-"+svc+"-advertisement")
				if kind == "receive-overflow" && svc == "git-receive-pack" || kind == "readback-overflow" && posts > 0 {
					_, _ = w.Write([]byte(strings.Repeat("x", pushbroker.MaxRefAdvertisementBytes+1)))
					return
				}
				_ = enc.EncodeString("# service=" + svc + "\n")
				_ = enc.Flush()
				_ = enc.EncodeString(tip + " refs/heads/main\x00report-status delete-refs\n")
				if posts > 0 && kind != "ng-absent" && kind != "ok-absent" {
					current := tip
					if kind == "ng-foreign" {
						current = strings.Repeat("b", 40)
					}
					_ = enc.EncodeString(current + " " + ref + "\n")
				}
				_ = enc.Flush()
			}))
			defer srv.Close()
			p, err := pushbroker.PreparePrereqCreate(context.Background(), prereqOpts(srv.URL+"/repo"))
			if err != nil {
				t.Fatal(err)
			}
			out, err := pushbroker.SendPrereqCreate(context.Background(), p)
			switch kind {
			case "ok", "ng-same", "lost-same":
				if out != pushbroker.PrereqCreateCreated || err != nil {
					t.Fatalf("%v %v", out, err)
				}
			case "ng-absent":
				if out != pushbroker.PrereqCreateRefused || !errors.Is(err, pushbroker.ErrPrereqCreateRefused) || err.Error() != pushbroker.ErrPrereqCreateRefused.Error() {
					t.Fatalf("%v %v", out, err)
				}
			case "ng-foreign":
				if out != pushbroker.PrereqCreateForeignTip {
					t.Fatalf("%v %v", out, err)
				}
			default:
				if out != pushbroker.PrereqCreateUnknown || err == nil {
					t.Fatalf("%v %v", out, err)
				}
			}
			if kind == "receive-overflow" && posts != 0 {
				t.Fatal("sent after overflow")
			}
			if strings.Contains(kind, "overflow") && !errors.Is(err, pushbroker.ErrRefAdvertisementTooLarge) {
				t.Fatalf("lost bound cause: %v", err)
			}
		})
	}
}

func TestPrereqDeleteWireCAS(t *testing.T) {
	f, remote, url, base, tip, _ := heldHTTPFixture(t)
	ref := "refs/uzi-prereq/" + heldRunID.String()
	f.git("push", "origin", base+":"+ref)
	f.git("push", "origin", tip+":refs/uzi-checkpoints/racer")
	moved := false
	proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("service") == "git-receive-pack" && !moved {
			// Move after upload discovery and before receive discovery: the command must
			// still use persisted base, never the newer advertised tip.
			run(t, "", "git", "-C", f.bare, "update-ref", ref, tip, base)
			moved = true
		}
		remote.srv.Config.Handler.ServeHTTP(w, r)
	}))
	defer proxy.Close()
	err := pushbroker.Delete(context.Background(), pushbroker.DeleteOptions{CloneURL: proxy.URL + strings.TrimPrefix(url, remote.srv.URL), Ref: ref, ExpectedOldTip: base})
	if err != nil {
		t.Fatal(err)
	}
	if !moved || f.originRef(ref) != tip {
		t.Fatal("CAS deleted foreign tip")
	}
}
