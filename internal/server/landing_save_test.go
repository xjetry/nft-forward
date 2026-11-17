package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"nft-forward/internal/db"
)

// A loopback subscription URL is refused by the fetcher's SSRF guard before
// anything is dialled, so it fails the same way on any machine.
const unfetchableSub = "http://127.0.0.1:1/sub"

const manualLandingURI = "vless://11111111-1111-1111-1111-111111111111@198.51.100.10:443?security=tls#HK"

// Saving a landing source whose subscription cannot be fetched still saves
// it, but says so: the reply carries the reason, the preview holds the manual
// nodes only, and the ledger keeps what it had.
func TestSaveLandingSourceReportsAnUnfetchableSubscription(t *testing.T) {
	d := openDB(t)
	admin := loginAsAdmin(t, d)
	uid, _ := loginAsUser(t, d, 10)
	s, _ := New(d)
	path := fmt.Sprintf("/api/users/%d/landing", uid)
	type reply struct {
		SubError string `json:"sub_error"`
		Nodes    []struct {
			Host string `json:"host"`
		} `json:"landing_nodes"`
	}
	save := func(body map[string]any) reply {
		t.Helper()
		rec := cookieDo(t, s, admin, "POST", path, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("save %v: %d %s", body, rec.Code, rec.Body)
		}
		var r reply
		if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	presentExits := func() int {
		t.Helper()
		exits, err := db.PresentLandingExitsForUser(d, uid)
		if err != nil {
			t.Fatal(err)
		}
		return len(exits)
	}

	// Manual URIs only: nothing to fetch, nothing to report.
	r := save(map[string]any{"landing_uris": manualLandingURI})
	if r.SubError != "" || len(r.Nodes) != 1 {
		t.Fatalf("manual-only save: sub_error=%q nodes=%d, want none and 1", r.SubError, len(r.Nodes))
	}
	if n := presentExits(); n != 1 {
		t.Fatalf("manual-only save materialized %d exits, want 1", n)
	}

	// Adding a subscription that cannot be fetched.
	r = save(map[string]any{"landing_sub_url": unfetchableSub, "landing_uris": manualLandingURI})
	if !strings.Contains(r.SubError, "127.0.0.1") {
		t.Fatalf("unfetchable subscription: sub_error=%q, want the fetch failure", r.SubError)
	}
	if len(r.Nodes) != 1 || r.Nodes[0].Host != "198.51.100.10" {
		t.Fatalf("unfetchable subscription: preview=%+v, want the manual node only", r.Nodes)
	}
	u, err := db.GetUserByID(d, uid)
	if err != nil {
		t.Fatal(err)
	}
	if u.LandingSubURL != unfetchableSub {
		t.Fatalf("the source was not stored: landing_sub_url=%q", u.LandingSubURL)
	}
	if n := presentExits(); n != 1 {
		t.Fatalf("the ledger changed to %d exits although the subscription was not fetched", n)
	}

	// Removing the subscription again clears the warning.
	if r := save(map[string]any{"landing_uris": manualLandingURI}); r.SubError != "" {
		t.Fatalf("after removing the subscription: sub_error=%q", r.SubError)
	}

	// A user that does not exist is "not found", not "saved, but ...".
	if rec := cookieDo(t, s, admin, "POST", "/api/users/99999/landing", map[string]any{"landing_uris": manualLandingURI}); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown user: want 404, got %d %s", rec.Code, rec.Body)
	}
}

// The v1 endpoint sets the subscription only: it reports an unfetchable one
// the same way and leaves the manual URIs as they are.
func TestV1SetLandingSubURLReportsAnUnfetchableSubscription(t *testing.T) {
	d := openDB(t)
	_, adminTok := v1AdminToken(t, d, db.TokenScopeReadWrite)
	uid, _ := loginAsUser(t, d, 10)
	if err := db.SetUserLandingSource(d, uid, "", manualLandingURI); err != nil {
		t.Fatal(err)
	}
	s, _ := New(d)
	path := fmt.Sprintf("/api/v1/users/%d/landing-sub-url", uid)
	set := func(url string) string {
		t.Helper()
		rec := v1Do(t, s, "PUT", path, adminTok, map[string]any{"landing_sub_url": url})
		if rec.Code != http.StatusOK {
			t.Fatalf("set %q: %d %s", url, rec.Code, rec.Body)
		}
		data := v1Data(t, rec)
		if data["updated"] != true {
			t.Fatalf("set %q: updated=%v", url, data["updated"])
		}
		subError, ok := data["sub_error"].(string)
		if !ok {
			t.Fatalf("set %q: reply has no sub_error string: %s", url, rec.Body)
		}
		return subError
	}
	if got := set(unfetchableSub); !strings.Contains(got, "127.0.0.1") {
		t.Fatalf("unfetchable subscription: sub_error=%q, want the fetch failure", got)
	}
	u, err := db.GetUserByID(d, uid)
	if err != nil {
		t.Fatal(err)
	}
	if u.LandingSubURL != unfetchableSub || u.LandingURIs != manualLandingURI {
		t.Fatalf("stored source = (%q, %q), want the new subscription and the untouched manual URIs", u.LandingSubURL, u.LandingURIs)
	}
	if got := set(""); got != "" {
		t.Fatalf("after clearing the subscription: sub_error=%q", got)
	}
}
