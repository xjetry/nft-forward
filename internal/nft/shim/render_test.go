package shim

import (
	"strings"
	"testing"

	"nft-forward/internal/nft"
)

func TestParseShimHandlesEmptyChain(t *testing.T) {
	out := `table ip filter {
	chain DOCKER-USER {
	}
}`
	handles := parseShimHandles(out)
	if len(handles) != 0 {
		t.Fatalf("expected 0 handles, got %v", handles)
	}
}

func TestParseShimHandlesIgnoresUnrelatedRules(t *testing.T) {
	out := `table ip filter {
	chain DOCKER-USER {
		counter packets 5 bytes 300 jump SOMEWHERE-ELSE # handle 7
		ip daddr 1.2.3.4 counter accept comment "third-party tool" # handle 8
	}
}`
	handles := parseShimHandles(out)
	if len(handles) != 0 {
		t.Fatalf("expected 0 handles (no nft-forward managed), got %v", handles)
	}
}

func TestParseShimHandlesPicksOwnerTagged(t *testing.T) {
	out := `table ip filter {
	chain DOCKER-USER {
		counter packets 0 bytes 0 jump SOMEWHERE # handle 7
		ct state established,related counter accept comment "nft-forward managed" # handle 12
		ip daddr 10.0.0.1 tcp dport 80 counter accept comment "nft-forward managed" # handle 17
		ip daddr 1.2.3.4 counter accept comment "other" # handle 19
	}
}`
	handles := parseShimHandles(out)
	want := []int{12, 17}
	if !equalInts(handles, want) {
		t.Fatalf("got %v, want %v", handles, want)
	}
}

func TestRenderShimScriptEmptyRulesStillEmitsCtState(t *testing.T) {
	script := renderShimScript("ip", "filter", "DOCKER-USER", nil, nil)
	if !strings.Contains(script, "ct state established,related") {
		t.Fatalf("ct state rule missing:\n%s", script)
	}
	if strings.Contains(script, "delete rule") {
		t.Fatalf("no stale handles, should not emit delete:\n%s", script)
	}
}

func TestRenderShimScriptDeletesStaleHandles(t *testing.T) {
	script := renderShimScript("ip", "filter", "DOCKER-USER", nil, []int{12, 17})
	if !strings.Contains(script, "delete rule ip filter DOCKER-USER handle 12") {
		t.Fatalf("handle 12 delete missing:\n%s", script)
	}
	if !strings.Contains(script, "delete rule ip filter DOCKER-USER handle 17") {
		t.Fatalf("handle 17 delete missing:\n%s", script)
	}
}

func TestRenderShimScriptInsertsTCPRule(t *testing.T) {
	rules := []nft.Rule{
		{Proto: "tcp", DestIP: "10.20.1.20", DestPort: 8443},
	}
	script := renderShimScript("ip", "filter", "DOCKER-USER", rules, nil)
	want := `add rule ip filter DOCKER-USER ip daddr 10.20.1.20 tcp dport 8443 counter accept comment "nft-forward managed"`
	if !strings.Contains(script, want) {
		t.Fatalf("expected line %q in:\n%s", want, script)
	}
}

func TestRenderShimScriptInsertsTCPUDPRule(t *testing.T) {
	rules := []nft.Rule{
		{Proto: "tcp+udp", DestIP: "10.20.1.20", DestPort: 8443},
	}
	script := renderShimScript("ip", "filter", "DOCKER-USER", rules, nil)
	want := `add rule ip filter DOCKER-USER ip daddr 10.20.1.20 meta l4proto { tcp, udp } th dport 8443 counter accept comment "nft-forward managed"`
	if !strings.Contains(script, want) {
		t.Fatalf("expected line %q in:\n%s", want, script)
	}
}

func TestRenderShimScriptOrderingDeleteBeforeAdd(t *testing.T) {
	rules := []nft.Rule{{Proto: "tcp", DestIP: "1.2.3.4", DestPort: 80}}
	script := renderShimScript("ip", "filter", "DOCKER-USER", rules, []int{5})
	delIdx := strings.Index(script, "delete rule")
	addIdx := strings.Index(script, "add rule")
	if delIdx < 0 || addIdx < 0 {
		t.Fatalf("missing delete or add:\n%s", script)
	}
	if delIdx > addIdx {
		t.Fatalf("delete must come before add for atomic swap:\n%s", script)
	}
}

func TestRenderInputShimScript(t *testing.T) {
	ports := []ListenPort{{Proto: "tcp", Port: 8443}, {Proto: "tcp", Port: 9000}}
	out := renderInputShimScript("ip", "filter", "ufw-user-input", ports, []int{5})
	if !strings.Contains(out, "delete rule ip filter ufw-user-input handle 5") {
		t.Errorf("missing stale delete:\n%s", out)
	}
	if !strings.Contains(out, `tcp dport 8443 counter accept comment "`+OwnerComment+`"`) {
		t.Errorf("missing input accept for 8443:\n%s", out)
	}
	if !strings.Contains(out, `tcp dport 9000 counter accept comment "`+OwnerComment+`"`) {
		t.Errorf("missing input accept for 9000:\n%s", out)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
