package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
	"nft-forward/internal/wsproto"
)

// The node-facing side of the panel under the same statement faults as the
// HTTP side (statement_fault_test.go): what a node is told about a rule
// command it sent, what a counters batch records, and what a hello stores of
// the node's relay addresses. None of these has a status code to carry a
// failure; each has its own way of not passing one off as something else.

// nodeFrame is one frame a node sends.
type nodeFrame struct {
	name string
	// node is the node the frame comes from.
	node func(seed faultSeed) int64
	// prepare readies the database for the frame, with no fault armed, and
	// returns the frame's type and payload.
	prepare func(s *Server, seed faultSeed) (typ string, payload any, err error)
}

// frameRun is what one frame led to.
type frameRun struct {
	acks        []wsproto.RuleCmdAck // the rule-command acks sent back
	stmts       []string             // the statements the frame issued, in order
	failed      string               // the statement that was made to fail; "" if none was
	failedIn    string               // see statementScript.failedIn
	failedStack []string             // see statementScript.failedStack
	// before and after are snapshot's view of the database around the frame.
	before, after map[string]string
}

const frameID = "frame-1"

// runNodeFrame has the hub handle f on a fresh copy of the seeded database,
// failing the failAt-th statement the handling issues (0: none).
//
// Re-dispatch and quota enforcement are switched off: the hub hands both to
// other goroutines, whose statements the script does not count, and which
// would still be running when the database is closed.
func runNodeFrame(seed faultSeed, path string, f nodeFrame, failAt int, snapshot func(*sql.DB) (map[string]string, error)) (frameRun, error) {
	s, script, release, err := openFaulted(seed, path)
	if err != nil {
		return frameRun{}, err
	}
	defer release()
	s.Hub.Redispatch = func([]int64) {}
	s.Hub.OnTrafficUpdate = nil

	typ, payload, err := f.prepare(s, seed)
	if err != nil {
		return frameRun{}, fmt.Errorf("prepare: %w", err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return frameRun{}, err
	}
	run := frameRun{}
	if run.before, err = snapshot(s.DB); err != nil {
		return frameRun{}, err
	}
	ac := &agentConn{nodeID: f.node(seed), writeCh: make(chan []byte, 8), closed: make(chan struct{})}
	script.arm(failAt)
	s.Hub.handleFrame(ac, wsproto.Envelope{Type: typ, ID: frameID, Payload: raw})
	script.disarm()
	if run.after, err = snapshot(s.DB); err != nil {
		return frameRun{}, err
	}
	run.stmts, run.failed, run.failedIn, run.failedStack = script.seen, script.failed, script.failedIn, script.failedStack
	if run.acks, err = sentRuleAcks(ac); err != nil {
		return frameRun{}, err
	}
	return run, nil
}

// sentRuleAcks drains what the hub queued for the node, which has to be
// rule-command acks answering frameID and nothing else.
func sentRuleAcks(ac *agentConn) ([]wsproto.RuleCmdAck, error) {
	var acks []wsproto.RuleCmdAck
	for {
		select {
		case b := <-ac.writeCh:
			var env wsproto.Envelope
			if err := json.Unmarshal(b, &env); err != nil {
				return nil, err
			}
			if env.Type != wsproto.TypeRuleCmdAck || env.ID != frameID {
				return nil, fmt.Errorf("the hub sent a %q frame with id %q", env.Type, env.ID)
			}
			var ack wsproto.RuleCmdAck
			if err := json.Unmarshal(env.Payload, &ack); err != nil {
				return nil, err
			}
			acks = append(acks, ack)
		default:
			return acks, nil
		}
	}
}

func wholeDatabase(d *sql.DB) (map[string]string, error) {
	state, err := storedState(d)
	return map[string]string{"database": state}, err
}

// ownedNode makes the seeded user the owner of the seeded entry node: a node
// creates and edits rules on behalf of its owner.
func ownedNode(s *Server, seed faultSeed) error {
	_, err := s.DB.Exec(`UPDATE nodes SET owner_id=? WHERE id=?`, seed.userID, seed.nodeID)
	return err
}

func entryNode(seed faultSeed) int64 { return seed.nodeID }

// ruleCommandFrames are the commands a node can send about rules, each one
// accepted when nothing fails.
func ruleCommandFrames() []nodeFrame {
	return []nodeFrame{
		{name: "rule_create", node: entryNode, prepare: func(s *Server, seed faultSeed) (string, any, error) {
			return wsproto.TypeRuleCreate, wsproto.RuleCreate{Proto: "tcp", ExitHost: "203.0.113.9", ExitPort: 8443, Name: "from the node"}, ownedNode(s, seed)
		}},
		{name: "rule_update", node: entryNode, prepare: func(s *Server, seed faultSeed) (string, any, error) {
			if err := ownedNode(s, seed); err != nil {
				return "", nil, err
			}
			// Only a single-hop rule can be edited from its node, and the
			// seeded rule has two hops.
			created, _ := json.Marshal(wsproto.RuleCreate{Proto: "tcp", ExitHost: "203.0.113.9", ExitPort: 8443, Name: "to update"})
			ac := &agentConn{nodeID: seed.nodeID, writeCh: make(chan []byte, 8), closed: make(chan struct{})}
			s.Hub.handleFrame(ac, wsproto.Envelope{Type: wsproto.TypeRuleCreate, ID: frameID, Payload: created})
			if acks, err := sentRuleAcks(ac); err != nil || len(acks) != 1 || !acks[0].OK {
				return "", nil, fmt.Errorf("creating the rule to update: acks %+v, err %v", acks, err)
			}
			var id int64
			if err := s.DB.QueryRow(`SELECT id FROM rules WHERE name='to update'`).Scan(&id); err != nil {
				return "", nil, err
			}
			return wsproto.TypeRuleUpdate, wsproto.RuleUpdate{RuleID: id, Proto: "tcp", ExitHost: "203.0.113.10", ExitPort: 9443, Name: "updated"}, nil
		}},
		{name: "migrate_rules", node: entryNode, prepare: func(s *Server, seed faultSeed) (string, any, error) {
			return wsproto.TypeMigrateRules, wsproto.MigrateRules{Rules: []nft.Rule{
				{Proto: "tcp", DestIP: "203.0.113.11", DestPort: 80, Comment: "migrated 1"},
				{Proto: "tcp", DestHost: "exit.example.net", DestPort: 81, RuleName: "migrated 2"},
			}}, ownedNode(s, seed)
		}},
		{name: "rule_hop_edit", node: entryNode, prepare: func(s *Server, seed faultSeed) (string, any, error) {
			return wsproto.TypeRuleHopEdit, wsproto.RuleHopEdit{RuleID: seed.ruleID, Mode: "kernel", Comment: "edited at the node"}, nil
		}},
		{name: "rule_delete", node: entryNode, prepare: func(s *Server, seed faultSeed) (string, any, error) {
			return wsproto.TypeRuleDelete, wsproto.RuleDelete{RuleID: seed.ruleID}, nil
		}},
	}
}

// panelFailure is how a rule-command ack marks a failure that is the
// panel's, not the command's (ruleAckMessage).
const panelFailure = "面板内部错误"

// A node shows the ack's error to its operator as the reason the command was
// refused. So when a statement fails while the panel handles a rule command,
// the ack has to say the panel failed — not "no such rule" or "no such node",
// which a lookup that could not be made knows nothing about — and the command
// must not be half applied, because the node goes on to treat it as not
// applied at all.
func TestRuleCommandFromANodeIsAckedAsAPanelFailureWhenAStatementFails(t *testing.T) {
	seed := seedFaultDB(t)
	dir := t.TempDir()
	serial := 0
	run := func(f nodeFrame, failAt int) frameRun {
		t.Helper()
		serial++
		r, err := runNodeFrame(seed, filepath.Join(dir, fmt.Sprintf("frame-%d.db", serial)), f, failAt, wholeDatabase)
		if err != nil {
			t.Fatalf("%s, statement %d: %v", f.name, failAt, err)
		}
		return r
	}
	checked := 0
	for _, f := range ruleCommandFrames() {
		base := run(f, 0)
		if len(base.acks) != 1 || !base.acks[0].OK {
			t.Errorf("%s: with nothing failing the ack is %+v, want one that is OK", f.name, base.acks)
			continue
		}
		if base.before["database"] == base.after["database"] {
			t.Errorf("%s: with nothing failing the command stores nothing, so it cannot show a half-applied one", f.name)
			continue
		}
		for k := 1; k <= len(base.stmts); k++ {
			r := run(f, k)
			if r.failed == "" {
				continue // this run issued fewer statements than the fault-free one
			}
			checked++
			where := fmt.Sprintf("%s, failed statement %d: %s", f.name, k, r.failed)
			if len(r.acks) != 1 {
				t.Errorf("%s\n    the node was sent %d acks, want 1", where, len(r.acks))
				continue
			}
			ack := r.acks[0]
			if r.failedIn != "" {
				if ack != base.acks[0] {
					t.Errorf("%s\n    the statement belongs to %s, but the ack changed: %+v, want %+v", where, r.failedIn, ack, base.acks[0])
				}
				continue
			}
			if ack.OK || !strings.Contains(ack.Error, panelFailure) || !strings.Contains(ack.Error, errInjectedFault.Error()) {
				t.Errorf("%s\n    acked %+v, want a failure that says %q and carries the statement's error", where, ack, panelFailure)
			}
			if r.before["database"] != r.after["database"] {
				t.Errorf("%s\n    acked %+v, but part of the command is stored", where, ack)
			}
		}
	}
	if checked < 20 {
		t.Fatalf("only %d statement failures were checked; the commands are not reaching the database", checked)
	}
}

// The other direction: a command about something that is not there is told
// so in its own words, and is not dressed up as a panel failure.
func TestRuleCommandFromANodeNamesWhatIsMissing(t *testing.T) {
	seed := seedFaultDB(t)
	const noSuch = int64(987654)
	create := wsproto.RuleCreate{Proto: "tcp", ExitHost: "203.0.113.9", ExitPort: 8443, Name: "from the node"}
	for _, c := range []struct {
		name    string
		node    int64
		owned   bool
		typ     string
		payload any
		want    string
	}{
		{"create from a node that does not exist", noSuch, false, wsproto.TypeRuleCreate, create, "节点不存在"},
		{"create from a node nobody owns", seed.nodeID, false, wsproto.TypeRuleCreate, create, "节点无归属用户"},
		{"update of a rule that does not exist", seed.nodeID, true, wsproto.TypeRuleUpdate, wsproto.RuleUpdate{RuleID: noSuch, Proto: "tcp", ExitHost: "203.0.113.9", ExitPort: 1}, "规则不存在"},
		{"migrate from a node that does not exist", noSuch, false, wsproto.TypeMigrateRules, wsproto.MigrateRules{}, "节点不存在"},
		{"hop edit of a rule that does not exist", seed.nodeID, false, wsproto.TypeRuleHopEdit, wsproto.RuleHopEdit{RuleID: noSuch}, "规则不存在"},
		{"hop edit from a node the rule does not run on", seed.spareID, false, wsproto.TypeRuleHopEdit, wsproto.RuleHopEdit{RuleID: seed.ruleID}, "节点不在该规则上"},
		{"delete from a node the rule does not run on", seed.spareID, false, wsproto.TypeRuleDelete, wsproto.RuleDelete{RuleID: seed.ruleID}, "节点不在该规则上"},
	} {
		f := nodeFrame{name: c.name, node: func(faultSeed) int64 { return c.node }, prepare: func(s *Server, seed faultSeed) (string, any, error) {
			if c.owned {
				return c.typ, c.payload, ownedNode(s, seed)
			}
			return c.typ, c.payload, nil
		}}
		r, err := runNodeFrame(seed, filepath.Join(t.TempDir(), "frame.db"), f, 0, wholeDatabase)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(r.acks) != 1 || r.acks[0].OK || r.acks[0].Error != c.want {
			t.Errorf("%s: acked %+v, want the error %q", c.name, r.acks, c.want)
		}
		if r.before["database"] != r.after["database"] {
			t.Errorf("%s: a refused command changed the database", c.name)
		}
	}
}

// counterLedgers are the places a counters batch is recorded, each as the
// query that reads it. Timestamps are left out.
var counterLedgers = map[string]string{
	"rule_hops":          `SELECT id, last_bytes, last_bytes_up, last_bytes_down, total_bytes FROM rule_hops ORDER BY id`,
	"users":              `SELECT id, traffic_used_bytes FROM users ORDER BY id`,
	"user_nodes":         `SELECT user_id, node_id, traffic_used_bytes FROM user_nodes ORDER BY user_id, node_id`,
	"user_landing_exits": `SELECT user_id, host, port, used_bytes, used_bytes_up, used_bytes_down FROM user_landing_exits ORDER BY user_id, host, port`,
	"node_raw_traffic":   `SELECT node_id, raw_bytes FROM node_raw_traffic ORDER BY node_id`,
}

func readLedgers(d *sql.DB) (map[string]string, error) {
	out := map[string]string{}
	for name, query := range counterLedgers {
		rows, err := d.Query(query)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			return nil, err
		}
		var lines []string
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return nil, err
			}
			lines = append(lines, fmt.Sprintf("%q", vals))
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		out[name] = strings.Join(lines, "\n")
	}
	return out, nil
}

// countersFrame is a counters batch from node for its hop of the seeded
// rule. The amounts that go into a charge are all set away from their
// defaults, so that a charge computed without one of them comes out
// different: the entry node's multiplier, the owner's billing rate. The rule
// exits at the owner's landing exit, so the exit ledger is in play, and when
// resetDue the owner's traffic cycle has run out.
func countersFrame(name string, node func(faultSeed) int64, resetDue bool) nodeFrame {
	return nodeFrame{name: name, node: node, prepare: func(s *Server, seed faultSeed) (string, any, error) {
		for _, stmt := range []struct {
			query string
			args  []any
		}{
			{`UPDATE nodes SET rate_multiplier=2 WHERE id=?`, []any{seed.nodeID}},
			{`UPDATE users SET billing_rate=1.5, traffic_used_bytes=5000 WHERE id=?`, []any{seed.userID}},
			{`UPDATE user_nodes SET traffic_used_bytes=700 WHERE user_id=?`, []any{seed.userID}},
			{`UPDATE rules SET exit_host=?, exit_port=? WHERE id=?`, []any{faultExitHost, faultExitPort, seed.ruleID}},
		} {
			if _, err := s.DB.Exec(stmt.query, stmt.args...); err != nil {
				return "", nil, err
			}
		}
		if resetDue {
			// A 30-day cycle on an account opened 31 days ago and never reset.
			if _, err := s.DB.Exec(`UPDATE users SET traffic_reset_days=30, created_at=?, last_traffic_reset_at=0 WHERE id=?`,
				time.Now().Unix()-31*86400, seed.userID); err != nil {
				return "", nil, err
			}
		}
		var sample wsproto.CounterSample
		if err := s.DB.QueryRow(`SELECT proto, listen_port FROM rule_hops WHERE rule_id=? AND node_id=?`, seed.ruleID, node(seed)).
			Scan(&sample.Proto, &sample.ListenPort); err != nil {
			return "", nil, err
		}
		sample.BytesUp, sample.BytesDown = 1000, 3000
		return wsproto.TypeCounters, wsproto.Counters{Samples: []wsproto.CounterSample{sample}}, nil
	}}
}

// countersSurvives lists the loads in applyCounters whose failure does not
// drop the batch, with what the batch then goes without. A statement counts
// as one of theirs when the function is on the stack that issued it. Any
// other statement of a batch that fails drops the batch whole.
var countersSurvives = map[string]struct {
	// without is the ledger that does not get this batch's bytes; "" when
	// every ledger does.
	without string
	// unreset says the traffic-cycle reset that was due does not happen, and
	// the batch is recorded on top of the cycle's usage so far.
	unreset bool
}{
	"nft-forward/internal/db.SegmentFirstHops":          {without: "user_nodes"},
	"nft-forward/internal/db.PresentLandingExitSet":     {without: "user_landing_exits"},
	"nft-forward/internal/db.MaxHopPositions":           {without: "user_landing_exits"},
	"nft-forward/internal/db.CheckAndResetTrafficCycle": {unreset: true},
	// Lists the nodes to re-push after a reset that has already happened.
	"nft-forward/internal/db.DistinctUserNodes": {},
}

// A counters batch carries bytes that are reported once: what the panel does
// not record of it is never billed. So a statement that fails while a batch
// is handled may make the panel record less — the whole batch, or one ledger
// of it — but never a different amount: a charge computed with a stand-in
// for a value that could not be loaded (a multiplier, a billing rate, an
// owner) is wrong in a way no later batch corrects.
//
// For every statement of a batch, this pins which of the two it is.
func TestCountersBatchRecordsItsBytesInFullOrNotAtAllPerLedger(t *testing.T) {
	seed := seedFaultDB(t)
	dir := t.TempDir()
	serial := 0
	run := func(f nodeFrame, failAt int) frameRun {
		t.Helper()
		serial++
		r, err := runNodeFrame(seed, filepath.Join(dir, fmt.Sprintf("counters-%d.db", serial)), f, failAt, readLedgers)
		if err != nil {
			t.Fatalf("%s, statement %d: %v", f.name, failAt, err)
		}
		if len(r.acks) != 0 {
			t.Fatalf("%s: a counters batch was answered with %+v", f.name, r.acks)
		}
		return r
	}
	viaNode := func(seed faultSeed) int64 { return seed.viaID }
	entry := countersFrame("the entry hop's batch", entryNode, false)
	last := countersFrame("the last hop's batch", viaNode, false)
	entryResetDue := countersFrame("the entry hop's batch, with a cycle reset due", entryNode, true)

	// What each frame records when nothing fails, and that between them the
	// frames move every ledger: a ledger nothing moves cannot show a wrong
	// amount.
	full := map[string]frameRun{}
	moved := map[string]bool{}
	for _, f := range []nodeFrame{entry, last, entryResetDue} {
		full[f.name] = run(f, 0)
		for ledger := range counterLedgers {
			if full[f.name].before[ledger] != full[f.name].after[ledger] {
				moved[ledger] = true
			}
		}
	}
	for ledger := range counterLedgers {
		if !moved[ledger] {
			t.Errorf("no frame changes the %s ledger", ledger)
		}
	}
	if full[entryResetDue.name].after["users"] == full[entry.name].after["users"] {
		t.Errorf("the cycle reset is not happening in %q: the owner's usage ends up as without it", entryResetDue.name)
	}
	if t.Failed() {
		t.FailNow()
	}

	// exercised records, per countersSurvives entry, that one of its
	// statements was failed in a batch where the entry's outcome differs
	// from a dropped batch and, for an entry that costs the batch something,
	// from a full one as well.
	exercised := map[string]bool{}
	checked := 0
	check := func(f nodeFrame, only func(fn string) bool, unresetAs frameRun) {
		base := full[f.name]
		for k := 1; k <= len(base.stmts); k++ {
			r := run(f, k)
			if r.failed == "" {
				continue
			}
			fn := firstOf(r.failedStack, countersSurvives)
			if !only(fn) {
				continue
			}
			checked++
			want := map[string]string{}
			outcome := "the batch is dropped"
			for ledger := range counterLedgers {
				want[ledger] = r.before[ledger]
			}
			if fn != "" {
				survives := countersSurvives[fn]
				recorded := base.after
				outcome = "the batch is recorded in full"
				if survives.unreset {
					recorded = unresetAs.after
					outcome = "the batch is recorded on top of the usage so far, with no reset"
				}
				for ledger := range counterLedgers {
					want[ledger] = recorded[ledger]
				}
				if survives.without != "" {
					want[survives.without] = r.before[survives.without]
					outcome = "the batch is recorded in every ledger but " + survives.without
				}
				differs := func(a, b map[string]string) bool {
					for ledger := range counterLedgers {
						if a[ledger] != b[ledger] {
							return true
						}
					}
					return false
				}
				costsSomething := survives.without != "" || survives.unreset
				if differs(want, r.before) && (!costsSomething || differs(want, base.after)) {
					exercised[fn] = true
				}
			}
			var wrong []string
			for ledger := range counterLedgers {
				if r.after[ledger] != want[ledger] {
					wrong = append(wrong, fmt.Sprintf("    %s is\n      %s\n    want\n      %s",
						ledger, strings.ReplaceAll(r.after[ledger], "\n", "\n      "), strings.ReplaceAll(want[ledger], "\n", "\n      ")))
				}
			}
			if len(wrong) > 0 {
				sort.Strings(wrong)
				t.Errorf("%s, failed statement %d: %s\n    (issued by %q) expected outcome: %s\n%s", f.name, k, r.failed, fn, outcome, strings.Join(wrong, "\n"))
			}
		}
	}
	all := func(string) bool { return true }
	check(entry, all, frameRun{})
	check(last, all, frameRun{})
	// With a reset in the picture, only the statements that belong to the
	// reset are new; the others are the ones the first frame already fails,
	// and their outcome here is the same one shifted by the reset.
	check(entryResetDue, func(fn string) bool {
		return fn == "nft-forward/internal/db.CheckAndResetTrafficCycle" || fn == "nft-forward/internal/db.DistinctUserNodes"
	}, full[entry.name])

	for fn := range countersSurvives {
		if !exercised[fn] {
			t.Errorf("no statement of %s was failed in a batch where its outcome can be told apart; its entry in countersSurvives is not being checked", fn)
		}
	}
	if checked < 20 {
		t.Fatalf("only %d statement failures were checked", checked)
	}
}

// relayFields are the columns a hello can write of a node's relay addresses.
type relayFields struct {
	host, hostV6         string
	declared, declaredV6 bool
}

func relayFieldsOf(n *db.Node) relayFields {
	return relayFields{n.RelayHost, n.RelayHostV6, n.RelayHostDeclared, n.RelayHostV6Declared}
}

// A hello's relay-address writes are separate statements, decided from the
// node row as loaded for that hello. When one of them fails:
//
//   - the failure is returned, so that it is logged as one;
//   - the in-memory node is never ahead of the stored row — a field moves
//     only to the value that was stored — since the next step decides from it;
//   - the next hello, starting from the row as stored, finishes the job: the
//     row ends up as it would have with nothing failing.
func TestHelloRelayHostWritesStopAtAFailedWriteAndTheNextHelloFinishes(t *testing.T) {
	seed := seedFaultDB(t)
	dir := t.TempDir()
	const connectV4 = "198.51.100.77"
	scenarios := []struct {
		name    string
		prepare string // SQL run against the spare node's row first; "" for none
		hello   wsproto.Hello
		want    relayFields
	}{
		{
			name:  "the daemon declares both addresses",
			hello: wsproto.Hello{DeclaredRelayHost: "192.0.2.10", DeclaredRelayHostV6: "2001:db8::10"},
			want:  relayFields{"192.0.2.10", "2001:db8::10", true, true},
		},
		{
			name:    "the daemon stops declaring",
			prepare: `UPDATE nodes SET relay_host_declared=1, relay_host_v6='2001:db8::10', relay_host_v6_declared=1 WHERE id=?`,
			want:    relayFields{"10.0.0.12", "2001:db8::10", false, false},
		},
		{
			name:    "both addresses are empty and get seeded",
			prepare: `UPDATE nodes SET relay_host='', relay_host_v6='' WHERE id=?`,
			hello:   wsproto.Hello{ProbedV6: "2001:db8::20"},
			want:    relayFields{connectV4, "2001:db8::20", false, false},
		},
		{
			name:    "an IPv6 address sits in the IPv4 field",
			prepare: `UPDATE nodes SET relay_host='2001:db8::30', relay_host_v6='' WHERE id=?`,
			want:    relayFields{connectV4, "2001:db8::30", false, false},
		},
	}
	serial := 0
	checked := 0
	for _, sc := range scenarios {
		// attempt runs the hello's writes with the failAt-th statement failing
		// and then, on the same database, a second hello with nothing failing.
		attempt := func(failAt int) (stmts int, failed string, err error, initial, inMemory, stored, afterSecond relayFields) {
			t.Helper()
			serial++
			s, script, release, oerr := openFaulted(seed, filepath.Join(dir, fmt.Sprintf("hello-%d.db", serial)))
			if oerr != nil {
				t.Fatal(oerr)
			}
			defer release()
			load := func() *db.Node {
				t.Helper()
				n, lerr := db.GetNode(s.DB, seed.spareID)
				if lerr != nil {
					t.Fatal(lerr)
				}
				return n
			}
			if sc.prepare != "" {
				if _, perr := s.DB.Exec(sc.prepare, seed.spareID); perr != nil {
					t.Fatal(perr)
				}
			}
			node := load()
			initial = relayFieldsOf(node)
			script.arm(failAt)
			err = storeHelloRelayHosts(s.DB, node, connectV4, sc.hello)
			script.disarm()
			stmts, failed = len(script.seen), script.failed
			inMemory, stored = relayFieldsOf(node), relayFieldsOf(load())
			if serr := storeHelloRelayHosts(s.DB, load(), connectV4, sc.hello); serr != nil {
				t.Fatalf("%s: the second hello, with nothing failing: %v", sc.name, serr)
			}
			afterSecond = relayFieldsOf(load())
			return
		}
		stmts, _, err, _, _, stored, _ := attempt(0)
		if err != nil {
			t.Errorf("%s: with nothing failing: %v", sc.name, err)
			continue
		}
		if stored != sc.want {
			t.Errorf("%s: with nothing failing the row is %+v, want %+v", sc.name, stored, sc.want)
			continue
		}
		if stmts == 0 {
			t.Errorf("%s: the hello writes nothing, so there is no write to fail", sc.name)
			continue
		}
		for k := 1; k <= stmts; k++ {
			_, failed, err, initial, inMemory, stored, afterSecond := attempt(k)
			checked++
			where := fmt.Sprintf("%s, failed statement %d: %s", sc.name, k, failed)
			if !errors.Is(err, errInjectedFault) {
				t.Errorf("%s\n    returned %v, want the statement's error", where, err)
			}
			for _, f := range []struct {
				name               string
				was, memory, saved any
			}{
				{"relay_host", initial.host, inMemory.host, stored.host},
				{"relay_host_v6", initial.hostV6, inMemory.hostV6, stored.hostV6},
				{"relay_host_declared", initial.declared, inMemory.declared, stored.declared},
				{"relay_host_v6_declared", initial.declaredV6, inMemory.declaredV6, stored.declaredV6},
			} {
				if f.memory != f.was && f.memory != f.saved {
					t.Errorf("%s\n    %s is %v in memory, but was %v and is stored as %v", where, f.name, f.memory, f.was, f.saved)
				}
			}
			if afterSecond != sc.want {
				t.Errorf("%s\n    after the next hello the row is %+v, want %+v", where, afterSecond, sc.want)
			}
		}
	}
	if checked < 8 {
		t.Fatalf("only %d failed writes were checked", checked)
	}
}

// A hello that changes a node's relay address is followed by the rules that
// run into the node being re-generated, so that the hop before the node's
// targets the new address (settleHelloRelayHosts). With any one statement of
// that failing:
//
//   - every rule is whole: as it was before the hello, or as the hello leaves
//     it with nothing failing — never with some of its hops gone;
//   - the failure is logged, naming the node;
//   - the nodes of every rule that was re-generated are handed to
//     Redispatch, so no node is left running a hop the panel replaced;
//   - the next hello, with nothing failing, finishes: the address is the
//     declared one and every rule targets it.
func TestHelloThatMovesANodeTakesItsRulesAlongOrLeavesEachWhole(t *testing.T) {
	seed := seedFaultDB(t)
	dir := t.TempDir()
	const declared = "192.0.2.50"
	hello := wsproto.Hello{DeclaredRelayHost: declared}

	type outcome struct {
		stmts  int
		failed string
		logged string
		// Per rule: its hops before the hello, after it, and as the hello
		// leaves them with nothing failing. A re-generated rule keeps the
		// ports its hops listen on, so the last is the first with the old
		// address replaced by the declared one. (The second rule's ports are
		// picked at random in every attempt, so there is no one expected
		// value across attempts.)
		before, hops, want map[int64][]string
		storedHost         string // the node's relay address after the hello
		redispatch         []int64
		// after a second hello with nothing failing
		finalHost  string
		finalHops  map[int64][]string
		finalStale []int64
	}
	var rules []int64
	serial := 0
	attempt := func(failAt int) outcome {
		t.Helper()
		serial++
		s, script, release, err := openFaulted(seed, filepath.Join(dir, fmt.Sprintf("move-%d.db", serial)))
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		s.Hub.OnTrafficUpdate = nil
		redispatched := make(chan []int64, 4)
		s.Hub.Redispatch = func(nodes []int64) { redispatched <- nodes }

		// A second rule over the same two nodes, so that one rule can be done
		// and the other not.
		second := &db.Rule{NodeID: seed.nodeID, Name: "wire-2", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 8443}
		tx, err := s.DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if second.ID, err = db.CreateRule(tx, second); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := db.RegenerateRule(tx, second, []db.HopInput{{NodeID: seed.nodeID}, {NodeID: seed.viaID}}, nil); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		rules = []int64{seed.ruleID, second.ID}
		hopsNow := func() map[int64][]string {
			t.Helper()
			out := map[int64][]string{}
			for _, id := range rules {
				hops, err := db.ListRuleHops(s.DB, id)
				if err != nil {
					t.Fatal(err)
				}
				for _, h := range hops {
					out[id] = append(out[id], fmt.Sprintf("#%d node %d :%d -> %s:%d", h.Position, h.NodeID, h.ListenPort, h.TargetHost, h.TargetPort))
				}
			}
			return out
		}
		node, err := db.GetNode(s.DB, seed.viaID)
		if err != nil {
			t.Fatal(err)
		}
		before := hopsNow()
		want := map[int64][]string{}
		for id, lines := range before {
			for _, line := range lines {
				want[id] = append(want[id], strings.Replace(line, "-> "+node.RelayHost+":", "-> "+declared+":", 1))
			}
		}

		var logs strings.Builder
		prevOut := log.Writer()
		log.SetOutput(&logs)
		script.arm(failAt)
		s.Hub.settleHelloRelayHosts(node, "198.51.100.77", hello)
		script.disarm()
		log.SetOutput(prevOut)

		o := outcome{stmts: len(script.seen), failed: script.failed, logged: logs.String(), before: before, hops: hopsNow(), want: want}
		// Redispatch is started on another goroutine when there is something
		// to re-push; whether there is, is known from the hops.
		regenerated := false
		for _, id := range rules {
			if !reflect.DeepEqual(o.hops[id], before[id]) {
				regenerated = true
			}
		}
		if regenerated {
			select {
			case o.redispatch = <-redispatched:
			case <-time.After(5 * time.Second):
				t.Fatalf("failAt %d: rules were re-generated but nothing was handed to Redispatch", failAt)
			}
		}

		reloaded, err := db.GetNode(s.DB, seed.viaID)
		if err != nil {
			t.Fatal(err)
		}
		o.storedHost = reloaded.RelayHost
		s.Hub.settleHelloRelayHosts(reloaded, "198.51.100.77", hello)
		final, err := db.GetNode(s.DB, seed.viaID)
		if err != nil {
			t.Fatal(err)
		}
		o.finalHost, o.finalHops = final.RelayHost, hopsNow()
		if o.finalStale, err = db.StaleRulesIntoNode(s.DB, seed.viaID); err != nil {
			t.Fatal(err)
		}
		return o
	}

	clean := attempt(0)
	if clean.logged != "" {
		t.Fatalf("with nothing failing the hello logs: %s", clean.logged)
	}
	for _, id := range rules {
		if len(clean.hops[id]) != 2 || !strings.Contains(clean.hops[id][0], "-> "+declared+":") {
			t.Fatalf("with nothing failing, rule %d is %v; want its first hop to target %s", id, clean.hops[id], declared)
		}
		if !reflect.DeepEqual(clean.hops[id], clean.want[id]) || reflect.DeepEqual(clean.want[id], clean.before[id]) {
			t.Fatalf("with nothing failing, rule %d is %v; before the hello it was %v, and re-generated it should be %v", id, clean.hops[id], clean.before[id], clean.want[id])
		}
	}
	if got := append([]int64{}, clean.redispatch...); len(got) != 2 {
		t.Fatalf("with nothing failing, Redispatch got %v; want the two nodes of the rules", got)
	}
	if clean.stmts < 10 {
		t.Fatalf("the hello issued only %d statements; the rules are not being re-generated", clean.stmts)
	}

	partial := 0
	for k := 1; k <= clean.stmts; k++ {
		o := attempt(k)
		if o.failed == "" {
			continue // this run issued fewer statements than the fault-free one
		}
		where := fmt.Sprintf("failed statement %d: %s", k, o.failed)
		before, want := o.before, o.want
		done := 0
		for _, id := range rules {
			switch {
			case reflect.DeepEqual(o.hops[id], before[id]):
			case reflect.DeepEqual(o.hops[id], want[id]):
				done++
			default:
				t.Errorf("%s\n    rule %d is neither as it was nor re-generated:\n    %v\n    was %v\n    re-generated it is %v", where, id, o.hops[id], before[id], want[id])
			}
		}
		if done == 1 {
			partial++
		}
		// The rules follow the address that is stored, whatever became of the
		// rest of the store step: a failure in writing the declared flag,
		// after the address, does not leave the rules behind.
		if strings.HasPrefix(o.failed, "UPDATE nodes SET") && o.storedHost == declared && done != len(rules) {
			t.Errorf("%s\n    the address is stored and no statement of the re-generation failed, but only %d of %d rules were re-generated", where, done, len(rules))
		}
		if !strings.Contains(o.logged, fmt.Sprintf("hub: node %d:", seed.viaID)) {
			t.Errorf("%s\n    is not logged against the node; logged: %q", where, o.logged)
		}
		if done > 0 {
			got := append([]int64{}, o.redispatch...)
			sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
			wantNodes := []int64{seed.nodeID, seed.viaID}
			sort.Slice(wantNodes, func(i, j int) bool { return wantNodes[i] < wantNodes[j] })
			if !reflect.DeepEqual(got, wantNodes) {
				t.Errorf("%s\n    %d rule(s) were re-generated but Redispatch got %v, want %v", where, done, got, wantNodes)
			}
		}
		if o.finalHost != declared || len(o.finalStale) != 0 || !reflect.DeepEqual(o.finalHops, want) {
			t.Errorf("%s\n    after the next hello: address %s, stale rules %v, hops %v\n    want address %s, none stale, hops %v",
				where, o.finalHost, o.finalStale, o.finalHops, declared, want)
		}
	}
	// A failure inside the second rule's re-generation leaves the first done
	// and the second as it was; if that never happens here, the rules are not
	// being re-generated one transaction each and the test is not looking at
	// what it describes.
	if partial == 0 {
		t.Error("no failed statement left one rule re-generated and the other as it was")
	}
}

// TestHelloThatChangesNothingStillBringsStaleRulesInLine: whether a rule is
// stale is read from the stored rows, not inferred from what the hello
// changed. So a database in which the node's address was written without its
// rules being re-generated — the hello stores nothing new there — is brought
// in line by the node's next hello all the same.
func TestHelloThatChangesNothingStillBringsStaleRulesInLine(t *testing.T) {
	d := openDB(t)
	s, err := newServer(d)
	if err != nil {
		t.Fatal(err)
	}
	rule, entry, via := seedTwoHopRuleDB(t, d)
	const declared = "192.0.2.60"
	if _, err := d.Exec(`UPDATE nodes SET relay_host=?, relay_host_declared=1 WHERE id=?`, declared, via); err != nil {
		t.Fatal(err)
	}
	redispatched := make(chan []int64, 2)
	s.Hub.Redispatch = func(nodes []int64) { redispatched <- nodes }

	node, err := db.GetNode(d, via)
	if err != nil {
		t.Fatal(err)
	}
	s.Hub.settleHelloRelayHosts(node, "198.51.100.77", wsproto.Hello{DeclaredRelayHost: declared})

	hops, err := db.ListRuleHops(d, rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 2 || hops[0].TargetHost != declared {
		t.Fatalf("the hop before the node targets %s, want the node's stored address %s", hops[0].TargetHost, declared)
	}
	select {
	case nodes := <-redispatched:
		sort.Slice(nodes, func(i, j int) bool { return nodes[i] < nodes[j] })
		want := []int64{entry, via}
		sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
		if !reflect.DeepEqual(nodes, want) {
			t.Errorf("Redispatch got %v, want %v", nodes, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the rule was re-generated but its nodes were not handed to Redispatch")
	}

	// And with every rule in line, a hello re-pushes nothing on this account.
	s.Hub.settleHelloRelayHosts(node, "198.51.100.77", wsproto.Hello{DeclaredRelayHost: declared})
	select {
	case nodes := <-redispatched:
		t.Errorf("a hello with no stale rule handed %v to Redispatch", nodes)
	case <-time.After(300 * time.Millisecond):
	}
}
