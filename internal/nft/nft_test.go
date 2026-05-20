package nft

import (
	"context"
	"errors"
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
	if contains(out, "meta l4proto") {
		t.Fatalf("single tcp rule must not use set syntax, got:\n%s", out)
	}
}

func TestRenderRulesetUsesDestIP(t *testing.T) {
	out := RenderRuleset([]Rule{{
		Proto: "tcp", SrcPort: 80, DestHost: "home.example.net",
		DestIP: "10.0.0.5", DestPort: 80,
	}})
	if !contains(out, "dnat to 10.0.0.5:80") {
		t.Fatalf("renderer must use DestIP, got:\n%s", out)
	}
	if contains(out, "home.example.net") {
		t.Fatalf("renderer leaked host into nft script:\n%s", out)
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
