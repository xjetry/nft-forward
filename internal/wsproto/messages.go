// Package wsproto defines the JSON message envelope and payload types
// exchanged between the daemon's dialer and the server's hub over
// WebSocket. It carries no I/O — both sides decode/encode through
// encoding/json — so it can be imported from either side without
// dragging in network code.
package wsproto

import (
	"encoding/json"

	"nft-forward/internal/nft"
)

// LocalAgentSocketPath 是面板“本机入口”的 unix socket 的默认路径：面板在这上面只
// 提供 /v1/agents，面板宿主机上的 daemon 经它用同一套帧协议接入（面板把从这里进来
// 的连接认定为 self 节点）。两端的默认值都取这一个常量——面板的 --agent-socket 与
// daemon 的 --panel-socket——改路径时两边一起变。
//
// 它不放在 daemon 的 systemd 运行时目录（/run/nft-forward）里：那个目录归 daemon 的
// 单元所有，daemon 进程每次停止——包括崩溃后被 systemd 拉起——时整个被删掉，放在里面
// 的 socket 文件会跟着消失。放在外面，这个 socket 的存续只取决于面板进程自己。
const LocalAgentSocketPath = "/var/run/nft-forward-panel.sock"

// Type constants. Strings (not iota) so the wire is self-describing
// when debugging with `wscat`/`websocat`.
const (
	TypeHello        = "hello"
	TypeHelloAck     = "hello_ack"
	TypeApplyRuleset = "apply_ruleset"
	TypeApplyAck     = "apply_ack"
	TypeCounters     = "counters"
	TypeRuleHopEdit  = "rule_hop_edit"
	TypeRuleDelete   = "rule_delete"
	TypeRuleCmdAck   = "rule_cmd_ack"
	TypeRuleCreate   = "rule_create"
	TypeRuleUpdate   = "rule_update"
	TypeMigrateRules = "migrate_rules"
	TypeUpgrade      = "upgrade"
	TypeUpgradeAck   = "upgrade_ack"
	TypeProbe        = "probe"
	TypeProbeAck     = "probe_ack"
	TypePing         = "ping"
	TypePong         = "pong"
	TypeError        = "error"
	TypeConfigUpdate = "config_update"
)

// Envelope wraps every frame. ID is required for req/resp pairs
// (hello/hello_ack, apply_ruleset/apply_ack, ping/pong) so the sender
// can match an ack back to its outstanding request; notification frames
// (counters, server-initiated error) leave ID empty.
type Envelope struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type RuleCreate struct {
	Proto      string `json:"proto"`
	ExitHost   string `json:"exit_host"`
	ExitPort   int    `json:"exit_port"`
	ListenPort int    `json:"listen_port"`
	Mode       string `json:"mode"`
	Comment    string `json:"comment"`
	Name       string `json:"name"`
}

// RuleUpdate 是节点对自己那条单跳规则的修改。它不携带规则名：规则名只在面板上
// 编辑，节点 TUI 的规则表单没有这一栏，面板对节点发起的修改一律保留原名
// （rulechange 的 Actor.header）。旧版节点的帧里还带着 name 字段，面板解码时把它
// 当未知字段丢弃。
type RuleUpdate struct {
	RuleID     int64  `json:"rule_id"`
	Proto      string `json:"proto"`
	ExitHost   string `json:"exit_host"`
	ExitPort   int    `json:"exit_port"`
	ListenPort int    `json:"listen_port"`
	Mode       string `json:"mode"`
	Comment    string `json:"comment"`
}

type MigrateRules struct {
	Rules []nft.Rule `json:"rules"`
}

type Hello struct {
	NodeToken    string `json:"node_token"`
	AgentVersion string `json:"agent_version"`
	// AgentSHA is the sha256 of the running nft-agent binary — the identity the
	// panel compares against the agent it would push to decide whether a push is
	// needed at all. Empty from agents that predate the split.
	AgentSHA       string `json:"agent_sha,omitempty"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	LastAppliedRev string `json:"last_applied_rev,omitempty"`
	PortRange      string `json:"port_range,omitempty"`
	// ProbedV4/ProbedV6 are this agent's own best-guess outbound address per
	// family, re-probed fresh on every hello. The panel only uses these to
	// seed the family its own connection-observed address didn't cover —
	// see hub.go's fillNodeRelayHosts. Empty from agents that predate this probe.
	ProbedV4 string `json:"probed_v4,omitempty"`
	ProbedV6 string `json:"probed_v6,omitempty"`
	// DeclaredRelayHost/DeclaredRelayHostV6 are explicit operator-provided
	// addresses (daemon --relay-host/--relay-host-v6), distinct from
	// ProbedV4/ProbedV6 which are the agent's own outbound-route guesses.
	// When present they are authoritative and override whatever is in the
	// DB, unlike ProbedV4/ProbedV6 which only ever seed an empty field —
	// see hub.go's applyDeclaredRelayHosts.
	DeclaredRelayHost   string `json:"declared_relay_host,omitempty"`
	DeclaredRelayHostV6 string `json:"declared_relay_host_v6,omitempty"`
}

// HelloAck is the panel's response to Hello. Error == "" means the
// node_token was accepted and NodeID/Name are populated; a non-empty
// Error means the daemon should not proceed (token revoked or unknown).
//
// PoolSize is the panel's connection-pool size setting. It is a pointer so
// that "the panel says 0" stays distinguishable on the wire from "the panel
// sent no value" (an error ack, or a panel that predates the setting): the
// agent applies any value that is present, 0 included, and keeps its own
// default only when the field is absent.
type HelloAck struct {
	NodeID   int64  `json:"node_id,omitempty"`
	Name     string `json:"name,omitempty"`
	Error    string `json:"error,omitempty"`
	PoolSize *int   `json:"pool_size,omitempty"`
}

type ApplyRuleset struct {
	Rev   string     `json:"rev"`
	Rules []nft.Rule `json:"rules"`
}

// ApplyAck is the agent's response to apply_ruleset. Peers must
// disambiguate success vs failure using OK *and* Error together:
// OK==true requires Error==""; OK==false requires Error!="". A peer
// that ever sees OK==false && Error=="" (or vice versa) should treat
// the ack as malformed — the OK bool is the load-bearing signal, the
// Error string is human-readable context for it.
type ApplyAck struct {
	Rev     string `json:"rev"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Warning string `json:"warning,omitempty"`
}

// CounterSample is a per-rule traffic delta since the last counters
// frame. The server adds BytesUp/BytesDown to the directional totals;
// the rule is located by (node_id, listen_port, proto) — there is no
// explicit rule_id on the wire because agent restarts re-key the same rule.
type CounterSample struct {
	ListenPort int    `json:"listen_port"`
	Proto      string `json:"proto"`
	BytesUp    int64  `json:"up"`
	BytesDown  int64  `json:"down"`
	// BytesDelta is the legacy field sent by pre-v0.33 agents that do not
	// distinguish direction. The server falls back to it when up+down is zero.
	BytesDelta int64 `json:"bytes_delta,omitempty"`
}

type Counters struct {
	Samples []CounterSample `json:"samples"`
}

// RuleHopEdit carries a node's edit to its single hop in a relay rule.
// The hop is located server-side by (rule_id, connection node) — a rule
// can't repeat a node — so neither position nor target rides on the wire.
// Only listen_port/mode/comment are editable; the server recomputes targets
// and uses rule.proto, so the relay skeleton can't be rewritten from a node.
type RuleHopEdit struct {
	RuleID     int64  `json:"rule_id"`
	ListenPort int    `json:"listen_port"`
	Mode       string `json:"mode,omitempty"`
	Comment    string `json:"comment,omitempty"`
}

// RuleDelete asks the server to delete an entire rule (all hops on all
// nodes), identified by RuleID. The requesting node must participate in it.
type RuleDelete struct {
	RuleID int64 `json:"rule_id"`
}

// RuleCmdAck is the server's reply to RuleHopEdit/RuleDelete, matched to
// the request via Envelope.ID. OK==true requires Error=="". Entry is only
// meaningful on a successful RuleHopEdit, where it carries the rule's
// copyable entry endpoint; a RuleDelete ack leaves it empty because the
// deleted rule has no endpoint left to surface. Mirrors ApplyAck's OK+Error
// contract: OK is the load-bearing signal, Error is human context.
type RuleCmdAck struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Entry string `json:"entry,omitempty"`
}

type Upgrade struct {
	Version    string `json:"version"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
	DownloadAt string `json:"download_at"`
	// Data, when non-empty, carries the binary inline so daemons that cannot
	// reach the panel over HTTP still upgrade over the WS link. DownloadAt
	// remains the fallback for daemons that predate inline transport.
	Data []byte `json:"data,omitempty"`
}

type UpgradeAck struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

type Probe struct {
	Target string `json:"target"`
	// Proto is "tcp" (default when empty, so pre-upgrade agents and servers
	// stay compatible) or "udp".
	Proto string `json:"proto,omitempty"`
}

type ProbeAck struct {
	OK      bool   `json:"ok"`
	Latency int    `json:"latency_ms"`
	Error   string `json:"error,omitempty"`
}

type Ping struct {
	TS int64 `json:"ts"`
}

type Pong struct {
	TS int64 `json:"ts"`
}

// Error is a server-initiated notification frame (not a req/resp ack).
// Code is a short machine-friendly identifier; Message is human-readable
// detail. Agents should log it; they should not assume the connection
// will close.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ConfigUpdate struct {
	PoolSize int `json:"pool_size"`
}
