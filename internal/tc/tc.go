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

// planClasses 从规则集推出 HTB 的叶子：每个限速分组一个 class，minor 是分组 id，
// 过滤器接的标记是 nft.GroupShapeMark 给的那个——nft 打标记时调的是同一个函数。
// 该函数返回 0 的规则没有 class。输出按 ClassID 排序，同一个规则集得到同一个结果。
func planClasses(rules []nft.Rule) []shapeClass {
	groups := map[int64]int{}
	for _, r := range rules {
		if nft.GroupShapeMark(r) != 0 {
			groups[r.ShapeGroup] = r.RateMBytes
		}
	}
	out := make([]shapeClass, 0, len(groups))
	for sg, mb := range groups {
		out = append(out, shapeClass{
			ClassID: fmt.Sprintf("1:%x", sg),
			// MB/s (2^20 bytes) expressed in exact bits so tc's own unit
			// parsing cannot skew the cap.
			Rate:   fmt.Sprintf("%dbit", int64(mb)*8388608),
			Handle: fmt.Sprintf("0x%x", 0x10000|sg),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ClassID < out[j].ClassID })
	return out
}

// Apply 在 iface 上按规则集里的分组限速重建 HTB 树。每次调用都先拆掉整棵树再从头
// 建，所以结果只取决于这一次的规则集；没有分组要整形时拆完就不再建。
//
// 布局：
//
//	qdisc 1: htb default 1
//	  class 1:1 — 默认 class，没有标记的流量走这里，不限速
//	  class 1:<分组 id> — 每个限速分组一个，rate 等于 ceil
//	filter handle <标记> fw → classid
//
// 标记由 nft 打（`meta mark set 0x1XXXX`，见 nft.RenderRuleset）。
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
