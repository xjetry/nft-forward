package daemon

import (
	"context"
	"testing"

	"nft-forward/internal/nft"
	"nft-forward/internal/nft/nfttest"
)

// reconcileCount 是假数据面到目前为止被要求落地的次数。
func reconcileCount(dp *fakeDataplane) int {
	dp.mu.Lock()
	defer dp.mu.Unlock()
	return len(dp.nftCalls)
}

// 面板重新下发的规则集如果只在数据面不读的字段上与现状不同（改了规则名、备注、
// 跳数），数据面不重放：重放是整表重建，会让每条规则的内核计数归零。新的字段值
// 照常记进归属段和状态文件，rev 也照常更新——不重放的只是数据面。
func TestPanelPushThatChangesNoDataplaneFieldDoesNotReapply(t *testing.T) {
	dp := &fakeDataplane{}
	d := newTestDaemon(t)
	d.dp = dp
	ctx := context.Background()

	base := nft.Rule{
		RuleID: 7, RuleName: "old", HopCount: 1, Comment: "c1",
		Proto: "tcp", SrcPort: 12000, DestIP: "10.0.0.1", DestPort: 443,
	}
	if _, err := d.SetPanelRuleset(ctx, "rev-1", []nft.Rule{base}); err != nil {
		t.Fatalf("首次下发: %v", err)
	}
	applied := reconcileCount(dp)
	if applied != 1 {
		t.Fatalf("首次下发应落地一次，实际 %d 次", applied)
	}

	relabelled := base
	relabelled.RuleName, relabelled.Comment, relabelled.HopCount = "new", "c2", 2
	if _, err := d.SetPanelRuleset(ctx, "rev-2", []nft.Rule{relabelled}); err != nil {
		t.Fatalf("只改名称、备注、跳数的下发: %v", err)
	}
	if got := reconcileCount(dp); got != applied {
		t.Errorf("只改了规则名、备注、跳数，数据面不应重放；落地次数从 %d 变成了 %d", applied, got)
	}
	d.mu.Lock()
	stored, rev := append([]nft.Rule(nil), d.owners[segPanel]...), d.meta.LastAppliedRev
	d.mu.Unlock()
	if len(stored) != 1 || stored[0] != relabelled || rev != "rev-2" {
		t.Errorf("新的名称、备注、跳数与 rev 应记进归属段，实际 %+v，rev %q", stored, rev)
	}
	onDisk, meta, err := LoadState(d.statePath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got := onDisk[segPanel]; len(got) != 1 || got[0] != relabelled || meta.LastAppliedRev != "rev-2" {
		t.Errorf("新的名称、备注、跳数与 rev 应落盘，实际 %+v，rev %q", got, meta.LastAppliedRev)
	}

	// 反方向：数据面读的字段变了，就要重放，落地的是新值。
	moved := relabelled
	moved.DestPort = 8443
	if _, err := d.SetPanelRuleset(ctx, "rev-3", []nft.Rule{moved}); err != nil {
		t.Fatalf("改目标端口的下发: %v", err)
	}
	if got := reconcileCount(dp); got != applied+1 {
		t.Fatalf("目标端口变了应重放一次，落地次数 %d（应为 %d）", got, applied+1)
	}
	dp.mu.Lock()
	last := dp.nftCalls[len(dp.nftCalls)-1]
	dp.mu.Unlock()
	if len(last) != 1 || last[0].DestPort != 8443 {
		t.Errorf("重放落地的应是新的目标端口，实际 %+v", last)
	}
}

// “数据面要不要重放”对规则的每个字段都与 nft.Rule.Dataplane 的归类一致：只改一个
// 字段再下发，重放当且仅当数据面投影变了。字段从 nft.Rule 的定义里枚举
// （nfttest.FieldVariants），这里不抄字段清单。
//
// 不论重放与否，新的规则都要记进归属段。
func TestDataplaneIsReappliedExactlyWhenTheProjectionChanges(t *testing.T) {
	base := nft.Rule{
		RuleID: 7, RuleName: "r", HopCount: 1, Comment: "c",
		Proto: "tcp", SrcPort: 12000, DestIP: "10.0.0.1", DestPort: 443,
	}
	ctx := context.Background()
	for field, variant := range nfttest.FieldVariants(base) {
		dp := &fakeDataplane{}
		d := newTestDaemon(t)
		d.dp = dp
		if _, err := d.SetPanelRuleset(ctx, "rev-1", []nft.Rule{base}); err != nil {
			t.Fatalf("%s：首次下发: %v", field, err)
		}
		before := reconcileCount(dp)
		if _, err := d.SetPanelRuleset(ctx, "rev-2", []nft.Rule{variant}); err != nil {
			t.Fatalf("%s：第二次下发: %v", field, err)
		}
		reapplied := reconcileCount(dp) != before
		if want := variant.Dataplane() != base.Dataplane(); reapplied != want {
			t.Errorf("只改 %s：数据面重放了=%v，按数据面投影应为 %v", field, reapplied, want)
		}
		d.mu.Lock()
		stored := append([]nft.Rule(nil), d.owners[segPanel]...)
		d.mu.Unlock()
		if len(stored) != 1 || stored[0] != variant {
			t.Errorf("只改 %s：新的规则应记进归属段，实际 %+v", field, stored)
		}
	}
}
