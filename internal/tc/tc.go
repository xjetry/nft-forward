package tc

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"nft-forward/internal/nft"
)

// Apply rebuilds the HTB tree on iface to match the rate limits encoded in
// rules. Rules with BandwidthMbps == 0 generate no tc state (default class,
// unmetered). The tree is rebuilt from scratch on every call so reconcile is
// trivially correct.
//
// Layout:
//
//	qdisc 1: htb default 1
//	  class 1:1 — default, no limit
//	  class 1:<port> — rate=ceil=<bw>mbit, one per shaped rule
//	filter handle <port> fw → classid 1:<port>
//
// Mark for shaped rules is set by nft (`meta mark set <port>`).
func Apply(rules []nft.Rule, iface string) error {
	if iface == "" {
		return nil
	}
	hasShaped := false
	for _, r := range rules {
		if r.BandwidthMbps > 0 {
			hasShaped = true
			break
		}
	}
	// Always tear down to keep state deterministic.
	_ = runIgnore("tc", "qdisc", "del", "dev", iface, "root")
	if !hasShaped {
		return nil
	}

	if err := run("tc", "qdisc", "add", "dev", iface, "root", "handle", "1:", "htb", "default", "1"); err != nil {
		return err
	}
	// Default class — huge ceiling so unmarked traffic isn't throttled.
	if err := run("tc", "class", "add", "dev", iface, "parent", "1:", "classid", "1:1", "htb", "rate", "100gbit"); err != nil {
		return err
	}
	// Per-port (listen port) classes. Same port may appear in tcp+udp variants
	// of the same forward; we install at most one class per port.
	seen := map[int]bool{}
	for _, r := range rules {
		if r.BandwidthMbps <= 0 || seen[r.SrcPort] {
			continue
		}
		seen[r.SrcPort] = true
		// tc parses class-id minor as hex by default and rejects anything
		// over 16 bits. Encode listen-port (≤65535) as hex.
		classid := fmt.Sprintf("1:%x", r.SrcPort)
		rate := fmt.Sprintf("%dmbit", r.BandwidthMbps)
		if err := run("tc", "class", "add", "dev", iface, "parent", "1:", "classid", classid,
			"htb", "rate", rate, "ceil", rate); err != nil {
			return fmt.Errorf("class %s: %w", classid, err)
		}
		// Filter handle holds the mark value set by nft. We pass it as hex
		// with explicit 0x prefix to keep the base unambiguous.
		if err := run("tc", "filter", "add", "dev", iface, "parent", "1:", "protocol", "ip",
			"handle", fmt.Sprintf("0x%x", r.SrcPort), "fw", "classid", classid); err != nil {
			return fmt.Errorf("filter %d: %w", r.SrcPort, err)
		}
	}
	return nil
}

func Available() bool {
	_, err := exec.LookPath("tc")
	return err == nil
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func runIgnore(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

// DefaultIface inspects /proc/net/route to find the interface holding the
// default route. Returns "" when nothing is found; callers fall back to
// "eth0" or to whatever the operator passed via --iface.
func DefaultIface() string {
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	return parseDefaultIface(data)
}

// parseDefaultIface picks the default-route interface from /proc/net/route
// contents. When a host runs Docker/libvirt alongside a real uplink, more than
// one interface can carry a default route; we prefer a physical NIC because
// shaping a container bridge (docker0, br-<id>, veth*) would miss the real
// egress. The virtual interface is kept only as a fallback so a host whose sole
// default route is virtual still gets shaping.
func parseDefaultIface(data []byte) string {
	var fallback string
	for i, line := range strings.Split(string(data), "\n") {
		if i == 0 || line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] != "00000000" {
			continue
		}
		iface := fields[0]
		if isVirtualIface(iface) {
			if fallback == "" {
				fallback = iface
			}
			continue
		}
		return iface
	}
	return fallback
}

// virtualIfacePrefixes names interface families added by container/VM stacks
// rather than real uplinks. "br-" matches Docker's user-defined bridges without
// catching a manually configured host bridge like "br0".
var virtualIfacePrefixes = []string{"docker", "br-", "veth", "virbr", "vnet", "lo"}

func isVirtualIface(name string) bool {
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
