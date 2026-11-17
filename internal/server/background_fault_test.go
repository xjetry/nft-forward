package server

import (
	"database/sql"
	"fmt"
	"log"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
)

// The panel's background work under statement faults: the periodic passes,
// the quota enforcement that follows a counters batch, and the re-push of
// nodes that follows a change. Nobody waits for the outcome of any of it, so
// a failure has no status code or ack to go into. What it must not do is
// leave a node running rules the panel no longer means it to run with
// nothing to show for it.
//
// Each piece is run on the test's goroutine, with one of its statements
// failing, against stand-ins for the nodes that remember the ruleset each
// was last pushed. Then it is run again with nothing failing, as the next
// tick or the next counters batch would run it. The failure has to leave
// its error somewhere a person can find it — the log, or the row of the node
// whose push it failed — and after the second run:
//
//   - what the piece is there to store is stored, as after a run in which
//     nothing failed;
//   - every node either runs the ruleset the panel would push it now, or is
//     known not to: its row carries the failed push (last_error), or the log
//     names it.
//
// The second run is what tells a failure that only postpones the work from
// one that strands it: a piece that stored its change before finding out
// which nodes to tell finds, the next time, nothing left to do.

// backgroundUnit is one piece of background work.
type backgroundUnit struct {
	name string
	// prepare puts the database in the state the piece acts on.
	prepare func(s *Server, seed faultSeed) error
	// nodesAlreadyFollow says the nodes already run what prepare's state
	// calls for (a user disabled long ago has no rules on any node). Without
	// it they run what the database called for before prepare, which is the
	// case when prepare stands for something that has just happened and
	// that the piece exists to act on.
	nodesAlreadyFollow bool
	run                func(s *Server, seed faultSeed)
	// facts are the stored facts the piece is there to bring about.
	facts func(d *sql.DB, seed faultSeed) (string, error)
}

func userFacts(d *sql.DB, seed faultSeed) (string, error) {
	var disabled int
	var reason string
	var used, resetAt int64
	err := d.QueryRow(`SELECT disabled, COALESCE(disable_reason,''), traffic_used_bytes, COALESCE(last_traffic_reset_at,0) FROM users WHERE id=?`, seed.userID).
		Scan(&disabled, &reason, &used, &resetAt)
	return fmt.Sprintf("disabled=%d reason=%q used=%d reset=%v", disabled, reason, used, resetAt != 0), err
}

func exitFacts(d *sql.DB, seed faultSeed) (string, error) {
	rows, err := d.Query(`SELECT host, port, present, quota_bytes, used_bytes FROM user_landing_exits WHERE user_id=? ORDER BY host, port`, seed.userID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var host string
		var port, present int
		var quota, used int64
		if err := rows.Scan(&host, &port, &present, &quota, &used); err != nil {
			return "", err
		}
		out = append(out, fmt.Sprintf("%s:%d present=%d quota=%d used=%d", host, port, present, quota, used))
	}
	return strings.Join(out, "; "), rows.Err()
}

func noFacts(*sql.DB, faultSeed) (string, error) { return "", nil }

func backgroundUnits() []backgroundUnit {
	exec := func(s *Server, q string, args ...any) error {
		_, err := s.DB.Exec(q, args...)
		return err
	}
	// The seeded rule exits to 9.9.9.9:443; a landing exit at that address
	// is what its quota state excludes the rule by.
	const exitHost, exitPort = "9.9.9.9", 443
	landingExit := func(s *Server, seed faultSeed, present int) error {
		return exec(s, `INSERT INTO user_landing_exits(user_id, host, port, name, protocol, uri, present, updated_at, quota_bytes, used_bytes)
			VALUES (?,?,?,?,?,?,?,?,?,?)`, seed.userID, exitHost, exitPort, "exit", "trojan", "trojan://x@9.9.9.9:443", present, 1, 100, 200)
	}
	return []backgroundUnit{
		{
			name: "re-push of the nodes of a changed rule",
			// 改掉每一跳的目标端口：两台节点该跑的规则集都变了，而没有谁重推过。
			prepare: func(s *Server, seed faultSeed) error {
				return exec(s, `UPDATE rule_hops SET target_port = target_port + 1 WHERE rule_id=?`, seed.ruleID)
			},
			run:   func(s *Server, seed faultSeed) { s.redispatchNodes([]int64{seed.nodeID, seed.viaID}) },
			facts: noFacts,
		},
		{
			name: "a user reaches the traffic quota",
			prepare: func(s *Server, seed faultSeed) error {
				return exec(s, `UPDATE users SET traffic_quota_bytes=100, traffic_used_bytes=200 WHERE id=?`, seed.userID)
			},
			run:   func(s *Server, seed faultSeed) { s.enforceUserQuota(seed.userID) },
			facts: userFacts,
		},
		{
			name: "a grant reaches its per-node quota",
			prepare: func(s *Server, seed faultSeed) error {
				if err := db.GrantNode(s.DB, seed.userID, seed.nodeID, 5, 0); err != nil {
					return err
				}
				return exec(s, `UPDATE user_nodes SET traffic_quota_bytes=100, traffic_used_bytes=200 WHERE user_id=? AND node_id=?`, seed.userID, seed.nodeID)
			},
			run:   func(s *Server, seed faultSeed) { s.enforcePerNodeQuota(seed.userID) },
			facts: noFacts,
		},
		{
			name:    "a landing exit reaches its quota",
			prepare: func(s *Server, seed faultSeed) error { return landingExit(s, seed, 1) },
			run:     func(s *Server, seed faultSeed) { s.enforceExitQuota(seed.userID) },
			facts:   exitFacts,
		},
		{
			name: "a user's expiry passes",
			prepare: func(s *Server, seed faultSeed) error {
				return exec(s, `UPDATE users SET expires_at=1 WHERE id=?`, seed.userID)
			},
			run:   func(s *Server, seed faultSeed) { s.expiryPass() },
			facts: noFacts,
		},
		{
			// 配额用尽不停用账号，只在下发时把他的规则排除出去：节点此刻跑的是不含他的
			// 规则的规则集（nodesAlreadyFollow），周期翻转后要靠重推才恢复。
			name: "the traffic cycle of a user over quota rolls over",
			prepare: func(s *Server, seed faultSeed) error {
				return exec(s, `UPDATE users SET traffic_reset_days=30, created_at=?, last_traffic_reset_at=0,
					traffic_quota_bytes=100, traffic_used_bytes=200 WHERE id=?`,
					time.Now().Add(-40*24*time.Hour).Unix(), seed.userID)
			},
			nodesAlreadyFollow: true,
			run:                func(s *Server, seed faultSeed) { s.cycleResetPass() },
			facts:              userFacts,
		},
		{
			name: "a landing exit over its quota comes back into the user's landing set",
			prepare: func(s *Server, seed faultSeed) error {
				if err := landingExit(s, seed, 0); err != nil {
					return err
				}
				return exec(s, `UPDATE users SET landing_uris=? WHERE id=?`, "trojan://x@9.9.9.9:443", seed.userID)
			},
			run:   func(s *Server, seed faultSeed) { s.landingSyncPass(true) },
			facts: exitFacts,
		},
	}
}

// backgroundRun is what one run of a unit left behind.
type backgroundRun struct {
	stmts  int
	failed string
	logged string
	// onRecord says the failed statement's error is in the log or in a
	// node's last_error.
	onRecord bool
	pushes   int // rulesets delivered to the stand-in nodes
	// stale lists the nodes that do not run the ruleset the panel would push
	// them now.
	stale []int64
	facts string
	// After the unit is run once more, with nothing failing: the stored
	// facts, and the nodes that are stale with nothing on record to say so.
	silentAfterRetry []string
	factsAfterRetry  string
}

var loggedNode = regexp.MustCompile(`node (\d+)`)

func runBackgroundUnit(seed faultSeed, path string, u backgroundUnit, fault faultAt) (backgroundRun, error) {
	s, script, release, err := openFaulted(seed, path)
	if err != nil {
		return backgroundRun{}, err
	}
	defer release()
	s.pushInline = true

	// The stand-ins for the nodes: each remembers the rev it was last pushed.
	running := map[int64]string{}
	pushes := 0
	// 本机节点与远程节点走同一条下发路径，一个替身就都接住了。
	s.pushRuleset = func(nodeID int64, _ []nft.Rule, rev string) (string, error) {
		running[nodeID] = rev
		pushes++
		return "", nil
	}
	nodes, err := db.ListNodes(s.DB)
	if err != nil {
		return backgroundRun{}, err
	}
	wanted := func(nodeID int64) (string, error) {
		hops, err := db.ActiveRuleHopsForPush(s.DB, nodeID)
		if err != nil {
			return "", err
		}
		rules, err := buildRules(s.DB, hops)
		if err != nil {
			return "", err
		}
		return computeRev(rules), nil
	}
	follow := func() error {
		for _, n := range nodes {
			if n.NodeType == "composite" {
				continue
			}
			rev, err := wanted(n.ID)
			if err != nil {
				return err
			}
			running[n.ID] = rev
		}
		return nil
	}
	if !u.nodesAlreadyFollow {
		if err := follow(); err != nil {
			return backgroundRun{}, err
		}
	}
	if err := u.prepare(s, seed); err != nil {
		return backgroundRun{}, fmt.Errorf("prepare: %w", err)
	}
	if u.nodesAlreadyFollow {
		if err := follow(); err != nil {
			return backgroundRun{}, err
		}
	}

	// outOfStep lists the nodes not running what the panel would push them,
	// and among them those with nothing on record to say so.
	outOfStep := func(logged string) (stale []int64, silent []string, err error) {
		named := map[int64]bool{}
		for _, m := range loggedNode.FindAllStringSubmatch(logged, -1) {
			id, _ := strconv.ParseInt(m[1], 10, 64)
			named[id] = true
		}
		for _, n := range nodes {
			if n.NodeType == "composite" {
				continue
			}
			rev, err := wanted(n.ID)
			if err != nil {
				return nil, nil, err
			}
			if running[n.ID] == rev {
				continue
			}
			stale = append(stale, n.ID)
			row, err := db.GetNode(s.DB, n.ID)
			if err != nil {
				return nil, nil, err
			}
			if !(row.LastError.Valid && row.LastError.String != "") && !named[n.ID] {
				silent = append(silent, fmt.Sprintf("node %d (%s)", n.ID, n.Name))
			}
		}
		sort.Strings(silent)
		return stale, silent, nil
	}

	var logs strings.Builder
	prevOut := log.Writer()
	log.SetOutput(&logs)
	script.armAt(fault)
	u.run(s, seed)
	script.disarm()
	log.SetOutput(prevOut)

	run := backgroundRun{stmts: len(script.seen), failed: script.failed, logged: logs.String(), pushes: pushes}
	run.onRecord = strings.Contains(run.logged, errInjectedFault.Error())
	for _, n := range nodes {
		row, err := db.GetNode(s.DB, n.ID)
		if err != nil {
			return backgroundRun{}, err
		}
		if strings.Contains(row.LastError.String, errInjectedFault.Error()) {
			run.onRecord = true
		}
	}
	if run.stale, _, err = outOfStep(run.logged); err != nil {
		return backgroundRun{}, err
	}
	if run.facts, err = u.facts(s.DB, seed); err != nil {
		return backgroundRun{}, err
	}

	logs.Reset()
	log.SetOutput(&logs)
	u.run(s, seed)
	log.SetOutput(prevOut)
	// What the first run logged still counts: a node it named stays known.
	if _, run.silentAfterRetry, err = outOfStep(run.logged + logs.String()); err != nil {
		return backgroundRun{}, err
	}
	if run.factsAfterRetry, err = u.facts(s.DB, seed); err != nil {
		return backgroundRun{}, err
	}
	return run, nil
}

func TestBackgroundWorkLeavesNoNodeSilentlyOutOfStep(t *testing.T) {
	seed := seedFaultDB(t)
	dir := t.TempDir()
	serial := 0
	for _, u := range backgroundUnits() {
		run := func(fault faultAt) backgroundRun {
			t.Helper()
			serial++
			r, err := runBackgroundUnit(seed, filepath.Join(dir, fmt.Sprintf("background-%d.db", serial)), u, fault)
			if err != nil {
				t.Fatalf("%s, fault %+v: %v", u.name, fault, err)
			}
			return r
		}
		clean := run(faultAt{})
		if len(clean.stale) != 0 {
			t.Errorf("%s: with nothing failing, nodes %v do not run what the panel would push them", u.name, clean.stale)
			continue
		}
		// The scenario has to be one in which nodes are pushed: otherwise no
		// failure can leave one out of step, and the unit shows nothing.
		if clean.pushes == 0 {
			t.Errorf("%s: with nothing failing no node is pushed; the scenario asks nothing of the nodes", u.name)
			continue
		}

		checked, fetchFaults := 0, 0
		for _, fault := range everyFault(clean.stmts) {
			r := run(fault)
			if r.failed == "" {
				continue
			}
			checked++
			if fault.fetch > 0 {
				fetchFaults++
			}
			where := fmt.Sprintf("%s, failed statement %d: %s", u.name, fault.stmt, r.failed)
			if !r.onRecord {
				t.Errorf("%s\n    the error is neither in the log nor on a node's row; logged: %q", where, r.logged)
			}
			if r.factsAfterRetry != clean.facts {
				t.Errorf("%s\n    after the next run the stored facts are %q, want %q", where, r.factsAfterRetry, clean.facts)
			}
			if len(r.silentAfterRetry) > 0 {
				t.Errorf("%s\n    after the next run, out of step with nothing to show for it: %s\n    stored after the failed run: %s\n    logged: %s",
					where, strings.Join(r.silentAfterRetry, ", "), r.facts, strings.TrimSpace(r.logged))
			}
		}
		if checked < 3 {
			t.Errorf("%s: only %d failures were checked", u.name, checked)
		}
		if fetchFaults == 0 {
			t.Errorf("%s: no failed fetch was checked", u.name)
		}
	}
}
