package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
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

	// OnTrafficUpdate, when set, is invoked once per tenant whose usage was
	// advanced by a counters batch. The Hub stays a pure transport: it knows
	// how to accumulate bytes but delegates quota policy (and the re-dispatch
	// it may trigger) to the owner that wires this callback, so the dispatch
	// path is never imported here.
	OnTrafficUpdate func(tenantID int64)

	// Redispatch re-pushes kernel state to a set of nodes after the hub
	// mutates chain state on their behalf. Like OnTrafficUpdate it keeps the
	// hub transport-only: the hub knows which nodes a chain edit touched but
	// delegates the actual dispatch to the owner that wires this, so the
	// dispatch path is never imported here.
	Redispatch func(nodeIDs []int64)

	mu    sync.RWMutex
	conns map[int64]*agentConn
}

func NewHub(d *sql.DB) *Hub {
	return &Hub{DB: d, conns: make(map[int64]*agentConn)}
}

type agentConn struct {
	nodeID  int64
	ws      *websocket.Conn
	writeCh chan []byte
	closed  chan struct{}

	// closeOnce guards closed so the multiple close paths (a displaced
	// conn in registerConn, unregisterConn on disconnect, and Hub.Close
	// on shutdown) can race without double-closing the channel (which
	// would panic).
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
// message type. Returns when the client disconnects, hello fails, or
// the read deadline expires.
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

	// Register the conn before sending hello_ack so both sides can rely
	// on the invariant "hello_ack visible ⇒ conn is in the hub map":
	// the agent may immediately push counters/register_local, and panel
	// dispatch may immediately call SendApplyRuleset, with no
	// goroutine-startup window where the lookup would miss.
	ac := &agentConn{
		nodeID:  node.ID,
		ws:      ws,
		writeCh: make(chan []byte, 16),
		closed:  make(chan struct{}),
		pending: make(map[string]chan json.RawMessage),
	}
	h.registerConn(ac)
	defer h.unregisterConn(ac)

	ackPayload, _ := json.Marshal(wsproto.HelloAck{NodeID: node.ID, Name: node.Name})
	if err := writeEnvelope(ctx, ws, wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: helloEnv.ID, Payload: ackPayload}); err != nil {
		ws.Close(websocket.StatusInternalError, "ack write failed")
		return
	}

	if err := db.MarkNodeOnline(h.DB, node.ID, hello.AgentVersion); err != nil {
		log.Printf("hub: MarkNodeOnline: %v", err)
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

// Close gracefully shuts down every agent connection by sending a
// StatusGoingAway close frame, so agents distinguish an intentional
// panel shutdown from a crash and can reconnect without alarm. Bounded
// by the caller's expectation of a quick shutdown — each close is
// best-effort and non-blocking.
func (h *Hub) Close() {
	h.mu.Lock()
	conns := make([]*agentConn, 0, len(h.conns))
	for _, ac := range h.conns {
		conns = append(conns, ac)
	}
	h.conns = make(map[int64]*agentConn)
	h.mu.Unlock()

	for _, ac := range conns {
		// Signal the reader/writer loops to stop, then send a polite
		// close frame. The websocket Close is best-effort: if the conn
		// is already broken it returns an error we don't care about.
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
		case wsproto.TypeTuiSegmentChanged:
			var tsc wsproto.TuiSegmentChanged
			if err := json.Unmarshal(env.Payload, &tsc); err != nil {
				log.Printf("hub: node %d malformed tui_segment_changed: %v", ac.nodeID, err)
				continue
			}
			fjb, _ := json.Marshal(tsc.Forwards)
			if err := db.UpsertTuiSnapshot(h.DB, ac.nodeID, string(fjb)); err != nil {
				log.Printf("hub: node %d upsert tui snapshot: %v", ac.nodeID, err)
			}
		case wsproto.TypePanelSegmentEdit:
			var pse wsproto.PanelSegmentEdit
			if err := json.Unmarshal(env.Payload, &pse); err != nil {
				log.Printf("hub: node %d malformed panel_segment_edit: %v", ac.nodeID, err)
				continue
			}
			h.applyPanelEdits(ac.nodeID, pse.Forwards)
		case wsproto.TypeChainHopEdit:
			var e wsproto.ChainHopEdit
			if err := json.Unmarshal(env.Payload, &e); err != nil {
				sendChainAckErr(ac, env.ID, "malformed payload")
				continue
			}
			entry, cerr := h.applyChainHopEdit(ac.nodeID, e.ChainID, e.ListenPort, e.Mode, e.Comment)
			ack := wsproto.ChainCmdAck{OK: cerr == nil, Entry: entry}
			if cerr != nil {
				ack.Error = cerr.Error()
			}
			ackP, _ := json.Marshal(ack)
			ac.enqueueWrite(wsproto.Envelope{Type: wsproto.TypeChainCmdAck, ID: env.ID, Payload: ackP})
		case wsproto.TypeChainDelete:
			var dl wsproto.ChainDelete
			if err := json.Unmarshal(env.Payload, &dl); err != nil {
				sendChainAckErr(ac, env.ID, "malformed payload")
				continue
			}
			cerr := h.applyChainDelete(ac.nodeID, dl.ChainID)
			ack := wsproto.ChainCmdAck{OK: cerr == nil}
			if cerr != nil {
				ack.Error = cerr.Error()
			}
			ackP, _ := json.Marshal(ack)
			ac.enqueueWrite(wsproto.Envelope{Type: wsproto.TypeChainCmdAck, ID: env.ID, Payload: ackP})
		case wsproto.TypeRegisterLocal:
			var rl wsproto.RegisterLocal
			if err := json.Unmarshal(env.Payload, &rl); err != nil {
				sendAckErr(ac, env.ID, wsproto.TypeRegisterLocalAck, "malformed payload")
				continue
			}
			imported, err := h.handleRegisterLocal(ac.nodeID, rl.Forwards)
			if err != nil {
				sendAckErr(ac, env.ID, wsproto.TypeRegisterLocalAck, err.Error())
				continue
			}
			ackP, _ := json.Marshal(wsproto.RegisterLocalAck{Imported: imported})
			ac.enqueueWrite(wsproto.Envelope{Type: wsproto.TypeRegisterLocalAck, ID: env.ID, Payload: ackP})
		case wsproto.TypeApplyAck, wsproto.TypeHelloAck, wsproto.TypeRegisterLocalAck:
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

func (h *Hub) SendApplyRuleset(nodeID int64, rules []nft.Rule, rev string) error {
	h.mu.RLock()
	ac, ok := h.conns[nodeID]
	h.mu.RUnlock()
	if !ok {
		return fmt.Errorf("node %d not connected", nodeID)
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
			return fmt.Errorf("malformed apply_ack: %w", err)
		}
		if !ack.OK {
			return fmt.Errorf("apply rejected: %s", ack.Error)
		}
		return nil
	case <-time.After(applyAckTimeout):
		return errors.New("apply_ack timeout")
	case <-ac.closed:
		return errors.New("connection closed before ack")
	}
}

// Helpers --------------------------------------------------------------

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

// handleRegisterLocal persists the agent's tui-segment forwards into the
// panel's forwards table on first call; subsequent calls (e.g. the ack was
// lost on the wire and the agent retries) return an empty Imported slice
// so the agent still clears its local tui segment. The
// nodes.local_migrated_at stamp is the idempotency anchor: without it a
// retry would duplicate-INSERT and trip the (node_id, proto, listen_port)
// UNIQUE constraint. Forwards imported this way are admin-owned (no
// tenant/tunnel) — they're whatever the operator was running directly on
// the daemon before the panel took over.
func (h *Hub) handleRegisterLocal(nodeID int64, forwards []wsproto.Forward) ([]wsproto.ImportedForward, error) {
	n, err := db.GetNode(h.DB, nodeID)
	if err != nil {
		return nil, err
	}
	if n.LocalMigratedAt != nil {
		return []wsproto.ImportedForward{}, nil
	}
	tx, err := h.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := make([]wsproto.ImportedForward, 0, len(forwards))
	for _, f := range forwards {
		res, err := tx.Exec(
			`INSERT INTO forwards(node_id, tenant_id, tunnel_id, proto, listen_port, target_ip, target_port, comment, created_at, mode) VALUES (?, NULL, NULL, ?, ?, ?, ?, ?, ?, ?)`,
			nodeID, f.Proto, f.ListenPort, f.TargetIP, f.TargetPort, f.Comment, time.Now().Unix(), db.NormalizeForwardMode(f.Mode))
		if err != nil {
			return nil, err
		}
		id, _ := res.LastInsertId()
		out = append(out, wsproto.ImportedForward{ListenPort: f.ListenPort, Proto: f.Proto, RuleID: id})
	}
	if _, err := tx.Exec(`UPDATE nodes SET local_migrated_at=? WHERE id=? AND local_migrated_at IS NULL`, time.Now().Unix(), nodeID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// applyCounters folds per-rule bytes_delta into the forwards table and the
// owning tenant's usage. last_bytes is the most recent delta (UI surfaces it
// as "current rate input"); total_bytes is monotonically accumulated. The
// (node_id, listen_port, proto) tuple identifies the rule — there is no
// rule_id on the wire because agent restarts re-key the same forward.
//
// Each sample is resolved to its forward row so we learn the forward id and
// the tenant it belongs to; tenant-owned bytes are added to the tenant's
// usage. Touched tenants are collected and OnTrafficUpdate fires once per
// tenant after the loop rather than once per sample, so a batch carrying many
// of a tenant's rules triggers a single quota evaluation instead of N.
//
// Per-sample failures (DB error, or a lookup miss meaning the rule was
// deleted on the panel side between the agent's count and the frame's
// arrival) are logged and the loop continues: counters are recoverable on
// the next frame, but abandoning the rest of the batch on the first hiccup
// would lose observability for unrelated rules.
func (h *Hub) applyCounters(nodeID int64, samples []wsproto.CounterSample) {
	fwdMap, err := db.ForwardMapByNode(h.DB, nodeID)
	if err != nil {
		log.Printf("hub: node %d load forward map for counters: %v", nodeID, err)
		return
	}
	touched := map[int64]bool{}
	for _, s := range samples {
		key := fmt.Sprintf("%s/%d", s.Proto, s.ListenPort)
		f, ok := fwdMap[key]
		if !ok {
			log.Printf("hub: node %d counters sample for %s/%d matched no forward row (rule may have been deleted)", nodeID, s.Proto, s.ListenPort)
			continue
		}
		if _, err := h.DB.Exec(`UPDATE forwards SET last_bytes=?, total_bytes=total_bytes+? WHERE id=?`,
			s.BytesDelta, s.BytesDelta, f.ID); err != nil {
			log.Printf("hub: node %d counters update for %s/%d: %v", nodeID, s.Proto, s.ListenPort, err)
			continue
		}
		if f.TenantID.Valid && s.BytesDelta > 0 {
			if err := db.AddTenantTraffic(h.DB, f.TenantID.Int64, s.BytesDelta); err != nil {
				log.Printf("hub: tenant %d traffic add: %v", f.TenantID.Int64, err)
				continue
			}
			touched[f.TenantID.Int64] = true
		}
	}
	if h.OnTrafficUpdate != nil {
		for tid := range touched {
			h.OnTrafficUpdate(tid)
		}
	}
}

// applyPanelEdits folds a node's edits to its panel-segment forwards back
// into the forwards table so the server becomes their authority. Each
// forward is located by (node_id, proto, listen_port). Only non-chain rows
// are updated: a chained hop's listen_port/target form a relay skeleton
// owned by chain orchestration (RegenerateChain wires neighbor hops
// together), so a node-side edit must never rewrite it — UpdateForward's
// chain_id IS NULL guard is the second backstop behind this ChainID.Valid
// skip.
//
// Per-edit failures (DB error, or a lookup miss meaning the forward was
// deleted on the panel side between the node's snapshot and the frame's
// arrival) are logged and the loop continues, mirroring applyCounters: one
// bad row shouldn't abandon the rest of the batch.
func (h *Hub) applyPanelEdits(nodeID int64, forwards []wsproto.Forward) {
	fwdMap, err := db.ForwardMapByNode(h.DB, nodeID)
	if err != nil {
		log.Printf("hub: node %d load forward map for panel edits: %v", nodeID, err)
		return
	}

	incoming := make(map[string]bool, len(forwards))
	for _, f := range forwards {
		key := fmt.Sprintf("%s/%d", f.Proto, f.ListenPort)
		incoming[key] = true
		existing, ok := fwdMap[key]
		if !ok {
			if _, err := db.CreateForward(h.DB, &db.Forward{
				NodeID:     nodeID,
				Proto:      f.Proto,
				ListenPort: f.ListenPort,
				TargetIP:   f.TargetIP,
				TargetPort: f.TargetPort,
				Comment:    f.Comment,
				Mode:       db.NormalizeForwardMode(f.Mode),
			}); err != nil {
				log.Printf("hub: node %d panel edit create %s/%d: %v", nodeID, f.Proto, f.ListenPort, err)
			}
			continue
		}
		if existing.ChainID.Valid {
			continue
		}
		if _, err := db.UpdateForward(h.DB, nodeID, f.Proto, f.ListenPort, f.TargetIP, f.TargetPort, f.Comment, f.Mode); err != nil {
			log.Printf("hub: node %d panel edit update for %s/%d: %v", nodeID, f.Proto, f.ListenPort, err)
		}
	}

	for key, existing := range fwdMap {
		if incoming[key] {
			continue
		}
		if existing.ChainID.Valid {
			continue
		}
		if _, err := db.DeleteForward(h.DB, existing.ID); err != nil {
			log.Printf("hub: node %d panel edit delete %s: %v", nodeID, key, err)
		}
	}
}

// applyChainHopEdit folds a node-reported edit to its hop in chainID back
// into the chain skeleton and re-dispatches every node the regeneration
// touched, returning the chain's copyable entry endpoint. The hop is located
// by (chainID, nodeID): a chain can't repeat a node, so that pair is unique.
// Only listen_port/mode/comment are editable — target/proto stay owned by
// chain orchestration, which is why RegenerateChain recomputes targets and
// uses chain.proto. A node may only edit a chain it actually participates in.
func (h *Hub) applyChainHopEdit(nodeID, chainID int64, listenPort int, mode, comment string) (string, error) {
	c, err := db.GetChain(h.DB, chainID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("链路不存在")
	}
	if err != nil {
		return "", err
	}
	hops, err := db.ListChainHops(h.DB, chainID)
	if err != nil {
		return "", err
	}
	found := false
	inputs := make([]db.HopInput, len(hops))
	for i, hp := range hops {
		in := db.HopInput{NodeID: hp.NodeID, TunnelID: hp.TunnelID, Mode: hp.Mode}
		if hp.NodeID == nodeID {
			found = true
			in.DesiredPort = listenPort
			in.Mode = db.NormalizeForwardMode(mode)
			in.Comment = comment
		}
		inputs[i] = in
	}
	if !found {
		return "", fmt.Errorf("节点不在该链路上")
	}
	tx, err := h.DB.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	entry, affected, err := db.RegenerateChain(tx, c, inputs, nil)
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

// applyChainDelete removes the whole chain (all hops on all nodes) on behalf
// of a node that participates in it, then re-dispatches every node that ran
// its forwards so the deleted rules leave the kernel.
func (h *Hub) applyChainDelete(nodeID, chainID int64) error {
	hops, err := db.ListChainHops(h.DB, chainID)
	if err != nil {
		return err
	}
	onChain := false
	for _, hp := range hops {
		if hp.NodeID == nodeID {
			onChain = true
			break
		}
	}
	if !onChain {
		return fmt.Errorf("节点不在该链路上")
	}
	nodes, err := db.DeleteChain(h.DB, chainID)
	if err != nil {
		return err
	}
	if h.Redispatch != nil {
		h.Redispatch(nodes)
	}
	return nil
}

func sendChainAckErr(ac *agentConn, id, msg string) {
	ackP, _ := json.Marshal(wsproto.ChainCmdAck{OK: false, Error: msg})
	ac.enqueueWrite(wsproto.Envelope{Type: wsproto.TypeChainCmdAck, ID: id, Payload: ackP})
}

// sendAckErr writes a {error: msg} payload on a typed ack envelope so the
// agent can decode it as the appropriate RegisterLocalAck shape (its
// Error string `json:"error,omitempty"` field captures the message).
func sendAckErr(ac *agentConn, id, ackType, msg string) {
	p, _ := json.Marshal(map[string]string{"error": msg})
	ac.enqueueWrite(wsproto.Envelope{Type: ackType, ID: id, Payload: p})
}
