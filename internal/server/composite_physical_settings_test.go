package server

import (
	"fmt"
	"net/http"
	"testing"

	"nft-forward/internal/db"
)

// “单向计费”和“禁止直接转发”是物理节点的设置：前者由上报计数的物理节点决定
// （Hub 入账时读上报节点的 Unidirectional），后者由链尾的物理节点决定
// （exitHopForbidsDirect 读最后一个物理跳的 NoDirectExit）。组合节点既不上报计数，
// 也不会成为物理跳，在它身上存这两个值没有任何读者，所以设置接口必须拒绝，而不是
// 收下一个不起作用的开关。
func TestCompositeRefusesPhysicalNodeSettings(t *testing.T) {
	d := openDB(t)
	c1, _ := db.CreateNode(d, "c1", "", "")
	c2, _ := db.CreateNode(d, "c2", "", "")
	comp := makeComposite(t, d, "comp", c1.ID, c2.ID)
	admin := loginAsAdmin(t, d)
	s, _ := New(d)

	settings := []struct {
		name string
		path string
		body map[string]any
		get  func(n *db.Node) bool
	}{
		{"单向计费", "unidirectional", map[string]any{"unidirectional": true}, func(n *db.Node) bool { return n.Unidirectional }},
		{"禁止直接转发", "no-direct-exit", map[string]any{"no_direct_exit": true}, func(n *db.Node) bool { return n.NoDirectExit }},
	}
	for _, st := range settings {
		t.Run(st.name, func(t *testing.T) {
			rec := adminPost(t, s, admin, fmt.Sprintf("/api/nodes/%d/%s", comp.ID, st.path), st.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("组合节点上设置%s应被拒绝（400），实际 %d %s", st.name, rec.Code, rec.Body.String())
			}
			if n, _ := db.GetNode(d, comp.ID); st.get(n) {
				t.Fatalf("组合节点上的%s不应被写入", st.name)
			}

			// 反方向：物理节点照常可设。
			rec = adminPost(t, s, admin, fmt.Sprintf("/api/nodes/%d/%s", c1.ID, st.path), st.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("物理节点上设置%s应成功，实际 %d %s", st.name, rec.Code, rec.Body.String())
			}
			if n, _ := db.GetNode(d, c1.ID); !st.get(n) {
				t.Fatalf("物理节点上的%s没有写入", st.name)
			}
		})
	}
}
