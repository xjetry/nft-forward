package server

import (
	"net/http"
	"testing"

	"nft-forward/internal/db"
)

// 服务端权威校验：开启「禁止直接转发」的节点不能作为链尾（出口段的发起者）。
// 入口开启则必须选择线路层；链尾中间层开启则其后必须再接一层。UI 隐藏
// 「直接转发」选项只是便利，绕过前端直接请求同样必须被拒绝。
func TestNoDirectExitEnforced(t *testing.T) {
	d := openDB(t)
	entry, _ := db.CreateNode(d, "entry", "", "")
	mid, _ := db.CreateNode(d, "mid", "", "")
	_ = db.UpdateNodeRelayHost(d, entry.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, mid.ID, "2.2.2.2")
	bindVia(t, d, entry.ID, mid.ID, "userspace")

	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, entry.ID, 5, 0)
	_ = db.GrantNode(d, uid, mid.ID, 5, 0)
	s, _ := New(d)

	// 入口开启开关、不带 via（前端绕过场景）→ 400
	if err := db.UpdateNodeNoDirectExit(d, entry.ID, true); err != nil {
		t.Fatal(err)
	}
	if rec := createMyRuleVia(t, s, cookie, entry.ID, nil, "r-direct"); rec.Code != http.StatusBadRequest {
		t.Fatalf("direct exit on guarded entry: want 400, got %d %s", rec.Code, rec.Body.String())
	}

	// 入口开启开关、经线路层出口（链尾未开启）→ OK
	if rec := createMyRuleVia(t, s, cookie, entry.ID, []int64{mid.ID}, "r-via"); rec.Code != http.StatusOK {
		t.Fatalf("via chain: want 200, got %d %s", rec.Code, rec.Body.String())
	}

	// 链尾中间层也开启开关 → 400（开关是"不能当链尾"，对任意层级生效）
	if err := db.UpdateNodeNoDirectExit(d, mid.ID, true); err != nil {
		t.Fatal(err)
	}
	if rec := createMyRuleVia(t, s, cookie, entry.ID, []int64{mid.ID}, "r-tail"); rec.Code != http.StatusBadRequest {
		t.Fatalf("guarded tail: want 400, got %d %s", rec.Code, rec.Body.String())
	}

	// 关掉入口的开关后，直接转发恢复可用
	if err := db.UpdateNodeNoDirectExit(d, entry.ID, false); err != nil {
		t.Fatal(err)
	}
	if rec := createMyRuleVia(t, s, cookie, entry.ID, nil, "r-direct-ok"); rec.Code != http.StatusOK {
		t.Fatalf("direct exit after disable: want 200, got %d %s", rec.Code, rec.Body.String())
	}
}
