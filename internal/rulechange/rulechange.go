// Package rulechange 是规则写入的唯一入口。
//
// 改规则的发起方有三种：管理员、普通用户、节点（节点侧 TUI 经 agent 通道发来的命令，
// 以节点属主的名义执行）。他们经不同的入口进来（/api、/api/my、/api/v1、agent 通道），
// 入口只做解码；“谁、对哪条规则、能做什么”在这里判定一次：
//
//   - 发起方的账号能不能改规则（Actor.mayChange）；
//   - 他能不能动这条规则（mayEdit / mayRemove）；
//   - 他能不能把某个节点用进规则（db.NodeAccess）；
//   - 规则表头是否成立（header）；
//   - 规则走哪条链（Path；推导在 internal/chain）；
//   - 条数上限（withinCaps，与写入在同一个事务里）；
//   - 表头与链在一个事务里落库（面板的非测试代码里，db.CreateRule、
//     db.UpdateRuleHeader、db.RegenerateRule、db.DeleteRule 只有本包调用）；
//   - 审计。
//
// 每个操作返回要重新下发的节点（Result.Affected）。本包不下发：HTTP 入口要等节点应
// 答，agent 通道的读循环不能被下发阻塞，等不等由入口决定。
//
// 资源层面的状态不在这里拦：流量配额用尽、经过的节点被停用时照样可以建规则、改规
// 则，规则是否转发由 db 包的压制判定（suppressedRulesSQL）回答并在界面上标出原因。
// 这里拦的是发起方本身不成立的情形（账号停用、到期、无权、超出条数上限）。
package rulechange

import (
	"database/sql"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"nft-forward/internal/chain"
	"nft-forward/internal/db"
	"nft-forward/internal/resolver"
)

// Kind 是拒绝的类别，入口据此选状态码。
type Kind int

const (
	// Invalid：请求本身不成立（字段非法、链推导不出来、端口不可用）。
	Invalid Kind = iota + 1
	// Forbidden：发起方无权做这件事。
	Forbidden
	// NotFound：规则不存在。
	NotFound
	// Conflict：请求成立，但与现状冲突（超出条数上限、同一物理节点在链上出现两次）。
	Conflict
	// Internal：读写库失败。
	Internal
)

// Error 是一次被拒绝的变更。Msg 是给发起方看的原因；Unwrap 给出底层错误，供入口识别
// db.ErrDuplicateChainNode 这类需要单独编码的情形。
type Error struct {
	Kind  Kind
	Msg   string
	cause error
}

func (e *Error) Error() string { return e.Msg }
func (e *Error) Unwrap() error { return e.cause }

func invalid(format string, a ...any) *Error {
	return &Error{Kind: Invalid, Msg: fmt.Sprintf(format, a...)}
}
func forbidden(msg string) *Error { return &Error{Kind: Forbidden, Msg: msg} }
func conflict(format string, a ...any) *Error {
	return &Error{Kind: Conflict, Msg: fmt.Sprintf(format, a...)}
}
func internal(err error) *Error { return &Error{Kind: Internal, Msg: err.Error(), cause: err} }

// Actor 是发起变更的一方。
//
// User 是变更记在谁名下：建出来的规则归他，条数上限数他的，审计记他。Node 非 0 表示
// 命令来自这台节点，此时 User 是节点的属主（NodeActor 填），节点只能动自己身上的
// 规则。节点没有属主时 User 为 nil，见 NodeActor。
type Actor struct {
	User *db.User
	Node int64
}

func (a Actor) admin() bool { return a.User != nil && a.User.Role == "admin" }

// onPanel 报告发起方是不是在面板上操作的管理员。只有他能动别人的规则、直接指定物理
// 跳、重新分配端口。节点以管理员属主的名义发来的命令不算：它的权限范围是那台节点。
func (a Actor) onPanel() bool { return a.Node == 0 && a.admin() }

// mayChange 判断发起方的账号此刻能不能建规则、改规则。
//
// 被停用或已到期的账号名下的规则一条都不转发（db 的压制判定里的账号层条件），让这样
// 的账号继续建规则、改规则没有意义。删除不经过这里：撤掉转发任何时候都允许。
//
// 在面板上操作的管理员不经这两项检查：他改的可以是任何人的规则，这是管理动作，不取
// 决于管理员自己账号的到期时间；被停用的账号过不了会话鉴权（requireAPIAuth），走不到
// 这里。同一个管理员账号作为节点属主时照常检查——那是在用这个账号的名义建转发。
func (a Actor) mayChange() *Error {
	if a.User == nil {
		return forbidden("节点无归属用户")
	}
	if a.onPanel() {
		return nil
	}
	if a.User.Disabled {
		return forbidden("用户已被禁用")
	}
	if a.User.Expired(time.Now()) {
		return forbidden("用户已过期")
	}
	return nil
}

// Changes 执行规则变更。它只持有库句柄，没有自己的状态。
type Changes struct {
	DB *sql.DB
}

func New(d *sql.DB) *Changes { return &Changes{DB: d} }

// NodeActor 给出“节点 nodeID 发来的命令”的发起方：以节点属主的名义。
//
// 节点没有属主时（没指定过，或属主账号已被删除——nodes.owner_id 的外键是 ON DELETE
// SET NULL）返回的 Actor 里 User 为 nil。这样的节点仍可以改自己那一跳、删经过自己的
// 规则（EditHop、Delete 的权限来自“节点在这条规则上”），但不能建规则、改规则、交出
// 本地规则：这些都要落到某个人名下，mayChange 会拒绝。
func (c *Changes) NodeActor(nodeID int64) (Actor, *Error) {
	node, err := db.GetNode(c.DB, nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return Actor{}, &Error{Kind: NotFound, Msg: "节点不存在"}
	}
	if err != nil {
		return Actor{}, internal(err)
	}
	if node.OwnerID == nil {
		return Actor{Node: nodeID}, nil
	}
	u, err := db.GetUserByID(c.DB, *node.OwnerID)
	if errors.Is(err, sql.ErrNoRows) {
		return Actor{Node: nodeID}, nil
	}
	if err != nil {
		return Actor{}, internal(err)
	}
	return Actor{User: u, Node: nodeID}, nil
}

// Fields 是规则表头里由发起方填写的部分。
type Fields struct {
	Name  string
	Proto string
	// Exit 是转发目标，host:port 形式；IPv6 字面量要带方括号。
	Exit    string
	Comment string
}

// Path 说明规则走哪条链，以及入口的监听端口。链有三种给法：
//
//   - 点名逻辑节点：Entry 是入口，Vias 是其后的中转层，物理链由 chain.Topology.Derive
//     按当前拓扑推导；
//   - Hops：直接给出物理跳序列，只有面板上的管理员可以，不校验角色与绑定边（仍检查
//     链尾能否直接转发）。表头里的入口与中转层由这些跳算出（见 explicitChain）；
//     这时 Entry、Vias 不是另一种给法，只能是同一条路径的复述，与算出的不一致就拒绝；
//   - 都不给：只在改规则时有意义，沿用规则落库的链。已存规则认的是落库的链，组合的
//     成员后来变了也不会自动跟着变；只有点名了入口或中转层的编辑才重新推导。
//
// 建规则与改规则取链的次序相同，见 Create 与 Update。
type Path struct {
	Entry int64
	// Vias 为 nil 表示请求没有提到中转层（改规则时沿用已存的）；非 nil 的空列表表示
	// 明确不要中转层。
	Vias *[]int64
	Hops []chain.Hop
	// ExitMode 指定出口那一跳的转发模式，取舍规则见 chain.ExitChoice。
	ExitMode string
	// EntryPort 大于 0 时指定入口的监听端口，否则由面板分配或沿用现有的。
	EntryPort int
}

func (p Path) vias() []int64 {
	if p.Vias == nil {
		return nil
	}
	return *p.Vias
}

// Result 是一次成功的变更。
type Result struct {
	// Rule 是落库后的规则表头；删除时是被删的那条，预演与批量接管时为 nil。
	Rule *db.Rule
	// Entry、EntryV6 是规则对外的入口地址（host:port）。
	Entry   string
	EntryV6 string
	// Hops 是规则的物理链。
	Hops []db.HopInput
	// Affected 是规则集因此变化、需要重新下发的节点。
	Affected []int64
}

// Create 以 actor 的名义建一条规则。dryRun 为真时把整条路径走完（校验、推导、上限、
// 端口分配都是真的）但不提交、不记审计：Result 里的入口地址与物理链是这条规则会得
// 到的结果，Rule 为 nil。
//
// p.Hops 非空时用它作物理链，表头的入口与中转层由它算出；否则按 p.Entry 与 p.Vias
// 推导。
func (c *Changes) Create(actor Actor, f Fields, p Path, dryRun bool) (Result, *Error) {
	results, cerr := c.create(actor, []creation{{f, p}}, "create", dryRun)
	if cerr != nil {
		return Result{}, cerr
	}
	return results[0], nil
}

// Local 是节点交给面板接管的一条本地规则。
type Local struct {
	Fields
	ListenPort int
	Mode       string
}

// Adopt 把节点的一批本地规则接管成面板规则：每条都按节点侧建规则的同一条路径建在这
// 台节点上、归节点属主。整批在一个事务里，任何一条不成立就整批不落库：节点只在收到
// 整批成功的应答后才清掉本地那一份，否则下次会话把整批原样再交一遍
// （daemon 的 Dialer.adoptLocalRules）；若允许部分落库，已落库的那几条下次会被再建
// 一遍。
func (c *Changes) Adopt(actor Actor, locals []Local) (Result, *Error) {
	if actor.Node == 0 {
		return Result{}, forbidden("只有节点可以交出本地规则")
	}
	if len(locals) == 0 {
		return Result{}, nil
	}
	items := make([]creation, len(locals))
	for i, l := range locals {
		items[i] = creation{l.Fields, Path{Entry: actor.Node, ExitMode: l.Mode, EntryPort: l.ListenPort}}
	}
	results, cerr := c.create(actor, items, "adopt", false)
	if cerr != nil {
		return Result{}, cerr
	}
	var out Result
	seen := map[int64]bool{}
	for _, r := range results {
		for _, n := range r.Affected {
			if !seen[n] {
				seen[n] = true
				out.Affected = append(out.Affected, n)
			}
		}
	}
	return out, nil
}

type creation struct {
	fields Fields
	path   Path
}

// create 是建规则的唯一实现，一次建一条或一批。读（鉴权、拓扑、推导）都发生在开事务
// 之前：db.Open 把连接数限成 1，事务持有这个连接期间再经 c.DB 读，会一直等事务放回
// 连接——自己等自己。事务里只经 tx 读写。
func (c *Changes) create(actor Actor, items []creation, op string, dryRun bool) ([]Result, *Error) {
	if cerr := actor.mayChange(); cerr != nil {
		return nil, cerr
	}
	type planned struct {
		rule *db.Rule
		hops []db.HopInput
	}
	var (
		plans  []planned
		use    usage
		topo   chain.Topology
		seats  map[int64]chain.Seat
		loaded bool
	)
	for _, it := range items {
		h, cerr := actor.header(it.fields, nil)
		if cerr != nil {
			return nil, cerr
		}
		p := it.path
		vias := p.vias()
		explicit := len(p.Hops) > 0
		if cerr := actor.pathAllowed(p, true); cerr != nil {
			return nil, cerr
		}
		if !explicit && p.Entry == 0 {
			if actor.onPanel() {
				return nil, invalid("需指定 node_id 或 hops")
			}
			return nil, invalid("node_id 不能为空")
		}
		if !explicit {
			grant, cerr := c.access(actor.User, p.Entry, vias)
			if cerr != nil {
				return nil, cerr
			}
			use.add(p.Entry, grant)
		} else {
			use.add(0, nil)
		}
		if !loaded {
			var err error
			if topo, err = db.LoadTopology(c.DB); err != nil {
				return nil, internal(err)
			}
			if seats, cerr = c.ownerSeats(actor.User); cerr != nil {
				return nil, cerr
			}
			loaded = true
		}
		rl := &db.Rule{
			NodeID:   p.Entry,
			OwnerID:  sql.NullInt64{Int64: actor.User.ID, Valid: true},
			Name:     h.name,
			Proto:    h.proto,
			ExitHost: h.host,
			ExitPort: h.port,
			Comment:  h.comment,
		}
		var hops []db.HopInput
		if explicit {
			if hops, rl.NodeID, rl.ViaNodeIDs, cerr = explicitChain(topo, p); cerr != nil {
				return nil, cerr
			}
		} else {
			derived, err := topo.Derive(chain.Request{
				Entry: p.Entry, Vias: vias,
				Exit:          chain.ExitChoice{Explicit: p.ExitMode},
				SeatOverrides: seats,
			})
			if err != nil {
				return nil, invalid("%s", err.Error())
			}
			hops = hopInputs(derived)
			rl.ViaNodeIDs = vias
		}
		if p.EntryPort > 0 {
			hops[0].DesiredPort = p.EntryPort
		}
		plans = append(plans, planned{rl, hops})
	}

	tx, err := c.DB.Begin()
	if err != nil {
		return nil, internal(err)
	}
	defer tx.Rollback()
	if cerr := withinCaps(tx, actor.User, use); cerr != nil {
		return nil, cerr
	}
	results := make([]Result, len(plans))
	for i, pl := range plans {
		id, err := db.CreateRule(tx, pl.rule)
		if err != nil {
			return nil, internal(err)
		}
		pl.rule.ID = id
		entry, entryV6, affected, err := db.RegenerateRule(tx, pl.rule, pl.hops, nil)
		if err != nil {
			return nil, regenerateRefused(err)
		}
		results[i] = Result{Rule: pl.rule, Entry: entry, EntryV6: entryV6, Hops: pl.hops, Affected: affected}
	}
	if dryRun {
		// 不提交，defer 回滚：端口分配与链都真实走过一遍，但没有规则、审计留下来。
		for i := range results {
			results[i].Rule = nil
		}
		return results, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, internal(err)
	}
	for _, r := range results {
		c.audit(actor, op, r.Rule.ID, r.Rule.Name)
	}
	return results, nil
}

// Update 以 actor 的名义改规则 ruleID 的表头与链。
//
// 链的取法依次是：p.Hops 非空，用它作物理链，表头的入口与中转层由它算出；否则
// p.Entry 大于 0 或 p.Vias 非 nil，按它们重新推导（没提到的那一项沿用规则已存的）；
// 都没有，沿用规则落库的链。出口模式的指定（p.ExitMode）对后两种取法
// 生效（显式链里每一跳的模式都是请求写明的），没有指定时保持现状——除非换了入口，
// 那时回到默认值。
func (c *Changes) Update(actor Actor, ruleID int64, f Fields, p Path) (Result, *Error) {
	if cerr := actor.mayChange(); cerr != nil {
		return Result{}, cerr
	}
	rl, stored, cerr := c.load(ruleID)
	if cerr != nil {
		return Result{}, cerr
	}
	if cerr := actor.mayEdit(rl, stored); cerr != nil {
		return Result{}, cerr
	}
	h, cerr := actor.header(f, rl)
	if cerr != nil {
		return Result{}, cerr
	}
	if cerr := actor.pathAllowed(p, false); cerr != nil {
		return Result{}, cerr
	}

	explicit := len(p.Hops) > 0
	entry, vias := rl.NodeID, rl.ViaNodeIDs
	var hops []db.HopInput
	if explicit {
		topo, err := db.LoadTopology(c.DB)
		if err != nil {
			return Result{}, internal(err)
		}
		if hops, entry, vias, cerr = explicitChain(topo, p); cerr != nil {
			return Result{}, cerr
		}
	} else {
		if p.Entry > 0 {
			entry = p.Entry
		}
		if p.Vias != nil {
			vias = *p.Vias
		}
	}
	// 发起方必须仍然能用规则的入口与中转层，哪怕这次只改表头：换目标、换协议都是在
	// 这些节点上重新安排转发。
	grant, cerr := c.access(actor.User, entry, vias)
	if cerr != nil {
		return Result{}, cerr
	}
	moved := entry != rl.NodeID

	switch {
	case explicit:
		// 链与表头的逻辑路径上面已经由 explicitChain 给出。
	case p.Entry > 0 || p.Vias != nil:
		topo, err := db.LoadTopology(c.DB)
		if err != nil {
			return Result{}, internal(err)
		}
		seats, cerr := c.ruleOwnerSeats(rl)
		if cerr != nil {
			return Result{}, cerr
		}
		exit := chain.ExitChoice{Explicit: p.ExitMode}
		if !moved && len(stored) > 0 {
			exit.Current = stored[len(stored)-1].Mode
		}
		derived, err := topo.Derive(chain.Request{Entry: entry, Vias: vias, Exit: exit, SeatOverrides: seats})
		if err != nil {
			return Result{}, invalid("%s", err.Error())
		}
		hops = hopInputs(derived)
	default:
		if len(stored) == 0 {
			return Result{}, invalid("至少添加一个节点")
		}
		kept := make([]chain.Hop, len(stored))
		for i, sh := range stored {
			kept[i] = chain.Hop{NodeID: sh.NodeID, Mode: sh.Mode, ViaNodeID: sh.ViaNodeID}
		}
		chain.ExitChoice{Explicit: p.ExitMode, Current: kept[len(kept)-1].Mode}.Apply(kept)
		hops = hopInputs(kept)
	}
	if p.EntryPort > 0 {
		hops[0].DesiredPort = p.EntryPort
	}
	// 表头里的入口与中转层不在这里改：下面的 RegenerateRule 按 hops 写它们，并把 rl
	// 上的两个字段同步过去。entry、vias 在本函数里只用来查权限与名额。
	rl.Name, rl.Proto, rl.ExitHost, rl.ExitPort, rl.Comment = h.name, h.proto, h.host, h.port, h.comment

	tx, err := c.DB.Begin()
	if err != nil {
		return Result{}, internal(err)
	}
	defer tx.Rollback()
	// 改规则不改变发起方名下的规则条数，账号的条数上限不用再查。入口换了节点时，这条
	// 规则开始占新入口那条授权的名额。
	if moved {
		var use usage
		use.add(entry, grant)
		if cerr := withinNodeCaps(tx, actor.User, use); cerr != nil {
			return Result{}, cerr
		}
	}
	if err := db.UpdateRuleHeader(tx, rl); err != nil {
		return Result{}, internal(err)
	}
	entryAddr, entryV6, affected, err := db.RegenerateRule(tx, rl, hops, nil)
	if err != nil {
		return Result{}, regenerateRefused(err)
	}
	if err := tx.Commit(); err != nil {
		return Result{}, internal(err)
	}
	c.audit(actor, "save", rl.ID, rl.Name)
	return Result{Rule: rl, Entry: entryAddr, EntryV6: entryV6, Hops: hops, Affected: affected}, nil
}

// Delete 以 actor 的名义删掉整条规则（它在所有节点上的跳）。
func (c *Changes) Delete(actor Actor, ruleID int64) (Result, *Error) {
	rl, stored, cerr := c.load(ruleID)
	if cerr != nil {
		return Result{}, cerr
	}
	if cerr := actor.mayRemove(rl, stored); cerr != nil {
		return Result{}, cerr
	}
	nodes, err := db.DeleteRule(c.DB, ruleID)
	if err != nil {
		return Result{}, internal(err)
	}
	c.audit(actor, "delete", ruleID, actor.origin())
	return Result{Rule: rl, Affected: nodes}, nil
}

// HopEdit 是节点对规则里自己那一跳的修改：监听端口、转发模式、备注。
type HopEdit struct {
	ListenPort int
	Mode       string
	Comment    string
}

// EditHop 把节点 actor.Node 对规则 ruleID 里自己那一跳的修改落库。规则里一个物理节点
// 至多出现一次（db.RegenerateRule 拒绝重复，db.ErrDuplicateChainNode），所以
// (ruleID, 节点) 唯一确定一跳。其余各跳原样保留；监听端口变了，上游那一跳的目标端口
// 由 RegenerateRule 跟着改，所以 Affected 可能含别的节点。
func (c *Changes) EditHop(actor Actor, ruleID int64, e HopEdit) (Result, *Error) {
	if actor.Node == 0 {
		return Result{}, forbidden("只有节点可以改自己那一跳")
	}
	rl, stored, cerr := c.load(ruleID)
	if cerr != nil {
		return Result{}, cerr
	}
	if !onRule(stored, actor.Node) {
		return Result{}, forbidden("节点不在该规则上")
	}
	inputs := db.StoredHopInputs(stored)
	for i, sh := range stored {
		if sh.NodeID == actor.Node {
			inputs[i].DesiredPort = e.ListenPort
			inputs[i].Mode = e.Mode
			inputs[i].Comment = e.Comment
		}
	}
	res, cerr := c.regenerate(rl, inputs, nil)
	if cerr != nil {
		return Result{}, cerr
	}
	c.audit(actor, "hop_edit", ruleID, actor.origin())
	return res, nil
}

// ReallocateHop 给规则 ruleID 的第 pos 跳换监听端口：port 大于 0 时换成它，否则让面板
// 另挑一个（避开现在这个）。只有面板上的管理员可以。
func (c *Changes) ReallocateHop(actor Actor, ruleID int64, pos, port int) (Result, *Error) {
	if !actor.onPanel() {
		return Result{}, forbidden("只有管理员可以重新分配端口")
	}
	rl, stored, cerr := c.load(ruleID)
	if cerr != nil {
		return Result{}, cerr
	}
	if pos < 0 || pos >= len(stored) {
		return Result{}, invalid("跳序号非法")
	}
	inputs := db.StoredHopInputs(stored)
	var avoid map[int64]int
	if port > 0 {
		inputs[pos].DesiredPort = port
	} else {
		avoid = map[int64]int{stored[pos].NodeID: stored[pos].ListenPort}
	}
	res, cerr := c.regenerate(rl, inputs, avoid)
	if cerr != nil {
		return Result{}, cerr
	}
	c.audit(actor, "reallocate", ruleID, strconv.Itoa(pos))
	return res, nil
}

// --- 判定 ---

// mayEdit 判断 actor 能不能改规则 rl（它落库的链是 stored）。
//
// 面板上的管理员能改任何规则。其余发起方只能改自己名下的。节点另有一层限制：规则必须
// 只有一跳、且这一跳就在这台节点上——节点的表单编辑的是“这台机器上的一条转发”，多跳
// 规则的其余部分不归它管（它对多跳规则能做的是 EditHop）。
func (a Actor) mayEdit(rl *db.Rule, stored []*db.RuleHop) *Error {
	if a.onPanel() {
		return nil
	}
	if !rl.OwnerID.Valid || rl.OwnerID.Int64 != a.User.ID {
		return forbidden("无权操作该规则")
	}
	if a.Node != 0 {
		if !onRule(stored, a.Node) {
			return forbidden("节点不在该规则上")
		}
		if len(stored) != 1 {
			return forbidden("仅支持编辑单跳规则")
		}
	}
	return nil
}

// mayRemove 判断 actor 能不能删规则 rl。节点能删经过自己的任何规则，不论规则归谁、有
// 几跳（节点 TUI 的“删整链”）。其余同 mayEdit。
func (a Actor) mayRemove(rl *db.Rule, stored []*db.RuleHop) *Error {
	if a.Node != 0 {
		if !onRule(stored, a.Node) {
			return forbidden("节点不在该规则上")
		}
		return nil
	}
	if a.onPanel() {
		return nil
	}
	if a.User == nil || !rl.OwnerID.Valid || rl.OwnerID.Int64 != a.User.ID {
		return forbidden("无权操作该规则")
	}
	return nil
}

// pathAllowed 检查 p 的给法在 actor 的权限之内。creating 区分建规则与改规则。
func (a Actor) pathAllowed(p Path, creating bool) *Error {
	if len(p.Hops) > 0 && !a.onPanel() {
		return forbidden("只有管理员可以直接指定物理跳")
	}
	if a.Node == 0 {
		return nil
	}
	// 节点的规则只住在它自己身上：建规则时入口必须是它自己，改规则时不能换链。
	if creating {
		if p.Entry != a.Node || len(p.vias()) > 0 {
			return forbidden("节点只能在自己身上建规则")
		}
		return nil
	}
	if p.Entry != 0 || p.Vias != nil {
		return forbidden("节点侧不能改规则走哪条链")
	}
	return nil
}

// access 检查 u 能把 entry 与 vias 这些逻辑节点用进规则，返回约束入口的那条授权（管
// 理员为 nil）。判定在 db.NodeAccess。组合是授权的单位：授权了组合就能用它展开出的整
// 条链，所以这里查的是被点名的逻辑节点，不是它们的成员。
func (c *Changes) access(u *db.User, entry int64, vias []int64) (*db.UserNode, *Error) {
	grant, ok, err := db.NodeAccess(c.DB, u, entry)
	if err != nil {
		return nil, internal(err)
	}
	if !ok {
		return nil, forbidden("无权使用该节点")
	}
	for _, via := range vias {
		_, ok, err := db.NodeAccess(c.DB, u, via)
		if err != nil {
			return nil, internal(err)
		}
		if !ok {
			return nil, forbidden("无权使用中转节点")
		}
	}
	return grant, nil
}

// usage 记一次变更要新占的名额：一共几条规则，其中以各个入口计的各几条、各受哪条授
// 权约束。
type usage struct {
	rules   int
	entries []int64
	adding  map[int64]int
	grants  map[int64]*db.UserNode
}

// add 记一条新规则。grant 为 nil（管理员，或管理员直接给物理跳的规则）时它不占任何
// 授权的名额。
func (u *usage) add(entry int64, grant *db.UserNode) {
	u.rules++
	if grant == nil {
		return
	}
	if u.adding == nil {
		u.adding = map[int64]int{}
		u.grants = map[int64]*db.UserNode{}
	}
	if _, seen := u.grants[entry]; !seen {
		u.entries = append(u.entries, entry)
		u.grants[entry] = grant
	}
	u.adding[entry]++
}

// withinCaps 检查 owner 再添 use 这些规则之后仍在条数上限之内。
//
// 两个上限数的都是规则条数，一条规则占一个名额，与链有几跳无关：授权的上限数
// owner 以该节点为入口的规则，账号的上限（users.max_forwards）数 owner 名下全部规则。
// 管理员没有上限：users.max_forwards 只约束普通用户（管理员账号的这一列默认是 0），
// 他也没有授权行。
//
// 必须在写入的事务里调用：db.Open 把连接数限成 1，一个事务从开始到提交独占这个连
// 接，于是“数现有条数”与“插入”之间不会插进另一次建规则。
func withinCaps(tx db.DBTX, owner *db.User, use usage) *Error {
	if cerr := withinNodeCaps(tx, owner, use); cerr != nil {
		return cerr
	}
	if owner.Role == "admin" {
		return nil
	}
	total, err := db.CountRulesForUser(tx, owner.ID)
	if err != nil {
		return internal(err)
	}
	if total+use.rules > owner.MaxForwards {
		return conflict("超出用户最大转发数（%d）", owner.MaxForwards)
	}
	return nil
}

// withinNodeCaps 是 withinCaps 里授权上限的那一半。改规则换入口时只查这一半。
func withinNodeCaps(tx db.DBTX, owner *db.User, use usage) *Error {
	for _, entry := range use.entries {
		grant := use.grants[entry]
		n, err := db.CountRulesForUserNode(tx, owner.ID, entry)
		if err != nil {
			return internal(err)
		}
		if n+use.adding[entry] > grant.MaxForwards {
			return conflict("超出该节点的转发上限（%d）", grant.MaxForwards)
		}
	}
	return nil
}

// --- 表头 ---

type header struct {
	name, proto, host, comment string
	port                       int
}

// header 校验并规整表头。stored 是被改的规则，建规则时为 nil。
//
// 规则名：面板上的发起方必须给。节点的表单没有规则名这一栏——改规则时保留原名，建规
// 则时请求里带了就用，没带就取备注，备注也没有取出口地址，所以每条新建的规则都有非
// 空的名字。
func (a Actor) header(f Fields, stored *db.Rule) (header, *Error) {
	h := header{
		name:    strings.TrimSpace(f.Name),
		proto:   strings.ToLower(strings.TrimSpace(f.Proto)),
		comment: strings.TrimSpace(f.Comment),
	}
	if a.Node == 0 && h.name == "" {
		return header{}, invalid("名称必填")
	}
	if !ValidProto(h.proto) {
		return header{}, invalid("协议须为 tcp、udp 或 tcp+udp")
	}
	host, port, err := ParseExit(f.Exit)
	if err != nil {
		return header{}, invalid("%s", err.Error())
	}
	h.host, h.port = host, port
	if a.Node != 0 {
		switch {
		case stored != nil:
			h.name = stored.Name
		case h.name == "" && h.comment != "":
			h.name = h.comment
		case h.name == "":
			h.name = net.JoinHostPort(host, strconv.Itoa(port))
		}
	}
	return h, nil
}

// ValidProto 报告 proto 是不是可转发的协议。tcp+udp 由数据面拆成两条（见
// forward.Partition）。
func ValidProto(proto string) bool {
	switch proto {
	case "tcp", "udp", "tcp+udp":
		return true
	}
	return false
}

// ParseExit 把 host:port 形式的转发目标拆开并校验：端口在 1–65535，主机是 IP 字面量
// 或形似域名的字符串。
func ParseExit(raw string) (string, int, error) {
	raw = strings.TrimSpace(raw)
	host, portStr, err := net.SplitHostPort(raw)
	if err != nil {
		// 多个冒号又不以 '[' 开头，不可能是 IPv4 或域名加端口，只能是漏了方括号的
		// IPv6 字面量。
		if !strings.HasPrefix(raw, "[") && strings.Count(raw, ":") >= 2 {
			return "", 0, fmt.Errorf("IPv6 地址需要用方括号包裹，例如 [::1]:1080")
		}
		return "", 0, fmt.Errorf("出口需为 host:port 形式")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("出口端口非法")
	}
	if host == "" {
		return "", 0, fmt.Errorf("出口地址不能为空")
	}
	if net.ParseIP(host) == nil && !resolver.PlausibleHostname(host) {
		return "", 0, fmt.Errorf("出口地址非法：%q 不是合法 IP 或域名", host)
	}
	return host, port, nil
}

// JoinExit 把分开给出的目标主机与端口拼成 Fields.Exit 的形式。agent 通道上的命令是
// 分开带这两项的。
func JoinExit(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// --- 链 ---

// ownerSeats 返回规则属主 owner 的各条授权对节点入口/中转资格的覆盖，键是节点 id，
// 只含确实覆盖了的（见 db.GrantSeats）。推导链时，一个节点能不能做入口或中转按规则
// 属主的授权判定，属主没有覆盖的节点才看节点自身的资格。管理员不受授权约束，没有
// 覆盖。
//
// 读授权失败返回错误，不退回“没有覆盖”：覆盖可以收窄资格（节点自身是入口+中转、授
// 权只给中转），把读失败当成没有覆盖会放宽判定。
func (c *Changes) ownerSeats(owner *db.User) (map[int64]chain.Seat, *Error) {
	if owner == nil || owner.Role == "admin" {
		return nil, nil
	}
	_, grants, err := db.ListNodesForUser(c.DB, owner.ID)
	if err != nil {
		return nil, internal(err)
	}
	seats := map[int64]chain.Seat{}
	for _, g := range grants {
		if s, ok := db.GrantSeats(g.Roles); ok {
			seats[g.NodeID] = s
		}
	}
	return seats, nil
}

// ruleOwnerSeats 是 ownerSeats 用在已存规则上：属主取规则的，不取发起方的——管理员替
// 用户改规则时，链仍按那个用户的授权推导。规则没有属主（账号已删）时没有覆盖。
func (c *Changes) ruleOwnerSeats(rl *db.Rule) (map[int64]chain.Seat, *Error) {
	if !rl.OwnerID.Valid {
		return nil, nil
	}
	owner, err := db.GetUserByID(c.DB, rl.OwnerID.Int64)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, internal(err)
	}
	return c.ownerSeats(owner)
}

// explicitChain 把管理员直接给出的物理跳序列（p.Hops）变成链，并算出这条链在表头里
// 的逻辑路径：入口节点与中转层。这是搭任意链的通道，有意不校验角色与绑定边；链尾
// 能否直接转发仍然检查（chain.CheckExit）。
//
// 没写 ViaNodeID 的跳自成一段：按授权计流量时一个逻辑段只在段首记一次、记到
// ViaNodeID 指的逻辑节点上（accounting.Place 的 SegmentStart 与 Segment），把组合
// 展开写成显式链时各跳要带组合的 id。
//
// 逻辑路径由跳算出（db.LogicalPath），不由请求另给。表头的入口与中转层有三处读者：
// 页面保存规则时把它们原样发回来，Update 据此重新推导整条链；Update 用它们查发起方
// 的授权；记账取入口节点的倍率（accounting 的 bookSQL）。表头与跳各说各的时，下一次
// 页面保存就会把链换成表头说的那一条。所以：
//
//   - 每一跳所属的逻辑节点必须存在（入口要写进 rules.node_id）；
//   - p.Entry、p.Vias 给了，就必须与算出的一致。它们不一致时没有哪一边是“对的”，
//     请求自相矛盾，拒绝。
func explicitChain(topo chain.Topology, p Path) (inputs []db.HopInput, entry int64, vias []int64, cerr *Error) {
	hops := make([]chain.Hop, len(p.Hops))
	segments := make([]int64, len(p.Hops))
	for i, h := range p.Hops {
		if h.ViaNodeID == 0 {
			h.ViaNodeID = h.NodeID
		}
		if _, ok := topo.Nodes[h.ViaNodeID]; !ok {
			return nil, 0, nil, invalid("节点 %d 不存在", h.ViaNodeID)
		}
		hops[i], segments[i] = h, h.ViaNodeID
	}
	entry, vias, err := db.LogicalPath(segments)
	if err != nil {
		return nil, 0, nil, invalid("%s", err.Error())
	}
	if p.Entry != 0 && p.Entry != entry {
		return nil, 0, nil, invalid("node_id 与 hops 不一致：按 hops，入口节点是 %d", entry)
	}
	if p.Vias != nil && !slices.Equal(*p.Vias, vias) {
		return nil, 0, nil, invalid("via_node_ids 与 hops 不一致：按 hops，中转层是 %v", append([]int64{}, vias...))
	}
	if err := topo.CheckExit(hops); err != nil {
		return nil, 0, nil, invalid("%s", err.Error())
	}
	return hopInputs(hops), entry, vias, nil
}

func hopInputs(hops []chain.Hop) []db.HopInput {
	inputs := make([]db.HopInput, len(hops))
	for i, h := range hops {
		inputs[i] = db.HopInput{NodeID: h.NodeID, Mode: h.Mode, ViaNodeID: h.ViaNodeID}
	}
	return inputs
}

func onRule(stored []*db.RuleHop, nodeID int64) bool {
	for _, sh := range stored {
		if sh.NodeID == nodeID {
			return true
		}
	}
	return false
}

// --- 落库 ---

func (c *Changes) load(ruleID int64) (*db.Rule, []*db.RuleHop, *Error) {
	rl, err := db.GetRule(c.DB, ruleID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, &Error{Kind: NotFound, Msg: "规则不存在"}
	}
	if err != nil {
		return nil, nil, internal(err)
	}
	stored, err := db.ListRuleHops(c.DB, ruleID)
	if err != nil {
		return nil, nil, internal(err)
	}
	return rl, stored, nil
}

// regenerate 在一个事务里按 inputs 重新生成规则 rl 的跳，表头不动。
func (c *Changes) regenerate(rl *db.Rule, inputs []db.HopInput, avoid map[int64]int) (Result, *Error) {
	tx, err := c.DB.Begin()
	if err != nil {
		return Result{}, internal(err)
	}
	defer tx.Rollback()
	entry, entryV6, affected, err := db.RegenerateRule(tx, rl, inputs, avoid)
	if err != nil {
		return Result{}, regenerateRefused(err)
	}
	if err := tx.Commit(); err != nil {
		return Result{}, internal(err)
	}
	return Result{Rule: rl, Entry: entry, EntryV6: entryV6, Hops: inputs, Affected: affected}, nil
}

// regenerateRefused 给 db.RegenerateRule 的错误分类。db.ChainError 说的是这条链按要求
// 搭不出来（节点缺数据面地址、要的端口用不了……），要改的是请求或节点的配置：同一个
// 物理节点在链上出现两次是发起方选的入口与中转层互相冲突，算 Conflict，其余算
// Invalid。不是 ChainError 的错误是语句没执行成，与这条链对不对无关，算 Internal——
// 按 Invalid 回给发起方，是让他去改一条没有问题的规则。
func regenerateRefused(err error) *Error {
	switch {
	case errors.Is(err, db.ErrDuplicateChainNode):
		return &Error{Kind: Conflict, Msg: err.Error(), cause: err}
	case db.IsChainError(err):
		return &Error{Kind: Invalid, Msg: err.Error(), cause: err}
	default:
		return internal(err)
	}
}

// origin 是写进审计备注里的来源：节点发起的变更记下是哪台节点。
func (a Actor) origin() string {
	if a.Node == 0 {
		return ""
	}
	return fmt.Sprintf("node=%d", a.Node)
}

// audit 记一条审计。动作名按发起方分三组：面板上的管理员是 rule.<op>，普通用户是
// rule.user_<op>，节点是 rule.node_<op>（记在节点属主名下，没有属主时不记账号）。
func (c *Changes) audit(actor Actor, op string, ruleID int64, payload string) {
	action := "rule.user_" + op
	switch {
	case actor.Node != 0:
		action = "rule.node_" + op
	case actor.admin():
		action = "rule." + op
	}
	target := strconv.FormatInt(ruleID, 10)
	if actor.User == nil {
		db.WriteAuditNoUser(c.DB, action, target, payload)
		return
	}
	db.WriteAudit(c.DB, actor.User.ID, action, target, payload)
}
