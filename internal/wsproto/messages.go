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
// tui_segment_changed. Renamed from nft.Rule because the server side
// stores forwards in a separate table whose columns match these fields.
type Forward struct {
	Proto         string `json:"proto"`
	ListenPort    int    `json:"listen_port"`
	TargetIP      string `json:"target_ip"`
	TargetPort    int    `json:"target_port"`
	Comment       string `json:"comment,omitempty"`
	BandwidthMbps int    `json:"bandwidth_mbps,omitempty"`
}

type Hello struct {
	NodeToken      string `json:"node_token"`
	AgentVersion   string `json:"agent_version"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	LastAppliedRev string `json:"last_applied_rev,omitempty"`
}

type HelloAck struct {
	NodeID int64  `json:"node_id,omitempty"`
	Name   string `json:"name,omitempty"`
	Error  string `json:"error,omitempty"`
}

type RegisterLocal struct {
	Forwards []Forward `json:"forwards"`
}

type ImportedForward struct {
	ListenPort int    `json:"listen_port"`
	Proto      string `json:"proto"`
	RuleID     int64  `json:"rule_id"`
}

type RegisterLocalAck struct {
	Imported []ImportedForward `json:"imported"`
	Error    string            `json:"error,omitempty"`
}

type ApplyRuleset struct {
	Rev   string     `json:"rev"`
	Rules []nft.Rule `json:"rules"`
}

type ApplyAck struct {
	Rev   string `json:"rev"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

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

type Ping struct {
	TS int64 `json:"ts"`
}

type Pong struct {
	TS int64 `json:"ts"`
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
