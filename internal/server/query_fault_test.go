package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
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
	path                    string
	image                   []byte // the seeded database file
	adminCookie, userCookie *http.Cookie
	adminToken, userToken   string
	// plainID / plainCookie is a second user, with no API token, grants or
	// rules.
	plainID     int64
	plainCookie *http.Cookie
	// userID is both the "user" identity and the user that admin calls act on.
	userID int64
	// nodeID and viaID are the two hops of ruleID, bound nodeID -> viaID.
	// compositeID is a composite of the two. spareID is a node nothing refers
	// to, for the calls that only work on such a node (deleting it).
	nodeID, viaID, compositeID, spareID, ruleID int64
}

// The seeded users' passwords, as set by loginAsAdmin and loginAsUser.
const (
	faultAdminName, faultAdminPassword = "admin-test", "testpass"
	faultUserPassword                  = "pw"
)

// The seeded user's landing exits: one its source currently lists, and one
// that has dropped out of the source and is only kept for its usage.
const (
	faultExitHost     = "198.51.100.10"
	faultGoneExitHost = "198.51.100.11"
	faultExitPort     = 443
)

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
	seed.ruleID, seed.nodeID, seed.viaID = rule.ID, n0, n1
	spare, err := db.CreateNode(d, "fault-spare", "https://spare", "tok-fault-spare")
	must(err)
	seed.spareID = spare.ID
	exec(`UPDATE nodes SET relay_host='10.0.0.12' WHERE id=?`, spare.ID)
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
	seedLandingExit(t, d, seed.userID, faultExitHost, faultExitPort, 1<<30, 2048)
	seedLandingExit(t, d, seed.userID, faultGoneExitHost, faultExitPort, 0, 512)
	exec(`UPDATE user_landing_exits SET present=0 WHERE user_id=? AND host=?`, seed.userID, faultGoneExitHost)
	for k, v := range map[string]string{
		"panel_name": "fault-panel", "panel_url": "https://panel.example.com", "show_rate_to_user": "1", "pool_size": "6",
		"node_roles": `{"vless:198.51.100.10:443":1}`,
	} {
		must(db.SetSetting(d, k, v))
	}
	db.WriteAudit(d, adminID, "seed", "1", "x")
	seed.plainID, seed.plainCookie = loginAsUser(t, d, 10)
	must(d.Close())
	seed.image, err = os.ReadFile(seed.path)
	must(err)
	return seed
}

// A dbFault is one way the database can fail a query while the server keeps
// running. It is applied by statements, after the server is built.
type dbFault struct {
	name  string
	stmts []string
}

// unreadableTable renames the table away: every statement that names it fails.
func unreadableTable(table string) dbFault {
	return dbFault{table + " unreadable", []string{`ALTER TABLE ` + table + ` RENAME TO ` + table + `_gone`}}
}

// unwritableTable leaves the table readable and fails every write to it. This
// is the fault that reaches a handler's writes to a table its own
// authentication has to read first (users, sessions, api_tokens).
func unwritableTable(table string) dbFault {
	f := dbFault{name: table + " unwritable"}
	for _, op := range []string{"INSERT", "UPDATE", "DELETE"} {
		f.stmts = append(f.stmts, `CREATE TRIGGER fault_`+table+`_`+strings.ToLower(op)+` BEFORE `+op+` ON `+table+
			` BEGIN SELECT RAISE(ABORT, 'fault: `+table+` is unwritable'); END`)
	}
	return f
}

// unreadableUserRow makes reading one user's row fail while every other row
// reads fine: a text value in an integer column fails the row's Scan. It
// reaches the lookups of that user which run after the caller — another user —
// has been authenticated; with the whole table unreadable no request gets
// that far.
func unreadableUserRow(userID int64) dbFault {
	return dbFault{fmt.Sprintf("users row %d unreadable", userID),
		[]string{fmt.Sprintf(`UPDATE users SET traffic_quota_bytes='unreadable' WHERE id=%d`, userID)}}
}

// serveWithFault builds a server over a private copy of the seed database at
// path and then applies the fault (nil for none), so that it is the requests
// served afterwards that meet it. The returned function releases the server.
func serveWithFault(seed faultSeed, path string, fault *dbFault) (http.Handler, func(), error) {
	if err := os.WriteFile(path, seed.image, 0o600); err != nil {
		return nil, nil, err
	}
	d, err := db.Open(path)
	if err != nil {
		return nil, nil, err
	}
	s, err := New(d)
	if err != nil {
		d.Close()
		return nil, nil, err
	}
	release := func() {
		close(s.stopExpiry)
		close(s.stopCycle)
		close(s.stopLandingSync)
		d.Close()
	}
	// New starts a landing sync pass in the background. Running one to the end
	// here leaves the landing ledger settled before the fault is applied; the
	// background pass then has nothing left to change.
	s.landingSyncPass(true)
	if fault != nil {
		for _, stmt := range fault.stmts {
			if _, err := d.Exec(stmt); err != nil {
				release()
				return nil, nil, fmt.Errorf("apply fault %q: %w", fault.name, err)
			}
		}
	}
	return s.Router(), release, nil
}

// authenticate makes the request come from one of the seeded identities.
func (seed faultSeed) authenticate(req *http.Request, who string) {
	switch who {
	case "admin":
		req.AddCookie(seed.adminCookie)
	case "user":
		req.AddCookie(seed.userCookie)
	case "admin-token":
		req.Header.Set("Authorization", "Bearer "+seed.adminToken)
	case "user-token":
		req.Header.Set("Authorization", "Bearer "+seed.userToken)
	case "user-no-token":
		req.AddCookie(seed.plainCookie)
	case "nobody":
	default:
		panic("unknown identity " + who)
	}
}

type faultCall struct{ who, url string }
type faultReply struct {
	status int
	body   string
}

var routeParam = regexp.MustCompile(`\{[^}]+\}`)

// concreteURLs are the URLs to try a GET route at: its pattern with the
// parameters filled with seeded ids, once per query string the route needs
// (getQueries). ok is false for a pattern with a parameter there is no seeded
// value for.
func (seed faultSeed) concreteURLs(pattern string) ([]string, bool) {
	base, ok := seed.concreteURL(pattern)
	if !ok {
		return nil, false
	}
	queries := seed.getQueries(pattern)
	if len(queries) == 0 {
		return []string{base}, true
	}
	urls := make([]string, len(queries))
	for i, q := range queries {
		urls[i] = base + "?" + q
	}
	return urls, true
}

// getQueries are the query strings for the GET routes whose handler refuses
// the request without one, and so would never reach a statement. The probes
// name a node: without one the panel itself would dial the target.
func (seed faultSeed) getQueries(pattern string) []string {
	target := "target=" + net.JoinHostPort(faultExitHost, strconv.Itoa(faultExitPort))
	switch strings.TrimPrefix(pattern, "/api/v1") {
	case "/api/probe", "/probe":
		return []string{
			fmt.Sprintf("%s&node=%d", target, seed.nodeID),
			fmt.Sprintf("%s&node=%d", target, seed.compositeID),
		}
	case "/api/probe-chain", "/probe-chain":
		return []string{fmt.Sprintf("rule_id=%d", seed.ruleID)}
	}
	return nil
}

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

// getIdentities are the identities to try a GET route as: the ones that can
// authenticate to the API the route belongs to.
func getIdentities(pattern string) []string {
	if strings.HasPrefix(pattern, "/api/v1/") {
		return []string{"admin-token", "user-token"}
	}
	return []string{"admin", "user"}
}

// runGETs serves every GET route of the JSON APIs under the fault (nil for
// none), as each identity that can authenticate to it.
func runGETs(t *testing.T, seed faultSeed, fault *dbFault) (map[faultCall]faultReply, []string) {
	t.Helper()
	router, release, err := serveWithFault(seed, filepath.Join(t.TempDir(), "copy.db"), fault)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	replies := map[faultCall]faultReply{}
	var unfilled []string
	err = chi.Walk(router.(chi.Routes), func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		pattern = strings.TrimSuffix(pattern, "/")
		if method != http.MethodGet || !strings.HasPrefix(pattern, "/api/") {
			return nil
		}
		urls, ok := seed.concreteURLs(pattern)
		if !ok {
			unfilled = append(unfilled, pattern)
			return nil
		}
		// A slice, not a map: the requests have side effects (a token's last
		// use, a landing sync), so their order is part of what is compared.
		for _, url := range urls {
			for _, who := range getIdentities(pattern) {
				req := httptest.NewRequest(http.MethodGet, url, nil)
				seed.authenticate(req, who)
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				body, _ := io.ReadAll(rec.Body)
				if rec.Code == http.StatusTooManyRequests {
					// Refused by the per-token limit, the request never got to
					// its route, and its reply would compare equal under any
					// fault.
					t.Fatalf("%s GET %s was rate limited: %s", who, url, body)
				}
				replies[faultCall{who, url}] = faultReply{rec.Code, string(body)}
			}
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

// readFaults are the faults a query can meet when reading: each table
// unreadable, and the seeded user's row unreadable.
func readFaults(t *testing.T, seed faultSeed) []dbFault {
	t.Helper()
	tables := faultTables(t, seed)
	if len(tables) < 10 {
		t.Fatalf("only %d tables found: %v", len(tables), tables)
	}
	var faults []dbFault
	for _, table := range tables {
		faults = append(faults, unreadableTable(table))
	}
	return append(faults, unreadableUserRow(seed.userID))
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
	baseline, unfilled := runGETs(t, seed, nil)
	if len(unfilled) > 0 {
		t.Fatalf("no seeded value for the parameters of these routes, so they are not covered:\n%s", strings.Join(unfilled, "\n"))
	}
	// Timestamps are whole seconds; the second run has to land in a later one.
	time.Sleep(1100 * time.Millisecond)
	again, _ := runGETs(t, seed, nil)
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
	faults := readFaults(t, seed)
	var hidden, misreported []string
	for _, fault := range faults {
		got, _ := runGETs(t, seed, &fault)
		for _, call := range calls {
			reply := got[call]
			switch {
			case reply.status >= 500:
			case reply.status != http.StatusOK:
				misreported = append(misreported, fmt.Sprintf("%-26s %s GET %s answered %d %s", fault.name+":", call.who, call.url, reply.status, strings.TrimSpace(reply.body)))
			default:
				if diff := differingPaths(baseline[call].body, reply.body, timeFields[call]); len(diff) > 0 {
					hidden = append(hidden, fmt.Sprintf("%-26s %s GET %s still 200, differs at %s (%d field(s))", fault.name+":", call.who, call.url, diff[0], len(diff)))
				}
			}
		}
	}
	sort.Strings(hidden)
	sort.Strings(misreported)
	if len(hidden) > 0 {
		t.Errorf("%d reply(ies) hide a failed query (%d calls x %d faults checked):\n%s", len(hidden), len(calls), len(faults), strings.Join(hidden, "\n"))
	}
	if len(misreported) > 0 {
		t.Errorf("%d reply(ies) answer a failed query as something the request did wrong (%d calls x %d faults checked):\n%s", len(misreported), len(calls), len(faults), strings.Join(misreported, "\n"))
	}
	t.Logf("%d calls x %d faults checked", len(calls), len(faults))
}
