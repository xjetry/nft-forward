package server

import (
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// fillRoute puts args, in order, in place of a route pattern's parameters.
func fillRoute(pattern string, args ...any) string {
	i := 0
	return routeParam.ReplaceAllStringFunc(pattern, func(string) string {
		i++
		return fmt.Sprint(args[i-1])
	})
}

// writeCalls is at least one request for every write route of the JSON APIs,
// each of which succeeds against the seed database unless it says otherwise
// (want). Success matters: a request refused early never reaches the queries
// behind the refusal, so failing them would test nothing.
func writeCalls(seed faultSeed) []sweepCall {
	u, plain := seed.userID, seed.plainID
	n0, n1, composite, spare, rule := seed.nodeID, seed.viaID, seed.compositeID, seed.spareID, seed.ruleID
	type m = map[string]any
	var calls []sweepCall
	add := func(who, method, pattern string, args []any, body any, want ...int) {
		c := sweepCall{method: method, pattern: pattern, url: fillRoute(pattern, args...), who: who, body: body}
		if len(want) > 0 {
			c.want = want[0]
		}
		calls = append(calls, c)
	}
	id := func(v ...any) []any { return v }
	exit := func(extra m) m {
		b := m{"host": faultExitHost, "port": faultExitPort}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}
	hops := []m{{"node_id": n0, "mode": "kernel"}, {"node_id": n1, "mode": "kernel"}}
	ruleBody := func(name string) m {
		return m{"node_id": n0, "name": name, "proto": "tcp", "exit": "9.9.9.9:443"}
	}

	add("nobody", "POST", "/api/login", nil, m{"username": faultAdminName, "password": faultAdminPassword})
	add("admin", "POST", "/api/logout", nil, nil)
	add("user", "POST", "/api/change-password", nil, m{"old_password": faultUserPassword, "new_password": "sweep-password-1"})
	// The subscription fetcher refuses a loopback address before dialling.
	add("admin", "POST", "/api/sub-fetch", nil, m{"url": "http://127.0.0.1:1/sub"}, http.StatusBadGateway)

	add("admin", "POST", "/api/nodes", nil, m{"name": "sweep-node"})
	add("admin", "POST", "/api/nodes", nil, m{"name": "sweep-node-full", "rate_multiplier": 2, "unidirectional": true, "port_range": "20000-20100", "user_ids": []int64{u, plain}})
	add("admin", "POST", "/api/nodes", nil, m{"name": "sweep-composite", "node_type": "composite", "hops": hops})
	add("admin", "POST", "/api/nodes", nil, m{"name": "sweep-composite-full", "node_type": "composite", "hops": hops, "rate_multiplier": 2, "user_ids": []int64{u}})
	add("admin", "POST", "/api/nodes/{id}/rename", id(spare), m{"name": "sweep-renamed"})
	add("admin", "POST", "/api/nodes/{id}/relay-host", id(spare), m{"relay_host": "10.0.0.99"})
	// A node the seeded rule runs through: the rule is re-generated.
	add("admin", "POST", "/api/nodes/{id}/relay-host", id(n1), m{"relay_host": "10.0.0.98"})
	add("admin", "POST", "/api/nodes/{id}/relay-host-v6", id(spare), m{"relay_host_v6": "2001:db8::1"})
	add("admin", "POST", "/api/nodes/{id}/relay-host-v6", id(n0), m{"relay_host_v6": ""})
	add("admin", "POST", "/api/nodes/{id}/port-range", id(spare), m{"port_range": "20000-20100"})
	// No agent is connected in this test, and a push to a node that is not
	// connected is answered 409 (pushErrAnswer).
	add("admin", "POST", "/api/nodes/{id}/resync", id(n0), nil, http.StatusConflict)
	add("admin", "POST", "/api/nodes/{id}/upgrade", id(n0), nil, http.StatusConflict)
	add("admin", "DELETE", "/api/nodes/{id}", id(spare), nil)
	// A node the seeded rule runs through, and a member of the composite.
	add("admin", "DELETE", "/api/nodes/{id}", id(n1), nil)
	calls[len(calls)-1].url += "?confirm=1"
	add("admin", "DELETE", "/api/nodes/{id}", id(composite), nil)
	// The seeded rule's entry: the rule goes with it.
	add("admin", "DELETE", "/api/nodes/{id}", id(n0), nil)
	calls[len(calls)-1].url += "?confirm=1"
	add("admin", "POST", "/api/nodes/{id}/toggle", id(spare), nil)
	add("admin", "POST", "/api/nodes/{id}/owner", id(spare), m{"owner_id": u})
	add("admin", "POST", "/api/nodes/reorder", nil, m{"ids": []int64{spare, n1, n0}})
	add("admin", "POST", "/api/nodes/resync-all", nil, nil)
	add("admin", "POST", "/api/nodes/upgrade-all", nil, nil)
	add("admin", "POST", "/api/nodes/{id}/rate-multiplier", id(spare), m{"rate_multiplier": 2})
	add("admin", "POST", "/api/nodes/{id}/unidirectional", id(spare), m{"unidirectional": true})
	add("admin", "POST", "/api/nodes/{id}/no-direct-exit", id(spare), m{"no_direct_exit": true})
	add("admin", "POST", "/api/nodes/{id}/hops", id(composite), m{"hops": hops})
	add("admin", "POST", "/api/nodes/{id}/roles", id(spare), m{"roles": 3})
	add("admin", "POST", "/api/nodes/{id}/bindings", id(spare), m{"upstream_node_id": n0, "mode": "kernel"})
	add("admin", "POST", "/api/nodes/{id}/downstream-bindings", id(n0), m{"downstream_node_id": spare, "mode": "kernel"})
	add("admin", "POST", "/api/nodes/{id}/grants/batch", id(spare), m{"add_user_ids": []int64{u}, "max_forwards": 3})
	// Takes the node from the user whose rule enters through it.
	add("admin", "POST", "/api/nodes/{id}/grants/batch", id(n0), m{"add_user_ids": []int64{plain}, "remove_user_ids": []int64{u}})
	add("admin", "POST", "/api/settings", nil, m{"panel_url": "https://panel.example.com", "panel_name": "sweep", "show_rate_to_user": false, "pool_size": 5})
	add("admin", "POST", "/api/node-roles", nil, m{"keys": []string{"vless:" + faultExitHost + ":443"}, "role": 1, "on": false})

	add("admin", "POST", "/api/rules", nil, ruleBody("sweep-rule"))
	add("admin", "PUT", "/api/rules/{id}", id(rule), m{"node_id": n0, "name": "sweep-edited", "proto": "tcp", "exit": "9.9.9.9:444", "via_node_ids": []int64{n1}})
	add("admin", "DELETE", "/api/rules/{id}", id(rule), nil)
	add("admin", "POST", "/api/rules/{id}/hops/{pos}/reallocate", id(rule, 0), m{"port": 0})

	add("admin", "POST", "/api/users", nil, m{"username": "sweep-user", "password": "sweep-password-1", "role": "user"})
	add("admin", "POST", "/api/users", nil, m{"username": "sweep-user-full", "password": "sweep-password-1", "max_forwards": 7, "traffic_quota_bytes": 1 << 30, "expires_at": "2099-01-01", "admin_note": "sweep"})
	add("admin", "POST", "/api/users/{id}/grants", id(u), m{"node_id": spare, "max_forwards": 3})
	add("admin", "POST", "/api/users/{id}/grants", id(plain), m{"node_ids": []int64{n0, spare}, "traffic_quota_bytes": 1 << 20})
	add("admin", "DELETE", "/api/users/{id}/grants/{nodeID}", id(u, composite), nil)
	// The grant the seeded rule runs behind: revoking it takes the rule too.
	add("admin", "DELETE", "/api/users/{id}/grants/{nodeID}", id(u, n0), nil)
	add("admin", "POST", "/api/users/{id}/grants/batch-revoke", id(u), m{"node_ids": []int64{composite, n0}})
	add("admin", "POST", "/api/users/{id}/quota", id(u), m{"traffic_quota_bytes": 1 << 30})
	add("admin", "POST", "/api/users/{id}/max-forwards", id(u), m{"max_forwards": 20})
	add("admin", "POST", "/api/users/{id}/landing", id(u), m{"landing_uris": manualLandingURI})
	add("admin", "POST", "/api/users/{id}/expiry", id(u), m{"expires_at": "2099-01-01"})
	add("admin", "POST", "/api/users/{id}/reset-traffic", id(u), nil)
	add("admin", "POST", "/api/users/{id}/reset-days", id(u), m{"traffic_reset_days": 30})
	add("admin", "POST", "/api/users/{id}/nodes/{nodeID}/max-forwards", id(u, n0), m{"max_forwards": 7})
	add("admin", "POST", "/api/users/{id}/nodes/{nodeID}/quota", id(u, n0), m{"traffic_quota_bytes": 1 << 20})
	add("admin", "POST", "/api/users/{id}/nodes/{nodeID}/rate-limit", id(u, n0), m{"rate_limit_mbytes": 10})
	add("admin", "POST", "/api/users/{id}/nodes/{nodeID}/roles", id(u, n0), m{"roles": 1})
	add("admin", "POST", "/api/users/{id}/nodes/{nodeID}/reset-traffic", id(u, n0), nil)
	add("admin", "POST", "/api/users/{id}/landing-exits/quota", id(u), exit(m{"quota_bytes": 1 << 20}))
	add("admin", "POST", "/api/users/{id}/landing-exits/reset", id(u), exit(nil))
	add("admin", "POST", "/api/users/{id}/landing-exits/delete", id(u), m{"host": faultGoneExitHost, "port": faultExitPort})
	add("admin", "POST", "/api/users/{id}/landing-exits/rename", id(u), exit(m{"name": "sweep-exit"}))
	add("admin", "PATCH", "/api/users/{id}/profile", id(u), m{"expires_at": "2099-01-01", "max_forwards": 20, "traffic_quota_gb": 1, "traffic_reset_days": 30, "admin_note": "sweep", "billing_rate": 1.5})
	add("admin", "POST", "/api/users/{id}/admin-note", id(u), m{"admin_note": "sweep"})
	add("admin", "POST", "/api/users/{id}/toggle", id(u), nil)
	add("admin", "POST", "/api/users/{id}/reset-password", id(u), nil)
	add("admin", "DELETE", "/api/users/{id}", id(u), nil)
	add("admin", "POST", "/api/grants/batch-apply", nil, m{"user_ids": []int64{u}, "grants": []m{{"node_name": "fault-spare", "max_forwards": 3}}})

	add("user", "POST", "/api/my/username", nil, m{"username": "sweep-renamed-user"})
	add("user", "POST", "/api/my/rules", nil, ruleBody("sweep-mine"))
	add("user", "PUT", "/api/my/rules/{id}", id(rule), m{"node_id": n0, "name": "sweep-mine-edited", "proto": "tcp", "exit": "9.9.9.9:444", "via_node_ids": []int64{n1}})
	add("user", "DELETE", "/api/my/rules/{id}", id(rule), nil)
	add("user-no-token", "POST", "/api/my/token", nil, m{"scope": "read"})
	add("user", "POST", "/api/my/token", nil, m{"scope": "read"}, http.StatusConflict)
	add("user", "DELETE", "/api/my/token", nil, nil)
	add("user", "POST", "/api/my/token/refresh", nil, nil)
	add("user", "POST", "/api/my/token/toggle", nil, nil)

	add("user-token", "POST", "/api/v1/my/rules", nil, ruleBody("sweep-v1"))
	add("user-token", "PUT", "/api/v1/my/rules/{id}", id(rule), m{"node_id": n0, "name": "sweep-v1-edited", "proto": "tcp", "exit": "9.9.9.9:444", "via_node_ids": []int64{n1}})
	add("user-token", "DELETE", "/api/v1/my/rules/{id}", id(rule), nil)
	add("admin-token", "POST", "/api/v1/users", nil, m{"username": "sweep-v1-user", "password": "sweep-password-1"})
	add("admin-token", "POST", "/api/v1/users", nil, m{"username": "sweep-v1-user-full", "password": "sweep-password-1", "max_forwards": 7, "expires_at": 4102444800, "issue_token": true, "token_scope": "readwrite"})
	add("admin-token", "POST", "/api/v1/users/{id}/token", id(u), m{"scope": "read"})
	add("admin-token", "DELETE", "/api/v1/users/{id}/token", id(u), nil)
	add("admin-token", "PUT", "/api/v1/users/{id}/quota", id(u), m{"traffic_quota_bytes": 1 << 30})
	add("admin-token", "PUT", "/api/v1/users/{id}/max-forwards", id(u), m{"max_forwards": 20})
	add("admin-token", "PUT", "/api/v1/users/{id}/expiry", id(u), m{"expires_at": 4102444800})
	add("admin-token", "PUT", "/api/v1/users/{id}/enabled", id(u), m{"enabled": false})
	add("admin-token", "PUT", "/api/v1/users/{id}/grants/{nodeId}", id(u, spare), m{"max_forwards": 3})
	add("admin-token", "DELETE", "/api/v1/users/{id}/grants/{nodeId}", id(u, composite), nil)
	add("admin-token", "DELETE", "/api/v1/users/{id}/grants/{nodeId}", id(u, n0), nil)
	add("admin-token", "PUT", "/api/v1/users/{id}/nodes/{nodeId}/quota", id(u, n0), m{"traffic_quota_bytes": 1 << 20})
	add("admin-token", "PUT", "/api/v1/users/{id}/nodes/{nodeId}/rate-limit", id(u, n0), m{"rate_limit_mbytes": 10})
	add("admin-token", "POST", "/api/v1/grants/batch-apply", nil, m{"user_ids": []int64{u}, "grants": []m{{"node_name": "fault-spare", "max_forwards": 3}}})
	add("admin-token", "POST", "/api/v1/nodes/{id}/resync", id(n0), nil, http.StatusConflict)
	add("admin-token", "POST", "/api/v1/nodes/resync-all", nil, nil)
	add("admin-token", "PUT", "/api/v1/users/{id}/landing-sub-url", id(u), m{"landing_sub_url": ""})
	return calls
}

// notSweptGETs are the GET routes the sweep cannot say anything about, and
// why that leaves nothing unchecked.
var notSweptGETs = map[string]string{
	"/api/ws/speed":        "a WebSocket stream of the hub's in-memory speed cache; the handler issues no statement",
	"/api/v1/openapi.json": "a document built into the binary; the handler issues no statement",
}

// savedButFailed matches the way a reply says that the request's change is
// stored although a later step failed: "规则已创建，但没能读回".
var savedButFailed = regexp.MustCompile(`已\p{Han}{1,4}，但`)

// Every statement a request issues can fail. Whichever one does:
//
//   - The reply is a failure (5xx). It is never the answer to a different
//     question — "not found", "forbidden", "already exists" — because a
//     lookup that could not be made says nothing about what it looked for;
//     and it is never success. The exceptions: a statement that belongs to
//     bestEffortWork, which has to leave the reply as it would have been, and
//     a GET whose reply comes out unchanged.
//   - If the request had already stored a change by then, the failure says
//     so (savedButFailed). The caller is otherwise told "failed" about a
//     change that is in effect.
//
// The sweep fails the 1st, 2nd, ... statement of each call in turn, each on a
// fresh database.
func TestEveryStatementFailureIsReportedAsAFailure(t *testing.T) {
	seed := seedFaultDB(t)
	dir := t.TempDir()

	// The routes, from the router itself.
	var getCalls []sweepCall
	routes := map[string]bool{}
	s, err := New(openDB(t))
	if err != nil {
		t.Fatal(err)
	}
	err = chi.Walk(s.Router().(chi.Routes), func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		pattern = strings.TrimSuffix(pattern, "/")
		if !strings.HasPrefix(pattern, "/api/") {
			return nil
		}
		if method != http.MethodGet {
			routes[method+" "+pattern] = true
			return nil
		}
		urls, ok := seed.concreteURLs(pattern)
		if !ok {
			t.Errorf("no seeded value for the parameters of GET %s", pattern)
			return nil
		}
		for _, url := range urls {
			for _, who := range getIdentities(pattern) {
				getCalls = append(getCalls, sweepCall{method: method, pattern: pattern, url: url, who: who})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	writes := writeCalls(seed)
	covered := map[string]bool{}
	for _, c := range writes {
		covered[c.method+" "+c.pattern] = true
		if !routes[c.method+" "+c.pattern] {
			t.Errorf("writeCalls has %s %s, which is not a route", c.method, c.pattern)
		}
	}
	for route := range routes {
		if !covered[route] {
			t.Errorf("no call in writeCalls for %s", route)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	// Run a batch of (call, failAt) jobs on a pool; each has its own database.
	type job struct {
		call   sweepCall
		failAt int
	}
	type outcome struct {
		job
		run sweepRun
		err error
	}
	var serial int
	runAll := func(jobs []job) []outcome {
		out := make([]outcome, len(jobs))
		paths := make([]string, len(jobs))
		for i := range jobs {
			serial++
			paths[i] = filepath.Join(dir, fmt.Sprintf("run-%d.db", serial))
		}
		next := make(chan int)
		var wg sync.WaitGroup
		for w := 0; w < runtime.GOMAXPROCS(0); w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range next {
					run, err := runSweepCall(seed, paths[i], jobs[i].call, jobs[i].failAt)
					out[i] = outcome{jobs[i], run, err}
				}
			}()
		}
		for i := range jobs {
			next <- i
		}
		close(next)
		wg.Wait()
		return out
	}
	faultFree := func(calls []sweepCall) []outcome {
		jobs := make([]job, len(calls))
		for i, c := range calls {
			jobs[i] = job{call: c}
		}
		res := runAll(jobs)
		for _, o := range res {
			if o.err != nil {
				t.Fatalf("%s: %v", o.call, o.err)
			}
		}
		return res
	}

	// Fault-free replies. A GET is kept only where it answers 200, as each
	// identity is tried on every GET route and most are refused by role; but
	// every GET route has to be kept for some identity, or the sweep says
	// nothing about it.
	type baseline struct {
		run        sweepRun
		timeFields map[string]bool
	}
	base := map[string]baseline{}
	swept := map[string]bool{}
	var calls []sweepCall
	var kept []outcome
	first := faultFree(getCalls)
	// Timestamps are whole seconds; the second run has to land in a later one.
	time.Sleep(1100 * time.Millisecond)
	second := faultFree(getCalls)
	for i, o := range first {
		if o.run.status != http.StatusOK {
			continue
		}
		b := baseline{run: o.run, timeFields: map[string]bool{}}
		for _, path := range differingPaths(o.run.body, second[i].run.body, nil) {
			b.timeFields[path] = true
		}
		base[o.call.String()] = b
		calls = append(calls, o.call)
		kept = append(kept, o)
	}
	// A route is swept when one of its kept calls issues a statement of the
	// handler's own. A 200 alone does not show that: a handler can answer
	// one without getting as far as the database (a required query parameter
	// left out, say), having issued only the statements that authenticate.
	authenticated := map[string]bool{}
	for _, o := range kept {
		if o.run.own > 0 {
			swept[o.call.pattern] = true
		}
		for fn := range o.run.auth {
			authenticated[fn] = true
		}
	}
	// "The handler's own" is everything authenticationWork does not claim,
	// so an entry that claims nothing has stopped doing its job.
	for fn := range authenticationWork {
		if !authenticated[fn] {
			t.Errorf("authenticationWork names %s, which issued no statement in any GET", fn)
		}
	}
	for _, o := range first {
		if _, exempt := notSweptGETs[o.call.pattern]; !exempt && !swept[o.call.pattern] {
			swept[o.call.pattern] = true // report each route once
			reply := o.run.body
			if len(reply) > 160 {
				reply = reply[:160] + "…"
			}
			t.Errorf("GET %s is not swept: no identity gets a 200 for which the handler issues a statement of its own (%s: %d %s)", o.call.pattern, o.call, o.run.status, reply)
		}
	}
	for _, o := range faultFree(writes) {
		if o.run.status != o.call.wantStatus() {
			t.Errorf("fault-free %s: got %d %s, want %d", o.call, o.run.status, o.run.body, o.call.wantStatus())
			continue
		}
		base[o.call.String()] = baseline{run: o.run}
		calls = append(calls, o.call)
	}
	if t.Failed() {
		t.FailNow()
	}

	var jobs []job
	for _, c := range calls {
		for k := 1; k <= len(base[c.String()].run.stmts); k++ {
			jobs = append(jobs, job{c, k})
		}
	}
	var misreported, blamed, partial []string
	checked := 0
	for _, o := range runAll(jobs) {
		if o.err != nil {
			t.Fatalf("%s, statement %d: %v", o.call, o.failAt, o.err)
		}
		if o.run.failed == "" {
			continue // this run issued fewer statements than the fault-free one
		}
		checked++
		b := base[o.call.String()]
		where := fmt.Sprintf("%s\n    failed statement %d: %s\n    answered %d %s", o.call, o.failAt, o.run.failed, o.run.status, o.run.body)
		if o.run.failedIn != "" {
			where += "\n    (the statement belongs to " + o.run.failedIn + ")"
		}
		isGET := o.call.method == http.MethodGet
		// A statement of best-effort work that fails leaves the answer what
		// it was without the fault, whatever that was.
		asWithout := !isGET && o.run.status == b.run.status && o.run.failedIn != ""
		switch {
		case o.run.status >= 500:
			// A statement is the panel's own work. The other 5xx answers say
			// that someone else failed (502, pushErrAnswer), and would send
			// the caller looking at a node or a remote host.
			if o.run.status != http.StatusInternalServerError && !asWithout {
				blamed = append(blamed, where)
			}
			if !isGET && o.run.changed && !savedButFailed.MatchString(o.run.body) {
				partial = append(partial, where)
			}
		case isGET && o.run.status == b.run.status && len(differingPaths(b.run.body, o.run.body, b.timeFields)) == 0:
		case asWithout:
		default:
			misreported = append(misreported, where)
		}
	}
	sort.Strings(misreported)
	sort.Strings(blamed)
	sort.Strings(partial)
	if len(blamed) > 0 {
		t.Errorf("%d failed statement(s) were answered as a failure of something other than the panel:\n%s", len(blamed), strings.Join(blamed, "\n"))
	}
	if len(misreported) > 0 {
		t.Errorf("%d failed statement(s) were not answered as a failure:\n%s", len(misreported), strings.Join(misreported, "\n"))
	}
	if len(partial) > 0 {
		t.Errorf("%d failure(s) left a change stored without saying so:\n%s", len(partial), strings.Join(partial, "\n"))
	}
	t.Logf("%d calls, %d statement failures checked", len(calls), checked)
}
