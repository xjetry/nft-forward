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

// Type constants. Strings (not iota) so the wire is self-describing
// when debugging with `wscat`/`websocat`.
const (
	TypeHello             = "hello"
	TypeHelloAck          = "hello_ack"
	TypeRegisterLocal     = "register_local"
	TypeRegisterLocalAck  = "register_local_ack"
	TypeApplyRuleset      = "apply_ruleset"
	TypeApplyAck          = "apply_ack"
	TypeCounters          = "counters"
	TypeTuiSegmentChanged = "tui_segment_changed"
	TypePanelSegmentEdit  = "panel_segment_edit"
	TypeChainHopEdit      = "chain_hop_edit"
	TypeChainDelete       = "chain_delete"
	TypeChainCmdAck       = "chain_cmd_ack"
	TypePing              = "ping"
	TypePong              = "pong"
	TypeError             = "error"
)

// Envelope wraps every frame. ID is required for req/resp pairs
// (hello/hello_ack, register_local/register_local_ack, apply_ruleset/
// apply_ack, ping/pong) so the sender can match an ack back to its
// outstanding request; notification frames (counters,
// tui_segment_changed, server-initiated error) leave ID empty.
type Envelope struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Forward is the panel-side rule view shared by register_local and
// tui_segment_changed. It intentionally differs from nft.Rule: panel
// storage doesn't need DestHost (daemon-resolved) or the wire-internal
// ID, and uses listen_port/target_ip/target_port field names that match
// the forwards DB schema rather than nft.Rule's kernel-side names.
type Forward struct {
	Proto         string `json:"proto"`
	ListenPort    int    `json:"listen_port"`
	TargetIP      string `json:"target_ip"`
	TargetPort    int    `json:"target_port"`
	Comment       string `json:"comment,omitempty"`
	BandwidthMbps int    `json:"bandwidth_mbps,omitempty"`
	Mode          string `json:"mode,omitempty"`
}

type Hello struct {
	NodeToken      string `json:"node_token"`
	AgentVersion   string `json:"agent_version"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	LastAppliedRev string `json:"last_applied_rev,omitempty"`
}

// HelloAck is the panel's response to Hello. Error == "" means the
// node_token was accepted and NodeID/Name are populated; a non-empty
// Error means the daemon should not proceed (token revoked or unknown).
type HelloAck struct {
	NodeID int64  `json:"node_id,omitempty"`
	Name   string `json:"name,omitempty"`
	Error  string `json:"error,omitempty"`
}

type RegisterLocal struct {
	Forwards []Forward `json:"forwards"`
}

// ImportedForward identifies a single row inserted into the forwards
// table during register_local processing. RuleID is the server-assigned
// DB rowid; the agent uses (ListenPort, Proto) to correlate it back to
// the source tui-segment rule.
type ImportedForward struct {
	ListenPort int    `json:"listen_port"`
	Proto      string `json:"proto"`
	RuleID     int64  `json:"rule_id"`
}

// RegisterLocalAck is the panel's response to RegisterLocal. Error == ""
// indicates the tui-segment forwards were persisted (Imported lists the
// new DB rule IDs; empty Imported on a repeat call means the node was
// already migrated and the request was a no-op).
type RegisterLocalAck struct {
	Imported []ImportedForward `json:"imported"`
	Error    string            `json:"error,omitempty"`
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
	Rev   string `json:"rev"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// CounterSample is a per-rule traffic delta since the last counters
// frame. The server adds BytesDelta to forwards.total_bytes; the rule
// is located by (node_id, listen_port, proto) — there is no explicit
// rule_id on the wire because agent restarts re-key the same rule.
type CounterSample struct {
	ListenPort int    `json:"listen_port"`
	Proto      string `json:"proto"`
	BytesDelta int64  `json:"bytes_delta"`
}

type Counters struct {
	Samples []CounterSample `json:"samples"`
}

type TuiSegmentChanged struct {
	Forwards []Forward `json:"forwards"`
}

// PanelSegmentEdit carries a node's edits to its panel-segment forwards
// back to the server. It mirrors TuiSegmentChanged: a full snapshot of the
// segment, not a delta. The server locates each forward by
// (node_id, proto, listen_port), reads chain_id from the DB to decide
// whether the row is editable, and persists non-chain edits into the
// forwards table — so chain_id never needs to ride on the wire.
type PanelSegmentEdit struct {
	Forwards []Forward `json:"forwards"`
}

// ChainHopEdit carries a node's edit to its single hop in a relay chain.
// The hop is located server-side by (chain_id, connection node) — a chain
// can't repeat a node — so neither position nor target rides on the wire.
// Only listen_port/mode/comment are editable; the server recomputes targets
// and uses chain.proto, so the relay skeleton can't be rewritten from a node.
type ChainHopEdit struct {
	ChainID    int64  `json:"chain_id"`
	ListenPort int    `json:"listen_port"`
	Mode       string `json:"mode,omitempty"`
	Comment    string `json:"comment,omitempty"`
}

// ChainDelete asks the server to delete an entire chain (all hops on all
// nodes), identified by ChainID. The requesting node must participate in it.
type ChainDelete struct {
	ChainID int64 `json:"chain_id"`
}

// ChainCmdAck is the server's reply to ChainHopEdit/ChainDelete, matched to
// the request via Envelope.ID. OK==true requires Error=="". Entry is only
// meaningful on a successful ChainHopEdit, where it carries the chain's
// copyable entry endpoint; a ChainDelete ack leaves it empty because the
// deleted chain has no endpoint left to surface. Mirrors ApplyAck's OK+Error
// contract: OK is the load-bearing signal, Error is human context.
type ChainCmdAck struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Entry string `json:"entry,omitempty"`
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
