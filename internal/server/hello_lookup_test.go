package server

import (
	"strings"
	"testing"

	"nft-forward/internal/wsproto"
)

// 握手时面板要先认定对端是哪个节点：远程入口按 token 查，本机入口查 self 节点。这次
// 查找有三种结局，各有各的回答——认出来了；没有这样的节点（“token 不认识”“面板没有
// self 节点”）；查找没能执行。第三种不能答成第二种：那是关于那一行的回答，而没能执行
// 的查找对那一行一无所知，节点的操作者会照着去换一个本来有效的 token。
//
// 每个入口两组：节点行确实不存在时回它自己的拒绝原因（对照组，说明下面的回答不是
// 入口对什么都这么答）；nodes 表读不出来时回 helloLookupFailed，并且不带那两句话。
func TestHelloIsNotToldTheNodeIsUnknownWhenTheLookupFails(t *testing.T) {
	for _, c := range []struct {
		name    string
		url     func(e *localAgentEnv) string
		hello   wsproto.Hello
		missing func(t *testing.T, e *localAgentEnv)
		absent  string
	}{
		{
			name:  "远程入口",
			url:   func(e *localAgentEnv) string { return wsURL(e.public) },
			hello: wsproto.Hello{NodeToken: "tok-nobody", OS: "linux", Arch: "amd64"},
			// 没有任何节点用这个 token，不需要再动数据。
			missing: func(*testing.T, *localAgentEnv) {},
			absent:  "unknown or revoked token",
		},
		{
			name:  "本机入口",
			url:   func(e *localAgentEnv) string { return wsURL(e.local) },
			hello: wsproto.Hello{OS: "linux", Arch: "amd64"},
			missing: func(t *testing.T, e *localAgentEnv) {
				if _, err := e.d.Exec(`DELETE FROM nodes WHERE node_type='self'`); err != nil {
					t.Fatal(err)
				}
			},
			absent: "panel has no self node",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newLocalAgentEnv(t)
			c.missing(t, e)
			if _, ack := helloOver(t, c.url(e), c.hello); ack.Error != c.absent {
				t.Fatalf("对照：节点行不存在时应回 %q，实际 %q", c.absent, ack.Error)
			}

			e = newLocalAgentEnv(t)
			loseTable(t, e.d, "nodes")
			_, ack := helloOver(t, c.url(e), c.hello)
			if ack.Error != helloLookupFailed {
				t.Fatalf("nodes 读不出来：应回 %q，实际 %q", helloLookupFailed, ack.Error)
			}
			if strings.Contains(ack.Error, c.absent) {
				t.Fatalf("查找没能执行，却被答成 %q", ack.Error)
			}
		})
	}
}
