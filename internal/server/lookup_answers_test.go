package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"nft-forward/internal/db"
)

// A lookup has three outcomes — the row is there, there is no such row, the
// lookup could not be made — and a handler that folds two of them together
// answers a question nobody asked. The statement sweep
// (TestEveryStatementFailureIsReportedAsAFailure) pins one fold: a lookup
// that could not be made is not "not found". The tests here pin the folds on
// the other side of the same predicate (lookupErr, and the errors.Is checks
// against sql.ErrNoRows that stand next to it): a row that is not there is
// neither an internal error nor quietly treated as there, and "no such row"
// is still what frees a name.

// noSuchID is an id no seeded row has.
const noSuchID = 987654

// goneIsDone lists the routes that answer success for a row that is not
// there, because what they are asked for is that the row be gone. Asking
// again after it went — a retry, a second admin — is then not an error.
var goneIsDone = map[string]string{
	"DELETE /api/rules/{id}":                    "deletes the rule",
	"DELETE /api/users/{id}/grants/{nodeID}":    "revokes the grant",
	"POST /api/users/{id}/grants/batch-revoke":  "revokes the grants",
	"DELETE /api/v1/users/{id}/grants/{nodeId}": "revokes the grant",
	"DELETE /api/v1/users/{id}/token":           "deletes the user's token",
}

// TestMissingRowIsAnsweredAsMissing asks every route that names rows in its
// path for rows that do not exist: all of them at once, and each one alone
// with the others real. The caller's request is what is off, so:
//
//   - the answer is never a 5xx. One would mean a lookup reports a missing
//     row with something other than sql.ErrNoRows, a handler went on to use
//     the row it did not get, or a write was left to trip over a foreign key;
//   - the answer is never success either, outside goneIsDone. An UPDATE that
//     matches no row changes nothing and fails nothing, and a list of what
//     belongs to a row that is not there is empty; reported as "done" or
//     "none", both tell the caller the row exists.
func TestMissingRowIsAnsweredAsMissing(t *testing.T) {
	seed := seedFaultDB(t)
	path := filepath.Join(t.TempDir(), "copy.db")
	if err := os.WriteFile(path, seed.image, 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s, err := newServer(d)
	if err != nil {
		t.Fatal(err)
	}
	// The requests below come several dozen to a token within a second, and
	// the per-token limit would refuse most of them before their route.
	s.tokenLimiter = nil
	router := s.Router()

	// A write route gets the body and the real ids of its sweep call, so that
	// it is the missing row the handler stops at and not a malformed request.
	bodies := map[string]any{}
	realURL := map[string]string{}
	for _, c := range writeCalls(seed) {
		if _, ok := bodies[c.method+" "+c.pattern]; !ok {
			bodies[c.method+" "+c.pattern] = c.body
			realURL[c.method+" "+c.pattern] = strings.SplitN(c.url, "?", 2)[0]
		}
	}
	var internal, success []string
	done := map[string]bool{}
	notFound := 0
	routes := 0
	err = chi.Walk(router.(chi.Routes), func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		pattern = strings.TrimSuffix(pattern, "/")
		if !strings.HasPrefix(pattern, "/api/") || !routeParam.MatchString(pattern) {
			return nil
		}
		routes++
		real, ok := realURL[method+" "+pattern]
		if !ok {
			if real, ok = seed.concreteURL(pattern); !ok {
				return fmt.Errorf("no real ids for %s %s", method, pattern)
			}
		}
		// The variants: every parameter missing, then each one alone.
		urls := []string{routeParam.ReplaceAllString(pattern, fmt.Sprint(noSuchID))}
		segments, realSegments := strings.Split(pattern, "/"), strings.Split(real, "/")
		if len(segments) != len(realSegments) {
			return fmt.Errorf("%s does not line up with its real URL %s", pattern, real)
		}
		for i, seg := range segments {
			if routeParam.MatchString(seg) && strings.Count(pattern, "{") > 1 {
				variant := append([]string{}, realSegments...)
				variant[i] = fmt.Sprint(noSuchID)
				urls = append(urls, strings.Join(variant, "/"))
			}
		}
		query := ""
		if queries := seed.getQueries(pattern); len(queries) > 0 {
			query = "?" + queries[0]
		}
		var payload []byte
		if body := bodies[method+" "+pattern]; body != nil {
			if payload, err = json.Marshal(body); err != nil {
				return err
			}
		}
		answered404 := false
		for _, url := range urls {
			for _, who := range getIdentities(pattern) {
				req := httptest.NewRequest(method, url+query, bytes.NewReader(payload))
				req.Header.Set("Content-Type", "application/json")
				seed.authenticate(req, who)
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				answer := fmt.Sprintf("%s %s %s: %d %s", who, method, url, rec.Code, strings.TrimSpace(rec.Body.String()))
				switch {
				case rec.Code == http.StatusTooManyRequests:
					return fmt.Errorf("refused before its route was reached: %s", answer)
				case rec.Code >= 500:
					internal = append(internal, answer)
				case rec.Code < 300:
					if _, ok := goneIsDone[method+" "+pattern]; ok {
						done[method+" "+pattern] = true
					} else {
						success = append(success, answer)
					}
				}
				answered404 = answered404 || rec.Code == http.StatusNotFound
			}
		}
		if answered404 {
			notFound++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(internal)
	if len(internal) > 0 {
		t.Errorf("a row that does not exist was answered as an internal error:\n%s", strings.Join(internal, "\n"))
	}
	sort.Strings(success)
	if len(success) > 0 {
		t.Errorf("a row that does not exist was answered with success:\n%s", strings.Join(success, "\n"))
	}
	for route := range goneIsDone {
		if !done[route] {
			t.Errorf("goneIsDone lists %s, which does not answer success for a missing row (or is not a route)", route)
		}
	}
	// Most of these routes look their row up first. If hardly any says "not
	// found", the requests are being stopped before the lookups.
	if notFound < routes/2 {
		t.Errorf("only %d of %d routes answer 404 for a row that does not exist; the requests are not reaching the lookups", notFound, routes)
	}
}

// TestUsernameChangeTellsFreeTakenAndUnknownApart: a name can be taken
// exactly when the lookup for it finds no row. A row found means the name is
// in use; a lookup that failed means neither, and the name is not changed.
func TestUsernameChangeTellsFreeTakenAndUnknownApart(t *testing.T) {
	seed := seedFaultDB(t)
	dir := t.TempDir()
	var own, taken string
	func() {
		d, err := sql.Open("sqlite", seed.path)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		if err := d.QueryRow(`SELECT username FROM users WHERE id=?`, seed.userID).Scan(&own); err != nil {
			t.Fatal(err)
		}
		if err := d.QueryRow(`SELECT username FROM users WHERE id=?`, seed.plainID).Scan(&taken); err != nil {
			t.Fatal(err)
		}
	}()
	serial := 0
	rename := func(to string, failAt int) sweepRun {
		t.Helper()
		serial++
		call := sweepCall{method: http.MethodPost, pattern: "/api/my/username", url: "/api/my/username", who: "user", body: map[string]any{"username": to}}
		run, err := runSweepCall(seed, filepath.Join(dir, fmt.Sprintf("rename-%d.db", serial)), call, failAt)
		if err != nil {
			t.Fatal(err)
		}
		return run
	}

	free := rename("a-name-nobody-has", 0)
	if free.status != http.StatusOK || !free.changed {
		t.Errorf("a free name: %d %s (stored: %v), want 200 and the name changed", free.status, free.body, free.changed)
	}
	if got := rename(taken, 0); got.status != http.StatusConflict || got.changed {
		t.Errorf("a name another user has: %d %s (stored: %v), want 409 and nothing changed", got.status, got.body, got.changed)
	}
	if got := rename(own, 0); got.status != http.StatusOK || got.changed {
		t.Errorf("the user's own name: %d %s (stored: %v), want 200 and nothing changed", got.status, got.body, got.changed)
	}

	lookup := 0
	for i, stmt := range free.stmts {
		if strings.Contains(stmt, "FROM users WHERE username") {
			lookup = i + 1
		}
	}
	if lookup == 0 {
		t.Fatalf("the rename issues no lookup by username; its statements:\n%s", strings.Join(free.stmts, "\n"))
	}
	for _, to := range []string{"a-name-nobody-has", taken} {
		got := rename(to, lookup)
		if !strings.Contains(got.failed, "FROM users WHERE username") {
			t.Fatalf("statement %d is %q here, not the lookup by username", lookup, got.failed)
		}
		if got.status != http.StatusInternalServerError || got.changed {
			t.Errorf("%q with the lookup failing: %d %s (stored: %v), want 500 and nothing changed", to, got.status, got.body, got.changed)
		}
	}
}

// TestIDsNamedInABodyAreLookedUpBeforeTheyAreWritten: a request that names,
// in its body, a user or node to grant is refused when there is no such row
// — naming the id, and storing none of the other things it asked for. The
// foreign keys on user_nodes would stop the grant as well, but as a failed
// insert, answered as an internal error.
func TestIDsNamedInABodyAreLookedUpBeforeTheyAreWritten(t *testing.T) {
	seed := seedFaultDB(t)
	dir := t.TempDir()
	type m = map[string]any
	for i, c := range []struct {
		name   string
		url    string
		body   m
		status int
	}{
		{"a new node granted to a user that does not exist", "/api/nodes",
			m{"name": "granted-to-nobody", "user_ids": []int64{seed.userID, noSuchID}}, http.StatusBadRequest},
		{"a roster that adds a user that does not exist", fmt.Sprintf("/api/nodes/%d/grants/batch", seed.spareID),
			m{"add_user_ids": []int64{seed.plainID, noSuchID}}, http.StatusBadRequest},
		{"a grant on a node that does not exist", fmt.Sprintf("/api/users/%d/grants", seed.plainID),
			m{"node_ids": []int64{seed.spareID, noSuchID}}, http.StatusNotFound},
	} {
		call := sweepCall{method: http.MethodPost, pattern: c.url, url: c.url, who: "admin", body: c.body}
		run, err := runSweepCall(seed, filepath.Join(dir, fmt.Sprintf("body-%d.db", i)), call, 0)
		if err != nil {
			t.Fatal(err)
		}
		if run.status != c.status {
			t.Errorf("%s: %d %s, want %d", c.name, run.status, run.body, c.status)
		}
		if !strings.Contains(run.body, fmt.Sprint(noSuchID)) {
			t.Errorf("%s: the answer does not name the id that is missing: %s", c.name, run.body)
		}
		if run.changed {
			t.Errorf("%s: the request was refused (%d) but part of it is stored", c.name, run.status)
		}
	}
}
