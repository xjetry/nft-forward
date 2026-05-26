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
			`INSERT INTO forwards(node_id, tenant_id, tunnel_id, proto, listen_port, target_ip, target_port, comment, created_at) VALUES (?, NULL, NULL, ?, ?, ?, ?, ?, ?)`,
			nodeID, f.Proto, f.ListenPort, f.TargetIP, f.TargetPort, f.Comment, time.Now().Unix())
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

// applyCounters folds per-rule bytes_delta into the forwards table:
// last_bytes is the most recent delta (UI surfaces it as "current rate
// input"); total_bytes is monotonically accumulated. The (node_id,
// listen_port, proto) tuple identifies the rule — there is no rule_id on
// the wire because agent restarts re-key the same forward.
//
// Per-sample failures (DB error, or zero-row match meaning the rule was
// deleted on the panel side between the agent's count and the frame's
// arrival) are logged and the loop continues: counters are recoverable
// on the next frame, but abandoning the rest of the batch on the first
// hiccup would lose observability for unrelated rules.
func (h *Hub) applyCounters(nodeID int64, samples []wsproto.CounterSample) {
	for _, s := range samples {
		res, err := h.DB.Exec(
			`UPDATE forwards SET last_bytes=?, total_bytes=total_bytes+? WHERE node_id=? AND listen_port=? AND proto=?`,
			s.BytesDelta, s.BytesDelta, nodeID, s.ListenPort, s.Proto)
		if err != nil {
			log.Printf("hub: node %d counters update for %s/%d: %v", nodeID, s.Proto, s.ListenPort, err)
			continue
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			log.Printf("hub: node %d counters sample for %s/%d matched no forward row (rule may have been deleted)", nodeID, s.Proto, s.ListenPort)
		}
	}
}

// sendAckErr writes a {error: msg} payload on a typed ack envelope so the
// agent can decode it as the appropriate RegisterLocalAck shape (its
// Error string `json:"error,omitempty"` field captures the message).
func sendAckErr(ac *agentConn, id, ackType, msg string) {
	p, _ := json.Marshal(map[string]string{"error": msg})
	ac.enqueueWrite(wsproto.Envelope{Type: ackType, ID: id, Payload: p})
}
