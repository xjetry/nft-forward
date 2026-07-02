package landing

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestParseURIs_Protocols(t *testing.T) {
	cases := []struct {
		name     string
		uri      string
		proto    string
		host     string
		port     int
		nodeName string
	}{
		{
			name:     "vless",
			uri:      "vless://11111111-2222-3333-4444-555555555555@example.com:443?security=reality&sni=a.com&pbk=KEY&flow=xtls-rprx-vision#HK-01",
			proto:    "vless",
			host:     "example.com",
			port:     443,
			nodeName: "HK-01",
		},
		{
			name:     "trojan",
			uri:      "trojan://pass@1.2.3.4:8443?sni=b.com#JP%20Tokyo",
			proto:    "trojan",
			host:     "1.2.3.4",
			port:     8443,
			nodeName: "JP Tokyo",
		},
		{
			name:     "tuic",
			uri:      "tuic://uuid:pass@t.example.com:2053?congestion_control=bbr#TUIC-1",
			proto:    "tuic",
			host:     "t.example.com",
			port:     2053,
			nodeName: "TUIC-1",
		},
		{
			name:     "hysteria2",
			uri:      "hysteria2://auth@h.example.com:36712?insecure=1#HY2",
			proto:    "hysteria2",
			host:     "h.example.com",
			port:     36712,
			nodeName: "HY2",
		},
		{
			name:     "hy2-alias",
			uri:      "hy2://auth@h2.example.com:443#H",
			proto:    "hysteria2",
			host:     "h2.example.com",
			port:     443,
			nodeName: "H",
		},
		{
			name:     "ss-sip002",
			uri:      "ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:secret")) + "@ss.example.com:8388#SS-Node",
			proto:    "ss",
			host:     "ss.example.com",
			port:     8388,
			nodeName: "SS-Node",
		},
		{
			name:     "ss-legacy",
			uri:      "ss://" + base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:secret@ss2.example.com:9999")) + "#SS-Legacy",
			proto:    "ss",
			host:     "ss2.example.com",
			port:     9999,
			nodeName: "SS-Legacy",
		},
		{
			name: "vmess",
			uri: "vmess://" + base64.StdEncoding.EncodeToString([]byte(
				`{"v":"2","ps":"VMESS-1","add":"v.example.com","port":"443","id":"uuid","net":"ws","tls":"tls"}`)),
			proto:    "vmess",
			host:     "v.example.com",
			port:     443,
			nodeName: "VMESS-1",
		},
		{
			name:     "ipv6",
			uri:      "vless://uuid@[2001:db8::1]:443?security=tls#V6",
			proto:    "vless",
			host:     "2001:db8::1",
			port:     443,
			nodeName: "V6",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			nodes := ParseURIs([]string{c.uri})
			if len(nodes) != 1 {
				t.Fatalf("expected 1 node, got %d", len(nodes))
			}
			n := nodes[0]
			if n.Protocol != c.proto {
				t.Errorf("proto = %q, want %q", n.Protocol, c.proto)
			}
			if n.Host != c.host {
				t.Errorf("host = %q, want %q", n.Host, c.host)
			}
			if n.Port != c.port {
				t.Errorf("port = %d, want %d", n.Port, c.port)
			}
			if n.Name != c.nodeName {
				t.Errorf("name = %q, want %q", n.Name, c.nodeName)
			}
		})
	}
}

func TestParseURIs_StripsPanelDedupSuffix(t *testing.T) {
	cases := []struct {
		name string
		uri  string
		want string
	}{
		{
			name: "authority fragment",
			uri:  "vless://uuid@example.com:443?security=tls#boil-hkt%20%5E~2~%5E",
			want: "boil-hkt",
		},
		{
			name: "ss fragment",
			uri:  "ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:secret")) + "@ss.example.com:8388#boil-hkt%20%5E~13~%5E",
			want: "boil-hkt",
		},
		{
			name: "vmess ps",
			uri: "vmess://" + base64.StdEncoding.EncodeToString([]byte(
				`{"ps":"boil-hkt ^~3~^","add":"v.example.com","port":"443"}`)),
			want: "boil-hkt",
		},
		{
			name: "suffix-only name kept",
			uri:  "vless://uuid@example.com:443#%5E~2~%5E",
			want: "^~2~^",
		},
		{
			name: "mid-name marker untouched",
			uri:  "vless://uuid@example.com:443#a%20%5E~2~%5E%20b",
			want: "a ^~2~^ b",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			nodes := ParseURIs([]string{c.uri})
			if len(nodes) != 1 {
				t.Fatalf("expected 1 node, got %d", len(nodes))
			}
			if nodes[0].Name != c.want {
				t.Errorf("name = %q, want %q", nodes[0].Name, c.want)
			}
		})
	}
}

func TestParseURIs_SkipsInvalid(t *testing.T) {
	nodes := ParseURIs([]string{
		"",
		"   ",
		"# a comment line",
		"not-a-uri",
		"http://example.com",
		"vless://uuid@ok.example.com:443#OK",
	})
	if len(nodes) != 1 {
		t.Fatalf("expected 1 valid node, got %d", len(nodes))
	}
	if nodes[0].Host != "ok.example.com" {
		t.Errorf("host = %q", nodes[0].Host)
	}
}

func TestRewriteEndpoint_PreservesEverythingElse(t *testing.T) {
	t.Run("vless keeps query and fragment verbatim", func(t *testing.T) {
		in := "vless://uuid@example.com:443?security=reality&sni=a.com&pbk=KEY&flow=xtls-rprx-vision#HK-01"
		out, err := RewriteEndpoint(in, "relay.host", 10001)
		if err != nil {
			t.Fatal(err)
		}
		want := "vless://uuid@relay.host:10001?security=reality&sni=a.com&pbk=KEY&flow=xtls-rprx-vision#HK-01"
		if out != want {
			t.Errorf("got  %q\nwant %q", out, want)
		}
	})
	t.Run("trojan", func(t *testing.T) {
		in := "trojan://pass@1.2.3.4:8443?sni=b.com#JP%20Tokyo"
		out, err := RewriteEndpoint(in, "5.6.7.8", 20000)
		if err != nil {
			t.Fatal(err)
		}
		want := "trojan://pass@5.6.7.8:20000?sni=b.com#JP%20Tokyo"
		if out != want {
			t.Errorf("got  %q\nwant %q", out, want)
		}
	})
	t.Run("ipv6 host in original, ipv4 replacement", func(t *testing.T) {
		in := "vless://uuid@[2001:db8::1]:443?security=tls#V6"
		out, err := RewriteEndpoint(in, "9.9.9.9", 443)
		if err != nil {
			t.Fatal(err)
		}
		want := "vless://uuid@9.9.9.9:443?security=tls#V6"
		if out != want {
			t.Errorf("got  %q\nwant %q", out, want)
		}
	})
	t.Run("ss sip002 keeps userinfo and name", func(t *testing.T) {
		ui := base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:secret"))
		in := "ss://" + ui + "@ss.example.com:8388#SS-Node"
		out, err := RewriteEndpoint(in, "relay", 12345)
		if err != nil {
			t.Fatal(err)
		}
		want := "ss://" + ui + "@relay:12345#SS-Node"
		if out != want {
			t.Errorf("got  %q\nwant %q", out, want)
		}
	})
	t.Run("ss legacy re-encodes with new endpoint", func(t *testing.T) {
		in := "ss://" + base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:secret@ss2.example.com:9999")) + "#SS-Legacy"
		out, err := RewriteEndpoint(in, "relay.host", 10002)
		if err != nil {
			t.Fatal(err)
		}
		// Re-parse the result to confirm the endpoint changed and credentials kept.
		nodes := ParseURIs([]string{out})
		if len(nodes) != 1 || nodes[0].Host != "relay.host" || nodes[0].Port != 10002 {
			t.Fatalf("rewrite did not take effect: %q -> %+v", out, nodes)
		}
		if nodes[0].Name != "SS-Legacy" {
			t.Errorf("name lost: %q", nodes[0].Name)
		}
	})
	t.Run("vmess rewrites add/port keeps other json fields", func(t *testing.T) {
		in := "vmess://" + base64.StdEncoding.EncodeToString([]byte(
			`{"v":"2","ps":"VMESS-1","add":"v.example.com","port":"443","id":"uuid","net":"ws","tls":"tls"}`))
		out, err := RewriteEndpoint(in, "relay.host", 10003)
		if err != nil {
			t.Fatal(err)
		}
		nodes := ParseURIs([]string{out})
		if len(nodes) != 1 || nodes[0].Host != "relay.host" || nodes[0].Port != 10003 {
			t.Fatalf("rewrite did not take effect: %q -> %+v", out, nodes)
		}
		if nodes[0].Name != "VMESS-1" {
			t.Errorf("name lost: %q", nodes[0].Name)
		}
	})
}

func TestDecodeSubscription(t *testing.T) {
	uris := "vless://uuid@a.com:443#A\ntrojan://pass@b.com:8443#B\n"
	t.Run("base64 std", func(t *testing.T) {
		lines := DecodeSubscription([]byte(base64.StdEncoding.EncodeToString([]byte(uris))))
		if len(lines) != 2 {
			t.Fatalf("got %d lines: %v", len(lines), lines)
		}
	})
	t.Run("plain text fallback", func(t *testing.T) {
		lines := DecodeSubscription([]byte(uris))
		if len(lines) != 2 {
			t.Fatalf("got %d lines: %v", len(lines), lines)
		}
	})
	t.Run("crlf and blank lines trimmed", func(t *testing.T) {
		lines := DecodeSubscription([]byte("vless://uuid@a.com:443#A\r\n\r\ntrojan://pass@b.com:8443#B\r\n"))
		if len(lines) != 2 {
			t.Fatalf("got %d lines: %v", len(lines), lines)
		}
		if strings.Contains(lines[0], "\r") {
			t.Errorf("CR not trimmed: %q", lines[0])
		}
	})
}

// RewriteName must round-trip through the parser: the rewritten URI parses
// back to the new name with the endpoint untouched, for every protocol shape
// the parser accepts.
func TestRewriteNameRoundTrip(t *testing.T) {
	uris := []string{
		"vless://u@1.2.3.4:443?type=tcp#Old",
		"trojan://u@5.6.7.8:8443#Old",
		"vless://u@1.2.3.4:443",
		"vmess://" + base64.StdEncoding.EncodeToString([]byte(`{"add":"9.9.9.9","port":"443","ps":"Old"}`)),
		"ss://YWVzLTEyOC1nY206cGFzcw==@1.2.3.4:8388#Old",
		"Old = snell, 1.2.3.4, 443, psk = xxx, version = 5",
	}
	const newName = "香港 01"
	for _, uri := range uris {
		got, err := RewriteName(uri, newName)
		if err != nil {
			t.Fatalf("%s: %v", uri, err)
		}
		nodes := ParseURIs([]string{got})
		if len(nodes) != 1 || nodes[0].Name != newName {
			t.Fatalf("%s -> %s: parsed %+v", uri, got, nodes)
		}
		orig := ParseURIs([]string{uri})[0]
		if nodes[0].Host != orig.Host || nodes[0].Port != orig.Port || nodes[0].Protocol != orig.Protocol {
			t.Fatalf("%s: endpoint changed, %+v vs %+v", uri, nodes[0], orig)
		}
	}
}

func TestRewriteNameMalformed(t *testing.T) {
	if _, err := RewriteName("not a proxy uri", "x"); err == nil {
		t.Fatal("malformed uri must error")
	}
	if _, err := RewriteName("vmess://!!!", "x"); err == nil {
		t.Fatal("undecodable vmess must error")
	}
}
