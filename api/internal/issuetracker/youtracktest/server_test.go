package youtracktest

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// recorder is a testing.TB that records Errorf calls and holds cleanups so
// the test can run them by hand. Embedding the interface leaves every other
// method nil: calling one fails loudly.
type recorder struct {
	testing.TB
	errors   []string
	cleanups []func()
}

func (r *recorder) Helper()                   {}
func (r *recorder) Cleanup(f func())          { r.cleanups = append(r.cleanups, f) }
func (r *recorder) Errorf(f string, a ...any) { r.errors = append(r.errors, fmt.Sprintf(f, a...)) }

func TestUnmodelledRouteIsLoud(t *testing.T) {
	rec := &recorder{TB: t}
	srv := NewServer(rec)
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/nowhere", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+srv.Token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", resp.StatusCode)
	}
	for i := len(rec.cleanups) - 1; i >= 0; i-- {
		rec.cleanups[i]()
	}
	if len(rec.errors) != 1 {
		t.Fatalf("cleanup reported %d errors, want 1: %v", len(rec.errors), rec.errors)
	}
}

func TestModelledRoutesAreQuietAtCleanup(t *testing.T) {
	rec := &recorder{TB: t}
	srv := NewServer(rec)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/users/me?fields=id", nil)
	req.Header.Set("Authorization", "Bearer "+srv.Token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	for i := len(rec.cleanups) - 1; i >= 0; i-- {
		rec.cleanups[i]()
	}
	if len(rec.errors) != 0 {
		t.Fatalf("cleanup reported %v", rec.errors)
	}
}

func TestKnownPathWithAnUnmodelledMethodIsLoud(t *testing.T) {
	rec := &recorder{TB: t}
	srv := NewServer(rec)
	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/api/tags", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+srv.Token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 rather than a silent 405", resp.StatusCode)
	}
	for i := len(rec.cleanups) - 1; i >= 0; i-- {
		rec.cleanups[i]()
	}
	if len(rec.errors) != 1 {
		t.Fatalf("cleanup reported %d errors, want 1: %v", len(rec.errors), rec.errors)
	}
}

func TestWrongBearerTokenIsRefused(t *testing.T) {
	srv := NewServer(t)
	for name, auth := range map[string]string{
		"wrong token": "Bearer " + srv.Token + "x",
		"no scheme":   srv.Token,
		"absent":      "",
	} {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/users/me?fields=id", nil)
		if err != nil {
			t.Fatal(err)
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, resp.StatusCode)
		}
	}
}

// call sends one authorised request to the fake and returns the status and body.
func call(t *testing.T, srv *Server, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+srv.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func TestDefaultSeedHasAnEligibilityTagAndASameNamedTag(t *testing.T) {
	srv := NewServer(t)
	got := srv.TagsNamed(EligibilityTagName)
	if len(got) != 2 {
		t.Fatalf("tags named %q = %+v, want two", EligibilityTagName, got)
	}
	elig, same := srv.TagByID(EligibilityTagID), srv.TagByID(SameNameTagID)
	if elig.OwnerLogin != OtherLogin || len(elig.Use.Groups) != 1 || elig.Use.Groups[0] != AllUsersGroupID {
		t.Errorf("eligibility tag = %+v, want owned by the other user and shared with All Users", elig)
	}
	if same.OwnerLogin != BotLogin || same.Name != EligibilityTagName || len(same.Use.Groups)+len(same.Read.Groups) != 0 {
		t.Errorf("same-named tag = %+v, want owned by the bot and private", same)
	}
}

func TestCreateTagIsOwnedByTheBotAndConflictsOnlyWithItsOwn(t *testing.T) {
	srv := NewServer(t)
	srv.SeedTag("theirs", OtherLogin, Sharing{}, Sharing{})
	body := `{"name":"theirs","readSharingSettings":{"permittedGroups":[{"id":"` + AllUsersGroupID + `"}],"permittedUsers":[{"id":"` + OtherUserID + `"}]},"tagSharingSettings":{"permittedGroups":[],"permittedUsers":[{"id":"` + OtherUserID + `"}]}}`
	status, _ := call(t, srv, http.MethodPost, "/api/tags?fields=id", body)
	if status != http.StatusOK {
		t.Fatalf("create = %d, want 200: another user's tag of the name must not block it", status)
	}
	made := srv.TagsNamed("theirs")
	if len(made) != 2 || made[1].OwnerLogin != BotLogin {
		t.Fatalf("tags = %+v", made)
	}
	if made[1].Read.Groups[0] != AllUsersGroupID || made[1].Read.Users[0] != OtherUserID || len(made[1].Use.Groups) != 0 || made[1].Use.Users[0] != OtherUserID {
		t.Errorf("stored sharing = read %+v use %+v", made[1].Read, made[1].Use)
	}
	if status, _ := call(t, srv, http.MethodPost, "/api/tags?fields=id", `{"name":"theirs"}`); status != http.StatusConflict {
		t.Errorf("duplicate of the bot's own tag = %d, want 409", status)
	}
	if status, _ := call(t, srv, http.MethodPost, "/api/tags?fields=id", `{"name":"x","tagSharingSettings":{"permittedGroups":[{"id":"675-99"}]}}`); status != http.StatusBadRequest {
		t.Errorf("unknown group = %d, want 400", status)
	}
}

func TestGetTagAnswers404ForATagTheBotCannotSee(t *testing.T) {
	srv := NewServer(t)
	hidden := srv.SeedTag("hidden", OtherLogin, Sharing{}, Sharing{})
	if status, _ := call(t, srv, http.MethodGet, "/api/tags/"+hidden+"?fields=id", ""); status != http.StatusNotFound {
		t.Errorf("hidden tag = %d, want 404", status)
	}
	status, body := call(t, srv, http.MethodGet, "/api/tags/"+EligibilityTagID+"?fields=id,owner(login)", "")
	if status != http.StatusOK || !strings.Contains(body, OtherLogin) {
		t.Errorf("eligibility tag = %d %s", status, body)
	}
	if status, body := call(t, srv, http.MethodGet, "/api/tags?fields=id", ""); status != http.StatusOK || strings.Contains(body, hidden) {
		t.Errorf("tag list = %d %s, want it to omit the hidden tag", status, body)
	}
}

func TestAttachRefusesATagTheBotCannotUse(t *testing.T) {
	srv := NewServer(t)
	readOnly := srv.SeedTag("read-only", OtherLogin, Sharing{Groups: []string{AllUsersGroupID}}, Sharing{})
	viaTeam := srv.SeedTag("via-team", OtherLogin, Sharing{Groups: []string{AllUsersGroupID}}, Sharing{Groups: []string{TeamGroupID}})
	solo := srv.SeedTag("solo", OtherLogin, Sharing{Groups: []string{AllUsersGroupID}}, Sharing{Groups: []string{SoloGroupID}})
	n := srv.SeedIssue("t", "")
	attach := func(id string) int {
		status, _ := call(t, srv, http.MethodPost, "/api/issues/UZI-"+strconv.FormatInt(n, 10)+"/tags?fields=id", `{"id":"`+id+`"}`)
		return status
	}
	for name, tc := range map[string]struct {
		id   string
		want int
	}{
		"read only":        {readOnly, http.StatusForbidden},
		"group without me": {solo, http.StatusForbidden},
		"group with me":    {viaTeam, http.StatusOK},
		"all users":        {EligibilityTagID, http.StatusOK},
		"own tag":          {SameNameTagID, http.StatusOK},
	} {
		if got := attach(tc.id); got != tc.want {
			t.Errorf("%s: attach = %d, want %d", name, got, tc.want)
		}
	}
}
