package nft

import "testing"

// nft -j emits the dport match of a single-protocol rule as a payload
// reference carrying that protocol, but the tcp+udp set form
// (meta l4proto { tcp, udp } th dport N) emits a generic transport-header
// payload whose protocol is "th". parseCounters must surface that as
// "tcp+udp" so a counter's proto matches how the rule is represented
// everywhere else.
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
				{"rule":{"chain":"prerouting","expr":[
					{"match":{"op":"==","left":{"payload":{"protocol":"tcp","field":"dport"}},"right":80}},
					{"counter":{"packets":4,"bytes":400}},
					{"dnat":{"addr":"10.0.0.1","port":80}}
				]}}
			]}`,
			wantPort: 80, wantProto: "tcp", wantBytes: 400,
		},
		{
			name: "udp",
			json: `{"nftables":[
				{"rule":{"chain":"prerouting","expr":[
					{"match":{"op":"==","left":{"payload":{"protocol":"udp","field":"dport"}},"right":53}},
					{"counter":{"packets":2,"bytes":120}},
					{"dnat":{"addr":"10.0.0.2","port":53}}
				]}}
			]}`,
			wantPort: 53, wantProto: "udp", wantBytes: 120,
		},
		{
			name: "tcp+udp set form",
			json: `{"nftables":[
				{"rule":{"chain":"prerouting","expr":[
					{"match":{"op":"==","left":{"meta":{"key":"l4proto"}},"right":{"set":["tcp","udp"]}}},
					{"match":{"op":"==","left":{"payload":{"protocol":"th","field":"dport"}},"right":8080}},
					{"counter":{"packets":3,"bytes":222}},
					{"dnat":{"addr":"10.0.0.3","port":8080}}
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

// Rules outside the prerouting chain (e.g. the masquerade postrouting chain)
// must not produce counters; otherwise totals would double-count.
func TestParseCountersSkipsPostrouting(t *testing.T) {
	doc := `{"nftables":[
		{"rule":{"chain":"postrouting","expr":[
			{"match":{"op":"==","left":{"payload":{"protocol":"tcp","field":"dport"}},"right":80}},
			{"counter":{"packets":9,"bytes":900}},
			{"masquerade":null}
		]}}
	]}`
	got, err := parseCounters([]byte(doc))
	if err != nil {
		t.Fatalf("parseCounters: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("postrouting rule should be ignored, got %+v", got)
	}
}
