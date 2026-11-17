package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"nft-forward/internal/db"
)

// 组合的成员可以还是组合。探测面对的必须是展平后的物理节点：只有物理节点上有
// agent 能应答探测帧、有真实的在线状态。outer = [n1, inner]，inner = [n2, n3] 时，
// 探测要逐个报告 n1、n2、n3，并由链尾的物理节点 n3 去拨目标——而不是把 inner 这个
// 组合节点当成一台机器去探测。
func TestCompositeProbeFlattensNestedComposites(t *testing.T) {
	d := openDB(t)
	n1, _ := db.CreateNode(d, "n1", "", "")
	n2, _ := db.CreateNode(d, "n2", "", "")
	n3, _ := db.CreateNode(d, "n3", "", "")
	for i, n := range []*db.Node{n1, n2, n3} {
		_ = db.UpdateNodeRelayHost(d, n.ID, fmt.Sprintf("10.0.0.%d", i+1))
	}
	inner := makeComposite(t, d, "inner", n2.ID, n3.ID)
	outer := makeComposite(t, d, "outer", n1.ID, inner.ID)
	admin := loginAsAdmin(t, d)
	s, _ := New(d)

	const target = "9.9.9.9:80"
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/probe?target=%s&node=%d", target, outer.ID), nil)
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	var res probeResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}

	var got []string
	for _, h := range res.Hops {
		got = append(got, h.Node)
	}
	want := []string{"n1", "n2", "n3"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("探测应逐个报告展平后的物理节点 %v，实际 %v（body=%s）", want, got, rec.Body.String())
	}
	if last := res.Hops[len(res.Hops)-1]; last.Target != target {
		t.Fatalf("应由链尾的物理节点去拨目标：末项 %+v", last)
	}
	for _, h := range res.Hops[:len(res.Hops)-1] {
		if h.Target != "" {
			t.Fatalf("只有链尾节点拨目标，%s 不应带 target", h.Node)
		}
	}
}
