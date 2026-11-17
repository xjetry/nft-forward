package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"nft-forward/internal/db"
	"nft-forward/internal/wsproto"
)

type quotaEnv struct {
	*upgradeTestEnv
	node  *db.Node
	agent *fakeAgent
	log   *pushLog
	uid   int64
	user  *http.Cookie
}

const quotaRulePort = 12001

// newQuotaEnv 起一个面板、一个走真实握手接入的模拟节点，和一个在该节点上有一条规则
// 的普通用户。返回时节点已经收到包含这条规则的下发。
func newQuotaEnv(t *testing.T) *quotaEnv {
	t.Helper()
	e := &quotaEnv{upgradeTestEnv: newUpgradeTestEnv(t)}
	n, err := db.CreateNode(e.d, "edge", "https://panel.example.com", "tok-edge")
	if err != nil {
		t.Fatal(err)
	}
	e.node = n
	e.uid, e.user = loginAsUser(t, e.d, 10)
	if err := db.GrantNode(e.d, e.uid, n.ID, 10, 0); err != nil {
		t.Fatal(err)
	}
	createStandaloneRuleHop(t, e.d, n.ID, "tcp", quotaRulePort, "10.0.0.30", 9200, sql.NullInt64{Int64: e.uid, Valid: true})

	e.agent = connectFakeAgent(t, wsURL(e.srv), wsproto.Hello{NodeToken: "tok-edge", AgentVersion: "v0.0.1", OS: "linux", Arch: "amd64"}, nil)
	e.log = e.agent.pushes
	e.log.waitForwarding(t, quotaRulePort, true, "前置条件：节点上线后应收到这条规则")
	return e
}

// reportTraffic 让节点上报这条规则的一批计数。
func (e *quotaEnv) reportTraffic(t *testing.T, up, down int64) {
	t.Helper()
	e.agent.reportTraffic(t, quotaRulePort, up, down)
}

func (e *quotaEnv) call(t *testing.T, cookie *http.Cookie, method, path string, body any) (int, string) {
	t.Helper()
	buf, _ := json.Marshal(body)
	req, err := http.NewRequest(method, e.srv.URL+path, bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *quotaEnv) admin(t *testing.T, method, path string, body any) {
	t.Helper()
	if code, out := e.call(t, e.cookie, method, path, body); code != http.StatusOK {
		t.Fatalf("%s %s: %d %s", method, path, code, out)
	}
}

// 全局流量配额用尽只停转发：这个用户的规则不再下发到节点，但账号没有被停用，他
// 仍能登录看到自己的用量。
func TestOverQuotaStopsForwardingButKeepsAccount(t *testing.T) {
	e := newQuotaEnv(t)
	e.admin(t, "POST", fmt.Sprintf("/api/users/%d/quota", e.uid), map[string]any{"traffic_quota_bytes": 1000})

	e.reportTraffic(t, 600, 600)
	e.log.waitForwarding(t, quotaRulePort, false, "用量达到全局配额后规则应撤出节点")

	u, err := db.GetUserByID(e.d, e.uid)
	if err != nil {
		t.Fatal(err)
	}
	if u.Disabled {
		t.Fatalf("超额不应停用账号（disable_reason=%q）", u.DisableReason.String)
	}
	code, body := e.call(t, e.user, "GET", "/api/me", nil)
	if code != http.StatusOK {
		t.Fatalf("超额用户应仍能访问面板，/api/me 返回 %d %s", code, body)
	}
	var me struct {
		User struct {
			OverQuota bool `json:"over_quota"`
		} `json:"user"`
	}
	if err := json.Unmarshal([]byte(body), &me); err != nil || !me.User.OverQuota {
		t.Fatalf("/api/me 应标出 over_quota=true：%s", body)
	}
}

// 配额状态只在下发时求值，所以每一个改动全局配额或用量的入口都必须重新下发，否则
// 调高配额之后规则要等到下一次无关的下发才恢复。
func TestRaisingGlobalQuotaRestoresForwarding(t *testing.T) {
	writers := map[string]func(t *testing.T, e *quotaEnv){
		"设置配额": func(t *testing.T, e *quotaEnv) {
			e.admin(t, "POST", fmt.Sprintf("/api/users/%d/quota", e.uid), map[string]any{"traffic_quota_bytes": 5000})
		},
		"修改用户资料": func(t *testing.T, e *quotaEnv) {
			e.admin(t, "PATCH", fmt.Sprintf("/api/users/%d/profile", e.uid), map[string]any{"max_forwards": 10, "traffic_quota_gb": 1})
		},
		"重置流量": func(t *testing.T, e *quotaEnv) {
			e.admin(t, "POST", fmt.Sprintf("/api/users/%d/reset-traffic", e.uid), nil)
		},
	}
	for name, raise := range writers {
		t.Run(name, func(t *testing.T) {
			e := newQuotaEnv(t)
			e.admin(t, "POST", fmt.Sprintf("/api/users/%d/quota", e.uid), map[string]any{"traffic_quota_bytes": 1000})
			e.reportTraffic(t, 600, 600)
			e.log.waitForwarding(t, quotaRulePort, false, "前置条件：超额后规则应撤出")

			raise(t, e)

			// 这些接口同步下发：返回时节点已经收到新的规则集。
			if has, _ := e.log.forwards(quotaRulePort); !has {
				t.Fatalf("%s之后规则应立即恢复转发", name)
			}
			if u, _ := db.GetUserByID(e.d, e.uid); u.Disabled {
				t.Fatalf("%s之后账号不应处于停用状态", name)
			}
		})
	}
}

// 授权配额同理：改动一条授权的配额（直接设置、重新授权、在节点侧批量加授权）都要
// 重新下发经过这条授权的规则。
func TestRaisingGrantQuotaRestoresForwarding(t *testing.T) {
	writers := map[string]func(t *testing.T, e *quotaEnv){
		"设置授权配额": func(t *testing.T, e *quotaEnv) {
			e.admin(t, "POST", fmt.Sprintf("/api/users/%d/nodes/%d/quota", e.uid, e.node.ID), map[string]any{"traffic_quota_bytes": 5000})
		},
		"重新授权": func(t *testing.T, e *quotaEnv) {
			e.admin(t, "POST", fmt.Sprintf("/api/users/%d/grants", e.uid), map[string]any{"node_id": e.node.ID, "max_forwards": 10, "traffic_quota_bytes": 5000})
		},
		"节点侧批量加授权": func(t *testing.T, e *quotaEnv) {
			e.admin(t, "POST", fmt.Sprintf("/api/nodes/%d/grants/batch", e.node.ID), map[string]any{"add_user_ids": []int64{e.uid}, "max_forwards": 10})
		},
		"重置授权流量": func(t *testing.T, e *quotaEnv) {
			e.admin(t, "POST", fmt.Sprintf("/api/users/%d/nodes/%d/reset-traffic", e.uid, e.node.ID), nil)
		},
	}
	for name, raise := range writers {
		t.Run(name, func(t *testing.T) {
			e := newQuotaEnv(t)
			e.admin(t, "POST", fmt.Sprintf("/api/users/%d/nodes/%d/quota", e.uid, e.node.ID), map[string]any{"traffic_quota_bytes": 1000})
			e.reportTraffic(t, 600, 600)
			e.log.waitForwarding(t, quotaRulePort, false, "前置条件：授权超额后规则应撤出")

			raise(t, e)

			if has, _ := e.log.forwards(quotaRulePort); !has {
				t.Fatalf("%s之后规则应立即恢复转发", name)
			}
		})
	}
}
