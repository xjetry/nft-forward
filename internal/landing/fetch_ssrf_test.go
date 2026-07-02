package landing

import (
	"net"
	"testing"
)

func TestIsPublicIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"2606:4700:4700::1111", true},
		{"127.0.0.1", false},
		{"::1", false},
		{"10.0.0.5", false},
		{"172.16.3.4", false},
		{"192.168.1.1", false},
		{"169.254.169.254", false}, // cloud metadata
		{"fe80::1", false},         // link-local
		{"fc00::1", false},         // ULA
		{"0.0.0.0", false},
		{"100.64.0.1", false}, // CGNAT
		{"224.0.0.1", false},  // multicast
	}
	for _, c := range cases {
		got := isPublicIP(net.ParseIP(c.ip))
		if got != c.want {
			t.Errorf("isPublicIP(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}
