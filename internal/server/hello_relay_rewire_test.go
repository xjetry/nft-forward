package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
	"nft-forward/internal/wsproto"
)

// recordingAgent connects a fake agent for the node with the given hello
// fields, acks every ruleset it is pushed and hands each one to the returned
// channel. hangUp closes its connection.
func recordingAgent(t *testing.T, e *pushEnv, n *db.Node, hello wsproto.Hello) (applied <-chan []nft.Rule, hangUp func()) {
	t.Helper()
	c, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(e.srv.URL, "http")+"/v1/agents", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(websocket.StatusNormalClosure, "") })
	hello.NodeToken, hello.AgentVersion, hello.OS, hello.Arch = "tok-"+n.Name, "v0.0.1", "linux", "amd64"
	payload, _ := json.Marshal(hello)
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: payload})
	var ack wsproto.HelloAck
	if env := recvEnvelope(t, c); json.Unmarshal(env.Payload, &ack) != nil || ack.Error != "" || ack.NodeID != n.ID {
		t.Fatalf("hello for %s rejected: %+v", n.Name, ack)
	}
	rulesets := make(chan []nft.Rule, 16)
	go func() {
		for {
			_, b, err := c.Read(context.Background())
			if err != nil {
				return
			}
			var env wsproto.Envelope
			if json.Unmarshal(b, &env) != nil || env.Type != wsproto.TypeApplyRuleset {
				continue
			}
			var apply wsproto.ApplyRuleset
			_ = json.Unmarshal(env.Payload, &apply)
			p, _ := json.Marshal(wsproto.ApplyAck{Rev: apply.Rev, OK: true})
			out, _ := json.Marshal(wsproto.Envelope{Type: wsproto.TypeApplyAck, ID: env.ID, Payload: p})
			_ = c.Write(context.Background(), websocket.MessageText, out)
			rulesets <- apply.Rules
		}
	}()
	return rulesets, func() { c.Close(websocket.StatusNormalClosure, "") }
}

// pushedTarget waits for a ruleset pushed to a node in which the rule
// listening on port forwards to host, and fails if none comes.
func pushedTarget(t *testing.T, applied <-chan []nft.Rule, port int, host, what string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	var last string
	for {
		select {
		case rules := <-applied:
			last = "no rule on that port"
			for _, r := range rules {
				if r.SrcPort != port {
					continue
				}
				if r.DestIP == host || r.DestHost == host {
					return
				}
				last = fmt.Sprintf("-> %s%s:%d", r.DestIP, r.DestHost, r.DestPort)
			}
		case <-deadline:
			t.Fatalf("%s: no ruleset forwarding port %d to %s was pushed (last pushed: %s)", what, port, host, last)
		}
	}
}

// TestHelloThatMovesANodeRepointsTheHopBeforeIt follows a declared relay
// address from the hello that changes it to the kernel of the node one hop
// up: in a rule entry -> exit, the entry's hop forwards to the exit node's
// relay address, so when the exit node's daemon comes back declaring another
// address, the entry has to be pushed a ruleset that forwards there.
func TestHelloThatMovesANodeRepointsTheHopBeforeIt(t *testing.T) {
	e := newPushEnv(t)
	entry, exit := e.offlineNode(t), e.offlineNode(t)
	if err := db.UpdateNodeRelayHost(e.d, entry.ID, "10.0.0.1"); err != nil {
		t.Fatal(err)
	}
	entryApplied, _ := recordingAgent(t, e, entry, wsproto.Hello{})
	_, exitHangUp := recordingAgent(t, e, exit, wsproto.Hello{DeclaredRelayHost: "192.0.2.10"})

	body, _ := json.Marshal(map[string]any{
		"node_id": entry.ID, "name": "moved", "proto": "tcp", "exit": "203.0.113.9:9000",
		"hops": []map[string]any{{"node_id": entry.ID, "mode": "kernel"}, {"node_id": exit.ID, "mode": "kernel"}},
	})
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/api/rules", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(e.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reply, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var created struct {
		Rule struct {
			ID              int64 `json:"id"`
			EntryListenPort int   `json:"entry_listen_port"`
		} `json:"rule"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(reply, &created) != nil || created.Rule.ID == 0 {
		t.Fatalf("create the rule: %d %s", resp.StatusCode, reply)
	}
	port := created.Rule.EntryListenPort
	pushedTarget(t, entryApplied, port, "192.0.2.10", "after the rule is created")

	// The exit node's daemon restarts with another --relay-host.
	exitHangUp()
	recordingAgent(t, e, exit, wsproto.Hello{DeclaredRelayHost: "192.0.2.20"})

	pushedTarget(t, entryApplied, port, "192.0.2.20", "after the exit node declared 192.0.2.20")
	hops, err := db.ListRuleHops(e.d, created.Rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 2 || hops[0].TargetHost != "192.0.2.20" || hops[0].ListenPort != port {
		t.Errorf("the stored hops are %+v; want the entry's hop to listen on %d and target 192.0.2.20", hops, port)
	}
	if n, err := db.GetNode(e.d, exit.ID); err != nil || n.RelayHost != "192.0.2.20" || !n.RelayHostDeclared {
		t.Errorf("the exit node is stored as %+v (%v); want relay host 192.0.2.20, declared", n, err)
	}
}
