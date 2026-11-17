package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
	"nft-forward/internal/wsproto"
)

const (
	hubWriteTimeout = 10 * time.Second
	hubReadTimeout  = 30 * time.Second
)

// applyAckTimeout and upgradeAckTimeout are how long a push waits for the
// node's ack before it is given up as unanswered. They are variables so that
// a test of that branch can shorten them instead of waiting them out.
var (
	applyAckTimeout   = 30 * time.Second
	upgradeAckTimeout = 60 * time.Second
)

type Hub struct {
	DB *sql.DB

	// OnTrafficUpdate, when set, is invoked once per (user, node) pair whose
	// usage was advanced by a counters batch. The Hub stays a pure transport:
	// it knows how to accumulate bytes but delegates quota policy (and the
	// re-dispatch it may trigger) to the owner that wires this callback.
	OnTrafficUpdate func(userID int64, nodeID int64)

	// Redispatch re-pushes kernel state to a set of nodes after the hub
	// mutates rule state on their behalf. Keeps the hub transport-only.
	Redispatch func(nodeIDs []int64)

	mu         sync.RWMutex
	conns      map[int64]*agentConn
	speedCache *speedCache
}

func NewHub(d *sql.DB) *Hub {
	return &Hub{DB: d, conns: make(map[int64]*agentConn), speedCache: newSpeedCache()}
}

type agentConn struct {
	nodeID int64
	// arch 是这条连接的 hello 里上报的 GOARCH，构造后不再改写（重连会换一个新的
	// agentConn），所以读它不需要锁。升级推送据此挑选对应架构的 nft-agent。
	arch    string
	ws      *websocket.Conn
	writeCh chan []byte
	closed  chan struct{}

	// closeOnce guards closed so the multiple close paths (a displaced
	// conn in registerConn, unregisterConn on disconnect, and Hub.Close
	// on shutdown) can race without double-closing the channel.
	closeOnce sync.Once

	pendMu  sync.Mutex
	pending map[string]chan json.RawMessage

	idSeq atomic.Uint64
}

func (a *agentConn) nextID() string {
	return strconv.FormatUint(a.idSeq.Add(1), 36)
}

// signalClose closes ac.closed exactly once, signalling the reader and
// writer loops (and any pending SendApplyRuleset) to stop.
func (a *agentConn) signalClose() {
	a.closeOnce.Do(func() { close(a.closed) })
}

func (h *Hub) IsOnline(nodeID int64) bool {
	h.mu.RLock()
	_, ok := h.conns[nodeID]
	h.mu.RUnlock()
	return ok
}

// NodeArch 返回节点当前连接在 hello 里上报的 GOARCH；节点未连接时 ok=false。
func (h *Hub) NodeArch(nodeID int64) (arch string, ok bool) {
	h.mu.RLock()
	ac, ok := h.conns[nodeID]
	h.mu.RUnlock()
	if !ok {
		return "", false
	}
	return ac.arch, true
}

// ServeWS handles the /v1/agents WS endpoint. Upgrades the request,
// reads the mandatory hello frame, validates the bearer token against
// nodes.secret, registers the conn, and loops on reads dispatching by
// message type.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // we authenticate via bearer in hello
	})
	if err != nil {
		log.Printf("hub: accept: %v", err)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	helloEnv, err := readEnvelope(ctx, ws, hubReadTimeout)
	if err != nil || helloEnv.Type != wsproto.TypeHello {
		writeError(ctx, ws, "protocol", "expected hello as first frame")
		ws.Close(websocket.StatusPolicyViolation, "no hello")
		return
	}
	var hello wsproto.Hello
	if err := json.Unmarshal(helloEnv.Payload, &hello); err != nil {
		writeError(ctx, ws, "protocol", "malformed hello payload")
		ws.Close(websocket.StatusPolicyViolation, "bad hello")
		return
	}

	node, err := lookupNodeBySecret(h.DB, hello.NodeToken)
	if err != nil || node == nil {
		ack, _ := json.Marshal(wsproto.HelloAck{Error: "unknown or revoked token"})
		writeEnvelope(ctx, ws, wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: helloEnv.ID, Payload: ack})
		ws.Close(websocket.StatusPolicyViolation, "bad token")
		return
	}

	// Register before hello_ack so both sides can rely on the invariant
	// "hello_ack visible => conn is in the hub map".
	ac := &agentConn{
		nodeID:  node.ID,
		arch:    hello.Arch,
		ws:      ws,
		writeCh: make(chan []byte, 16),
		closed:  make(chan struct{}),
		pending: make(map[string]chan json.RawMessage),
	}
	h.registerConn(ac)
	defer h.unregisterConn(ac)

	// The agent applies whatever pool size the ack carries and keeps its own
	// when the ack carries none (daemon.Dialer). So when the setting cannot be
	// read, none is sent: a default sent in its place would overwrite the
	// size the agent was configured with.
	var poolSize *int
	if psStr, err := db.GetSetting(h.DB, "pool_size"); err != nil {
		log.Printf("hub: node %d: read pool_size, leaving the agent's as it is: %v", node.ID, err)
	} else {
		n := 4
		if v, err := strconv.Atoi(psStr); err == nil && v >= 0 {
			n = v
		}
		poolSize = &n
	}
	ackPayload, _ := json.Marshal(wsproto.HelloAck{NodeID: node.ID, Name: node.Name, PoolSize: poolSize})
	if err := writeEnvelope(ctx, ws, wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: helloEnv.ID, Payload: ackPayload}); err != nil {
		ws.Close(websocket.StatusInternalError, "ack write failed")
		return
	}

	connectIP := extractIP(r)
	if err := db.MarkNodeOnline(h.DB, node.ID, hello.AgentVersion, hello.AgentSHA, connectIP); err != nil {
		log.Printf("hub: MarkNodeOnline: %v", err)
	}
	h.settleHelloRelayHosts(node, connectIP, hello)
	if hello.PortRange != "" {
		if err := db.ValidatePortRange(hello.PortRange); err != nil {
			log.Printf("hub: node %d reported invalid port range %q, ignoring: %v", node.ID, hello.PortRange, err)
		} else if err := db.UpdateNodePortRange(h.DB, node.ID, hello.PortRange); err != nil {
			log.Printf("hub: node %d: store reported port range: %v", node.ID, err)
		}
	}

	// A node may have missed rule changes while it was offline. Reconcile now so
	// the kernel state converges on reconnect instead of drifting until the next
	// mutation. The rev check keeps this a no-op when the node is already in sync.
	h.reconcileOnConnect(node.ID, hello.LastAppliedRev)

	go h.writerLoop(ac)
	h.readerLoop(ctx, ac)
}

// reconcileOnConnect re-pushes the node's ruleset after a (re)connect unless the
// agent's reported last-applied rev already matches what the panel would send.
// Redispatch runs off-goroutine, so it schedules the apply without blocking the
// hello path; the apply_ack is handled once readerLoop starts.
func (h *Hub) reconcileOnConnect(nodeID int64, lastAppliedRev string) {
	if h.Redispatch == nil {
		return
	}
	ruleHops, err := db.ActiveRuleHopsForPush(h.DB, nodeID)
	var rules []nft.Rule
	if err == nil {
		rules, err = buildRules(h.DB, ruleHops)
	}
	if err != nil {
		// Can't compute the target rev — force a resync rather than risk drift.
		log.Printf("hub: node %d: compute target rev on connect, forcing a resync: %v", nodeID, err)
		go h.Redispatch([]int64{nodeID})
		return
	}
	if lastAppliedRev != "" && computeRev(rules) == lastAppliedRev {
		return
	}
	go h.Redispatch([]int64{nodeID})
}

func (h *Hub) registerConn(ac *agentConn) {
	h.mu.Lock()
	if old, ok := h.conns[ac.nodeID]; ok {
		old.signalClose()
		old.ws.Close(websocket.StatusGoingAway, "replaced by newer connection")
	}
	h.conns[ac.nodeID] = ac
	h.mu.Unlock()
}

func (h *Hub) unregisterConn(ac *agentConn) {
	h.mu.Lock()
	if cur, ok := h.conns[ac.nodeID]; ok && cur == ac {
		delete(h.conns, ac.nodeID)
	}
	h.mu.Unlock()
	ac.signalClose()
	if err := db.MarkNodeOffline(h.DB, ac.nodeID); err != nil {
		log.Printf("hub: node %d: mark offline: %v", ac.nodeID, err)
	}
}

// Close gracefully shuts down every agent connection.
func (h *Hub) Close() {
	h.mu.Lock()
	conns := make([]*agentConn, 0, len(h.conns))
	for _, ac := range h.conns {
		conns = append(conns, ac)
	}
	h.conns = make(map[int64]*agentConn)
	h.mu.Unlock()

	for _, ac := range conns {
		ac.signalClose()
		_ = ac.ws.Close(websocket.StatusGoingAway, "panel shutting down")
	}
}

func (h *Hub) writerLoop(ac *agentConn) {
	for {
		select {
		case <-ac.closed:
			return
		case b := <-ac.writeCh:
			ctx, cancel := context.WithTimeout(context.Background(), hubWriteTimeout)
			err := ac.ws.Write(ctx, websocket.MessageText, b)
			cancel()
			if err != nil {
				ac.ws.Close(websocket.StatusInternalError, "write error")
				return
			}
		}
	}
}

func (h *Hub) readerLoop(parent context.Context, ac *agentConn) {
	for {
		ctx, cancel := context.WithTimeout(parent, hubReadTimeout)
		_, b, err := ac.ws.Read(ctx)
		cancel()
		if err != nil {
			return
		}
		var env wsproto.Envelope
		if err := json.Unmarshal(b, &env); err != nil {
			log.Printf("hub: malformed envelope from node %d: %v", ac.nodeID, err)
			continue
		}
		h.handleFrame(ac, env)
	}
}

// handleFrame acts on one frame a node sent. Replies go out through
// ac.enqueueWrite; nothing here touches the socket itself.
func (h *Hub) handleFrame(ac *agentConn, env wsproto.Envelope) {
	switch env.Type {
	case wsproto.TypePing:
		pong, _ := json.Marshal(wsproto.Pong{TS: time.Now().UnixMilli()})
		ac.enqueueWrite(wsproto.Envelope{Type: wsproto.TypePong, ID: env.ID, Payload: pong})
	case wsproto.TypeCounters:
		var co wsproto.Counters
		if err := json.Unmarshal(env.Payload, &co); err != nil {
			log.Printf("hub: node %d malformed counters: %v", ac.nodeID, err)
			return
		}
		h.applyCounters(ac.nodeID, co.Samples)
	case wsproto.TypeRuleCreate:
		h.handleRuleCreate(ac, env)
	case wsproto.TypeRuleUpdate:
		h.handleRuleUpdate(ac, env)
	case wsproto.TypeMigrateRules:
		h.handleMigrateRules(ac, env)
	case wsproto.TypeRuleHopEdit:
		var e wsproto.RuleHopEdit
		if err := json.Unmarshal(env.Payload, &e); err != nil {
			sendRuleAckErr(ac, env.ID, "malformed payload")
			return
		}
		entry, cerr := h.applyRuleHopEdit(ac.nodeID, e.RuleID, e.ListenPort, e.Mode, e.Comment)
		ack := wsproto.RuleCmdAck{OK: cerr == nil, Entry: entry}
		if cerr != nil {
			ack.Error = ruleAckMessage(cerr)
		}
		ackP, _ := json.Marshal(ack)
		ac.enqueueWrite(wsproto.Envelope{Type: wsproto.TypeRuleCmdAck, ID: env.ID, Payload: ackP})
	case wsproto.TypeRuleDelete:
		var dl wsproto.RuleDelete
		if err := json.Unmarshal(env.Payload, &dl); err != nil {
			sendRuleAckErr(ac, env.ID, "malformed payload")
			return
		}
		cerr := h.applyRuleDelete(ac.nodeID, dl.RuleID)
		ack := wsproto.RuleCmdAck{OK: cerr == nil}
		if cerr != nil {
			ack.Error = ruleAckMessage(cerr)
		}
		ackP, _ := json.Marshal(ack)
		ac.enqueueWrite(wsproto.Envelope{Type: wsproto.TypeRuleCmdAck, ID: env.ID, Payload: ackP})
	case wsproto.TypeApplyAck, wsproto.TypeHelloAck, wsproto.TypeUpgradeAck, wsproto.TypeProbeAck:
		ac.dispatchAck(env)
	default:
		log.Printf("hub: node %d unknown frame type %q", ac.nodeID, env.Type)
	}
}

func (ac *agentConn) enqueueWrite(env wsproto.Envelope) {
	b, err := json.Marshal(env)
	if err != nil {
		return
	}
	select {
	case ac.writeCh <- b:
	case <-ac.closed:
	}
}

func (ac *agentConn) dispatchAck(env wsproto.Envelope) {
	ac.pendMu.Lock()
	ch, ok := ac.pending[env.ID]
	if ok {
		delete(ac.pending, env.ID)
	}
	ac.pendMu.Unlock()
	if ok {
		ch <- env.Payload
	}
}

func (h *Hub) SendApplyRuleset(nodeID int64, rules []nft.Rule, rev string) (string, error) {
	h.mu.RLock()
	ac, ok := h.conns[nodeID]
	h.mu.RUnlock()
	if !ok {
		return "", errNodeNotConnected
	}
	id := ac.nextID()
	ch := make(chan json.RawMessage, 1)
	ac.pendMu.Lock()
	ac.pending[id] = ch
	ac.pendMu.Unlock()
	defer func() {
		ac.pendMu.Lock()
		delete(ac.pending, id)
		ac.pendMu.Unlock()
	}()

	payload, _ := json.Marshal(wsproto.ApplyRuleset{Rev: rev, Rules: rules})
	ac.enqueueWrite(wsproto.Envelope{Type: wsproto.TypeApplyRuleset, ID: id, Payload: payload})

	select {
	case raw := <-ch:
		var ack wsproto.ApplyAck
		if err := json.Unmarshal(raw, &ack); err != nil {
			return "", pushUpstream(fmt.Errorf("malformed apply_ack: %w", err))
		}
		if !ack.OK {
			return "", pushUpstream(fmt.Errorf("apply rejected: %s", ack.Error))
		}
		return ack.Warning, nil
	case <-time.After(applyAckTimeout):
		return "", pushUpstream(errors.New("apply_ack timeout"))
	case <-ac.closed:
		return "", pushUpstream(errors.New("connection closed before ack"))
	}
}

func (h *Hub) SendProbe(nodeID int64, target, proto string) (wsproto.ProbeAck, error) {
	h.mu.RLock()
	ac, ok := h.conns[nodeID]
	h.mu.RUnlock()
	if !ok {
		return wsproto.ProbeAck{}, errNodeNotConnected
	}
	id := ac.nextID()
	ch := make(chan json.RawMessage, 1)
	ac.pendMu.Lock()
	ac.pending[id] = ch
	ac.pendMu.Unlock()
	defer func() {
		ac.pendMu.Lock()
		delete(ac.pending, id)
		ac.pendMu.Unlock()
	}()

	if proto != "udp" {
		proto = "" // tcp is the wire default; omit for pre-upgrade agents
	}
	payload, _ := json.Marshal(wsproto.Probe{Target: target, Proto: proto})
	ac.enqueueWrite(wsproto.Envelope{Type: wsproto.TypeProbe, ID: id, Payload: payload})

	select {
	case raw := <-ch:
		var ack wsproto.ProbeAck
		if err := json.Unmarshal(raw, &ack); err != nil {
			return wsproto.ProbeAck{}, pushUpstream(fmt.Errorf("malformed probe_ack: %w", err))
		}
		return ack, nil
	case <-time.After(10 * time.Second):
		return wsproto.ProbeAck{}, pushUpstream(errors.New("probe timeout"))
	case <-ac.closed:
		return wsproto.ProbeAck{}, pushUpstream(errors.New("connection closed"))
	}
}

func (h *Hub) BroadcastConfigUpdate(poolSize int) {
	payload, _ := json.Marshal(wsproto.ConfigUpdate{PoolSize: poolSize})
	env := wsproto.Envelope{Type: wsproto.TypeConfigUpdate, Payload: payload}
	h.mu.RLock()
	conns := make([]*agentConn, 0, len(h.conns))
	for _, ac := range h.conns {
		conns = append(conns, ac)
	}
	h.mu.RUnlock()
	for _, ac := range conns {
		ac.enqueueWrite(env)
	}
}

// Helpers --------------------------------------------------------------

func extractIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.Index(xff, ","); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return strings.Trim(host, "[]")
}

// settleHelloRelayHosts stores what a hello says about the node's relay
// addresses (storeHelloRelayHosts) and brings the rules in line with the
// address the node then has.
//
// In every rule, the hop before one on this node targets the node's relay
// address, as stored when the rule was generated. A hello can change that
// address — the daemon declares another one, or an empty field is seeded —
// and the hops that target the old one would go on sending traffic there.
// db.RewireRulesIntoNode re-generates the rules that do not target the
// stored address. It decides from the stored rows alone, so it is run on
// every hello, including one whose store step failed or changed nothing:
// that is what picks up a rule it could not re-generate on an earlier hello,
// and a rule that is stale in the database the panel was started on.
//
// The nodes of the re-generated rules are re-pushed off-goroutine. The node
// saying hello is among them, and its ack to a push cannot be read before
// this hello has been handled (ServeWS starts reading frames after it).
func (h *Hub) settleHelloRelayHosts(node *db.Node, connectIP string, hello wsproto.Hello) {
	if err := storeHelloRelayHosts(h.DB, node, connectIP, hello); err != nil {
		log.Printf("hub: node %d: store relay hosts: %v", node.ID, err)
	}
	nodes, err := db.RewireRulesIntoNode(h.DB, node.ID)
	if err != nil {
		log.Printf("hub: node %d: rules left targeting an address the node no longer has: %v", node.ID, err)
	}
	if len(nodes) > 0 && h.Redispatch != nil {
		go h.Redispatch(nodes)
	}
}

// storeHelloRelayHosts brings a node's stored relay addresses in line with a
// hello: what the daemon declares first (applyDeclaredRelayHosts), then the
// seeding of whatever is still empty (fillNodeRelayHosts). node is the row as
// loaded for this hello.
//
// It returns the first write's error. Both steps decide from node's fields
// and advance them only after the write they mirror succeeded, so after a
// failure node may no longer mirror the stored row; the seeding is therefore
// not attempted on top of a failed first step. Nothing is lost by stopping:
// every decision is a comparison against the stored row, so the next hello,
// which loads the row afresh, makes the writes that are still outstanding.
func storeHelloRelayHosts(d *sql.DB, node *db.Node, connectIP string, hello wsproto.Hello) error {
	if err := applyDeclaredRelayHosts(d, node, hello.DeclaredRelayHost, hello.DeclaredRelayHostV6); err != nil {
		return err
	}
	return fillNodeRelayHosts(d, node, connectIP, hello.ProbedV4, hello.ProbedV6, hello.DeclaredRelayHost, hello.DeclaredRelayHostV6)
}

// fillNodeRelayHosts seeds relay_host/relay_host_v6 for a node that hasn't
// had them set yet. connectIP (the address the panel observed this WS
// connection arrive from) is authoritative for whichever family it belongs
// to — it reflects the address as seen after any NAT, unlike a locally
// self-probed address. The agent's self-probed address only fills the
// OTHER family, the one this connection didn't use. Never overwrites a
// manually-configured value (only fires when the DB field is still empty).
//
// relay_host must always hold a v4 literal or hostname: an IPv6 literal
// found there can only be leftover data from before the two fields were
// split by address family. Such a value is evicted from relay_host
// unconditionally, so the empty-field seeding below can re-fill it with a
// proper v4 value. It is migrated into relay_host_v6 only when this
// connection didn't itself supply a fresher v6 connectIP: connectIP is
// always more authoritative than data carried over from before the split,
// since the agent's address may well have changed since that data was
// written. This makes stale data self-heal on the node's next connection
// instead of persisting forever, without letting the stale value block a
// fresher one from landing.
//
// declaredV4/declaredV6 come from this same hello's applyDeclaredRelayHosts
// call. A non-empty declared value — even one that failed validation and so
// was never written — means the operator expressed intent for that family;
// auto-filling it from connectIP would silently paper over a rejected
// declaration with the very kind of address (the connection's outbound
// route) the declare feature exists to override. So a non-empty declared
// value suppresses auto-fill for its family regardless of whether it was
// valid. Nodes that never declare anything see declaredV4/declaredV6 always
// empty, so this guard is always true for them and behavior is unchanged.
//
// It stops at the first write that fails and returns the error; node's fields
// are advanced only after the write they mirror succeeded.
func fillNodeRelayHosts(d *sql.DB, node *db.Node, connectIP, probedV4, probedV6, declaredV4, declaredV6 string) error {
	connectIsV6 := false
	if ip := net.ParseIP(connectIP); ip != nil {
		connectIsV6 = ip.To4() == nil
	}
	if ip := net.ParseIP(node.RelayHost); ip != nil && ip.To4() == nil {
		if node.RelayHostV6 == "" && !(connectIsV6 && connectIP != "") {
			if err := db.UpdateNodeRelayHostV6(d, node.ID, node.RelayHost); err != nil {
				return err
			}
			node.RelayHostV6 = node.RelayHost
		}
		if err := db.UpdateNodeRelayHost(d, node.ID, ""); err != nil {
			return err
		}
		node.RelayHost = ""
	}
	if node.RelayHost == "" && declaredV4 == "" {
		seed := probedV4
		if !connectIsV6 && connectIP != "" {
			seed = connectIP
		}
		if seed != "" {
			if err := db.UpdateNodeRelayHost(d, node.ID, seed); err != nil {
				return err
			}
		}
	}
	if node.RelayHostV6 == "" && declaredV6 == "" {
		seed := probedV6
		if connectIsV6 && connectIP != "" {
			seed = connectIP
		}
		if seed != "" {
			if err := db.UpdateNodeRelayHostV6(d, node.ID, seed); err != nil {
				return err
			}
		}
	}
	return nil
}

// applyDeclaredRelayHosts handles operator-declared relay_host/relay_host_v6
// values sent via Hello.DeclaredRelayHost/DeclaredRelayHostV6 (see
// cmd/nft-agent's --relay-host/--relay-host-v6 flags). Unlike
// fillNodeRelayHosts, which only ever seeds an empty field once, a declared
// value is authoritative: it overwrites whatever is in the DB on every
// hello where it's present, so config drift self-heals. When the daemon
// stops declaring a value (flag removed, daemon restarted), the DB field
// unlocks but keeps its last value rather than going blank, so a live route
//
// It stops at the first write that fails and returns the error; node's fields
// are advanced only after the writes they mirror succeeded. A value whose
// address was stored but whose declared flag was not is written again on the
// next hello, because the comparison below still sees the flag unset.
func applyDeclaredRelayHosts(d *sql.DB, node *db.Node, declaredV4, declaredV6 string) error {
	if declaredV4 != "" {
		if isValidRelayHost(declaredV4) {
			if node.RelayHost != declaredV4 || !node.RelayHostDeclared {
				if err := db.UpdateNodeRelayHost(d, node.ID, declaredV4); err != nil {
					return err
				}
				if err := db.SetNodeRelayHostDeclared(d, node.ID, true); err != nil {
					return err
				}
				node.RelayHost, node.RelayHostDeclared = declaredV4, true
			}
		} else {
			log.Printf("hub: node %d declared invalid relay_host %q, ignoring", node.ID, declaredV4)
		}
	} else if node.RelayHostDeclared {
		if err := db.SetNodeRelayHostDeclared(d, node.ID, false); err != nil {
			return err
		}
		node.RelayHostDeclared = false
	}

	if declaredV6 != "" {
		if isValidRelayHostV6(declaredV6) {
			if node.RelayHostV6 != declaredV6 || !node.RelayHostV6Declared {
				if err := db.UpdateNodeRelayHostV6(d, node.ID, declaredV6); err != nil {
					return err
				}
				if err := db.SetNodeRelayHostV6Declared(d, node.ID, true); err != nil {
					return err
				}
				node.RelayHostV6, node.RelayHostV6Declared = declaredV6, true
			}
		} else {
			log.Printf("hub: node %d declared invalid relay_host_v6 %q, ignoring", node.ID, declaredV6)
		}
	} else if node.RelayHostV6Declared {
		if err := db.SetNodeRelayHostV6Declared(d, node.ID, false); err != nil {
			return err
		}
		node.RelayHostV6Declared = false
	}
	return nil
}

func lookupNodeBySecret(d *sql.DB, secret string) (*db.Node, error) {
	if secret == "" {
		return nil, errors.New("empty secret")
	}
	var id int64
	err := d.QueryRow(`SELECT id FROM nodes WHERE secret=? AND disabled=0`, secret).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return db.GetNode(d, id)
}

func readEnvelope(ctx context.Context, ws *websocket.Conn, timeout time.Duration) (wsproto.Envelope, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, b, err := ws.Read(rctx)
	if err != nil {
		return wsproto.Envelope{}, err
	}
	var env wsproto.Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return wsproto.Envelope{}, err
	}
	return env, nil
}

func writeEnvelope(ctx context.Context, ws *websocket.Conn, env wsproto.Envelope) error {
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, hubWriteTimeout)
	defer cancel()
	return ws.Write(wctx, websocket.MessageText, b)
}

func writeError(ctx context.Context, ws *websocket.Conn, code, msg string) {
	p, _ := json.Marshal(wsproto.Error{Code: code, Message: msg})
	_ = writeEnvelope(ctx, ws, wsproto.Envelope{Type: wsproto.TypeError, Payload: p})
}

// applyCounters folds per-rule bytes_delta into the rule_hops table and the
// owning user's usage. The (node_id, listen_port, proto) tuple identifies
// the hop. Each sample is resolved to its rule_hop row so we learn the hop
// id and the owning rule/user.
//
// The same bytes flow through every hop of a chain, so the global user quota is
// billed exactly once — at the entry hop (position 0) — weighted by the entry
// node's own rate_multiplier and the user's billing rate. Per-grant quota
// charges raw bytes once per logical segment, at that segment's first hop, onto
// the segment's logical node grant. Quota suppression keys on the same logical
// node end to end.
func (h *Hub) applyCounters(nodeID int64, samples []wsproto.CounterSample) {
	hopMap, err := db.RuleHopMapByNode(h.DB, nodeID)
	if err != nil {
		log.Printf("hub: node %d load rule hop map for counters: %v", nodeID, err)
		return
	}
	// Only the rules referenced by this node's hops are ever looked up, so load
	// just those instead of scanning the whole rules table every counters batch.
	ruleIDSet := map[int64]bool{}
	for _, rh := range hopMap {
		ruleIDSet[rh.RuleID] = true
	}
	ruleIDs := make([]int64, 0, len(ruleIDSet))
	for id := range ruleIDSet {
		ruleIDs = append(ruleIDs, id)
	}
	// The rules tell which user each hop's bytes belong to. Without them every
	// sample would be recorded as owned by no one — forwarded but billed to
	// nobody — so the batch is dropped, as it is when the hop map fails above.
	ruleMap, err := db.RulesByIDs(h.DB, ruleIDs)
	if err != nil {
		log.Printf("hub: node %d load rules for counters: %v", nodeID, err)
		return
	}
	// The entry node's multiplier goes into the amount charged. It has no
	// stand-in: read as "none set" every node would be billed at 1.0, which
	// charges a free node's traffic and undercharges a node priced above 1.0.
	// So the batch is dropped.
	multipliers, err := db.NodeRateMultipliers(h.DB)
	if err != nil {
		log.Printf("hub: node %d load node rate multipliers: %v", nodeID, err)
		return
	}
	// Segment-first hops drive per-grant accounting: each logical segment's
	// grant is charged once, at the hop where the segment begins. On a load
	// error the empty map below means "no hop begins a segment", so this
	// batch charges no grant: the per-node quotas go without these bytes,
	// while the hop totals and the global quota still get them.
	segFirst, err := db.SegmentFirstHops(h.DB, ruleIDs)
	if err != nil {
		log.Printf("hub: node %d load segment first hops: %v", nodeID, err)
		segFirst = map[int64]map[int]int64{}
	}

	// Landing-exit ledger lookups for this batch: which (owner, host, port)
	// triples are present landing exits, and each rule's final hop position —
	// the only hop whose bytes reach the exit ledger, since middle hops target
	// system relay addresses. On a load error the batch skips exit metering
	// entirely (under-counting beats mis-counting).
	ownerSet := map[int64]bool{}
	for _, r := range ruleMap {
		if r.OwnerID.Valid {
			ownerSet[r.OwnerID.Int64] = true
		}
	}
	ownerIDs := make([]int64, 0, len(ownerSet))
	for id := range ownerSet {
		ownerIDs = append(ownerIDs, id)
	}
	exitSet, err := db.PresentLandingExitSet(h.DB, ownerIDs)
	if err != nil {
		log.Printf("hub: node %d load landing exit set: %v", nodeID, err)
		exitSet = nil
	}
	maxPos, err := db.MaxHopPositions(h.DB, ruleIDs)
	if err != nil {
		log.Printf("hub: node %d load hop positions: %v", nodeID, err)
		exitSet = nil
	}

	node, err := db.GetNode(h.DB, nodeID)
	if err != nil {
		log.Printf("hub: node %d load for billing direction: %v", nodeID, err)
		return
	}

	type userNode struct{ userID, nodeID int64 }
	touched := map[userNode]bool{}
	// userCache holds each owning user loaded once per batch (nil when the user
	// row no longer exists), serving both the cycle-reset check and the
	// billing-rate lookup so the same user isn't queried twice per sample.
	userCache := map[int64]*db.User{}

	// Accumulate all row mutations and flush them in one transaction after the
	// loop. Reads, cycle resets and redispatch stay outside any tx: with
	// MaxOpenConns(1) a tx holds the only connection, so a pool read or a
	// redispatch goroutine inside it would deadlock.
	type hopWrite struct{ lastBytes, lastUp, lastDown, addWeighted int64 }
	hopWrites := map[int64]*hopWrite{}
	userNodeAdds := map[userNode]int64{}
	userAdds := map[int64]int64{}
	type exitAdd struct{ up, down int64 }
	exitAdds := map[db.UserExitKey]*exitAdd{}
	// The reporting node's raw ledger: every sampled byte counts, both
	// directions regardless of unidirectional billing, and before the rule_hop
	// match below — a sample whose rule was deleted mid-batch is still real
	// forwarded volume.
	var rawAdd int64

	for _, s := range samples {
		// Pre-v0.33 agents send BytesDelta without direction; fall back to it
		// so traffic accounting continues while the node upgrades.
		if s.BytesUp == 0 && s.BytesDown == 0 && s.BytesDelta > 0 {
			s.BytesUp = s.BytesDelta
		}
		totalDelta := s.BytesUp + s.BytesDown
		rawAdd += totalDelta
		billedDelta := totalDelta
		if node.Unidirectional {
			// Unidirectional billing charges the heavier direction only, not
			// their sum: a flow is dominated by one side (download for most
			// traffic, upload for backups) and max(up,down) prices that side
			// without double-counting the light reverse channel.
			billedDelta = max(s.BytesUp, s.BytesDown)
		}
		key := fmt.Sprintf("%s/%d", s.Proto, s.ListenPort)
		rh, ok := hopMap[key]
		if !ok {
			log.Printf("hub: node %d counters sample for %s/%d matched no rule_hop row (rule may have been deleted)", nodeID, s.Proto, s.ListenPort)
			continue
		}
		r := ruleMap[rh.RuleID]

		// Exit ledger: final hop only, raw and unweighted — it records real
		// traffic to the destination, independent of billing multipliers and
		// the node's unidirectional setting. It marks the pair touched from its
		// own totalDelta so the quota callback fires on real forwarded volume,
		// independent of whatever the billing path below charges.
		if r != nil && r.OwnerID.Valid && totalDelta > 0 && len(exitSet) > 0 && rh.Position == maxPos[rh.RuleID] {
			key := db.UserExitKey{UserID: r.OwnerID.Int64, Host: r.ExitHost, Port: r.ExitPort}
			if exitSet[key] {
				ea := exitAdds[key]
				if ea == nil {
					ea = &exitAdd{}
					exitAdds[key] = ea
				}
				ea.up += s.BytesUp
				ea.down += s.BytesDown
				touched[userNode{key.UserID, nodeID}] = true
			}
		}

		// Global quota: the same bytes flow through every hop, so the user is
		// billed exactly once — at the entry hop — with the entry node's own
		// rate_multiplier (a composite entry carries the baked composite factor;
		// middle-layer and child hops never stack their own).
		weighted := billedDelta
		var userID int64
		hasOwner := r != nil && r.OwnerID.Valid && billedDelta > 0
		if hasOwner {
			userID = r.OwnerID.Int64

			u, cached := userCache[userID]
			if !cached {
				var err error
				u, err = db.GetUserByID(h.DB, userID)
				if errors.Is(err, sql.ErrNoRows) {
					// The owner was deleted mid-batch: nobody is left to bill.
					u = nil
				} else if err != nil {
					// The user's billing rate goes into the amount charged and
					// has no stand-in value. This batch's own rows are written
					// only after the loop, so returning here drops it whole.
					log.Printf("hub: node %d load user %d for counters: %v", nodeID, userID, err)
					return
				}
				userCache[userID] = u
				if u != nil {
					reset, err := db.CheckAndResetTrafficCycle(h.DB, u)
					if err != nil {
						// Not fatal to the batch: the reset is due until it
						// happens, and the cycle enforcer checks it as well.
						log.Printf("hub: node %d check traffic cycle for user %d: %v", nodeID, userID, err)
					}
					if reset && h.Redispatch != nil {
						// Quota exclusions are evaluated at push time only; a
						// fresh cycle must re-push or suppressed rules stay dead.
						nodes, err := db.DistinctUserNodes(h.DB, userID)
						if err != nil {
							log.Printf("hub: user %d was reset but its nodes could not be listed for re-dispatch: %v", userID, err)
						} else {
							go h.Redispatch(nodes)
						}
					}
				}
			}

			// Entry multiplier 0 marks a free node: global usage accrues nothing.
			// Only a negative value or a map-miss (entry node vanished mid-batch)
			// falls back to 1.0 — a miss must never silently make a rule free.
			// Per-grant raw-byte quotas and the exit ledger still accrue regardless.
			mult, ok := multipliers[r.NodeID]
			if !ok || mult < 0 {
				mult = 1.0
			}
			billingRate := 1.0
			if u != nil && u.BillingRate > 0 {
				billingRate = u.BillingRate
			}
			weighted = int64(math.Round(float64(billedDelta) * mult * billingRate))
		}

		// rule_hops: last_bytes stay raw for speed display. total_bytes carries
		// the billed (weighted) amount on the entry hop — what per-rule traffic
		// shows and the global quota consumes — and raw bytes on every other hop.
		// A tcp+udp hop can fan in as two samples to the same row; last_* take
		// the last sample and the total sums.
		w := hopWrites[rh.ID]
		if w == nil {
			w = &hopWrite{}
			hopWrites[rh.ID] = w
		}
		w.lastBytes = totalDelta
		w.lastUp = s.BytesUp
		w.lastDown = s.BytesDown
		if rh.Position == 0 {
			w.addWeighted += weighted
		} else {
			w.addWeighted += billedDelta
		}

		if !hasOwner {
			continue
		}

		// Per-grant quota: raw bytes, charged once per logical segment at its
		// first hop, onto the segment's logical node grant (the entry segment's
		// via is rules.node_id, so its grant is included). Suppression marks the
		// same logical node so RulesAffectedByNode and OnTrafficUpdate stay in
		// step with this accounting.
		if via, ok := segFirst[rh.RuleID][rh.Position]; ok {
			userNodeAdds[userNode{userID, via}] += billedDelta
			touched[userNode{userID, via}] = true
		}
		// Global usage is entry-only: bill the weighted amount once, at position 0.
		if rh.Position == 0 && weighted > 0 {
			userAdds[userID] += weighted
		}
	}

	// Flush every accumulated mutation in a single transaction: one commit (one
	// fsync) for the whole batch instead of 3-5 auto-commits per sample.
	if len(hopWrites) > 0 || len(userNodeAdds) > 0 || len(userAdds) > 0 || len(exitAdds) > 0 || rawAdd > 0 {
		if tx, err := h.DB.Begin(); err != nil {
			log.Printf("hub: node %d counters tx begin: %v", nodeID, err)
		} else {
			ok := true
			if rawAdd > 0 {
				if err := db.AddNodeRawTraffic(tx, nodeID, rawAdd); err != nil {
					log.Printf("hub: node %d raw traffic add: %v", nodeID, err)
					ok = false
				}
			}
			for id, w := range hopWrites {
				if !ok {
					break
				}
				if _, err := tx.Exec(`UPDATE rule_hops SET last_bytes=?, last_bytes_up=?, last_bytes_down=?, total_bytes=total_bytes+? WHERE id=?`,
					w.lastBytes, w.lastUp, w.lastDown, w.addWeighted, id); err != nil {
					log.Printf("hub: node %d counters rule_hop update: %v", nodeID, err)
					ok = false
					break
				}
			}
			for un, delta := range userNodeAdds {
				if !ok {
					break
				}
				if _, err := tx.Exec(`UPDATE user_nodes SET traffic_used_bytes = traffic_used_bytes + ? WHERE user_id=? AND node_id=?`, delta, un.userID, un.nodeID); err != nil {
					log.Printf("hub: user %d node %d per-node traffic add: %v", un.userID, un.nodeID, err)
					ok = false
					break
				}
			}
			for uid, delta := range userAdds {
				if !ok {
					break
				}
				if _, err := tx.Exec(`UPDATE users SET traffic_used_bytes = traffic_used_bytes + ? WHERE id=?`, delta, uid); err != nil {
					log.Printf("hub: user %d traffic add: %v", uid, err)
					ok = false
					break
				}
			}
			for k, ea := range exitAdds {
				if !ok {
					break
				}
				// A zero-row hit means the row was flipped absent and deleted
				// between load and flush; dropping one batch is the intent of
				// that deletion.
				if _, err := tx.Exec(`UPDATE user_landing_exits SET used_bytes = used_bytes + ?, used_bytes_up = used_bytes_up + ?, used_bytes_down = used_bytes_down + ?, updated_at = ? WHERE user_id=? AND host=? AND port=?`,
					ea.up+ea.down, ea.up, ea.down, time.Now().Unix(), k.UserID, k.Host, k.Port); err != nil {
					log.Printf("hub: user %d exit %s:%d ledger add: %v", k.UserID, k.Host, k.Port, err)
					ok = false
					break
				}
			}
			if ok {
				if err := tx.Commit(); err != nil {
					log.Printf("hub: node %d counters tx commit: %v", nodeID, err)
				}
			} else {
				_ = tx.Rollback()
			}
		}
	}

	deltas := make([]counterDelta, 0, len(samples))
	for _, s := range samples {
		// Attribute the hop's speed to its rule's owner so the per-user speed
		// view can filter it. An unmatched hop (rule deleted mid-batch) or an
		// ownerless admin rule leaves ownerID 0 — it still counts toward the
		// node total, just not toward any user's share.
		var ownerID int64
		if rh, ok := hopMap[s.Proto+"/"+strconv.Itoa(s.ListenPort)]; ok {
			if r := ruleMap[rh.RuleID]; r != nil && r.OwnerID.Valid {
				ownerID = r.OwnerID.Int64
			}
		}
		deltas = append(deltas, counterDelta{
			proto:         s.Proto,
			listenPortStr: strconv.Itoa(s.ListenPort),
			bytesUp:       s.BytesUp,
			bytesDown:     s.BytesDown,
			ownerID:       ownerID,
		})
	}
	h.speedCache.update(nodeID, deltas)

	// Quota enforcement (the OnTrafficUpdate callback) can call back into the hub
	// to re-dispatch a ruleset, which blocks on an apply_ack that this very
	// readerLoop must deliver. Run it off-goroutine so the reader never waits on
	// itself — otherwise every enforcement fires the 30s apply timeout and flaps
	// the node. touched is loop-local, so snapshotting it here is race-free.
	if h.OnTrafficUpdate != nil && len(touched) > 0 {
		pairs := make([]userNode, 0, len(touched))
		for un := range touched {
			pairs = append(pairs, un)
		}
		go func() {
			for _, un := range pairs {
				h.OnTrafficUpdate(un.userID, un.nodeID)
			}
		}()
	}
}

// applyRuleHopEdit folds a node-reported edit to its hop in ruleID back
// into the rule skeleton and re-dispatches every node the regeneration
// touched, returning the rule's copyable entry endpoint. The hop is located
// by (ruleID, nodeID): a rule can't repeat a node, so that pair is unique.
func (h *Hub) applyRuleHopEdit(nodeID, ruleID int64, listenPort int, mode, comment string) (string, error) {
	r, err := db.GetRule(h.DB, ruleID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", db.ChainErrorf("规则不存在")
	}
	if err != nil {
		return "", err
	}
	hops, err := db.ListRuleHops(h.DB, ruleID)
	if err != nil {
		return "", err
	}
	found := false
	inputs := make([]db.HopInput, len(hops))
	for i, hp := range hops {
		in := db.HopInput{NodeID: hp.NodeID, Mode: hp.Mode, ViaNodeID: hp.ViaNodeID}
		if hp.NodeID == nodeID {
			found = true
			in.DesiredPort = listenPort
			in.Mode = db.NormalizeForwardMode(mode)
			in.Comment = comment
		}
		inputs[i] = in
	}
	if !found {
		return "", db.ChainErrorf("节点不在该规则上")
	}
	tx, err := h.DB.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	entry, _, affected, err := db.RegenerateRule(tx, r, inputs, nil)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	if h.Redispatch != nil {
		h.Redispatch(affected)
	}
	return entry, nil
}

// applyRuleDelete removes the whole rule (all hops on all nodes) on behalf
// of a node that participates in it, then re-dispatches every node that ran
// its hops so the deleted rules leave the kernel.
func (h *Hub) applyRuleDelete(nodeID, ruleID int64) error {
	hops, err := db.ListRuleHops(h.DB, ruleID)
	if err != nil {
		return err
	}
	onRule := false
	for _, hp := range hops {
		if hp.NodeID == nodeID {
			onRule = true
			break
		}
	}
	if !onRule {
		return db.ChainErrorf("节点不在该规则上")
	}
	nodes, err := db.DeleteRule(h.DB, ruleID)
	if err != nil {
		return err
	}
	if h.Redispatch != nil {
		h.Redispatch(nodes)
	}
	return nil
}

// handleRuleCreate creates a new single-hop rule on the requesting node.
// The node must have an owner (via node.owner_id) who is active and within quota.
func (h *Hub) handleRuleCreate(ac *agentConn, env wsproto.Envelope) {
	var req wsproto.RuleCreate
	if err := json.Unmarshal(env.Payload, &req); err != nil {
		sendRuleAckErr(ac, env.ID, "malformed payload")
		return
	}
	node, err := db.GetNode(h.DB, ac.nodeID)
	if err != nil {
		sendRuleAckLookupErr(ac, env.ID, err, "节点不存在")
		return
	}
	if node.OwnerID == nil {
		sendRuleAckErr(ac, env.ID, "节点无归属用户")
		return
	}
	ownerID := *node.OwnerID
	u, err := db.GetUserByID(h.DB, ownerID)
	if err != nil {
		sendRuleAckLookupErr(ac, env.ID, err, "归属用户不存在")
		return
	}
	if u.Disabled {
		sendRuleAckErr(ac, env.ID, "用户已被禁用")
		return
	}
	if u.ExpiresAt.Valid && u.ExpiresAt.Int64 > 0 && u.ExpiresAt.Int64 < time.Now().Unix() {
		sendRuleAckErr(ac, env.ID, "用户已过期")
		return
	}
	total, err := db.CountRulesForUser(h.DB, ownerID)
	if err != nil {
		sendRuleAckFailure(ac, env.ID, err)
		return
	}
	if total >= u.MaxForwards {
		sendRuleAckErr(ac, env.ID, fmt.Sprintf("超出用户最大转发数（%d）", u.MaxForwards))
		return
	}

	tx, err := h.DB.Begin()
	if err != nil {
		sendRuleAckFailure(ac, env.ID, err)
		return
	}
	defer tx.Rollback()

	rl := &db.Rule{
		NodeID:   ac.nodeID,
		OwnerID:  sql.NullInt64{Int64: ownerID, Valid: true},
		Name:     req.Name,
		Proto:    req.Proto,
		ExitHost: req.ExitHost,
		ExitPort: req.ExitPort,
		Comment:  req.Comment,
	}
	id, err := db.CreateRule(tx, rl)
	if err != nil {
		sendRuleAckFailure(ac, env.ID, err)
		return
	}
	rl.ID = id

	hop := db.HopInput{NodeID: ac.nodeID, Mode: db.NormalizeForwardMode(req.Mode), ViaNodeID: ac.nodeID}
	if req.ListenPort > 0 {
		hop.DesiredPort = req.ListenPort
	}
	entry, _, affected, err := db.RegenerateRule(tx, rl, []db.HopInput{hop}, nil)
	if err != nil {
		sendRuleAckFailure(ac, env.ID, err)
		return
	}
	if err := tx.Commit(); err != nil {
		sendRuleAckFailure(ac, env.ID, err)
		return
	}
	if h.Redispatch != nil {
		go h.Redispatch(affected)
	}
	ackP, _ := json.Marshal(wsproto.RuleCmdAck{OK: true, Entry: entry})
	ac.enqueueWrite(wsproto.Envelope{Type: wsproto.TypeRuleCmdAck, ID: env.ID, Payload: ackP})
}

// handleRuleUpdate modifies a single-hop rule's header and regenerates it.
// Only rules with exactly 1 hop that belong to the node's owner are editable.
func (h *Hub) handleRuleUpdate(ac *agentConn, env wsproto.Envelope) {
	var req wsproto.RuleUpdate
	if err := json.Unmarshal(env.Payload, &req); err != nil {
		sendRuleAckErr(ac, env.ID, "malformed payload")
		return
	}
	node, err := db.GetNode(h.DB, ac.nodeID)
	if err != nil {
		sendRuleAckLookupErr(ac, env.ID, err, "节点不存在")
		return
	}
	if node.OwnerID == nil {
		sendRuleAckErr(ac, env.ID, "节点无归属用户")
		return
	}
	rl, err := db.GetRule(h.DB, req.RuleID)
	if err != nil {
		sendRuleAckLookupErr(ac, env.ID, err, "规则不存在")
		return
	}
	if !rl.OwnerID.Valid || rl.OwnerID.Int64 != *node.OwnerID {
		sendRuleAckErr(ac, env.ID, "无权操作该规则")
		return
	}
	// Only single-hop rules are editable from a node
	hops, err := db.ListRuleHops(h.DB, req.RuleID)
	if err != nil {
		sendRuleAckFailure(ac, env.ID, err)
		return
	}
	if len(hops) != 1 {
		sendRuleAckErr(ac, env.ID, "仅支持编辑单跳规则")
		return
	}

	rl.Name = req.Name
	rl.Proto = req.Proto
	rl.ExitHost = req.ExitHost
	rl.ExitPort = req.ExitPort
	rl.Comment = req.Comment

	tx, err := h.DB.Begin()
	if err != nil {
		sendRuleAckFailure(ac, env.ID, err)
		return
	}
	defer tx.Rollback()

	if err := db.UpdateRuleHeader(tx, rl); err != nil {
		sendRuleAckFailure(ac, env.ID, err)
		return
	}
	hop := db.HopInput{NodeID: ac.nodeID, Mode: db.NormalizeForwardMode(req.Mode), ViaNodeID: ac.nodeID}
	if req.ListenPort > 0 {
		hop.DesiredPort = req.ListenPort
	}
	entry, _, affected, err := db.RegenerateRule(tx, rl, []db.HopInput{hop}, nil)
	if err != nil {
		sendRuleAckFailure(ac, env.ID, err)
		return
	}
	if err := tx.Commit(); err != nil {
		sendRuleAckFailure(ac, env.ID, err)
		return
	}
	if h.Redispatch != nil {
		go h.Redispatch(affected)
	}
	ackP, _ := json.Marshal(wsproto.RuleCmdAck{OK: true, Entry: entry})
	ac.enqueueWrite(wsproto.Envelope{Type: wsproto.TypeRuleCmdAck, ID: env.ID, Payload: ackP})
}

// handleMigrateRules bulk-imports rules from an agent's local state.
// Each rule becomes a new single-hop rule owned by the node's owner.
func (h *Hub) handleMigrateRules(ac *agentConn, env wsproto.Envelope) {
	var req wsproto.MigrateRules
	if err := json.Unmarshal(env.Payload, &req); err != nil {
		sendRuleAckErr(ac, env.ID, "malformed payload")
		return
	}
	node, err := db.GetNode(h.DB, ac.nodeID)
	if err != nil {
		sendRuleAckLookupErr(ac, env.ID, err, "节点不存在")
		return
	}
	if node.OwnerID == nil {
		sendRuleAckErr(ac, env.ID, "节点无归属用户")
		return
	}
	ownerID := *node.OwnerID
	u, err := db.GetUserByID(h.DB, ownerID)
	if err != nil {
		sendRuleAckLookupErr(ac, env.ID, err, "归属用户不存在")
		return
	}
	if u.Disabled {
		sendRuleAckErr(ac, env.ID, "用户已被禁用")
		return
	}
	if u.ExpiresAt.Valid && u.ExpiresAt.Int64 > 0 && u.ExpiresAt.Int64 < time.Now().Unix() {
		sendRuleAckErr(ac, env.ID, "用户已过期")
		return
	}

	total, err := db.CountRulesForUser(h.DB, ownerID)
	if err != nil {
		sendRuleAckFailure(ac, env.ID, err)
		return
	}
	if total+len(req.Rules) > u.MaxForwards {
		sendRuleAckErr(ac, env.ID, fmt.Sprintf("超出用户最大转发数（%d），当前 %d 条，迁入 %d 条", u.MaxForwards, total, len(req.Rules)))
		return
	}

	tx, err := h.DB.Begin()
	if err != nil {
		sendRuleAckFailure(ac, env.ID, err)
		return
	}
	defer tx.Rollback()

	var allAffected []int64
	for _, r := range req.Rules {
		exitHost := r.DestIP
		if r.DestHost != "" {
			exitHost = r.DestHost
		}
		name := r.Comment
		if name == "" {
			name = r.RuleName
		}
		rl := &db.Rule{
			NodeID:   ac.nodeID,
			OwnerID:  sql.NullInt64{Int64: ownerID, Valid: true},
			Name:     name,
			Proto:    r.Proto,
			ExitHost: exitHost,
			ExitPort: r.DestPort,
		}
		id, err := db.CreateRule(tx, rl)
		if err != nil {
			sendRuleAckErr(ac, env.ID, "创建规则失败: "+ruleAckMessage(err))
			return
		}
		rl.ID = id
		hop := db.HopInput{NodeID: ac.nodeID, Mode: db.NormalizeForwardMode(r.Mode), ViaNodeID: ac.nodeID}
		if r.SrcPort > 0 {
			hop.DesiredPort = r.SrcPort
		}
		_, _, affected, err := db.RegenerateRule(tx, rl, []db.HopInput{hop}, nil)
		if err != nil {
			sendRuleAckErr(ac, env.ID, "生成规则失败: "+ruleAckMessage(err))
			return
		}
		allAffected = append(allAffected, affected...)
	}

	if err := tx.Commit(); err != nil {
		sendRuleAckFailure(ac, env.ID, err)
		return
	}
	if h.Redispatch != nil {
		go h.Redispatch(allAffected)
	}
	ackP, _ := json.Marshal(wsproto.RuleCmdAck{OK: true})
	ac.enqueueWrite(wsproto.Envelope{Type: wsproto.TypeRuleCmdAck, ID: env.ID, Payload: ackP})
}

// ruleAckMessage is what a failed rule command tells the node that sent it.
// A db.ChainError is about the command as asked — a port that is taken, a
// rule this node is not on — and is passed on as it is. Anything else is a
// statement that failed on the panel, and is said to be that: the node's
// operator would otherwise be told to fix a rule there is nothing wrong with.
func ruleAckMessage(err error) string {
	if db.IsChainError(err) {
		return err.Error()
	}
	return "面板内部错误: " + err.Error()
}

func sendRuleAckFailure(ac *agentConn, id string, err error) {
	sendRuleAckErr(ac, id, ruleAckMessage(err))
}

// sendRuleAckLookupErr acks a failed lookup of a row the command depends on:
// notFound when the row does not exist, ruleAckMessage otherwise.
func sendRuleAckLookupErr(ac *agentConn, id string, err error, notFound string) {
	if errors.Is(err, sql.ErrNoRows) {
		sendRuleAckErr(ac, id, notFound)
		return
	}
	sendRuleAckFailure(ac, id, err)
}

func sendRuleAckErr(ac *agentConn, id, msg string) {
	ackP, _ := json.Marshal(wsproto.RuleCmdAck{OK: false, Error: msg})
	ac.enqueueWrite(wsproto.Envelope{Type: wsproto.TypeRuleCmdAck, ID: id, Payload: ackP})
}
