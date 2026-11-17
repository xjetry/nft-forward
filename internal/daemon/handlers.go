package daemon

import (
	"context"
	"encoding/json"
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
	"nft-forward/internal/resolver"
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

	// 这个 daemon 归哪个面板管，由下面三项决定（New 保证 connectURL 与 serverLocal
	// 不同时出现）：
	//   - connectURL 非空：远程面板，拨它的 WebSocket 地址，握手带 connectTok；
	//   - serverLocal：本机面板，拨它的本机入口 socket panelSocket，握手不带 token；
	//   - 都没有：单机，不拨任何面板，没有拨号器。
	// managedByPanel 是“前两种之一”的判定，Bootstrap 的降级与 Run 里是否起拨号器
	// 都只问它。
	//
	// dialer is atomic so unix-socket handlers running on their own
	// goroutines can safely read it (e.g. to push tui_segment_changed)
	// without coordinating with Run's lifecycle code.
	connectURL          string
	connectTok          string
	serverLocal         bool
	panelSocket         string
	portRange           string
	declaredRelayHost   string
	declaredRelayHostV6 string
	dialer              atomic.Pointer[Dialer]

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

	// The kernel apply is skipped whenever the applyable set is unchanged
	// from what's already live -- this covers both the DNS-refresh no-op
	// case and a write (create/update/delete) whose only effect was to
	// add/hold a rule that's still unresolved. DNS-refresh callers additionally
	// skip the commit below (nothing to persist); write callers still commit
	// so the new/edited rule (even an unresolved one) lands in d.owners and
	// on disk for the refresh loop to retry.
	if !rulesDiffer(prev, applyable) {
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

// managedByPanel 报告这个 daemon 是否归某个面板管（远程面板或本机面板）。
func (d *Daemon) managedByPanel() bool {
	return d.connectURL != "" || d.serverLocal
}

// localPanelSocket 返回要拨的本机入口 socket；不是 serverLocal 模式时为空，拨号器
// 据此走 connectURL。
func (d *Daemon) localPanelSocket() string {
	if d.serverLocal {
		return d.panelSocket
	}
	return ""
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

// statusResp is returned by GET /v1/status.
type statusResp struct {
	Connected bool   `json:"connected"`
	NodeName  string `json:"node_name,omitempty"`
	NodeID    int64  `json:"node_id,omitempty"`
}

func (d *Daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := statusResp{}
	if dl := d.Dialer(); dl != nil && dl.IsConnected() {
		resp.Connected = true
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

// 规则地址里的两个命名空间，对应 /v1/rules/{命名空间}/{id}。
//
// 本地规则（tui 段）的 id 由 nft.NewRuleID 生成，是 8 位十六进制串；面板规则的 id
// 是面板数据库里的正整数。十六进制串可能恰好全是数字，两种 id 单看字符串分不开，
// 所以由调用方在路径里写明命名空间：TUI 的每一行都知道自己是哪一种（面板规则的
// nft.Rule.RuleID 非零）。这里不从 id 的长相推断归属。
const (
	ruleNSLocal = "local"
	ruleNSPanel = "panel"
)

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

// handleListRules returns the active rule set: "panel" segment when connected
// to the server, "tui" segment when running standalone.
// handleListRules 返回数据面里的全部规则：面板下发的（panel 段）在前，本地的（tui
// 段）在后。每一行自带归属——面板规则的 RuleID 非零——调用方据此决定改/删走哪个
// 地址（见 ruleNSLocal / ruleNSPanel），列表本身不按连接状态取舍。
//
// 两段同时非空是会发生的：接入面板时本地规则交给面板接管（Dialer.adoptLocalRules），
// 被面板拒绝的那一批留在 tui 段继续转发；面板不可达时 panel 段也照常在转发。只列
// 其中一段，另一段的规则就在转发却没有任何界面能看到、改掉或删除。
func (d *Daemon) handleListRules(w http.ResponseWriter, _ *http.Request) {
	d.mu.Lock()
	rules := make([]nft.Rule, 0, len(d.owners["panel"])+len(d.owners["tui"]))
	rules = append(rules, d.owners["panel"]...)
	rules = append(rules, d.owners["tui"]...)
	d.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
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

// handleCreateRule creates a rule either through the server (when connected)
// or locally in the "tui" segment (when standalone). Local rules get an
// auto-assigned port when ListenPort is 0.
func (d *Daemon) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	var req createRuleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Connected: relay to server via WS.
	if dl := d.Dialer(); dl != nil && dl.IsConnected() {
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

	// Standalone: manage locally in "tui" segment.
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
	if net.ParseIP(req.ExitHost) == nil {
		if !resolver.PlausibleHostname(req.ExitHost) {
			http.Error(w, "出口地址非法："+req.ExitHost+" 不是合法 IP 或域名", http.StatusBadRequest)
			return
		}
		rule.DestHost = req.ExitHost
	} else {
		rule.DestIP = req.ExitHost
	}

	_, _, _, err := d.reconcileOwners(r.Context(),
		func(candidate OwnerRuleset) {
			candidate["tui"] = append(candidate["tui"], rule)
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
	for _, r := range d.owners["panel"] {
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
	dl := d.Dialer()
	if dl == nil || !dl.IsConnected() {
		http.Error(w, "daemon not connected to server", http.StatusServiceUnavailable)
		return
	}
	var ack wsproto.RuleCmdAck
	var err error
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

// handleUpdateLocalRule 修改 tui 段里 ID 等于 id 的那条规则。
func (d *Daemon) handleUpdateLocalRule(w http.ResponseWriter, r *http.Request, id string) {
	req, ok := decodeUpdateRuleReq(w, r)
	if !ok {
		return
	}
	if req.ExitHost != "" && net.ParseIP(req.ExitHost) == nil && !resolver.PlausibleHostname(req.ExitHost) {
		http.Error(w, "出口地址非法："+req.ExitHost+" 不是合法 IP 或域名", http.StatusBadRequest)
		return
	}

	found := false
	_, _, _, err := d.reconcileOwners(r.Context(),
		func(candidate OwnerRuleset) {
			rules := candidate["tui"]
			for i := range rules {
				if rules[i].ID == id {
					if req.Proto != "" {
						rules[i].Proto = req.Proto
					}
					if req.ExitHost != "" {
						if ip := net.ParseIP(req.ExitHost); ip != nil {
							rules[i].DestIP = req.ExitHost
							rules[i].DestHost = ""
						} else {
							rules[i].DestHost = req.ExitHost
							rules[i].DestIP = ""
						}
					}
					if req.ExitPort != 0 {
						rules[i].DestPort = req.ExitPort
					}
					if req.ListenPort != 0 {
						rules[i].SrcPort = req.ListenPort
					}
					if req.Mode != "" {
						rules[i].Mode = req.Mode
					}
					rules[i].Comment = req.Comment
					found = true
					break
				}
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
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleDeletePanelRule 把删除面板规则的请求经 WS 转给面板。
func (d *Daemon) handleDeletePanelRule(w http.ResponseWriter, r *http.Request, ruleID int64) {
	dl := d.Dialer()
	if dl == nil || !dl.IsConnected() {
		http.Error(w, "daemon not connected to server", http.StatusServiceUnavailable)
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

// handleDeleteLocalRule 从 tui 段删除 ID 等于 id 的那条规则。
func (d *Daemon) handleDeleteLocalRule(w http.ResponseWriter, r *http.Request, id string) {
	found := false
	_, _, _, err := d.reconcileOwners(r.Context(),
		func(candidate OwnerRuleset) {
			rules := candidate["tui"]
			for i := range rules {
				if rules[i].ID == id {
					candidate["tui"] = append(rules[:i], rules[i+1:]...)
					found = true
					break
				}
			}
			if len(candidate["tui"]) == 0 {
				delete(candidate, "tui")
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
