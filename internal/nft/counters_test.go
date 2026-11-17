package nft

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

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

	// 协议写成 IANA 协议号的几种形状：机器上没有 /etc/protocols 时 nft 这样输出。
	// 单个协议号与集合 {"set":[6,17]} 两种写法都在 nftables 1.0.6 上实测得到，整份的
	// 实测输出见 testdata。计数链的渲染器对 tcp+udp 按协议各出一条、不用集合写法
	// （accountProtos），集合这一条钉的是 parseL4Proto 里仍保留的那个分支。
	cases = append(cases, []struct {
		name      string
		json      string
		wantPort  int
		wantProto string
		wantBytes int64
	}{
		{
			name: "tcp as protocol number",
			json: `{"nftables":[
				{"rule":{"family":"inet","table":"nft_forward","chain":"account","handle":6,"expr":[
					{"match":{"op":"==","left":{"meta":{"key":"l4proto"}},"right":6}},
					{"match":{"op":"==","left":{"ct":{"key":"proto-dst","dir":"original"}},"right":12001}},
					{"match":{"op":"==","left":{"ct":{"key":"direction"}},"right":"original"}},
					{"counter":{"packets":22,"bytes":201152}}
				]}}
			]}`,
			wantPort: 12001, wantProto: "tcp", wantBytes: 201152,
		},
		{
			name: "udp as protocol number",
			json: `{"nftables":[
				{"rule":{"chain":"account","expr":[
					{"match":{"op":"==","left":{"meta":{"key":"l4proto"}},"right":17}},
					{"match":{"op":"==","left":{"ct":{"key":"proto-dst","dir":"original"}},"right":53}},
					{"counter":{"packets":2,"bytes":120}}
				]}}
			]}`,
			wantPort: 53, wantProto: "udp", wantBytes: 120,
		},
		{
			name: "tcp+udp set of protocol numbers",
			json: `{"nftables":[
				{"rule":{"chain":"account","expr":[
					{"match":{"op":"==","left":{"meta":{"key":"l4proto"}},"right":{"set":[6,17]}}},
					{"match":{"op":"==","left":{"ct":{"key":"proto-dst","dir":"original"}},"right":8080}},
					{"counter":{"packets":3,"bytes":222}}
				]}}
			]}`,
			wantPort: 8080, wantProto: "tcp+udp", wantBytes: 222,
		},
	}...)

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

// testdata 里的两份文件是同一台机器（nftables 1.0.6）上同一条 account 链的两次
// `nft -j list chain` 输出：一次机器上有 /etc/protocols，一次把它挪走了。链里是三条
// 规则的计数——tcp、udp、tcp+udp 各一条，每条按协议和方向展开。两份输出只差在协议
// 写成名字还是协议号，解析结果必须逐项相同：协议号那一份解析不出来，这台机器上
// 所有内核模式的规则就都不计费。
func TestParseCountersIsTheSameWhetherNftPrintsProtocolNamesOrNumbers(t *testing.T) {
	parse := func(file string) []Counter {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("testdata", file))
		if err != nil {
			t.Fatal(err)
		}
		got, err := parseCounters(data)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		return got
	}
	names := parse("account-chain-nft-1.0.6-protocol-names.json")
	numbers := parse("account-chain-nft-1.0.6-protocol-numbers.json")

	want := []Counter{
		{Proto: "tcp", ListenPort: 12041, Direction: "original"},
		{Proto: "tcp", ListenPort: 12041, Direction: "reply"},
		{Proto: "udp", ListenPort: 12042, Direction: "original"},
		{Proto: "udp", ListenPort: 12042, Direction: "reply"},
		{Proto: "tcp", ListenPort: 12043, Direction: "original"},
		{Proto: "tcp", ListenPort: 12043, Direction: "reply"},
		{Proto: "udp", ListenPort: 12043, Direction: "original"},
		{Proto: "udp", ListenPort: 12043, Direction: "reply"},
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("协议名那一份解析结果不对：\n got %+v\nwant %+v", names, want)
	}
	if !reflect.DeepEqual(numbers, want) {
		t.Fatalf("协议号那一份应与协议名那一份解析出同样的计数：\n got %+v\nwant %+v", numbers, want)
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
