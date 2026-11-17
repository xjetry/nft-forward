package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"nft-forward/internal/db"
)

// faultSeed is a database file with at least one row behind every list a GET
// endpoint can return, plus the credentials and ids needed to call them.
type faultSeed struct {
	path                                string
	adminCookie, userCookie             *http.Cookie
	adminToken, userToken               string
	userID, nodeID, compositeID, ruleID int64
}

func seedFaultDB(t *testing.T) faultSeed {
	t.Helper()
	seed := faultSeed{path: filepath.Join(t.TempDir(), "seed.db")}
	d, err := db.Open(seed.path)
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	exec := func(q string, args ...any) {
		t.Helper()
		_, err := d.Exec(q, args...)
		must(err)
	}
	if _, err := EnsureSelfNode(d); err != nil {
		t.Fatal(err)
	}
	seed.adminCookie = loginAsAdmin(t, d)
	var adminID int64
	must(d.QueryRow(`SELECT id FROM users WHERE role='admin'`).Scan(&adminID))
	seed.adminToken, err = db.CreateAPIToken(d, adminID, "readwrite")
	must(err)
	seed.userID, seed.userCookie = loginAsUser(t, d, 10)
	seed.userToken, err = db.CreateAPIToken(d, seed.userID, "readwrite")
	must(err)

	rule, n0, n1 := seedTwoHopRuleDB(t, d)
	seed.ruleID, seed.nodeID = rule.ID, n0
	exec(`UPDATE rules SET owner_id=? WHERE id=?`, seed.userID, rule.ID)
	composite, err := db.CreateNode(d, "fault-composite", "", "")
	must(err)
	seed.compositeID = composite.ID
	exec(`UPDATE nodes SET node_type='composite' WHERE id=?`, composite.ID)
	must(db.CreateNodeHops(d, composite.ID, []db.NodeHop{{HopNodeID: n0, Position: 0, Mode: "kernel", TrafficMultiplier: 1}, {HopNodeID: n1, Position: 1, Mode: "kernel", TrafficMultiplier: 1}}))
	for _, nid := range []int64{n0, n1, composite.ID} {
		must(db.GrantNode(d, seed.userID, nid, 5, 1<<30))
	}
	exec(`INSERT INTO node_bindings(upstream_node_id, downstream_node_id, mode) VALUES (?,?, 'userspace')`, n0, n1)
	must(db.AddNodeRawTraffic(d, n0, 4096))
	exec(`UPDATE users SET landing_uris=? WHERE id=?`, "vless://11111111-1111-1111-1111-111111111111@198.51.100.10:443?security=tls#fault-landing", seed.userID)
	seedLandingExit(t, d, seed.userID, "198.51.100.10", 443, 1<<30, 2048)
	for k, v := range map[string]string{
		"panel_name": "fault-panel", "panel_url": "https://panel.example.com", "show_rate_to_user": "1", "pool_size": "6",
		"node_roles": `{"vless:198.51.100.10:443":1}`,
	} {
		must(db.SetSetting(d, k, v))
	}
	db.WriteAudit(d, adminID, "seed", "1", "x")
	must(d.Close())
	return seed
}

type faultCall struct{ who, url string }
type faultReply struct {
	status int
	body   string
}

var routeParam = regexp.MustCompile(`\{[^}]+\}`)

// concreteURL fills a route pattern's parameters with seeded ids. ok is false
// for a pattern it has no value for.
func (seed faultSeed) concreteURL(pattern string) (string, bool) {
	ok := true
	url := routeParam.ReplaceAllStringFunc(pattern, func(param string) string {
		before := pattern[:strings.Index(pattern, param)]
		switch {
		case param == "{nodeID}":
			return fmt.Sprint(seed.nodeID)
		case param == "{id}" && strings.HasSuffix(before, "/nodes/"):
			return fmt.Sprint(seed.compositeID)
		case param == "{id}" && strings.HasSuffix(before, "/rules/"):
			return fmt.Sprint(seed.ruleID)
		case param == "{id}" && strings.HasSuffix(before, "/users/"):
			return fmt.Sprint(seed.userID)
		}
		ok = false
		return param
	})
	return url, ok
}

// runGETs serves every GET route of the JSON APIs from a private copy of the
// seed database, as each identity that can authenticate to it. With
// brokenTable set, that table is renamed away after the server is built, so
// every query that reads it fails while everything else keeps working.
func runGETs(t *testing.T, seed faultSeed, brokenTable string) (map[faultCall]faultReply, []string) {
	t.Helper()
	raw, err := os.ReadFile(seed.path)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "copy.db")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	s, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	// New starts a landing sync pass in the background; let it finish before
	// the table goes away so that it is the requests below that meet the fault.
	time.Sleep(50 * time.Millisecond)
	if brokenTable != "" {
		if _, err := d.Exec(`ALTER TABLE ` + brokenTable + ` RENAME TO ` + brokenTable + `_gone`); err != nil {
			t.Fatalf("rename %s: %v", brokenTable, err)
		}
	}
	router := s.Router()
	replies := map[faultCall]faultReply{}
	var unfilled []string
	err = chi.Walk(router.(chi.Routes), func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		pattern = strings.TrimSuffix(pattern, "/")
		if method != http.MethodGet || !strings.HasPrefix(pattern, "/api/") {
			return nil
		}
		url, ok := seed.concreteURL(pattern)
		if !ok {
			unfilled = append(unfilled, pattern)
			return nil
		}
		// A slice, not a map: the requests have side effects (a token's last
		// use, a landing sync), so their order is part of what is compared.
		type identity struct {
			who          string
			authenticate func(*http.Request)
		}
		identities := []identity{
			{"admin", func(r *http.Request) { r.AddCookie(seed.adminCookie) }},
			{"user", func(r *http.Request) { r.AddCookie(seed.userCookie) }},
		}
		if strings.HasPrefix(pattern, "/api/v1/") {
			identities = []identity{
				{"admin-token", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+seed.adminToken) }},
				{"user-token", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+seed.userToken) }},
			}
		}
		for _, id := range identities {
			req := httptest.NewRequest(http.MethodGet, url, nil)
			id.authenticate(req)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			body, _ := io.ReadAll(rec.Body)
			replies[faultCall{id.who, url}] = faultReply{rec.Code, string(body)}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(unfilled)
	return replies, unfilled
}

func faultTables(t *testing.T, seed faultSeed) []string {
	t.Helper()
	d, err := sql.Open("sqlite", seed.path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rows, err := d.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	return tables
}

// flattenJSON turns a reply body into path -> value for every scalar in it
// (and the length of every array), so that two replies can be compared field
// by field. A body that is not JSON is a single value at the empty path.
func flattenJSON(body string) map[string]string {
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return map[string]string{"": body}
	}
	out := map[string]string{}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			out[path+".{}"] = strconv.Itoa(len(x))
			for k, c := range x {
				walk(path+"."+k, c)
			}
		case []any:
			out[path+".#"] = strconv.Itoa(len(x))
			for i, c := range x {
				walk(path+"["+strconv.Itoa(i)+"]", c)
			}
		default:
			b, _ := json.Marshal(x)
			out[path] = string(b)
		}
	}
	walk("", v)
	return out
}

// differingPaths lists the paths at which two replies differ, skipping the
// ones in ignore.
func differingPaths(a, b string, ignore map[string]bool) []string {
	fa, fb := flattenJSON(a), flattenJSON(b)
	seen := map[string]bool{}
	var out []string
	for _, m := range []map[string]string{fa, fb} {
		for path := range m {
			if seen[path] || ignore[path] {
				continue
			}
			seen[path] = true
			va, oka := fa[path]
			vb, okb := fb[path]
			if oka != okb || va != vb {
				out = append(out, path)
			}
		}
	}
	sort.Strings(out)
	return out
}

// A GET endpoint answers from what its queries return. If one of them fails
// and the handler carries on, the reply is still a 200 but says something
// else — an empty list, a zero count, a missing name — and the page shows
// that as fact. So, for every table: with that table unreadable, each
// endpoint must either fail or reply exactly as it does when nothing is
// wrong (which is what an endpoint that never reads the table does).
//
// "Exactly" leaves out the fields that carry the time of the request
// (last_seen, generated_at, ...). They are found, not listed: two fault-free
// runs are taken more than a second apart, and every field that differs
// between them is left out of the comparison for that call. A failure that
// shows only in such a field is therefore not seen here.
func TestGETEndpointsDoNotHideQueryFailures(t *testing.T) {
	seed := seedFaultDB(t)
	baseline, unfilled := runGETs(t, seed, "")
	if len(unfilled) > 0 {
		t.Fatalf("no seeded value for the parameters of these routes, so they are not covered:\n%s", strings.Join(unfilled, "\n"))
	}
	// Timestamps are whole seconds; the second run has to land in a later one.
	time.Sleep(1100 * time.Millisecond)
	again, _ := runGETs(t, seed, "")
	var calls []faultCall
	timeFields := map[faultCall]map[string]bool{}
	masked := map[string]bool{}
	for call, reply := range baseline {
		if reply.status != http.StatusOK {
			continue
		}
		calls = append(calls, call)
		timeFields[call] = map[string]bool{}
		for _, path := range differingPaths(reply.body, again[call].body, nil) {
			timeFields[call][path] = true
			masked[call.url+" "+path] = true
		}
	}
	if len(masked) > 0 {
		list := make([]string, 0, len(masked))
		for m := range masked {
			list = append(list, m)
		}
		sort.Strings(list)
		t.Logf("fields left out of the comparison (they differ between two fault-free runs):\n%s", strings.Join(list, "\n"))
	}
	if len(calls) < 20 {
		t.Fatalf("only %d comparable calls; the seed or the route walk is broken", len(calls))
	}
	tables := faultTables(t, seed)
	if len(tables) < 10 {
		t.Fatalf("only %d tables found: %v", len(tables), tables)
	}
	var hidden []string
	for _, table := range tables {
		got, _ := runGETs(t, seed, table)
		for _, call := range calls {
			reply := got[call]
			if reply.status != http.StatusOK {
				continue
			}
			if diff := differingPaths(baseline[call].body, reply.body, timeFields[call]); len(diff) > 0 {
				hidden = append(hidden, fmt.Sprintf("%-22s unreadable: %s GET %s still 200, differs at %s (%d field(s))", table, call.who, call.url, diff[0], len(diff)))
			}
		}
	}
	sort.Strings(hidden)
	if len(hidden) > 0 {
		t.Fatalf("%d reply(ies) hide a failed query (%d calls x %d tables checked):\n%s", len(hidden), len(calls), len(tables), strings.Join(hidden, "\n"))
	}
	t.Logf("%d calls x %d tables checked", len(calls), len(tables))
}
