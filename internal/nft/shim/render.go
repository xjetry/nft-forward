package shim

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"nft-forward/internal/nft"
)

// handleRegex captures the trailing `# handle N` annotation emitted by
// `nft -a list chain`. nft prints it on every rule line.
var handleRegex = regexp.MustCompile(`#\s*handle\s+(\d+)\s*$`)

// parseShimHandles walks nft -a list chain output and returns every
// handle whose rule line carries the OwnerComment string. Lines without
// OwnerComment (other tools' rules, ct rules from a different owner)
// are ignored.
func parseShimHandles(listOutput string) []int {
	var out []int
	for _, line := range strings.Split(listOutput, "\n") {
		if !strings.Contains(line, "comment \""+OwnerComment+"\"") {
			continue
		}
		m := handleRegex.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	return out
}

// renderShimScript builds the `nft -f -` script that, in one atomic
// transaction:
//
//  1. Deletes every rule whose handle is in staleHandles (the
//     previously-injected daemon-managed rules).
//  2. Re-adds the ct state established,related accept tail rule and
//     one accept rule per DNAT (matching ip daddr + proto/dport).
//
// Empty rules + empty staleHandles still emits the ct state rule so
// reply traffic for any future rule has a route through.
func renderShimScript(family, table, chain string, rules []nft.Rule, staleHandles []int) string {
	var b strings.Builder
	for _, h := range staleHandles {
		fmt.Fprintf(&b, "delete rule %s %s %s handle %d\n", family, table, chain, h)
	}
	fmt.Fprintf(&b,
		"add rule %s %s %s ct state established,related counter accept comment \"%s\"\n",
		family, table, chain, OwnerComment,
	)
	for _, r := range rules {
		if r.DestIP == "" {
			continue
		}
		match := protoForwardMatch(r.Proto, r.DestPort)
		fmt.Fprintf(&b,
			"add rule %s %s %s ip daddr %s %s counter accept comment \"%s\"\n",
			family, table, chain, r.DestIP, match, OwnerComment,
		)
	}
	return b.String()
}

// protoForwardMatch produces the proto + dport match clause for the
// forward chain. Mirrors nft.protoPostMatch so tcp+udp uses set syntax.
func protoForwardMatch(proto string, port int) string {
	switch proto {
	case "tcp+udp":
		return fmt.Sprintf("meta l4proto { tcp, udp } th dport %d", port)
	default:
		return fmt.Sprintf("%s dport %d", proto, port)
	}
}
