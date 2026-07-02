package tc

import (
	"reflect"
	"testing"

	"nft-forward/internal/nft"
)

// /proc/net/route rows: the second column is the destination; "00000000" marks
// a default route. Only the Iface (col 1) and Destination (col 2) matter here.
const routeHeader = "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n"

func TestParseDefaultIface(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "physical default route",
			body: routeHeader +
				"eth0\t00000000\t0102A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n" +
				"eth0\t0002A8C0\t00000000\t0001\t0\t0\t100\t00FFFFFF\t0\t0\t0\n",
			want: "eth0",
		},
		{
			name: "docker bridge listed before physical default",
			body: routeHeader +
				"docker0\t00000000\t00000000\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
				"eth0\t00000000\t0102A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n",
			want: "eth0",
		},
		{
			name: "veth and user bridge skipped in favor of physical",
			body: routeHeader +
				"veth1a2b\t00000000\t00000000\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
				"br-deadbeef\t00000000\t00000000\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
				"ens3\t00000000\t0102A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n",
			want: "ens3",
		},
		{
			name: "only virtual default route falls back to it",
			body: routeHeader +
				"docker0\t00000000\t00000000\t0003\t0\t0\t0\t00000000\t0\t0\t0\n",
			want: "docker0",
		},
		{
			name: "manual host bridge br0 is not treated as virtual",
			body: routeHeader +
				"br0\t00000000\t0102A8C0\t0003\t0\t0\t0\t00000000\t0\t0\t0\n",
			want: "br0",
		},
		{
			name: "no default route",
			body: routeHeader +
				"eth0\t0002A8C0\t00000000\t0001\t0\t0\t100\t00FFFFFF\t0\t0\t0\n",
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseDefaultIface([]byte(c.body)); got != c.want {
				t.Fatalf("parseDefaultIface() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestPlanClasses(t *testing.T) {
	rules := []nft.Rule{
		// Two rules in one group produce exactly one class.
		{Proto: "tcp", SrcPort: 8080, DestIP: "10.0.0.2", DestPort: 80, ShapeGroup: 5, RateMBytes: 10},
		{Proto: "tcp", SrcPort: 8081, DestIP: "10.0.0.3", DestPort: 80, ShapeGroup: 5, RateMBytes: 10},
		// Legacy per-port cap from a pre-group panel.
		{Proto: "tcp", SrcPort: 9000, DestIP: "10.0.0.4", DestPort: 80, BandwidthMbps: 50},
		// Group-shaped rules must not leak a legacy class from the mirror value.
		{Proto: "tcp", SrcPort: 9001, DestIP: "10.0.0.5", DestPort: 80, ShapeGroup: 6, RateMBytes: 2, BandwidthMbps: 17},
		// Unshaped.
		{Proto: "tcp", SrcPort: 9002, DestIP: "10.0.0.6", DestPort: 80},
	}
	got := planClasses(rules)
	// Sorted lexicographically by ClassID ("1:2328" < "1:5" < "1:6").
	want := []shapeClass{
		{ClassID: "1:2328", Rate: "50mbit", Handle: "0x2328"},
		{ClassID: "1:5", Rate: "83886080bit", Handle: "0x10005"},
		{ClassID: "1:6", Rate: "16777216bit", Handle: "0x10006"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("planClasses:\n got %+v\nwant %+v", got, want)
	}
}

func TestPlanClasses_GroupWinsMinorCollision(t *testing.T) {
	// A legacy port whose hex minor equals a group id would collide in the
	// class-id space; the group keeps the minor, the legacy class is dropped.
	rules := []nft.Rule{
		{Proto: "tcp", SrcPort: 5, DestIP: "10.0.0.2", DestPort: 80, BandwidthMbps: 50},
		{Proto: "tcp", SrcPort: 8080, DestIP: "10.0.0.3", DestPort: 80, ShapeGroup: 5, RateMBytes: 10},
	}
	got := planClasses(rules)
	want := []shapeClass{
		{ClassID: "1:5", Rate: "83886080bit", Handle: "0x10005"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("planClasses:\n got %+v\nwant %+v", got, want)
	}
}
