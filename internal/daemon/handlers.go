package daemon

import (
	"context"
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

	"nft-forward/internal/forward"
	"nft-forward/internal/nft"
	"nft-forward/internal/portutil"
	"nft-forward/internal/wsproto"
)

// Daemon holds the in-memory owner-segmented ruleset and data-plane wiring
// shared by the HTTP handlers and the lifecycle code.
// Fields are unexported; production callers go through New().
type Daemon struct {
	socketPath  string
	statePath   string
	groupName   string
	dp          Dataplane
	legacyPaths LegacyMigrationPaths
	countersFn  func() ([]forward.Counter, error)
	resolveFn   resolveFunc

	// countersMu guards lastCounters, the per-rule byte total observed on the
	// previous counterSamples call. The sampler computes deltas against it.
	countersMu   sync.Mutex
	lastCounters map[string][2]int64

	// mode 是这个 daemon 归谁管（见 RunMode），New 写入后不再改。“归不归面板管”
	// 的每一处判断都读它：Bootstrap 的降级、Run 里是否起拨号器、本机控制口上哪些
	// 操作要交给面板裁决（panelSession）。
	mode                RunMode
	portRange           string
	declaredRelayHost   string
	declaredRelayHostV6 string
	// dialer 只在托管形态下由 Run 创建一次。用原子指针是因为本机控制口的处理协程
	// 在 Run 写入它之前就已经开始服务；读到 nil 表示会话还没建立，按离线对待。
	dialer atomic.Pointer[Dialer]

	// reconcileMu 让对数据面的两种操作互斥：dp.Reconcile（applySerialized）与关停时的
	// dp.Close（closeSerialized）。后者不经过 reconcileOwners，所以 writeMu 管不到它：
	// 收到退出信号时可能正有一次写入停在 dp.Reconcile 里。取 reconcileMu 时不持有 mu。
	reconcileMu sync.Mutex

	// writeMu 让 reconcileOwners 的一次调用——取当前归属段、改、解析、下数据面、
	// 提交——整体串行。每次调用都是在当前内容的一份拷贝上改完再整体写回；两次调用
	// 交叠时后提交的会把先提交的改动原样冲掉（它的拷贝是在对方提交之前取的）。
	// 调用方互相是并发的：本机控制口的各个 HTTP 处理、拨号器主循环里的面板下发、
	// 拨号器的接管协程、DNS 刷新循环。
	//
	// 它与 mu 分开，是为了让只读路径（列规则、查状态、握手时取快照）不必等一次
	// 可能长达数秒的解析。加锁顺序：writeMu → mu；writeMu → reconcileMu。持有 mu
	// 或 reconcileMu 时不得再取 writeMu。
	writeMu sync.Mutex

	mu           sync.Mutex
	owners       OwnerRuleset
	meta         AgentMeta
	lastResolved []nft.Rule
}

// applySerialized 在 reconcileMu 下调用 dp.Reconcile，与关停时的 closeSerialized 互斥。
// 它不取 mu。
func (d *Daemon) applySerialized(ctx context.Context, resolved []nft.Rule) error {
	d.reconcileMu.Lock()
	defer d.reconcileMu.Unlock()
	return d.dp.Reconcile(ctx, resolved)
}

// reconcileOwners is the shared merge->resolve->apply pipeline used by
// every path that writes the owner-segmented ruleset.
//
// 整个调用在 writeMu 下串行（理由见该字段），所以 mutate 看到的总是上一次写入提交后
// 的内容。调用方不得持有 d.mu：这里取它拿当前内容的拷贝，在慢操作（解析、下数据面）
// 期间放开，提交时再取。mutate receives a deep clone of d.owners; it may modify
// the map freely. A nil mutate means re-resolve with the current owners
// unchanged (used by the DNS refresh loop).
//
// DNS resolution is best-effort: a host whose lookup fails keeps the address
// it currently has in the data plane; a rule whose host fails to resolve and
// has no live address is held back rather than failing the whole call, so one
// bad target can't red the rest of the node. *resolved* is the rules that were
// actually applied;
// *unresolved* is the rules skipped for lack of an IP -- still persisted (via
// candidate) so the refresh loop retries them, but never handed to the data
// plane. *committed* reports whether d.owners/d.lastResolved were actually
// updated (false when the applyable set is identical to the previous one --
// the DNS-refresh no-op case).
//
// metaFn, if non-nil, is called under d.mu right before commit to let the
// caller adjust AgentMeta (e.g. record a panel rev).
//
// saveToDisk controls whether SaveState is called as part of the commit.
// The DNS refresh path skips persistence because the resolver output is
// ephemeral (re-derived on restart).
func (d *Daemon) reconcileOwners(
	ctx context.Context,
	mutate func(OwnerRuleset),
	metaFn func(*AgentMeta),
	saveToDisk bool,
) (resolved []nft.Rule, unresolved []nft.Rule, committed bool, err error) {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	d.mu.Lock()
	candidate := cloneOwners(d.owners)
	prev := append([]nft.Rule(nil), d.lastResolved...)
	d.mu.Unlock()

	if mutate != nil {
		mutate(candidate)
	}

	merged, err := MergedRuleset(candidate)
	if err != nil {
		return nil, nil, false, err
	}

	// 归属段里存的是未解析的规则（域名规则的 DestIP 为空），每一轮都从头解析。
	// 解析失败的那一项要沿用该域名此刻在数据面里生效的地址，否则一次 DNS 抖动就
	// 会把正在转发的规则摘掉。所以先把 prev（上一次真正下到数据面的规则集）里同一
	// 域名的地址填进待解析的规则：nft.ResolveHosts 对解析失败的项保持输入的 DestIP
	// 不动，对解析成功的项写入新地址。
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	resolved, _, rerr := d.resolveFn(rctx, carryResolvedTargets(merged, prev))
	cancel()
	// 解析是尽力而为的：到这里仍没有地址的，是这一轮解析失败、数据面里也没有该
	// 域名现成地址的规则。把它们扣下而不是让整次下发失败，一个坏目标不拖累同节点
	// 的其他规则。
	applyable, unresolved := partitionResolved(resolved)
	if len(unresolved) > 0 {
		log.Printf("reconcile: holding back %d rule(s) with unresolved target (retry in refresh loop): %v", len(unresolved), rerr)
	}

	// 数据面只在它读的字段变了的时候重放（nft.SameDataplane 比的是 prev 与 applyable
	// 的数据面投影）：重放是整表重建（nft.Apply 先删表再建），内核里每条规则的计数
	// 随之归零，不该为数据面不读的字段付这个代价。不重放的情形例如：DNS 刷新后地址
	// 没变；写入只动了一条目标还没解析出来的规则（它不在 applyable 里）；写入只改了
	// 名称、备注、跳数这类数据面不读的字段。
	//
	// 不重放不等于不提交：saveToDisk 为假的调用方（DNS 刷新，没有要落盘的东西）到此
	// 为止；其余调用方照常走下面的提交，新的字段值与还没解析的规则都记进 d.owners
	// 和状态文件。
	if nft.SameDataplane(prev, applyable) {
		if !saveToDisk {
			return applyable, unresolved, false, nil
		}
	} else if err := d.applySerialized(ctx, applyable); err != nil {
		return nil, unresolved, false, fmt.Errorf("apply: %w", err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	meta := d.meta
	if metaFn != nil {
		metaFn(&meta)
	}
	if saveToDisk {
		if err := SaveState(d.statePath, candidate, meta); err != nil {
			return nil, unresolved, false, fmt.Errorf("save state: %w", err)
		}
	}
	d.owners = candidate
	d.meta = meta
	d.lastResolved = append([]nft.Rule(nil), applyable...)
	return applyable, unresolved, true, nil
}

// closeSerialized runs dp.Close under the same reconcileMu as applySerialized
// so a shutdown-time close (firewall-shim DELETEs, relay teardown) can't
// overlap an in-flight refresh-loop reconcile (shim INSERTs) and leave the
// shim chain half-cleaned. The refresh loop exits on ctx cancel, but a tick
// already inside applySerialized when the signal lands would otherwise race
// here.
func (d *Daemon) closeSerialized(ctx context.Context) error {
	d.reconcileMu.Lock()
	defer d.reconcileMu.Unlock()
	return d.dp.Close(ctx)
}

// Handler returns the HTTP mux serving all daemon endpoints.
//
// 这些都是本机控制口（TUI、安装脚本）。其中没有“整段替换 panel 段”的入口：panel
// 段的内容只由面板经 agent 连接下发（Dialer 的 OnApply → SetPanelRuleset），本机
// 能对它做的只有逐条的受限编辑（转给面板裁决）和整段清空。
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", d.handleHealth)
	mux.HandleFunc("/v1/counters", d.handleCounters)
	mux.HandleFunc("/v1/status", d.handleStatus)
	mux.HandleFunc("/v1/rules", d.handleRules)
	mux.HandleFunc("/v1/rules/", d.handleRulesWithID)
	return mux
}

func (d *Daemon) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleClearPanelSegment 清空 panel 段（DELETE /v1/rules/panel）：面板下发过的规则
// 全部撤出数据面并从状态文件里去掉。给安装脚本在带 --purge 卸载时用：卸载面板角色
// 时面板已停，卸载 agent 角色时 daemon 即将脱离面板，两种情况下都不会再有人来撤
// 这些规则。
//
// 已应用的 rev 一并作废（传空 rev）：daemon 若之后又连上面板，握手里报的 rev 对
// 不上，面板会把规则集重新下发。所以这个操作清不掉一个仍在管着本 daemon 的面板的
// 规则，只对“面板已停或即将脱离”的 daemon 有持久效果。
func (d *Daemon) handleClearPanelSegment(w http.ResponseWriter, r *http.Request) {
	if _, err := d.SetPanelRuleset(r.Context(), "", nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// 本机控制口上“这一步要面板裁决，但现在办不到”的两个原因。
var (
	// 单机形态：没有面板，也就没有面板规则。
	errNoPanel = errors.New("本机是单机形态，没有面板规则")
	// 托管形态，但此刻没有与面板的会话。
	errPanelUnreachable = errors.New("面板不可达：本机由面板托管，这一步要面板裁决，连接恢复后再试")
)

// panelSession 返回与面板的在线会话，供需要面板裁决的本机操作把命令转过去。
//
// 本机控制口上所有“归不归面板管、面板此刻在不在”的判断都经过这里，分两步：
// 先看形态（d.mode，启动时定下、不变），单机形态回 errNoPanel；再看会话，托管
// 形态下没有在线会话回 errPanelUnreachable。两个原因不能合成一个：前者是“没有这
// 回事”，后者是“等会儿再试”。
//
// 返回的会话只在调用这一刻在线，随后可能断开；那时 Dialer.sendCommand 会报错，
// 由调用方按网关错误回给本机客户端。
func (d *Daemon) panelSession() (*Dialer, error) {
	if !d.mode.Managed() {
		return nil, errNoPanel
	}
	dl := d.dialer.Load()
	if dl == nil || !dl.IsConnected() {
		return nil, errPanelUnreachable
	}
	return dl, nil
}

// writePanelSessionError 把 panelSession 的错误回给本机客户端：没有面板是 404，
// 面板不可达是 503。
func writePanelSessionError(w http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	if errors.Is(err, errNoPanel) {
		status = http.StatusNotFound
	}
	http.Error(w, err.Error(), status)
}

// statusResp 是 GET /v1/status 的应答，把形态与会话状态分开报。
//
// Mode 是 RunMode.Name()，进程存续期间不变。Connected 只在托管形态下可能为真，
// 表示与面板的会话此刻在线。NodeName / NodeID 是最近一次握手时面板给的节点身份：
// 断线后仍然报出，界面据此说明是哪个节点离线了；托管形态下从未握手成功过时为空。
type statusResp struct {
	Mode      string `json:"mode"`
	Connected bool   `json:"connected"`
	NodeName  string `json:"node_name,omitempty"`
	NodeID    int64  `json:"node_id,omitempty"`
}

func (d *Daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := statusResp{Mode: d.mode.Name()}
	if dl := d.dialer.Load(); d.mode.Managed() && dl != nil {
		resp.Connected = dl.IsConnected()
		resp.NodeName = dl.NodeName()
		resp.NodeID = dl.NodeID()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleRules dispatches GET /v1/rules (list) and POST /v1/rules (create).
func (d *Daemon) handleRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		d.handleListRules(w, r)
	case http.MethodPost:
		d.handleCreateRule(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// 规则地址里的两个命名空间，对应 /v1/rules/{命名空间}/{id}，与两个归属段一一对应：
// local 是本地段，panel 是面板段。
//
// 本地规则的 id 由 nft.NewRuleID 生成，是 8 位十六进制串；面板规则的 id 是面板
// 数据库里的正整数。十六进制串可能恰好全是数字，两种 id 单看字符串分不开，所以
// 地址必须带命名空间。地址由 daemon 在列规则时按规则所在的段给出（listRows），
// 调用方原样带回，谁也不从 id 的长相或规则的字段推断归属。
const (
	ruleNSLocal = "local"
	ruleNSPanel = "panel"
)

// ruleRef 是一条规则在本机控制口上的地址，即 /v1/rules/{NS}/{ID} 的两段。
type ruleRef struct {
	NS string `json:"ns"`
	ID string `json:"id"`
}

// listedRule 是 GET /v1/rules 的一行：规则本身（字段平铺），加上它的地址。
type listedRule struct {
	nft.Rule
	Ref ruleRef `json:"ref"`
}

// listRows 把归属段排成列表：面板段在前，本地段在后，每一行的地址取自它所在的段。
// 段到命名空间的对应只写在这里。
func listRows(owners OwnerRuleset) []listedRule {
	rows := make([]listedRule, 0, len(owners[segPanel])+len(owners[segLocal]))
	for _, r := range owners[segPanel] {
		rows = append(rows, listedRule{Rule: r, Ref: ruleRef{NS: ruleNSPanel, ID: strconv.FormatInt(r.RuleID, 10)}})
	}
	for _, r := range owners[segLocal] {
		rows = append(rows, listedRule{Rule: r, Ref: ruleRef{NS: ruleNSLocal, ID: r.ID}})
	}
	return rows
}

// handleRulesWithID 分发 PUT / DELETE /v1/rules/{命名空间}/{id}。
func (d *Daemon) handleRulesWithID(w http.ResponseWriter, r *http.Request) {
	ns, id, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v1/rules/"), "/")
	// 不带 id 的地址指整个命名空间，只支持一个操作：清空 panel 段。
	if id == "" && ns == ruleNSPanel && r.Method == http.MethodDelete {
		d.handleClearPanelSegment(w, r)
		return
	}
	if id == "" {
		http.Error(w, "rule address must be /v1/rules/{local|panel}/{id}", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPut && r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch ns {
	case ruleNSLocal:
		if r.Method == http.MethodPut {
			d.handleUpdateLocalRule(w, r, id)
		} else {
			d.handleDeleteLocalRule(w, r, id)
		}
	case ruleNSPanel:
		ruleID, err := strconv.ParseInt(id, 10, 64)
		if err != nil || ruleID <= 0 {
			http.Error(w, "invalid panel rule id: "+id, http.StatusBadRequest)
			return
		}
		if r.Method == http.MethodPut {
			d.handleUpdatePanelRule(w, r, ruleID)
		} else {
			d.handleDeletePanelRule(w, r, ruleID)
		}
	default:
		http.Error(w, "unknown rule namespace: "+ns, http.StatusNotFound)
	}
}

// handleListRules 返回数据面里的全部规则：面板下发的在前，本地的在后，每一行带着
// 自己的地址（见 listRows）。列表不按形态、也不按会话是否在线取舍。
//
// 两段同时非空是会发生的：接入面板时本地规则交给面板接管（Dialer.adoptLocalRules），
// 被面板拒绝的那一批留在本地段继续转发；面板不可达时面板段也照常在转发。只列
// 其中一段，另一段的规则就在转发却没有任何界面能看到、改掉或删除。
func (d *Daemon) handleListRules(w http.ResponseWriter, _ *http.Request) {
	d.mu.Lock()
	rows := listRows(d.owners)
	d.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"rules": rows})
}

type createRuleReq struct {
	Proto      string `json:"proto"`
	ExitHost   string `json:"exit_host"`
	ExitPort   int    `json:"exit_port"`
	ListenPort int    `json:"listen_port"`
	Mode       string `json:"mode"`
	Comment    string `json:"comment"`
	Name       string `json:"name"`
}

type createRuleResp struct {
	Entry      string `json:"entry"`
	ListenPort int    `json:"listen_port"`
}

// handleCreateRule 新建一条规则，往哪建由形态决定：
//
//   - 托管形态：规则由面板建。请求经会话转给面板，面板建好后随下发进入面板段。
//     面板不可达时拒绝（503），不退回去建本地规则——那样规则会在面板不知情、
//     不计入任何上限与账本的情况下开始转发，重连后才作为一条新规则请面板接管，
//     而面板可以拒绝接管。
//   - 单机形态：建一条本地规则，ListenPort 为 0 时自动挑一个空闲端口。
//
// 所以托管形态下本地段不会多出规则：里面只有接入面板之前留下、还没被面板接管的
// 那些（clearTuiSegment 依赖这一点）。
func (d *Daemon) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	var req createRuleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}

	if d.mode.Managed() {
		dl, err := d.panelSession()
		if err != nil {
			writePanelSessionError(w, err)
			return
		}
		ack, err := dl.CreateRule(r.Context(), createToWSProto(req))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if !ack.OK {
			http.Error(w, ack.Error, http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, createRuleResp{Entry: ack.Entry})
		return
	}

	listenPort := req.ListenPort
	if listenPort == 0 {
		proto := req.Proto
		if proto == "" {
			proto = "tcp"
		}
		listenPort = d.pickLocalFreePort(proto)
		if listenPort == 0 {
			http.Error(w, "no free port available", http.StatusServiceUnavailable)
			return
		}
	}

	rule := nft.Rule{
		ID:       nft.NewRuleID(),
		Proto:    req.Proto,
		SrcPort:  listenPort,
		DestPort: req.ExitPort,
		Comment:  req.Comment,
		Mode:     req.Mode,
	}
	setLocalRuleTarget(&rule, req.ExitHost)
	if err := nft.Validate(rule); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	_, _, _, err := d.reconcileOwners(r.Context(),
		func(candidate OwnerRuleset) {
			candidate[segLocal] = append(candidate[segLocal], rule)
		}, nil, true)
	if err != nil {
		http.Error(w, err.Error(), d.classifyWriteError(err).(*ownerWriteError).status)
		return
	}

	writeJSON(w, http.StatusOK, createRuleResp{ListenPort: listenPort})
}

// panelHopCount returns the HopCount metadata of the panel-segment rule with
// the given server RuleID, or 0 when the rule is not in local state. Chain
// rules (HopCount > 1) must be edited through the per-hop channel: the server
// rejects whole-rule updates on multi-hop rules.
func (d *Daemon) panelHopCount(ruleID int64) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, r := range d.owners[segPanel] {
		if r.RuleID == ruleID {
			return r.HopCount
		}
	}
	return 0
}

// updateRuleReq 是 PUT /v1/rules/{命名空间}/{id} 的请求体，两个命名空间共用。
type updateRuleReq struct {
	Proto      string `json:"proto"`
	ExitHost   string `json:"exit_host"`
	ExitPort   int    `json:"exit_port"`
	ListenPort int    `json:"listen_port"`
	Mode       string `json:"mode"`
	Comment    string `json:"comment"`
}

func decodeUpdateRuleReq(w http.ResponseWriter, r *http.Request) (updateRuleReq, bool) {
	var req updateRuleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return req, false
	}
	return req, true
}

// handleUpdatePanelRule 把对面板规则的修改经 WS 转给面板；规则的真值在面板数据库
// 里，daemon 不在本地改 panel 段，等面板重新下发。
func (d *Daemon) handleUpdatePanelRule(w http.ResponseWriter, r *http.Request, ruleID int64) {
	req, ok := decodeUpdateRuleReq(w, r)
	if !ok {
		return
	}
	dl, err := d.panelSession()
	if err != nil {
		writePanelSessionError(w, err)
		return
	}
	var ack wsproto.RuleCmdAck
	// Multi-hop chain rules only expose port/mode/comment to the node side;
	// the server owns the rest of the skeleton and rejects rule_update on
	// them, so those edits go through the per-hop channel instead.
	if d.panelHopCount(ruleID) > 1 {
		ack, err = dl.EditRuleHop(r.Context(), wsproto.RuleHopEdit{
			RuleID: ruleID, ListenPort: req.ListenPort, Mode: req.Mode, Comment: req.Comment,
		})
	} else {
		ack, err = dl.UpdateRule(r.Context(), updateToWSProto(ruleID, req))
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if !ack.OK {
		http.Error(w, ack.Error, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"entry": ack.Entry})
}

// setLocalRuleTarget 把请求里的出口地址放进本地规则：是 IP 字面量就进 DestIP，否则
// 当作域名进 DestHost，另一个字段清空。它不判断地址合不合法，那是 nft.Validate 的事。
func setLocalRuleTarget(r *nft.Rule, exitHost string) {
	if net.ParseIP(exitHost) != nil {
		r.DestIP, r.DestHost = exitHost, ""
	} else {
		r.DestIP, r.DestHost = "", exitHost
	}
}

// handleUpdateLocalRule 修改本地段里 ID 等于 id 的那条规则。
//
// 请求里没给的字段沿用原值，所以合不合法要看改完之后的整条规则（nft.Validate），
// 不能只看请求：把一条 UDP 规则的模式改成用户态，请求里的每个字段单看都合法。不
// 合法时本地段保持原样。
func (d *Daemon) handleUpdateLocalRule(w http.ResponseWriter, r *http.Request, id string) {
	req, ok := decodeUpdateRuleReq(w, r)
	if !ok {
		return
	}

	found := false
	var invalid error
	_, _, _, err := d.reconcileOwners(r.Context(),
		func(candidate OwnerRuleset) {
			rules := candidate[segLocal]
			for i := range rules {
				if rules[i].ID != id {
					continue
				}
				found = true
				edited := rules[i]
				if req.Proto != "" {
					edited.Proto = req.Proto
				}
				if req.ExitHost != "" {
					setLocalRuleTarget(&edited, req.ExitHost)
				}
				if req.ExitPort != 0 {
					edited.DestPort = req.ExitPort
				}
				if req.ListenPort != 0 {
					edited.SrcPort = req.ListenPort
				}
				if req.Mode != "" {
					edited.Mode = req.Mode
				}
				edited.Comment = req.Comment
				if invalid = nft.Validate(edited); invalid == nil {
					rules[i] = edited
				}
				return
			}
		}, nil, true)
	if err != nil {
		http.Error(w, err.Error(), d.classifyWriteError(err).(*ownerWriteError).status)
		return
	}
	if !found {
		http.Error(w, "rule not found", http.StatusNotFound)
		return
	}
	if invalid != nil {
		http.Error(w, invalid.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleDeletePanelRule 把删除面板规则的请求经 WS 转给面板。
func (d *Daemon) handleDeletePanelRule(w http.ResponseWriter, r *http.Request, ruleID int64) {
	dl, err := d.panelSession()
	if err != nil {
		writePanelSessionError(w, err)
		return
	}
	ack, err := dl.DeleteRule(r.Context(), ruleID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if !ack.OK {
		http.Error(w, ack.Error, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleDeleteLocalRule 从本地段删除 ID 等于 id 的那条规则。
func (d *Daemon) handleDeleteLocalRule(w http.ResponseWriter, r *http.Request, id string) {
	found := false
	_, _, _, err := d.reconcileOwners(r.Context(),
		func(candidate OwnerRuleset) {
			rules := candidate[segLocal]
			for i := range rules {
				if rules[i].ID == id {
					candidate[segLocal] = append(rules[:i], rules[i+1:]...)
					found = true
					break
				}
			}
			if len(candidate[segLocal]) == 0 {
				delete(candidate, segLocal)
			}
		}, nil, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "rule not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// pickLocalFreePort finds an unoccupied port in the chain-port range
// across all owner segments. A port is considered occupied if any rule
// with a matching or overlapping protocol already uses it.
func (d *Daemon) pickLocalFreePort(proto string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	occupied := make(map[int]bool)
	for _, rules := range d.owners {
		for _, r := range rules {
			if r.Proto == proto || r.Proto == "tcp+udp" || proto == "tcp+udp" {
				occupied[r.SrcPort] = true
			}
		}
	}
	return portutil.PickFreePort(portutil.ChainPortMin, portutil.ChainPortMax, occupied)
}

// ownerWriteError is the typed error returned by reconcileOwners so the
// HTTP handler can map merge conflicts to 409 without reparsing the error
// message. Syntactically invalid targets are rejected up front by the
// handler before reconcileOwners ever runs, so they never reach here.
type ownerWriteError struct {
	status int
	err    error
}

func (e *ownerWriteError) Error() string { return e.err.Error() }
func (e *ownerWriteError) Unwrap() error { return e.err }

// classifyWriteError wraps a reconcileOwners error into an ownerWriteError
// with the appropriate HTTP status so the handler can surface a precise
// status code to the client.
func (d *Daemon) classifyWriteError(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "already claimed"):
		return &ownerWriteError{status: http.StatusConflict, err: err}
	default:
		return &ownerWriteError{status: http.StatusInternalServerError, err: err}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// cloneOwners returns a deep-enough copy that the caller can mutate the
// map (delete/replace keys) without affecting the original. Rule slices
// themselves are shallow-copied -- Rule is a value type so this is safe.
func cloneOwners(src OwnerRuleset) OwnerRuleset {
	if src == nil {
		return OwnerRuleset{}
	}
	out := make(OwnerRuleset, len(src))
	for k, v := range src {
		out[k] = append([]nft.Rule(nil), v...)
	}
	return out
}

// createToWSProto converts a local create request to the WS protocol struct.
func createToWSProto(req createRuleReq) wsproto.RuleCreate {
	return wsproto.RuleCreate{
		Proto:      req.Proto,
		ExitHost:   req.ExitHost,
		ExitPort:   req.ExitPort,
		ListenPort: req.ListenPort,
		Mode:       req.Mode,
		Comment:    req.Comment,
		Name:       req.Name,
	}
}

// updateToWSProto converts a local update request to the WS protocol struct.
func updateToWSProto(ruleID int64, req updateRuleReq) wsproto.RuleUpdate {
	return wsproto.RuleUpdate{
		RuleID:     ruleID,
		Proto:      req.Proto,
		ExitHost:   req.ExitHost,
		ExitPort:   req.ExitPort,
		ListenPort: req.ListenPort,
		Mode:       req.Mode,
		Comment:    req.Comment,
	}
}
