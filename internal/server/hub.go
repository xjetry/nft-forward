package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"nft-forward/internal/accounting"
	"nft-forward/internal/db"
	"nft-forward/internal/nft"
	"nft-forward/internal/rulechange"
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

	// OnTrafficUpdate 在一批计数入账后，对这一批向其授权已用量或落地出口账记过账的
	// 每个属主各调用一次。全局已用量只在入口跳记账，入口跳总是一段的段首、同时记
	// 授权账，所以全局配额的变化也在其中。配额判定与它可能引起的重推属于面板，由
	// 接线的一方负责。
	OnTrafficUpdate func(userID int64)

	// Rules 执行节点发来的规则命令（见 handleRuleCommand）。Hub 自己不判定谁能改
	// 什么、也不碰规则表。
	Rules *rulechange.Changes

	// Redispatch 在一条规则命令改动了规则之后被调用，参数是规则集因此变化的节点。
	// 它必须立即返回：调用发生在发命令那台节点的读循环里，而下发要等节点应答，应答
	// 正是由这个读循环收的。
	Redispatch func(nodeIDs []int64)

	mu         sync.RWMutex
	conns      map[int64]*agentConn
	speedCache *speedCache
}

func NewHub(d *sql.DB) *Hub {
	return &Hub{DB: d, Rules: rulechange.New(d), conns: make(map[int64]*agentConn), speedCache: newSpeedCache()}
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

// IsOnline 报告节点此刻有没有登记着的连接。面板对外的在线状态读的是 nodes.online
// 列（连接登记与注销时写），不读这里；这个方法是给测试观察 Hub 自己的登记表用的。
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

// agentPeer 是一次握手认定出来的对端：它是哪个节点，以及这条连接从哪里来。
type agentPeer struct {
	node *db.Node
	// origin 写进 nodes.address，供页面显示这条连接的来源。
	origin string
	// observedIP 是面板在这条连接上观察到的对端网络地址，作为回填数据面地址
	// （relay_host）的候选。没有网络地址的连接（本机入口）留空，回填就只用 agent
	// 自己探测到的出口地址。
	observedIP string
}

// localAgentOrigin 是本机入口的连接在 nodes.address 里的来源标记。
const localAgentOrigin = "本机"

// helloLookupFailed 是认定对端的那次查找没能执行时回给对端的拒绝原因。“token 不认识”
// “面板没有 self 节点”是关于那一行的回答，而一次没能执行的查找对那一行一无所知：
// 答成前者，节点的操作者会去换一个本来有效的 token。查找的错误原文只进面板日志、
// 不回给对端——远程入口上对端此时还没有通过认证。
const helloLookupFailed = "panel could not look up the node; see the panel's log"

// ServeWS 是对外的 agent 入口（HTTP 路由上的 /v1/agents）：节点凭 hello 里的 token
// 认定，token 对不上任何节点就拒绝。空 token 不认出任何节点（lookupNodeBySecret），
// 所以没有 token 的 self 节点不可能从这里接入。
//
// 被停用的节点照样可以接入（两个入口都是）。停用的含义是“不承载转发”，由压制条件
// 落实：经过它的规则不下发，它收到的规则集是空的。它必须连得上才收得到这次撤销——
// 拒绝它接入的话，离线期间被停用的节点重连不上，会一直转发停用之前的那份规则集，
// 而且流量不再入账。
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	h.serveAgent(w, r, func(hello wsproto.Hello) (agentPeer, string) {
		node, err := lookupNodeBySecret(h.DB, hello.NodeToken)
		if err != nil {
			log.Printf("hub: hello: look up the node by its token: %v", err)
			return agentPeer{}, helloLookupFailed
		}
		if node == nil {
			return agentPeer{}, "unknown or revoked token"
		}
		ip := extractIP(r)
		return agentPeer{node: node, origin: ip, observedIP: ip}, ""
	})
}

// ServeLocalWS 是只给本机 daemon 用的 agent 入口。它不看 token：能连到这个入口本身
// 就是认证——入口挂在 ListenLocalAgent 建的 unix socket 上，只有属主（root）可连，
// 对外的 HTTP 路由上没有它。从这里进来的连接一律认定为 self 节点，握手之后与远程
// 节点走同一条通道（下发、计数、探测、升级）。
func (h *Hub) ServeLocalWS(w http.ResponseWriter, r *http.Request) {
	h.serveAgent(w, r, func(wsproto.Hello) (agentPeer, string) {
		node, err := db.GetSelfNode(h.DB)
		if errors.Is(err, sql.ErrNoRows) {
			return agentPeer{}, "panel has no self node"
		}
		if err != nil {
			log.Printf("hub: local hello: look up the self node: %v", err)
			return agentPeer{}, helloLookupFailed
		}
		return agentPeer{node: node, origin: localAgentOrigin}, ""
	})
}

// serveAgent 是两个入口共用的会话：升级为 WebSocket，读必须先到的 hello，由 identify
// 认定对端（返回非空的拒绝原因则回 hello_ack 报错并断开），登记连接，然后进入读循环
// 按帧类型分发。两个入口只在“怎么认定对端”上不同。
func (h *Hub) serveAgent(w http.ResponseWriter, r *http.Request, identify func(wsproto.Hello) (agentPeer, string)) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // the peer is authenticated by identify, not by Origin
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

	peer, refusal := identify(hello)
	if refusal != "" {
		ack, _ := json.Marshal(wsproto.HelloAck{Error: refusal})
		writeEnvelope(ctx, ws, wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: helloEnv.ID, Payload: ack})
		ws.Close(websocket.StatusPolicyViolation, "hello refused")
		return
	}
	node := peer.node

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

	if err := db.MarkNodeOnline(h.DB, node.ID, hello.AgentVersion, hello.AgentSHA, peer.origin); err != nil {
		log.Printf("hub: MarkNodeOnline: %v", err)
	}
	h.settleHelloRelayHosts(node, peer.observedIP, hello)
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
	case wsproto.TypeRuleCreate, wsproto.TypeRuleUpdate, wsproto.TypeMigrateRules,
		wsproto.TypeRuleHopEdit, wsproto.TypeRuleDelete:
		h.handleRuleCommand(ac, env)
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
// this hello has been handled (serveAgent starts reading frames after it).
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

// lookupNodeBySecret 按接入凭据认出一台机器；没有这样的机器时返回 nil 节点、nil
// 错误，错误只表示查找没能执行。空凭据不认出任何机器：self 节点与组合的 secret 列
// 都是空串，空凭据若参与匹配就会认出它们。组合不跑 agent，即使行上残留着凭据也不
// 参与匹配。
func lookupNodeBySecret(d *sql.DB, secret string) (*db.Node, error) {
	if secret == "" {
		return nil, nil
	}
	var id int64
	err := d.QueryRow(`SELECT id FROM nodes WHERE secret=? AND node_type<>'composite'`, secret).Scan(&id)
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

// applyCounters 把节点上报的一批计数样本交给记账（accounting.Record），再把记账的
// 结果分发出去：速度缓存按规则属主归属每个样本，周期翻转的属主重推节点，配额账有
// 增长的属主交给 OnTrafficUpdate。账怎么记、记在哪里都在 accounting 里，Hub 只传递。
func (h *Hub) applyCounters(nodeID int64, wire []wsproto.CounterSample) {
	res, err := accounting.Record(h.DB, nodeID, accounting.SamplesFromWire(wire))
	if err != nil {
		log.Printf("hub: node %d counters: %v", nodeID, err)
	}
	for _, s := range res.Unmatched {
		log.Printf("hub: node %d counters sample for %s/%d matched no rule_hop row (rule may have been deleted)", nodeID, s.Proto, s.ListenPort)
	}

	// 把每个样本的速度归属到规则属主，按用户看的速度视图据此筛选。匹配不到规则跳
	// 或无属主的样本归属 0：仍算进节点总速度，只是不属于任何用户。
	deltas := make([]counterDelta, 0, len(wire))
	for _, s := range wire {
		deltas = append(deltas, counterDelta{
			proto:         s.Proto,
			listenPortStr: strconv.Itoa(s.ListenPort),
			bytesUp:       s.BytesUp,
			bytesDown:     s.BytesDown,
			ownerID:       res.OwnerOf(s.Proto, s.ListenPort),
		})
	}
	h.speedCache.update(nodeID, deltas)

	// 配额条件只在下发时求值：周期翻转把账本清零之后不重推，因配额用尽被排除的规则
	// 就一直不恢复。要重推的节点随翻转一起从记账返回（accounting.Result.Repush）。
	if h.Redispatch != nil && len(res.Repush) > 0 {
		go h.Redispatch(res.Repush)
	}

	// 配额判定（OnTrafficUpdate）可能回头经 Hub 重推规则集，重推要等这个 readerLoop
	// 自己去收的 apply_ack；放到另一个 goroutine 里，读循环才不会等自己——否则它要
	// 一直等到下发超时（applyAckTimeout），期间收不到这条连接上的任何帧。
	if h.OnTrafficUpdate != nil && len(res.QuotaOwners) > 0 {
		owners := res.QuotaOwners
		go func() {
			for _, uid := range owners {
				h.OnTrafficUpdate(uid)
			}
		}()
	}
}
