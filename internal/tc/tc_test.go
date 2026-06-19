package tc

import "testing"

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
