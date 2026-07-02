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
	applyAckTimeout = 30 * time.Second
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
	nodeID  int64
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
		ws:      ws,
		writeCh: make(chan []byte, 16),
		closed:  make(chan struct{}),
		pending: make(map[string]chan json.RawMessage),
	}
	h.registerConn(ac)
	defer h.unregisterConn(ac)

	poolSize := 4
	if psStr, err := db.GetSetting(h.DB, "pool_size"); err == nil {
		if n, err := strconv.Atoi(psStr); err == nil && n >= 0 {
			poolSize = n
		}
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
	fillNodeRelayHosts(h.DB, node, connectIP, hello.ProbedV4, hello.ProbedV6)
	if hello.PortRange != "" {
		if err := db.ValidatePortRange(hello.PortRange); err == nil {
			_ = db.UpdateNodePortRange(h.DB, node.ID, hello.PortRange)
		}
	}

	go h.writerLoop(ac)
	h.readerLoop(ctx, ac, hello.LastAppliedRev)
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
	_ = db.MarkNodeOffline(h.DB, ac.nodeID)
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

func (h *Hub) readerLoop(parent context.Context, ac *agentConn, lastAppliedRev string) {
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
		switch env.Type {
		case wsproto.TypePing:
			pong, _ := json.Marshal(wsproto.Pong{TS: time.Now().UnixMilli()})
			ac.enqueueWrite(wsproto.Envelope{Type: wsproto.TypePong, ID: env.ID, Payload: pong})
		case wsproto.TypeCounters:
			var co wsproto.Counters
			if err := json.Unmarshal(env.Payload, &co); err != nil {
				log.Printf("hub: node %d malformed counters: %v", ac.nodeID, err)
				continue
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
				continue
			}
			entry, cerr := h.applyRuleHopEdit(ac.nodeID, e.RuleID, e.ListenPort, e.Mode, e.Comment)
			ack := wsproto.RuleCmdAck{OK: cerr == nil, Entry: entry}
			if cerr != nil {
				ack.Error = cerr.Error()
			}
			ackP, _ := json.Marshal(ack)
			ac.enqueueWrite(wsproto.Envelope{Type: wsproto.TypeRuleCmdAck, ID: env.ID, Payload: ackP})
		case wsproto.TypeRuleDelete:
			var dl wsproto.RuleDelete
			if err := json.Unmarshal(env.Payload, &dl); err != nil {
				sendRuleAckErr(ac, env.ID, "malformed payload")
				continue
			}
			cerr := h.applyRuleDelete(ac.nodeID, dl.RuleID)
			ack := wsproto.RuleCmdAck{OK: cerr == nil}
			if cerr != nil {
				ack.Error = cerr.Error()
			}
			ackP, _ := json.Marshal(ack)
			ac.enqueueWrite(wsproto.Envelope{Type: wsproto.TypeRuleCmdAck, ID: env.ID, Payload: ackP})
		case wsproto.TypeApplyAck, wsproto.TypeHelloAck, wsproto.TypeUpgradeAck, wsproto.TypeProbeAck:
			ac.dispatchAck(env)
		default:
			log.Printf("hub: node %d unknown frame type %q", ac.nodeID, env.Type)
		}
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
		return "", fmt.Errorf("node %d not connected", nodeID)
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
			return "", fmt.Errorf("malformed apply_ack: %w", err)
		}
		if !ack.OK {
			return "", fmt.Errorf("apply rejected: %s", ack.Error)
		}
		return ack.Warning, nil
	case <-time.After(applyAckTimeout):
		return "", errors.New("apply_ack timeout")
	case <-ac.closed:
		return "", errors.New("connection closed before ack")
	}
}

func (h *Hub) SendProbe(nodeID int64, target string) (wsproto.ProbeAck, error) {
	h.mu.RLock()
	ac, ok := h.conns[nodeID]
	h.mu.RUnlock()
	if !ok {
		return wsproto.ProbeAck{}, fmt.Errorf("node %d not connected", nodeID)
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

	payload, _ := json.Marshal(wsproto.Probe{Target: target})
	ac.enqueueWrite(wsproto.Envelope{Type: wsproto.TypeProbe, ID: id, Payload: payload})

	select {
	case raw := <-ch:
		var ack wsproto.ProbeAck
		if err := json.Unmarshal(raw, &ack); err != nil {
			return wsproto.ProbeAck{}, fmt.Errorf("malformed probe_ack: %w", err)
		}
		return ack, nil
	case <-time.After(10 * time.Second):
		return wsproto.ProbeAck{}, errors.New("probe timeout")
	case <-ac.closed:
		return wsproto.ProbeAck{}, errors.New("connection closed")
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

// fillNodeRelayHosts seeds relay_host/relay_host_v6 for a node that hasn't
// had them set yet. connectIP (the address the panel observed this WS
// connection arrive from) is authoritative for whichever family it belongs
// to — it reflects the address as seen after any NAT, unlike a locally
// self-probed address. The agent's self-probed address only fills the
// OTHER family, the one this connection didn't use. Never overwrites a
// manually-configured value (only fires when the DB field is still empty).
func fillNodeRelayHosts(d *sql.DB, node *db.Node, connectIP, probedV4, probedV6 string) {
	connectIsV6 := false
	if ip := net.ParseIP(connectIP); ip != nil {
		connectIsV6 = ip.To4() == nil
	}
	if node.RelayHost == "" {
		if !connectIsV6 && connectIP != "" {
			_ = db.UpdateNodeRelayHost(d, node.ID, connectIP)
		} else if probedV4 != "" {
			_ = db.UpdateNodeRelayHost(d, node.ID, probedV4)
		}
	}
	if node.RelayHostV6 == "" {
		if connectIsV6 && connectIP != "" {
			_ = db.UpdateNodeRelayHostV6(d, node.ID, connectIP)
		} else if probedV6 != "" {
			_ = db.UpdateNodeRelayHostV6(d, node.ID, probedV6)
		}
	}
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
// Per-node usage accumulates raw bytes on every physical hop. Global usage
// accumulates bytes weighted by the hop's traffic_multiplier so a physical
// node in a high-cost chain is priced differently from the same node in a
// low-cost chain.
func (h *Hub) applyCounters(nodeID int64, samples []wsproto.CounterSample) {
	hopMap, err := db.RuleHopMapByNode(h.DB, nodeID)
	if err != nil {
		log.Printf("hub: node %d load rule hop map for counters: %v", nodeID, err)
		return
	}
	hopMultipliers, err := db.HopMultipliers(h.DB)
	if err != nil {
		log.Printf("hub: load hop multipliers: %v", err)
		hopMultipliers = map[int64]map[int64]float64{}
	}
	ruleMap, _ := db.RulesByID(h.DB)
	if ruleMap == nil {
		ruleMap = map[int64]*db.Rule{}
	}

	node, err := db.GetNode(h.DB, nodeID)
	if err != nil {
		log.Printf("hub: node %d load for billing direction: %v", nodeID, err)
		return
	}

	type userNode struct{ userID, nodeID int64 }
	touched := map[userNode]bool{}
	cycleChecked := map[int64]bool{}
	billingRates := map[int64]float64{}

	for _, s := range samples {
		// Pre-v0.33 agents send BytesDelta without direction; fall back to it
		// so traffic accounting continues while the node upgrades.
		if s.BytesUp == 0 && s.BytesDown == 0 && s.BytesDelta > 0 {
			s.BytesUp = s.BytesDelta
		}
		totalDelta := s.BytesUp + s.BytesDown
		billedDelta := totalDelta
		if node.Unidirectional {
			billedDelta = s.BytesUp
		}
		key := fmt.Sprintf("%s/%d", s.Proto, s.ListenPort)
		rh, ok := hopMap[key]
		if !ok {
			log.Printf("hub: node %d counters sample for %s/%d matched no rule_hop row (rule may have been deleted)", nodeID, s.Proto, s.ListenPort)
			continue
		}
		r := ruleMap[rh.RuleID]

		// Compute weighted = billedDelta × mult × billingRate.
		// Direct nodes use the node's own rate_multiplier; composite hops
		// use the per-hop traffic_multiplier from node_hops.
		weighted := billedDelta
		var userID int64
		hasOwner := r != nil && r.OwnerID.Valid && billedDelta > 0
		if hasOwner {
			userID = r.OwnerID.Int64

			if !cycleChecked[userID] {
				cycleChecked[userID] = true
				if u, err := db.GetUserByID(h.DB, userID); err == nil {
					if reset, _ := db.CheckAndResetTrafficCycle(h.DB, u); reset {
						if u.Disabled && u.DisableReason.Valid && u.DisableReason.String == "流量超额" {
							_ = db.SetUserDisabled(h.DB, userID, false, "")
							if nodes, err := db.DistinctUserNodes(h.DB, userID); err == nil && h.Redispatch != nil {
								go h.Redispatch(nodes)
							}
						}
					}
				}
			}

			mult := node.RateMultiplier
			if mult <= 0 {
				mult = 1.0
			}
			if r.NodeID != nodeID {
				if hm, ok := hopMultipliers[r.NodeID]; ok {
					if m, ok := hm[nodeID]; ok {
						mult = m
					}
				}
			}
			billingRate, ok := billingRates[userID]
			if !ok {
				billingRate = 1.0
				if u, err := db.GetUserByID(h.DB, userID); err == nil && u.BillingRate > 0 {
					billingRate = u.BillingRate
				}
				billingRates[userID] = billingRate
			}
			weighted = int64(math.Round(float64(billedDelta) * mult * billingRate))
		}

		// rule_hops: last_bytes are raw (for speed display), total_bytes is
		// the cumulative billed amount so it matches quota accounting.
		if _, err := h.DB.Exec(`UPDATE rule_hops SET last_bytes=?, last_bytes_up=?, last_bytes_down=?, total_bytes=total_bytes+? WHERE id=?`,
			totalDelta, s.BytesUp, s.BytesDown, weighted, rh.ID); err != nil {
			log.Printf("hub: node %d counters update for %s/%d: %v", nodeID, s.Proto, s.ListenPort, err)
			continue
		}

		if !hasOwner || weighted <= 0 {
			continue
		}

		if err := db.AddUserNodeTraffic(h.DB, userID, nodeID, weighted); err != nil {
			log.Printf("hub: user %d node %d per-node traffic add: %v", userID, nodeID, err)
		}
		if r.NodeID != nodeID && rh.Position == 0 {
			if err := db.AddUserNodeTraffic(h.DB, userID, r.NodeID, weighted); err != nil {
				log.Printf("hub: user %d composite node %d per-node traffic add: %v", userID, r.NodeID, err)
			}
		}
		if err := db.AddUserTraffic(h.DB, userID, weighted); err != nil {
			log.Printf("hub: user %d traffic add: %v", userID, err)
			continue
		}

		touched[userNode{userID, nodeID}] = true
	}

	deltas := make([]counterDelta, 0, len(samples))
	for _, s := range samples {
		deltas = append(deltas, counterDelta{
			proto:         s.Proto,
			listenPortStr: strconv.Itoa(s.ListenPort),
			bytesUp:       s.BytesUp,
			bytesDown:     s.BytesDown,
		})
	}
	h.speedCache.update(nodeID, deltas)

	if h.OnTrafficUpdate != nil {
		for un := range touched {
			h.OnTrafficUpdate(un.userID, un.nodeID)
		}
	}
}

// applyRuleHopEdit folds a node-reported edit to its hop in ruleID back
// into the rule skeleton and re-dispatches every node the regeneration
// touched, returning the rule's copyable entry endpoint. The hop is located
// by (ruleID, nodeID): a rule can't repeat a node, so that pair is unique.
func (h *Hub) applyRuleHopEdit(nodeID, ruleID int64, listenPort int, mode, comment string) (string, error) {
	r, err := db.GetRule(h.DB, ruleID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("规则不存在")
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
		in := db.HopInput{NodeID: hp.NodeID, Mode: hp.Mode}
		if hp.NodeID == nodeID {
			found = true
			in.DesiredPort = listenPort
			in.Mode = db.NormalizeForwardMode(mode)
			in.Comment = comment
		}
		inputs[i] = in
	}
	if !found {
		return "", fmt.Errorf("节点不在该规则上")
	}
	tx, err := h.DB.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	entry, affected, err := db.RegenerateRule(tx, r, inputs, nil)
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
		return fmt.Errorf("节点不在该规则上")
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
		sendRuleAckErr(ac, env.ID, "节点不存在")
		return
	}
	if node.OwnerID == nil {
		sendRuleAckErr(ac, env.ID, "节点无归属用户")
		return
	}
	ownerID := *node.OwnerID
	u, err := db.GetUserByID(h.DB, ownerID)
	if err != nil {
		sendRuleAckErr(ac, env.ID, "归属用户不存在")
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
	total, _ := db.CountRulesForUser(h.DB, ownerID)
	if total >= u.MaxForwards {
		sendRuleAckErr(ac, env.ID, fmt.Sprintf("超出用户最大转发数（%d）", u.MaxForwards))
		return
	}

	tx, err := h.DB.Begin()
	if err != nil {
		sendRuleAckErr(ac, env.ID, "内部错误")
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
		sendRuleAckErr(ac, env.ID, err.Error())
		return
	}
	rl.ID = id

	hop := db.HopInput{NodeID: ac.nodeID, Mode: db.NormalizeForwardMode(req.Mode)}
	if req.ListenPort > 0 {
		hop.DesiredPort = req.ListenPort
	}
	entry, affected, err := db.RegenerateRule(tx, rl, []db.HopInput{hop}, nil)
	if err != nil {
		sendRuleAckErr(ac, env.ID, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		sendRuleAckErr(ac, env.ID, "提交失败")
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
		sendRuleAckErr(ac, env.ID, "节点不存在")
		return
	}
	if node.OwnerID == nil {
		sendRuleAckErr(ac, env.ID, "节点无归属用户")
		return
	}
	rl, err := db.GetRule(h.DB, req.RuleID)
	if err != nil {
		sendRuleAckErr(ac, env.ID, "规则不存在")
		return
	}
	if !rl.OwnerID.Valid || rl.OwnerID.Int64 != *node.OwnerID {
		sendRuleAckErr(ac, env.ID, "无权操作该规则")
		return
	}
	// Only single-hop rules are editable from a node
	hops, err := db.ListRuleHops(h.DB, req.RuleID)
	if err != nil {
		sendRuleAckErr(ac, env.ID, "读取跳数失败")
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
		sendRuleAckErr(ac, env.ID, "内部错误")
		return
	}
	defer tx.Rollback()

	if err := db.UpdateRuleHeader(tx, rl); err != nil {
		sendRuleAckErr(ac, env.ID, err.Error())
		return
	}
	hop := db.HopInput{NodeID: ac.nodeID, Mode: db.NormalizeForwardMode(req.Mode)}
	if req.ListenPort > 0 {
		hop.DesiredPort = req.ListenPort
	}
	entry, affected, err := db.RegenerateRule(tx, rl, []db.HopInput{hop}, nil)
	if err != nil {
		sendRuleAckErr(ac, env.ID, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		sendRuleAckErr(ac, env.ID, "提交失败")
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
		sendRuleAckErr(ac, env.ID, "节点不存在")
		return
	}
	if node.OwnerID == nil {
		sendRuleAckErr(ac, env.ID, "节点无归属用户")
		return
	}
	ownerID := *node.OwnerID
	u, err := db.GetUserByID(h.DB, ownerID)
	if err != nil {
		sendRuleAckErr(ac, env.ID, "归属用户不存在")
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

	total, _ := db.CountRulesForUser(h.DB, ownerID)
	if total+len(req.Rules) > u.MaxForwards {
		sendRuleAckErr(ac, env.ID, fmt.Sprintf("超出用户最大转发数（%d），当前 %d 条，迁入 %d 条", u.MaxForwards, total, len(req.Rules)))
		return
	}

	tx, err := h.DB.Begin()
	if err != nil {
		sendRuleAckErr(ac, env.ID, "内部错误")
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
			sendRuleAckErr(ac, env.ID, fmt.Sprintf("创建规则失败: %v", err))
			return
		}
		rl.ID = id
		hop := db.HopInput{NodeID: ac.nodeID, Mode: db.NormalizeForwardMode(r.Mode)}
		if r.SrcPort > 0 {
			hop.DesiredPort = r.SrcPort
		}
		_, affected, err := db.RegenerateRule(tx, rl, []db.HopInput{hop}, nil)
		if err != nil {
			sendRuleAckErr(ac, env.ID, fmt.Sprintf("生成规则失败: %v", err))
			return
		}
		allAffected = append(allAffected, affected...)
	}

	if err := tx.Commit(); err != nil {
		sendRuleAckErr(ac, env.ID, "提交失败")
		return
	}
	if h.Redispatch != nil {
		go h.Redispatch(allAffected)
	}
	ackP, _ := json.Marshal(wsproto.RuleCmdAck{OK: true})
	ac.enqueueWrite(wsproto.Envelope{Type: wsproto.TypeRuleCmdAck, ID: env.ID, Payload: ackP})
}

func sendRuleAckErr(ac *agentConn, id, msg string) {
	ackP, _ := json.Marshal(wsproto.RuleCmdAck{OK: false, Error: msg})
	ac.enqueueWrite(wsproto.Envelope{Type: wsproto.TypeRuleCmdAck, ID: id, Payload: ackP})
}
