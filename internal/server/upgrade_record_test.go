package server

import (
	"testing"

	"nft-forward/internal/db"
)

func TestNodeUpgradeColumnsRoundTrip(t *testing.T) {
	d := openDB(t)
	n, err := db.CreateNode(d, "edge", "https://p", "tok")
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.GetNode(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastUpgradeAt.Valid {
		t.Fatalf("fresh node should have null last_upgrade_at, got %+v", got.LastUpgradeAt)
	}

	if err := db.RecordUpgradeResult(d, n.ID, "v1.2.3", "acked", ""); err != nil {
		t.Fatal(err)
	}
	got, err = db.GetNode(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastUpgradeAt.Valid || got.LastUpgradeVersion != "v1.2.3" || got.LastUpgradeStatus != "acked" || got.LastUpgradeError != "" {
		t.Fatalf("after acked record: %+v", got)
	}

	if err := db.RecordUpgradeResult(d, n.ID, "v1.2.3", "error", "节点未连接"); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetNode(d, n.ID)
	if got.LastUpgradeStatus != "error" || got.LastUpgradeError != "节点未连接" {
		t.Fatalf("after error record: status=%q err=%q", got.LastUpgradeStatus, got.LastUpgradeError)
	}
}
