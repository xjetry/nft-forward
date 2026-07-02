package tc

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"nft-forward/internal/nft"
)

// shapeClass is one HTB leaf: a class and the fw-mark filter feeding it.
type shapeClass struct {
	ClassID string // "1:<minor hex>"
	Rate    string // tc rate expression, rate == ceil
	Handle  string // fw mark the filter matches, "0x..."
}

// planClasses derives the HTB leaves from the ruleset: one class per shape
// group (minor = group id, mark carries the 0x10000 offset) plus one class per
// legacy per-port cap from pre-group panels. Group-shaped rules never spawn a
// legacy class from their mirror BandwidthMbps. When a legacy port's minor
// collides with a group id the group wins — the group is current policy, the
// port cap is a compatibility remnant. Output is sorted for determinism.
func planClasses(rules []nft.Rule) []shapeClass {
	groups := map[int64]int{}
	legacy := map[int]int{}
	for _, r := range rules {
		if nft.GroupShapeMark(r) != 0 {
			groups[r.ShapeGroup] = r.RateMBytes
		} else if r.BandwidthMbps > 0 {
			legacy[r.SrcPort] = r.BandwidthMbps
		}
	}
	out := make([]shapeClass, 0, len(groups)+len(legacy))
	for sg, mb := range groups {
		out = append(out, shapeClass{
			ClassID: fmt.Sprintf("1:%x", sg),
			// MB/s (2^20 bytes) expressed in exact bits so tc's own unit
			// parsing cannot skew the cap.
			Rate:   fmt.Sprintf("%dbit", int64(mb)*8388608),
			Handle: fmt.Sprintf("0x%x", 0x10000|sg),
		})
	}
	for port, mbps := range legacy {
		if _, taken := groups[int64(port)]; taken {
			continue
		}
		out = append(out, shapeClass{
			ClassID: fmt.Sprintf("1:%x", port),
			Rate:    fmt.Sprintf("%dmbit", mbps),
			Handle:  fmt.Sprintf("0x%x", port),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ClassID < out[j].ClassID })
	return out
}

// Apply rebuilds the HTB tree on iface to match the rate limits encoded in
// rules. Rules with group shaping or per-port bandwidth caps generate tc state
// (default class for unmetered traffic). The tree is rebuilt from scratch on
// every call so reconcile is trivially correct.
//
// Layout:
//
//	qdisc 1: htb default 1
//	  class 1:1 — default, no limit
//	  class 1:<group> — each shape group one class, rate=ceil
//	  class 1:<port> — legacy per-rule cap
//	filter handle fw-mark fw → classid
//
// Mark for shaped rules is set by nft (`meta mark set 0x1XXXX` for groups,
// `meta mark set <port>` for legacy).
func Apply(rules []nft.Rule, iface string) error {
	if iface == "" {
		return nil
	}
	classes := planClasses(rules)
	// Always tear down to keep state deterministic.
	_ = runIgnore("tc", "qdisc", "del", "dev", iface, "root")
	if len(classes) == 0 {
		return nil
	}

	if err := run("tc", "qdisc", "add", "dev", iface, "root", "handle", "1:", "htb", "default", "1"); err != nil {
		return err
	}
	// Default class — huge ceiling so unmarked traffic isn't throttled.
	if err := run("tc", "class", "add", "dev", iface, "parent", "1:", "classid", "1:1", "htb", "rate", "100gbit"); err != nil {
		return err
	}
	for _, c := range classes {
		if err := run("tc", "class", "add", "dev", iface, "parent", "1:", "classid", c.ClassID,
			"htb", "rate", c.Rate, "ceil", c.Rate); err != nil {
			return fmt.Errorf("class %s: %w", c.ClassID, err)
		}
		for _, proto := range []string{"ip", "ipv6"} {
			if err := run("tc", "filter", "add", "dev", iface, "parent", "1:", "protocol", proto,
				"handle", c.Handle, "fw", "classid", c.ClassID); err != nil {
				return fmt.Errorf("filter %s/%s: %w", proto, c.Handle, err)
			}
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
