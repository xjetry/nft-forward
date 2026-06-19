package nft

import "testing"

// The accounting chain identifies a rule by its transport protocol
// (meta l4proto) and the listen port recovered from the conntrack original
// tuple (ct original proto-dst), since DNAT has already rewritten the packet's
// own dport by the time it reaches the forward hook. The tcp+udp form emits
// l4proto as a {tcp,udp} set, which parseCounters must surface as "tcp+udp".
func TestParseCountersProto(t *testing.T) {
	cases := []struct {
		name      string
		json      string
		wantPort  int
		wantProto string
		wantBytes int64
	}{
		{
			name: "tcp",
			json: `{"nftables":[
				{"metainfo":{"version":"1.0.6"}},
				{"rule":{"chain":"account","expr":[
					{"match":{"op":"==","left":{"meta":{"key":"l4proto"}},"right":"tcp"}},
					{"match":{"op":"==","left":{"ct":{"key":"proto-dst","dir":"original"}},"right":80}},
					{"counter":{"packets":4,"bytes":400}}
				]}}
			]}`,
			wantPort: 80, wantProto: "tcp", wantBytes: 400,
		},
		{
			name: "udp",
			json: `{"nftables":[
				{"rule":{"chain":"account","expr":[
					{"match":{"op":"==","left":{"meta":{"key":"l4proto"}},"right":"udp"}},
					{"match":{"op":"==","left":{"ct":{"key":"proto-dst","dir":"original"}},"right":53}},
					{"counter":{"packets":2,"bytes":120}}
				]}}
			]}`,
			wantPort: 53, wantProto: "udp", wantBytes: 120,
		},
		{
			name: "tcp+udp set form",
			json: `{"nftables":[
				{"rule":{"chain":"account","expr":[
					{"match":{"op":"==","left":{"meta":{"key":"l4proto"}},"right":{"set":["tcp","udp"]}}},
					{"match":{"op":"==","left":{"ct":{"key":"proto-dst","dir":"original"}},"right":8080}},
					{"counter":{"packets":3,"bytes":222}}
				]}}
			]}`,
			wantPort: 8080, wantProto: "tcp+udp", wantBytes: 222,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseCounters([]byte(c.json))
			if err != nil {
				t.Fatalf("parseCounters: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("want 1 counter, got %d: %+v", len(got), got)
			}
			if got[0].Proto != c.wantProto {
				t.Errorf("Proto = %q, want %q", got[0].Proto, c.wantProto)
			}
			if got[0].ListenPort != c.wantPort {
				t.Errorf("ListenPort = %d, want %d", got[0].ListenPort, c.wantPort)
			}
			if got[0].Bytes != c.wantBytes {
				t.Errorf("Bytes = %d, want %d", got[0].Bytes, c.wantBytes)
			}
		})
	}
}

// Rules outside the accounting chain (the nat prerouting/postrouting chains)
// must not produce counters; only the forward-hook account chain measures
// throughput.
func TestParseCountersSkipsNatChains(t *testing.T) {
	doc := `{"nftables":[
		{"rule":{"chain":"postrouting","expr":[
			{"counter":{"packets":9,"bytes":900}},
			{"masquerade":null}
		]}}
	]}`
	got, err := parseCounters([]byte(doc))
	if err != nil {
		t.Fatalf("parseCounters: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("non-account rule should be ignored, got %+v", got)
	}
}
