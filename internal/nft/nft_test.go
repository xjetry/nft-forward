package nft

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"nft-forward/internal/resolver"
)

func TestValidateAcceptsIPOnly(t *testing.T) {
	r := Rule{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80}
	if err := Validate(r); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
}

func TestValidateAcceptsHostOnly(t *testing.T) {
	r := Rule{Proto: "tcp", SrcPort: 80, DestHost: "home.example.net", DestPort: 80}
	if err := Validate(r); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
}

func TestValidateRejectsNeither(t *testing.T) {
	r := Rule{Proto: "tcp", SrcPort: 80, DestPort: 80}
	if err := Validate(r); err == nil {
		t.Fatal("expected error when both DestIP and DestHost empty")
	}
}

func TestValidateRejectsBadHost(t *testing.T) {
	r := Rule{Proto: "tcp", SrcPort: 80, DestHost: "bad host name!", DestPort: 80}
	if err := Validate(r); err == nil {
		t.Fatal("expected error on invalid host")
	}
}

func TestValidateAcceptsTCPUDP(t *testing.T) {
	r := Rule{Proto: "tcp+udp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80}
	if err := Validate(r); err != nil {
		t.Fatalf("expected tcp+udp to be valid, got %v", err)
	}
}

func TestValidateRejectsUnknownCompositeProto(t *testing.T) {
	r := Rule{Proto: "tcp+udp+icmp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80}
	if err := Validate(r); err == nil {
		t.Fatal("expected error for tcp+udp+icmp, got nil")
	}
}

func TestRenderRulesetTCPUDPUsesSetSyntax(t *testing.T) {
	out := RenderRuleset([]Rule{{
		Proto: "tcp+udp", SrcPort: 8080, DestIP: "10.0.0.5", DestPort: 8080,
	}})
	if !contains(out, "meta l4proto { tcp, udp }") {
		t.Fatalf("expected set-syntax l4proto match, got:\n%s", out)
	}
	if !contains(out, "th dport") {
		t.Fatalf("expected th dport keyword, got:\n%s", out)
	}
	if contains(out, "tcp dport") {
		t.Fatalf("must not emit single-protocol tcp dport, got:\n%s", out)
	}
	if contains(out, "udp dport") {
		t.Fatalf("must not emit single-protocol udp dport, got:\n%s", out)
	}
}

func TestRenderRulesetTCPSingleProtocol(t *testing.T) {
	out := RenderRuleset([]Rule{{
		Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80,
	}})
	if !contains(out, "tcp dport 80") {
		t.Fatalf("expected single-protocol tcp dport 80, got:\n%s", out)
	}
	if contains(out, "meta l4proto { tcp, udp }") {
		t.Fatalf("single tcp rule must not use the tcp+udp set syntax, got:\n%s", out)
	}
}

func TestRenderRulesetUsesDestIP(t *testing.T) {
	out := RenderRuleset([]Rule{{
		Proto: "tcp", SrcPort: 80, DestHost: "home.example.net",
		DestIP: "10.0.0.5", DestPort: 80,
	}})
	if !contains(out, "dnat ip to 10.0.0.5:80") {
		t.Fatalf("renderer must use DestIP, got:\n%s", out)
	}
	if contains(out, "home.example.net") {
		t.Fatalf("renderer leaked host into nft script:\n%s", out)
	}
}

func TestRenderRulesetMasqueradeScopedToDNAT(t *testing.T) {
	out := RenderRuleset([]Rule{
		{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 8080},
	})
	if !contains(out, "ip daddr 10.0.0.1 tcp dport 8080 ct status dnat masquerade") {
		t.Fatalf("masquerade must be scoped to DNAT'd conns via ct status dnat, got:\n%s", out)
	}
}

func TestValidateAcceptsIPv6(t *testing.T) {
	r := Rule{Proto: "tcp", SrcPort: 80, DestIP: "2001:db8::1", DestPort: 80}
	if err := Validate(r); err != nil {
		t.Fatalf("expected ok for IPv6 address, got %v", err)
	}
}

func TestRenderRulesetIPv6DNAT(t *testing.T) {
	out := RenderRuleset([]Rule{{
		Proto: "tcp", SrcPort: 8080, DestIP: "2001:db8::1", DestPort: 80,
	}})
	if !contains(out, "dnat ip6 to [2001:db8::1]:80") {
		t.Fatalf("expected IPv6 dnat syntax, got:\n%s", out)
	}
	if !contains(out, "ip6 daddr 2001:db8::1 tcp dport 80 ct status dnat masquerade") {
		t.Fatalf("expected IPv6 masquerade syntax, got:\n%s", out)
	}
}

func TestRenderRulesetInetFamily(t *testing.T) {
	out := RenderRuleset(nil)
	if !contains(out, "table inet nft_forward") {
		t.Fatalf("expected inet family table, got:\n%s", out)
	}
}

func TestIsIPv6(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"2001:db8::1", true},
		{"::1", true},
		{"10.0.0.1", false},
		{"not-an-ip", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsIPv6(c.addr); got != c.want {
			t.Errorf("IsIPv6(%q)=%v want %v", c.addr, got, c.want)
		}
	}
}

func TestIsLoopback(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1", true},
		{"127.0.0.2", true},
		{"::1", true},
		{"10.0.0.1", false},
		{"2001:db8::1", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsLoopback(c.addr); got != c.want {
			t.Errorf("IsLoopback(%q)=%v want %v", c.addr, got, c.want)
		}
	}
}

func TestRenderRulesetLoopbackIPv4(t *testing.T) {
	out := RenderRuleset([]Rule{
		{Proto: "tcp", SrcPort: 8080, DestIP: "127.0.0.1", DestPort: 80},
	})
	if !contains(out, "dnat ip to 127.0.0.1:80") {
		t.Fatalf("expected IPv4 loopback DNAT, got:\n%s", out)
	}
	if contains(out, "masquerade") {
		t.Fatalf("loopback rule must not emit masquerade, got:\n%s", out)
	}
	if contains(out, "chain account {\n\t\ttype filter hook forward") && contains(out, "proto-dst 8080 ct direction original") {
		t.Fatalf("loopback rule must not appear in forward-hook account chain, got:\n%s", out)
	}
	if !contains(out, "chain account_local {") {
		t.Fatalf("expected account_local chain for loopback, got:\n%s", out)
	}
	if !contains(out, "ct original proto-dst 8080 ct status dnat ct direction original counter") {
		t.Fatalf("expected loopback input accounting, got:\n%s", out)
	}
	if !contains(out, "chain account_local_reply {") {
		t.Fatalf("expected account_local_reply chain for loopback, got:\n%s", out)
	}
}

func TestRenderRulesetLoopbackIPv6(t *testing.T) {
	out := RenderRuleset([]Rule{
		{Proto: "tcp", SrcPort: 8080, DestIP: "::1", DestPort: 80},
	})
	if !contains(out, "meta nfproto ipv6 tcp dport 8080 redirect to :80") {
		t.Fatalf("expected IPv6 redirect for ::1, got:\n%s", out)
	}
	if contains(out, "dnat ip6") {
		t.Fatalf("must not emit dnat ip6 for ::1, got:\n%s", out)
	}
	if contains(out, "masquerade") {
		t.Fatalf("loopback rule must not emit masquerade, got:\n%s", out)
	}
}

func TestRenderRulesetMixedLoopbackAndRemote(t *testing.T) {
	out := RenderRuleset([]Rule{
		{Proto: "tcp", SrcPort: 8080, DestIP: "127.0.0.1", DestPort: 80},
		{Proto: "tcp", SrcPort: 9090, DestIP: "10.0.0.5", DestPort: 443},
	})
	// Remote rule has masquerade and forward-hook accounting
	if !contains(out, "ip daddr 10.0.0.5 tcp dport 443 ct status dnat masquerade") {
		t.Fatalf("remote rule must have masquerade, got:\n%s", out)
	}
	// Loopback rule has no masquerade
	if contains(out, "ip daddr 127.0.0.1") {
		t.Fatalf("loopback rule must not appear in masquerade, got:\n%s", out)
	}
	// Both account and account_local chains exist
	if !contains(out, "chain account {") || !contains(out, "chain account_local {") {
		t.Fatalf("expected both account and account_local chains, got:\n%s", out)
	}
}

func TestRenderRulesetNoLoopbackOmitsLocalChains(t *testing.T) {
	out := RenderRuleset([]Rule{
		{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80},
	})
	if contains(out, "account_local") {
		t.Fatalf("non-loopback rules must not emit account_local chains, got:\n%s", out)
	}
}

func TestRenderRulesetSkipsEmptyDestIP(t *testing.T) {
	// An unresolved DestHost leaves DestIP empty; the rule must be skipped in
	// both chains rather than emitting invalid syntax that fails the whole table.
	out := RenderRuleset([]Rule{
		{Proto: "tcp", SrcPort: 8443, DestHost: "unresolved.example", DestPort: 443},
		{Proto: "tcp", SrcPort: 9443, DestIP: "10.0.0.6", DestPort: 443},
	})
	if contains(out, "dport 8443") {
		t.Fatalf("rule with empty DestIP must be skipped entirely, got:\n%s", out)
	}
	if contains(out, "dnat to :443") || contains(out, "ip daddr  ") {
		t.Fatalf("must not emit invalid syntax for empty DestIP, got:\n%s", out)
	}
	if !contains(out, "tcp dport 9443 dnat ip to 10.0.0.6:443") {
		t.Fatalf("sibling rule with valid DestIP must still render, got:\n%s", out)
	}
	if !contains(out, "ip daddr 10.0.0.6 tcp dport 443 ct status dnat masquerade") {
		t.Fatalf("sibling masquerade must still render, got:\n%s", out)
	}
	if !contains(out, "meta l4proto tcp ct original proto-dst 9443 ct direction original counter") {
		t.Fatalf("sibling accounting counter must render, got:\n%s", out)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestResolveHostsFillsDestIP(t *testing.T) {
	r := &resolver.Resolver{
		Lookup: func(ctx context.Context, host string) ([]string, error) {
			return []string{"203.0.113.7"}, nil
		},
	}
	rules := []Rule{{Proto: "tcp", SrcPort: 80, DestHost: "x.example", DestPort: 80}}
	out, changed, err := ResolveHosts(context.Background(), rules, r)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected changed=true on first resolve")
	}
	if out[0].DestIP != "203.0.113.7" {
		t.Fatalf("got %q", out[0].DestIP)
	}
}

func TestResolveHostsNoChangeWhenSame(t *testing.T) {
	r := &resolver.Resolver{
		Lookup: func(ctx context.Context, host string) ([]string, error) {
			return []string{"203.0.113.7"}, nil
		},
	}
	rules := []Rule{{Proto: "tcp", SrcPort: 80, DestHost: "x.example", DestIP: "203.0.113.7", DestPort: 80}}
	out, changed, err := ResolveHosts(context.Background(), rules, r)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("expected changed=false when IP unchanged")
	}
	if out[0].DestIP != "203.0.113.7" {
		t.Fatalf("got %q", out[0].DestIP)
	}
}

func TestResolveHostsKeepsOldIPOnError(t *testing.T) {
	r := &resolver.Resolver{
		Lookup: func(ctx context.Context, host string) ([]string, error) {
			return nil, errors.New("dns down")
		},
	}
	rules := []Rule{{Proto: "tcp", SrcPort: 80, DestHost: "x.example", DestIP: "203.0.113.7", DestPort: 80}}
	out, changed, err := ResolveHosts(context.Background(), rules, r)
	if err == nil {
		t.Fatal("expected aggregated error")
	}
	if changed {
		t.Fatal("expected changed=false on failure")
	}
	if out[0].DestIP != "203.0.113.7" {
		t.Fatalf("stale IP should be preserved, got %q", out[0].DestIP)
	}
}

func TestEffectiveMode(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ModeKernel},
		{ModeKernel, ModeKernel},
		{ModeUserspace, ModeUserspace},
		{"bogus", ModeKernel}, // unknown normalizes to kernel (defensive default)
	}
	for _, c := range cases {
		got := Rule{Mode: c.in}.EffectiveMode()
		if got != c.want {
			t.Errorf("EffectiveMode(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestValidateModeMatrix(t *testing.T) {
	base := Rule{SrcPort: 8443, DestIP: "10.0.0.1", DestPort: 443}
	mk := func(proto, mode string) Rule { r := base; r.Proto = proto; r.Mode = mode; return r }

	ok := []Rule{
		mk("tcp", ""), mk("tcp", ModeKernel), mk("udp", ModeKernel), mk("tcp+udp", ModeKernel),
		mk("tcp", ModeUserspace), mk("tcp+udp", ModeUserspace),
	}
	for _, r := range ok {
		if err := Validate(r); err != nil {
			t.Errorf("Validate(%s/%s) unexpected error: %v", r.Proto, r.Mode, err)
		}
	}

	bad := []Rule{
		mk("udp", ModeUserspace), // UDP cannot use userspace
		mk("tcp", "weird"),       // illegal mode
	}
	for _, r := range bad {
		if err := Validate(r); err == nil {
			t.Errorf("Validate(%s/%s) expected error, got nil", r.Proto, r.Mode)
		}
	}
}

func TestRuleMetaIsInert(t *testing.T) {
	// Rule metadata is panel-side only; it must never change data-plane behavior.
	r := Rule{Proto: "tcp", SrcPort: 100, DestIP: "10.0.0.1", DestPort: 200,
		RuleID: 7, RuleName: "seednet-vless"}
	if r.EffectiveMode() != ModeKernel {
		t.Fatalf("rule meta must not affect EffectiveMode, got %q", r.EffectiveMode())
	}
	// A rule without rule metadata must not serialize the fields.
	b, err := json.Marshal(Rule{Proto: "tcp", SrcPort: 100, DestPort: 200})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "rule_id") || strings.Contains(string(b), "rule_name") {
		t.Fatalf("empty rule meta must be omitted, got %s", b)
	}
}

func TestRule_OwnerNameRoundTripAndOmitempty(t *testing.T) {
	r := Rule{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80, OwnerName: "qqpw"}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"owner_name":"qqpw"`) {
		t.Fatalf("owner_name not marshaled: %s", b)
	}
	var got Rule
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.OwnerName != "qqpw" {
		t.Fatalf("owner_name round-trip mismatch: %q", got.OwnerName)
	}

	// Empty owner must be omitted from the wire (display-only metadata).
	bare, _ := json.Marshal(Rule{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80})
	if strings.Contains(string(bare), "owner_name") {
		t.Fatalf("empty owner_name should be omitted, got: %s", bare)
	}
}

func TestGroupShapeMark(t *testing.T) {
	cases := []struct {
		r    Rule
		want uint32
	}{
		{Rule{ShapeGroup: 5, RateMBytes: 10}, 0x10005},
		{Rule{ShapeGroup: 0, RateMBytes: 10}, 0},
		{Rule{ShapeGroup: 5, RateMBytes: 0}, 0},
		{Rule{ShapeGroup: 0x10000, RateMBytes: 10}, 0}, // minor is 16-bit; oversize falls back to legacy shaping
	}
	for i, c := range cases {
		if got := GroupShapeMark(c.r); got != c.want {
			t.Errorf("case %d: mark = %#x, want %#x", i, got, c.want)
		}
	}
}

func TestRenderRuleset_GroupShaping(t *testing.T) {
	out := RenderRuleset([]Rule{
		{Proto: "tcp", SrcPort: 8080, DestIP: "10.0.0.2", DestPort: 80, ShapeGroup: 5, RateMBytes: 10},
	})
	// First packet: stamp the packet and store the mark on the conntrack
	// entry in one DNAT rule.
	if !strings.Contains(out, "meta mark set 0x10005 ct mark set meta mark dnat ip to 10.0.0.2:80") {
		t.Fatalf("missing group mark on DNAT rule:\n%s", out)
	}
	// Every later packet, both directions: restore the mark before routing so
	// tc's egress fw filter classifies the whole connection.
	if !strings.Contains(out, "chain restore_mark") ||
		!strings.Contains(out, "type filter hook prerouting priority mangle; policy accept;") ||
		!strings.Contains(out, "ct mark != 0 meta mark set ct mark") {
		t.Fatalf("missing restore_mark chain:\n%s", out)
	}
}

func TestRenderRuleset_LegacyPortMark(t *testing.T) {
	out := RenderRuleset([]Rule{
		{Proto: "tcp", SrcPort: 8080, DestIP: "10.0.0.2", DestPort: 80, BandwidthMbps: 50},
	})
	if !strings.Contains(out, "meta mark set 8080 dnat ip to 10.0.0.2:80") {
		t.Fatalf("legacy per-port mark missing:\n%s", out)
	}
	if strings.Contains(out, "restore_mark") {
		t.Fatalf("legacy-only ruleset must not emit the restore chain:\n%s", out)
	}
}

func TestRenderRuleset_GroupOverridesLegacyMirror(t *testing.T) {
	out := RenderRuleset([]Rule{
		{Proto: "tcp", SrcPort: 8080, DestIP: "10.0.0.2", DestPort: 80, ShapeGroup: 5, RateMBytes: 10, BandwidthMbps: 84},
	})
	if strings.Contains(out, "meta mark set 8080 ") {
		t.Fatalf("group-shaped rule must not also carry the legacy port mark:\n%s", out)
	}
}

func TestRenderRuleset_OversizeGroupFallsBackToLegacy(t *testing.T) {
	out := RenderRuleset([]Rule{
		{Proto: "tcp", SrcPort: 8080, DestIP: "10.0.0.2", DestPort: 80, ShapeGroup: 0x10000, RateMBytes: 10, BandwidthMbps: 84},
	})
	if !strings.Contains(out, "meta mark set 8080 ") {
		t.Fatalf("oversize group must fall back to the legacy port mark:\n%s", out)
	}
	if strings.Contains(out, "restore_mark") {
		t.Fatalf("no valid group → no restore chain:\n%s", out)
	}
}
